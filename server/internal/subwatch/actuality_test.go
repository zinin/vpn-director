package subwatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestClassifyActive(t *testing.T) {
	oslo := fastServer("Oslo", "oslo.example", "203.0.113.10")
	record := vpnconfig.RecordActiveServer(nil, oslo)
	moved := fastServer("Oslo", "oslo-2.example", "203.0.113.20")
	rekeyed := oslo
	rekeyed.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), "000000000010", "000000000011", 1))
	// A second inbound of the provider under the same name and address.
	ported := oslo
	ported.Port = 8443
	ported.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), `"port":443`, `"port":8443`, 1))
	bergen := fastServer("Bergen", "bergen.example", "203.0.113.30")
	alpha := func(servers ...vpnconfig.Server) []vpnconfig.Subscription {
		// The entry returned carries its subscription's id whether or not the
		// list's servers do: fastServer's, cleared here, must not supply it.
		for i := range servers {
			servers[i].Subscription = ""
		}
		return []vpnconfig.Subscription{{ID: "alpha", Name: "Alpha", URL: "https://alpha.example/list", Servers: servers}}
	}
	// The record's server as another subscription lists it: the same name is
	// another server there.
	beta := alpha(oslo)[0]
	beta.ID, beta.Name, beta.URL = "beta", "Beta", "https://beta.example/list"
	cases := []struct {
		name    string
		subs    []vpnconfig.Subscription
		a       *vpnconfig.ActiveServer
		want    actuality
		address string // of the entry returned
	}{
		{"current", alpha(bergen, oslo), record, actualCurrent, "oslo.example"},
		{"current: the name twice, one at the record's address and port", alpha(ported, oslo), record, actualCurrent, "oslo.example"},
		{"changed in place", alpha(rekeyed), record, actualChanged, "oslo.example"},
		{"moved", alpha(bergen, moved), record, actualMoved, "oslo-2.example"},
		{"gone: no entry of the name", alpha(bergen), record, actualGone, ""},
		{"gone: the name twice", alpha(moved, fastServer("Oslo", "oslo-3.example", "203.0.113.40")), record, actualGone, ""},
		{"gone: the subscription deleted", nil, record, actualGone, ""},
		{"gone: the name only in another subscription", append([]vpnconfig.Subscription{beta}, alpha(bergen)...), record, actualGone, ""},
		{"a record without a mark is checked by name, address and port", alpha(rekeyed), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}, actualCurrent, "oslo.example"},
		{"a record without address and port is checked by name", alpha(moved), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo"}, actualCurrent, "oslo-2.example"},
		{"a record without a subscription", alpha(oslo), &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}, actualUnknown, ""},
		{"a record without a name", alpha(oslo), &vpnconfig.ActiveServer{Subscription: "alpha", Address: "oslo.example", Port: 443}, actualUnknown, ""},
		{"no record", alpha(oslo), nil, actualUnknown, ""},
	}
	for _, tc := range cases {
		got, entry := classifyActive(tc.subs, tc.a)
		if got != tc.want || entry.Address != tc.address {
			t.Errorf("%s: %d with entry %q, want %d with %q", tc.name, got, entry.Address, tc.want, tc.address)
		}
		if tc.address != "" && entry.Subscription != "alpha" {
			t.Errorf("%s: entry carries subscription %q", tc.name, entry.Subscription)
		}
	}
}
