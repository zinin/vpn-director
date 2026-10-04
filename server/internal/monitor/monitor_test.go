package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func TestMonitor_ChecksEveryEndpointAndEachOnItsOwnSchedule(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")

	h.at(0, true)
	for _, k := range []string{"k1", "k2", "k3"} {
		st := h.state(k)
		if st.Status != watchdapi.StatusAlive || st.LatencyMS != 100 || st.NextAt != t0.Add(time.Minute) {
			t.Fatalf("%s: %+v", k, st)
		}
	}
	h.at(30*time.Second, false)
	if n := h.l.checkCount("k1"); n != 1 {
		t.Fatalf("k1 checked %d times by 30 s, want once", n)
	}
	h.at(time.Minute, false)
	if n := h.l.checkCount("k1"); n != 2 {
		t.Fatalf("k1 checked %d times by 1 min, want twice", n)
	}
	if h.l.startCount() != 1 {
		t.Fatalf("prober started %d times, want once", h.l.startCount())
	}
}

// One lost packet does not flip a status: a failure is retried once, and only
// a second failure fails the check.
func TestMonitor_AFailureIsRetriedOnceBeforeItCounts(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.script["k1"] = []error{errTimeout}
	h.l.script["k2"] = []error{errTimeout, errTimeout}

	h.at(0, true)

	if st := h.state("k1"); st.Status != watchdapi.StatusAlive || h.l.checkCount("k1") != 2 {
		t.Fatalf("k1 %+v after %d attempts, want alive after two", st, h.l.checkCount("k1"))
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusDead || st.Error != "timeout" || st.Fails != 1 {
		t.Fatalf("k2 %+v, want dead of a timeout", st)
	}
}

func TestMonitor_ADeadEndpointBacksOff(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.script["k1"] = []error{errTimeout, errTimeout, errTimeout, errTimeout}

	h.at(0, true)
	if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.NextAt != t0.Add(2*time.Minute) {
		t.Fatalf("after the first failure %+v", st)
	}
	h.at(time.Minute, false)
	if n := h.l.checkCount("k1"); n != 2 {
		t.Fatalf("a dead endpoint was checked again within its pause: %d attempts", n)
	}
	h.at(2*time.Minute, false)
	if st := h.state("k1"); st.NextAt != t0.Add(6*time.Minute) || st.Fails != 2 {
		t.Fatalf("after the second failure %+v, want the next check 4 min later", st)
	}
}

// A dead endpoint's next check can be 30 minutes away; asked for, it is
// checked now.
func TestMonitor_RequestPutsAnEndpointAheadOfItsSchedule(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.at(0, true)

	n, err := h.m.Request([]string{"k1", "absent"})
	if err != nil || n != 1 {
		t.Fatalf("Request() = %d, %v", n, err)
	}
	h.at(10*time.Second, false)
	if st := h.state("k1"); st.Status != watchdapi.StatusAlive || st.CheckedAt != t0.Add(10*time.Second) {
		t.Fatalf("k1 %+v, want checked at 10 s", st)
	}
	if h.l.checkCount("k2") != 1 {
		t.Fatal("k2 was checked ahead of its schedule")
	}
}

func TestMonitor_RequestWhileStoppedIsErrNotActive(t *testing.T) {
	h := newHarness(t, "k1")
	h.stopped = true
	h.at(0, true)

	if _, err := h.m.Request(nil); !errors.Is(err, watchdapi.ErrNotActive) {
		t.Fatalf("Request() error %v, want ErrNotActive", err)
	}
}

// Review focus: a subscription refresh that replaces the endpoints while
// checks are on their way. The answers of the replaced prober count for
// nothing, and the new set is checked.
func TestMonitor_AnswersOfAReplacedProberAreDropped(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 2 {
		t.Fatalf("%d checks in flight, want 2", h.inFlight())
	}

	h.eps = eps("k2", "k3")
	h.m.refresh(h.ctx, t0)
	close(h.l.gate)
	for i := 0; i < 2; i++ {
		h.m.apply(h.ctx, <-h.m.results)
	}
	if _, ok := h.m.Snapshot().Endpoints["k1"]; ok {
		t.Fatal("k1 is still listed after it left the set")
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusUnknown {
		t.Fatalf("k2 %+v; the replaced prober's answer counted", st)
	}
	h.l.mu.Lock()
	h.l.gate = nil
	h.l.mu.Unlock()
	h.at(0, false)
	if st := h.state("k3"); st.Status != watchdapi.StatusAlive {
		t.Fatalf("k3 %+v, want checked by the new prober", st)
	}
	if h.l.startCount() != 2 {
		t.Fatalf("prober started %d times, want twice", h.l.startCount())
	}
}

func TestMonitor_OrderOnlyRefreshKeepsSessionAndInflightCompletion(t *testing.T) {
	for _, activeOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "order-only", true: "active-only"}[activeOnly], func(t *testing.T) {
			h := newHarness(t, "k1", "k2", "k3")
			h.settings.Concurrency = 1
			next := eps("k3", "k1", "k2")
			if activeOnly {
				servers := []vpnconfig.Server{
					{Name: "first", Address: "192.0.2.10", Port: 443, UUID: "00000000-0000-4000-8000-000000000001", Subscription: "0a1b2c3d"},
					{Name: "second", Address: "192.0.2.20", Port: 443, UUID: "00000000-0000-4000-8000-000000000002", Subscription: "0a1b2c3d"},
					{Name: "third", Address: "192.0.2.30", Port: 443, UUID: "00000000-0000-4000-8000-000000000003", Subscription: "0a1b2c3d"},
				}
				subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Synthetic", Servers: servers}}
				outbound := func(vpnconfig.Server) (json.RawMessage, error) {
					return json.RawMessage(`{"protocol":"freedom"}`), nil
				}
				h.eps, _ = Build(subs, vpnconfig.NewActiveServer(servers[0]), outbound)
				next, _ = Build(subs, vpnconfig.NewActiveServer(servers[2]), outbound)
			}
			before := append([]Endpoint(nil), h.eps...)
			want := []Endpoint{before[2], before[0], before[1]}
			if !reflect.DeepEqual(next, want) {
				t.Fatal("active/order-only fixture changed endpoint keys, labels or outbounds")
			}
			gate, entered := make(chan struct{}), make(chan string, 3)
			cleanGate(t, gate)
			h.l.gate, h.l.entered = gate, entered
			h.m.refresh(h.ctx, t0)
			session := h.m.session
			h.m.tick(h.ctx, t0)
			enteredCheck(t, entered, before[0].Key)
			h.eps = next
			h.m.refresh(h.ctx, t0)
			if h.m.session != session || exited(session) || h.l.startCount() != 1 || h.inFlight() != 1 {
				t.Error("order-only refresh replaced the session or discarded its in-flight check")
			}
			if got := h.m.checkableSet(); !reflect.DeepEqual(got, want) {
				t.Error("session identity comparison changed active-first order, labels or outbounds")
			}
			close(gate)
			h.answer()
			if st := h.state(before[0].Key); st.Status != watchdapi.StatusAlive || st.LatencyMS != 100 || st.CheckedAt != t0 {
				t.Errorf("in-flight completion was discarded: %+v", st)
			}
			h.m.tick(h.ctx, t0)
			enteredCheck(t, entered, before[2].Key)
			h.answer()
			if st := h.state(before[2].Key); st.Status != watchdapi.StatusAlive {
				t.Errorf("updated active-first dispatch did not complete: %+v", st)
			}
		})
	}
}

func TestMonitor_KeySetChangesReplaceSessionAndDropInflightCompletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
	}{
		{"add", []string{"k1", "k2", "k3"}},
		{"remove", []string{"k1"}},
		{"rotated-key", []string{"k1", "rotated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "k1", "k2")
			h.settings.Concurrency = 1
			gate, entered := make(chan struct{}), make(chan string, 2)
			cleanGate(t, gate)
			h.l.gate, h.l.entered = gate, entered
			h.m.refresh(h.ctx, t0)
			old := h.m.session
			h.m.tick(h.ctx, t0)
			enteredCheck(t, entered, "k1")
			h.eps = eps(tc.keys...)
			h.m.refresh(h.ctx, t0)
			if h.m.session == old || !exited(old) || h.l.startCount() != 2 {
				t.Fatal("changed key set did not replace the session")
			}
			close(gate)
			h.answer()
			if st := h.state("k1"); st.Status != watchdapi.StatusUnknown || !st.CheckedAt.IsZero() {
				t.Fatalf("replaced session's answer was accepted: %+v", st)
			}
			h.m.tick(h.ctx, t0)
			enteredCheck(t, entered, "k1")
			h.answer()
			if st := h.state("k1"); st.Status != watchdapi.StatusAlive || st.CheckedAt != t0 {
				t.Fatalf("new session's answer was discarded: %+v", st)
			}
		})
	}
}

// Run drives the same steps on a real clock; Check waits for an answer from
// after the call.
func TestRun_ChecksAndAnswersCheck(t *testing.T) {
	fast(t)
	l := newFakeLauncher()
	l.latency = time.Millisecond
	statePath := filepath.Join(t.TempDir(), "state.json")
	m := New(Deps{
		Settings:  func() (Settings, error) { return minuteSettings(), nil },
		Endpoints: func() ([]Endpoint, map[string]string, error) { return eps("k1", "k2"), nil, nil },
		Launcher:  l,
		Stopped:   func() bool { return false },
		WANUp:     func(context.Context) bool { return true },
		StatePath: statePath,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	got, err := m.Check(checkCtx, []string{"k1"})
	stop()
	cancel()
	<-done
	if err != nil || got["k1"].Status != watchdapi.StatusAlive {
		t.Fatalf("Check() = %+v, %v", got, err)
	}
	data, err := os.ReadFile(statePath)
	var saved savedState
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Entries["k1"].State.Status != watchdapi.StatusAlive {
		t.Fatalf("state saved at shutdown: %s, %v", data, err)
	}
}

// At a rebuild every new endpoint is due at once; the active server's, which
// Build puts first, is checked first.
func TestMonitor_TheActiveServerIsCheckedFirst(t *testing.T) {
	h := newHarness(t, "k3", "k1", "k2")
	h.settings.Concurrency = 1
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)

	h.m.mu.Lock()
	first := h.m.entries["k3"].inFlight
	h.m.mu.Unlock()
	close(h.l.gate)
	if !first {
		t.Fatal("the first endpoint of the set is not the first one checked")
	}
}

func TestMonitor_CheckAllWaitsForEveryGatedAnswer(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	g1, g2 := make(chan struct{}), make(chan struct{})
	h.l.gates = map[string]chan struct{}{"k1": g1, "k2": g2}
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)
	done := h.ask(nil)
	stillWaiting(t, done)
	close(g1)
	h.answer()
	stillWaiting(t, done)
	close(g2)
	h.answer()
	if t.Failed() {
		return
	}
	got := answered(t, done)
	if got.err != nil || len(got.states) != 2 || got.states["k1"].Status != watchdapi.StatusAlive || got.states["k2"].Status != watchdapi.StatusAlive {
		t.Fatalf("Check(nil) = %+v, %v", got.states, got.err)
	}
}

func TestMonitor_CheckRequiresANewCompletionOnAStationaryClock(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)
	for i := 0; i < 2; i++ {
		gate := make(chan struct{})
		h.l.mu.Lock()
		h.l.gate = gate
		h.l.mu.Unlock()
		done := h.ask([]string{"k1"})
		stillWaiting(t, done)
		h.m.tick(h.ctx, t0)
		close(gate)
		h.answer()
		if t.Failed() {
			return
		}
		got := answered(t, done)
		if got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive || got.states["k1"].CheckedAt != t0 || h.l.checkCount("k1") != i+2 {
			t.Fatalf("repeat %d: Check = %+v, %v; checks %d", i, got.states, got.err, h.l.checkCount("k1"))
		}
	}
}

func TestMonitor_CheckEndsWhenMonitoringIsStopped(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)
	done := h.ask([]string{"k1"})
	stillWaiting(t, done)
	h.stopped = true
	h.m.refresh(h.ctx, t0)
	if t.Failed() {
		return
	}
	if got := answered(t, done); !errors.Is(got.err, watchdapi.ErrNotActive) {
		t.Fatalf("Check after stop = %+v, %v", got.states, got.err)
	}
}

func TestMonitor_RequestPriorityAndConcurrencyArePreserved(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4")
	h.settings.Concurrency = 1
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)
	if n, err := h.m.Request([]string{"k3", "absent"}); n != 1 || err != nil {
		t.Fatalf("Request = %d, %v", n, err)
	}
	h.m.tick(h.ctx, t0)
	h.m.mu.Lock()
	first := h.m.entries["k3"].inFlight
	h.m.mu.Unlock()
	if !first || h.inFlight() != 1 {
		t.Fatal("urgent endpoint did not take the single worker")
	}
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 1 {
		t.Fatal("dispatch exceeded concurrency")
	}
	close(h.l.gate)
	for range 4 {
		h.answer()
		h.m.tick(h.ctx, t0)
		if h.inFlight() > 1 {
			t.Fatal("refill exceeded concurrency")
		}
	}
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		if h.l.checkCount(k) != 1 {
			t.Fatalf("%s checked %d times", k, h.l.checkCount(k))
		}
	}
}

func TestMonitor_CheckDoesNotAcceptAReplacedSessionsPendingFailure(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
	h.at(0, true)
	h.now = t0.Add(time.Minute)
	done := h.ask([]string{"k1"})
	old := h.m.session
	h.complete("k1", errTimeout)
	stillWaiting(t, done)
	h.eps = eps("k1", "new")
	h.m.refresh(h.ctx, h.now)
	h.m.apply(h.ctx, result{sess: old, key: "k1", err: errTimeout})
	stillWaiting(t, done)
	h.complete("k1", nil)
	if t.Failed() {
		return
	}
	if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive {
		t.Fatalf("Check = %+v, %v", got.states, got.err)
	}
}

func TestMonitor_CheckUsesCompletionNotPublicationSequence(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
	h.at(0, true)
	h.now = t0.Add(time.Minute)
	h.complete("k1", errTimeout)
	done := h.ask([]string{"k1"})
	h.complete("k2", nil)
	stillWaiting(t, done)
	h.complete("k1", nil)
	if t.Failed() {
		return
	}
	if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive {
		t.Fatalf("Check accepted an older pending completion: %+v, %v", got.states, got.err)
	}
}

func TestRun_ConcurrentSnapshotsRequestsAndChecks(t *testing.T) {
	fast(t)
	l := newFakeLauncher()
	m := New(Deps{
		Settings:  func() (Settings, error) { return minuteSettings(), nil },
		Endpoints: func() ([]Endpoint, map[string]string, error) { return eps("k1", "k2"), nil, nil },
		Launcher:  l,
		Stopped:   func() bool { return false },
		WANUp:     func(context.Context) bool { return true },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() { m.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := m.Request([]string{"k1", "k2"}); err != nil {
					t.Error(err)
					return
				}
				got, err := m.Check(ctx, []string{"k1", "k2"})
				if err != nil || len(got) != 2 || got["k1"].Status != watchdapi.StatusAlive || got["k2"].Status != watchdapi.StatusAlive {
					t.Errorf("concurrent Check = %+v, %v", got, err)
					return
				}
				s := m.Snapshot()
				delete(s.Endpoints, "k1")
				if _, ok := m.Snapshot().Endpoints["k1"]; !ok {
					t.Error("Snapshot shared its endpoint map")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestMonitor_CheckDoesNotAcceptAQueuedPreCallCompletion(t *testing.T) {
	h := newHarness(t, "k1")
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)
	h.queued()
	gate, entered := make(chan struct{}), make(chan string, 2)
	cleanGate(t, gate)
	h.l.mu.Lock()
	h.l.gate, h.l.entered = gate, entered
	h.l.mu.Unlock()
	one, all := h.ask([]string{"k1"}), h.ask(nil)
	h.answer()
	stillWaiting(t, one)
	stillWaiting(t, all)
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 1 {
		t.Error("the queued old answer left no urgent follow-up despite its future NextAt")
	}
	if t.Failed() {
		return
	}
	enteredCheck(t, entered, "k1")
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 1 || h.l.checkCount("k1") != 1 {
		t.Fatal("coalesced Check calls started parallel duplicate checks")
	}
	stillWaiting(t, one)
	stillWaiting(t, all)
	close(gate)
	h.answer()
	for _, done := range []<-chan checkAnswer{one, all} {
		if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive || got.states["k1"].CheckedAt != t0 {
			t.Fatalf("fresh stationary-clock answer = %+v, %v", got.states, got.err)
		}
	}
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 0 || h.l.checkCount("k1") != 2 {
		t.Fatal("satisfied coalesced follow-up was dispatched again")
	}
}

func TestMonitor_CheckDoesNotRetokenAQueuedWANFailure(t *testing.T) {
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
	before := h.state("k1")
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.m.tick(h.ctx, h.now)
	h.queued()
	gate := make(chan struct{})
	cleanGate(t, gate)
	h.l.mu.Lock()
	h.l.gate = gate
	h.l.mu.Unlock()
	done := h.ask([]string{"k1"})
	h.answer()
	stillWaiting(t, done)
	h.m.tick(h.ctx, h.now)
	if h.state("k1") != before || h.inFlight() != 0 || h.l.checkCount("k1") != 3 {
		t.Fatal("an unresolved queued failure was published or redispatched")
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cleanGate(t, release)
	h.m.d.WANUp = func(ctx context.Context) bool {
		close(entered)
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	h.now = h.now.Add(GuardWindow)
	go func() { h.m.tick(h.ctx, h.now); close(finished) }()
	<-entered
	stillWaiting(t, done)
	if h.state("k1") != before {
		t.Error("gated WAN evidence exposed a provisional failure")
	}
	close(release)
	<-finished
	stillWaiting(t, done)
	if h.state("k1").Status != watchdapi.StatusDead || h.inFlight() != 1 {
		t.Error("late publication failed to retain the urgent post-call follow-up")
	}
	if t.Failed() {
		return
	}
	close(gate)
	h.answer()
	if got := answered(t, done); got.err != nil || got.states["k1"].Status != watchdapi.StatusAlive || got.states["k1"].CheckedAt != h.now {
		t.Fatalf("fresh post-publication answer = %+v, %v", got.states, got.err)
	}
}

func TestMonitor_AnUntaggedResultCannotCreateCheckFreshness(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)
	done := h.ask([]string{"k1"})
	h.m.apply(h.ctx, result{sess: h.m.session, key: "k1"})
	stillWaiting(t, done)
	if t.Failed() {
		return
	}
	h.at(0, false)
	if got := answered(t, done); got.err != nil || h.l.checkCount("k1") != 2 {
		t.Fatalf("tagged fresh check = %+v, %v", got.states, got.err)
	}
}
