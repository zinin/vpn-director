package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// subOf is a subscription whose servers carry the given names, one address each.
func subOf(id, name, url string, servers ...string) vpnconfig.Subscription {
	s := vpnconfig.Subscription{ID: id, Name: name, URL: url}
	for i, n := range servers {
		s.Servers = append(s.Servers, vpnconfig.Server{Name: n, Address: strings.ToLower(n) + ".example", Port: 443, IPs: []string{fmt.Sprintf("203.0.113.%d", 10+i)}})
	}
	return s
}

// fetchFrom serves each link the list subs holds for it, and fails the links in down.
func fetchFrom(subs []vpnconfig.Subscription, down ...string) func(context.Context, string) ([]vpnconfig.Server, error) {
	lists := map[string][]vpnconfig.Server{}
	for _, s := range subs {
		lists[s.URL] = s.Servers
	}
	return func(_ context.Context, url string) ([]vpnconfig.Server, error) {
		for _, d := range down {
			if d == url {
				return nil, errors.New("HTTP 403")
			}
		}
		return lists[url], nil
	}
}

// walkRig is a failed-over watch whose Generate records each server it is
// handed as "<subscription>/<name>" and names it in active_server, and whose
// probe passes once the walk has written live.
func walkRig(f *fake, live string, events *[]string) *Watch {
	w := runningWatch(f.watch())
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		*events = append(*events, s.Subscription+"/"+s.Name)
		vpnconfig.RecordWalkedServer(f.cfg, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		a := f.cfg.Xray.ActiveServer
		if len(*events) > 0 && a != nil && a.Subscription+"/"+a.Name == live {
			return nil
		}
		return errProbe
	}
	return w
}

func waveFake(subs ...vpnconfig.Subscription) *fake {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0), subs: subs}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: subs[0].ID, Name: subs[0].Servers[0].Name,
		Address: subs[0].Servers[0].Address, Port: 443}
	return f
}

// Alpha fails as a whole. After OwnFirst of its servers the walk turns to
// Beta, and the clients come back on Beta's first live server.
func TestWave_TheWalkReachesTheOtherSubscription(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1", "A2", "A3", "A4", "A5")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1", "B2", "B3")
	f := waveFake(alpha, beta)
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "A2", Address: "a2.example", Port: 443}
	var events []string
	w := walkRig(f, "bbbbbbbb/B2", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})

	w.Tick(context.Background())

	want := []string{"aaaaaaaa/A2", "aaaaaaaa/A1", "aaaaaaaa/A3", "bbbbbbbb/B1", "aaaaaaaa/A4", "bbbbbbbb/B2"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
	if n := countNotes(f.notes, "LAN clients back on Xray; server Beta / B2"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// Every subscription downloads at once: a slow provider does not hold the others back.
func TestWave_EverySubscriptionDownloadsAtOnce(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "", &events)
	started := make(chan string, 2)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		started <- url
		// Each download waits until both have started: one after the other
		// would wait here for ever, and the test's deadline would say so.
		for len(started) < 2 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		return fetchFrom([]vpnconfig.Subscription{alpha, beta})(ctx, url)
	}
	done := make(chan struct{})
	go func() { w.Tick(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the downloads ran one after the other")
	}
}

// When no download arrived - the WAN is the likely cause - there is no walk:
// the message names the subscriptions, the lists stay, each records why, and
// the next wave comes ImportRetry later.
func TestWave_NoWalkWhenEveryDownloadFailed(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "", &events)
	var fetches atomic.Int32 // one goroutine per subscription calls it
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL, beta.URL)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		fetches.Add(1)
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if len(events) != 0 {
		t.Fatalf("walked %v after every download failed", events)
	}
	if n := countNotes(f.notes, "Subscription refresh failed: Alpha, Beta; still on tunnel:ovpnc2"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
	if f.subs[0].Error != "HTTP 403" || len(f.subs[0].Servers) != 1 {
		t.Fatalf("Alpha %+v", f.subs[0])
	}

	f.now = f.now.Add(ImportRetry - time.Second)
	w.Tick(context.Background())
	f.now = f.now.Add(time.Second)
	w.Tick(context.Background())
	if n := fetches.Load(); n != 4 {
		t.Fatalf("fetches %d, want 2 waves of 2", n)
	}
}

// Alpha's panel is down while its servers work: its last list is walked.
func TestWave_AFailedDownloadIsWalkedFromItsLastList(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(beta, alpha)
	var events []string
	w := walkRig(f, "aaaaaaaa/A1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL)

	w.Tick(context.Background())

	if want := []string{"bbbbbbbb/B1", "aaaaaaaa/A1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
	if f.subs[1].Error != "HTTP 403" {
		t.Fatalf("Alpha %+v", f.subs[1])
	}
	if n := countNotes(f.notes, "Subscription refresh failed"); n != 0 {
		t.Fatalf("a partial failure reached Telegram: %v", f.notes)
	}
}

// A refresh from the Web UI that succeeds while the watch's download of the
// same subscription fails is not marked failed by the older download.
func TestWave_AnOlderFailureDoesNotMarkANewerRefresh(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		if url == alpha.URL {
			f.subs[0].Refreshed = time.Unix(1_700_000_100, 0).UTC() // the Web UI refreshed Alpha meanwhile
		}
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if f.subs[0].Error != "" {
		t.Fatalf("Alpha marked failed over a newer refresh: %q", f.subs[0].Error)
	}
}

// During an outage every wave fails alike, each ImportRetry. The error is
// recorded once: the file is not written again for each wave.
func TestWave_AnOutageRecordsItsErrorOnce(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	f := waveFake(alpha)
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha}, alpha.URL)
	save := w.SaveSubscription
	saves := 0
	w.SaveSubscription = func(s vpnconfig.Subscription) error {
		saves++
		return save(s)
	}

	w.Tick(context.Background())
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())

	if saves != 1 || f.subs[0].Error != "HTTP 403" {
		t.Fatalf("%d writes of Alpha in two failed waves, error %q; want one write", saves, f.subs[0].Error)
	}
}

// A download that decodes to no server keeps the list it would have replaced.
func TestWave_ADownloadWithoutAServerKeepsTheList(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	f := waveFake(alpha)
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { return nil, nil }

	w.Tick(context.Background())

	if len(f.subs[0].Servers) != 1 || f.subs[0].Error == "" {
		t.Fatalf("Alpha %+v", f.subs[0])
	}
}

// A static list has nothing to download, and its servers are walked.
func TestWave_AStaticListIsWalkedWithoutADownload(t *testing.T) {
	gamma := subOf("cccccccc", "Gamma", "", "G1", "G2")
	f := waveFake(gamma)
	var events []string
	w := walkRig(f, "cccccccc/G2", &events)
	fetches := 0
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { fetches++; return nil, nil }

	w.Tick(context.Background())

	if fetches != 0 || !reflect.DeepEqual(events, []string{"cccccccc/G1", "cccccccc/G2"}) || f.cfg.Xray.Failover != nil {
		t.Fatalf("fetches %d, walk %v, failover %+v", fetches, events, f.cfg.Xray.Failover)
	}
}

// Alpha is deleted while its download runs: its list is not published - the
// file is not brought back - and only Beta is walked.
func TestWave_ASubscriptionDeletedWhileItDownloadsIsNotPublished(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta})
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		if url == alpha.URL {
			f.subs = f.subs[1:]
		}
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if len(f.subs) != 1 || f.subs[0].ID != "bbbbbbbb" {
		t.Fatalf("files %+v", f.subs)
	}
	if !reflect.DeepEqual(events, []string{"bbbbbbbb/B1"}) {
		t.Fatalf("walk %v", events)
	}
}

// Alpha is deleted while the walk is on its first server: the walk does not
// write Alpha's other servers, and goes on with Beta.
func TestWave_TheWalkSkipsASubscriptionDeletedUnderIt(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1", "A2")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})
	generate := w.Generate
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		ok, seq, err := generate(s, guard)
		if s.Name == "A1" {
			f.subs = f.subs[1:]
		}
		return ok, seq, err
	}

	w.Tick(context.Background())

	if want := []string{"aaaaaaaa/A1", "bbbbbbbb/B1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
}

// The walk's live server belongs to a subscription deleted after its probe:
// it runs and answers, and the clients come back to it all the same.
func TestWave_ARestoreDoesNotWaitForThePickedServersSubscription(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})
	probe := w.Probe
	w.Probe = func(ctx context.Context, port int) error {
		err := probe(ctx, port)
		if err == nil {
			f.subs = f.subs[:1] // Beta goes
		}
		return err
	}

	w.Tick(context.Background())

	if f.cfg.Xray.Failover != nil {
		t.Fatal("the restore waited for a deleted subscription")
	}
}

// One provider lists many names on one endpoint. A server whose outbound, with
// its address in place, was tried already is not tried again.
func TestWave_OneEndpointIsTriedOnce(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]}}`)
	alpha := vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: []vpnconfig.Server{
		{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"198.51.100.20"}, Outbound: ob},
		{Name: "Germany-2", Address: "de.example", Port: 443, IPs: []string{"198.51.100.20"}, Outbound: ob},
		{Name: "Germany-3", Address: "de.example", Port: 443, IPs: []string{"198.51.100.21"}, Outbound: ob},
	}}
	f := waveFake(alpha)
	var events []string
	w := walkRig(f, "aaaaaaaa/Germany-3", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha})

	w.Tick(context.Background())

	if want := []string{"aaaaaaaa/Germany-1", "aaaaaaaa/Germany-3"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
}

// Beta lists the endpoint of Alpha's server under the same outbound, and Alpha
// is deleted as the walk turns to that server. A candidate the walk did not
// write was not tried: Beta's copy of the endpoint still is.
func TestWave_AnEndpointWhoseWriteWasRefusedIsStillTried(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]}}`)
	server := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"198.51.100.20"}, Outbound: ob}
	alpha := vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: []vpnconfig.Server{server}}
	beta := vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", URL: "https://b.example/s/token", Servers: []vpnconfig.Server{server}}
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/Germany-1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})
	generate := w.Generate
	deleted := false
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if s.Subscription == "aaaaaaaa" && !deleted {
			deleted = true
			f.subs = f.subs[1:] // the user deletes Alpha while its server waits for the lock
		}
		return generate(s, guard)
	}

	w.Tick(context.Background())

	if want := []string{"bbbbbbbb/Germany-1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
}

// Both subscriptions name a server Germany-1; the user chose Beta's. The walk
// starts from Beta's, and returns to it when nothing is live - never Alpha's.
func TestWave_TheSameNameInAnotherSubscriptionIsNotTheChosenServer(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "Germany-1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "Germany-1")
	f := waveFake(alpha, beta)
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "germany-1.example", Port: 443}
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})

	w.Tick(context.Background())

	if len(events) < 2 || events[0] != "bbbbbbbb/Germany-1" || events[len(events)-1] != "bbbbbbbb/Germany-1" {
		t.Fatalf("walk %v", events)
	}
}

// The active_server of the release before names no subscription, so no server
// is the chosen one: the walk takes the subscriptions in turn from the first,
// and with nothing live returns to nothing.
func TestWave_ARecordFromBeforeSubscriptionsIsNotReturnedTo(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "A1", Address: "a1.example", Port: 443}
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})

	w.Tick(context.Background())

	if want := []string{"aaaaaaaa/A1", "bbbbbbbb/B1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
}

// Xray clients without a subscription: nothing to walk, and no failover of the watch's own.
func TestWave_NoSubscriptionArmsNothing(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0), noSubs: true}
	w := f.watch()

	tickUntilDead(w, f)

	if f.cfg.Xray.Failover != nil || f.applies != 0 {
		t.Fatalf("failover %+v, applies %d with no subscription", f.cfg.Xray.Failover, f.applies)
	}
}

func TestWave_AStaticListArmsTheWatch(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0),
		subs: []vpnconfig.Subscription{subOf("cccccccc", "Gamma", "", "G1")}}
	w := f.watch()

	tickUntilDead(w, f)

	if f.cfg.Xray.Failover == nil {
		t.Fatal("a static list did not arm the watch")
	}
}

type healthWalkFixture struct {
	f         *fake
	w         *Watch
	h         *fastHealth
	live      map[string]bool
	generated []vpnconfig.Server
	attempts  []string
	events    []string
}

func newHealthWalkFixture(t *testing.T, subs ...vpnconfig.Subscription) *healthWalkFixture {
	t.Helper()
	r := &healthWalkFixture{f: waveFake(subs...), live: make(map[string]bool)}
	r.f.plat = connected("ovpnc2")
	r.f.cfg.Advanced = map[string]interface{}{"xray": map[string]interface{}{"tproxy_port": float64(22345), "socks_port": float64(22346)}}
	r.h = &fastHealth{cached: orderTestEvidence(r.f.now, nil), invalid: make(map[string]bool)}
	r.w = runningWatch(r.f.watch())
	r.w.Health = r.h
	r.w.Fetch = fetchFrom(subs)
	r.w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		r.attempts = append(r.attempts, s.Name)
		if err := r.f.checkGuard(guard); err != nil {
			return false, r.f.seq(), err
		}
		r.generated = append(r.generated, s)
		r.events = append(r.events, "generate "+s.Name+"@"+dialIP(s))
		vpnconfig.RecordWalkedServer(r.f.cfg, s)
		return true, r.f.seq(), nil
	}
	r.w.RestartXray = func() error {
		r.events = append(r.events, "restart")
		return nil
	}
	r.w.AfterRestart = func(delay time.Duration) {
		if delay != 3*time.Second {
			t.Errorf("settle %v, want 3 seconds before the main probe", delay)
		}
		r.events = append(r.events, "settle")
	}
	r.w.Probe = func(ctx context.Context, port int) error {
		if ctx.Err() != nil || port != 22346 {
			t.Errorf("main probe context %v, SOCKS port %d, want 22346", ctx.Err(), port)
		}
		name := r.f.cfg.Xray.ActiveServer.Name
		r.events = append(r.events, "probe "+name)
		if len(r.generated) > 0 && r.live[name] {
			return nil
		}
		return errProbe
	}
	return r
}

func (r *healthWalkFixture) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.w.Tick(ctx)
}

func (r *healthWalkFixture) setHealth(statuses map[string]watchdapi.Status) {
	states := make(map[string]watchdapi.Status)
	for _, s := range endpoint.PerAddress(vpnconfig.AllServers(cloneSubs(r.f.subs))) {
		if status, ok := statuses[s.Name]; ok {
			states[endpoint.Key(s)] = status
		}
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.cached = orderTestEvidence(r.f.now, states)
	r.h.fresh = copyFastEvidence(r.h.cached, nil)
}

func TestWalk_HealthyFirstStillProbesMain(t *testing.T) {
	t.Run("cached alive still needs each main HTTPS probe", func(t *testing.T) {
		sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Unknown", "Dead", "Alive1", "Alive2", "Rejected")
		sub.Servers[3].IPs = []string{"203.0.113.13", "203.0.113.33"}
		r := newHealthWalkFixture(t, sub)
		r.setHealth(map[string]watchdapi.Status{
			"Alive1": watchdapi.StatusAlive, "Alive2": watchdapi.StatusAlive,
			"Dead": watchdapi.StatusDead, "Rejected": watchdapi.StatusRejected,
		})
		st := r.h.cached.Endpoints[endpoint.Keys(sub.Servers[3])[0]]
		st.LatencyMS = 900
		r.h.cached.Endpoints[endpoint.Keys(sub.Servers[3])[0]] = st
		st = r.h.cached.Endpoints[endpoint.Key(sub.Servers[4])]
		st.LatencyMS = 1
		r.h.cached.Endpoints[endpoint.Key(sub.Servers[4])] = st
		r.live["Unknown"] = true

		r.tick()

		want := []string{
			"probe Preferred",
			"generate Alive1@203.0.113.13", "restart", "settle", "probe Alive1",
			"generate Alive1@203.0.113.33", "restart", "settle", "probe Alive1",
			"generate Alive2@203.0.113.14", "restart", "settle", "probe Alive2",
			"generate Preferred@203.0.113.10", "restart", "settle", "probe Preferred",
			"generate Unknown@203.0.113.11", "restart", "settle", "probe Unknown",
		}
		if !reflect.DeepEqual(r.events, want) {
			t.Fatalf("main process transitions %v, want %v", r.events, want)
		}
		wantActive := &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "Unknown", Address: "unknown.example", Port: 443, Seq: 5}
		wantPreferred := &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "Preferred", Address: "preferred.example", Port: 443}
		if !reflect.DeepEqual(r.f.cfg.Xray.ActiveServer, wantActive) || !reflect.DeepEqual(r.f.cfg.Xray.PreferredServer, wantPreferred) {
			t.Fatalf("active %+v, preferred %+v; the health order must not replace the user's choice", r.f.cfg.Xray.ActiveServer, r.f.cfg.Xray.PreferredServer)
		}
		if r.f.cfg.Xray.Failover != nil || r.f.cfg.Xray.PendingRestore != nil || !contains(r.f.cfg.Xray.Clients, "192.168.1.8") ||
			contains(r.f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
			t.Fatal("the failed cached-alive probes did not continue to the live main-probed server and restore its clients")
		}
		if countNotes(r.f.notes, "LAN clients back on Xray; server Alpha / Unknown") != 1 {
			t.Fatalf("notes %v; only the main-probed server may be announced", r.f.notes)
		}
	})

	for _, tc := range []struct {
		name     string
		state    watchdapi.State
		absent   bool
		disabled bool
		stale    string
	}{
		{name: "unavailable health", state: watchdapi.StateOK, absent: true},
		{name: "disabled setting with cached ok", state: watchdapi.StateOK, disabled: true},
		{name: "WAN down", state: watchdapi.StateWANDown},
		{name: "prober error", state: watchdapi.StateProberError},
		{name: "no Xray", state: watchdapi.StateNoXray},
		{name: "monitor disabled", state: watchdapi.StateDisabled},
		{name: "monitor stopped", state: watchdapi.StateStopped},
		{name: "monitor not running", state: watchdapi.StateNotRunning},
		{name: "stale rejected is eligible again", state: watchdapi.StateOK, stale: "Preferred"},
		{name: "stale alive preserves original order", state: watchdapi.StateOK, stale: "Healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Healthy", "Unknown")
			r := newHealthWalkFixture(t, sub)
			r.setHealth(map[string]watchdapi.Status{"Preferred": watchdapi.StatusRejected, "Healthy": watchdapi.StatusAlive})
			r.h.cached.State = tc.state
			if tc.absent {
				r.w.Health = nil
			}
			if tc.disabled {
				enabled := false
				r.f.cfg.Monitor = &vpnconfig.MonitorConfig{Enabled: &enabled}
			}
			for _, s := range sub.Servers {
				if s.Name == tc.stale {
					r.h.invalidate(endpoint.Key(s))
				}
			}
			r.live["Preferred"] = true

			r.tick()

			if got := healthLabels(r.generated); !reflect.DeepEqual(got, []string{"Preferred@203.0.113.10"}) {
				t.Fatalf("unusable evidence changed legacy order: %v", got)
			}
			want := []string{"probe Preferred", "generate Preferred@203.0.113.10", "restart", "settle", "probe Preferred"}
			if !reflect.DeepEqual(r.events, want) || r.f.cfg.Xray.Failover != nil {
				t.Fatalf("events %v, failover %+v; a stale rejection must not suppress the main check", r.events, r.f.cfg.Xray.Failover)
			}
		})
	}

	for _, change := range []string{"late rejection", "alive becomes dead", "new generation", "global failure", "stale rejection"} {
		t.Run("remaining keys after "+change, func(t *testing.T) {
			sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Alive1", "Alive2", "Unknown", "Rejected")
			r := newHealthWalkFixture(t, sub)
			statuses := map[string]watchdapi.Status{
				"Preferred": watchdapi.StatusRejected, "Alive1": watchdapi.StatusAlive,
				"Alive2": watchdapi.StatusAlive, "Rejected": watchdapi.StatusRejected,
			}
			if change == "alive becomes dead" {
				delete(statuses, "Preferred")
			}
			r.setHealth(statuses)
			live := "Preferred"
			if change == "late rejection" {
				live = "Unknown"
			}
			r.live[live] = true
			r.h.validate = func(e monitor.Evidence, keys []string) error {
				r.h.mu.Lock()
				defer r.h.mu.Unlock()
				for _, key := range keys {
					if e.Endpoints[key] != r.h.cached.Endpoints[key] {
						return errors.New("synthetic generation changed")
					}
				}
				return nil
			}
			probe := r.w.Probe
			changed := false
			r.w.Probe = func(ctx context.Context, port int) error {
				err := probe(ctx, port)
				if r.f.cfg.Xray.ActiveServer.Name == "Alive1" && !changed {
					changed = true
					switch change {
					case "late rejection":
						r.setHealth(map[string]watchdapi.Status{
							"Preferred": watchdapi.StatusRejected, "Alive1": watchdapi.StatusAlive,
							"Alive2": watchdapi.StatusRejected, "Unknown": watchdapi.StatusAlive, "Rejected": watchdapi.StatusRejected,
						})
					case "alive becomes dead":
						r.setHealth(map[string]watchdapi.Status{
							"Alive1": watchdapi.StatusAlive, "Alive2": watchdapi.StatusDead, "Rejected": watchdapi.StatusRejected,
						})
					case "new generation":
						r.setHealth(map[string]watchdapi.Status{
							"Preferred": watchdapi.StatusAlive, "Alive1": watchdapi.StatusAlive,
							"Alive2": watchdapi.StatusDead, "Rejected": watchdapi.StatusRejected,
						})
						for key, st := range r.h.cached.Endpoints {
							st.CheckedAt = r.f.now.Add(time.Second)
							r.h.cached.Endpoints[key] = st
						}
					case "global failure":
						r.h.cached.State = watchdapi.StateProberError
					case "stale rejection":
						r.h.invalidate(endpoint.Key(sub.Servers[0]))
					}
				}
				return err
			}

			r.tick()

			want := []string{"Alive1@203.0.113.11", "Preferred@203.0.113.10"}
			if change == "late rejection" {
				want = []string{"Alive1@203.0.113.11", "Unknown@203.0.113.13"}
			}
			if got := healthLabels(r.generated); !changed || !reflect.DeepEqual(got, want) {
				t.Fatalf("generation change reached %v, writes %v, want %v without retrying tried keys", changed, got, want)
			}
			if r.f.cfg.Xray.Failover != nil || r.f.cfg.Xray.ActiveServer.Name != live {
				t.Fatalf("active %+v, failover %+v; remaining evidence must be reread before the next generation", r.f.cfg.Xray.ActiveServer, r.f.cfg.Xray.Failover)
			}
		})
	}

	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("endpoint alias first generation refused=%v", refused), func(t *testing.T) {
			first := fastServer("Alias1", "alias.example", "198.51.100.20")
			second := first
			second.Name = "Alias2"
			sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Last")
			sub.Servers = []vpnconfig.Server{sub.Servers[0], first, second, sub.Servers[1]}
			r := newHealthWalkFixture(t, sub)
			r.setHealth(map[string]watchdapi.Status{"Alias1": watchdapi.StatusAlive, "Alias2": watchdapi.StatusAlive})
			r.live["Last"] = true
			generate := r.w.Generate
			var attempts []string
			r.w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				attempts = append(attempts, s.Name)
				if refused && s.Name == "Alias1" {
					return false, r.f.seq(), errors.New("synthetic generator refusal")
				}
				return generate(s, guard)
			}

			r.tick()

			wantAttempts := []string{"Alias1", "Preferred", "Last"}
			wantWritten := []string{"Alias1@198.51.100.20", "Preferred@203.0.113.10", "Last@203.0.113.11"}
			if refused {
				wantAttempts = []string{"Alias1", "Alias2", "Preferred", "Last"}
				wantWritten[0] = "Alias2@198.51.100.20"
			}
			if !reflect.DeepEqual(attempts, wantAttempts) || !reflect.DeepEqual(healthLabels(r.generated), wantWritten) {
				t.Fatalf("attempts %v, writes %v, want %v / %v; only a written DialKey is tried", attempts, healthLabels(r.generated), wantAttempts, wantWritten)
			}
		})
	}

	for _, mutation := range []string{"same server selection", "stop"} {
		t.Run("guard after health ordering and "+mutation, func(t *testing.T) {
			sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Healthy")
			r := newHealthWalkFixture(t, sub)
			r.setHealth(map[string]watchdapi.Status{"Healthy": watchdapi.StatusAlive})
			var stopped atomic.Bool
			r.w.Stopped = stopped.Load
			generate := r.w.Generate
			r.w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				if mutation == "stop" {
					stopped.Store(true)
				} else {
					chosen := sub.Servers[0]
					chosen.Subscription = sub.ID
					r.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(r.f.cfg.Xray.ActiveServer, chosen)
				}
				return generate(s, guard)
			}

			r.tick()

			if !reflect.DeepEqual(r.attempts, []string{"Healthy"}) || len(r.generated) != 0 ||
				!reflect.DeepEqual(r.events, []string{"probe Preferred"}) || len(r.f.notes) != 0 || r.w.importRetry != 0 {
				t.Fatalf("attempts %v, writes %v, events %v, notes %v, retry %v; health ordering must not bypass the lock guard", r.attempts, healthLabels(r.generated), r.events, r.f.notes, r.w.importRetry)
			}
			wantSeq := 0
			if mutation == "same server selection" {
				wantSeq = 1
			}
			if a := r.f.cfg.Xray.ActiveServer; a.Name != "Preferred" || a.Seq != wantSeq || r.f.cfg.Xray.Failover == nil {
				t.Fatalf("active %+v, failover %+v; the newer choice/stop must stand", a, r.f.cfg.Xray.Failover)
			}
		})
	}
}
