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

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
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
