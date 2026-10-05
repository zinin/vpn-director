package chatstore

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UserChat represents a user with their chat ID.
type UserChat struct {
	Username  string
	ChatID    int64
	FirstSeen time.Time
}

// userRecord is the stored data for each user.
type userRecord struct {
	ChatID           int64     `json:"chat_id"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	Active           bool      `json:"active"`
	NotifiedVersions []string  `json:"notified_versions"`
}

// Store provides thread-safe persistent storage for chat IDs.
type Store struct {
	mu    sync.RWMutex
	path  string
	users map[string]*userRecord
}

// New creates a new Store. If file exists, it loads existing data.
func New(path string) *Store {
	s := &Store{
		path:  path,
		users: make(map[string]*userRecord),
	}
	s.load()
	return s
}

// load reads data from file. Invalid JSON is treated as empty store.
func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // File doesn't exist — start with empty store
	}
	if err := json.Unmarshal(data, &s.users); err != nil {
		slog.Warn("Invalid chats.json, starting with empty store", "error", err)
		// s.users remains initialized from New()
	}
	// Ensure map is never nil (protects against JSON "null" at root)
	if s.users == nil {
		s.users = make(map[string]*userRecord)
	}
	// Remove nil entries (protects against "username": null in JSON)
	for username, record := range s.users {
		if record == nil {
			delete(s.users, username)
			slog.Warn("Removed nil user record from chats.json", "username", username)
		}
	}
}

// save writes data to file atomically.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}

	return os.Rename(tmpPath, s.path)
}

// RecordInteraction updates user's last_seen and sets active=true.
// Creates new user record if not exists.
// Username is normalized to lowercase for consistency with auth.
func (s *Store) RecordInteraction(username string, chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	username = strings.ToLower(username) // Normalize for consistency with auth
	now := time.Now()
	if record, ok := s.users[username]; ok {
		record.ChatID = chatID
		record.LastSeen = now
		record.Active = true
	} else {
		s.users[username] = &userRecord{
			ChatID:           chatID,
			FirstSeen:        now,
			LastSeen:         now,
			Active:           true,
			NotifiedVersions: []string{},
		}
	}

	if err := s.save(); err != nil {
		slog.Warn("Failed to save chat store", "error", err)
		return err
	}
	return nil
}

// GetActiveUsers returns all users with active=true.
func (s *Store) GetActiveUsers() ([]UserChat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var users []UserChat
	for username, record := range s.users {
		if record.Active {
			users = append(users, UserChat{
				Username:  username,
				ChatID:    record.ChatID,
				FirstSeen: record.FirstSeen,
			})
		}
	}
	return users, nil
}

// MarkNotified adds version to user's notified_versions list.
func (s *Store) MarkNotified(username string, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	username = strings.ToLower(username) // Normalize
	record, ok := s.users[username]
	if !ok {
		return nil
	}

	// Check if already notified
	for _, v := range record.NotifiedVersions {
		if v == version {
			return nil
		}
	}

	record.NotifiedVersions = append(record.NotifiedVersions, version)
	// Limit to last 10 versions to prevent unbounded growth
	if len(record.NotifiedVersions) > 10 {
		record.NotifiedVersions = record.NotifiedVersions[len(record.NotifiedVersions)-10:]
	}
	if err := s.save(); err != nil {
		slog.Warn("Failed to save chat store", "error", err)
		return err
	}
	return nil
}

// IsNotified checks if user was notified about version.
func (s *Store) IsNotified(username string, version string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	username = strings.ToLower(username) // Normalize
	record, ok := s.users[username]
	if !ok {
		return false
	}

	for _, v := range record.NotifiedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// SetInactiveChat deactivates every username associated with a blocked chat.
func (s *Store) SetInactiveChat(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for _, record := range s.users {
		if record.ChatID == chatID {
			record.Active = false
			found = true
		}
	}
	if !found {
		return nil
	}
	if err := s.save(); err != nil {
		slog.Warn("Failed to save chat store", "error", err)
		return err
	}
	return nil
}

// SetInactive marks user as inactive (e.g., bot blocked).
func (s *Store) SetInactive(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	username = strings.ToLower(username) // Normalize
	record, ok := s.users[username]
	if !ok {
		return nil
	}

	record.Active = false
	if err := s.save(); err != nil {
		slog.Warn("Failed to save chat store", "error", err)
		return err
	}
	return nil
}
