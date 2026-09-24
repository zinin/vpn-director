package handler

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var subsNow = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

func subsHandler(store *subsStore) (*SubsHandler, *recordingSender) {
	sender := &recordingSender{}
	h := NewSubsHandler(&Deps{Sender: sender, Config: store})
	h.now = func() time.Time { return subsNow }
	return h, sender
}

func subsCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: "cb", Data: data, Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}
}

func chatText(s string) *tgbotapi.Message {
	return &tgbotapi.Message{Text: s, Chat: &tgbotapi.Chat{ID: 123}}
}

func TestSubs_ListsEachSubscriptionWithItsButtons(t *testing.T) {
	at := subsNow.Add(-2 * time.Hour)
	store := newSubsStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/token", Refreshed: at, Servers: make([]vpnconfig.Server, 32)},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Refreshed: at, Error: "download failed: HTTP 403", Servers: make([]vpnconfig.Server, 5)},
	)
	h, sender := subsHandler(store)

	h.HandleSubs(chatText("/subs"))

	got := sender.last()
	if !strings.Contains(got, `Alpha — sub\.example\.com — 32 servers — 2 h ago — OK`) || strings.Contains(got, "token") {
		t.Fatalf("list %q", got)
	}
	if !strings.Contains(got, "Beta — static list — 5 servers — 2 h ago — download failed: HTTP 403") {
		t.Fatalf("list %q", got)
	}
	want := []string{"subs:r:0a1b2c3d", "subs:n:0a1b2c3d", "subs:d:0a1b2c3d", "subs:n:1b2c3d4e", "subs:d:1b2c3d4e"}
	if got := sender.buttons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons %v, want %v", got, want)
	}
}

func TestSubs_NoSubscriptionSaysHowToAddOne(t *testing.T) {
	h, sender := subsHandler(newSubsStore())

	h.HandleSubs(chatText("/subs"))

	if !strings.Contains(sender.last(), "/import") || len(sender.buttons()) != 0 {
		t.Fatalf("reply %q, buttons %v", sender.last(), sender.buttons())
	}
}

func TestSubs_RefreshButtonRefreshesThatSubscription(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/token"})
	h, sender := subsHandler(store)
	h.httpClient = servingClient(t, serveBody(osloSubscription))

	h.HandleCallback(subsCallback("subs:r:0a1b2c3d"))

	if len(store.subs[0].Servers) != 1 || !strings.Contains(sender.all(), "Alpha: Imported 1 servers") {
		t.Fatalf("files %+v, messages %q", store.subs, sender.all())
	}
	if sender.edits == 0 {
		t.Fatal("the list was not redrawn")
	}
}

func TestSubs_RenameTakesTheNextMessage(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	if !strings.Contains(sender.last(), "new name for Alpha") {
		t.Fatalf("prompt %q", sender.last())
	}

	// A taken name is an answer the rename refuses: it is taken, and it uses the prompt up.
	if !h.HandleTextInput(chatText("beta")) || store.subs[0].Name != "Alpha" || !strings.Contains(sender.last(), "Not renamed") {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
	if h.HandleTextInput(chatText("Main")) {
		t.Fatal("a text without a prompt was taken")
	}

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	if !h.HandleTextInput(chatText(" Main ")) || store.subs[0].Name != "Main" {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
}

func TestSubs_ARenameWaitsFiveMinutesAtMost(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, _ := subsHandler(store)
	now := subsNow
	h.now = func() time.Time { return now }

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	now = now.Add(renameWait + time.Second)

	if h.HandleTextInput(chatText("Main")) || store.subs[0].Name != "Alpha" {
		t.Fatalf("a late answer renamed %+v", store.subs)
	}
}

func TestSubs_ClearStateEndsTheRename(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, _ := subsHandler(store)

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	h.ClearState(123)

	if h.HandleTextInput(chatText("Main")) {
		t.Fatal("the text was taken after ClearState")
	}
}

func TestSubs_DeleteAsksFirstAndWarnsAboutTheRunningServer(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: make([]vpnconfig.Server, 2)})
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:d:0a1b2c3d"))

	if len(store.subs) != 1 || !strings.Contains(sender.last(), "running Xray server comes from it") {
		t.Fatalf("files %d, question %q", len(store.subs), sender.last())
	}
	if got := sender.buttons(); !reflect.DeepEqual(got, []string{"subs:dy:0a1b2c3d", "subs:dn"}) {
		t.Fatalf("buttons %v", got)
	}

	h.HandleCallback(subsCallback("subs:dy:0a1b2c3d"))

	if len(store.subs) != 0 || !strings.Contains(sender.all(), "no longer in any subscription") {
		t.Fatalf("files %d, messages %q", len(store.subs), sender.all())
	}
}

func TestSubs_NoKeepsTheSubscription(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:d:0a1b2c3d"))
	h.HandleCallback(subsCallback("subs:dn"))

	if len(store.subs) != 1 || !reflect.DeepEqual(sender.buttons(), []string{"subs:n:0a1b2c3d", "subs:d:0a1b2c3d"}) {
		t.Fatalf("files %d, buttons %v", len(store.subs), sender.buttons())
	}
}

// The file is gone and only the config beside it is stale: the reply says the
// subscription was deleted, and still says when the running server came from it.
func TestSubs_ADeleteWhoseConfigWriteFailedSaysItDeleted(t *testing.T) {
	for _, active := range []bool{false, true} {
		store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
		if active {
			store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}
		}
		sender := &recordingSender{}
		h := NewSubsHandler(&Deps{Sender: sender, Config: configSaveFails{store}})

		h.HandleCallback(subsCallback("subs:dy:0a1b2c3d"))

		got := sender.all()
		if len(store.subs) != 0 || !strings.Contains(got, `Deleted Alpha, but xray\.servers sync failed: disk full`) ||
			strings.Contains(got, "no longer in any subscription: select another with /xray") != active {
			t.Errorf("active %v: files %d, messages %q", active, len(store.subs), got)
		}
	}
}
