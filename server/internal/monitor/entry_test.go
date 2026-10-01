package monitor

import (
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var t0 = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func minuteSettings() Settings {
	return Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
}

func TestEntry_ALiveEndpointComesBackAfterTheIntervalWithinTheJitter(t *testing.T) {
	for _, jitter := range []float64{-1, 0, 0.999} {
		e := &entry{}
		if !e.succeed(t0, 142*time.Millisecond, minuteSettings(), jitter) {
			t.Fatal("a first success changed nothing")
		}
		wait := e.st.NextAt.Sub(t0)
		if wait < 54*time.Second || wait > 66*time.Second {
			t.Fatalf("jitter %v: next check in %s, want 60s ± 10%%", jitter, wait)
		}
		if e.st.Status != watchdapi.StatusAlive || e.st.LatencyMS != 142 || e.st.Since != t0 || e.st.CheckedAt != t0 {
			t.Fatalf("state %+v", e.st)
		}
	}
}

// A dead endpoint waits twice the interval, then twice its last pause, up to
// the cap; one success brings it back to the interval.
func TestEntry_TheDeadPauseDoublesToItsCapAndASuccessResetsIt(t *testing.T) {
	e := &entry{}
	now := t0
	var pauses []time.Duration
	for i := 0; i < 7; i++ {
		e.fail(now, "timeout", minuteSettings())
		pauses = append(pauses, e.st.NextAt.Sub(now))
		now = e.st.NextAt
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if pauses[i] != want[i] {
			t.Fatalf("pauses %v, want %v", pauses, want)
		}
	}
	if e.st.Fails != 7 || e.st.Since != t0 || e.st.Error != "timeout" {
		t.Fatalf("state %+v", e.st)
	}

	e.succeed(now, time.Second, minuteSettings(), 0)
	e.fail(now.Add(time.Minute), "timeout", minuteSettings())
	if got := e.st.NextAt.Sub(now.Add(time.Minute)); got != 2*time.Minute {
		t.Fatalf("pause after a success %s, want 2m", got)
	}
}

// The latency of the last success stays on a dead endpoint: the page shows it
// no longer, the record keeps it.
func TestEntry_AFailureKeepsTheLastLatency(t *testing.T) {
	e := &entry{}
	e.succeed(t0, 120*time.Millisecond, minuteSettings(), 0)
	if changed := e.fail(t0.Add(time.Minute), "HTTP 403", minuteSettings()); !changed {
		t.Fatal("alive to dead reported no change")
	}
	if e.st.LatencyMS != 120 || e.st.Since != t0.Add(time.Minute) {
		t.Fatalf("state %+v", e.st)
	}
}
