// internal/handler/health_test.go
package handler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeMonitor is vpn-director-watchd for the handler tests.
type fakeMonitor struct {
	snap watchdapi.Snapshot
	err  error
}

func (f *fakeMonitor) Monitor(context.Context) (watchdapi.Snapshot, error) { return f.snap, f.err }
func (f *fakeMonitor) Check(context.Context, []string) (int, error)        { return 0, nil }

// monitored is a subscription of two servers, S1 alive at 142 ms and S2 dead,
// and the monitor that says so.
func monitored() ([]vpnconfig.Subscription, *fakeMonitor) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(2)}
	keys := func(i int) string {
		s := sub.Servers[i]
		s.Subscription = sub.ID
		return endpoint.Keys(s)[0]
	}
	return []vpnconfig.Subscription{sub}, &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		keys(0): {Status: watchdapi.StatusAlive, LatencyMS: 142},
		keys(1): {Status: watchdapi.StatusDead, Error: "timeout"},
	}}}
}

func TestBuildServersPage_MarksEachServerWithItsStatus(t *testing.T) {
	subs, mon := monitored()

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(mon, subs))

	for _, want := range []string{"*Alpha* — 1/2 живы", "🟢 1\\. S1", "· 142 ms", "🔴 2\\. S2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("page lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "мониторинг") {
		t.Fatalf("a monitor that checks got a note:\n%s", text)
	}
}

func TestBuildServersPage_ASilentDaemonLeavesTheMarksOut(t *testing.T) {
	subs, _ := monitored()

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(&fakeMonitor{err: errors.New("no socket")}, subs))

	if !strings.Contains(text, "мониторинг не запущен") || strings.Contains(text, "🟢") || strings.Contains(text, "живы") {
		t.Fatalf("page:\n%s", text)
	}
}

// A monitor that is not checking says why: its statuses may be old.
func TestBuildServersPage_AStoppedMonitorSaysSo(t *testing.T) {
	subs, mon := monitored()
	mon.snap.State = watchdapi.StateStopped

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(mon, subs))

	if !strings.Contains(text, "мониторинг приостановлен") {
		t.Fatalf("page:\n%s", text)
	}
}

func TestXray_ButtonsCarryTheStatus(t *testing.T) {
	subs, mon := monitored()
	h := monitorHealth(mon, subs)

	_, kb := xraySubscriptions(append(subs, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Servers: servers(1)}), nil, h)
	if got := kb.InlineKeyboard[0][0].Text; got != "Alpha (1/2)" {
		t.Fatalf("subscription button %q", got)
	}
	_, kb = xrayServersPage(subs[0], 0, false, nil, h)
	if a, b := kb.InlineKeyboard[0][0].Text, kb.InlineKeyboard[0][1].Text; a != "🟢 1. S1 · 142 ms" || b != "🔴 2. S2" {
		t.Fatalf("server buttons %q, %q", a, b)
	}
}

// monitorPageCases pins the exact notes and all four server statuses.
func monitorPageCases(sub vpnconfig.Subscription) []struct {
	name   string
	api    watchdapi.API
	note   string
	marked bool
} {
	snap := watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{}}
	statuses := []watchdapi.Status{watchdapi.StatusAlive, watchdapi.StatusDead, watchdapi.StatusUnknown, watchdapi.StatusRejected}
	for i, s := range sub.Servers {
		s.Subscription = sub.ID
		for _, key := range endpoint.Keys(s) {
			snap.Endpoints[key] = watchdapi.EndpointState{Status: statuses[i%len(statuses)], LatencyMS: 142}
		}
	}
	cases := []struct {
		name   string
		api    watchdapi.API
		note   string
		marked bool
	}{
		{"nil", nil, "мониторинг не запущен", false},
		{"silent", &fakeMonitor{err: errors.New("no socket")}, "мониторинг не запущен", false},
		{"ok", &fakeMonitor{snap: snap}, "", true},
	}
	for _, state := range []struct {
		state watchdapi.State
		note  string
	}{
		{watchdapi.StateStopped, "мониторинг приостановлен: VPN Director остановлен"},
		{watchdapi.StateDisabled, "мониторинг выключен в настройках"},
		{watchdapi.StateNoXray, "мониторинг: xray не найден"},
		{watchdapi.StateWANDown, "мониторинг: WAN недоступен, статусы сохранены"},
		{watchdapi.StateProberError, "мониторинг: пробный Xray не запускается"},
	} {
		idle := snap
		idle.State = state.state
		cases = append(cases, struct {
			name   string
			api    watchdapi.API
			note   string
			marked bool
		}{string(state.state), &fakeMonitor{snap: idle}, state.note, true})
	}
	return cases
}

func TestMonitorHealth_FoldsEveryAddressAndDropsChangedEndpoints(t *testing.T) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(1)}
	sub.Servers[0].IPs = []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
	keys := endpoint.Keys(sub.Servers[0])
	mon := &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		keys[0]: {Status: watchdapi.StatusDead},
		keys[1]: {Status: watchdapi.StatusAlive, LatencyMS: 142},
		keys[2]: {Status: watchdapi.StatusAlive, LatencyMS: 71},
	}}}
	h := monitorHealth(mon, []vpnconfig.Subscription{sub})
	if got, ok := h.of(sub.ID, 0); !ok || got.Status != watchdapi.StatusAlive || got.LatencyMS != 71 || h.alive[sub.ID] != 1 {
		t.Fatalf("health %+v, present %t, alive %d", got, ok, h.alive[sub.ID])
	}
	for _, index := range []int{-1, 1} {
		if _, ok := h.of(sub.ID, index); ok {
			t.Fatalf("out-of-range index %d has a status", index)
		}
	}
	if _, ok := h.of("1b2c3d4e", 0); ok {
		t.Fatal("another subscription has a status")
	}
	sub.Servers[0].UUID = "synthetic-rotated-credential"
	h = monitorHealth(mon, []vpnconfig.Subscription{sub})
	if got, ok := h.of(sub.ID, 0); !ok || got.Status != watchdapi.StatusUnknown || h.alive[sub.ID] != 0 {
		t.Fatalf("changed endpoint inherited old health: %+v, present %t, alive %d", got, ok, h.alive[sub.ID])
	}
}

func TestMonitorHealth_AHungSocketIsBoundedByTheClient(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "w.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(ln) }()
	subs, _ := monitored()
	started := time.Now()
	h := monitorHealth(watchdapi.NewClient(socket), subs)
	elapsed := time.Since(started)
	if h.ok || h.header() != "мониторинг не запущен" {
		t.Fatalf("hung daemon health %+v", h)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("client's 2 s bound took %s", elapsed)
	}
}

func TestHealthHeaders_EscapeMarkdownOnEveryPage(t *testing.T) {
	subs, _ := monitored()
	h := health{ok: true, note: "[idle] _note_ (old)!"}
	const escaped = `\[idle\] \_note\_ \(old\)\!`
	servers, _ := buildServersPage(serverLines(subs), 1, 0, h)
	subscriptions, _ := xraySubscriptions(subs, nil, h)
	xray, _ := xrayServersPage(subs[0], 0, false, nil, h)
	for _, text := range []string{servers, subscriptions, xray} {
		if strings.Count(text, escaped) != 1 {
			t.Fatalf("escaped note missing or repeated: %q", text)
		}
	}
}
