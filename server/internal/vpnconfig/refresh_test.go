package vpnconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMergeRefresh_WhatAPanelPicksAtRandomKeepsTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want the stored list as it is", m)
	}
}

// 3x-ui's default remark puts the traffic left into every name.
func TestMergeRefresh_TheFreshNameOnTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")})

	if len(m.Servers) != 1 || m.Servers[0].Name != "DE 9GB" || !bytes.Equal(m.Servers[0].Outbound, stored[0].Outbound) || m.Renamed != 1 {
		t.Fatalf("merge %+v", m)
	}
}

func TestMergeRefresh_Addresses(t *testing.T) {
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10", "203.0.113.11")}
	for _, tc := range []struct {
		name  string
		fresh []string
		want  []string
		moved int
	}{
		{"the same set in another order", []string{"203.0.113.11", "203.0.113.10"}, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a host that did not resolve this time", nil, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a new set", []string{"203.0.113.12"}, []string{"203.0.113.12"}, 1},
	} {
		m := MergeRefresh(stored, []Server{reality("DE", "t.example", "bb22", tc.fresh...)})
		if len(m.Servers) != 1 || !reflect.DeepEqual(m.Servers[0].IPs, tc.want) || m.Readdressed != tc.moved {
			t.Errorf("%s: merge %+v", tc.name, m)
		}
	}
}

func TestMergeRefresh_NewChangedAndGoneServers(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10"), fr}
	rotated := reality("DE", "s.example", "bb22", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))
	nl := Server{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")} // did not resolve

	m := MergeRefresh(stored, []Server{rotated, nl})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, rotated.Outbound) {
		t.Fatalf("servers %+v, want the rotated server alone", m.Servers)
	}
	if m.Added != 1 || m.Removed != 2 {
		t.Fatalf("added %d, removed %d, want 1 and 2", m.Added, m.Removed)
	}
}

func TestMergeRefresh_TwinsPairInOrder(t *testing.T) {
	stored := []Server{reality("DE-1", "a.example", "aa11", "203.0.113.10"), reality("DE-2", "a.example", "bb22", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE-1", "b.example", "cc33", "203.0.113.10"), reality("DE-2", "b.example", "dd44", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) {
		t.Fatalf("merge %+v, want each twin on its own stored copy", m.Servers)
	}
}

func TestMergeRefresh_TheFreshOrder(t *testing.T) {
	de := reality("DE", "a.example", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}

	m := MergeRefresh([]Server{de, fr}, []Server{fr, reality("DE", "b.example", "bb22", "203.0.113.10")})

	if len(m.Servers) != 2 || m.Servers[0].Name != "FR" || !bytes.Equal(m.Servers[1].Outbound, de.Outbound) {
		t.Fatalf("merge %+v, want FR, then the stored DE", m.Servers)
	}
}

// A record stored before outbounds has no identity; the first refresh puts a
// fresh copy in its place.
func TestMergeRefresh_ALegacyRecordMakesWayForAFreshCopy(t *testing.T) {
	legacy := Server{Name: "DE", Address: "de.example", Port: 443, UUID: "u-1", Security: "reality", IPs: []string{"203.0.113.10"}}
	fresh := reality("DE", "a.example", "aa11", "203.0.113.10")

	m := MergeRefresh([]Server{legacy}, []Server{fresh})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, fresh.Outbound) || m.Added != 1 || m.Removed != 1 {
		t.Fatalf("merge %+v", m)
	}
}
