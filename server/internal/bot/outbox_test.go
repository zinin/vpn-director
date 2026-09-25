package bot

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
)

// flakySender is a recordingSender whose SendPlain can fail: fail answers each
// attempt, and a nil answer lets the message through.
type flakySender struct {
	recordingSender
	mu       sync.Mutex
	fail     func(chatID int64, text string) error
	attempts int
}

func (s *flakySender) SendPlain(chatID int64, text string) error {
	s.mu.Lock()
	s.attempts++
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if err := fail(chatID, text); err != nil {
			return err
		}
	}
	return s.recordingSender.SendPlain(chatID, text)
}

func (s *flakySender) attempted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// failOnce fails the first attempt with err and lets every later one through.
func failOnce(err error) func(int64, string) error {
	var failed atomic.Bool
	return func(int64, string) error {
		if failed.CompareAndSwap(false, true) {
			return err
		}
		return nil
	}
}

// errNoPathYet is what the path client answers while no path to Telegram is
// alive: a transport error, not an answer from Telegram.
var errNoPathYet = errors.New(`Post "https://api.telegram.org/bot…/sendMessage": telegram API unreachable on every path`)

// notifyBot is a bot whose active chats are alice (100) and, with bob, bob
// (200), a clock the test moves, and a path to Telegram the test turns on and
// off; it starts with the path up.
func notifyBot(t *testing.T, sender *flakySender, withBob bool) (*Bot, *time.Time, *atomic.Bool) {
	t.Helper()
	store := chatstore.New(filepath.Join(t.TempDir(), "chats.json"))
	users := []string{"alice"}
	if err := store.RecordInteraction("alice", 100); err != nil {
		t.Fatal(err)
	}
	if withBob {
		users = append(users, "bob")
		if err := store.RecordInteraction("bob", 200); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Date(2026, 9, 25, 10, 46, 0, 0, time.Local)
	live := &atomic.Bool{}
	live.Store(true)
	b := &Bot{auth: NewAuth(users), sender: sender, chatStore: store}
	b.pathLive = live.Load
	b.outbox.now = func() time.Time { return clock }
	return b, &clock, live
}

// The outage of 2026-09-25: the outbound died, and Telegram was reachable only
// through it. The message waits for a path instead of being lost.
func TestNotifyActiveChats_WaitsForAPathToTelegram(t *testing.T) {
	sender := &flakySender{}
	b, _, live := notifyBot(t, sender, false)
	live.Store(false)

	b.notifyActiveChats("Xray outbound is dead")
	if n := sender.attempted(); n != 0 {
		t.Fatalf("%d send attempts with no path to Telegram, want none", n)
	}

	live.Store(true)
	b.flushNotifications()
	want := []sentPlain{{chatID: 100, text: "Xray outbound is dead"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// The path manager can still name a path that has just died: the send fails,
// and the message stays for the next attempt.
func TestNotifyActiveChats_KeepsAMessageWhoseSendFailed(t *testing.T) {
	sender := &flakySender{fail: failOnce(errNoPathYet)}
	b, _, _ := notifyBot(t, sender, false)

	b.notifyActiveChats("Xray outbound is dead")
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("delivered %+v through a failing send", got)
	}

	b.flushNotifications()
	want := []sentPlain{{chatID: 100, text: "Xray outbound is dead"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// Telegram's own refusal - the user blocked the bot, a bad request - never
// changes on a retry, and would hold the chat's queue forever.
func TestNotifyActiveChats_RetriesOnlyWhatTelegramDidNotRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kept bool
	}{
		{"no path", errNoPathYet, true},
		{"too many requests", &tgbotapi.Error{Code: 429, Message: "Too Many Requests: retry after 5"}, true},
		{"bad gateway", &tgbotapi.Error{Code: 502, Message: "Bad Gateway"}, true},
		{"blocked by the user", &tgbotapi.Error{Code: 403, Message: "Forbidden: bot was blocked by the user"}, false},
		{"bad request", &tgbotapi.Error{Code: 400, Message: "Bad Request: chat not found"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender := &flakySender{fail: failOnce(tc.err)}
			b, _, _ := notifyBot(t, sender, false)

			b.notifyActiveChats("Xray outbound is dead")
			b.flushNotifications()

			delivered := len(sender.snapshot()) == 1
			if delivered != tc.kept {
				t.Fatalf("delivered on the retry: %v, want %v (sent %+v)", delivered, tc.kept, sender.snapshot())
			}
			if n := sender.attempted(); tc.kept && n != 2 || !tc.kept && n != 1 {
				t.Fatalf("%d attempts", n)
			}
		})
	}
}

// A message never overtakes one still waiting: the chat reads the outage in
// the order it happened.
func TestNotifyActiveChats_ANewMessageWaitsBehindAnOlderOne(t *testing.T) {
	sender := &flakySender{fail: failOnce(errNoPathYet)}
	b, _, _ := notifyBot(t, sender, false)

	b.notifyActiveChats("Xray outbound is dead")
	b.notifyActiveChats("Xray moved to Beta / Germany-1")

	want := []sentPlain{
		{chatID: 100, text: "Xray outbound is dead"},
		{chatID: 100, text: "Xray moved to Beta / Germany-1"},
	}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// A message delivered a minute or more after it happened says when it
// happened; a fresh one reads as it always did.
func TestNotifyActiveChats_ALateMessageSaysWhenItHappened(t *testing.T) {
	sender := &flakySender{}
	b, clock, live := notifyBot(t, sender, false)
	live.Store(false)

	*clock = time.Date(2026, 9, 25, 10, 46, 59, 0, time.Local)
	b.notifyActiveChats("Xray outbound is dead")
	*clock = time.Date(2026, 9, 25, 10, 47, 49, 0, time.Local)
	b.notifyActiveChats("Xray moved to Beta / Germany-1")

	*clock = time.Date(2026, 9, 25, 10, 48, 5, 0, time.Local)
	live.Store(true)
	b.flushNotifications()

	want := []sentPlain{
		{chatID: 100, text: "(10:46, delayed) Xray outbound is dead"},
		{chatID: 100, text: "Xray moved to Beta / Germany-1"},
	}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// Past midnight the time alone would point at the wrong day.
func TestNotifyActiveChats_ALateMessageFromAnotherDayNamesTheDay(t *testing.T) {
	sender := &flakySender{}
	b, clock, live := notifyBot(t, sender, false)
	live.Store(false)

	*clock = time.Date(2026, 9, 24, 23, 50, 0, 0, time.Local)
	b.notifyActiveChats("Xray outbound is dead")

	*clock = time.Date(2026, 9, 25, 0, 20, 0, 0, time.Local)
	live.Store(true)
	b.flushNotifications()

	want := []sentPlain{{chatID: 100, text: "(Sep 24 23:50, delayed) Xray outbound is dead"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// A long outage keeps the latest 20 messages of a chat, not every one.
func TestNotifyActiveChats_KeepsTheLatestTwentyMessages(t *testing.T) {
	sender := &flakySender{}
	b, _, live := notifyBot(t, sender, false)
	live.Store(false)

	texts := []string{
		"m01", "m02", "m03", "m04", "m05", "m06", "m07", "m08", "m09", "m10", "m11",
		"m12", "m13", "m14", "m15", "m16", "m17", "m18", "m19", "m20", "m21",
	}
	for _, text := range texts {
		b.notifyActiveChats(text)
	}

	live.Store(true)
	b.flushNotifications()
	var got []string
	for _, s := range sender.snapshot() {
		got = append(got, s.text)
	}
	if want := texts[1:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %q, want %q", got, want)
	}
}

// A message older than 12 hours tells nothing the chat can still act on.
func TestNotifyActiveChats_DropsAMessageOlderThanTwelveHours(t *testing.T) {
	sender := &flakySender{}
	b, clock, live := notifyBot(t, sender, false)
	live.Store(false)

	*clock = time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	b.notifyActiveChats("Xray outbound is dead")
	*clock = time.Date(2026, 9, 25, 20, 0, 0, 0, time.Local)
	b.notifyActiveChats("LAN clients back on Xray")

	*clock = time.Date(2026, 9, 25, 21, 1, 0, 0, time.Local)
	live.Store(true)
	b.flushNotifications()

	want := []sentPlain{{chatID: 100, text: "(20:00, delayed) LAN clients back on Xray"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// Each chat waits on its own sends only.
func TestNotifyActiveChats_AChatThatFailsDoesNotHoldUpAnother(t *testing.T) {
	sender := &flakySender{fail: func(chatID int64, _ string) error {
		if chatID == 100 {
			return errNoPathYet
		}
		return nil
	}}
	b, _, _ := notifyBot(t, sender, true)

	b.notifyActiveChats("Xray outbound is dead")

	want := []sentPlain{{chatID: 200, text: "Xray outbound is dead"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
	if !b.outbox.pending() {
		t.Fatal("alice's message is gone; it should wait for her next attempt")
	}
}

// Nothing else is sent after an outage ends, so the bot itself has to try
// again once a path is back.
func TestRetryNotifications_DeliversOnceAPathIsBack(t *testing.T) {
	sender := &flakySender{}
	b, _, live := notifyBot(t, sender, false)
	live.Store(false)
	b.notifyActiveChats("Xray outbound is dead")
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("delivered %+v with no path to Telegram", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.retryNotifications(ctx, 5*time.Millisecond)

	live.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for len(sender.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the waiting message was never delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := []sentPlain{{chatID: 100, text: "Xray outbound is dead"}}
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}
