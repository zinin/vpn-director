package subwatch

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	// Another twin of the record's server: the same name, address and port
	// under a UUID of its own.
	rekeyed2 := oslo
	rekeyed2.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), "000000000010", "000000000012", 1))
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
	// A Shadowsocks inbound of the provider under the same name: another kind.
	ss := ssServer("Oslo", "198.51.100.40")
	// The same inbound at the record's own address and port.
	ssHere := ss
	ssHere.Address, ssHere.Port = "oslo.example", 443
	cases := []struct {
		name string
		subs []vpnconfig.Subscription
		a    *vpnconfig.ActiveServer
		want actuality
		// entry is the server returned, nil for none. Twins differ in their
		// outbound alone, so the whole entry is compared, and with it the
		// subscription alpha that every fixture names.
		entry *vpnconfig.Server
	}{
		{"current", alpha(bergen, oslo), record, actualCurrent, &oslo},
		{"current: the name twice, one at the record's address and port", alpha(ported, oslo), record, actualCurrent, &oslo},
		{"current: the record's mark on the second of two twins", alpha(rekeyed, oslo), record, actualCurrent, &oslo},
		{"changed in place", alpha(rekeyed), record, actualChanged, &rekeyed},
		{"changed: twins without the record's mark, the first followed", alpha(rekeyed, rekeyed2), record, actualChanged, &rekeyed},
		{"changed: the name also at another address", alpha(moved, rekeyed), record, actualChanged, &rekeyed},
		{"moved", alpha(bergen, moved), record, actualMoved, &moved},
		{"moved: another port at the address", alpha(bergen, ported), record, actualMoved, &ported},
		{"current: another kind at the record's address and port beside it", alpha(ssHere, oslo), record, actualCurrent, &oslo},
		{"moved: the one entry of the name of the record's kind", alpha(ss, moved), record, actualMoved, &moved},
		{"gone: only another kind left under the name", alpha(bergen, ss), record, actualGone, nil},
		{"gone: another kind at the record's address and port", alpha(ssHere), record, actualGone, nil},
		{"a record without a group follows an entry of any kind", alpha(bergen, ss), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}, actualMoved, &ss},
		{"gone: no entry of the name", alpha(bergen), record, actualGone, nil},
		{"gone: the name twice", alpha(moved, fastServer("Oslo", "oslo-3.example", "203.0.113.40")), record, actualGone, nil},
		{"gone: the subscription deleted", nil, record, actualGone, nil},
		{"gone: the name only in another subscription", append([]vpnconfig.Subscription{beta}, alpha(bergen)...), record, actualGone, nil},
		{"a record without a mark is checked by name, address and port", alpha(rekeyed), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}, actualCurrent, &rekeyed},
		{"a record without address and port is checked by name", alpha(moved), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo"}, actualCurrent, &moved},
		{"a record without a subscription", alpha(oslo), &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}, actualUnknown, nil},
		{"a record without a name", alpha(oslo), &vpnconfig.ActiveServer{Subscription: "alpha", Address: "oslo.example", Port: 443}, actualUnknown, nil},
		{"no record", alpha(oslo), nil, actualUnknown, nil},
	}
	show := func(s vpnconfig.Server) string {
		if s.Name == "" {
			return "no entry"
		}
		return fmt.Sprintf("%s:%d mark %s of %q", s.Address, s.Port, vpnconfig.IdentityMark(s), s.Subscription)
	}
	for _, tc := range cases {
		got, entry := classifyActive(tc.subs, tc.a)
		var want vpnconfig.Server
		if tc.entry != nil {
			want = *tc.entry
		}
		if got != tc.want || !reflect.DeepEqual(entry, want) {
			t.Errorf("%s: %d with %s, want %d with %s", tc.name, got, show(entry), tc.want, show(want))
		}
	}
}
