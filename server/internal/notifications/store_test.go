package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
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

func TestPending_EncodedSizeBound(t *testing.T) {
	at := notificationTestTime()
	s := newNotificationStore(t, filepath.Join(t.TempDir(), "watchd-notifications.json"), func() time.Time { return at })
	var recipients []watchdapi.Recipient
	for _, chatID := range []int64{1000, 10, -2, 100, 2, -1000} {
		recipients = append(recipients, watchdapi.Recipient{ChatID: chatID, FirstSeen: at})
	}
	replaceNotificationRecipients(t, s, recipients...)
	text := strings.Repeat("<", 600<<10)
	id := publishNotification(t, s, text)
	var messages []watchdapi.Notification
	cursor := ""
	seen := make(map[string]bool)
	for pages := 0; pages < 10; pages++ {
		page, err := s.Pending(cursor)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		// The HTTP JSON encoder also writes a newline and escapes HTML characters.
		if len(encoded)+1 >= 16<<20 || len(page.Messages) > 100 || len(page.NextCursor) > 256 {
			t.Fatalf("page encoded=%d messages=%d cursor=%d", len(encoded)+1, len(page.Messages), len(page.NextCursor))
		}
		messages = append(messages, page.Messages...)
		if page.NextCursor == "" {
			break
		}
		if len(page.Messages) == 0 || seen[page.NextCursor] || page.NextCursor == cursor {
			t.Fatal("size-bounded pagination did not advance")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if len(messages) != 6 || s.Status().Pending != 6 {
		t.Fatalf("size-bounded messages=%d pending=%d, want 6 without consumption", len(messages), s.Status().Pending)
	}
	for i, chatID := range []int64{-1000, -2, 2, 10, 100, 1000} {
		message := messages[i]
		if message.ChatID != chatID || message.EventID != id || !message.At.Equal(at) || message.Text != text {
			t.Fatalf("size-bounded message %d changed payload or numeric chat order", i)
		}
	}
	if again := pendingNotifications(t, s); !reflect.DeepEqual(again, messages) {
		t.Fatal("size-bounded read consumed or changed messages")
	}
}

func TestPending_InvalidCursorIsAtomic(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, func() time.Time { return at })
	var recipients []watchdapi.Recipient
	for chatID := int64(100); chatID < 106; chatID++ {
		recipients = append(recipients, watchdapi.Recipient{ChatID: chatID, FirstSeen: at})
	}
	replaceNotificationRecipients(t, s, recipients...)
	for i := 0; i < 20; i++ {
		publishNotification(t, s, fmt.Sprintf("event %02d", i))
	}
	first, err := s.Pending("")
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page did not provide a cursor: %v", err)
	}
	before := pendingNotifications(t, s)
	durable, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"not-a-cursor", first.NextCursor + "!", strings.Repeat("x", 256), strings.Repeat("x", 257)} {
		if _, err := s.Pending(cursor); err == nil {
			t.Errorf("invalid %d-byte cursor was accepted", len(cursor))
		}
		if after := pendingNotifications(t, s); !reflect.DeepEqual(after, before) || s.Status().Pending != 120 {
			t.Error("invalid cursor consumed or changed pending messages")
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, durable) {
			t.Errorf("invalid cursor changed durable storage: %v", err)
		}
	}
}

func TestPending_AckValidationIsAtomic(t *testing.T) {
	for _, name := range []string{"malformed", "zero-sequence", "leading-zero-sequence", "future-sequence", "maximum-future-sequence"} {
		t.Run(name, func(t *testing.T) {
			at := notificationTestTime()
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, func() time.Time { return at })
			replaceNotificationRecipients(t, s,
				watchdapi.Recipient{ChatID: 100, FirstSeen: at},
				watchdapi.Recipient{ChatID: 200, FirstSeen: at},
			)
			first := publishNotification(t, s, "first")
			second := publishNotification(t, s, "second")
			epoch, _ := splitNotificationID(t, first)
			invalid := watchdapi.EventID(map[string]string{
				"malformed":               "not-an-event",
				"zero-sequence":           epoch + ":0",
				"leading-zero-sequence":   epoch + ":03",
				"future-sequence":         epoch + ":3",
				"maximum-future-sequence": epoch + ":18446744073709551615",
			}[name])
			before := pendingNotifications(t, s)
			durable, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Ack(100, invalid); err == nil {
				t.Errorf("Ack accepted %s", name)
			}
			if after := pendingNotifications(t, s); !reflect.DeepEqual(after, before) || s.Status().StorageError != "" {
				t.Error("invalid Ack changed pending or storage health")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, durable) {
				t.Errorf("invalid Ack changed durable storage: %v", err)
			}
			third := publishNotification(t, s, "after rejected ack")
			messages := pendingNotifications(t, s)
			for _, chatID := range []int64{100, 200} {
				assertNotificationIDs(t, notificationsForChat(messages, chatID), []watchdapi.EventID{first, second, third})
			}
		})
	}
}

// A data path watchd cannot read at startup leaves the store unopened: it
// hands out no event ID, keeps what Publish gets in memory and refuses every
// other operation, until a Flush finds the path and opens the store there.
func TestOpenStore_DefersUntilThePathResolves(t *testing.T) {
	pathErr := errors.New("synthetic unreadable configuration")
	deferred := func(t *testing.T, now func() time.Time) (*Store, func(string)) {
		t.Helper()
		resolved := ""
		resolve := func() (string, error) {
			if resolved == "" {
				return "", pathErr
			}
			return resolved, nil
		}
		s, err := OpenStore(resolve, now)
		if !errors.Is(err, pathErr) || s == nil {
			t.Fatalf("OpenStore = %v, %v; want an unopened store and the resolver's error", s, err)
		}
		return s, func(path string) { resolved = path }
	}

	t.Run("unopened", func(t *testing.T) {
		clock := notificationTestTime()
		s, _ := deferred(t, func() time.Time { return clock })
		if status := s.Status(); status.StorageError != "notification data path is unavailable" || status.Pending != 0 {
			t.Fatalf("status %+v", status)
		}
		for i := 0; i < maxRecent+5; i++ {
			id, err := s.Publish(fmt.Sprintf("event %d", i))
			if id != "" || !errors.Is(err, ErrDeferred) {
				t.Fatalf("Publish = %q, %v; want no ID and ErrDeferred", id, err)
			}
			clock = clock.Add(time.Second)
		}
		if len(s.backlog) != maxRecent || s.backlog[0].Text != "event 5" || s.backlog[maxRecent-1].Text != fmt.Sprintf("event %d", maxRecent+4) {
			t.Fatalf("backlog %+v; want the newest %d events", s.backlog, maxRecent)
		}
		if _, err := s.Pending(""); !errors.Is(err, errUnavailable) {
			t.Errorf("Pending = %v", err)
		}
		if err := s.Ack(100, watchdapi.EventID(strings.Repeat("a", 32)+":1")); !errors.Is(err, errUnavailable) {
			t.Errorf("Ack = %v", err)
		}
		if err := s.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: clock.Add(-time.Hour)}}); !errors.Is(err, errUnavailable) {
			t.Errorf("ReplaceRecipients = %v", err)
		}
		if err := s.ObserveSubscriptions(nil, watchdapi.Snapshot{State: watchdapi.StateOK}); !errors.Is(err, errUnavailable) {
			t.Errorf("ObserveSubscriptions = %v", err)
		}
		if s.epoch != "" || len(s.recipients) != 0 || len(s.recent) != 0 || len(s.pending) != 0 {
			t.Fatalf("an unopened store took state: epoch %q, recipients %v, recent %v", s.epoch, s.recipients, s.recent)
		}
		if err := s.Flush(); !errors.Is(err, pathErr) || s.Status().StorageError != pathErr.Error() {
			t.Fatalf("Flush = %v, status %+v; want the resolver's error recorded", err, s.Status())
		}
	})

	t.Run("opens the existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		clock := notificationTestTime()
		now := func() time.Time { return clock }
		existing := newNotificationStore(t, path, now)
		replaceNotificationRecipients(t, existing, watchdapi.Recipient{ChatID: 100, FirstSeen: clock.Add(-time.Hour)})
		before := publishNotification(t, existing, "before the restart")
		epoch, _ := splitNotificationID(t, before)

		clock = clock.Add(time.Minute)
		s, resolve := deferred(t, now)
		first := clock
		if _, err := s.Publish("deferred one"); !errors.Is(err, ErrDeferred) {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
		second := clock
		if _, err := s.Publish("deferred two"); !errors.Is(err, ErrDeferred) {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
		resolve(path)
		if err := s.Flush(); err != nil {
			t.Fatal("the resolved Flush did not open the store:", err)
		}
		if status := s.Status(); status.StorageError != "" || status.Pending != 3 {
			t.Fatalf("status %+v", status)
		}
		messages := notificationsForChat(pendingNotifications(t, s), 100)
		if len(messages) != 3 || messages[0].EventID != before {
			t.Fatalf("pending %+v; want the restored event, then the deferred ones", messages)
		}
		for i, want := range []struct {
			text string
			at   time.Time
		}{{"deferred one", first}, {"deferred two", second}} {
			got := messages[i+1]
			gotEpoch, _ := splitNotificationID(t, got.EventID)
			if got.Text != want.text || !got.At.Equal(want.at) || gotEpoch != epoch {
				t.Errorf("deferred event %+v; want %q at %v in epoch %s", got, want.text, want.at, epoch)
			}
		}
		if len(s.backlog) != 0 {
			t.Fatalf("backlog %+v after opening", s.backlog)
		}
		reopened := newNotificationStore(t, path, now)
		if got := notificationsForChat(pendingNotifications(t, reopened), 100); !reflect.DeepEqual(got, messages) {
			t.Fatalf("the file holds %+v, want %+v", got, messages)
		}
	})

	t.Run("starts a new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "data", "watchd-notifications.json")
		clock := notificationTestTime()
		s, resolve := deferred(t, func() time.Time { return clock })
		published := clock
		if _, err := s.Publish("deferred"); !errors.Is(err, ErrDeferred) {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
		resolve(path)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("new store file: %v", err)
		}
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: published.Add(-time.Minute)})
		messages := notificationsForChat(pendingNotifications(t, s), 100)
		if len(messages) != 1 || messages[0].Text != "deferred" || !messages[0].At.Equal(published) {
			t.Fatalf("pending %+v; want the deferred event at its own time", messages)
		}
	})
}

// A restart restores the sequence at the saved reserve, so the first event
// needs a save that extends it. While writes fail, Publish hands out no ID
// beyond the saved reserve - a restart with storage still failing would hand
// it out again - and keeps the event in memory until a save succeeds.
func TestStore_PublishWaitsWhileTheReserveCannotBeSaved(t *testing.T) {
	// restarted saves a store with chat 100 and one event, then restores it
	// as a restart does with every write failing; enable lets writes through.
	restarted := func(t *testing.T) (s *Store, path string, clock *time.Time, enable func()) {
		t.Helper()
		at := notificationTestTime()
		now := func() time.Time { return at }
		path = filepath.Join(t.TempDir(), "watchd-notifications.json")
		before := newNotificationStore(t, path, now)
		replaceNotificationRecipients(t, before, watchdapi.Recipient{ChatID: 100, FirstSeen: at.Add(-time.Hour)})
		publishNotification(t, before, "before the restart")
		at = at.Add(time.Minute)
		s = newStore(path, now)
		enable = failNotificationIO(t, s, "write")
		if err := s.open(); err == nil {
			t.Fatal("the restart saved its reserve despite failing writes")
		}
		s.mu.Lock()
		spent := s.sequence >= s.reservedThrough
		s.mu.Unlock()
		if !spent {
			t.Fatal("the restored store holds event IDs no save reserved")
		}
		return s, path, &at, enable
	}
	waitFor := func(t *testing.T, s *Store, clock *time.Time, texts ...string) []time.Time {
		t.Helper()
		var times []time.Time
		for _, text := range texts {
			if id, err := s.Publish(text); id != "" || !errors.Is(err, ErrDeferred) {
				t.Fatalf("Publish(%q) = %q, %v; want no ID and ErrDeferred", text, id, err)
			}
			times = append(times, *clock)
			*clock = clock.Add(time.Second)
		}
		return times
	}
	texts := func(messages []watchdapi.Notification) []string {
		var out []string
		for _, message := range messages {
			out = append(out, message.Text)
		}
		return out
	}

	t.Run("waits in order, bounded", func(t *testing.T) {
		s, _, clock, _ := restarted(t)
		var published []string
		for i := 0; i < maxRecent+3; i++ {
			published = append(published, fmt.Sprintf("waiting %d", i))
		}
		waitFor(t, s, clock, published...)
		if len(s.backlog) != maxRecent {
			t.Fatalf("backlog holds %d events, want %d", len(s.backlog), maxRecent)
		}
		for i, event := range s.backlog {
			if event.Text != published[i+3] {
				t.Fatalf("backlog %d is %q, want %q: the oldest go first", i, event.Text, published[i+3])
			}
		}
		if status := s.Status(); status.StorageError == "" || status.Pending != 1 {
			t.Fatalf("status %+v; want the failing save named and only the restored event pending", status)
		}
	})

	t.Run("drains once a save succeeds", func(t *testing.T) {
		s, path, clock, enable := restarted(t)
		times := waitFor(t, s, clock, "first waiting", "second waiting")
		enable()
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		if len(s.backlog) != 0 {
			t.Fatalf("backlog %+v after a successful save", s.backlog)
		}
		messages := notificationsForChat(pendingNotifications(t, s), 100)
		if got := texts(messages); !reflect.DeepEqual(got, []string{"before the restart", "first waiting", "second waiting"}) {
			t.Fatalf("pending %v", got)
		}
		for i, at := range times {
			if !messages[i+1].At.Equal(at) {
				t.Errorf("%q at %v, want its own time %v", messages[i+1].Text, messages[i+1].At, at)
			}
		}
		next := publishNotification(t, s, "after the drain")
		_, nextSequence := splitNotificationID(t, next)
		_, lastSequence := splitNotificationID(t, messages[2].EventID)
		if nextSequence <= lastSequence {
			t.Fatalf("%s came before the drained %s", next, messages[2].EventID)
		}
		reopened := newNotificationStore(t, path, func() time.Time { return *clock })
		if got := texts(notificationsForChat(pendingNotifications(t, reopened), 100)); !reflect.DeepEqual(got, []string{"before the restart", "first waiting", "second waiting", "after the drain"}) {
			t.Fatalf("the file holds %v", got)
		}
	})

	t.Run("a failed save leaves the rest waiting", func(t *testing.T) {
		s, _, clock, enable := restarted(t)
		waitFor(t, s, clock, "one", "two", "three")
		enable()
		// The reserve's save and the first drained event's save go through.
		write, writes := s.io.write, 0
		s.io.write = func(f *os.File, data []byte) (int, error) {
			writes++
			if writes > 2 {
				return 0, errors.New("injected write failure during the drain")
			}
			return write(f, data)
		}
		if err := s.Flush(); err == nil {
			t.Fatal("the drain hid a failed save")
		}
		if len(s.backlog) != 1 || s.backlog[0].Text != "three" {
			t.Fatalf("backlog %+v; want only the entry the drain did not reach", s.backlog)
		}
		if got := texts(notificationsForChat(pendingNotifications(t, s), 100)); !reflect.DeepEqual(got, []string{"before the restart", "one", "two"}) {
			t.Fatalf("pending %v", got)
		}
		if id, err := s.Publish("behind the waiting one"); id != "" || !errors.Is(err, ErrDeferred) {
			t.Fatalf("Publish = %q, %v; a newer event must wait behind the older one", id, err)
		}
		s.io.write = write
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := texts(notificationsForChat(pendingNotifications(t, s), 100)); !reflect.DeepEqual(got, []string{"before the restart", "one", "two", "three", "behind the waiting one"}) {
			t.Fatalf("pending %v", got)
		}
	})

	t.Run("health after the drained messages", func(t *testing.T) {
		s, _, clock, enable := restarted(t)
		waitFor(t, s, clock, "waiting one", "waiting two")
		enable()
		sub := healthTestSubscription()
		if err := s.ObserveSubscriptions([]vpnconfig.Subscription{sub}, healthTestSnapshot(sub, watchdapi.StatusDead)); err != nil {
			t.Fatal(err)
		}
		if got := texts(notificationsForChat(pendingNotifications(t, s), 100)); !reflect.DeepEqual(got, []string{"before the restart", "waiting one", "waiting two", "Subscription North has no live servers"}) {
			t.Fatalf("pending %v", got)
		}
	})
}
