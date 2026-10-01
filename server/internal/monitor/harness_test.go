package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeLauncher is a prober for the engine's tests. Each Check of a key takes
// the next answer of script[key] - nil is alive - and is alive once the script
// runs out; a key in crashOn crashes its session; refuse and opaque make Start
// fail, naming the key or not; gate, when set, holds every Check until it is
// closed.
type fakeLauncher struct {
	mu      sync.Mutex
	ready   error
	refuse  map[string]string
	opaque  map[string]bool
	fail    error // every Start fails with it, and Test passes
	script  map[string][]error
	crashOn map[string]bool
	gate    chan struct{}
	gates   map[string]chan struct{}
	latency time.Duration
	starts  [][]string
	checks  map[string]int
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{refuse: map[string]string{}, opaque: map[string]bool{}, script: map[string][]error{},
		crashOn: map[string]bool{}, checks: map[string]int{}, latency: 100 * time.Millisecond}
}

func (f *fakeLauncher) Ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeLauncher) Start(_ context.Context, eps []Endpoint) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, len(eps))
	for i, ep := range eps {
		keys[i] = ep.Key
	}
	f.starts = append(f.starts, keys)
	if f.fail != nil {
		return nil, f.fail
	}
	for _, ep := range eps {
		if reason, ok := f.refuse[ep.Key]; ok {
			return nil, &RefusedError{Key: ep.Key, Reason: reason}
		}
	}
	for _, ep := range eps {
		if f.opaque[ep.Key] {
			return nil, &ConfigError{Reason: "Xray refused the outbound"}
		}
	}
	return &fakeSession2{l: f, exited: make(chan struct{})}, nil
}

func (f *fakeLauncher) Test(_ context.Context, eps []Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ep := range eps {
		if f.opaque[ep.Key] {
			return &ConfigError{Reason: "Xray refused the outbound"}
		}
	}
	return nil
}

func (f *fakeLauncher) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *fakeLauncher) checkCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks[key]
}

type fakeSession2 struct {
	l      *fakeLauncher
	exited chan struct{}
	once   sync.Once
}

func (s *fakeSession2) Check(ctx context.Context, key string) (time.Duration, error) {
	s.l.mu.Lock()
	gate := s.l.gate
	if keyed := s.l.gates[key]; keyed != nil {
		gate = keyed
	}
	s.l.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.l.mu.Lock()
	s.l.checks[key]++
	crash := s.l.crashOn[key]
	var err error
	if script := s.l.script[key]; len(script) > 0 {
		err, s.l.script[key] = script[0], script[1:]
	}
	latency := s.l.latency
	s.l.mu.Unlock()
	if crash {
		s.Stop()
		return 0, errors.New("EOF")
	}
	if err != nil {
		return 0, err
	}
	return latency, nil
}

func (s *fakeSession2) Exited() <-chan struct{} { return s.exited }

func (s *fakeSession2) Stop() { s.once.Do(func() { close(s.exited) }) }

// harness drives a monitor step by step on a fake clock.
type harness struct {
	t        *testing.T
	ctx      context.Context
	m        *Monitor
	l        *fakeLauncher
	now      time.Time
	settings Settings
	eps      []Endpoint
	refused  map[string]string
	stopped  bool
	wan      bool
	wanDials int
}

// fast shortens the engine's real waits for a test.
func fast(t *testing.T) {
	t.Helper()
	oldRetry, oldGrace := retryAfter, crashGrace
	retryAfter, crashGrace = 0, 20*time.Millisecond
	t.Cleanup(func() { retryAfter, crashGrace = oldRetry, oldGrace })
}

func eps(keys ...string) []Endpoint {
	out := make([]Endpoint, len(keys))
	for i, k := range keys {
		out[i] = Endpoint{Key: k, Label: "Alpha / " + k, Outbound: json.RawMessage(`{"protocol":"freedom"}`)}
	}
	return out
}

func newHarness(t *testing.T, keys ...string) *harness {
	t.Helper()
	fast(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, l: newFakeLauncher(), now: t0, settings: minuteSettings(), eps: eps(keys...), wan: true}
	h.m = New(h.deps(""))
	return h
}

func (h *harness) deps(statePath string) Deps {
	return Deps{
		Settings:  func() (Settings, error) { return h.settings, nil },
		Endpoints: func() ([]Endpoint, map[string]string, error) { return h.eps, h.refused, nil },
		Launcher:  h.l,
		Stopped:   func() bool { return h.stopped },
		WANUp:     func(context.Context) bool { h.wanDials++; return h.wan },
		StatePath: statePath,
		Now:       func() time.Time { return h.now },
		Jitter:    func() float64 { return 0 },
	}
}

func (h *harness) inFlight() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	n := 0
	for _, e := range h.m.entries {
		if e.inFlight {
			n++
		}
	}
	return n
}

// at moves the clock to t0+d and runs the loop's steps there: a refresh when
// asked, then dispatches and answers until no check is in flight.
func (h *harness) at(d time.Duration, refresh bool) {
	h.t.Helper()
	h.now = t0.Add(d)
	if refresh {
		h.m.refresh(h.ctx, h.now)
	}
	for i := 0; i < 1000; i++ {
		h.m.tick(h.ctx, h.now)
		if s := h.m.session; s != nil && exited(s) {
			h.m.crashed(h.ctx)
			continue
		}
		if h.inFlight() == 0 {
			return
		}
		select {
		case r := <-h.m.results:
			h.m.apply(h.ctx, r)
		case <-time.After(5 * time.Second):
			h.t.Fatal("no answer came")
		}
	}
	h.t.Fatal("the loop did not settle")
}

func (h *harness) state(key string) watchdapi.EndpointState {
	h.t.Helper()
	st, ok := h.m.Snapshot().Endpoints[key]
	if !ok {
		h.t.Fatalf("no endpoint %s", key)
	}
	return st
}

var errTimeout = context.DeadlineExceeded

// launcherFaults injects failures at a specific lifecycle step.
type launcherFaults struct {
	*fakeLauncher
	onReady               func() error
	onStart               func(context.Context, []Endpoint) (Session, error)
	onTest                func(context.Context, []Endpoint) error
	readyCalls, testCalls int
}

func (f *launcherFaults) Ready() error {
	f.readyCalls++
	if f.onReady != nil {
		return f.onReady()
	}
	return f.fakeLauncher.Ready()
}

func (f *launcherFaults) Start(ctx context.Context, set []Endpoint) (Session, error) {
	if f.onStart != nil {
		return f.onStart(ctx, set)
	}
	return f.fakeLauncher.Start(ctx, set)
}

func (f *launcherFaults) Test(ctx context.Context, set []Endpoint) error {
	f.testCalls++
	if f.onTest != nil {
		return f.onTest(ctx, set)
	}
	return f.fakeLauncher.Test(ctx, set)
}

func (h *harness) complete(key string, err error) {
	h.t.Helper()
	h.m.mu.Lock()
	e := h.m.entries[key]
	e.inFlight, e.urgent = true, false
	sess := h.m.session
	h.m.mu.Unlock()
	h.m.apply(h.ctx, result{sess: sess, key: key, latency: 100 * time.Millisecond, err: err})
}

func (h *harness) answer() {
	h.t.Helper()
	select {
	case r := <-h.m.results:
		h.m.apply(h.ctx, r)
	case <-time.After(5 * time.Second):
		h.t.Fatal("no worker result")
	}
}

type checkAnswer struct {
	states map[string]watchdapi.EndpointState
	err    error
}

func (h *harness) ask(keys []string) <-chan checkAnswer {
	h.t.Helper()
	select {
	case <-h.m.wake:
	default:
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	h.t.Cleanup(cancel)
	done := make(chan checkAnswer, 1)
	go func() {
		states, err := h.m.Check(ctx, keys)
		done <- checkAnswer{states, err}
	}()
	select {
	case <-h.m.wake:
	case <-time.After(5 * time.Second):
		h.t.Fatal("Check did not queue its request")
	}
	return done
}

func stillWaiting(t *testing.T, done <-chan checkAnswer) {
	t.Helper()
	select {
	case got := <-done:
		t.Errorf("Check returned before new resolved answers: %+v, %v", got.states, got.err)
	case <-time.After(20 * time.Millisecond):
	}
}

func answered(t *testing.T, done <-chan checkAnswer) checkAnswer {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not finish")
	}
	return checkAnswer{}
}

type engineLogs struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *engineLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *engineLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func captureEngineLogs(t *testing.T) *engineLogs {
	t.Helper()
	old := slog.Default()
	logs := &engineLogs{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}
