// internal/handler/xray_test.go
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockXrayGenerator for testing
type mockXrayGenerator struct {
	lastServer vpnconfig.Server
	err        error
}

func (m *mockXrayGenerator) GenerateConfig(server vpnconfig.Server, _ ...service.InboundPorts) error {
	m.lastServer = server
	return m.err
}

// servers is n servers named S1..Sn at a.example.com, ports 1..n.
func servers(n int) []vpnconfig.Server {
	out := make([]vpnconfig.Server, n)
	for i := range out {
		out[i] = vpnconfig.Server{Name: fmt.Sprintf("S%d", i+1), Address: "a.example.com", Port: i + 1, IPs: []string{"192.0.2.1"}}
	}
	return out
}

// twoGermanies is two subscriptions that both name a server Germany-1.
func twoGermanies() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de.example.com", Port: 443}}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de.example.com", Port: 443}}},
	}
}

func xrayCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: "cb", Data: data, Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}
}

func xrayHandler(store *subsStore) (*XrayHandler, *recordingSender, *mockXrayGenerator, *mockVPNDirector) {
	sender, gen, vpn := &recordingSender{}, &mockXrayGenerator{}, &mockVPNDirector{}
	return NewXrayHandler(&Deps{Sender: sender, Config: store, Xray: gen, VPN: vpn}), sender, gen, vpn
}

func selectData(sub vpnconfig.Subscription, i int) string {
	s := sub.Servers[i]
	s.Subscription = sub.ID
	return fmt.Sprintf("xray:select:%s:%d:%s", sub.ID, i, serverFingerprint(s))
}

func TestXray_OneSubscriptionOpensOnItsServers(t *testing.T) {
	h, sender, _, _ := xrayHandler(newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(2)}))

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	b := sender.buttons()
	if len(b) != 2 || !strings.HasPrefix(b[0], "xray:select:0a1b2c3d:0:") {
		t.Fatalf("buttons %v", b)
	}
}

func TestXray_SeveralSubscriptionsOpenOnTheSubscriptions(t *testing.T) {
	store := newSubsStore(twoGermanies()...)
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "de.example.com", Port: 443}
	h, sender, _, _ := xrayHandler(store)

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if got := sender.buttons(); !reflect.DeepEqual(got, []string{"xray:sub:0a1b2c3d:0", "xray:sub:1b2c3d4e:0"}) {
		t.Fatalf("buttons %v", got)
	}
	rows := sender.keyboard.InlineKeyboard
	if rows[0][0].Text != "Alpha (1)" || rows[1][0].Text != "✓ Beta (1)" {
		t.Fatalf("labels %q, %q", rows[0][0].Text, rows[1][0].Text)
	}
}

func TestXray_ASubscriptionIsListedThirtyServersAPage(t *testing.T) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(62)}

	_, kb := xrayServersPage(sub, 0, true, nil)
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			data = append(data, *b.CallbackData)
		}
	}
	if len(data) != 32 || data[30] != "xray:sub:0a1b2c3d:1" || data[31] != "xray:subs" {
		t.Fatalf("page 1: %d buttons, tail %v", len(data), data[30:])
	}

	_, kb = xrayServersPage(sub, 2, false, nil)
	last := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	if len(last) != 1 || *last[0].CallbackData != "xray:sub:0a1b2c3d:1" {
		t.Fatalf("page 3 navigation %v", last)
	}
}

// Both subscriptions list Germany-1 at the same address and port. The ✓ marks
// the running server in the subscription it came from, not its twin, and a
// record from before subscriptions marks neither.
func TestXray_TheCheckMarksOnlyTheRunningServer(t *testing.T) {
	subs := twoGermanies()
	for _, tc := range []struct {
		name        string
		active      *vpnconfig.ActiveServer
		alpha, beta string
	}{
		{"Beta's", &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "de.example.com", Port: 443}, "1. Germany-1", "✓ 1. Germany-1"},
		{"a record without a subscription", &vpnconfig.ActiveServer{Name: "Germany-1", Address: "de.example.com", Port: 443}, "1. Germany-1", "1. Germany-1"},
	} {
		_, alpha := xrayServersPage(subs[0], 0, true, tc.active)
		_, beta := xrayServersPage(subs[1], 0, true, tc.active)
		if a, b := alpha.InlineKeyboard[0][0].Text, beta.InlineKeyboard[0][0].Text; a != tc.alpha || b != tc.beta {
			t.Errorf("%s: Alpha %q, Beta %q", tc.name, a, b)
		}
	}
}

// Two subscriptions can list the very same name, address and port: the
// fingerprint tells their buttons apart.
func TestXray_TheFingerprintTellsSubscriptionsApart(t *testing.T) {
	a, b := twoGermanies()[0].Servers[0], twoGermanies()[1].Servers[0]
	a.Subscription, b.Subscription = "0a1b2c3d", "1b2c3d4e"
	if serverFingerprint(a) == serverFingerprint(b) {
		t.Fatal("one fingerprint for two subscriptions")
	}
}

func TestXray_SelectsTheServerOfTheSubscriptionTapped(t *testing.T) {
	subs := twoGermanies()
	store := newSubsStore(subs...)
	h, sender, gen, _ := xrayHandler(store)

	h.HandleCallback(xrayCallback(selectData(subs[1], 0)))

	if gen.lastServer.Subscription != "1b2c3d4e" {
		t.Fatalf("generated %+v", gen.lastServer)
	}
	if a := store.cfg.Xray.ActiveServer; a == nil || a.Subscription != "1b2c3d4e" {
		t.Fatalf("active %+v", a)
	}
	if !strings.Contains(sender.last(), "Beta / Germany") {
		t.Fatalf("reply %q", sender.last())
	}
}

// A keyboard sent before subscriptions indexes a list that is gone, and a
// button whose server moved names another one. Neither switches anything.
func TestXray_AStaleButtonSwitchesNothing(t *testing.T) {
	subs := twoGermanies()
	for _, data := range []string{
		"xray:select:0",
		"xray:select:0:abcd1234",
		"xray:select:0a1b2c3d:0:00000000",
		"xray:select:ffffffff:0:00000000",
		"xray:select:0a1b2c3d:5:00000000",
	} {
		h, sender, gen, _ := xrayHandler(newSubsStore(subs...))
		h.HandleCallback(xrayCallback(data))
		if gen.lastServer.Name != "" || !strings.Contains(sender.last(), "run /xray again") {
			t.Errorf("%s: generated %+v, reply %q", data, gen.lastServer, sender.last())
		}
	}
}

func TestXray_AGenerationThatFailsIsReported(t *testing.T) {
	subs := twoGermanies()
	h, sender, gen, vpn := xrayHandler(newSubsStore(subs...))
	gen.err = errors.New("xray rejected the config")

	h.HandleCallback(xrayCallback(selectData(subs[0], 0)))

	if !strings.Contains(sender.last(), "xray rejected the config") || vpn.restartCalled {
		t.Fatalf("reply %q, restarted %v", sender.last(), vpn.restartCalled)
	}
}

func TestXray_NoServer(t *testing.T) {
	h, sender, _, _ := xrayHandler(newSubsStore())

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if !strings.Contains(sender.last(), "/import") || len(sender.buttons()) != 0 {
		t.Fatalf("reply %q", sender.last())
	}
}

// « Back after every subscription went: the message says there is nothing to
// choose, and its keyboard goes as an empty array - Telegram refuses a null
// one, and the old buttons would stay.
func TestXray_BackWithNothingLeftClearsTheKeyboard(t *testing.T) {
	store := newSubsStore(twoGermanies()...)
	h, sender, _, _ := xrayHandler(store)
	store.subs = nil

	h.HandleCallback(xrayCallback("xray:subs"))

	kb, err := json.Marshal(sender.keyboard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.last(), "/import") || string(kb) != `{"inline_keyboard":[]}` {
		t.Fatalf("reply %q, keyboard %s", sender.last(), kb)
	}
}
