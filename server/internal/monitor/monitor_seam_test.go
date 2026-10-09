package monitor

import (
	"context"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Check is a test helper over the production path: queue and the changed
// signal, which CheckEvidence, the watch's in-process check, waits on. It makes
// the endpoints of keys due now and waits until each has an answer from after
// the call, or ctx ends; it returns their states as they stand then. A key not
// in the set yet is waited for too.
func (m *Monitor) Check(ctx context.Context, keys []string) (map[string]watchdapi.EndpointState, error) {
	m.mu.Lock()
	start := m.sequence
	if len(keys) == 0 {
		keys = make([]string, 0, len(m.entries))
		for k := range m.entries {
			keys = append(keys, k)
		}
	}
	_, err := m.queue(keys, true)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for {
		m.mu.Lock()
		out := make(map[string]watchdapi.EndpointState, len(keys))
		done := true
		for _, k := range keys {
			e := m.entries[k]
			if e == nil {
				done = false
				continue
			}
			out[k] = e.st
			if e.checkable() && (e.completed <= start || e.completedSession != m.session || m.session == nil || exited(m.session)) {
				done = false
			}
		}
		inactive := m.state == watchdapi.StateStopped || m.state == watchdapi.StateDisabled
		ch := m.changed
		m.mu.Unlock()
		if inactive {
			return out, watchdapi.ErrNotActive
		}
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ch:
		}
	}
}
