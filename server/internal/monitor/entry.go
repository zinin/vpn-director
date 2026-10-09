package monitor

import (
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// jitterShare is how far a live endpoint's next check may move, as a share of
// the interval, so checks spread over the minute instead of arriving in waves.
const jitterShare = 0.1

// entry is the monitor's record of one endpoint.
type entry struct {
	ep Endpoint
	st watchdapi.EndpointState
	// pause is a dead endpoint's current wait before its next check; zero
	// while it is not dead.
	pause time.Duration
	// sticky marks a rejection that holds until the outbound changes - Xray
	// refused it, or it crashed Xray - as opposed to one the generator makes,
	// which every refresh decides again.
	sticky bool
	// inFlight is set while a worker checks the endpoint.
	inFlight bool
	// pending holds a failed completion until WAN evidence resolves it.
	pending bool
	// Revisions bind evidence to a record; restored rejections have no proof.
	revision        uint64
	rejectedCurrent bool
	// completed identifies the last resolved check of completedSession.
	completed           uint64
	completedSession    Session
	completedGeneration uint64
	completedStart      uint64
	// followUp keeps Check urgent until a completion newer than after resolves.
	followUp bool
	after    uint64
	// Evidence also requires dispatch after its request, not just completion.
	evidenceFollowUp bool
	evidenceAfter    uint64
	// urgent puts the endpoint ahead of the rest (Request).
	urgent bool
}

// checkable reports an endpoint the prober holds.
func (e *entry) checkable() bool {
	return e.st.Status != watchdapi.StatusRejected
}

// succeed records a check that got its answer; jitter is in [-1, 1). It
// reports whether the status changed.
func (e *entry) succeed(now time.Time, latency time.Duration, s Settings, jitter float64) bool {
	e.revision++
	changed := e.st.Status != watchdapi.StatusAlive
	if changed {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusAlive
	e.st.LatencyMS = latency.Milliseconds()
	e.st.CheckedAt = now
	e.st.Fails = 0
	e.st.Error = ""
	e.pause = 0
	delay := float64(s.Interval) * (1 + jitterShare*jitter)
	wait := time.Duration(1<<63 - 1)
	if delay < float64(wait) {
		wait = time.Duration(delay)
	}
	e.st.NextAt = now.Add(wait)
	return changed
}

// fail records a check that failed after its retry: the endpoint is dead and
// waits 2 × Interval, then twice its last pause, up to DeadMax. It reports
// whether the status changed. The latency of the last success stays.
func (e *entry) fail(now time.Time, reason string, s Settings) bool {
	e.revision++
	changed := e.st.Status != watchdapi.StatusDead
	if changed {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusDead
	e.st.CheckedAt = now
	e.st.Fails++
	e.st.Error = reason
	if e.pause == 0 {
		e.pause = s.Interval
	}
	if e.pause > s.DeadMax/2 {
		e.pause = s.DeadMax
	} else {
		e.pause *= 2
	}
	e.st.NextAt = now.Add(e.pause)
	return changed
}

// reject takes the endpoint out of the prober. A sticky rejection holds until
// the outbound changes, which makes a new key.
func (e *entry) reject(now time.Time, reason string, sticky bool) {
	before, wasSticky := e.st, e.sticky
	if e.st.Status != watchdapi.StatusRejected {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusRejected
	e.st.Error = reason
	e.st.NextAt = time.Time{}
	e.pause = 0
	e.sticky = sticky
	e.inFlight = false
	e.pending = false
	e.urgent = false
	e.followUp = false
	e.evidenceFollowUp = false
	if before != e.st || wasSticky != sticky || !e.rejectedCurrent {
		e.revision++
	}
	e.rejectedCurrent = true
}
