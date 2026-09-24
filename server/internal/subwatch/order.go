package subwatch

import "github.com/zinin/vpn-director/server/internal/vpnconfig"

// OwnFirst is how many servers of the chosen server's subscription a walk
// tries, the chosen one included, before it takes one server of each
// subscription in turn. A server that fails alone usually has a neighbour that
// works; a provider that fails as a whole - expired, blocked - costs no more
// than these few tries before another provider's first server.
const OwnFirst = 3

// walkOrder is the order a walk tries the servers of subs in (spec 5.3): the
// chosen server - found by its subscription, name, address and port, or by
// subscription and name - then the next servers of its subscription in list
// order, OwnFirst of them in all; then one server of each subscription in
// turn, in subscription order, starting with the one after the chosen
// server's; the chosen subscription's remaining servers take their turns too.
// Without the chosen server's subscription the turns start at the first. With
// one subscription this is pickOrder. chosenFirst reports that the chosen
// server leads the order. Every server carries its subscription's id.
func walkOrder(subs []vpnconfig.Subscription, chosen *vpnconfig.ActiveServer) (order []vpnconfig.Server, chosenFirst bool) {
	if len(subs) == 0 {
		return nil, false
	}
	queues := make([][]vpnconfig.Server, len(subs))
	own := -1
	for i, sub := range subs {
		q := make([]vpnconfig.Server, len(sub.Servers))
		for j, s := range sub.Servers {
			s.Subscription = sub.ID
			q[j] = s
		}
		queues[i] = q
		if own < 0 && chosen != nil && sub.ID == chosen.Subscription {
			own = i
		}
	}
	start := 0
	if own >= 0 {
		chosenFirst = chosenIndex(queues[own], chosen) >= 0
		q := pickOrder(queues[own], chosen)
		n := min(OwnFirst, len(q))
		order = append(order, q[:n]...)
		queues[own] = q[n:]
		start = (own + 1) % len(subs)
	}
	for placed := true; placed; {
		placed = false
		for k := range subs {
			i := (start + k) % len(subs)
			if len(queues[i]) == 0 {
				continue
			}
			order = append(order, queues[i][0])
			queues[i] = queues[i][1:]
			placed = true
		}
	}
	return order, chosenFirst
}

// dialKey is what a walk's copy of a server dials: its outbound with the
// address in place, as Generate writes it (ServerForDial). Copies with one key
// are one server to Xray whatever their names - one provider lists 62 names on
// 9 endpoints - and a walk tries each key once. A record without an outbound
// has no key and is never taken for another.
func dialKey(c vpnconfig.Server) string {
	if len(c.Outbound) == 0 {
		return ""
	}
	return string(ServerForDial(c).Outbound)
}
