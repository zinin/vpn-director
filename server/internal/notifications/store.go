// Package notifications keeps watch events and per-chat delivery progress.
package notifications

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	maxPerChat      = 20
	maxRecent       = 20
	maxAge          = 12 * time.Hour
	saveEvery       = 10 * time.Second
	pageSize        = 100
	maxPageBytes    = 16 << 20
	maxCursorLength = 256
	sequenceReserve = uint64(1 << 32)
)

var (
	errRead     = errors.New("cannot read notification storage")
	errCorrupt  = errors.New("notification storage is corrupt")
	errBackup   = errors.New("cannot preserve corrupt notification storage")
	errWrite    = errors.New("cannot write notification storage")
	errSync     = errors.New("cannot sync notification storage")
	errRename   = errors.New("cannot replace notification storage")
	errEncoding = errors.New("cannot encode notification storage")
	errIdentity = errors.New("cannot create notification store identity")
	errSequence = errors.New("notification sequence exhausted")
	errCursor   = watchdapi.ErrInvalidCursor
	errEventID  = watchdapi.ErrInvalidEventID
	errPageSize = errors.New("notification exceeds page size")
)

type storedEvent struct {
	EventID watchdapi.EventID `json:"event_id"`
	At      time.Time         `json:"at"`
	Text    string            `json:"text"`
}

// Store has one writer; saves serialize independently of RAM mutations.
type Store struct {
	mu     sync.Mutex
	saving sync.Mutex
	path   string
	now    func() time.Time
	io     storeIO

	epoch           string
	sequence        uint64
	reservedThrough uint64
	revision        uint64
	savedRevision   uint64
	recent          []storedEvent
	recipients      map[int64]watchdapi.Recipient
	pending         map[int64][]storedEvent
	closed          map[int64]map[watchdapi.EventID]time.Time
	health          map[string]json.RawMessage
	storageError    string

	needsBackup bool // protected by saving
}

func NewStore(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{
		path: path, now: now, io: realStoreIO(),
		revision: 1, reservedThrough: sequenceReserve,
		recent:     []storedEvent{},
		recipients: make(map[int64]watchdapi.Recipient),
		pending:    make(map[int64][]storedEvent),
		closed:     make(map[int64]map[watchdapi.EventID]time.Time),
		health:     make(map[string]json.RawMessage),
	}
	loadErr := s.restore()
	identityErr := s.ensureEpoch()
	at := s.now()
	s.mu.Lock()
	s.pruneLocked(at)
	if loadErr != nil {
		s.storageError = loadErr.Error()
	}
	s.mu.Unlock()
	return s, errors.Join(loadErr, identityErr, s.Flush())
}

func (s *Store) Publish(text string) (watchdapi.EventID, error) {
	if err := s.ensureEpoch(); err != nil {
		return "", err
	}
	at := s.now()
	for {
		s.mu.Lock()
		s.pruneLocked(at)
		if s.sequence == ^uint64(0) {
			s.mu.Unlock()
			return "", errSequence
		}
		if s.sequence >= s.reservedThrough {
			s.mu.Unlock()
			if err := s.Flush(); err != nil {
				return "", err
			}
			continue
		}
		id := s.appendEventLocked(text, at)
		s.mu.Unlock()
		return id, s.Flush()
	}
}

// Caller holds mu and has reserved enough event IDs.
func (s *Store) appendEventLocked(text string, at time.Time) watchdapi.EventID {
	s.sequence++
	id := watchdapi.EventID(s.epoch + ":" + strconv.FormatUint(s.sequence, 10))
	event := storedEvent{EventID: id, At: at, Text: text}
	s.recent = append(s.recent, event)
	for chatID, recipient := range s.recipients {
		if !at.Before(recipient.FirstSeen) {
			s.pending[chatID] = append(s.pending[chatID], event)
		}
	}
	s.revision++
	s.pruneLocked(at)
	return id
}

func (s *Store) Pending(cursor string) (watchdapi.NotificationPage, error) {
	page := watchdapi.NotificationPage{Messages: make([]watchdapi.Notification, 0, pageSize)}
	if len(cursor) > maxCursorLength {
		return page, errCursor
	}
	at := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastChat int64
	var lastSequence uint64
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 || parts[0] != s.epoch {
			return page, errCursor
		}
		var err error
		lastChat, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || lastChat == 0 || strconv.FormatInt(lastChat, 10) != parts[1] {
			return page, errCursor
		}
		lastSequence, err = strconv.ParseUint(parts[2], 10, 64)
		if err != nil || lastSequence == 0 || lastSequence > s.sequence || strconv.FormatUint(lastSequence, 10) != parts[2] {
			return page, errCursor
		}
	}
	s.pruneLocked(at)
	empty, err := json.Marshal(page)
	if err != nil {
		return page, errEncoding
	}
	// Count the envelope, commas, escaped message JSON, cursor and encoder newline.
	encodedSize := len(empty) + 1
	chats := make([]int64, 0, len(s.pending))
	for chatID := range s.pending {
		chats = append(chats, chatID)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	for chatIndex, chatID := range chats {
		queue := s.pending[chatID]
		for eventIndex, event := range queue {
			sequence := eventSequence(event.EventID)
			if cursor != "" && (chatID < lastChat || chatID == lastChat && sequence <= lastSequence) {
				continue
			}
			if len(page.Messages) == pageSize {
				last := page.Messages[len(page.Messages)-1]
				page.NextCursor = s.pageCursor(last.ChatID, eventSequence(last.EventID))
				return page, nil
			}
			message := watchdapi.Notification{ChatID: chatID, EventID: event.EventID, At: event.At, Text: event.Text}
			encoded, err := json.Marshal(message)
			if err != nil {
				return page, errEncoding
			}
			delta := len(encoded)
			if len(page.Messages) > 0 {
				delta++
			}
			nextCursor := ""
			if eventIndex+1 < len(queue) || chatIndex+1 < len(chats) {
				nextCursor = s.pageCursor(chatID, sequence)
			}
			if encodedSize+delta+len(nextCursor) >= maxPageBytes {
				if len(page.Messages) == 0 {
					return page, errPageSize
				}
				last := page.Messages[len(page.Messages)-1]
				page.NextCursor = s.pageCursor(last.ChatID, eventSequence(last.EventID))
				return page, nil
			}
			page.Messages = append(page.Messages, message)
			encodedSize += delta
		}
	}
	return page, nil
}

func (s *Store) pageCursor(chatID int64, sequence uint64) string {
	return s.epoch + ":" + strconv.FormatInt(chatID, 10) + ":" + strconv.FormatUint(sequence, 10)
}

func (s *Store) Ack(chatID int64, eventID watchdapi.EventID) error {
	epoch, sequence, valid := watchdapi.ParseEventID(eventID)
	if !valid {
		return errEventID
	}
	at := s.now()
	s.mu.Lock()
	if epoch == s.epoch && sequence > s.sequence {
		s.mu.Unlock()
		return errEventID
	}
	s.pruneLocked(at)
	changed := false
	queue := s.pending[chatID]
	kept := make([]storedEvent, 0, len(queue))
	for _, event := range queue {
		if event.EventID == eventID {
			changed = true
		} else {
			kept = append(kept, event)
		}
	}
	if changed {
		if len(kept) == 0 {
			delete(s.pending, chatID)
		} else {
			s.pending[chatID] = kept
		}
	}
	for _, event := range s.recent {
		if event.EventID != eventID {
			continue
		}
		if s.closed[chatID] == nil {
			s.closed[chatID] = make(map[watchdapi.EventID]time.Time)
		}
		if _, exists := s.closed[chatID][eventID]; !exists {
			s.closed[chatID][eventID] = event.At
			changed = true
		}
		break
	}
	if changed {
		s.revision++
	}
	s.mu.Unlock()
	// A repeated ack still saves a previous logical removal that failed I/O.
	return s.Flush()
}

func (s *Store) Status() watchdapi.NotificationsStatus {
	at := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(at)
	status := watchdapi.NotificationsStatus{StorageError: s.storageError}
	for _, queue := range s.pending {
		status.Pending += len(queue)
	}
	return status
}

func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(saveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Flush()
			return
		case <-ticker.C:
			s.Flush()
		}
	}
}

func (s *Store) ensureEpoch() error {
	s.mu.Lock()
	missing := s.epoch == ""
	s.mu.Unlock()
	if !missing {
		return nil
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return errIdentity
	}
	epoch := hex.EncodeToString(bytes[:])
	s.mu.Lock()
	if s.epoch == "" {
		s.epoch = epoch
		s.revision++
	}
	s.mu.Unlock()
	return nil
}

func (s *Store) pruneLocked(at time.Time) {
	changed := false
	recent := boundedEvents(s.recent, at, maxRecent, nil)
	if !sameEvents(s.recent, recent) {
		s.recent = recent
		changed = true
	}
	live := make(map[watchdapi.EventID]bool, len(s.recent))
	for _, event := range s.recent {
		live[event.EventID] = true
	}
	for chatID, progress := range s.closed {
		for id := range progress {
			if !live[id] {
				delete(progress, id)
				changed = true
			}
		}
		if len(progress) == 0 {
			delete(s.closed, chatID)
			changed = true
		}
	}
	for chatID, queue := range s.pending {
		recipient, active := s.recipients[chatID]
		if !active {
			delete(s.pending, chatID)
			changed = true
			continue
		}
		kept := boundedEvents(queue, at, maxPerChat, func(event storedEvent) bool {
			_, closed := s.closed[chatID][event.EventID]
			return !closed && !event.At.Before(recipient.FirstSeen)
		})
		if len(kept) == 0 {
			delete(s.pending, chatID)
			changed = true
		} else if !sameEvents(queue, kept) {
			s.pending[chatID] = kept
			changed = true
		}
	}
	if changed {
		s.revision++
	}
}

func boundedEvents(events []storedEvent, at time.Time, limit int, keep func(storedEvent) bool) []storedEvent {
	kept := make([]storedEvent, 0, min(len(events), limit))
	for _, event := range events {
		if at.Sub(event.At) > maxAge || keep != nil && !keep(event) {
			continue
		}
		if len(kept) == limit {
			copy(kept, kept[1:])
			kept[len(kept)-1] = event
		} else {
			kept = append(kept, event)
		}
	}
	return kept
}

func sameEvents(a, b []storedEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].EventID != b[i].EventID || !a[i].At.Equal(b[i].At) || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

func validEpoch(epoch string) bool {
	if len(epoch) != 32 {
		return false
	}
	for i := range epoch {
		if (epoch[i] < '0' || epoch[i] > '9') && (epoch[i] < 'a' || epoch[i] > 'f') {
			return false
		}
	}
	return true
}

func eventSequence(id watchdapi.EventID) uint64 {
	_, sequence, _ := watchdapi.ParseEventID(id)
	return sequence
}
