package notifications

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type storeIO struct {
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	rename func(string, string) error
}

func realStoreIO() storeIO {
	return storeIO{write: (*os.File).Write, sync: (*os.File).Sync, rename: os.Rename}
}

type savedState struct {
	Version         int                                       `json:"version"`
	Epoch           string                                    `json:"epoch"`
	Sequence        uint64                                    `json:"sequence"`
	ReservedThrough uint64                                    `json:"reserved_through"`
	Recent          []storedEvent                             `json:"recent"`
	Recipients      map[int64]watchdapi.Recipient             `json:"recipients"`
	Pending         map[int64][]storedEvent                   `json:"pending"`
	Closed          map[int64]map[watchdapi.EventID]time.Time `json:"closed_progress"`
	Health          map[string]json.RawMessage                `json:"health"`
}

func (s *Store) restore() error {
	if s.path == "" {
		return errRead
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		s.needsBackup = true
		return errRead
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.needsBackup = true
		return errRead
	}
	var state savedState
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &state) != nil || json.Unmarshal(data, &fields) != nil {
		s.needsBackup = true
		return errCorrupt
	}
	for _, key := range []string{"version", "epoch", "sequence", "recent", "recipients", "pending", "closed_progress", "health"} {
		if _, present := fields[key]; !present {
			s.needsBackup = true
			return errCorrupt
		}
	}
	if _, present := fields["reserved_through"]; !present {
		state.ReservedThrough = state.Sequence
	}
	if !validSavedState(state) {
		s.needsBackup = true
		return errCorrupt
	}
	s.epoch = state.Epoch
	s.sequence = state.ReservedThrough
	s.reservedThrough = state.ReservedThrough
	s.recent = state.Recent
	if state.Recipients != nil {
		s.recipients = state.Recipients
	}
	if state.Pending != nil {
		s.pending = state.Pending
	}
	if state.Closed != nil {
		s.closed = state.Closed
	}
	if state.Health != nil {
		s.health = state.Health
	}
	s.savedRevision = s.revision
	if state.Sequence != s.sequence || info.Mode().Perm() != 0600 {
		s.revision++
	}
	return nil
}

func validSavedState(state savedState) bool {
	if state.Version != 1 || !validEpoch(state.Epoch) || state.ReservedThrough < state.Sequence {
		return false
	}
	known := make(map[watchdapi.EventID]storedEvent, len(state.Recent))
	validEvents := func(events []storedEvent) bool {
		var previous uint64
		for _, event := range events {
			epoch, sequence, valid := parseEventID(event.EventID)
			if !valid || epoch != state.Epoch || sequence > state.Sequence || sequence <= previous {
				return false
			}
			previous = sequence
			if original, exists := known[event.EventID]; exists && (!original.At.Equal(event.At) || original.Text != event.Text) {
				return false
			}
			known[event.EventID] = event
		}
		return true
	}
	if !validEvents(state.Recent) {
		return false
	}
	for chatID, recipient := range state.Recipients {
		if recipient.ChatID != chatID {
			return false
		}
	}
	for chatID, queue := range state.Pending {
		if _, active := state.Recipients[chatID]; !active || !validEvents(queue) {
			return false
		}
		for _, event := range queue {
			if _, closed := state.Closed[chatID][event.EventID]; closed {
				return false
			}
		}
	}
	for _, progress := range state.Closed {
		for id, at := range progress {
			epoch, sequence, valid := parseEventID(id)
			if !valid || epoch != state.Epoch || sequence > state.Sequence {
				return false
			}
			if event, exists := known[id]; exists && !event.At.Equal(at) {
				return false
			}
		}
	}
	return true
}

func (s *Store) Flush() error {
	s.saving.Lock()
	defer s.saving.Unlock()
	if err := s.ensureEpoch(); err != nil {
		s.recordStorageError(err)
		return err
	}
	at := s.now()
	s.mu.Lock()
	s.pruneLocked(at)
	reserveNeeded := s.sequence >= s.reservedThrough && s.sequence != ^uint64(0)
	if s.revision == s.savedRevision && !reserveNeeded && !s.needsBackup {
		s.mu.Unlock()
		return nil
	}
	state := s.snapshotLocked()
	revision := s.revision
	s.mu.Unlock()
	data, err := json.Marshal(state)
	if err != nil {
		err = errEncoding
	} else if s.needsBackup {
		err = s.preserveOriginal()
	}
	if err == nil {
		err = s.writeAtomic(data)
	}
	s.mu.Lock()
	if err != nil {
		s.storageError = err.Error()
	} else {
		// Only the snapshot's revision reached durable storage.
		s.savedRevision = revision
		s.reservedThrough = max(s.reservedThrough, state.ReservedThrough)
		s.storageError = ""
	}
	s.mu.Unlock()
	return err
}

func (s *Store) recordStorageError(err error) {
	s.mu.Lock()
	s.storageError = err.Error()
	s.mu.Unlock()
}

func (s *Store) snapshotLocked() savedState {
	// A durable upper bound prevents reuse after a crash loses dirty events.
	reserved := ^uint64(0)
	if s.sequence <= ^uint64(0)-sequenceReserve {
		reserved = max(s.reservedThrough, s.sequence+sequenceReserve)
	}
	state := savedState{
		Version: 1, Epoch: s.epoch, Sequence: s.sequence, ReservedThrough: reserved,
		Recent:     append([]storedEvent{}, s.recent...),
		Recipients: make(map[int64]watchdapi.Recipient, len(s.recipients)),
		Pending:    make(map[int64][]storedEvent, len(s.pending)),
		Closed:     make(map[int64]map[watchdapi.EventID]time.Time, len(s.closed)),
		Health:     make(map[string]json.RawMessage, len(s.health)),
	}
	for chatID, recipient := range s.recipients {
		state.Recipients[chatID] = recipient
	}
	for chatID, queue := range s.pending {
		state.Pending[chatID] = append([]storedEvent{}, queue...)
	}
	for chatID, progress := range s.closed {
		progressCopy := make(map[watchdapi.EventID]time.Time, len(progress))
		for id, at := range progress {
			progressCopy[id] = at
		}
		state.Closed[chatID] = progressCopy
	}
	for id, health := range s.health {
		state.Health[id] = append(json.RawMessage(nil), health...)
	}
	return state
}

func (s *Store) writeAtomic(data []byte) error {
	if s.path == "" {
		return errWrite
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errWrite
	}
	file, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-")
	if err != nil {
		return errWrite
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return errWrite
	}
	n, err := s.io.write(file, data)
	if err != nil || n != len(data) {
		return errWrite
	}
	if err := s.io.sync(file); err != nil {
		return errSync
	}
	if err := file.Close(); err != nil {
		return errWrite
	}
	if err := s.io.rename(file.Name(), s.path); err != nil {
		return errRename
	}
	return s.syncDirectory(dir)
}

type backupWriter struct {
	file  *os.File
	write func(*os.File, []byte) (int, error)
}

func (w backupWriter) Write(data []byte) (int, error) {
	return w.write(w.file, data)
}

func (s *Store) preserveOriginal() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.needsBackup = false
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errBackup
	}
	original, err := os.Open(s.path)
	if err != nil {
		return errBackup
	}
	defer original.Close()
	dir := filepath.Dir(s.path)
	backup, err := os.CreateTemp(dir, filepath.Base(s.path)+".corrupt-")
	if err != nil {
		return errBackup
	}
	complete := false
	defer func() {
		backup.Close()
		if !complete {
			os.Remove(backup.Name())
		}
	}()
	if backup.Chmod(0600) != nil {
		return errBackup
	}
	if _, err := io.Copy(backupWriter{file: backup, write: s.io.write}, original); err != nil {
		return errBackup
	}
	if s.io.sync(backup) != nil || backup.Close() != nil {
		return errBackup
	}
	if s.syncDirectory(dir) != nil {
		return errBackup
	}
	complete = true
	s.needsBackup = false
	return nil
}

func (s *Store) syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return errSync
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		return errSync
	}
	if err := s.io.sync(file); err != nil {
		return errSync
	}
	if err := file.Close(); err != nil {
		return errSync
	}
	return nil
}
