package monitor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// savedEntry is an endpoint as the state file keeps it.
type savedEntry struct {
	State  watchdapi.EndpointState `json:"state"`
	Pause  time.Duration           `json:"pause"`
	Sticky bool                    `json:"sticky,omitempty"`
}

type savedState struct {
	SavedAt time.Time             `json:"saved_at"`
	Entries map[string]savedEntry `json:"entries"`
}

// restore reads the state an earlier run saved; the first refresh takes the
// entries of the keys still there. A missing or broken file is no state.
func (m *Monitor) restore() {
	if m.d.StatePath == "" {
		return
	}
	data, err := os.ReadFile(m.d.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var s savedState
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err != nil {
		slog.Warn("Monitor: cannot read the saved state; starting from nothing", "path", m.d.StatePath, "error", err)
		return
	}
	m.mu.Lock()
	m.restored = s.Entries
	m.mu.Unlock()
}

// save writes the state through a temp file and a rename, mode 0600. A
// failure is logged, and the next save tries again in SaveEvery.
func (m *Monitor) save(now time.Time) {
	m.mu.Lock()
	s := savedState{SavedAt: now, Entries: make(map[string]savedEntry, len(m.entries)+len(m.restored))}
	for k, r := range m.restored {
		s.Entries[k] = r
	}
	for k, e := range m.entries {
		s.Entries[k] = savedEntry{State: e.st, Pause: e.pause, Sticky: e.sticky}
	}
	m.lastSave = now
	m.mu.Unlock()
	var err error
	if m.d.StatePath != "" {
		var data []byte
		data, err = json.Marshal(s)
		if err == nil {
			err = writeAtomic(m.d.StatePath, data)
		}
	}
	m.mu.Lock()
	m.dirty, m.saveFailed = err != nil, err != nil
	if err == nil {
		m.statusDirty = false
	}
	m.mu.Unlock()
	if err != nil {
		slog.Warn("Monitor: cannot save the state", "path", m.d.StatePath, "error", err)
	}
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
