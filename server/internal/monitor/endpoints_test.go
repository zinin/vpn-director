package monitor

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func trojan(address string) json.RawMessage {
	return json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + address + `","port":443,"password":"p"}]}}`)
}

// outboundAsIs stands in for service.OutboundJSON: the stored outbound, and a
// refusal for a server named "Refused".
func outboundAsIs(s vpnconfig.Server) (json.RawMessage, error) {
	if s.Name == "Refused" {
		return nil, errors.New("stored outbound: xhttp downloadSettings without an address")
	}
	return s.Outbound, nil
}

// One provider lists many names on few endpoints: the monitor checks each
// endpoint once, whatever names and subscriptions share it, and every address
// of a server apart.
func TestBuild_OneEndpointPerKey(t *testing.T) {
	de := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1", "192.0.2.2"}, Outbound: trojan("de.example")}
	twin := de
	twin.Name = "Germany-2"
	subs := []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{de, twin}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{de}},
	}

	eps, refused := Build(subs, nil, outboundAsIs)

	if len(eps) != 2 || len(refused) != 0 {
		t.Fatalf("endpoints %+v, refused %v", eps, refused)
	}
	keys := endpoint.Keys(de)
	if eps[0].Key != keys[0] || eps[1].Key != keys[1] || eps[0].Label != "Alpha / Germany-1" {
		t.Fatalf("endpoints %+v, want the keys %v", eps, keys)
	}
	var ob map[string]interface{}
	if err := json.Unmarshal(eps[1].Outbound, &ob); err != nil {
		t.Fatal(err)
	}
	servers := ob["settings"].(map[string]interface{})["servers"].([]interface{})
	if addr := servers[0].(map[string]interface{})["address"]; addr != "192.0.2.2" {
		t.Fatalf("second endpoint dials %v, want its own address", addr)
	}
}

// At a rebuild the running server's endpoints are checked first.
func TestBuild_TheActiveServerComesFirst(t *testing.T) {
	a := vpnconfig.Server{Name: "Oslo", Address: "a.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: trojan("a.example")}
	b := vpnconfig.Server{Name: "Riga", Address: "b.example", Port: 443, IPs: []string{"192.0.2.2"}, Outbound: trojan("b.example")}
	subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{a, b}}}
	active := &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Riga", Address: "b.example", Port: 443}

	eps, _ := Build(subs, active, outboundAsIs)

	if len(eps) != 2 || eps[0].Label != "Alpha / Riga" {
		t.Fatalf("endpoints %+v", eps)
	}
}

func TestBuild_ARefusedOutboundIsNoEndpoint(t *testing.T) {
	bad := vpnconfig.Server{Name: "Refused", Address: "x.example", Port: 443, IPs: []string{"192.0.2.9"}, Outbound: trojan("x.example")}
	subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{bad}}}

	eps, refused := Build(subs, nil, outboundAsIs)

	key := endpoint.Keys(bad)[0]
	if len(eps) != 0 || refused[key] != "stored outbound: xhttp downloadSettings without an address" {
		t.Fatalf("endpoints %+v, refused %v", eps, refused)
	}
}

func TestBuild_ActiveKeysComeFirstWhenAnEarlierTwinOwnsTheLabel(t *testing.T) {
	for _, tc := range []struct {
		name              string
		crossSubscription bool
	}{
		{name: "same subscription"},
		{name: "cross subscription", crossSubscription: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := vpnconfig.Server{Name: "Oslo", Address: "other.example", Port: 443,
				IPs: []string{"192.0.2.1"}, Outbound: trojan("other.example")}
			twin := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443,
				IPs: []string{"192.0.2.2", "192.0.2.3"}, Outbound: trojan("de.example")}
			activeTwin := twin
			activeTwin.Name = "Germany-2"
			subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{other, twin}}}
			activeSub := subs[0].ID
			if tc.crossSubscription {
				subs = append(subs, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{activeTwin}})
				activeSub = subs[1].ID
			} else {
				subs[0].Servers = append(subs[0].Servers, activeTwin)
			}
			active := &vpnconfig.ActiveServer{Subscription: activeSub, Name: activeTwin.Name,
				Address: activeTwin.Address, Port: activeTwin.Port}
			calls := 0
			eps, refused := Build(subs, active, func(s vpnconfig.Server) (json.RawMessage, error) {
				calls++
				return outboundAsIs(s)
			})

			if len(eps) != 3 || len(refused) != 0 || calls != 3 {
				t.Fatalf("endpoints %d, refusals %d, outbound calls %d; want 3, 0, 3", len(eps), len(refused), calls)
			}
			keys := append(endpoint.Keys(twin), endpoint.Keys(other)...)
			labels := []string{"Alpha / Germany-1", "Alpha / Germany-1", "Alpha / Oslo"}
			for i, ep := range eps {
				if ep.Key != keys[i] || ep.Label != labels[i] {
					t.Fatalf("endpoint %d: key %q, label %q; want key %q, label %q", i, ep.Key, ep.Label, keys[i], labels[i])
				}
			}
		})
	}
}
