package subwatch

import (
	"reflect"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func orderTestEvidence(now time.Time, statuses map[string]watchdapi.Status) monitor.Evidence {
	e := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	for key, status := range statuses {
		st := watchdapi.EndpointState{Status: status, CheckedAt: now, NextAt: now.Add(time.Minute), Since: now}
		switch status {
		case watchdapi.StatusAlive:
			st.LatencyMS = 17
		case watchdapi.StatusDead:
			st.Fails, st.Error = 1, "timeout"
		case watchdapi.StatusRejected:
			st.NextAt, st.Error = time.Time{}, "synthetic generator refusal"
		}
		e.Endpoints[key] = st
	}
	return e
}

func healthLabels(servers []vpnconfig.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name + "@" + dialIP(s)
	}
	return out
}

func TestHealthOrder_StableGroupsAndCurrentRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	t.Run("alive first without latency sorting and missing keys stay unknown", func(t *testing.T) {
		servers := subOf("aaaaaaaa", "Alpha", "", "unknown1", "alive1", "dead1", "alive2", "rejected1").Servers
		e := orderTestEvidence(now, map[string]watchdapi.Status{
			endpoint.Key(servers[1]): watchdapi.StatusAlive,
			endpoint.Key(servers[2]): watchdapi.StatusDead,
			endpoint.Key(servers[3]): watchdapi.StatusAlive,
			endpoint.Key(servers[4]): watchdapi.StatusRejected,
		})
		st := e.Endpoints[endpoint.Key(servers[1])]
		st.LatencyMS = 900
		e.Endpoints[endpoint.Key(servers[1])] = st
		st = e.Endpoints[endpoint.Key(servers[3])]
		st.LatencyMS = 1
		e.Endpoints[endpoint.Key(servers[3])] = st

		got := healthOrder(servers, e)

		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		if want := []string{"alive1", "alive2", "unknown1", "dead1"}; !reflect.DeepEqual(names, want) {
			t.Fatalf("health order %v, want %v", names, want)
		}
	})

	t.Run("stable after OwnFirst round robin and PerAddress", func(t *testing.T) {
		alpha := subOf("aaaaaaaa", "Alpha", "", "A1", "A2", "A3", "A4", "A5")
		beta := subOf("bbbbbbbb", "Beta", "", "B1", "B2")
		gamma := subOf("cccccccc", "Gamma", "", "G1", "G2")
		alpha.Servers[1].IPs = []string{"203.0.113.21", "203.0.113.22"}
		beta.Servers[0].IPs = []string{"198.51.100.31", "198.51.100.32"}
		order, first := walkOrder([]vpnconfig.Subscription{alpha, beta, gamma}, chosenOf("aaaaaaaa", "A2"))
		copies := endpoint.PerAddress(order)
		wantBase := []string{
			"A2@203.0.113.21", "A2@203.0.113.22", "A1@203.0.113.10", "A3@203.0.113.12",
			"B1@198.51.100.31", "B1@198.51.100.32", "G1@203.0.113.10", "A4@203.0.113.13",
			"B2@203.0.113.11", "G2@203.0.113.11", "A5@203.0.113.14",
		}
		if got := healthLabels(copies); !first || !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("original order %v, chosen first %v, want %v", got, first, wantBase)
		}
		e := orderTestEvidence(now, map[string]watchdapi.Status{
			endpoint.Key(copies[0]): watchdapi.StatusDead,
			endpoint.Key(copies[1]): watchdapi.StatusAlive,
			endpoint.Key(copies[3]): watchdapi.StatusAlive,
			endpoint.Key(copies[5]): watchdapi.StatusAlive,
			endpoint.Key(copies[6]): watchdapi.StatusAlive,
			endpoint.Key(copies[7]): watchdapi.StatusUnknown,
			endpoint.Key(copies[8]): watchdapi.StatusRejected,
			endpoint.Key(copies[9]): watchdapi.StatusDead,
		})

		got := healthOrder(copies, e)

		want := []string{
			"A2@203.0.113.22", "A3@203.0.113.12", "B1@198.51.100.32", "G1@203.0.113.10",
			"A2@203.0.113.21", "A1@203.0.113.10", "B1@198.51.100.31", "A4@203.0.113.13",
			"G2@203.0.113.11", "A5@203.0.113.14",
		}
		if labels := healthLabels(got); !reflect.DeepEqual(labels, want) {
			t.Fatalf("health order %v, want %v", labels, want)
		}
	})

	t.Run("no matching keys retains every candidate", func(t *testing.T) {
		servers := subOf("aaaaaaaa", "Alpha", "", "unknown1", "alive1", "dead1", "alive2", "rejected1").Servers
		got := healthOrder(servers, orderTestEvidence(now, nil))
		want := []string{"unknown1@203.0.113.10", "alive1@203.0.113.11", "dead1@203.0.113.12", "alive2@203.0.113.13", "rejected1@203.0.113.14"}
		if labels := healthLabels(got); !reflect.DeepEqual(labels, want) {
			t.Fatalf("missing evidence changed order: %v, want %v", labels, want)
		}
	})
}
