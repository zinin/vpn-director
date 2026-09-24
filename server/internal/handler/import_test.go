package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func importCommand(text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 123},
		Text:     text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 7}},
	}
}

func importHandler(t *testing.T, store *subsStore, h http.HandlerFunc) (*ImportHandler, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	imp := NewImportHandler(&Deps{Sender: sender, Config: store})
	imp.httpClient = servingClient(t, h)
	return imp, sender
}

func TestImport_AddsASubscriptionUnderTheNameGiven(t *testing.T) {
	store := newSubsStore()
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import https://93.184.216.34/s/token Alpha VPN"))

	if len(store.subs) != 1 || store.subs[0].Name != "Alpha VPN" || store.subs[0].URL != "https://93.184.216.34/s/token" {
		t.Fatalf("files %+v", store.subs)
	}
	if got := sender.last(); !strings.Contains(got, "Alpha VPN: Imported 1 servers:") || strings.Contains(got, "/s/token") {
		t.Fatalf("reply %q", got)
	}
}

func TestImport_ASavedLinkIsRefreshedAndSaysSo(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/token"})
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import https://93.184.216.34/s/token"))

	if len(store.subs) != 1 || len(store.subs[0].Servers) != 1 || store.subs[0].Name != "Alpha" {
		t.Fatalf("files %+v", store.subs)
	}
	if !strings.Contains(sender.last(), "saved already") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestImport_AloneRefreshesEverySubscription(t *testing.T) {
	store := newSubsStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
		vpnconfig.Subscription{ID: "2c3d4e5f", Name: "Gamma", URL: "https://93.184.216.34/b"},
	)
	h, sender := importHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(osloSubscription))
	})

	h.HandleImport(importCommand("/import"))

	got := sender.last()
	if !strings.Contains(got, "Alpha: Imported 1 servers") || !strings.Contains(got, "Gamma: download failed: HTTP 403") || strings.Contains(got, "Beta") {
		t.Fatalf("reply %q", got)
	}
	if store.subs[2].Error != "download failed: HTTP 403" {
		t.Fatalf("Gamma %+v", store.subs[2])
	}
}

func TestImport_AloneWithoutALinkExplains(t *testing.T) {
	h, sender := importHandler(t, newSubsStore(vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"}), serveBody(""))

	h.HandleImport(importCommand("/import"))

	if !strings.Contains(sender.last(), "Usage") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestImport_RefusesAPlainHTTPLink(t *testing.T) {
	store := newSubsStore()
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import http://93.184.216.34/s/token"))

	if len(store.subs) != 0 || !strings.Contains(sender.last(), "nothing was imported") {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
}

// A *url.Error's text is the whole link, and the token must not reach the chat.
func TestImport_AFailedDownloadNamesNoLink(t *testing.T) {
	store := newSubsStore()
	sender := &recordingSender{}
	h := NewImportHandler(&Deps{Sender: sender, Config: store})
	h.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	h.HandleImport(importCommand("/import https://93.184.216.34/s/secret-token"))

	if strings.Contains(sender.all(), "secret-token") || !strings.Contains(sender.last(), "download failed: connection refused") {
		t.Fatalf("messages %q", sender.all())
	}
}

// The subscription file is written and only the config beside it is not: the
// add happened, and the reply must not tell the user that nothing was imported.
func TestImport_AConfigWriteThatFailsAfterTheFileSaysTheSubscriptionIsSaved(t *testing.T) {
	store := newSubsStore()
	sender := &recordingSender{}
	h := NewImportHandler(&Deps{Sender: sender, Config: configSaveFails{store}})
	h.httpClient = servingClient(t, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import https://93.184.216.34/s/token"))

	got := sender.last()
	if len(store.subs) != 1 || !strings.Contains(got, `the subscription is saved, but xray\.servers sync failed: disk full`) || strings.Contains(got, "nothing was imported") {
		t.Fatalf("files %+v, reply %q", store.subs, got)
	}
}
