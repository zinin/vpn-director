package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func readNotificationFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func notificationDocument(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data := readNotificationFile(t, path)
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("invalid notification file: %v", err)
	}
	var version int
	if err := json.Unmarshal(document["version"], &version); err != nil || version != 1 {
		t.Fatalf("notification schema version = %d, %v, want 1", version, err)
	}
	return document
}

// The per-store seam leaves other stores and concurrent tests on real I/O.
func failNotificationIO(t *testing.T, s *Store, stage string) func() {
	t.Helper()
	write, syncFile, rename := s.io.write, s.io.sync, s.io.rename
	if write == nil || syncFile == nil || rename == nil {
		t.Fatal("notification I/O seam has no real defaults")
	}
	injected := errors.New("injected " + stage + " failure")
	switch stage {
	case "write":
		s.io.write = func(*os.File, []byte) (int, error) { return 0, injected }
	case "file_sync", "directory_sync":
		s.io.sync = func(f *os.File) error {
			info, err := f.Stat()
			if err != nil {
				return err
			}
			if stage == "file_sync" && !info.IsDir() || stage == "directory_sync" && info.IsDir() {
				return injected
			}
			return syncFile(f)
		}
	case "rename":
		s.io.rename = func(string, string) error { return injected }
	default:
		t.Fatalf("unknown notification I/O failure stage %q", stage)
	}
	restore := func() {
		s.io.write, s.io.sync, s.io.rename = write, syncFile, rename
	}
	t.Cleanup(restore)
	return restore
}

func assertNoNotificationTempFiles(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(path) {
			t.Errorf("atomic save left an unexpected file: %s", entry.Name())
		}
	}
}

func TestStore_AtomicWriteSyncRenameDirectorySyncOrder(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, func() time.Time { return at })
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	write, syncFile, rename := s.io.write, s.io.sync, s.io.rename
	var stages []string
	s.io.write = func(f *os.File, data []byte) (int, error) {
		stages = append(stages, "write")
		if f.Name() == path || filepath.Dir(f.Name()) != filepath.Dir(path) {
			t.Errorf("write is not staged next to the destination: %s", f.Name())
		}
		return write(f, data)
	}
	s.io.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			stages = append(stages, "directory_sync")
		} else {
			stages = append(stages, "file_sync")
		}
		return syncFile(f)
	}
	s.io.rename = func(from, to string) error {
		stages = append(stages, "rename")
		if to != path || filepath.Dir(from) != filepath.Dir(path) {
			t.Errorf("atomic rename = %s -> %s", from, to)
		}
		return rename(from, to)
	}
	t.Cleanup(func() { s.io.write, s.io.sync, s.io.rename = write, syncFile, rename })
	publishNotification(t, s, "atomic notification")
	var compact []string
	for _, stage := range stages {
		if len(compact) == 0 || compact[len(compact)-1] != stage {
			compact = append(compact, stage)
		}
	}
	want := []string{"write", "file_sync", "rename", "directory_sync"}
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("atomic save stages = %v, want %v", stages, want)
	}
	assertNoNotificationTempFiles(t, path)
}

func TestStore_FailedPublishPreservesPreviousFileAndDirtyState(t *testing.T) {
	for _, stage := range []string{"write", "file_sync", "rename"} {
		t.Run(stage, func(t *testing.T) {
			at := notificationTestTime()
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			now := func() time.Time { return at }
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
			first := publishNotification(t, s, "already durable")
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			before := readNotificationFile(t, path)
			restore := failNotificationIO(t, s, stage)
			second, err := s.Publish("waiting for durable storage")
			if err == nil {
				t.Fatalf("Publish succeeded despite %s failure", stage)
			}
			splitNotificationID(t, second)
			if second == first {
				t.Fatal("failed save reused a durable event ID")
			}
			if status := s.Status(); status.StorageError == "" || status.Pending != 2 {
				t.Fatalf("failed Publish status = %+v", status)
			}
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{first, second})
			if after := readNotificationFile(t, path); !bytes.Equal(after, before) {
				t.Fatal("failed publication changed the previous durable file")
			}
			if err := s.Flush(); err == nil {
				t.Fatal("Flush lost dirty intent while I/O still fails")
			}
			assertNoNotificationTempFiles(t, path)
			restore()
			if err := s.Flush(); err != nil {
				t.Fatalf("recovered Flush: %v", err)
			}
			if status := s.Status(); status.StorageError != "" || status.Pending != 2 {
				t.Fatalf("recovered status = %+v", status)
			}
			reopened := newNotificationStore(t, path, now)
			assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{first, second})
			assertNoNotificationTempFiles(t, path)
		})
	}
}

func TestStore_LostDirtyPublishDoesNotReuseIDs(t *testing.T) {
	for _, stage := range []string{"write", "file_sync", "rename"} {
		t.Run(stage, func(t *testing.T) {
			at := notificationTestTime()
			now := func() time.Time { return at }
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
			first := publishNotification(t, s, "already durable")
			before := readNotificationFile(t, path)
			failNotificationIO(t, s, stage)
			lost, err := s.Publish("lost dirty event")
			if err == nil {
				t.Fatalf("Publish succeeded despite %s failure", stage)
			}
			lostEpoch, lostSequence := splitNotificationID(t, lost)
			if status := s.Status(); status.StorageError == "" || status.Pending != 2 {
				t.Fatalf("failed Publish status = %+v", status)
			}
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{first, lost})
			if after := readNotificationFile(t, path); !bytes.Equal(after, before) {
				t.Fatal("failed publication changed the previous durable file")
			}
			assertNoNotificationTempFiles(t, path)

			// A crash drops dirty RAM without retrying the failed save.
			s = newNotificationStore(t, path, now)
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{first})
			if status := s.Status(); status.StorageError != "" || status.Pending != 1 {
				t.Fatalf("restart retained lost dirty state: %+v", status)
			}
			next := publishNotification(t, s, "after dirty state loss")
			if next == lost {
				t.Fatalf("restart reused lost dirty event ID %q with the same injected clock", lost)
			}
			nextEpoch, nextSequence := splitNotificationID(t, next)
			if nextEpoch != lostEpoch || nextSequence <= lostSequence {
				t.Fatalf("valid restart reset epoch or dirty sequence: %q after %q", next, lost)
			}
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{first, next})
			replaceNotificationRecipients(t, s,
				watchdapi.Recipient{ChatID: 100, FirstSeen: at},
				watchdapi.Recipient{ChatID: 200, FirstSeen: at},
			)
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			s = newNotificationStore(t, path, now)
			messages := pendingNotifications(t, s)
			for _, chatID := range []int64{100, 200} {
				queue := notificationsForChat(messages, chatID)
				assertNotificationIDs(t, queue, []watchdapi.EventID{first, next})
				if queue[0].Text != "already durable" || queue[1].Text != "after dirty state loss" {
					t.Fatalf("durable queue for chat %d contains lost dirty data: %+v", chatID, queue)
				}
			}
			assertNoNotificationTempFiles(t, path)
		})
	}
}

func TestStore_DirectorySyncFailureRetainsDirtyIntent(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	restore := failNotificationIO(t, s, "directory_sync")
	id, err := s.Publish("rename is not the durability boundary")
	if err == nil || s.Status().StorageError == "" {
		t.Fatalf("directory sync failure was hidden: %v, %+v", err, s.Status())
	}
	splitNotificationID(t, id)
	assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{id})
	if err := s.Flush(); err == nil {
		t.Fatal("directory sync failure cleared dirty intent")
	}
	restore()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if status := s.Status(); status.StorageError != "" {
		t.Fatalf("successful directory sync did not recover storage: %+v", status)
	}
	reopened := newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{id})
	assertNoNotificationTempFiles(t, path)
}

func TestAck_DirtyIntentSurvivesLogicalRemoval(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s,
		watchdapi.Recipient{ChatID: 100, FirstSeen: at},
		watchdapi.Recipient{ChatID: 200, FirstSeen: at},
	)
	id := publishNotification(t, s, "outbound is dead")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	before := readNotificationFile(t, path)
	restore := failNotificationIO(t, s, "rename")
	if err := s.Ack(100, id); err == nil {
		t.Fatal("Ack succeeded despite rename failure")
	}
	if status := s.Status(); status.StorageError == "" || status.Pending != 1 {
		t.Fatalf("failed Ack status = %+v", status)
	}
	messages := pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), nil)
	assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{id})
	if after := readNotificationFile(t, path); !bytes.Equal(after, before) {
		t.Fatal("failed Ack changed the previous durable file")
	}
	if err := s.Ack(100, id); err == nil {
		t.Fatal("repeated Ack forgot the logically removed event's dirty intent")
	}
	if s.Status().StorageError == "" {
		t.Fatal("repeated failed Ack hid the storage error")
	}
	restore()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if status := s.Status(); status.StorageError != "" {
		t.Fatalf("durable Ack still reports a storage error: %+v", status)
	}
	s = newNotificationStore(t, path, now)
	messages = pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), nil)
	assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{id})
	replaceNotificationRecipients(t, s)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	if messages := pendingNotifications(t, s); len(messages) != 0 {
		t.Fatalf("Ack removal and closed progress were not one durable record: %+v", messages)
	}
	if err := s.Ack(100, id); err != nil {
		t.Fatalf("Ack after successful Flush is not idempotent: %v", err)
	}
}

func TestStore_CapAndTTLRemainBoundedOnWriteFailure(t *testing.T) {
	at := notificationTestTime()
	clock := at
	now := func() time.Time { return clock }
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	restore := failNotificationIO(t, s, "write")
	var ids []watchdapi.EventID
	for i := 0; i < 21; i++ {
		id, err := s.Publish(fmt.Sprintf("unsaved %02d", i))
		if err == nil {
			t.Fatal("injected write failure did not fail Publish")
		}
		splitNotificationID(t, id)
		ids = append(ids, id)
	}
	assertNotificationIDs(t, pendingNotifications(t, s), ids[1:])
	if status := s.Status(); status.Pending != 20 || status.StorageError == "" {
		t.Fatalf("unbounded RAM queue or hidden write failure: %+v", status)
	}
	clock = at.Add(12 * time.Hour)
	assertNotificationIDs(t, pendingNotifications(t, s), ids[1:])
	clock = clock.Add(time.Nanosecond)
	if messages := pendingNotifications(t, s); len(messages) != 0 {
		t.Fatalf("RAM TTL depends on a successful save: %+v", messages)
	}
	if status := s.Status(); status.Pending != 0 || status.StorageError == "" {
		t.Fatalf("expired dirty status = %+v", status)
	}
	restore()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, reopened,
		watchdapi.Recipient{ChatID: 100, FirstSeen: at},
		watchdapi.Recipient{ChatID: 200, FirstSeen: at},
	)
	if messages := pendingNotifications(t, reopened); len(messages) != 0 {
		t.Fatalf("expired recent history was persisted after I/O recovery: %+v", messages)
	}
}

func TestStore_ReadErrorReturnsUsableBoundedStore(t *testing.T) {
	at := notificationTestTime()
	clock := at
	now := func() time.Time { return clock }
	parent := filepath.Join(t.TempDir(), "blocked-parent")
	if err := os.WriteFile(parent, []byte("SENSITIVE_READ_FAILURE_BYTES"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "watchd-notifications.json")
	s, err := NewStore(path, now)
	if s == nil || err == nil {
		t.Fatalf("NewStore with unreadable parent = %v, %v", s, err)
	}
	if status := s.Status(); status.StorageError == "" || status.Pending != 0 {
		t.Fatalf("read failure status = %+v", status)
	}
	if strings.Contains(err.Error(), "SENSITIVE_READ_FAILURE_BYTES") || strings.Contains(s.Status().StorageError, "SENSITIVE_READ_FAILURE_BYTES") {
		t.Fatal("read diagnostic exposed original file bytes")
	}
	if err := s.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: at}}); err == nil {
		t.Fatal("recipient update hid the unavailable storage")
	}
	var ids []watchdapi.EventID
	for i := 0; i < 21; i++ {
		id, err := s.Publish(fmt.Sprintf("in RAM %02d", i))
		if err == nil {
			t.Fatal("Publish hid unavailable storage")
		}
		ids = append(ids, id)
	}
	assertNotificationIDs(t, pendingNotifications(t, s), ids[1:])
	clock = at.Add(12*time.Hour + time.Nanosecond)
	if messages := pendingNotifications(t, s); len(messages) != 0 {
		t.Fatalf("read-error fallback store is not bounded by TTL: %+v", messages)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("fallback store did not recover: %v", err)
	}
	if status := s.Status(); status.StorageError != "" || status.Pending != 0 {
		t.Fatalf("recovered fallback status = %+v", status)
	}
	reopened := newNotificationStore(t, path, now)
	if messages := pendingNotifications(t, reopened); len(messages) != 0 {
		t.Fatalf("expired fallback messages returned on restart: %+v", messages)
	}
}

func TestStore_CorruptOriginalIsPreserved(t *testing.T) {
	at := notificationTestTime()
	dir := t.TempDir()
	path := filepath.Join(dir, "watchd-notifications.json")
	original := []byte("{\"version\":1,\"private\":\"SENSITIVE_CORRUPT_BYTES\"")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	olderBackup := path + ".corrupt-existing"
	olderBytes := []byte("previous synthetic diagnostic")
	if err := os.WriteFile(olderBackup, olderBytes, 0600); err != nil {
		t.Fatal(err)
	}
	hasPreservedBackup := func() bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == filepath.Base(path) || entry.Name() == filepath.Base(olderBackup) {
				continue
			}
			if bytes.Equal(readNotificationFile(t, filepath.Join(dir, entry.Name())), original) {
				return true
			}
		}
		return false
	}
	s, err := NewStore(path, func() time.Time { return at })
	if s == nil || err == nil {
		t.Fatalf("corrupt NewStore = %v, %v", s, err)
	}
	if status := s.Status(); status.Pending != 0 {
		t.Fatalf("corrupt store has pending messages: %+v", status)
	}
	if strings.Contains(err.Error(), "SENSITIVE_CORRUPT_BYTES") || strings.Contains(s.Status().StorageError, "SENSITIVE_CORRUPT_BYTES") {
		t.Fatal("corruption diagnostic exposed the original bytes")
	}
	current, readErr := os.ReadFile(path)
	if (readErr != nil || !bytes.Equal(current, original)) && !hasPreservedBackup() {
		t.Fatal("NewStore replaced the corrupt original before preserving a backup")
	}
	rename := s.io.rename
	s.io.rename = func(from, to string) error {
		if to == path && !hasPreservedBackup() {
			t.Error("corrupt original has no backup before the replacement rename")
		}
		return rename(from, to)
	}
	t.Cleanup(func() { s.io.rename = rename })
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	id := publishNotification(t, s, "recovered store")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readNotificationFile(t, olderBackup); !bytes.Equal(got, olderBytes) {
		t.Fatal("recovery overwrote an older diagnostic backup")
	}
	if !hasPreservedBackup() {
		t.Fatal("corrupt original bytes were not preserved in a separate unique backup")
	}
	notificationDocument(t, path)
	reopened := newNotificationStore(t, path, func() time.Time { return at })
	assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{id})
}

func TestStore_CorruptionDoesNotReuseIDs(t *testing.T) {
	at := notificationTestTime()
	now := func() time.Time { return at }
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	oldID := publishNotification(t, s, "before corruption")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	broken := append(readNotificationFile(t, path), []byte("\nnot-json")...)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewStore(path, now)
	if recovered == nil || err == nil {
		t.Fatalf("corrupt restart = %v, %v", recovered, err)
	}
	replaceNotificationRecipients(t, recovered, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	newID := publishNotification(t, recovered, "after corruption")
	if newID == oldID {
		t.Fatalf("corruption reused event ID %q with the same injected clock", oldID)
	}
	oldEpoch, _ := splitNotificationID(t, oldID)
	newEpoch, sequence := splitNotificationID(t, newID)
	if newEpoch == oldEpoch {
		t.Fatalf("new queue reused corrupt store epoch %q", oldEpoch)
	}
	if err := recovered.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened := newNotificationStore(t, path, now)
	nextID := publishNotification(t, reopened, "normal restart after recovery")
	nextEpoch, nextSequence := splitNotificationID(t, nextID)
	if nextEpoch != newEpoch || nextSequence <= sequence {
		t.Fatalf("recovered epoch/sequence did not survive restart: %q after %q", nextID, newID)
	}
}

func TestStore_LostFileDoesNotReuseIDs(t *testing.T) {
	at := notificationTestTime()
	now := func() time.Time { return at }
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, now)
	oldID := publishNotification(t, s, "before file loss")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened := newNotificationStore(t, path, now)
	newID := publishNotification(t, reopened, "after file loss")
	oldEpoch, _ := splitNotificationID(t, oldID)
	newEpoch, _ := splitNotificationID(t, newID)
	if newID == oldID || oldEpoch == newEpoch {
		t.Fatalf("file loss reused event identity: %q after %q", newID, oldID)
	}
}

func TestStore_AtomicHealthSchema(t *testing.T) {
	at := notificationTestTime()
	now := func() time.Time { return at }
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	s := newNotificationStore(t, path, now)
	recipients := []watchdapi.Recipient{{ChatID: 100, FirstSeen: at}, {ChatID: 200, FirstSeen: at}}
	replaceNotificationRecipients(t, s, recipients...)
	first := publishNotification(t, s, "first recent event")
	second := publishNotification(t, s, "second recent event")
	if err := s.Ack(100, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("notification file mode = %v, %v, want 0600", info, err)
	}
	document := notificationDocument(t, path)
	health, ok := document["health"]
	if !ok || !json.Valid(health) {
		t.Fatal("version 1 schema has no valid subscription health section")
	}
	healthFixture := json.RawMessage(`{
		"subscription-synthetic-alpha": {
			"available": true,
			"endpoint": "192.0.2.10:443",
			"checked_at": "2026-10-05T10:00:00Z",
			"samples": [3, 0, 7],
			"metadata": {"host": "health.example.test", "note": null}
		},
		"subscription-synthetic-beta": {
			"available": false,
			"retry_count": 2,
			"endpoints": ["198.51.100.20:8443"]
		}
	}`)
	document["health"] = healthFixture
	fixture, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	before := readNotificationFile(t, path)
	oldFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	third := publishNotification(t, s, "third recent event")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := io.ReadAll(oldFile)
	if err != nil || !bytes.Equal(oldBytes, before) {
		t.Fatalf("successful publication overwrote the previous inode: %v", err)
	}
	epoch, firstSequence := splitNotificationID(t, first)
	secondEpoch, secondSequence := splitNotificationID(t, second)
	thirdEpoch, thirdSequence := splitNotificationID(t, third)
	if epoch != secondEpoch || epoch != thirdEpoch || firstSequence >= secondSequence || secondSequence >= thirdSequence {
		t.Fatalf("non-monotonic store sequence: %q, %q, %q", first, second, third)
	}
	s = newNotificationStore(t, path, now)
	messages := pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{second, third})
	assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{first, second, third})
	fourth := publishNotification(t, s, "recipients survived restart")
	fourthEpoch, fourthSequence := splitNotificationID(t, fourth)
	if fourthEpoch != epoch || fourthSequence <= thirdSequence {
		t.Fatalf("normal restart reset epoch or sequence: %q after %q", fourth, third)
	}
	assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 100), []watchdapi.EventID{second, third, fourth})
	replaceNotificationRecipients(t, s)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, append(recipients, watchdapi.Recipient{ChatID: 300, FirstSeen: at})...)
	messages = pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{second, third, fourth})
	assertNotificationIDs(t, notificationsForChat(messages, 300), []watchdapi.EventID{first, second, third, fourth})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s = newNotificationStore(t, path, now)
	messages = pendingNotifications(t, s)
	assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{second, third, fourth})
	assertNotificationIDs(t, notificationsForChat(messages, 300), []watchdapi.EventID{first, second, third, fourth})
	after := notificationDocument(t, path)
	var wantHealth, afterHealth any
	if err := json.Unmarshal(healthFixture, &wantHealth); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after["health"], &afterHealth); err != nil || !reflect.DeepEqual(wantHealth, afterHealth) {
		t.Fatalf("health section did not survive round-trip: %s, %v, want %s", after["health"], err, healthFixture)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replaced notification file mode = %v, %v, want 0600", info, err)
	}
	assertNoNotificationTempFiles(t, path)
}

func TestStore_SavesOutsideStateMutexAndKeepsNewerDirtyRevision(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	realSync := s.io.sync
	entered := make(chan struct{})
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var fileSyncs atomic.Int32
	var olderIODone, overlappingSaves atomic.Bool
	s.io.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			switch fileSyncs.Add(1) {
			case 1:
				close(entered)
				<-gate
			case 2:
				if !olderIODone.Load() {
					overlappingSaves.Store(true)
				}
				return errors.New("injected newer revision sync failure")
			}
		}
		err = realSync(f)
		if info.IsDir() {
			olderIODone.Store(true)
		}
		return err
	}
	type result struct {
		id  watchdapi.EventID
		err error
	}
	firstDone := make(chan result, 1)
	secondDone := make(chan result, 1)
	go func() {
		id, err := s.Publish("older revision")
		firstDone <- result{id: id, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first save never reached file sync")
	}
	go func() {
		id, err := s.Publish("newer revision")
		secondDone <- result{id: id, err: err}
	}()
	observed := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(time.Second)
		for {
			page, err := s.Pending("")
			status := s.Status()
			if err != nil {
				observed <- -1
				return
			}
			if len(page.Messages) == 2 && status.Pending == 2 {
				observed <- 2
				return
			}
			if time.Now().After(deadline) {
				observed <- len(page.Messages)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case count := <-observed:
		if count != 2 {
			t.Errorf("RAM mutation/read did not proceed during older save: %d messages", count)
		}
	case <-time.After(2 * time.Second):
		t.Error("state mutex is held while file sync is blocked")
	}
	if calls := fileSyncs.Load(); calls != 1 {
		t.Errorf("saves overlap while the older file sync is blocked: %d sync calls", calls)
	}
	release()
	var first, second result
	select {
	case first = <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("older save did not finish")
	}
	select {
	case second = <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("newer save did not finish")
	}
	if first.err != nil || second.err == nil {
		t.Fatalf("revision save results: older %v, newer %v", first.err, second.err)
	}
	if overlappingSaves.Load() {
		t.Error("newer save reached file sync before older directory sync completed")
	}
	if status := s.Status(); status.StorageError == "" || status.Pending != 2 {
		t.Fatalf("older save cleared newer dirty intent: %+v", status)
	}
	s.io.sync = realSync
	if err := s.Flush(); err != nil {
		t.Fatalf("newer revision retry: %v", err)
	}
	reopened := newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{first.id, second.id})
}

func TestStore_RunFlushesDirtyStateOnCancellation(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	restore := failNotificationIO(t, s, "rename")
	id, err := s.Publish("final save on cancellation")
	if err == nil {
		t.Fatal("injected rename failure did not fail Publish")
	}
	restore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if status := s.Status(); status.StorageError != "" {
		t.Fatalf("Run skipped its final dirty save: %+v", status)
	}
	reopened := newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{id})
}

func TestStore_RunRetriesDirtySaveEveryTenSeconds(t *testing.T) {
	at := notificationTestTime()
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	now := func() time.Time { return at }
	s := newNotificationStore(t, path, now)
	replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
	restore := failNotificationIO(t, s, "write")
	id, err := s.Publish("retry without another mutation")
	if err == nil {
		t.Fatal("injected write failure did not fail Publish")
	}
	restore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("notification Run goroutine did not stop")
		}
	})
	deadline := started.Add(15 * time.Second)
	for s.Status().StorageError != "" {
		if time.Now().After(deadline) {
			t.Fatal("Run did not retry a dirty save on the 10-second cadence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed < 9*time.Second {
		t.Fatalf("dirty save retried after %s, want the 10-second cadence", elapsed)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after successful retry")
	}
	reopened := newNotificationStore(t, path, now)
	assertNotificationIDs(t, pendingNotifications(t, reopened), []watchdapi.EventID{id})
}
