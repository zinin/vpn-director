package subwatch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
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
	up      map[string]bool // addresses that accept TCP besides the control addresses
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
	w.LoadSubscriptions = subsOf(servers)
	w.Reachable = func(_ context.Context, ip string, _ int) bool { return r.up[ip] || controlUp(ip) }
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
	// The next attempts wait 10, 20, then 30 minutes; the fourth is the last.
	for i, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute} {
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
	for _, at := range []time.Duration{90 * time.Minute, 3 * time.Hour} {
		r.f.now = start.Add(at)
		r.tick()
		if n := r.attempts(); n != 4 {
			t.Fatalf("attempts %d at %v, want 4: the returns have stopped", n, at)
		}
	}
}

// failReturns has the return to Oslo fail ReturnFailsMax times in a row - at
// the start, then 10, 30 and 60 minutes after it - with Madrid live to go back
// to, and returns that start.
func failReturns(t *testing.T, r *returnRig) time.Time {
	t.Helper()
	r.up[osloIP] = true
	r.live[madridIP] = true
	start := r.f.now
	r.tick()
	for _, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute} {
		r.f.now = start.Add(at)
		r.tick()
	}
	if n := r.attempts(); n != ReturnFailsMax || r.w.returnFails != ReturnFailsMax {
		t.Fatalf("attempts %d, returnFails %d, want %d of each", n, r.w.returnFails, ReturnFailsMax)
	}
	return start
}

// A selection starts stopped returns over: once a walk leaves another server
// running again, the next attempt comes at its time.
func TestTick_ASelectionStartsStoppedReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	start := failReturns(t, r)
	r.f.cfg.Xray.PreferredServer = nil // a selection clears it
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.w.returnFails != 0 {
		t.Fatalf("returnFails %d; a selection starts the returns over", r.w.returnFails)
	}
	r.f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	// The last failure, 60 minutes in, set the next attempt 30 minutes later.
	r.f.now = start.Add(90*time.Minute - time.Second)
	r.tick()
	if n := r.attempts(); n != ReturnFailsMax {
		t.Fatalf("attempts %d before its time, want %d", n, ReturnFailsMax)
	}
	r.f.now = start.Add(90 * time.Minute)
	r.tick()
	if n := r.attempts(); n != ReturnFailsMax+1 {
		t.Fatalf("attempts %d at its time, want %d", n, ReturnFailsMax+1)
	}
}

func TestTick_ADeathStartsStoppedReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)
	r.live[madridIP] = false // Madrid dies
	r.f.plat = connected("ovpnc2")
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnFails != 0 || r.w.returnRetry != 0 {
		t.Fatalf("returnFails %d, returnRetry %v; a death starts the returns over", r.w.returnFails, r.w.returnRetry)
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

// A step of the switch to the preferred server that times out on its own
// deadline while the tick runs - Xray's config test of one address, an Xray
// restart - fails that address and ends nothing: the return tries the next
// address and, with none live, goes back to the server that ran before.
func TestTick_AReturnStepThatTimesOutFailsOnlyThatAddress(t *testing.T) {
	for _, step := range []string{"config test", "restart"} {
		t.Run(step, func(t *testing.T) {
			servers := returnServers()
			servers[0].IPs = []string{osloIP, osloIP2}
			r := newReturnRig(servers)
			r.up[osloIP] = true
			r.up[osloIP2] = true
			r.live[madridIP] = true
			generate, restart := r.w.Generate, r.w.RestartXray
			r.w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				if step == "config test" && dialIP(s) == osloIP {
					return false, r.f.seq(), fmt.Errorf("xray config test timed out after 15s: %w", context.DeadlineExceeded)
				}
				return generate(s, guard)
			}
			r.w.RestartXray = func() error {
				err := restart()
				if step == "restart" && r.running == osloIP {
					return fmt.Errorf("command timed out after 5m0s: %w", context.DeadlineExceeded)
				}
				return err
			}
			r.tick()
			want := []string{"Oslo@" + osloIP2, "restart", "Madrid@" + madridIP, "restart"}
			if step == "restart" {
				want = append([]string{"Oslo@" + osloIP, "restart"}, want...)
			}
			if !reflect.DeepEqual(r.events, want) {
				t.Fatalf("events %v, want %v", r.events, want)
			}
			if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
				t.Fatalf("active %+v, want Madrid back", a)
			}
			if p := r.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
				t.Fatalf("preferred %+v, want Oslo kept", p)
			}
			if r.w.returnFails != 1 || r.w.returnRetry != ReturnRetry || len(r.f.notes) != 0 {
				t.Fatalf("returnFails %d, returnRetry %v, notes %v; want one failed return that tells nobody", r.w.returnFails, r.w.returnRetry, r.f.notes)
			}
		})
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

// An import dropped the server that runs, and the process remembers no copy of
// it: a return that failed could not be undone, so none is tried.
func TestTick_NoReturnWithoutAWayBack(t *testing.T) {
	r := newReturnRig(returnServers()[:1]) // the import dropped Madrid
	r.up[osloIP] = true
	looks := 0
	reachable := r.w.Reachable
	r.w.Reachable = func(ctx context.Context, ip string, port int) bool {
		looks++
		return reachable(ctx, ip, port)
	}
	start := r.f.now
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; there is no way back to Madrid", r.events)
	}
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; nothing was tried", r.w.returnRetry)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
		t.Fatalf("active %+v, want Madrid", a)
	}
	if looks != 1 {
		t.Fatalf("looks %d, want 1", looks)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if looks != 2 {
		t.Fatalf("looks %d, want the next %v later", looks, ReturnCheck)
	}
	if len(r.events) != 0 {
		t.Fatalf("events %v; the list still has no way back", r.events)
	}
}

// The copy the walk picked is a way back even after an import dropped its
// server: Xray passed its probe on it.
func TestTick_RollbackUsesTheRememberedCopyOfAServerNoLongerListed(t *testing.T) {
	r := newReturnRig(returnServers()[:1]) // the import dropped Madrid
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.w.lastPicked = &vpnconfig.Server{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP}}
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
		t.Fatalf("active %+v, want Madrid back", a)
	}
}

// An import moved the server that runs to another address: the address Xray
// ran on comes first all the same.
func TestTick_RollbackStartsWithTheRememberedAddressAnImportDropped(t *testing.T) {
	servers := returnServers()
	servers[1].IPs = []string{madridIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.w.lastPicked = &vpnconfig.Server{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP}}
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
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

// A death soon after a return counts toward the cap as a failed return does:
// one failure short of it, the death stops the returns.
func TestTick_ADeathSoonAfterAReturnCountsTowardTheCap(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick() // the return: Oslo runs again
	r.w.returnFails = ReturnFailsMax - 1
	r.live[osloIP] = false // Oslo dies again
	r.up[osloIP] = false
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.w.returnFails != ReturnFailsMax {
		t.Fatalf("returnFails %d, want %d", r.w.returnFails, ReturnFailsMax)
	}
	// A walk puts Madrid in Oslo's place, and the clients come back to Xray.
	vpnconfig.RecordWalkedServer(r.f.cfg, returnServers()[1])
	r.running = madridIP
	r.live[madridIP] = true
	looks := 0
	reachable := r.w.Reachable
	r.w.Reachable = func(ctx context.Context, ip string, port int) bool {
		if ip == osloIP {
			looks++
		}
		return reachable(ctx, ip, port)
	}
	tickFor(r.w, r.f, time.Hour)
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the clients must be back on Xray")
	}
	if looks != 0 {
		t.Fatalf("%d looks at Oslo; the returns have stopped", looks)
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

// A Hysteria2 preferred server answers no TCP dial, so the cheap check cannot
// see it come back. The return is made on the schedule instead.
func TestTick_ReturnsToAQUICPreferredServerNoDialCanSee(t *testing.T) {
	servers := returnServers()
	servers[0].Outbound = hysteria2Outbound
	r := newReturnRig(servers)
	r.live[osloIP] = true // the probe passes; nothing accepts TCP
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if want := []string{"Xray back on the preferred server Oslo"}; !reflect.DeepEqual(r.f.notes, want) {
		t.Fatalf("notes %v, want %v", r.f.notes, want)
	}
}

// A return made without a check costs a switch like any other, so one that
// fails backs the next one off just the same.
func TestTick_AReturnNoDialCanSeeBacksOffWhenItFails(t *testing.T) {
	servers := returnServers()
	servers[0].Outbound = hysteria2Outbound
	r := newReturnRig(servers)
	r.live[madridIP] = true // Oslo refuses the proxy; Madrid comes back
	start := r.f.now
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if n := r.attempts(); n != 1 {
		t.Fatalf("attempts %d at %v, want the failed return to wait ReturnRetry", n, ReturnCheck)
	}
	r.f.now = start.Add(ReturnRetry)
	r.tick()
	if n := r.attempts(); n != 2 {
		t.Fatalf("attempts %d at %v, want the next attempt", n, ReturnRetry)
	}
}

// A subscription deleted while a return looks at its server: the switch is
// refused under the lock, and nothing is written.
func TestTick_AReturnWritesNoServerOfADeletedSubscription(t *testing.T) {
	r := newReturnRig(returnServers())
	subs := []vpnconfig.Subscription{{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: returnServers()}}
	r.f.cfg.Xray.ActiveServer.Subscription = "aaaaaaaa"
	r.f.cfg.Xray.PreferredServer.Subscription = "aaaaaaaa"
	deleted := false
	r.w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) {
		if deleted {
			return nil, nil
		}
		return cloneSubs(subs), nil
	}
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.w.Reachable = func(_ context.Context, ip string, _ int) bool {
		deleted = true // the user deletes Alpha while the return looks at Oslo
		return r.up[ip] || controlUp(ip)
	}

	r.tick()

	if len(r.events) != 0 {
		t.Fatalf("events %v; a server of a deleted subscription was written", r.events)
	}
}

// The first walk record after the update parks the active_server of the
// release before - which names no subscription - in preferred_server. No
// server matches it, so there is nothing to return to, and the look that finds
// so every ReturnCheck says nothing.
func TestTick_APreferredServerFromBeforeSubscriptionsIsNothingToReturnTo(t *testing.T) {
	logs := captureLog(t)
	r := newReturnRig(returnServers())
	subs := []vpnconfig.Subscription{{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: returnServers()}}
	r.w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) { return cloneSubs(subs), nil }
	r.f.cfg.Xray.ActiveServer.Subscription = "aaaaaaaa"
	r.up[osloIP] = true
	r.live[osloIP] = true

	r.tick()
	r.f.now = r.f.now.Add(ReturnCheck)
	r.tick()

	if len(r.events) != 0 {
		t.Fatalf("events %v; a record from before subscriptions names no server", r.events)
	}
	if strings.Contains(logs.String(), "level=INFO") || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("log %q; nothing to return to is nothing to say", logs.String())
	}
}

// The user deletes the preferred server's subscription while the return probes
// the first of its addresses, which does not answer. The switch to the second
// address is refused, and Xray goes back to the server that ran before - the
// way back is not held to the subscription, though Madrid's went too - rather
// than staying on the address its probe just found dead.
func TestTick_AReturnWhoseSubscriptionGoesMidAttemptRollsBack(t *testing.T) {
	servers := returnServers()
	servers[0].IPs = []string{osloIP, osloIP2}
	r := newReturnRig(servers)
	subs := []vpnconfig.Subscription{{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: servers}}
	r.f.cfg.Xray.ActiveServer.Subscription = "aaaaaaaa"
	r.f.cfg.Xray.PreferredServer.Subscription = "aaaaaaaa"
	deleted := false
	r.w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) {
		if deleted {
			return nil, nil
		}
		return cloneSubs(subs), nil
	}
	r.up[osloIP] = true
	r.up[osloIP2] = true
	r.live[madridIP] = true
	r.onProbe = func() {
		if r.running == osloIP {
			deleted = true // the user deletes Alpha while the return probes Oslo
		}
	}

	r.tick()

	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v: no second switch to Oslo, and the way back", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
		t.Fatalf("active %+v, want Madrid back", a)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v; nothing returned", r.f.notes)
	}
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v: the attempt failed", r.w.returnRetry, ReturnRetry)
	}
}

func TestReturn_HealthOrderingPreservesBackoffAndChoice(t *testing.T) {
	newHealthReturn := func() *returnRig {
		r := newReturnRig(returnServers())
		servers := returnServers()
		e := orderTestEvidence(r.f.now, map[string]watchdapi.Status{
			endpoint.Key(servers[0]): watchdapi.StatusDead,
			endpoint.Key(servers[1]): watchdapi.StatusAlive,
		})
		r.w.Health = &fastHealth{cached: e, fresh: copyFastEvidence(e, nil), invalid: make(map[string]bool)}
		return r
	}

	t.Run("all failed main probes return to the original walk choice", func(t *testing.T) {
		sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Unknown", "Alive1", "Alive2")
		r := newHealthWalkFixture(t, sub)
		r.setHealth(map[string]watchdapi.Status{"Alive1": watchdapi.StatusAlive, "Alive2": watchdapi.StatusAlive})

		r.tick()

		want := []string{
			"Alive1@203.0.113.12", "Alive2@203.0.113.13", "Preferred@203.0.113.10",
			"Unknown@203.0.113.11", "Preferred@203.0.113.10",
		}
		if got := healthLabels(r.generated); !reflect.DeepEqual(got, want) {
			t.Fatalf("walk and fallback %v, want %v; preferred comes from the original walk order", got, want)
		}
		wantActive := &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "Preferred", Address: "preferred.example", Port: 443, Seq: 5}
		if !reflect.DeepEqual(r.f.cfg.Xray.ActiveServer, wantActive) || r.f.cfg.Xray.PreferredServer != nil || r.f.cfg.Xray.Failover == nil {
			t.Fatalf("active %+v, preferred %+v, failover %+v; an all-dead wave must leave the user's choice on the tunnel", r.f.cfg.Xray.ActiveServer, r.f.cfg.Xray.PreferredServer, r.f.cfg.Xray.Failover)
		}
		last := r.events[len(r.events)-2:]
		if !reflect.DeepEqual(last, []string{"generate Preferred@203.0.113.10", "restart"}) ||
			r.w.importRetry != 10*time.Minute || countNotes(r.f.notes, "No live server in any subscription") != 1 ||
			countNotes(r.f.notes, "LAN clients back on Xray") != 0 {
			t.Fatalf("fallback %v, retry %v, notes %v; the fallback is not another main probe or a restore", last, r.w.importRetry, r.f.notes)
		}
	})

	t.Run("health-selected server keeps the original preferred and five minute delay", func(t *testing.T) {
		sub := subOf("aaaaaaaa", "Alpha", "", "Preferred", "Healthy")
		r := newHealthWalkFixture(t, sub)
		r.setHealth(map[string]watchdapi.Status{"Preferred": watchdapi.StatusDead, "Healthy": watchdapi.StatusAlive})
		r.live["Healthy"] = true
		r.live["Preferred"] = true
		r.w.Reachable = func(_ context.Context, ip string, _ int) bool { return ip == "203.0.113.10" || controlUp(ip) }
		start := r.f.now

		r.tick()

		if got := healthLabels(r.generated); !reflect.DeepEqual(got, []string{"Healthy@203.0.113.11"}) {
			t.Fatalf("walk %v; the monitored healthy server must lead", got)
		}
		wantPreferred := &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "Preferred", Address: "preferred.example", Port: 443}
		if !reflect.DeepEqual(r.f.cfg.Xray.PreferredServer, wantPreferred) || !r.w.returnNotBefore.Equal(start.Add(5*time.Minute)) || r.f.cfg.Xray.Failover != nil {
			t.Fatalf("preferred %+v, next return %v, failover %+v", r.f.cfg.Xray.PreferredServer, r.w.returnNotBefore, r.f.cfg.Xray.Failover)
		}
		r.f.now = start.Add(5*time.Minute - time.Second)
		r.tick()
		if len(r.generated) != 1 {
			t.Fatalf("early return %v; five minutes have not passed", healthLabels(r.generated))
		}
		r.f.now = start.Add(5 * time.Minute)
		r.tick()
		if got := healthLabels(r.generated); !reflect.DeepEqual(got, []string{"Healthy@203.0.113.11", "Preferred@203.0.113.10"}) {
			t.Fatalf("scheduled return %v; cached dead must not replace the original preferred choice", got)
		}
		wantActive := &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "Preferred", Address: "preferred.example", Port: 443, Seq: 2}
		if !reflect.DeepEqual(r.f.cfg.Xray.ActiveServer, wantActive) || r.f.cfg.Xray.PreferredServer != nil ||
			countNotes(r.f.notes, "Xray back on the preferred server Alpha / Preferred") != 1 {
			t.Fatalf("active %+v, preferred %+v, notes %v", r.f.cfg.Xray.ActiveServer, r.f.cfg.Xray.PreferredServer, r.f.notes)
		}
	})

	t.Run("failed return rolls back with ten twenty thirty minute retry and four failure cap", func(t *testing.T) {
		r := newHealthReturn()
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
		for i, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute} {
			r.f.now = start.Add(at - time.Second)
			r.tick()
			if n := r.attempts(); n != i+1 {
				t.Fatalf("attempts %d at %v, want %d", n, at-time.Second, i+1)
			}
			r.f.now = start.Add(at)
			r.tick()
			if n := r.attempts(); n != i+2 || r.w.returnFails != i+2 {
				t.Fatalf("attempts %d, failures %d at %v, want %d", n, r.w.returnFails, at, i+2)
			}
		}
		for _, at := range []time.Duration{90 * time.Minute, 3 * time.Hour} {
			r.f.now = start.Add(at)
			r.tick()
			if n := r.attempts(); n != 4 {
				t.Fatalf("attempts %d at %v, want 4: the returns have stopped", n, at)
			}
		}
		if r.w.returnRetry != 30*time.Minute || r.w.returnFails != 4 || len(r.f.notes) != 0 {
			t.Fatalf("retry %v, failures %d, notes %v", r.w.returnRetry, r.w.returnFails, r.f.notes)
		}
	})

	t.Run("a successful return keeps the backoff until it holds thirty minutes", func(t *testing.T) {
		r := newHealthReturn()
		r.up[osloIP] = true
		r.live[madridIP] = true
		r.tick()
		if r.w.returnRetry != 10*time.Minute || r.w.returnFails != 1 {
			t.Fatalf("failed return retry %v, failures %d", r.w.returnRetry, r.w.returnFails)
		}
		r.live[osloIP] = true
		r.f.now = r.f.now.Add(10 * time.Minute)
		r.tick()
		if r.f.cfg.Xray.PreferredServer != nil || r.f.cfg.Xray.ActiveServer.Name != "Oslo" {
			t.Fatalf("active %+v, preferred %+v; the scheduled preferred return must still probe Oslo", r.f.cfg.Xray.ActiveServer, r.f.cfg.Xray.PreferredServer)
		}
		returned := r.f.now
		r.f.now = returned.Add(30*time.Minute - time.Second)
		r.tick()
		if r.w.returnRetry != 10*time.Minute || r.w.returnFails != 1 {
			t.Fatalf("retry %v, failures %d before the thirty-minute hold", r.w.returnRetry, r.w.returnFails)
		}
		r.f.now = returned.Add(30 * time.Minute)
		r.tick()
		if r.w.returnRetry != 0 || r.w.returnFails != 0 || len(r.events) != 6 {
			t.Fatalf("retry %v, failures %d, events %v after the return held", r.w.returnRetry, r.w.returnFails, r.events)
		}
	})

	for _, choice := range []string{"same server", "another server"} {
		for _, live := range []bool{false, true} {
			t.Run(choice+" manually selected during preferred probe live="+fmt.Sprint(live), func(t *testing.T) {
				r := newHealthReturn()
				r.up[osloIP] = true
				r.live[osloIP] = live
				r.live[madridIP] = true
				r.onProbe = func() {
					if r.running == osloIP {
						if choice == "same server" {
							r.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(r.f.cfg.Xray.ActiveServer, returnServers()[0])
						} else {
							selectManual(r.f)
						}
						r.f.cfg.Xray.PreferredServer = nil
					}
				}

				r.tick()

				want := []string{"Oslo@" + osloIP, "restart"}
				if !reflect.DeepEqual(r.events, want) || len(r.f.notes) != 0 {
					t.Fatalf("events %v, notes %v; no rollback or announcement may overwrite a manual selection", r.events, r.f.notes)
				}
				name := "Oslo"
				if choice == "another server" {
					name = "Manual"
				}
				if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != name || a.Seq != 9 || r.f.cfg.Xray.PreferredServer != nil {
					t.Fatalf("active %+v, preferred %+v; the manual seq must stand", a, r.f.cfg.Xray.PreferredServer)
				}
			})
		}
	}

	for _, boundary := range []string{"settle loses gate", "probe loses gate", "local probe timeout"} {
		t.Run(boundary, func(t *testing.T) {
			r := newHealthReturn()
			r.up[osloIP] = true
			r.live[osloIP] = true
			r.live[madridIP] = true
			var incompatible atomic.Bool
			r.w.CanMutate = mutationGate(&incompatible)
			candidateProbes := 0
			if boundary == "settle loses gate" {
				r.w.AfterRestart = func(time.Duration) { incompatible.Store(true) }
			}
			probe := r.w.Probe
			r.w.Probe = func(ctx context.Context, port int) error {
				if r.running == osloIP {
					candidateProbes++
					if boundary == "probe loses gate" {
						incompatible.Store(true)
					}
					if boundary == "local probe timeout" {
						if ctx.Err() != nil {
							t.Fatal("the attempt context expired, not the local main probe")
						}
						return context.DeadlineExceeded
					}
				}
				return probe(ctx, port)
			}

			r.tick()

			want := []string{"Oslo@" + osloIP, "restart"}
			wantProbes := 1
			if boundary == "settle loses gate" {
				wantProbes = 0
			}
			if boundary == "local probe timeout" {
				want = append(want, "Madrid@"+madridIP, "restart")
				if r.w.returnFails != 1 || r.w.returnRetry != 10*time.Minute || !r.w.returnNotBefore.Equal(r.f.now.Add(10*time.Minute)) || r.f.cfg.Xray.ActiveServer.Name != "Madrid" {
					t.Fatalf("local timeout failures %d, retry %v, next %v, active %+v; a live attempt still counts a server failure and rolls back", r.w.returnFails, r.w.returnRetry, r.w.returnNotBefore, r.f.cfg.Xray.ActiveServer)
				}
			} else if r.w.returnFails != 0 || r.w.returnRetry != 0 {
				t.Fatalf("failures %d, retry %v; late gate loss must end the return, not fail it", r.w.returnFails, r.w.returnRetry)
			}
			if !reflect.DeepEqual(r.events, want) || candidateProbes != wantProbes || len(r.f.notes) != 0 {
				t.Fatalf("events %v, candidate probes %d, notes %v, want %v / %d without another attempt or notification", r.events, candidateProbes, r.f.notes, want, wantProbes)
			}
		})
	}
}
