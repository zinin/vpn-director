package subwatch

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	osloIP    = "203.0.113.10"
	osloIP2   = "203.0.113.11"
	madridIP  = "203.0.113.20"
	madridIP2 = "203.0.113.21"
)

func returnServers() []vpnconfig.Server {
	return []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{osloIP}},
		{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP}},
	}
}

// awayCfg is a healthy router a walk left on Madrid while the user chose Oslo.
func awayCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Madrid", Address: "madrid.example", Port: 443, Seq: 7}
	cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	return cfg
}

// returnRig is a watch whose Generate records the way production's
// GenerateAndRecordWalkedServer does. running is the address Xray was last
// generated for - "" for the server it started on, which works - and the probe
// passes for the addresses in live. events lists every Generate as "name@ip"
// and every restart as "restart".
type returnRig struct {
	f       *fake
	w       *Watch
	up      map[string]bool // addresses that accept TCP
	live    map[string]bool // addresses the SOCKS probe passes on
	running string
	events  []string
	onProbe func() // runs at every probe of a switched Xray, before it answers
}

func newReturnRig(servers []vpnconfig.Server) *returnRig {
	r := &returnRig{
		f:    &fake{cfg: awayCfg(), now: time.Unix(1_700_000_000, 0)},
		up:   map[string]bool{},
		live: map[string]bool{},
	}
	w := runningWatch(r.f.watch())
	w.LoadServers = func() ([]vpnconfig.Server, error) { return servers, nil }
	w.Reachable = func(_ context.Context, ip string, _ int) bool { return r.up[ip] }
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := r.f.checkGuard(guard); err != nil {
			return false, r.f.seq(), err
		}
		r.running = dialIP(s)
		r.events = append(r.events, s.Name+"@"+r.running)
		vpnconfig.RecordWalkedServer(r.f.cfg, s)
		return true, r.f.seq(), nil
	}
	w.RestartXray = func() error {
		r.events = append(r.events, "restart")
		return nil
	}
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if r.running == "" {
			return nil
		}
		if r.onProbe != nil {
			r.onProbe()
		}
		if r.live[r.running] {
			return nil
		}
		return errProbe
	}
	r.w = w
	return r
}

func (r *returnRig) tick() { r.w.Tick(context.Background()) }

// attempts counts the switches to Oslo.
func (r *returnRig) attempts() int {
	n := 0
	for _, e := range r.events {
		if e == "Oslo@"+osloIP {
			n++
		}
	}
	return n
}

func TestTick_ReturnsToThePreferredServerOnceItAnswers(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Oslo" {
		t.Fatalf("active %+v, want Oslo", a)
	}
	if p := r.f.cfg.Xray.PreferredServer; p != nil {
		t.Fatalf("preferred %+v, want none once Oslo runs again", p)
	}
	if want := []string{"Xray back on the preferred server Oslo"}; !reflect.DeepEqual(r.f.notes, want) {
		t.Fatalf("notes %v, want %v", r.f.notes, want)
	}
	r.f.now = r.f.now.Add(ReturnCheck)
	r.tick()
	if len(r.events) != 2 {
		t.Fatalf("events %v; nothing is left to return to", r.events)
	}
}

// Every address of the preferred server is tried in turn until one works.
func TestTick_ReturnTriesEveryAddressOfThePreferredServer(t *testing.T) {
	servers := returnServers()
	servers[0].IPs = []string{osloIP, osloIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.up[osloIP2] = true
	r.live[osloIP2] = true
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Oslo@" + osloIP2, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if want := []string{"Xray back on the preferred server Oslo"}; !reflect.DeepEqual(r.f.notes, want) {
		t.Fatalf("notes %v, want %v", r.f.notes, want)
	}
	if r.w.lastPicked == nil || dialIP(*r.w.lastPicked) != osloIP2 {
		t.Fatalf("lastPicked %+v, want the address that works", r.w.lastPicked)
	}
}

func TestTick_FailedReturnGoesBackAndBacksOff(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	start := r.f.now
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
		t.Fatalf("active %+v, want Madrid back", a)
	}
	if p := r.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept", p)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v; a failed return tells nobody", r.f.notes)
	}
	// The next attempts wait 10, 20, then 30 minutes.
	for i, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute, 90 * time.Minute} {
		r.f.now = start.Add(at - time.Second)
		r.tick()
		if n := r.attempts(); n != i+1 {
			t.Fatalf("attempts %d at %v, want %d", n, at-time.Second, i+1)
		}
		r.f.now = start.Add(at)
		r.tick()
		if n := r.attempts(); n != i+2 {
			t.Fatalf("attempts %d at %v, want %d", n, at, i+2)
		}
	}
}

// A config.json that could not be generated for the preferred server is a
// failed return: nothing was restarted, so there is nothing to switch back.
func TestTick_AReturnWhoseConfigWasNotWrittenBacksOff(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	generate := r.w.Generate
	r.w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if s.Name == "Oslo" {
			return false, r.f.seq(), errors.New("read config.json.template: no such file or directory")
		}
		return generate(s, guard)
	}
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; nothing to restart or switch back", r.events)
	}
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
}

func TestTick_ReturnWaitsForThePreferredServerToAcceptTCP(t *testing.T) {
	r := newReturnRig(returnServers())
	r.live[osloIP] = true
	start := r.f.now
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; Oslo accepts no TCP yet", r.events)
	}
	r.up[osloIP] = true
	r.f.now = start.Add(ReturnCheck - time.Second)
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the next look is %v after the last", r.events, ReturnCheck)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_ARestoreHoldsTheFirstReturnForFiveMinutes(t *testing.T) {
	r := newReturnRig(returnServers())
	cfg := committedCfg()
	cfg.Xray.ActiveServer = awayCfg().Xray.ActiveServer
	cfg.Xray.PreferredServer = awayCfg().Xray.PreferredServer
	r.f.cfg = cfg
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick() // the probe passes while failed over: the clients come back
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the probe that passed must restore the clients")
	}
	start := r.f.now
	r.f.now = start.Add(ReturnCheck - time.Second)
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the preferred server failed minutes ago", r.events)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_NoReturnWhileARestoreApplyIsPending(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.f.applyErr = errApply
	r.w.pendingApply = true
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the pending apply comes first", r.events)
	}
}

func TestTick_ASelectionDuringTheReturnStands(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.onProbe = func() {
		if r.running == osloIP {
			// The Web UI selects a server while the watch probes Oslo.
			selectManual(r.f)
			r.f.cfg.Xray.PreferredServer = nil
		}
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v: nothing is written over the selection", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Manual" {
		t.Fatalf("active %+v, want the selection", a)
	}
}

// A probe that passes does not announce Oslo when a selection committed while
// it ran: Oslo no longer runs.
func TestTick_ASelectionDuringTheLiveProbeIsNotAnnounced(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.onProbe = func() {
		if r.running == osloIP {
			// The Web UI selects a server while the watch probes Oslo.
			selectManual(r.f)
			r.f.cfg.Xray.PreferredServer = nil
		}
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v; the selection replaced Oslo", r.f.notes)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Manual" {
		t.Fatalf("active %+v, want the selection", a)
	}
	if p := r.w.lastPicked; p != nil && p.Name == "Oslo" {
		t.Fatalf("lastPicked %+v; Oslo no longer runs", p)
	}
}

func TestTick_AStopDuringTheReturnEndsIt(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	var stopped atomic.Bool
	r.w.Stopped = stopped.Load
	r.w.RestartXray = func() error {
		r.events = append(r.events, "restart")
		stopped.Store(true) // the stop finishes while Xray restarts
		return nil
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v", r.f.notes)
	}
}

func TestTick_AStopDuringTheLiveProbeAnnouncesNothing(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	var stopped atomic.Bool
	r.w.Stopped = stopped.Load
	r.onProbe = func() {
		if r.running == osloIP {
			stopped.Store(true) // the stop finishes while the watch probes Oslo
		}
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v", r.f.notes)
	}
}

// A bot shutdown while Oslo is probed ends the attempt there: a probe the
// cancel cut short says nothing about Oslo, so no rollback is written and no
// wait is added.
func TestTick_ACancelDuringTheReturnProbeEndsIt(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.onProbe = func() {
		if r.running == osloIP {
			cancel()
		}
	}
	r.w.Tick(ctx)
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; the attempt did not fail, it was cut short", r.w.returnRetry)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v", r.f.notes)
	}
}

// A bot restart forgets which address the previous server ran on: the rollback
// tries its addresses in order.
func TestTick_RollbackTriesEveryAddressOfThePreviousServer(t *testing.T) {
	servers := returnServers()
	servers[1].IPs = []string{madridIP, madridIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[madridIP2] = true
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart", "Madrid@" + madridIP2, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.w.lastPicked == nil || dialIP(*r.w.lastPicked) != madridIP2 {
		t.Fatalf("lastPicked %+v, want the address that came back", r.w.lastPicked)
	}
}

func TestTick_RollbackStartsWithTheAddressTheWalkPicked(t *testing.T) {
	servers := returnServers()
	servers[1].IPs = []string{madridIP, madridIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[madridIP2] = true
	r.w.lastPicked = &vpnconfig.Server{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP2}}
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP2, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_ADeathStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick() // a failed return: the next waits ReturnRetry
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	r.live[madridIP] = false // Madrid dies
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; a death starts the returns over", r.w.returnRetry)
	}
}

// A preferred server that dies again soon after the return flaps: the death
// counts as a failed return, and the restore after it does not shorten the
// wait.
func TestTick_ADeathSoonAfterAReturnCountsAsAFailedReturn(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick()               // the return: Oslo runs again
	r.live[osloIP] = false // Oslo dies again
	r.up[osloIP] = false
	r.f.now = r.f.now.Add(ProbeInterval)
	start := r.f.now
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	// Oslo accepts no TCP: it is dead a minute after the first miss.
	want := start.Add(FastDeadAfter).Add(ReturnRetry)
	if !r.w.returnNotBefore.Equal(want) {
		t.Fatalf("returnNotBefore %v, want %v", r.w.returnNotBefore, want)
	}
	r.w.settled()
	if !r.w.returnNotBefore.Equal(want) {
		t.Fatalf("returnNotBefore %v after the restore, want %v", r.w.returnNotBefore, want)
	}
}

func TestTick_ADeathAfterAReturnHeldStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick() // the return: Oslo runs again
	r.live[osloIP] = false
	r.up[osloIP] = false
	r.f.now = r.f.now.Add(ReturnHold)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; the return held for %v", r.w.returnRetry, ReturnHold)
	}
}

// With no fallback the later ticks of a death come back to the death branch:
// the death counts as one failed return, not one per pass.
func TestTick_ADeathSoonAfterAReturnCountsOnce(t *testing.T) {
	r := newReturnRig(returnServers()) // no tunnel to fall back on
	r.up[osloIP] = true
	r.live[osloIP] = true
	platforms := 0
	load := r.w.LoadPlatform
	r.w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
		platforms++
		return load()
	}
	r.tick() // the return: Oslo runs again
	r.live[osloIP] = false
	r.up[osloIP] = false
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if n := countNotes(r.f.notes, "Xray outbound is down; no Tunnel Director fallback"); n != 1 {
		t.Fatalf("notes %v", r.f.notes)
	}
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	n := platforms
	tickFor(r.w, r.f, ImportRetry)
	if platforms == n {
		t.Fatal("the death branch must run again once ImportRetry has passed")
	}
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v after more passes of the same death, want %v", r.w.returnRetry, ReturnRetry)
	}
}

// With no preferred server left - a selection clears it - what the failed
// returns backed off to is done with.
func TestTick_NoPreferredServerStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.tick() // a failed return: the next waits ReturnRetry
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	r.f.cfg.Xray.PreferredServer = nil
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; with no preferred server the returns start over", r.w.returnRetry)
	}
}

// A return after failed ones ends their backoff only once it has held: until
// then a death would count as one more failed return.
func TestTick_AReturnEndsTheBackoffOnceItHasHeld(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.tick() // a failed return: the next waits ReturnRetry
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	r.live[osloIP] = true
	r.f.now = r.f.now.Add(ReturnRetry)
	r.tick() // the next attempt: Oslo works now
	if p := r.f.cfg.Xray.PreferredServer; p != nil {
		t.Fatalf("preferred %+v, want none once Oslo runs again", p)
	}
	returned := r.f.now
	r.f.now = returned.Add(ReturnHold - ProbeInterval)
	r.tick()
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v %v after the return, want %v", r.w.returnRetry, ReturnHold-ProbeInterval, ReturnRetry)
	}
	r.f.now = returned.Add(ReturnHold)
	r.tick()
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; the return held for %v", r.w.returnRetry, ReturnHold)
	}
}

func TestTick_NoReturnToAPreferredServerWithoutAnAddress(t *testing.T) {
	servers := returnServers()
	servers[0].IPs = nil // oslo.example resolved to nothing at the import
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; there is no address to check", r.events)
	}
}

func TestTick_WalkRemembersTheAddressItPicked(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())
	if w.lastPicked == nil || w.lastPicked.Name != "Oslo" || dialIP(*w.lastPicked) != "203.0.113.10" {
		t.Fatalf("lastPicked %+v, want Oslo at 203.0.113.10", w.lastPicked)
	}
}
