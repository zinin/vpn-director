package subwatch

import (
	"sort"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// bestRef is what the choice of a replacement leans on (spec 4.5): the
// subscription and the group of the user's choice.
type bestRef struct {
	Subscription string
	Group        string
}

// refFor is the reference for cfg: preferred_server when one is kept - a
// choice that left its list included, whose group is still the one the user
// wants - and active_server otherwise. The group comes from the record, or
// from the entry it names in servers when the record has none.
func refFor(cfg *vpnconfig.VPNDirectorConfig, servers []vpnconfig.Server) bestRef {
	if cfg == nil {
		return bestRef{}
	}
	a := cfg.Xray.PreferredServer
	if a == nil {
		a = cfg.Xray.ActiveServer
	}
	if a == nil {
		return bestRef{}
	}
	ref := bestRef{Subscription: a.Subscription, Group: a.Group}
	if ref.Group == "" {
		if i := chosenIndex(servers, a); i >= 0 {
			ref.Group = servers[i].Group()
		}
	}
	return ref
}

// orderBest orders copies - endpoint.PerAddress copies in the walk's order -
// for a switch. With ok, the copies the monitor holds alive come first, in
// tiers: the reference group in the reference subscription, the group
// elsewhere, other groups in the subscription, other groups elsewhere; each
// tier by latency, ties in the order given. Every other copy follows, the
// reference group first, each part in the order given: a provider that fails
// as a whole then costs the walk's OwnFirst tries, not one per server. Copies
// the monitor rejected are left out. Without ok nothing counts as alive. With
// no reference group, groups order nothing.
func orderBest(copies []vpnconfig.Server, ref bestRef, ev monitor.Evidence, ok bool) []vpnconfig.Server {
	type ranked struct {
		s       vpnconfig.Server
		tier    int
		latency int64
	}
	out := make([]ranked, 0, len(copies))
	for _, c := range copies {
		st, known := ev.Endpoints[endpoint.Key(c)]
		if ok && known && st.Status == watchdapi.StatusRejected {
			continue
		}
		group := ref.Group == "" || c.Group() == ref.Group
		own := c.Subscription == ref.Subscription
		alive := ok && known && st.Status == watchdapi.StatusAlive
		r := ranked{s: c}
		switch {
		case alive && group && own:
			r.tier = 0
		case alive && group:
			r.tier = 1
		case alive && own:
			r.tier = 2
		case alive:
			r.tier = 3
		case group:
			r.tier = 4
		default:
			r.tier = 5
		}
		if alive {
			r.latency = st.LatencyMS
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		return out[i].tier < 4 && out[i].latency < out[j].latency
	})
	servers := make([]vpnconfig.Server, len(out))
	for i, r := range out {
		servers[i] = r.s
	}
	return servers
}

// walkEvidence is the monitor's evidence for ordering servers, and whether it
// may: monitoring on, its state OK and every key of servers it knows still
// valid. Missing keys stay unknown; an empty validation request would mean the
// whole monitor set. waiting is a monitor that is on and OK with nothing valid
// for servers yet - a new endpoint set or prober whose first checks have not
// come in, or a check that completed while the evidence was read - which a
// look that needs the monitor waits for rather than look without it.
func (w *Watch) walkEvidence(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) (ev monitor.Evidence, fresh, waiting bool) {
	if len(servers) == 0 || w.Health == nil ||
		(cfg != nil && cfg.Monitor != nil && cfg.Monitor.Enabled != nil && !*cfg.Monitor.Enabled) {
		return monitor.Evidence{}, false, false
	}
	evidence := w.Health.Evidence()
	if evidence.State != watchdapi.StateOK {
		return monitor.Evidence{}, false, false
	}
	keys := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		key := endpoint.Key(s)
		if _, ok := evidence.Endpoints[key]; ok && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	if len(keys) == 0 || w.Health.ValidateEvidence(evidence, keys) != nil {
		return monitor.Evidence{}, false, true
	}
	return evidence, true, false
}

// bestWalkOrder is orderBest on the monitor's evidence for servers, when it may
// order them.
func (w *Watch) bestWalkOrder(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig, ref bestRef) []vpnconfig.Server {
	ev, ok, _ := w.walkEvidence(servers, cfg)
	return orderBest(servers, ref, ev, ok)
}
