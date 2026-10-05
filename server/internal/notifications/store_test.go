package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var _ func(string, func() time.Time) (*Store, error) = NewStore

var _ interface {
	Publish(string) (watchdapi.EventID, error)
	ReplaceRecipients([]watchdapi.Recipient) error
	Pending(string) (watchdapi.NotificationPage, error)
	Ack(int64, watchdapi.EventID) error
	Status() watchdapi.NotificationsStatus
	Flush() error
	Run(context.Context)
} = (*Store)(nil)

var notificationEventIDPattern = regexp.MustCompile(`^([0-9a-f]{32}):([1-9][0-9]*)$`)

func notificationTestTime() time.Time {
	return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
}

func newNotificationStore(t *testing.T, path string, now func() time.Time) *Store {
	t.Helper()
	s, err := NewStore(path, now)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s == nil {
		t.Fatal("NewStore returned a nil store without an error")
	}
	return s
}

func replaceNotificationRecipients(t *testing.T, s *Store, recipients ...watchdapi.Recipient) {
	t.Helper()
	if err := s.ReplaceRecipients(recipients); err != nil {
		t.Fatalf("ReplaceRecipients: %v", err)
	}
}

func publishNotification(t *testing.T, s *Store, text string) watchdapi.EventID {
	t.Helper()
	id, err := s.Publish(text)
	if err != nil {
		t.Fatalf("Publish(%q): %v", text, err)
	}
	splitNotificationID(t, id)
	return id
}

func splitNotificationID(t *testing.T, id watchdapi.EventID) (string, uint64) {
	t.Helper()
	parts := notificationEventIDPattern.FindStringSubmatch(string(id))
	if parts == nil {
		t.Fatalf("event ID %q is not <32 lowercase hex epoch>:<positive decimal sequence>", id)
	}
	sequence, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		t.Fatalf("event sequence in %q: %v", id, err)
	}
	return parts[1], sequence
}

func pendingNotifications(t *testing.T, s *Store) []watchdapi.Notification {
	t.Helper()
	var messages []watchdapi.Notification
	cursor := ""
	seen := make(map[string]bool)
	for pages := 0; pages < 100; pages++ {
		page, err := s.Pending(cursor)
		if err != nil {
			t.Fatalf("Pending(%q): %v", cursor, err)
		}
		if len(page.Messages) > 100 {
			t.Fatalf("pending page contains %d messages, want at most 100", len(page.Messages))
		}
		if len(page.NextCursor) > 256 {
			t.Fatalf("pending cursor has %d bytes, want at most 256", len(page.NextCursor))
		}
		messages = append(messages, page.Messages...)
		if page.NextCursor == "" {
			return messages
		}
		if seen[page.NextCursor] || page.NextCursor == cursor {
			t.Fatalf("pending cursor repeated: %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	t.Fatal("pending pagination did not terminate")
	return nil
}

func notificationsForChat(messages []watchdapi.Notification, chatID int64) []watchdapi.Notification {
	var selected []watchdapi.Notification
	for _, message := range messages {
		if message.ChatID == chatID {
			selected = append(selected, message)
		}
	}
	return selected
}

func assertNotificationIDs(t *testing.T, messages []watchdapi.Notification, want []watchdapi.EventID) {
	t.Helper()
	got := make([]watchdapi.EventID, 0, len(messages))
	for _, message := range messages {
		got = append(got, message.EventID)
	}
	if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("pending event IDs %v, want %v", got, want)
	}
}

func TestStore_RestartTTLAndCap(t *testing.T) {
	at := notificationTestTime()
	clock := at
	now := func() time.Time { return clock }
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})

	var publishedIDs []watchdapi.EventID
	for i := 1; i <= 21; i++ {
		publishedIDs = append(publishedIDs, publishNotification(t, s, fmt.Sprintf("m%02d", i)))
	}
	messages := pendingNotifications(t, s)
	if len(messages) != 20 {
		t.Fatalf("len(Messages) = %d, want 20", len(messages))
	}
	if messages[0].EventID != publishedIDs[1] {
		t.Fatalf("Messages[0].EventID = %q, want publishedIDs[1] = %q", messages[0].EventID, publishedIDs[1])
	}
	assertNotificationIDs(t, messages, publishedIDs[1:])
	for i, message := range messages {
		if message.ChatID != 100 || !message.At.Equal(at) || message.Text != fmt.Sprintf("m%02d", i+2) {
			t.Fatalf("message %d = %+v", i, message)
		}
	}
	if status := s.Status(); status.Pending != 20 || status.StorageError != "" {
		t.Fatalf("status after cap = %+v", status)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, s), publishedIDs[1:])

	clock = at.Add(12 * time.Hour)
	assertNotificationIDs(t, pendingNotifications(t, s), publishedIDs[1:])
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, s), publishedIDs[1:])

	clock = clock.Add(time.Nanosecond)
	if messages := pendingNotifications(t, s); len(messages) != 0 {
		t.Fatalf("messages older than TTL survived: %+v", messages)
	}
	if status := s.Status(); status.Pending != 0 {
		t.Fatalf("expired pending count = %d", status.Pending)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	if messages := pendingNotifications(t, s); len(messages) != 0 {
		t.Fatalf("expired messages returned after restart: %+v", messages)
	}
	id := publishNotification(t, s, "after expiry")
	oldEpoch, oldSequence := splitNotificationID(t, publishedIDs[len(publishedIDs)-1])
	newEpoch, newSequence := splitNotificationID(t, id)
	if newEpoch != oldEpoch || newSequence <= oldSequence {
		t.Fatalf("TTL reset event identity: %q after %q", id, publishedIDs[len(publishedIDs)-1])
	}
}

func TestStore_AckIsPerChatAndDoesNotCloseOlderEvents(t *testing.T) {
	at := notificationTestTime()
	s := newNotificationStore(t, filepath.Join(t.TempDir(), "watchd-notifications.json"), func() time.Time { return at })
	replaceNotificationRecipients(t, s,
		watchdapi.Recipient{ChatID: 100, FirstSeen: at},
		watchdapi.Recipient{ChatID: 200, FirstSeen: at},
	)
	first := publishNotification(t, s, "outbound is dead")
	second := publishNotification(t, s, "outbound changed")
	if err := s.Ack(100, second); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(100, second); err != nil {
		t.Fatalf("idempotent Ack: %v", err)
	}
	messages := pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{first})
	assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{first, second})
	if status := s.Status(); status.Pending != 3 || status.StorageError != "" {
		t.Fatalf("status after per-chat Ack = %+v", status)
	}
}

func TestStore_PendingPagesDoNotConsumeMessages(t *testing.T) {
	at := notificationTestTime()
	s := newNotificationStore(t, filepath.Join(t.TempDir(), "watchd-notifications.json"), func() time.Time { return at })
	var recipients []watchdapi.Recipient
	for id := int64(100); id < 106; id++ {
		recipients = append(recipients, watchdapi.Recipient{ChatID: id, FirstSeen: at})
	}
	replaceNotificationRecipients(t, s, recipients...)
	var ids []watchdapi.EventID
	for i := 0; i < 20; i++ {
		ids = append(ids, publishNotification(t, s, fmt.Sprintf("event %02d", i)))
	}
	first, err := s.Pending("")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) == 0 || len(first.Messages) > 100 || first.NextCursor == "" {
		t.Fatalf("first of 120 messages = %+v", first)
	}
	messages := pendingNotifications(t, s)
	if len(messages) != 120 {
		t.Fatalf("paged message count = %d, want 120", len(messages))
	}
	seen := make(map[string]bool)
	for _, message := range messages {
		key := fmt.Sprintf("%d/%s", message.ChatID, message.EventID)
		if seen[key] {
			t.Fatalf("duplicate delivery entry %s", key)
		}
		seen[key] = true
	}
	for _, recipient := range recipients {
		assertNotificationIDs(t, notificationsForChat(messages, recipient.ChatID), ids)
	}
	if status := s.Status(); status.Pending != 120 {
		t.Fatalf("Pending consumed messages: %+v", status)
	}
	if again := pendingNotifications(t, s); !reflect.DeepEqual(again, messages) {
		t.Fatalf("unchanged pagination differs: %+v, want %+v", again, messages)
	}
	if _, err := s.Pending(strings.Repeat("x", 257)); err == nil {
		t.Fatal("a cursor longer than 256 bytes was accepted")
	}
}

func TestStore_ConcurrentAccessPersistsEveryUnackedEvent(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	const writers = 12
	ids := make(chan watchdapi.EventID, writers)
	errors := make(chan error, writers*3)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := s.Publish(fmt.Sprintf("concurrent %02d", i))
			if err != nil {
				errors <- err
				return
			}
			ids <- id
			if _, err := s.Pending(""); err != nil {
				errors <- err
			}
			s.Status()
			if err := s.Flush(); err != nil {
				errors <- err
			}
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errors)
	for err := range errors {
		t.Errorf("concurrent operation: %v", err)
	}
	seen := make(map[watchdapi.EventID]bool)
	for id := range ids {
		splitNotificationID(t, id)
		if seen[id] {
			t.Fatalf("event ID reused by concurrent publishers: %q", id)
		}
		seen[id] = true
	}
	if len(seen) != writers {
		t.Fatalf("published %d distinct IDs, want %d", len(seen), writers)
	}
	messages := pendingNotifications(t, s)
	if len(messages) != writers {
		t.Fatalf("concurrent pending count = %d, want %d", len(messages), writers)
	}
	var last uint64
	var epoch string
	for _, message := range messages {
		currentEpoch, sequence := splitNotificationID(t, message.EventID)
		if !seen[message.EventID] || sequence <= last || epoch != "" && epoch != currentEpoch {
			t.Fatalf("non-monotonic concurrent event: %+v", message)
		}
		epoch, last = currentEpoch, sequence
	}
	for i, message := range messages {
		if i%2 == 0 {
			if err := s.Ack(100, message.EventID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	want := pendingNotifications(t, s)
	reopened := newNotificationStore(t, path, now)
	if got := pendingNotifications(t, reopened); !reflect.DeepEqual(got, want) || len(got) != writers/2 {
		t.Fatalf("unacked concurrent events after restart = %+v, want %+v", got, want)
	}
}

func assertNotificationJSONFields(t *testing.T, value any, want map[string]any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %s, want fields %+v", data, want)
	}
}

func TestWatchTypes_SnakeCaseJSON(t *testing.T) {
	at := notificationTestTime()
	stamp := at.Format(time.RFC3339Nano)
	id := watchdapi.EventID("0123456789abcdef0123456789abcdef:1")
	message := watchdapi.Notification{ChatID: 100, EventID: id, At: at, Text: "Xray outbound is dead"}
	messageJSON := map[string]any{"chat_id": float64(100), "event_id": string(id), "at": stamp, "text": message.Text}
	assertNotificationJSONFields(t, watchdapi.Recipient{ChatID: 100, FirstSeen: at}, map[string]any{
		"chat_id": float64(100), "first_seen": stamp,
	})
	assertNotificationJSONFields(t, message, messageJSON)
	assertNotificationJSONFields(t, watchdapi.NotificationPage{Messages: []watchdapi.Notification{message}, NextCursor: "cursor"}, map[string]any{
		"messages": []any{messageJSON}, "next_cursor": "cursor",
	})
	assertNotificationJSONFields(t, watchdapi.NotificationsStatus{Pending: 1, StorageError: "storage unavailable"}, map[string]any{
		"pending": float64(1), "storage_error": "storage unavailable",
	})
	for _, state := range []watchdapi.WatchState{"starting", "active", "stopped", "incompatible", "error", "not_running"} {
		t.Run(string(state), func(t *testing.T) {
			snapshot := watchdapi.WatchSnapshot{
				State: state, UpdatedAt: at, Message: "watch status", Action: "retry",
				CommittedFailover: true, PendingRestore: true,
				Notifications: watchdapi.NotificationsStatus{Pending: 1, StorageError: "storage unavailable"},
			}
			assertNotificationJSONFields(t, snapshot, map[string]any{
				"state": string(state), "updated_at": stamp, "message": "watch status", "action": "retry",
				"committed_failover": true, "pending_restore": true,
				"notifications": map[string]any{"pending": float64(1), "storage_error": "storage unavailable"},
			})
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			var reopened watchdapi.WatchSnapshot
			if err := json.Unmarshal(data, &reopened); err != nil || !reflect.DeepEqual(reopened, snapshot) {
				t.Fatalf("watch snapshot round-trip = %+v, %v", reopened, err)
			}
		})
	}
}
