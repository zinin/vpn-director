package notifications

import (
	"fmt"
	"path/filepath"
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
