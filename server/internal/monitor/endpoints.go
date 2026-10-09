package monitor

import (
	"encoding/json"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Endpoint is one address of one server as the prober dials it.
type Endpoint struct {
	Key string
	// Label names the first server that dials it, "<subscription> / <server>",
	// for the log.
	Label string
	// Outbound is what Generate would write for it; the prober tags it.
	Outbound json.RawMessage
}

// Build lists the endpoints of subs: each server once per address
// (endpoint.PerAddress), the address in its outbound (endpoint.ServerForDial),
// one endpoint per key. The endpoints of the active server come first. A
// server whose outbound the generator refuses is no endpoint: refused maps its
// key to the reason.
func Build(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer, outbound func(vpnconfig.Server) (json.RawMessage, error)) (eps []Endpoint, refused map[string]string) {
	refused = map[string]string{}
	seen := map[string]bool{}
	// An active key may first appear under an earlier alias.
	activeKeys := map[string]bool{}
	if active != nil {
		for _, sub := range subs {
			if active.Subscription != sub.ID {
				continue
			}
			for _, s := range sub.Servers {
				if active.Name != s.Name || active.Address != s.Address || active.Port != s.Port {
					continue
				}
				s.Subscription = sub.ID
				for _, key := range endpoint.Keys(s) {
					activeKeys[key] = true
				}
			}
		}
	}
	var first, rest []Endpoint
	for _, sub := range subs {
		for _, s := range sub.Servers {
			s.Subscription = sub.ID
			for _, c := range endpoint.PerAddress([]vpnconfig.Server{s}) {
				key := endpoint.Key(c)
				if seen[key] {
					continue
				}
				seen[key] = true
				ob, err := outbound(endpoint.ServerForDial(c))
				if err != nil {
					refused[key] = err.Error()
					continue
				}
				ep := Endpoint{Key: key, Label: sub.Name + " / " + s.Name, Outbound: ob}
				if activeKeys[key] {
					first = append(first, ep)
				} else {
					rest = append(rest, ep)
				}
			}
		}
	}
	return append(first, rest...), refused
}
