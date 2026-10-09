package watchdapi

import "time"

// ServerHealth is a server's status folded from its endpoints (Health).
type ServerHealth struct {
	Status    Status    `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	Since     time.Time `json:"since"`
	NextAt    time.Time `json:"next_at"`
	Error     string    `json:"error,omitempty"`
}

// Health folds the endpoints of one server - its keys, one per address
// (endpoint.Keys) - into one status: alive when any address is alive, with the
// best latency; otherwise unknown when any address is not yet checked, or not
// in snap at all; otherwise dead when any is dead, as the latest check found
// it; otherwise rejected.
func Health(keys []string, snap Snapshot) ServerHealth {
	var alive, unknown, dead, rejected *EndpointState
	for _, k := range keys {
		st, ok := snap.Endpoints[k]
		if !ok || st.Status == "" {
			st = EndpointState{Status: StatusUnknown}
		}
		switch st.Status {
		case StatusAlive:
			if alive == nil || st.LatencyMS < alive.LatencyMS {
				alive = &st
			}
		case StatusDead:
			if dead == nil || st.CheckedAt.After(dead.CheckedAt) {
				dead = &st
			}
		case StatusRejected:
			if rejected == nil {
				rejected = &st
			}
		default:
			if unknown == nil {
				unknown = &st
			}
		}
	}
	pick := alive
	for _, next := range []*EndpointState{unknown, dead, rejected} {
		if pick == nil {
			pick = next
		}
	}
	if pick == nil {
		return ServerHealth{Status: StatusUnknown}
	}
	return ServerHealth{
		Status:    pick.Status,
		LatencyMS: pick.LatencyMS,
		CheckedAt: pick.CheckedAt,
		Since:     pick.Since,
		NextAt:    pick.NextAt,
		Error:     pick.Error,
	}
}
