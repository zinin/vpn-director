package monitor

import (
	"context"
	"errors"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Evidence is an independent copy of results with a private applicability proof.
type Evidence struct {
	State     watchdapi.State
	Endpoints map[string]watchdapi.EndpointState
	token     evidenceToken
}

type evidenceToken struct {
	owner      *Monitor
	session    Session
	generation uint64
	records    map[string]evidenceRecord
}

type evidenceRecord struct {
	completion uint64
	revision   uint64
	status     watchdapi.Status
}

var (
	errEvidenceStale    = errors.New("monitor evidence is stale or incomplete")
	errEvidenceInactive = errors.New("monitor evidence is inactive")
)

// Evidence returns only results applicable to the current set and activity epoch.
func (m *Monitor) Evidence() Evidence {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evidenceLocked(nil)
}

// CheckEvidence waits for checks dispatched after the call in one fixed epoch.
// Generator refusals need no worker or live session, but missing keys stay incomplete.
func (m *Monitor) CheckEvidence(ctx context.Context, keys []string) (Evidence, error) {
	m.mu.Lock()
	keys = m.evidenceKeys(keys)
	generation, session := m.generation, m.session
	after, afterStart := m.sequence, m.starts
	out := m.evidenceLocked(keys)
	if err := m.evidenceActive(); err != nil {
		m.mu.Unlock()
		return out, err
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return out, err
	}
	if len(keys) == 0 {
		m.mu.Unlock()
		return out, errEvidenceStale
	}
	if _, err := m.queue(keys, true); err != nil {
		m.mu.Unlock()
		return out, err
	}
	for _, key := range keys {
		if e := m.entries[key]; e != nil && e.checkable() {
			e.evidenceFollowUp, e.evidenceAfter = true, afterStart
		}
	}
	var gone <-chan struct{}
	if session != nil {
		gone = session.Exited()
	}
	m.mu.Unlock()
	for {
		m.mu.Lock()
		out = m.evidenceLocked(keys)
		if err := m.evidenceActive(); err != nil {
			m.mu.Unlock()
			return out, err
		}
		if m.generation != generation || m.session != session {
			m.mu.Unlock()
			return out, errEvidenceStale
		}
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return out, err
		}
		done := true
		for _, key := range keys {
			record, ok := out.token.records[key]
			if !ok {
				done = false
				continue
			}
			if record.status != watchdapi.StatusRejected && (record.completion <= after || m.entries[key].completedStart <= afterStart) {
				done = false
			}
		}
		changed := m.changed
		m.mu.Unlock()
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-changed:
		case <-gone:
		}
	}
}

// ValidateEvidence compares requested results using cached records under a short lock.
func (m *Monitor) ValidateEvidence(e Evidence, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.evidenceActive(); err != nil {
		return err
	}
	if e.State != m.state || e.token.owner != m || e.token.generation != m.generation || e.token.session != m.session {
		return errEvidenceStale
	}
	keys = m.evidenceKeys(keys)
	if len(keys) == 0 {
		return errEvidenceStale
	}
	for _, key := range keys {
		entry := m.entries[key]
		current, applicable := m.recordFor(entry)
		proof, proved := e.token.records[key]
		state, present := e.Endpoints[key]
		if !applicable || !proved || !present || current != proof || state != entry.st {
			return errEvidenceStale
		}
	}
	return nil
}

// evidenceActive uses no config readers or launcher operations; mu is held.
func (m *Monitor) evidenceActive() error {
	if m.state == watchdapi.StateStopped || m.state == watchdapi.StateDisabled || !m.settings.Enabled {
		return watchdapi.ErrNotActive
	}
	if m.closed || m.state != watchdapi.StateOK || m.wanPaused || (m.session != nil && exited(m.session)) {
		return errEvidenceInactive
	}
	return nil
}

// evidenceKeys copies and deduplicates a request; empty means the current set.
// The caller holds mu.
func (m *Monitor) evidenceKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	if len(keys) == 0 {
		for key := range m.entries {
			out = append(out, key)
		}
		return out
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// evidenceLocked copies both public values and private records with mu held.
func (m *Monitor) evidenceLocked(keys []string) Evidence {
	out := Evidence{
		State:     m.state,
		Endpoints: make(map[string]watchdapi.EndpointState),
		token: evidenceToken{
			owner: m, session: m.session, generation: m.generation,
			records: make(map[string]evidenceRecord),
		},
	}
	if m.evidenceActive() != nil {
		return out
	}
	if keys == nil {
		keys = m.evidenceKeys(nil)
	}
	for _, key := range keys {
		e := m.entries[key]
		if record, ok := m.recordFor(e); ok {
			out.Endpoints[key], out.token.records[key] = e.st, record
		}
	}
	return out
}

// recordFor excludes restored checks and unrevalidated persisted rejections.
// The caller holds mu and has checked activity.
func (m *Monitor) recordFor(e *entry) (evidenceRecord, bool) {
	if e == nil {
		return evidenceRecord{}, false
	}
	switch e.st.Status {
	case watchdapi.StatusRejected:
		if !e.rejectedCurrent {
			return evidenceRecord{}, false
		}
	case watchdapi.StatusAlive, watchdapi.StatusDead:
		if m.session == nil || e.completed == 0 || e.completedSession != m.session || e.completedGeneration != m.generation {
			return evidenceRecord{}, false
		}
	default:
		return evidenceRecord{}, false
	}
	return evidenceRecord{completion: e.completed, revision: e.revision, status: e.st.Status}, true
}

// invalidateEvidence advances applicability without changing public statuses or legacy Check intent.
// The caller holds mu.
func (m *Monitor) invalidateEvidence() {
	m.generation++
	for _, e := range m.entries {
		e.evidenceFollowUp = false
	}
	m.notify()
}
