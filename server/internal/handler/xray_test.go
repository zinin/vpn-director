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
	return fmt.Sprintf("xray:select:%s:%d:%s", sub.ID, i, vpnconfig.ServerFingerprint(s))
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

	_, kb := xrayServersPage(sub, 0, true, nil, health{})
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			data = append(data, *b.CallbackData)
		}
	}
	if len(data) != 32 || data[30] != "xray:sub:0a1b2c3d:1" || data[31] != "xray:subs" {
		t.Fatalf("page 1: %d buttons, tail %v", len(data), data[30:])
	}

	_, kb = xrayServersPage(sub, 2, false, nil, health{})
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
		_, alpha := xrayServersPage(subs[0], 0, true, tc.active, health{})
		_, beta := xrayServersPage(subs[1], 0, true, tc.active, health{})
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
	if vpnconfig.ServerFingerprint(a) == vpnconfig.ServerFingerprint(b) {
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

func TestXray_TheExistingButtonFingerprintStillWorks(t *testing.T) {
	store := newSubsStore(twoGermanies()...)
	h, sender, gen, vpn := xrayHandler(store)
	_, kb := xrayServersPage(store.subs[0], 0, true, nil, health{})
	const callback = "xray:select:0a1b2c3d:0:84130acd"
	if got := *kb.InlineKeyboard[0][0].CallbackData; got != callback {
		t.Fatalf("callback %q", got)
	}
	h.HandleCallback(xrayCallback(callback))
	if gen.lastServer.Subscription != "0a1b2c3d" || gen.lastServer.Name != "Germany-1" || !vpn.restartCalled {
		t.Fatalf("generated %+v, restarted %t", gen.lastServer, vpn.restartCalled)
	}
	if a := store.cfg.Xray.ActiveServer; a == nil || a.Subscription != "0a1b2c3d" || !strings.Contains(sender.last(), "Alpha / Germany") {
		t.Fatalf("active %+v, reply %q", a, sender.last())
	}
}

func TestXray_MonitorGolden(t *testing.T) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha_[x]", Servers: servers(4)}
	beta := vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Servers: servers(1)}
	beta.Servers[0].Port = 100
	for _, tc := range monitorPageCases(sub) {
		t.Run(tc.name, func(t *testing.T) {
			for _, flow := range []struct {
				name          string
				subs          []vpnconfig.Subscription
				callback      string
				subscriptions bool
			}{
				{"single-command", []vpnconfig.Subscription{sub}, "", false},
				{"multiple-command", []vpnconfig.Subscription{sub, beta}, "", true},
				{"single-callback", []vpnconfig.Subscription{sub}, "xray:sub:0a1b2c3d:0", false},
				{"multiple-callback", []vpnconfig.Subscription{sub, beta}, "xray:sub:0a1b2c3d:0", false},
				{"single-back", []vpnconfig.Subscription{sub}, "xray:subs", false},
				{"multiple-back", []vpnconfig.Subscription{sub, beta}, "xray:subs", true},
			} {
				t.Run(flow.name, func(t *testing.T) {
					store := newSubsStore(flow.subs...)
					store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: sub.ID, Name: "S1", Address: "a.example.com", Port: 1}
					sender := &recordingSender{}
					h := NewXrayHandler(&Deps{Sender: sender, Config: store, Monitor: tc.api})
					if flow.callback != "" {
						h.HandleCallback(xrayCallback(flow.callback))
					} else {
						h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})
					}
					wantText := `Alpha\_\[x\]: выберите сервер`
					if flow.subscriptions {
						wantText = "Выберите подписку:"
					}
					if tc.note != "" {
						wantText += "\n" + tc.note
					}
					var wantKB tgbotapi.InlineKeyboardMarkup
					if flow.subscriptions {
						alphaLabel, betaLabel := "✓ Alpha_[x] (4)", "Beta (1)"
						if tc.marked {
							alphaLabel, betaLabel = "✓ Alpha_[x] (1/4)", "Beta (0/1)"
						}
						wantKB = tgbotapi.NewInlineKeyboardMarkup(
							tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(alphaLabel, "xray:sub:0a1b2c3d:0")),
							tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(betaLabel, "xray:sub:1b2c3d4e:0")),
						)
					} else {
						labels := []string{"✓ 1. S1", "2. S2", "3. S3", "4. S4"}
						if tc.marked {
							labels = []string{"✓ 🟢 1. S1 · 142 ms", "🔴 2. S2", "⚪ 3. S3", "⛔ 4. S4"}
						}
						wantKB = tgbotapi.NewInlineKeyboardMarkup(
							tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(labels[0], selectData(sub, 0)), tgbotapi.NewInlineKeyboardButtonData(labels[1], selectData(sub, 1))),
							tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(labels[2], selectData(sub, 2)), tgbotapi.NewInlineKeyboardButtonData(labels[3], selectData(sub, 3))),
						)
						if len(flow.subs) > 1 {
							wantKB.InlineKeyboard = append(wantKB.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("« Back", "xray:subs")))
						}
					}
					if got := sender.last(); got != wantText {
						t.Fatalf("got %q, want %q", got, wantText)
					}
					if !reflect.DeepEqual(sender.keyboard, wantKB) {
						t.Fatalf("keyboard got %+v, want %+v", sender.keyboard, wantKB)
					}
					if got := sender.edits; (flow.callback == "" && got != 0) || (flow.callback != "" && got != 1) {
						t.Fatalf("edits=%d for callback %q", got, flow.callback)
					}
				})
			}
		})
	}
}

func TestXray_PaginationRetainsEveryMonitorNote(t *testing.T) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha_[x]", Servers: servers(32)}
	beta := vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Servers: servers(1)}
	for _, tc := range monitorPageCases(sub) {
		t.Run(tc.name, func(t *testing.T) {
			for _, subs := range [][]vpnconfig.Subscription{{sub}, {sub, beta}} {
				sender := &recordingSender{}
				h := NewXrayHandler(&Deps{Sender: sender, Config: newSubsStore(subs...), Monitor: tc.api})
				h.HandleCallback(xrayCallback("xray:sub:0a1b2c3d:1"))
				want := `Alpha\_\[x\]: выберите сервер \(стр\. 2/2\)`
				if tc.note != "" {
					want += "\n" + tc.note
				}
				if got := sender.last(); got != want {
					t.Fatalf("got %q, want %q", got, want)
				}
				labels := []string{"31. S31", "32. S32"}
				if tc.marked {
					labels = []string{"⚪ 31. S31", "⛔ 32. S32"}
				}
				wantKB := tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(labels[0], selectData(sub, 30)), tgbotapi.NewInlineKeyboardButtonData(labels[1], selectData(sub, 31))),
					tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("◀", "xray:sub:0a1b2c3d:0")),
				)
				if len(subs) > 1 {
					wantKB.InlineKeyboard[1] = append(wantKB.InlineKeyboard[1], tgbotapi.NewInlineKeyboardButtonData("« Back", "xray:subs"))
				}
				if !reflect.DeepEqual(sender.keyboard, wantKB) {
					t.Fatalf("keyboard got %+v, want %+v", sender.keyboard, wantKB)
				}
			}
		})
	}
}
