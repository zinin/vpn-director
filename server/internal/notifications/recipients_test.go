package notifications

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func TestRecipients_FirstSeenRevokeAndRejoin(t *testing.T) {
	t.Run("recent history cap", func(t *testing.T) {
		at := notificationTestTime()
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		now := func() time.Time { return at }
		s := newNotificationStore(t, path, now)
		var ids []watchdapi.EventID
		for i := 1; i <= 21; i++ {
			ids = append(ids, publishNotification(t, s, fmt.Sprintf("history %02d", i)))
		}
		if messages := pendingNotifications(t, s); len(messages) != 0 {
			t.Fatalf("messages queued without recipients: %+v", messages)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		messages := pendingNotifications(t, s)
		if len(messages) != 20 || messages[0].EventID != ids[1] {
			t.Fatalf("new chat history = %+v, want the last 20 events", messages)
		}
		assertNotificationIDs(t, messages, ids[1:])
	})

	t.Run("inclusive FirstSeen and durable per-chat closed progress", func(t *testing.T) {
		firstSeen := notificationTestTime()
		clock := firstSeen.Add(-time.Nanosecond)
		now := func() time.Time { return clock }
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		s := newNotificationStore(t, path, now)
		before := publishNotification(t, s, "before this chat")
		clock = firstSeen
		acked := publishNotification(t, s, "exactly at FirstSeen")
		clock = firstSeen.Add(time.Minute)
		waiting := publishNotification(t, s, "still waiting")
		recipient := watchdapi.Recipient{ChatID: 100, FirstSeen: firstSeen}
		replaceNotificationRecipients(t, s, recipient)
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{acked, waiting})
		if err := s.Ack(100, acked); err != nil {
			t.Fatal(err)
		}
		replaceNotificationRecipients(t, s, recipient)
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{waiting})
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{waiting})

		replaceNotificationRecipients(t, s)
		if messages := pendingNotifications(t, s); len(messages) != 0 {
			t.Fatalf("revoked chat retained pending messages: %+v", messages)
		}
		clock = firstSeen.Add(2 * time.Minute)
		whileRevoked := publishNotification(t, s, "while revoked")
		if messages := pendingNotifications(t, s); len(messages) != 0 {
			t.Fatalf("revoked chat received new messages: %+v", messages)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		if messages := pendingNotifications(t, s); len(messages) != 0 {
			t.Fatalf("revocation was not durable: %+v", messages)
		}
		replaceNotificationRecipients(t, s,
			recipient,
			watchdapi.Recipient{ChatID: 200, FirstSeen: firstSeen.Add(time.Minute)},
			watchdapi.Recipient{ChatID: 300, FirstSeen: firstSeen},
		)
		messages := pendingNotifications(t, s)
		assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{waiting, whileRevoked})
		assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{waiting, whileRevoked})
		assertNotificationIDs(t, notificationsForChat(messages, 300), []watchdapi.EventID{acked, waiting, whileRevoked})
		for _, message := range messages {
			if message.EventID == before || message.At.Before(firstSeen) {
				t.Fatalf("new chat received pre-FirstSeen event: %+v", message)
			}
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 100), []watchdapi.EventID{waiting, whileRevoked})
	})

	t.Run("history TTL is inclusive", func(t *testing.T) {
		at := notificationTestTime()
		clock := at
		now := func() time.Time { return clock }
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		s := newNotificationStore(t, path, now)
		boundary := publishNotification(t, s, "twelve hours old")
		clock = at.Add(12 * time.Hour)
		fresh := publishNotification(t, s, "fresh")
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{boundary, fresh})
		replaceNotificationRecipients(t, s)
		clock = clock.Add(time.Nanosecond)
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{fresh})
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		replaceNotificationRecipients(t, s,
			watchdapi.Recipient{ChatID: 100, FirstSeen: at},
			watchdapi.Recipient{ChatID: 200, FirstSeen: at},
		)
		messages := pendingNotifications(t, s)
		assertNotificationIDs(t, notificationsForChat(messages, 100), []watchdapi.EventID{fresh})
		assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{fresh})
	})
}

func TestRecipients_ClosedProgressPrunedWithRecentHistory(t *testing.T) {
	progress := func(s *Store) (map[int64]map[watchdapi.EventID]time.Time, uint64, uint64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		closed := make(map[int64]map[watchdapi.EventID]time.Time, len(s.closed))
		for chatID, entries := range s.closed {
			closed[chatID] = make(map[watchdapi.EventID]time.Time, len(entries))
			for id, at := range entries {
				closed[chatID][id] = at
			}
		}
		return closed, s.revision, s.savedRevision
	}
	assertProgress := func(t *testing.T, s *Store, want map[int64]map[watchdapi.EventID]time.Time) {
		t.Helper()
		got, _, _ := progress(s)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("closed delivery progress %+v, want %+v", got, want)
		}
	}

	t.Run("recent cap eviction removes closed IDs and empty chat maps", func(t *testing.T) {
		at := notificationTestTime()
		now := func() time.Time { return at }
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		s := newNotificationStore(t, path, now)
		recipients := []watchdapi.Recipient{{ChatID: 100, FirstSeen: at}, {ChatID: 200, FirstSeen: at}}
		replaceNotificationRecipients(t, s, recipients...)
		var ids []watchdapi.EventID
		allClosed := make(map[watchdapi.EventID]time.Time)
		for i := 0; i < 20; i++ {
			id := publishNotification(t, s, fmt.Sprintf("closed history %02d", i))
			ids = append(ids, id)
			allClosed[id] = at
			if err := s.Ack(100, id); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				if err := s.Ack(200, id); err != nil {
					t.Fatal(err)
				}
			}
		}
		assertProgress(t, s, map[int64]map[watchdapi.EventID]time.Time{100: allClosed, 200: {ids[0]: at}})
		_, revision, saved := progress(s)
		if revision != saved {
			t.Fatal("fixture has unsaved progress before cap eviction")
		}

		last := publishNotification(t, s, "evicts the first closed event")

		delete(allClosed, ids[0])
		assertProgress(t, s, map[int64]map[watchdapi.EventID]time.Time{100: allClosed})
		_, prunedRevision, prunedSaved := progress(s)
		if prunedRevision != revision+2 || prunedSaved != prunedRevision {
			t.Fatalf("cap publication/pruning revision=%d saved=%d, want %d", prunedRevision, prunedSaved, revision+2)
		}
		if err := s.Ack(100, last); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, last)
		allClosed[last] = at
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		s = newNotificationStore(t, path, now)
		assertProgress(t, s, map[int64]map[watchdapi.EventID]time.Time{100: allClosed})
		replaceNotificationRecipients(t, s)
		replaceNotificationRecipients(t, s, recipients...)
		messages := pendingNotifications(t, s)
		assertNotificationIDs(t, notificationsForChat(messages, 100), nil)
		assertNotificationIDs(t, notificationsForChat(messages, 200), ids[1:])
		assertProgress(t, s, map[int64]map[watchdapi.EventID]time.Time{100: allClosed})
	})

	t.Run("closed TTL is inclusive and expires one nanosecond later", func(t *testing.T) {
		at := notificationTestTime()
		clock := at
		now := func() time.Time { return clock }
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		s := newNotificationStore(t, path, now)
		recipients := []watchdapi.Recipient{{ChatID: 100, FirstSeen: at}, {ChatID: 200, FirstSeen: at}}
		replaceNotificationRecipients(t, s, recipients...)
		old := publishNotification(t, s, "closed at the TTL boundary")
		for _, chatID := range []int64{100, 200} {
			if err := s.Ack(chatID, old); err != nil {
				t.Fatal(err)
			}
		}
		clock = at.Add(time.Hour)
		fresh := publishNotification(t, s, "newer closed progress survives")
		if err := s.Ack(100, fresh); err != nil {
			t.Fatal(err)
		}
		want := map[int64]map[watchdapi.EventID]time.Time{
			100: {old: at, fresh: clock}, 200: {old: at},
		}
		assertProgress(t, s, want)
		_, revision, saved := progress(s)
		clock = at.Add(12 * time.Hour)
		if status := s.Status(); status.Pending != 1 {
			t.Fatalf("inclusive TTL dropped newer pending delivery: %+v", status)
		}
		assertProgress(t, s, want)
		_, boundaryRevision, boundarySaved := progress(s)
		if boundaryRevision != revision || boundarySaved != saved {
			t.Fatal("exactly twelve-hour-old closed progress was pruned or dirtied")
		}

		clock = clock.Add(time.Nanosecond)
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{fresh})

		want = map[int64]map[watchdapi.EventID]time.Time{100: {fresh: at.Add(time.Hour)}}
		assertProgress(t, s, want)
		_, expiredRevision, expiredSaved := progress(s)
		if expiredRevision != revision+1 || expiredSaved != saved {
			t.Fatalf("TTL pruning revision=%d saved=%d, want %d / %d before Flush", expiredRevision, expiredSaved, revision+1, saved)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		_, flushedRevision, flushedSaved := progress(s)
		if flushedRevision != expiredRevision || flushedSaved != flushedRevision {
			t.Fatal("Flush did not persist the pruned closed progress revision")
		}
		s = newNotificationStore(t, path, now)
		assertProgress(t, s, want)
		replaceNotificationRecipients(t, s)
		replaceNotificationRecipients(t, s, recipients...)
		messages := pendingNotifications(t, s)
		assertNotificationIDs(t, notificationsForChat(messages, 100), nil)
		assertNotificationIDs(t, notificationsForChat(messages, 200), []watchdapi.EventID{fresh})
		assertProgress(t, s, want)
	})
}

func TestRecipients_EmptyListRevokesAllChats(t *testing.T) {
	for _, recipients := range [][]watchdapi.Recipient{nil, {}} {
		name := "empty"
		if recipients == nil {
			name = "nil"
		}
		t.Run(name, func(t *testing.T) {
			at := notificationTestTime()
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			now := func() time.Time { return at }
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s,
				watchdapi.Recipient{ChatID: 100, FirstSeen: at},
				watchdapi.Recipient{ChatID: 200, FirstSeen: at},
			)
			publishNotification(t, s, "before revoke")
			if err := s.ReplaceRecipients(recipients); err != nil {
				t.Fatal(err)
			}
			publishNotification(t, s, "after revoke")
			if status := s.Status(); status.Pending != 0 {
				t.Fatalf("revoked pending status = %+v", status)
			}
			if messages := pendingNotifications(t, s); len(messages) != 0 {
				t.Fatalf("empty recipients did not revoke all chats: %+v", messages)
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			reopened := newNotificationStore(t, path, now)
			publishNotification(t, reopened, "after restart")
			if messages := pendingNotifications(t, reopened); len(messages) != 0 {
				t.Fatalf("revoked chats returned after restart: %+v", messages)
			}
		})
	}
}
