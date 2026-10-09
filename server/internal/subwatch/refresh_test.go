package subwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

// realityAt is a VLESS REALITY server on address as an import stores it, with
// the short id a panel picked for one download.
func realityAt(name, address, sid string, ips ...string) vpnconfig.Server {
	ob := fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"www.example.com","fingerprint":"chrome","publicKey":"pk-1","shortId":%q}}}`, address, sid)
	return vpnconfig.Server{Name: name, Address: address, Port: 443, IPs: ips, Outbound: json.RawMessage(ob)}
}

const alphaURL = "https://a.example/s/token"

func alphaOf(servers ...vpnconfig.Server) vpnconfig.Subscription {
	return vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", URL: alphaURL, Servers: servers}
}

// refreshRig is a healthy watch over subs with the periodic refresh on, and
// the count of the subscription files it writes.
func refreshRig(subs ...vpnconfig.Subscription) (*fake, *Watch, *atomic.Int32) {
	f := &fake{cfg: baseCfg(), now: time.Unix(1_700_000_000, 0), subs: subs}
	w := runningWatch(f.watch())
	saves := new(atomic.Int32)
	save := w.SaveSubscription
	w.SaveSubscription = func(s vpnconfig.Subscription) error {
		saves.Add(1)
		return save(s)
	}
	w.RefreshInterval = func() time.Duration { return 5 * time.Minute }
	return f, w, saves
}

// serveList answers every download with servers.
func serveList(servers ...vpnconfig.Server) func(context.Context, string) ([]vpnconfig.Server, error) {
	return func(context.Context, string) ([]vpnconfig.Server, error) { return servers, nil }
}

func shortPoll(t *testing.T) {
	t.Helper()
	old := stopPoll
	stopPoll = time.Millisecond
	t.Cleanup(func() { stopPoll = old })
}

// A download that differs from the file only in what the panel picked at
// random writes nothing: the monitor keeps every status, and the prober runs on.
func TestRefreshRound_AnUnchangedListWritesNothing(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22", "203.0.113.10"))

	if !w.refreshRound(context.Background()) {
		t.Fatal("the round stood down")
	}

	if saves.Load() != 0 || !reflect.DeepEqual(f.subs[0].Servers, []vpnconfig.Server{stored}) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
	if f.cfg.Xray.Servers != nil {
		t.Fatalf("the config was written: xray.servers %v", f.cfg.Xray.Servers)
	}
}

func TestRefreshRound_ARenamedServerTakesItsRecords(t *testing.T) {
	stored := realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
	f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443}
	w.FetchList = serveList(realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10"))

	w.refreshRound(context.Background())

	if saves.Load() != 1 || f.subs[0].Servers[0].Name != "DE 9GB" || !bytes.Equal(f.subs[0].Servers[0].Outbound, stored.Outbound) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
	if a := f.cfg.Xray.ActiveServer; a.Name != "DE 9GB" || a.Seq != 7 {
		t.Fatalf("active %+v", a)
	}
	if p := f.cfg.Xray.PreferredServer; p.Name != "DE 9GB" {
		t.Fatalf("preferred %+v", p)
	}
}

// renameOslo serves the fast fixture's list with its active server, Oslo,
// under another name.
func renameOslo(s *fastFixture) {
	servers := cloneSubs(s.f.subs)[0].Servers
	servers[0].Name = "Oslo 10GB"
	s.w.FetchList = serveList(servers...)
}

// The fast path takes the monitor's dead for proof only when it came after the
// main probe last passed on the active server (diedAfterProbeOK). A periodic
// publication that renames that server takes the probe's memory along with its
// record: the first miss after the rename still switches at once.
func TestRefreshRound_TheProbesMemoryFollowsARenamedActiveServer(t *testing.T) {
	s := newFastFixture(t)
	s.passThenMiss(10 * time.Second)
	renameOslo(s)

	if !s.w.refreshRound(context.Background()) || s.f.cfg.Xray.ActiveServer.Name != "Oslo 10GB" {
		t.Fatalf("the round did not rename the active server: %+v", s.f.cfg.Xray.ActiveServer)
	}
	s.w.Tick(context.Background())

	if s.generateCalls != 1 || s.f.cfg.Xray.ActiveServer.Name != "Backup" {
		t.Fatalf("generate %d, active %+v; the first miss after the rename must switch at once", s.generateCalls, s.f.cfg.Xray.ActiveServer)
	}
}

// A probe that passed on another server proves nothing about the renamed one.
func TestRefreshRound_TheProbesMemoryOfAnotherServerStays(t *testing.T) {
	for _, other := range []*vpnconfig.ActiveServer{
		{Subscription: "alpha", Name: "Backup", Address: "backup.example", Port: 443},
		{Subscription: "beta", Name: "Oslo", Address: "oslo.example", Port: 443}, // another subscription's Oslo
	} {
		s := newFastFixture(t)
		s.w.probeOKActive = activeID(other)
		renameOslo(s)

		s.w.refreshRound(context.Background())

		if s.f.cfg.Xray.ActiveServer.Name != "Oslo 10GB" || s.w.probeOKActive != activeID(other) {
			t.Errorf("active %+v, probe's memory %q; want %+v's left as it was", s.f.cfg.Xray.ActiveServer, s.w.probeOKActive, other)
		}
	}
}

// pickedAt is the copy of s the walk picks on ip, in subscription sub.
func pickedAt(s vpnconfig.Server, sub, ip string) vpnconfig.Server {
	s.Subscription, s.IPs = sub, []string{ip}
	return s
}

// pickedLabel names a picked copy for a failure message, without its outbound.
func pickedLabel(p *vpnconfig.Server) string {
	if p == nil {
		return "nothing"
	}
	return fmt.Sprintf("%s / %s on %s", p.Subscription, p.Name, dialIP(*p))
}

// A failed return goes back to the server that ran, led by the copy the walk
// picked: the address Xray passed its probe on (rollbackOrder). A periodic
// publication that renames that server takes the copy along with its record.
func TestRefreshRound_ThePickedCopyFollowsARenamedActiveServer(t *testing.T) {
	de := realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10", "203.0.113.11")
	f, w, _ := refreshRig(alphaOf(de))
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
	picked := pickedAt(de, "aaaaaaaa", "203.0.113.11")
	w.lastPicked = &picked
	w.FetchList = serveList(realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10", "203.0.113.11"))

	w.refreshRound(context.Background())

	want := picked
	want.Name = "DE 9GB"
	if p := w.lastPicked; p == nil || !reflect.DeepEqual(*p, want) {
		t.Fatalf("picked %s, want %s", pickedLabel(p), pickedLabel(&want))
	}
	var back []string
	for _, c := range rollbackOrder(vpnconfig.AllServers(f.subs), f.cfg.Xray.ActiveServer, w.lastPicked) {
		back = append(back, dialIP(c))
	}
	if !reflect.DeepEqual(back, []string{"203.0.113.11", "203.0.113.10"}) {
		t.Fatalf("the way back %v; want the picked address first", back)
	}
}

func TestRefreshRound_APickedCopyOfAnotherServerStays(t *testing.T) {
	de := realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")
	fr := realityAt("FR", "fr.example", "cc33", "203.0.113.20")
	for _, other := range []vpnconfig.Server{
		pickedAt(fr, "aaaaaaaa", "203.0.113.20"),
		pickedAt(de, "bbbbbbbb", "203.0.113.10"), // another subscription's DE 10GB
	} {
		f, w, _ := refreshRig(alphaOf(de, fr))
		f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
		picked := other
		w.lastPicked = &picked
		w.FetchList = serveList(realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10"), fr)

		w.refreshRound(context.Background())

		if p := w.lastPicked; f.cfg.Xray.ActiveServer.Name != "DE 9GB" || p == nil || !reflect.DeepEqual(*p, other) {
			t.Errorf("active %+v, picked %s; want %s left as it was", f.cfg.Xray.ActiveServer, pickedLabel(p), pickedLabel(&other))
		}
	}
}

// While the watch handles an Xray failure the wave refreshes: the periodic
// refresh downloads nothing and writes nothing, and so while VPN Director is
// stopped or the gate is closed.
func TestRefreshRound_StandsDownWhileTheWatchMayNotOrIsBusy(t *testing.T) {
	for name, set := range map[string]func(*fake, *Watch){
		"a failover":      func(f *fake, _ *Watch) { f.cfg = failedOverCfg() },
		"a restore":       func(f *fake, _ *Watch) { f.cfg.Xray.PendingRestore = &vpnconfig.XrayPendingRestore{} },
		"a failing probe": func(f *fake, w *Watch) { w.failSince = f.now },
		"a pending apply": func(_ *fake, w *Watch) { w.pendingApply = true },
		"a stop":          func(_ *fake, w *Watch) { w.Stopped = func() bool { return true } },
		"a closed gate":   func(_ *fake, w *Watch) { w.CanMutate = func() error { return watchcompat.ErrIncompatible } },
	} {
		f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
		set(f, w)
		fetched := 0
		w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
			fetched++
			return nil, nil
		}

		if w.refreshRound(context.Background()) || fetched != 0 || saves.Load() != 0 {
			t.Errorf("%s: the round ran, downloads %d, writes %d", name, fetched, saves.Load())
		}
	}
}

// A failover that commits while the downloads run makes them the wave's.
func TestRefreshRound_AnEpisodeThatBeginsDuringTheDownloadsDropsThem(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		f.cfg.Xray.Failover = &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
		return []vpnconfig.Server{realityAt("FR", "fr.example", "cc33", "203.0.113.20")}, nil
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
}

// The publication waits for a running tick: a walk or a return must never see
// a record renamed under it.
func TestRefreshRound_PublishesOnlyBetweenTicks(t *testing.T) {
	_, w, saves := refreshRig(alphaOf(realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")))
	started, release := make(chan struct{}), make(chan struct{})
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		close(started)
		<-release
		return []vpnconfig.Server{realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10")}, nil
	}
	done := make(chan bool)
	go func() { done <- w.refreshRound(context.Background()) }()

	<-started
	w.tickMu.Lock() // a tick begins while the download runs
	close(release)
	time.Sleep(50 * time.Millisecond)
	if n := saves.Load(); n != 0 {
		w.tickMu.Unlock()
		t.Fatalf("published %d times inside a tick", n)
	}
	w.tickMu.Unlock()

	if !<-done || saves.Load() != 1 {
		t.Fatalf("writes %d after the tick, want 1", saves.Load())
	}
}

func TestRefreshRound_AStopEndsTheDownloadsAndRecordsNothing(t *testing.T) {
	shortPoll(t)
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	var stopped atomic.Bool
	w.Stopped = stopped.Load
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		stopped.Store(true)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

func TestRefreshRound_AFailedDownloadIsRecordedOnce(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		return nil, errors.New("download failed: HTTP 403")
	}

	w.refreshRound(context.Background())
	w.refreshRound(context.Background())

	if f.subs[0].Error != "download failed: HTTP 403" || saves.Load() != 1 || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("error %q, writes %d, servers %+v", f.subs[0].Error, saves.Load(), f.subs[0].Servers)
	}
}

// A resolver that answers nothing this time takes no server off the list.
func TestRefreshRound_NothingResolvedKeepsTheStoredAddresses(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22"))

	w.refreshRound(context.Background())

	if saves.Load() != 0 || !reflect.DeepEqual(f.subs[0].Servers, []vpnconfig.Server{stored}) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
}

func TestRefreshRound_AnEmptyMergeRecordsWhy(t *testing.T) {
	f, w, _ := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = serveList(realityAt("FR", "fr.example", "cc33")) // another server, unresolved

	w.refreshRound(context.Background())

	if f.subs[0].Error != "could not resolve IP for any server" || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
}

func TestRefreshRound_LeavesStaticListsOut(t *testing.T) {
	beta := vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{realityAt("NL", "nl.example", "dd44", "203.0.113.30")}}
	f, w, _ := refreshRig(beta, alphaOf(realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")))
	var asked []string
	w.FetchList = func(_ context.Context, url string) ([]vpnconfig.Server, error) {
		asked = append(asked, url)
		return []vpnconfig.Server{realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10")}, nil
	}

	w.refreshRound(context.Background())

	if !reflect.DeepEqual(asked, []string{alphaURL}) || f.subs[1].Servers[0].Name != "DE 9GB" || f.subs[0].Servers[0].Name != "NL" {
		t.Fatalf("asked %v, subscriptions %+v", asked, f.subs)
	}
}

func TestRefreshRound_NoLinkStandsDown(t *testing.T) {
	_, w, _ := refreshRig(vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{realityAt("NL", "nl.example", "dd44", "203.0.113.30")}})
	fetched := 0
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetched++
		return nil, nil
	}

	if w.refreshRound(context.Background()) || fetched != 0 {
		t.Fatalf("the round ran over a static list alone, downloads %d", fetched)
	}
}

// A panel that answers with an HTML page, or with no server Xray can run,
// costs the subscription nothing but its error, and the next good answer
// clears it.
func TestRefreshRound_AnUnreadableAnswerIsRecordedAndTheNextRoundClearsIt(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, _ := refreshRig(alphaOf(stored))
	answers := 0
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		answers++
		if answers == 1 {
			return nil, errors.New("unrecognized subscription format")
		}
		return []vpnconfig.Server{realityAt("DE", "de.example", "bb22", "203.0.113.10")}, nil
	}

	w.refreshRound(context.Background())
	if f.subs[0].Error != "unrecognized subscription format" || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("after the HTML page: error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
	w.refreshRound(context.Background())
	if f.subs[0].Error != "" || !bytes.Equal(f.subs[0].Servers[0].Outbound, stored.Outbound) {
		t.Fatalf("after the good answer: error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
}

// watchd shutting down in the middle of a round writes nothing more, and a
// download it cut short says nothing about the subscription.
func TestRefreshRound_AShutdownWritesNothingMore(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	ctx, cancel := context.WithCancel(context.Background())
	w.FetchList = func(fctx context.Context, _ string) ([]vpnconfig.Server, error) {
		cancel()
		<-fctx.Done()
		return nil, fctx.Err()
	}

	if w.refreshRound(ctx) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

// watchd shutting down while a failed download's record waits for the config
// lock cuts the record short: that is no failure to record, and no WARN.
func TestRefreshRound_AShutdownThatCutsTheErrorRecordShortLogsNoWarn(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		return nil, errors.New("download failed: HTTP 403")
	}
	update := w.UpdateVPN
	w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		cancel() // the shutdown lands while the record waits for the lock
		return update(fn)
	}
	records := logRecords(t)

	w.refreshRound(ctx)

	if saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q; a shutdown records nothing", saves.Load(), f.subs[0].Error)
	}
	if got := levelsOf(records(), "Failed to record why the subscription did not refresh"); len(got) != 0 {
		t.Fatalf("logged at %v; a shutdown is no failure to record", got)
	}
}

func TestRefreshRound_AGateThatClosesDropsTheDownloads(t *testing.T) {
	shortPoll(t)
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	var closed atomic.Bool
	w.CanMutate = func() error {
		if closed.Load() {
			return watchcompat.ErrIncompatible
		}
		return nil
	}
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		closed.Store(true)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

// A gate that closes during the downloads and opens again before the
// publication still drops them: they were cut short, and recording why would
// mark every subscription failed.
func TestRefreshRound_AGateThatClosesAndOpensAgainDropsTheDownloads(t *testing.T) {
	shortPoll(t)
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	var closed atomic.Bool
	w.CanMutate = func() error {
		if closed.Load() {
			return watchcompat.ErrIncompatible
		}
		return nil
	}
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		closed.Store(true)
		<-ctx.Done()
		closed.Store(false)
		return nil, context.Cause(ctx)
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

// watchd's CanMutate answers for the running tick, with its context's error
// once that tick ends. A tick that ends during the downloads is neither a stop
// nor a closed gate: they run on, and the round publishes them.
func TestRefreshRound_ATickThatEndsDuringTheDownloadsDropsNothing(t *testing.T) {
	shortPoll(t)
	stored := realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	var ended atomic.Bool
	answered := make(chan struct{}, 8)
	w.CanMutate = func() error {
		if !ended.Load() {
			return nil
		}
		// Never blocks the poller: a full buffer already holds what the
		// download waits for.
		select {
		case answered <- struct{}{}:
		default:
		}
		return context.Canceled
	}
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		ended.Store(true)
		// A second answer means the poller outlived the first one.
		for range 2 {
			select {
			case <-answered:
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		}
		ended.Store(false)
		return []vpnconfig.Server{realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10")}, nil
	}

	if !w.refreshRound(context.Background()) {
		t.Fatal("the round dropped its downloads")
	}
	if saves.Load() != 1 || f.subs[0].Servers[0].Name != "DE 9GB" || !bytes.Equal(f.subs[0].Servers[0].Outbound, stored.Outbound) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
}

// levelsOf is the level of every record of msg, in the order they were logged.
func levelsOf(records []map[string]any, msg string) []string {
	var levels []string
	for _, r := range records {
		if r["msg"] == msg {
			levels = append(levels, fmt.Sprint(r["level"]))
		}
	}
	return levels
}

// A panel that puts the traffic left into every server name renames servers at
// nearly every round, and watchd's log is cut at 200 KB: a round that only
// renamed them, and the record that followed, say so at DEBUG.
func TestRefreshRound_ARenameAloneLogsNothingAtInfo(t *testing.T) {
	f, w, _ := refreshRig(alphaOf(realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")))
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
	w.FetchList = serveList(realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10"))
	records := logRecords(t)

	w.refreshRound(context.Background())

	logged := records()
	for _, r := range logged {
		if r["level"] != "DEBUG" {
			t.Errorf("%q logged at %v; a rename alone logs at DEBUG", r["msg"], r["level"])
		}
	}
	published, followed := levelsOf(logged, "Subscription list published"), levelsOf(logged, "Server record follows its renamed server")
	if !reflect.DeepEqual(published, []string{"DEBUG"}) || !reflect.DeepEqual(followed, []string{"DEBUG"}) {
		t.Fatalf("list published at %v, record followed at %v; want each once at DEBUG", published, followed)
	}
}

func TestRefreshRound_AnAddedServerIsPublishedAtInfo(t *testing.T) {
	_, w, _ := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22", "203.0.113.10"), realityAt("FR", "fr.example", "cc33", "203.0.113.20"))
	records := logRecords(t)

	w.refreshRound(context.Background())

	if got := levelsOf(records(), "Subscription list published"); !reflect.DeepEqual(got, []string{"INFO"}) {
		t.Fatalf("list published at %v, want once at INFO", got)
	}
}

// A list that downloads again unchanged clears the recorded error: that is
// news, the list is not.
func TestRefreshRound_AClearedErrorIsNewsAndTheUnchangedListIsNot(t *testing.T) {
	sub := alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10"))
	sub.Error = "download failed: HTTP 403"
	_, w, _ := refreshRig(sub)
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22", "203.0.113.10"))
	records := logRecords(t)

	w.refreshRound(context.Background())

	logged := records()
	again, published := levelsOf(logged, "Subscription downloads again"), levelsOf(logged, "Subscription list published")
	if !reflect.DeepEqual(again, []string{"INFO"}) || !reflect.DeepEqual(published, []string{"DEBUG"}) {
		t.Fatalf("downloads again at %v, list published at %v; want INFO and DEBUG", again, published)
	}
}

// The first round a minute after the start, the next one an interval after a
// round ran, a minute after one that stood down, and a refresh turned off
// looks again every minute without downloading.
func TestStartRefresh_Schedule(t *testing.T) {
	_, w, _ := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	waits := make(chan time.Duration, 10)
	fire := make(chan time.Time)
	w.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	var interval atomic.Int64
	interval.Store(int64(7 * time.Minute))
	w.RefreshInterval = func() time.Duration { return time.Duration(interval.Load()) }
	var stopped atomic.Bool
	w.Stopped = stopped.Load
	var fetched atomic.Int32
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetched.Add(1)
		return []vpnconfig.Server{realityAt("DE", "de.example", "bb22", "203.0.113.10")}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.StartRefresh(ctx); close(done) }()

	expect := func(want time.Duration) {
		t.Helper()
		select {
		case d := <-waits:
			if d != want {
				t.Fatalf("wait %s, want %s", d, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no wait of %s", want)
		}
	}
	expect(RefreshFirst)
	fire <- time.Now() // a round that runs
	expect(7 * time.Minute)
	stopped.Store(true)
	fire <- time.Now() // a round that stands down
	expect(RefreshRetry)
	stopped.Store(false)
	interval.Store(0)
	fire <- time.Now() // the refresh is off
	expect(RefreshRetry)
	cancel()
	<-done

	if n := fetched.Load(); n != 1 {
		t.Fatalf("downloads %d, want the first round's alone", n)
	}
}
