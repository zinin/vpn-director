package watchdapi

import (
	"testing"
	"time"
)

func snap(states map[string]EndpointState) Snapshot {
	return Snapshot{State: StateOK, Endpoints: states}
}

// A server lives while any of its addresses does: the walk dials every one.
func TestHealth_AliveWhenAnyAddressIsAliveWithTheBestLatency(t *testing.T) {
	s := snap(map[string]EndpointState{
		"a": {Status: StatusDead, Error: "timeout"},
		"b": {Status: StatusAlive, LatencyMS: 300},
		"c": {Status: StatusAlive, LatencyMS: 120},
	})
	h := Health([]string{"a", "b", "c"}, s)
	if h.Status != StatusAlive || h.LatencyMS != 120 {
		t.Fatalf("health %+v, want alive at 120 ms", h)
	}
}

// Without a live address, one not yet checked leaves the question open: the
// server is unknown, not dead. A key the monitor has not seen yet - a refresh
// it has not read - counts as not checked.
func TestHealth_UnknownBeforeDead(t *testing.T) {
	s := snap(map[string]EndpointState{"a": {Status: StatusDead}})
	if h := Health([]string{"a", "gone"}, s); h.Status != StatusUnknown {
		t.Fatalf("health %+v, want unknown", h)
	}
	if h := Health([]string{"a"}, s); h.Status != StatusDead {
		t.Fatalf("health %+v, want dead", h)
	}
}

func TestHealth_DeadAsTheLatestCheckFoundIt(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	s := snap(map[string]EndpointState{
		"a": {Status: StatusDead, CheckedAt: t0, Error: "timeout"},
		"b": {Status: StatusDead, CheckedAt: t0.Add(time.Minute), Error: "HTTP 403"},
		"c": {Status: StatusRejected, Error: "crashes Xray"},
	})
	if h := Health([]string{"a", "b", "c"}, s); h.Status != StatusDead || h.Error != "HTTP 403" {
		t.Fatalf("health %+v, want dead with the latest error", h)
	}
	if h := Health([]string{"c"}, s); h.Status != StatusRejected || h.Error != "crashes Xray" {
		t.Fatalf("health %+v, want rejected with its reason", h)
	}
}

func TestHealth_NoKeysIsUnknown(t *testing.T) {
	if h := Health(nil, snap(nil)); h.Status != StatusUnknown {
		t.Fatalf("health %+v", h)
	}
}
