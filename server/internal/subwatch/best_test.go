package subwatch

import (
	"encoding/json"
	"fmt"
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

func TestOrderBest_StableGroupsAndCurrentRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	t.Run("alive first by latency", func(t *testing.T) {
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

		got := orderBest(servers, bestRef{Subscription: "aaaaaaaa"}, e, true)

		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		if want := []string{"alive2", "alive1", "unknown1", "dead1"}; !reflect.DeepEqual(names, want) {
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

		got := orderBest(copies, bestRef{Subscription: "aaaaaaaa"}, e, true)

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
		got := orderBest(servers, bestRef{Subscription: "aaaaaaaa"}, orderTestEvidence(now, nil), true)
		want := []string{"unknown1@203.0.113.10", "alive1@203.0.113.11", "dead1@203.0.113.12", "alive2@203.0.113.13", "rejected1@203.0.113.14"}
		if labels := healthLabels(got); !reflect.DeepEqual(labels, want) {
			t.Fatalf("missing evidence changed order: %v, want %v", labels, want)
		}
	})
}

// ssServer is a Shadowsocks server of subscription alpha on ip: another group
// than fastServer's VLESS over TLS.
func ssServer(name, ip string) vpnconfig.Server {
	return vpnconfig.Server{
		Subscription: "alpha", Name: name, Address: ip, Port: 8388, IPs: []string{ip},
		Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"shadowsocks","settings":{"servers":[{"address":%q,"port":8388,"method":"aes-256-gcm","password":"p"}]}}`, ip)),
	}
}

func bestNames(servers []vpnconfig.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name
	}
	return out
}

func bestEvidence(latency map[string]int64, states map[string]watchdapi.Status, servers []vpnconfig.Server) monitor.Evidence {
	e := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	for _, s := range servers {
		st, ok := states[s.Name]
		if !ok {
			continue
		}
		e.Endpoints[endpoint.Key(s)] = watchdapi.EndpointState{Status: st, LatencyMS: latency[s.Name]}
	}
	return e
}

// The alive servers go first in tiers - the user's group in the user's
// subscription, the group elsewhere, other groups in the subscription, other
// groups elsewhere - each by latency; then the rest, the group first, each
// part in the order given; rejected ones not at all.
func TestOrderBest_Tiers(t *testing.T) {
	beta := func(s vpnconfig.Server) vpnconfig.Server { s.Subscription = "beta"; return s }
	aV1 := fastServer("aV1", "a1.example", "203.0.113.1")
	aSS := ssServer("aSS", "203.0.113.2")
	aV2 := fastServer("aV2", "a2.example", "203.0.113.3")
	bV1 := beta(fastServer("bV1", "b1.example", "198.51.100.1"))
	bSS := beta(ssServer("bSS", "198.51.100.2"))
	aV3 := fastServer("aV3", "a3.example", "203.0.113.4")
	bV2 := beta(fastServer("bV2", "b2.example", "198.51.100.3"))
	aV4 := fastServer("aV4", "a4.example", "203.0.113.5")
	aSS2 := ssServer("aSS2", "203.0.113.6")
	copies := []vpnconfig.Server{aV1, aSS, aV2, bV1, bSS, aV3, bV2, aV4, aSS2}
	e := bestEvidence(
		map[string]int64{"aV1": 30, "aSS": 5, "aV2": 10, "bV1": 1, "bSS": 2},
		map[string]watchdapi.Status{
			"aV1": watchdapi.StatusAlive, "aSS": watchdapi.StatusAlive, "aV2": watchdapi.StatusAlive,
			"bV1": watchdapi.StatusAlive, "bSS": watchdapi.StatusAlive, "bV2": watchdapi.StatusDead,
			"aV4": watchdapi.StatusRejected,
		}, copies)
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha", Group: "vless+tls"}, e, true))
	want := []string{"aV2", "aV1", "bV1", "aSS", "bSS", "aV3", "bV2", "aSS2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// The group outranks the subscription: an alive server of the user's group in
// another subscription goes before a faster one of another group in the
// user's own.
func TestOrderBest_GroupOutranksSubscription(t *testing.T) {
	aSS, bV1 := ssServer("aSS", "203.0.113.2"), fastServer("bV1", "b1.example", "198.51.100.1")
	bV1.Subscription = "beta"
	copies := []vpnconfig.Server{aSS, bV1}
	e := bestEvidence(map[string]int64{"aSS": 5, "bV1": 30},
		map[string]watchdapi.Status{"aSS": watchdapi.StatusAlive, "bV1": watchdapi.StatusAlive}, copies)
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha", Group: "vless+tls"}, e, true))
	if want := []string{"bV1", "aSS"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Without evidence nothing counts as alive: the group goes first and each part
// keeps the order given - the walk's OwnFirst and round robin - rather than
// every server of the user's subscription before another provider's first.
func TestOrderBest_WithoutEvidenceKeepsTheWalkOrderGroupFirst(t *testing.T) {
	beta := func(s vpnconfig.Server) vpnconfig.Server { s.Subscription = "beta"; return s }
	copies := []vpnconfig.Server{
		ssServer("aSS", "203.0.113.2"), fastServer("aV1", "a1.example", "203.0.113.1"),
		beta(fastServer("bV1", "b1.example", "198.51.100.1")), fastServer("aV2", "a2.example", "203.0.113.3"),
	}
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha", Group: "vless+tls"}, monitor.Evidence{}, false))
	if want := []string{"aV1", "bV1", "aV2", "aSS"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// With no group to lean on, groups do not order anything.
func TestOrderBest_NoReferenceGroup(t *testing.T) {
	aSS, aV1 := ssServer("aSS", "203.0.113.2"), fastServer("aV1", "a1.example", "203.0.113.1")
	copies := []vpnconfig.Server{aV1, aSS}
	e := bestEvidence(map[string]int64{"aV1": 30, "aSS": 5},
		map[string]watchdapi.Status{"aV1": watchdapi.StatusAlive, "aSS": watchdapi.StatusAlive}, copies)
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha"}, e, true))
	if want := []string{"aSS", "aV1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// The reference is the user's choice: preferred_server, else active_server;
// its group from the record, else from the entry it names.
func TestRefFor(t *testing.T) {
	oslo, shadow := fastServer("Oslo", "oslo.example", "203.0.113.10"), ssServer("Shadow", "203.0.113.20")
	servers := []vpnconfig.Server{oslo, shadow}
	cfg := &vpnconfig.VPNDirectorConfig{}
	if got := refFor(cfg, servers); got != (bestRef{}) {
		t.Fatalf("no record: %+v", got)
	}
	cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(nil, shadow)
	if got := refFor(cfg, servers); got != (bestRef{Subscription: "alpha", Group: "shadowsocks"}) {
		t.Fatalf("active: %+v", got)
	}
	cfg.Xray.PreferredServer = vpnconfig.NewActiveServer(oslo)
	if got := refFor(cfg, servers); got != (bestRef{Subscription: "alpha", Group: "vless+tls"}) {
		t.Fatalf("preferred: %+v", got)
	}
	cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}
	if got := refFor(cfg, servers); got.Group != "vless+tls" {
		t.Fatalf("a record without a group takes its entry's: %+v", got)
	}
}
