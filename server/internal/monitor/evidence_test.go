package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func TestEvidence_OnlyAfterRequestInLiveGeneration(t *testing.T) {
	captureEngineLogs(t)
	for _, queued := range []bool{false, true} {
		name := "pre-call-in-flight"
		if queued {
			name = "pre-call-queued-completion"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "k1")
			oldGate, freshGate := make(chan struct{}), make(chan struct{})
			cleanGate(t, oldGate)
			cleanGate(t, freshGate)
			entered := make(chan string, 4)
			if !queued {
				h.l.gate, h.l.entered = oldGate, entered
			}
			h.m.refresh(h.ctx, t0)
			h.m.tick(h.ctx, t0)
			if queued {
				h.queued()
			} else {
				enteredCheck(t, entered, "k1")
			}
			one, all := h.askEvidence([]string{"k1"}), h.askEvidence(nil)
			h.l.mu.Lock()
			h.l.gate, h.l.entered = freshGate, entered
			h.l.mu.Unlock()
			close(oldGate)
			h.answer()
			evidenceWaiting(t, one)
			evidenceWaiting(t, all)
			h.m.tick(h.ctx, t0)
			enteredCheck(t, entered, "k1")
			h.m.tick(h.ctx, t0)
			if h.inFlight() != 1 || h.l.checkCount("k1") != 1 {
				t.Fatal("coalesced evidence requests did not share one urgent follow-up")
			}
			close(freshGate)
			h.answer()
			for _, done := range []<-chan evidenceAnswer{one, all} {
				got := evidenceAnswered(t, done)
				st := got.evidence.Endpoints["k1"]
				if got.err != nil || got.evidence.State != watchdapi.StateOK || st.Status != watchdapi.StatusAlive || st.LatencyMS != 100 || st.CheckedAt != t0 {
					t.Fatalf("stationary-clock fresh evidence = %+v, %v", got.evidence, got.err)
				}
				if err := h.m.ValidateEvidence(got.evidence, []string{"k1"}); err != nil {
					t.Fatalf("post-call evidence is not applicable: %v", err)
				}
			}
			h.m.tick(h.ctx, t0)
			if h.inFlight() != 0 || h.l.checkCount("k1") != 2 {
				t.Fatal("satisfied evidence follow-up was dispatched again")
			}
		})
	}

	t.Run("restored-results-are-not-live-proof", func(t *testing.T) {
		h := newHarness(t, "alive", "dead")
		path := filepath.Join(t.TempDir(), "watchd-state.json")
		h.m = New(h.deps(path))
		h.l.script["dead"] = []error{errTimeout, errTimeout}
		h.at(0, true)
		before := applicableEvidence(t, h.m, []string{"alive", "dead"})
		if before.Endpoints["alive"].Status != watchdapi.StatusAlive || before.Endpoints["dead"].Status != watchdapi.StatusDead {
			t.Fatal("fixture did not save both alive and dead results")
		}
		old := h.m
		old.shutdown()
		if err := old.ValidateEvidence(before, []string{"alive"}); err == nil {
			t.Fatal("shutdown left its live proof applicable")
		}
		h.m = New(h.deps(path))
		h.m.restore()
		h.m.refresh(h.ctx, t0)
		if h.state("alive").Status != watchdapi.StatusAlive || h.state("dead").Status != watchdapi.StatusDead {
			t.Fatal("evidence invalidation erased the persisted public statuses")
		}
		restored := h.m.Evidence()
		for _, key := range []string{"alive", "dead"} {
			if _, ok := restored.Endpoints[key]; ok {
				t.Errorf("restored %s was published as current-session evidence", key)
			}
			if err := h.m.ValidateEvidence(restored, []string{key}); err == nil {
				t.Errorf("restored %s is fresh without a live completion", key)
			}
		}
		done := h.askEvidence([]string{"alive", "dead"})
		evidenceWaiting(t, done)
		h.at(0, false)
		got := evidenceAnswered(t, done)
		if got.err != nil || got.evidence.Endpoints["alive"].Status != watchdapi.StatusAlive || got.evidence.Endpoints["dead"].Status != watchdapi.StatusAlive {
			t.Fatalf("new-session evidence = %+v, %v", got.evidence, got.err)
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"alive", "dead"}); err != nil {
			t.Fatalf("live completions after restore are not applicable: %v", err)
		}
	})

	t.Run("pending-failure-publication-is-not-a-new-completion", func(t *testing.T) {
		h := newHarness(t, "k1", "k2", "k3", "k4", "k5", "k6")
		h.at(0, true)
		h.now = t0.Add(time.Minute)
		h.m.mu.Lock()
		for key, e := range h.m.entries {
			if key != "k1" {
				e.st.NextAt = h.now.Add(time.Hour)
			}
		}
		h.m.mu.Unlock()
		h.complete("k1", errTimeout)
		if h.state("k1").Status != watchdapi.StatusAlive {
			t.Fatal("fixture published the failure before WAN evidence")
		}
		done := h.askEvidence([]string{"k1"})
		h.complete("k2", nil)
		if h.state("k1").Status != watchdapi.StatusDead {
			t.Fatal("post-call success did not resolve the older pending failure")
		}
		evidenceWaiting(t, done)
		gate, entered := make(chan struct{}), make(chan string, 1)
		cleanGate(t, gate)
		h.l.mu.Lock()
		h.l.gate, h.l.entered = gate, entered
		h.l.mu.Unlock()
		h.m.tick(h.ctx, h.now)
		enteredCheck(t, entered, "k1")
		evidenceWaiting(t, done)
		close(gate)
		h.answer()
		got := evidenceAnswered(t, done)
		if got.err != nil || got.evidence.Endpoints["k1"].Status != watchdapi.StatusAlive || got.evidence.Endpoints["k1"].CheckedAt != t0.Add(time.Minute) || h.l.checkCount("k1") != 2 {
			t.Fatalf("post-publication fresh evidence = %+v, %v", got.evidence, got.err)
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"k1"}); err != nil {
			t.Fatalf("fresh follow-up after WAN resolution is not applicable: %v", err)
		}
	})

	t.Run("late-old-session-result-is-not-fresh", func(t *testing.T) {
		h := newHarness(t, "k1")
		h.at(0, true)
		before := applicableEvidence(t, h.m, []string{"k1"})
		old := h.m.session
		h.m.stopSession()
		h.m.ensureSession(h.ctx, t0)
		if h.m.session == old || h.m.session == nil {
			t.Fatal("fixture did not replace the live session")
		}
		if _, ok := h.m.Evidence().Endpoints["k1"]; ok {
			t.Fatal("old-session result became evidence in the replacement session")
		}
		done := h.askEvidence([]string{"k1"})
		h.m.apply(h.ctx, h.m.finish(result{sess: old, key: "k1", latency: time.Millisecond}))
		evidenceWaiting(t, done)
		h.at(0, false)
		got := evidenceAnswered(t, done)
		if got.err != nil || got.evidence.Endpoints["k1"].LatencyMS != 100 {
			t.Fatalf("replacement-session evidence = %+v, %v", got.evidence, got.err)
		}
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
			t.Fatal("new completion revived an old-session token")
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"k1"}); err != nil {
			t.Fatalf("replacement-session completion is not applicable: %v", err)
		}
	})
}

func TestEvidence_InvalidatesOnCrashStateAndKeyChange(t *testing.T) {
	captureEngineLogs(t)
	for _, tc := range []struct {
		name  string
		state watchdapi.State
		pause func(*harness)
	}{
		{"stop", watchdapi.StateStopped, func(h *harness) {
			h.stopped = true
			h.m.refresh(h.ctx, h.now)
		}},
		{"disable", watchdapi.StateDisabled, func(h *harness) {
			h.settings.Enabled = false
			h.m.refresh(h.ctx, h.now)
		}},
		{"no-xray", watchdapi.StateNoXray, func(h *harness) {
			h.l.ready = ErrNoXray
			h.m.refresh(h.ctx, h.now)
		}},
		{"prober-error", watchdapi.StateProberError, func(h *harness) {
			h.l.ready = errors.New("synthetic prober failure")
			h.m.refresh(h.ctx, h.now)
		}},
		{"wan-down", watchdapi.StateWANDown, func(h *harness) {
			h.wan = false
			h.complete("k1", errTimeout)
		}},
		{"shutdown", watchdapi.StateOK, func(h *harness) { h.m.shutdown() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "k1")
			h.at(0, true)
			before := applicableEvidence(t, h.m, []string{"k1"})
			h.now = t0.Add(time.Minute)
			done := h.askEvidence([]string{"k1"})
			evidenceWaiting(t, done)
			tc.pause(h)
			if state := h.m.Snapshot().State; state != tc.state {
				t.Fatalf("fixture state %s, want %s", state, tc.state)
			}
			if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
				t.Fatal("inactive monitor accepted its preceding evidence")
			}
			if current := h.m.Evidence(); current.State != tc.state || len(current.Endpoints) != 0 {
				t.Fatalf("inactive evidence retained live records: %+v", current)
			}
			if got := evidenceAnswered(t, done); got.err == nil || errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("CheckEvidence did not report the state/session change: %+v, %v", got.evidence, got.err)
			}
			if before.Endpoints["k1"].Status != watchdapi.StatusAlive || before.Endpoints["k1"].CheckedAt != t0 {
				t.Fatal("state invalidation mutated the returned evidence")
			}
		})
	}

	t.Run("state-round-trip-does-not-revive-proof", func(t *testing.T) {
		h := newHarness(t, "k1")
		h.at(0, true)
		before := applicableEvidence(t, h.m, []string{"k1"})
		session := h.m.session
		h.m.setState(watchdapi.StateWANDown, "")
		h.m.setState(watchdapi.StateOK, "")
		if h.m.session != session || exited(session) {
			t.Fatal("fixture replaced the session instead of testing a state-only round trip")
		}
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
			t.Fatal("returning to ok revived proof from before the WAN pause")
		}
		if _, ok := h.m.Evidence().Endpoints["k1"]; ok {
			t.Fatal("state round trip relabeled an old completion as current-generation proof")
		}
	})

	t.Run("crash-and-restart", func(t *testing.T) {
		h := newHarness(t, "k1")
		h.at(0, true)
		before := applicableEvidence(t, h.m, []string{"k1"})
		done := h.askEvidence([]string{"k1"})
		old := h.m.session
		old.Stop()
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
			t.Fatal("exited prober remained a live proof before crash handling")
		}
		if _, ok := h.m.Evidence().Endpoints["k1"]; ok {
			t.Fatal("exited prober's alive result remained applicable")
		}
		h.m.crashed(h.ctx)
		if h.m.session == old || h.m.session == nil {
			t.Fatal("crash fixture did not restart the prober")
		}
		if got := evidenceAnswered(t, done); got.err == nil || errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("CheckEvidence did not report the crashed generation: %v", got.err)
		}
		h.m.apply(h.ctx, h.m.finish(result{sess: old, key: "k1"}))
		h.complete("k1", nil)
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
			t.Fatal("late/new completions revived pre-crash evidence")
		}
		applicableEvidence(t, h.m, []string{"k1"})
	})

	for _, tc := range []struct {
		name     string
		keys     []string
		freshKey string
	}{
		{"add-key", []string{"k1", "k2", "new"}, "k1"},
		{"remove-key", []string{"k1"}, "k1"},
		{"rotate-key", []string{"k1", "rotated"}, "k1"},
		{"remove-required-key", []string{"k2"}, "k2"},
		{"rotate-required-key", []string{"rotated", "k2"}, "rotated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "k1", "k2")
			h.at(0, true)
			before := applicableEvidence(t, h.m, []string{"k1"})
			done := h.askEvidence([]string{"k1"})
			h.eps = eps(tc.keys...)
			h.m.refresh(h.ctx, t0)
			if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
				t.Fatal("changed endpoint set retained the old generation's proof")
			}
			if got := evidenceAnswered(t, done); got.err == nil || errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("CheckEvidence did not report the changed endpoint set: %v", got.err)
			}
			h.complete(tc.freshKey, nil)
			applicableEvidence(t, h.m, []string{tc.freshKey})
			if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
				t.Fatal("new current-set completion revived old-generation proof")
			}
		})
	}

	t.Run("requested-status-and-completion-revision", func(t *testing.T) {
		h := newHarness(t, "k1", "k2")
		h.at(0, true)
		before := applicableEvidence(t, h.m, []string{"k1", "k2"})
		h.complete("k2", nil)
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err != nil {
			t.Fatalf("unrelated endpoint completion invalidated k1: %v", err)
		}
		if err := h.m.ValidateEvidence(before, []string{"k2"}); err == nil {
			t.Fatal("same status and timestamp hid a newer required completion")
		}
		h.complete("k1", errTimeout)
		if h.state("k1").Status != watchdapi.StatusDead {
			t.Fatal("fixture failure was not resolved by the recent success")
		}
		if err := h.m.ValidateEvidence(before, []string{"k1"}); err == nil {
			t.Fatal("changed required status retained its old proof")
		}
		dead := applicableEvidence(t, h.m, []string{"k1"})
		h.complete("k1", nil)
		if err := h.m.ValidateEvidence(dead, []string{"k1"}); err == nil {
			t.Fatal("dead evidence remained applicable after recovery")
		}
		if before.Endpoints["k1"].Status != watchdapi.StatusAlive || dead.Endpoints["k1"].Status != watchdapi.StatusDead {
			t.Fatal("completion changed an already returned evidence value")
		}
	})

	for _, activeOnly := range []bool{false, true} {
		name := "order-only-keeps-generation"
		if activeOnly {
			name = "active-only-keeps-generation"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "k1", "k2", "k3")
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
			keys := []string{h.eps[0].Key, h.eps[1].Key, h.eps[2].Key}
			h.at(0, true)
			before := applicableEvidence(t, h.m, keys)
			session := h.m.session
			done := h.askEvidence(keys)
			h.eps = next
			h.m.refresh(h.ctx, t0)
			if h.m.session != session || h.l.startCount() != 1 || h.m.checkableSet()[0].Key != keys[2] {
				t.Fatal("order/active-only rebuild replaced the session or lost active-first order")
			}
			if err := h.m.ValidateEvidence(before, keys); err != nil {
				t.Fatalf("order/active-only rebuild invalidated unchanged records: %v", err)
			}
			evidenceWaiting(t, done)
			h.at(0, false)
			got := evidenceAnswered(t, done)
			if got.err != nil || len(got.evidence.Endpoints) != 3 {
				t.Fatalf("CheckEvidence failed across an order-only rebuild: %+v, %v", got.evidence, got.err)
			}
			if err := h.m.ValidateEvidence(got.evidence, keys); err != nil {
				t.Fatalf("post-rebuild fresh evidence is not applicable: %v", err)
			}
		})
	}
}

func TestEvidence_MissingRejectedAndTimeout(t *testing.T) {
	captureEngineLogs(t)
	t.Run("unknown-active-key-is-incomplete", func(t *testing.T) {
		h := newHarness(t, "unknown")
		h.m.refresh(h.ctx, t0)
		current := h.m.Evidence()
		if _, ok := current.Endpoints["unknown"]; ok {
			t.Fatal("unchecked active key was published as applicable evidence")
		}
		if err := h.m.ValidateEvidence(current, []string{"unknown"}); err == nil {
			t.Fatal("unknown active key passed validation")
		}
		ctx, cancel := context.WithTimeout(h.ctx, 50*time.Millisecond)
		defer cancel()
		got, err := h.m.CheckEvidence(ctx, []string{"unknown"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unchecked active key = %+v, %v; want deadline", got, err)
		}
	})

	t.Run("missing-key-is-not-filtered-out", func(t *testing.T) {
		h := newHarness(t, "k1")
		h.at(0, true)
		ctx, cancel := context.WithTimeout(h.ctx, 150*time.Millisecond)
		defer cancel()
		done := h.askEvidenceContext(ctx, []string{"k1", "absent-active-key"})
		h.complete("k1", nil)
		evidenceWaiting(t, done)
		got := evidenceAnswered(t, done)
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("partially present active keys = %+v, %v; want deadline", got.evidence, got.err)
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"k1", "absent-active-key"}); err == nil {
			t.Fatal("partial evidence validated the missing required key")
		}
		if _, ok := got.evidence.Endpoints["absent-active-key"]; ok {
			t.Fatal("missing key acquired a fabricated endpoint result")
		}
	})

	t.Run("rejected-is-not-fresh-dead", func(t *testing.T) {
		h := newHarness(t, "dead")
		h.refused = map[string]string{"rejected": "unsupported synthetic outbound"}
		h.l.script["dead"] = []error{errTimeout, errTimeout}
		h.at(0, true)
		ctx, cancel := context.WithTimeout(h.ctx, time.Second)
		defer cancel()
		rejected, err := h.m.CheckEvidence(ctx, []string{"rejected"})
		st := rejected.Endpoints["rejected"]
		if err != nil || st.Status != watchdapi.StatusRejected || st.Fails != 0 || !st.CheckedAt.IsZero() {
			t.Fatalf("generator refusal = %+v, %v; want rejected without a check", rejected, err)
		}
		if err := h.m.ValidateEvidence(rejected, []string{"rejected"}); err != nil {
			t.Fatalf("current generator refusal is not applicable: %v", err)
		}
		h.l.mu.Lock()
		h.l.script["dead"] = []error{errTimeout, errTimeout}
		h.l.mu.Unlock()
		done := h.askEvidence([]string{"dead", "rejected"})
		evidenceWaiting(t, done)
		h.at(0, false)
		got := evidenceAnswered(t, done)
		dead := got.evidence.Endpoints["dead"]
		if got.err != nil || dead.Status != watchdapi.StatusDead || dead.Fails != 2 || dead.Error != "timeout" || dead.CheckedAt != t0 || got.evidence.Endpoints["rejected"].Status != watchdapi.StatusRejected {
			t.Fatalf("fresh dead versus static rejected = %+v, %v", got.evidence, got.err)
		}
		if h.l.checkCount("rejected") != 0 || h.l.checkCount("dead") != 4 {
			t.Fatal("rejection was probed or dead evidence reused the prior failure")
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"dead", "rejected"}); err != nil {
			t.Fatalf("resolved dead/rejected evidence is not applicable: %v", err)
		}
	})

	t.Run("deadline-ends-wait-for-gated-worker", func(t *testing.T) {
		h := newHarness(t, "k1")
		h.at(0, true)
		gate, entered := make(chan struct{}), make(chan string, 1)
		cleanGate(t, gate)
		h.l.mu.Lock()
		h.l.gate, h.l.entered = gate, entered
		h.l.mu.Unlock()
		ctx, cancel := context.WithTimeout(h.ctx, 150*time.Millisecond)
		defer cancel()
		done := h.askEvidenceContext(ctx, []string{"k1"})
		h.m.tick(h.ctx, t0)
		enteredCheck(t, entered, "k1")
		got := evidenceAnswered(t, done)
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("gated CheckEvidence = %+v, %v; want deadline", got.evidence, got.err)
		}
		if h.l.checkCount("k1") != 1 {
			t.Fatal("old result or blocked worker was counted as a new completion")
		}
		close(gate)
		h.answer()
	})
}

func TestEvidence_AllRejectedCurrentBuild(t *testing.T) {
	captureEngineLogs(t)
	t.Run("all-static-rejections-need-no-session", func(t *testing.T) {
		h := newHarness(t)
		h.refused = map[string]string{"bad1": "unsupported synthetic transport", "bad2": "missing synthetic address"}
		h.m.refresh(h.ctx, t0)
		if h.m.session != nil || h.l.startCount() != 0 {
			t.Fatal("all-rejected build started a prober")
		}
		keys := []string{"bad1", "bad2"}
		before := applicableEvidence(t, h.m, keys)
		ctx, cancel := context.WithTimeout(h.ctx, time.Second)
		defer cancel()
		got, err := h.m.CheckEvidence(ctx, keys)
		if err != nil || got.State != watchdapi.StateOK || len(got.Endpoints) != 2 || got.Endpoints["bad1"].Status != watchdapi.StatusRejected || got.Endpoints["bad2"].Status != watchdapi.StatusRejected {
			t.Fatalf("all-rejected evidence = %+v, %v", got, err)
		}
		if err := h.m.ValidateEvidence(got, keys); err != nil {
			t.Fatalf("all-rejected proof without a session is not applicable: %v", err)
		}
		h.m.refresh(h.ctx, t0)
		if err := h.m.ValidateEvidence(before, keys); err != nil {
			t.Fatalf("unchanged current refusal lost applicability: %v", err)
		}
		h.refused = map[string]string{"bad1": "changed synthetic generator refusal", "bad2": "missing synthetic address"}
		h.m.refresh(h.ctx, t0)
		if err := h.m.ValidateEvidence(before, []string{"bad1"}); err == nil {
			t.Fatal("changed rejection record kept its preceding revision applicable")
		}
		fresh := applicableEvidence(t, h.m, []string{"bad1"})
		if fresh.Endpoints["bad1"].Error != "changed synthetic generator refusal" {
			t.Fatalf("current build retained the old refusal: %+v", fresh)
		}
		h.stopped = true
		h.m.refresh(h.ctx, t0)
		if err := h.m.ValidateEvidence(fresh, []string{"bad1"}); err == nil {
			t.Fatal("static refusal bypassed the global stop guard")
		}
		if stopped, err := h.m.CheckEvidence(ctx, []string{"bad1"}); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("all-rejected stopped monitor did not refuse immediately: %+v, %v", stopped, err)
		}
	})

	t.Run("persisted-refusal-is-reassessed-by-current-build", func(t *testing.T) {
		h := newHarness(t)
		path := filepath.Join(t.TempDir(), "watchd-state.json")
		h.m = New(h.deps(path))
		h.refused = map[string]string{"bad": "persisted synthetic refusal"}
		h.at(0, true)
		h.m.shutdown()
		h.refused = map[string]string{"bad": "current synthetic refusal"}
		h.m = New(h.deps(path))
		h.m.restore()
		h.m.refresh(h.ctx, t0)
		current := applicableEvidence(t, h.m, []string{"bad"})
		if current.Endpoints["bad"].Status != watchdapi.StatusRejected || current.Endpoints["bad"].Error != "current synthetic refusal" || h.m.session != nil {
			t.Fatalf("persisted rejection bypassed current build: %+v", current)
		}
		h.refused, h.eps = nil, eps("bad")
		h.m.refresh(h.ctx, t0)
		if h.state("bad").Status != watchdapi.StatusUnknown {
			t.Fatal("accepted current build kept the persisted rejection")
		}
		if err := h.m.ValidateEvidence(current, []string{"bad"}); err == nil {
			t.Fatal("accepted outbound retained the preceding rejection proof")
		}
		if _, ok := h.m.Evidence().Endpoints["bad"]; ok {
			t.Fatal("newly accepted but unchecked outbound was already applicable")
		}
		done := h.askEvidence([]string{"bad"})
		h.at(0, false)
		got := evidenceAnswered(t, done)
		if got.err != nil || got.evidence.Endpoints["bad"].Status != watchdapi.StatusAlive {
			t.Fatalf("accepted current build did not acquire live proof: %+v, %v", got.evidence, got.err)
		}
	})

	t.Run("changed-outbound-invalidates-static-rejection", func(t *testing.T) {
		h := newHarness(t)
		server := vpnconfig.Server{Name: "Refused", Address: "evidence.example", Port: 443, IPs: []string{"192.0.2.9"}, Outbound: trojan("evidence.example")}
		subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Synthetic", Servers: []vpnconfig.Server{server}}}
		h.eps, h.refused = Build(subs, nil, outboundAsIs)
		oldKey := endpoint.Keys(server)[0]
		h.m.refresh(h.ctx, t0)
		before := applicableEvidence(t, h.m, []string{oldKey})
		subs[0].Servers[0].Outbound = json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"evidence.example","port":443,"password":"new-synthetic"}]}}`)
		newKey := endpoint.Keys(subs[0].Servers[0])[0]
		if oldKey == newKey {
			t.Fatal("fixture did not change the outbound key")
		}
		h.eps, h.refused = Build(subs, nil, outboundAsIs)
		h.m.refresh(h.ctx, t0)
		for _, key := range []string{oldKey, newKey} {
			if err := h.m.ValidateEvidence(before, []string{key}); err == nil {
				t.Fatal("changed outbound accepted its preceding rejection proof")
			}
		}
		fresh := applicableEvidence(t, h.m, []string{newKey})
		if _, ok := fresh.Endpoints[oldKey]; ok {
			t.Fatal("current all-rejected build retained the old outbound key")
		}
		if fresh.Endpoints[newKey].Status != watchdapi.StatusRejected || h.m.session != nil || h.l.startCount() != 0 {
			t.Fatal("new static refusal lost applicability without a live session")
		}
	})
}

func TestEvidence_RestoredXrayRejection(t *testing.T) {
	captureEngineLogs(t)
	restoreRefusal := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t, "bad")
		path := filepath.Join(t.TempDir(), "watchd-state.json")
		h.m = New(h.deps(path))
		h.l.refuse["bad"] = "persisted synthetic Xray refusal"
		h.m.refresh(h.ctx, t0)
		before := applicableEvidence(t, h.m, []string{"bad"})
		if before.Endpoints["bad"].Status != watchdapi.StatusRejected || before.Endpoints["bad"].Error != "persisted synthetic Xray refusal" || h.m.session != nil {
			t.Fatal("fixture did not obtain an all-rejected Xray build")
		}
		h.m.shutdown()
		h.l = newFakeLauncher()
		h.m = New(h.deps(path))
		h.m.restore()
		saved, ok := h.m.restored["bad"]
		if !ok || !saved.Sticky || saved.State.Status != watchdapi.StatusRejected || saved.State.Error != "persisted synthetic Xray refusal" {
			t.Fatal("save/restore did not retain the sticky Xray rejection")
		}
		t.Cleanup(func() { h.m.shutdown() })
		return h
	}

	t.Run("current-refusal-has-new-proof", func(t *testing.T) {
		h := restoreRefusal(t)
		h.l.refuse["bad"] = "current synthetic Xray refusal"
		h.m.refresh(h.ctx, t0)
		h.m.refresh(h.ctx, t0)
		ctx, cancel := context.WithTimeout(h.ctx, 150*time.Millisecond)
		defer cancel()
		got, err := h.m.CheckEvidence(ctx, []string{"bad"})
		st := got.Endpoints["bad"]
		if err != nil || got.State != watchdapi.StateOK || st.Status != watchdapi.StatusRejected || st.Error != "current synthetic Xray refusal" || st.Fails != 0 || !st.CheckedAt.IsZero() {
			t.Fatalf("restored Xray refusal was not reassessed by the current build: %+v, %v", got, err)
		}
		if err := h.m.ValidateEvidence(got, []string{"bad"}); err != nil {
			t.Fatalf("current Xray refusal has no applicable proof: %v", err)
		}
		current := applicableEvidence(t, h.m, []string{"bad"})
		if current.Endpoints["bad"] != st || h.m.session != nil || h.l.checkCount("bad") != 0 {
			t.Fatal("all-rejected current build lost its static proof or dispatched a live check")
		}
	})

	t.Run("accepted-build-needs-live-check", func(t *testing.T) {
		h := restoreRefusal(t)
		h.m.refresh(h.ctx, t0)
		if h.state("bad").Status != watchdapi.StatusUnknown {
			t.Fatal("accepted current Xray build retained the persisted sticky rejection")
		}
		if _, ok := h.m.Evidence().Endpoints["bad"]; ok {
			t.Fatal("accepted but unchecked outbound acquired proof from a persisted rejection")
		}
		done := h.askEvidence([]string{"bad"})
		evidenceWaiting(t, done)
		h.at(0, false)
		got := evidenceAnswered(t, done)
		st := got.evidence.Endpoints["bad"]
		if got.err != nil || st.Status != watchdapi.StatusAlive || st.CheckedAt != t0 || st.LatencyMS != 100 || h.l.checkCount("bad") != 1 {
			t.Fatalf("accepted restored outbound did not acquire fresh live proof: %+v, %v", got.evidence, got.err)
		}
		if err := h.m.ValidateEvidence(got.evidence, []string{"bad"}); err != nil {
			t.Fatalf("post-restore live completion is not applicable: %v", err)
		}
	})

	t.Run("reevaluation-does-not-block-ipc", func(t *testing.T) {
		h := restoreRefusal(t)
		h.l.refuse["bad"] = "current synthetic Xray refusal"
		entered, release, refreshed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		block := func(set []Endpoint) {
			for _, ep := range set {
				if ep.Key == "bad" {
					once.Do(func() { close(entered) })
					<-release
				}
			}
		}
		launcher := &launcherFaults{fakeLauncher: h.l}
		launcher.onStart = func(ctx context.Context, set []Endpoint) (Session, error) {
			block(set)
			return h.l.Start(ctx, set)
		}
		launcher.onTest = func(ctx context.Context, set []Endpoint) error {
			block(set)
			return h.l.Test(ctx, set)
		}
		h.m.d.Launcher = launcher
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
			select {
			case <-refreshed:
			case <-time.After(5 * time.Second):
				t.Error("gated restored-rejection refresh did not stop")
			}
		})
		go func() {
			h.m.refresh(h.ctx, t0)
			close(refreshed)
		}()
		select {
		case <-entered:
		case <-refreshed:
			t.Fatal("restored Xray rejection skipped current launcher reevaluation")
		case <-time.After(5 * time.Second):
			t.Fatal("restored Xray rejection did not reach launcher reevaluation")
		}

		path := filepath.Join(t.TempDir(), "w.sock")
		listener, err := watchdapi.Listen(h.ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		serveCtx, cancel := context.WithCancel(h.ctx)
		served := make(chan error, 1)
		go func() { served <- watchdapi.ServeListener(serveCtx, listener, h.m) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("IPC shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("IPC did not stop")
			}
			if err := listener.Close(); err != nil {
				t.Errorf("listener close: %v", err)
			}
		})
		cleanGate(t, release)
		validated := make(chan evidenceAnswer, 1)
		go func() {
			current := h.m.Evidence()
			validated <- evidenceAnswer{current, h.m.ValidateEvidence(current, []string{"bad"})}
		}()
		client := watchdapi.NewClient(path)
		ipcCtx, stop := context.WithTimeout(h.ctx, time.Second)
		defer stop()
		snapshot, err := client.Monitor(ipcCtx)
		if _, ok := snapshot.Endpoints["bad"]; err != nil || snapshot.State != watchdapi.StateOK || !ok {
			t.Errorf("GET monitor blocked during restored-rejection reevaluation: %+v, %v", snapshot, err)
		}
		if queued, err := client.Check(ipcCtx, []string{"bad"}); err != nil || queued < 0 || queued > 1 {
			t.Errorf("POST check blocked during restored-rejection reevaluation: queued %d, %v", queued, err)
		}
		select {
		case got := <-validated:
			if _, ok := got.evidence.Endpoints["bad"]; got.err == nil || ok {
				t.Error("persisted rejection became applicable before launcher reevaluation finished")
			}
		case <-time.After(time.Second):
			t.Error("evidence access waited for restored-rejection launcher reevaluation")
		}
		close(release)
		select {
		case <-refreshed:
		case <-time.After(5 * time.Second):
			t.Fatal("released restored-rejection refresh did not complete")
		}
		current := applicableEvidence(t, h.m, []string{"bad"})
		if current.Endpoints["bad"].Status != watchdapi.StatusRejected || current.Endpoints["bad"].Error != "current synthetic Xray refusal" {
			t.Fatalf("released launcher reevaluation retained the persisted refusal: %+v", current)
		}
	})
}

func TestEvidence_DoesNotBlockIPC(t *testing.T) {
	captureEngineLogs(t)
	for _, stage := range []string{"endpoint-build", "launcher-validation"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t, "k1")
			h.at(0, true)
			before := applicableEvidence(t, h.m, []string{"k1"})
			path := filepath.Join(t.TempDir(), "w.sock")
			listener, err := watchdapi.Listen(h.ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			serveCtx, cancel := context.WithCancel(h.ctx)
			served := make(chan error, 1)
			go func() { served <- watchdapi.ServeListener(serveCtx, listener, h.m) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-served:
					if err != nil {
						t.Errorf("IPC shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("IPC did not stop")
				}
				if err := listener.Close(); err != nil {
					t.Errorf("listener close: %v", err)
				}
			})
			entered, release, refreshed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			block := func() {
				once.Do(func() { close(entered) })
				<-release
			}
			if stage == "endpoint-build" {
				read := h.m.d.Endpoints
				h.m.d.Endpoints = func() ([]Endpoint, map[string]string, error) {
					block()
					return read()
				}
			} else {
				h.eps = eps("k1", "k2")
				launcher := &launcherFaults{fakeLauncher: h.l}
				launcher.onStart = func(context.Context, []Endpoint) (Session, error) {
					return nil, &ConfigError{Reason: "synthetic config refusal"}
				}
				launcher.onTest = func(context.Context, []Endpoint) error {
					block()
					return &ConfigError{Reason: "synthetic config refusal"}
				}
				h.m.d.Launcher = launcher
			}
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				select {
				case <-refreshed:
				case <-time.After(5 * time.Second):
					t.Error("gated refresh did not stop")
				}
			})
			go func() {
				h.m.refresh(h.ctx, t0)
				close(refreshed)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not reach the gated external work")
			}
			validated := make(chan error, 1)
			go func() { validated <- h.m.ValidateEvidence(before, []string{"k1"}) }()
			client := watchdapi.NewClient(path)
			ipcCtx, stop := context.WithTimeout(h.ctx, time.Second)
			defer stop()
			snapshot, err := client.Monitor(ipcCtx)
			if err != nil || snapshot.State != watchdapi.StateOK || snapshot.Endpoints["k1"].Status != watchdapi.StatusAlive {
				t.Errorf("GET monitor blocked or changed during %s: %+v, %v", stage, snapshot, err)
			}
			queued, err := client.Check(ipcCtx, []string{"k1"})
			if err != nil || queued != 1 {
				t.Errorf("POST check blocked during %s: queued %d, %v", stage, queued, err)
			}
			select {
			case err := <-validated:
				if stage == "endpoint-build" && err != nil {
					t.Errorf("unpublished build invalidated current proof: %v", err)
				}
				if stage == "launcher-validation" && err == nil {
					t.Error("changed set/ended session retained old proof during validation")
				}
			case <-time.After(time.Second):
				t.Error("revalidation waited for config generation/validation")
			}
			close(release)
			select {
			case <-refreshed:
			case <-time.After(5 * time.Second):
				t.Fatal("released refresh did not complete")
			}
		})
	}
}

func TestEvidence_ImmutableAndOwnerBound(t *testing.T) {
	captureEngineLogs(t)
	h := newHarness(t, "alive", "dead")
	h.l.script["dead"] = []error{errTimeout, errTimeout}
	h.at(0, true)
	original := applicableEvidence(t, h.m, []string{"alive", "dead"})
	edited := h.m.Evidence()
	edited.Endpoints["dead"] = watchdapi.EndpointState{Status: watchdapi.StatusAlive, LatencyMS: 1, CheckedAt: t0}
	if original.Endpoints["dead"].Status != watchdapi.StatusDead || h.state("dead").Status != watchdapi.StatusDead {
		t.Fatal("Evidence shared its endpoint map with another value or the monitor")
	}
	if err := h.m.ValidateEvidence(edited, []string{"dead"}); err == nil {
		t.Fatal("edited public result forged an alive proof from a dead record")
	}
	if err := h.m.ValidateEvidence(original, []string{"dead"}); err != nil {
		t.Fatalf("editing another value corrupted the original private records: %v", err)
	}
	missing := h.m.Evidence()
	delete(missing.Endpoints, "alive")
	if err := h.m.ValidateEvidence(missing, []string{"alive"}); err == nil {
		t.Fatal("removed required result retained a valid proof")
	}
	inactive := h.m.Evidence()
	inactive.State = watchdapi.StateStopped
	if err := h.m.ValidateEvidence(inactive, []string{"alive"}); err == nil {
		t.Fatal("edited inactive state retained a valid proof")
	}
	forged := Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{"alive": original.Endpoints["alive"]}}
	if err := h.m.ValidateEvidence(forged, []string{"alive"}); err == nil {
		t.Fatal("public fields alone manufactured a private monitor proof")
	}
	other := newHarness(t, "alive", "dead")
	other.l.script["dead"] = []error{errTimeout, errTimeout}
	other.at(0, true)
	if err := other.m.ValidateEvidence(original, []string{"alive", "dead"}); err == nil {
		t.Fatal("another Monitor accepted a foreign proof with matching public states")
	}
	h.complete("alive", nil)
	if original.Endpoints["alive"].LatencyMS != 100 || original.Endpoints["alive"].CheckedAt != t0 {
		t.Fatal("new completion mutated a returned evidence value")
	}
	if err := h.m.ValidateEvidence(original, []string{"alive"}); err == nil {
		t.Fatal("shared private records advanced an old token to the new completion")
	}
}

func TestEvidence_SharesWorkerPool(t *testing.T) {
	captureEngineLogs(t)
	h := newHarness(t, "k1", "k2", "k3")
	h.settings.Concurrency = 1
	g1, g2, g3 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cleanGate(t, g1)
	cleanGate(t, g2)
	cleanGate(t, g3)
	entered := make(chan string, 8)
	h.l.gates, h.l.entered = map[string]chan struct{}{"k1": g1, "k2": g2, "k3": g3}, entered
	h.m.refresh(h.ctx, t0)
	if n, err := h.m.Request([]string{"k1"}); err != nil || n != 1 {
		t.Fatalf("Request = %d, %v", n, err)
	}
	h.m.tick(h.ctx, t0)
	enteredCheck(t, entered, "k1")
	legacy := h.ask([]string{"k2"})
	fresh := h.askEvidence([]string{"k3", "k3"})
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 1 {
		t.Fatal("evidence request exceeded the monitor's single worker")
	}
	select {
	case key := <-entered:
		t.Fatalf("%s entered a separate pool while the monitor worker was busy", key)
	case <-time.After(20 * time.Millisecond):
	}
	evidenceWaiting(t, fresh)
	close(g1)
	h.answer()
	h.m.tick(h.ctx, t0)
	enteredCheck(t, entered, "k2")
	if h.inFlight() != 1 {
		t.Fatal("legacy Check lost the common urgent queue or concurrency bound")
	}
	close(g2)
	h.answer()
	gotLegacy := answered(t, legacy)
	if gotLegacy.err != nil || gotLegacy.states["k2"].Status != watchdapi.StatusAlive {
		t.Fatalf("existing Check contract changed: %+v, %v", gotLegacy.states, gotLegacy.err)
	}
	evidenceWaiting(t, fresh)
	h.m.tick(h.ctx, t0)
	enteredCheck(t, entered, "k3")
	if h.inFlight() != 1 {
		t.Fatal("evidence refill exceeded the common concurrency bound")
	}
	close(g3)
	h.answer()
	got := evidenceAnswered(t, fresh)
	if got.err != nil || got.evidence.Endpoints["k3"].Status != watchdapi.StatusAlive {
		t.Fatalf("common-pool evidence = %+v, %v", got.evidence, got.err)
	}
	if err := h.m.ValidateEvidence(got.evidence, []string{"k3"}); err != nil {
		t.Fatalf("common-pool completion is not applicable: %v", err)
	}
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 0 || h.l.startCount() != 1 || h.l.checkCount("k1") != 1 || h.l.checkCount("k2") != 1 || h.l.checkCount("k3") != 1 {
		t.Fatal("duplicate evidence keys created extra checks or a separate prober")
	}
}
