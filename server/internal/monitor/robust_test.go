package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Review focus: the WAN goes down while every server is fine. The checks that
// failed on it are undone, statuses stay as they were, and every server is
// checked again once a control answers.
func TestMonitor_AWANOutageFreezesTheStatuses(t *testing.T) {
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	h := newHarness(t, keys...)
	h.at(0, true)

	for _, k := range keys {
		h.l.script[k] = []error{errTimeout, errTimeout}
	}
	h.wan = false
	h.at(time.Minute, false)

	if s := h.m.Snapshot(); s.State != watchdapi.StateWANDown {
		t.Fatalf("state %s, want wan_down", s.State)
	}
	for _, k := range keys {
		if st := h.state(k); st.Status != watchdapi.StatusAlive {
			t.Fatalf("%s %+v; an outage turned a server dead", k, st)
		}
	}
	dials := h.wanDials
	h.at(time.Minute+15*time.Second, false)
	if h.wanDials != dials+1 {
		t.Fatalf("controls dialed %d times more in 15 s, want once", h.wanDials-dials)
	}

	h.wan = true
	for _, k := range keys {
		h.l.script[k] = nil
	}
	h.at(time.Minute+30*time.Second, false)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK {
		t.Fatalf("state %s after a control answered", s.State)
	}
	for _, k := range keys {
		if st := h.state(k); st.CheckedAt != t0.Add(time.Minute+30*time.Second) {
			t.Fatalf("%s %+v, want checked the moment the WAN came back", k, st)
		}
	}
}

// Every server dead with the WAN up is the truth, and the controls are not
// asked again for every failure that follows.
func TestMonitor_AllDeadWithTheWANUpAreDead(t *testing.T) {
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	h := newHarness(t, keys...)
	for _, k := range keys {
		h.l.script[k] = []error{errTimeout, errTimeout}
	}

	h.at(0, true)

	for _, k := range keys {
		if st := h.state(k); st.Status != watchdapi.StatusDead {
			t.Fatalf("%s %+v, want dead", k, st)
		}
	}
	if h.wanDials != 1 {
		t.Fatalf("controls dialed %d times, want once", h.wanDials)
	}
}

// Xray names the outbound it refuses: that endpoint is rejected, and the
// prober starts with the rest.
func TestMonitor_ARefusedOutboundIsRejectedAndTheRestAreChecked(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.l.refuse["k2"] = "failed to build stream settings for outbound detour > Failed to build REALITY config."

	h.at(0, true)

	st := h.state("k2")
	if st.Status != watchdapi.StatusRejected || !strings.Contains(st.Error, "REALITY") {
		t.Fatalf("k2 %+v", st)
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 2 || got[0] != "k1" || got[1] != "k3" {
		t.Fatalf("the prober holds %v", got)
	}
	if h.state("k1").Status != watchdapi.StatusAlive || h.state("k3").Status != watchdapi.StatusAlive {
		t.Fatal("the other endpoints were not checked")
	}
	// The rejection holds at the next refresh: the outbound is the same.
	h.at(time.Minute, true)
	if h.state("k2").Status != watchdapi.StatusRejected {
		t.Fatal("the rejection did not hold")
	}
}

// Xray's error names no endpoint: halves are tested down to the one it
// refuses.
func TestMonitor_AnUnnamedRefusalIsFoundByHalves(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5")
	h.l.opaque["k4"] = true

	h.at(0, true)

	if st := h.state("k4"); st.Status != watchdapi.StatusRejected || st.Error != "Xray refused the outbound" {
		t.Fatalf("k4 %+v", st)
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 4 {
		t.Fatalf("the prober holds %v", got)
	}
}

// A prober that cannot start is retried after 1, then 2 minutes, not on
// every refresh.
func TestMonitor_AProberThatCannotStartBacksOff(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.fail = errors.New("the prober did not listen within 10s: address already in use")

	h.at(0, true)
	s := h.m.Snapshot()
	if s.State != watchdapi.StateProberError || !strings.Contains(s.Message, "address already in use") {
		t.Fatalf("snapshot %+v", s)
	}
	h.at(30*time.Second, true)
	if h.l.startCount() != 1 {
		t.Fatalf("%d starts within the first minute, want 1", h.l.startCount())
	}
	h.at(time.Minute, true)
	h.at(2*time.Minute, true)
	if h.l.startCount() != 2 {
		t.Fatalf("%d starts by 2 min, want 2: the second wait is 2 min", h.l.startCount())
	}
	h.l.fail = nil
	h.at(3*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK || h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatalf("snapshot %+v", s)
	}
}

// The endpoint that crashes Xray is found, checked alone, and left out; the
// others are checked again.
func TestMonitor_ACrashIsPinnedOnItsEndpoint(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.l.crashOn["k2"] = true

	h.at(0, true)

	if st := h.state("k2"); st.Status != watchdapi.StatusRejected || st.Error != "crashes Xray" {
		t.Fatalf("k2 %+v", st)
	}
	for _, k := range []string{"k1", "k3"} {
		if st := h.state(k); st.Status != watchdapi.StatusAlive {
			t.Fatalf("%s %+v", k, st)
		}
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 2 {
		t.Fatalf("the prober holds %v after the crash", got)
	}
}

func TestMonitor_StoppedDisabledAndNoXrayPauseTheChecks(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)

	h.stopped = true
	h.at(time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateStopped || h.m.session != nil {
		t.Fatalf("state %s, session %v", s.State, h.m.session)
	}
	if h.l.checkCount("k1") != 1 || h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatal("a stop checked or forgot the server")
	}

	h.stopped = false
	h.settings.Enabled = false
	h.at(2*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateDisabled {
		t.Fatalf("state %s, want disabled", s.State)
	}

	h.settings.Enabled = true
	h.l.ready = ErrNoXray
	h.at(3*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateNoXray {
		t.Fatalf("state %s, want no_xray", s.State)
	}

	h.l.ready = nil
	h.at(4*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK || h.l.checkCount("k1") != 2 {
		t.Fatalf("state %s, %d checks: the checks did not resume", s.State, h.l.checkCount("k1"))
	}
}

// The generator's refusal is decided again at every refresh.
func TestMonitor_AGeneratorRefusalIsRejectedUntilItGoes(t *testing.T) {
	h := newHarness(t, "k1")
	h.refused = map[string]string{"k9": "stored outbound: xhttp downloadSettings without an address"}

	h.at(0, true)
	if st := h.state("k9"); st.Status != watchdapi.StatusRejected || h.l.checkCount("k9") != 0 {
		t.Fatalf("k9 %+v", st)
	}
	h.refused = nil
	h.eps = eps("k1", "k9")
	h.at(time.Minute, true)
	if st := h.state("k9"); st.Status != watchdapi.StatusAlive {
		t.Fatalf("k9 %+v after the refusal went", st)
	}
}

// Review focus: a restart of the daemon - an update - keeps every status and
// every pause; a key that left the set while it was down is dropped.
func TestMonitor_TheStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchd-state.json")
	h := newHarness(t, "k1", "k2", "k3")
	h.m = New(h.deps(path))
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.at(0, true)
	h.m.shutdown()

	h.eps = eps("k1", "k2")
	h.m = New(h.deps(path))
	h.m.restore()
	h.at(30*time.Second, true)

	if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.NextAt != t0.Add(2*time.Minute) {
		t.Fatalf("k1 %+v, want dead until 2 min", st)
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusAlive || st.NextAt != t0.Add(time.Minute) {
		t.Fatalf("k2 %+v, want alive until 1 min", st)
	}
	if _, ok := h.m.Snapshot().Endpoints["k3"]; ok {
		t.Fatal("k3 came back after it left the set")
	}
	if h.l.checkCount("k2") != 1 {
		t.Fatal("a restart checked a live server before its time")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file %v, %v", info, err)
	}
}

func TestMonitor_ABrokenStateFileIsNoState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchd-state.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "k1")
	h.m = New(h.deps(path))
	h.m.restore()
	h.at(0, true)
	if h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatal("a broken state file stopped the monitor")
	}
}

// Checks that wait longer than an interval are reported.
func TestMonitor_ChecksFallingBehindAreReported(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.settings.Concurrency = 1
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)

	h.now = t0.Add(3 * time.Minute)
	h.m.tick(h.ctx, h.now)
	if lag := h.m.Snapshot().LagSeconds; lag != 180 {
		t.Fatalf("lag %d s, want 180", lag)
	}
	close(h.l.gate)
}

// One start rejects at most MaxRefusals endpoints: a set that keeps being
// refused is a prober error, retried later, not an endless loop of starts.
func TestMonitor_TwentyRefusalsInARowStopTheStart(t *testing.T) {
	var keys []string
	for i := 0; i < MaxRefusals+5; i++ {
		keys = append(keys, fmt.Sprintf("k%02d", i))
	}
	h := newHarness(t, keys...)
	for _, k := range keys {
		h.l.refuse[k] = "Xray refused the outbound"
	}

	h.at(0, true)

	s := h.m.Snapshot()
	if s.State != watchdapi.StateProberError || !strings.Contains(s.Message, "20 outbounds in a row") {
		t.Fatalf("snapshot state %s, message %q", s.State, s.Message)
	}
	rejected := 0
	for _, st := range s.Endpoints {
		if st.Status == watchdapi.StatusRejected {
			rejected++
		}
	}
	if rejected != MaxRefusals || h.l.startCount() != MaxRefusals {
		t.Fatalf("%d rejected after %d starts, want %d of each", rejected, h.l.startCount(), MaxRefusals)
	}
}

func TestMonitor_WANFailuresStayPrivateUntilControlsResolve(t *testing.T) {
	for _, wan := range []bool{false, true} {
		t.Run(fmt.Sprintf("wan_up_%v", wan), func(t *testing.T) {
			h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
			h.at(0, true)
			before := h.m.Snapshot().Endpoints
			logs := captureEngineLogs(t)
			h.now = t0.Add(time.Minute)
			done := h.ask([]string{"k1"})
			assertFrozen := func() {
				t.Helper()
				if got := h.m.Snapshot().Endpoints; !reflect.DeepEqual(got, before) {
					t.Errorf("unresolved WAN failure changed public state: %+v", got)
				}
				h.m.mu.Lock()
				for k, e := range h.m.entries {
					if e.pause != 0 {
						t.Errorf("%s pause grew before WAN evidence: %s", k, e.pause)
					}
				}
				h.m.mu.Unlock()
				if strings.Contains(logs.String(), "Monitor: server down") {
					t.Error("server-down log escaped before WAN evidence")
				}
			}
			for i := 1; i < GuardMin; i++ {
				h.complete(fmt.Sprintf("k%d", i), errTimeout)
				assertFrozen()
				stillWaiting(t, done)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			h.m.d.WANUp = func(ctx context.Context) bool {
				close(entered)
				select {
				case <-release:
					return wan
				case <-ctx.Done():
					return false
				}
			}
			applied := make(chan struct{})
			go func() { h.complete("k5", errTimeout); close(applied) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("guard did not dial at its threshold")
			}
			assertFrozen()
			stillWaiting(t, done)
			close(release)
			<-applied
			if wan {
				if t.Failed() {
					return
				}
				if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusDead || got.states["k1"].Fails != 1 {
					t.Fatalf("resolved Check = %+v, %v", got.states, got.err)
				}
				for i := 1; i <= GuardMin; i++ {
					if st := h.state(fmt.Sprintf("k%d", i)); st.Status != watchdapi.StatusDead || st.Fails != 1 {
						t.Fatalf("confirmed failure %+v", st)
					}
				}
			} else {
				assertFrozen()
				stillWaiting(t, done)
				if h.m.Snapshot().State != watchdapi.StateWANDown {
					t.Fatal("controls failing did not pause checks")
				}
				h.m.d.WANUp = func(context.Context) bool { return true }
				h.at(time.Minute+ControlEvery, false)
				if t.Failed() {
					return
				}
				if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive {
					t.Fatalf("recovered Check = %+v, %v", got.states, got.err)
				}
			}
		})
	}
}

func TestMonitor_PendingFailuresHaveABoundedResolutionWithoutBusyLooping(t *testing.T) {
	for _, wan := range []bool{true, false} {
		t.Run(fmt.Sprintf("wan_up_%v", wan), func(t *testing.T) {
			h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
			h.at(0, true)
			h.now = t0.Add(time.Minute)
			h.m.mu.Lock()
			for _, e := range h.m.entries {
				e.st.NextAt = h.now.Add(time.Hour)
			}
			h.m.mu.Unlock()
			h.m.save(h.now)
			before := h.state("k1")
			h.wan = wan
			h.complete("k1", errTimeout)
			if h.state("k1") != before || h.wanDials != 0 {
				t.Error("a lone failure was published or dialed before the guard window")
			}
			wait := h.m.untilNext(h.now, h.now.Add(RefreshEvery))
			if wait <= 0 || wait > GuardWindow {
				t.Errorf("pending resolution sleeps %s, want (0, %s]", wait, GuardWindow)
			}
			h.now = h.now.Add(GuardWindow - time.Second)
			h.m.tick(h.ctx, h.now)
			if h.wanDials != 0 || h.state("k1") != before {
				t.Error("pending failure resolved before its bound")
			}
			h.now = h.now.Add(time.Second)
			h.m.tick(h.ctx, h.now)
			if h.wanDials != 1 {
				t.Fatalf("controls dialed %d times at pending deadline, want once", h.wanDials)
			}
			if wan {
				if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.Fails != 1 || st.CheckedAt != t0.Add(time.Minute) || st.NextAt != t0.Add(3*time.Minute) {
					t.Fatalf("confirmed failure %+v", st)
				}
			} else if h.state("k1") != before || h.m.Snapshot().State != watchdapi.StateWANDown {
				t.Fatal("WAN failure was counted at the pending deadline")
			}
			if wait := h.m.untilNext(h.now, h.now.Add(RefreshEvery)); wait <= 0 {
				t.Fatalf("resolved pending outcome causes a busy loop: %s", wait)
			}
		})
	}
}

func TestMonitor_ASuccessResolvesPendingFailuresWithoutControls(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.m.refresh(h.ctx, t0)
	h.complete("k1", errTimeout)
	if st := h.state("k1"); st.Status != watchdapi.StatusUnknown || st.Fails != 0 {
		t.Fatalf("unresolved failure %+v", st)
	}
	h.complete("k2", nil)
	if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.Fails != 1 || h.wanDials != 0 {
		t.Fatalf("after success %+v, %d control dials", st, h.wanDials)
	}
	h.complete("k3", errTimeout)
	if st := h.state("k3"); st.Status != watchdapi.StatusDead || h.wanDials != 0 {
		t.Fatalf("recent success did not resolve %+v", st)
	}
}

func TestMonitor_InfrastructureFailuresNeverEnterBisectionOrReject(t *testing.T) {
	for _, cause := range []error{fs.ErrPermission, errors.New("exec format error"), context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			h := newHarness(t, "k1", "k2")
			h.l.fail, h.l.opaque["k1"] = cause, true
			f := &launcherFaults{fakeLauncher: h.l}
			h.m.d.Launcher = f
			h.at(0, true)
			if s := h.m.Snapshot(); s.State != watchdapi.StateProberError || f.testCalls != 0 {
				t.Errorf("infra error entered config testing: %+v; tests %d", s, f.testCalls)
			}
			for k, st := range h.m.Snapshot().Endpoints {
				if st.Status == watchdapi.StatusRejected {
					t.Errorf("infra error rejected %s", k)
				}
			}
			if h.m.proberAt != t0.Add(time.Minute) {
				t.Fatalf("first backoff ends %s", h.m.proberAt)
			}
		})
	}
}

func TestMonitor_BisectionInfrastructureErrorsNeverReject(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("test_%d", failAt), func(t *testing.T) {
			h := newHarness(t, "k1", "k2")
			f := &launcherFaults{fakeLauncher: h.l}
			f.onStart = func(context.Context, []Endpoint) (Session, error) {
				return nil, &ConfigError{Reason: "Xray refused the outbound"}
			}
			f.onTest = func(context.Context, []Endpoint) error {
				if f.testCalls == failAt {
					return fs.ErrPermission
				}
				return &ConfigError{Reason: "Xray refused the outbound"}
			}
			h.m.d.Launcher = f
			h.at(0, true)
			if h.m.Snapshot().State != watchdapi.StateProberError || f.testCalls != failAt {
				t.Errorf("bisection did not stop at infra error: tests %d, state %s", f.testCalls, h.m.Snapshot().State)
			}
			for k, st := range h.m.Snapshot().Endpoints {
				if st.Status == watchdapi.StatusRejected {
					t.Errorf("bisection infrastructure error rejected %s", k)
				}
			}
		})
	}
}

func TestMonitor_CancellationNeverRejects(t *testing.T) {
	for _, where := range []string{"start", "test"} {
		t.Run(where, func(t *testing.T) {
			h := newHarness(t, "k1", "k2")
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			h.ctx = ctx
			f := &launcherFaults{fakeLauncher: h.l}
			f.onStart = func(context.Context, []Endpoint) (Session, error) {
				if where == "start" {
					cancel()
					return nil, &RefusedError{Key: "k1", Reason: "Xray refused the outbound"}
				}
				return nil, &ConfigError{Reason: "Xray refused the outbound"}
			}
			f.onTest = func(context.Context, []Endpoint) error { cancel(); return ctx.Err() }
			h.m.d.Launcher = f
			h.m.refresh(ctx, t0)
			for k, st := range h.m.Snapshot().Endpoints {
				if st.Status == watchdapi.StatusRejected {
					t.Errorf("cancelled %s rejected %s", where, k)
				}
			}
			if h.m.proberFails != 0 {
				t.Error("caller cancellation grew the start backoff")
			}
		})
	}
}

func TestMonitor_ReadyFailuresBackOffAndRecover(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.ready = fs.ErrPermission
	f := &launcherFaults{fakeLauncher: h.l}
	h.m.d.Launcher = f
	h.at(0, true)
	for _, at := range []time.Duration{10 * time.Second, 30 * time.Second} {
		h.at(at, true)
	}
	if f.readyCalls != 1 || h.m.proberAt != t0.Add(time.Minute) {
		t.Errorf("Ready retried within first backoff: %d calls, retry %s", f.readyCalls, h.m.proberAt)
	}
	h.at(time.Minute, true)
	h.at(2*time.Minute, true)
	if f.readyCalls != 2 || h.m.proberAt != t0.Add(3*time.Minute) {
		t.Errorf("second Ready backoff: %d calls, retry %s", f.readyCalls, h.m.proberAt)
	}
	h.at(3*time.Minute, true)
	if f.readyCalls != 3 || h.m.proberAt != t0.Add(8*time.Minute) {
		t.Errorf("third Ready backoff: %d calls, retry %s", f.readyCalls, h.m.proberAt)
	}
	h.l.ready = nil
	h.at(8*time.Minute, true)
	if h.state("k1").Status != watchdapi.StatusAlive || h.m.Snapshot().State != watchdapi.StateOK {
		t.Fatal("Ready did not recover after its capped backoff")
	}
}

func TestMonitor_NoXrayIsRedetectedEveryMinute(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.ready = ErrNoXray
	f := &launcherFaults{fakeLauncher: h.l}
	h.m.d.Launcher = f
	for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute} {
		h.at(at, true)
	}
	if f.readyCalls != 3 || h.m.proberFails != 0 || h.m.Snapshot().State != watchdapi.StateNoXray {
		t.Fatalf("missing Xray: %d calls, %d failures, %s", f.readyCalls, h.m.proberFails, h.m.Snapshot().State)
	}
	h.l.ready = nil
	h.at(3*time.Minute, true)
	if h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatal("new Xray was not detected at the next minute")
	}
}

func TestMonitor_FailedSaveRetriesWithoutNewChecks(t *testing.T) {
	h := newHarness(t, "k1")
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	h.m = New(h.deps(path))
	h.at(0, true)
	h.m.save(t0)
	h.stopped = true
	h.m.refresh(h.ctx, t0)
	if !h.m.dirty {
		t.Error("failed save cleared dirty intent")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.at(30*time.Second, false)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("save retried before SaveEvery: %v", err)
	}
	h.at(SaveEvery, false)
	data, err := os.ReadFile(path)
	var s savedState
	if err != nil || json.Unmarshal(data, &s) != nil || s.Entries["k1"].State.Status != watchdapi.StatusAlive || s.SavedAt != t0.Add(SaveEvery) {
		t.Fatalf("retry did not save unchanged statuses: %s, %v", data, err)
	}
	if h.m.dirty || h.l.checkCount("k1") != 1 {
		t.Fatal("successful save stayed dirty or ran a new check")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v, %v", info, err)
	}
	if tmp, err := filepath.Glob(path + ".tmp*"); err != nil || len(tmp) != 0 {
		t.Fatalf("save leaked temp files: %v, %v", tmp, err)
	}
	h.at(2*SaveEvery, false)
	later, err := os.ReadFile(path)
	if err != nil || string(later) != string(data) {
		t.Fatal("clean state was saved again without a change")
	}
}

func TestMonitor_IdleStartupReconcilesAndPreservesTheSavedState(t *testing.T) {
	for _, idle := range []watchdapi.State{watchdapi.StateStopped, watchdapi.StateDisabled, watchdapi.StateNoXray} {
		t.Run(string(idle), func(t *testing.T) {
			h := newHarness(t, "k1", "k2", "rejected")
			path := filepath.Join(t.TempDir(), "state.json")
			want := map[string]savedEntry{
				"k1":       {State: watchdapi.EndpointState{Status: watchdapi.StatusDead, Fails: 4, Error: "timeout", LatencyMS: 100, CheckedAt: t0, Since: t0.Add(-time.Hour), NextAt: t0.Add(16 * time.Minute)}, Pause: 16 * time.Minute},
				"k2":       {State: watchdapi.EndpointState{Status: watchdapi.StatusAlive, LatencyMS: 120, CheckedAt: t0, Since: t0.Add(-time.Hour), NextAt: t0.Add(time.Minute)}},
				"rejected": {State: watchdapi.EndpointState{Status: watchdapi.StatusRejected, Error: "crashes Xray", Since: t0}, Sticky: true},
			}
			seed := savedState{SavedAt: t0, Entries: map[string]savedEntry{"gone": {State: watchdapi.EndpointState{Status: watchdapi.StatusAlive}}}}
			for k, v := range want {
				seed.Entries[k] = v
			}
			data, err := json.Marshal(seed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			h.stopped = idle == watchdapi.StateStopped
			h.settings.Enabled = idle != watchdapi.StateDisabled
			if idle == watchdapi.StateNoXray {
				h.l.ready = ErrNoXray
			}
			h.m = New(h.deps(path))
			h.m.restore()
			h.at(30*time.Second, true)
			if h.m.Snapshot().State != idle || h.m.session != nil || h.l.startCount() != 0 {
				t.Fatalf("idle startup state %s, session %v", h.m.Snapshot().State, h.m.session)
			}
			for k, v := range want {
				if st, ok := h.m.Snapshot().Endpoints[k]; !ok || st != v.State {
					t.Errorf("idle startup lost %s: %+v", k, st)
				}
			}
			if _, ok := h.m.Snapshot().Endpoints["gone"]; ok {
				t.Error("idle startup retained a removed key")
			}
			h.m.shutdown()
			data, err = os.ReadFile(path)
			var saved savedState
			if err != nil || json.Unmarshal(data, &saved) != nil || !reflect.DeepEqual(saved.Entries, want) {
				t.Fatalf("idle shutdown lost statuses/pauses: %+v, %v", saved, err)
			}
			h.stopped, h.settings.Enabled, h.l.ready = false, true, nil
			h.m = New(h.deps(path))
			h.m.restore()
			h.at(40*time.Second, true)
			for k, v := range want {
				if h.state(k) != v.State {
					t.Fatalf("idle restart changed %s", k)
				}
			}
			if h.l.checkCount("k1") != 0 || h.l.checkCount("k2") != 0 {
				t.Fatal("idle restart checked before saved schedules")
			}
		})
	}
}

func TestMonitor_CrashLimitBacksOffAndRecovers(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)
	for range CrashLimit {
		h.m.session.Stop()
		h.m.crashed(h.ctx)
	}
	if h.m.session != nil || h.m.Snapshot().State != watchdapi.StateProberError || !strings.Contains(h.m.Snapshot().Message, "crashed 5 times") || h.m.proberAt != t0.Add(time.Minute) {
		t.Fatalf("crash limit did not back off: %+v; retry %s", h.m.Snapshot(), h.m.proberAt)
	}
	if h.l.startCount() != CrashLimit {
		t.Fatalf("%d starts before crash backoff", h.l.startCount())
	}
	h.at(30*time.Second, true)
	if h.l.startCount() != CrashLimit {
		t.Fatal("crash loop restarted before its backoff")
	}
	h.at(time.Minute, true)
	if h.m.Snapshot().State != watchdapi.StateOK || h.state("k1").Status != watchdapi.StatusAlive || h.l.startCount() != CrashLimit+1 {
		t.Fatal("prober did not recover after crash-limit backoff")
	}
}

func TestMonitor_AWANOutageKeepsExistingDeadPausesAndFails(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.at(0, true)
	if h.state("k1").Status != watchdapi.StatusDead {
		t.Fatal("fixture endpoint was not dead")
	}
	before := h.m.Snapshot().Endpoints
	pauses := map[string]time.Duration{}
	h.m.mu.Lock()
	for k, e := range h.m.entries {
		pauses[k] = e.pause
	}
	h.m.mu.Unlock()
	logs := captureEngineLogs(t)
	h.now, h.wan = t0.Add(2*time.Minute), false
	for i := 1; i <= GuardMin; i++ {
		h.complete(fmt.Sprintf("k%d", i), errTimeout)
		if !reflect.DeepEqual(h.m.Snapshot().Endpoints, before) {
			t.Errorf("completion %d changed frozen statuses/fails/schedules", i)
		}
		h.m.mu.Lock()
		for k, e := range h.m.entries {
			if e.pause != pauses[k] {
				t.Errorf("completion %d grew %s pause to %s", i, k, e.pause)
			}
		}
		h.m.mu.Unlock()
		if strings.Contains(logs.String(), "Monitor: server down") {
			t.Errorf("completion %d logged a provisional death", i)
		}
	}
	if h.m.Snapshot().State != watchdapi.StateWANDown || h.wanDials != 1 {
		t.Fatal("the outage did not trip the all-failed guard")
	}
}

func TestMonitor_PendingFailuresAreNotRedispatched(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
	h.at(0, true)
	h.now = t0.Add(time.Minute)
	h.m.mu.Lock()
	for k, e := range h.m.entries {
		if k != "k1" {
			e.st.NextAt = h.now.Add(time.Hour)
		}
	}
	h.m.mu.Unlock()
	h.m.save(h.now)
	h.l.mu.Lock()
	h.l.gate = make(chan struct{})
	h.l.mu.Unlock()
	h.complete("k1", errTimeout)
	h.m.tick(h.ctx, h.now)
	if h.inFlight() != 0 || h.l.checkCount("k1") != 1 {
		t.Fatal("unresolved failure was dispatched again")
	}
	if wait := h.m.untilNext(h.now, h.now.Add(RefreshEvery)); wait != GuardWindow {
		t.Fatalf("pending failure sleep %s, want %s", wait, GuardWindow)
	}
}

func TestMonitor_AnUnreadableSetDoesNotEraseRestoredStateWhileIdle(t *testing.T) {
	h := newHarness(t, "k1")
	h.stopped = true
	path := filepath.Join(t.TempDir(), "state.json")
	seed := savedState{SavedAt: t0, Entries: map[string]savedEntry{"k1": {State: watchdapi.EndpointState{Status: watchdapi.StatusDead, Fails: 2, CheckedAt: t0, NextAt: t0.Add(4 * time.Minute)}, Pause: 4 * time.Minute}}}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	d := h.deps(path)
	d.Endpoints = func() ([]Endpoint, map[string]string, error) { return nil, nil, fs.ErrPermission }
	h.m = New(d)
	h.m.restore()
	h.m.refresh(h.ctx, t0)
	h.m.shutdown()
	data, err = os.ReadFile(path)
	var saved savedState
	if err != nil || json.Unmarshal(data, &saved) != nil || !reflect.DeepEqual(saved.Entries, seed.Entries) {
		t.Fatalf("unreadable subscriptions erased saved statuses/pauses: %+v, %v", saved, err)
	}
}

func TestMonitor_AWrappedConfigurationRefusalIsStillBisected(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	f := &launcherFaults{fakeLauncher: h.l}
	refuse := func(set []Endpoint) bool {
		for _, ep := range set {
			if ep.Key == "k2" {
				return true
			}
		}
		return false
	}
	f.onStart = func(ctx context.Context, set []Endpoint) (Session, error) {
		if refuse(set) {
			return nil, fmt.Errorf("config: %w", &ConfigError{Reason: "Xray refused the outbound"})
		}
		return h.l.Start(ctx, set)
	}
	f.onTest = func(_ context.Context, set []Endpoint) error {
		if refuse(set) {
			return fmt.Errorf("config: %w", &ConfigError{Reason: "Xray refused the outbound"})
		}
		return nil
	}
	h.m.d.Launcher = f
	h.at(0, true)
	if h.state("k2").Status != watchdapi.StatusRejected || h.state("k1").Status != watchdapi.StatusAlive || h.state("k3").Status != watchdapi.StatusAlive || f.testCalls == 0 {
		t.Fatal("wrapped typed refusal was not isolated")
	}
}
