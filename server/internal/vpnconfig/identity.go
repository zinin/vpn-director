package vpnconfig

import (
	"encoding/json"
	"strings"
)

// realityPicks are the realitySettings keys a panel may fill at random on
// every request. 3x-ui and Marzban pick the server name from serverNames and
// the short id from shortIds anew for each download, and spiderX is the
// client's own path on the borrowed site. Any value the server lists works,
// so none of them tells one server from another.
var realityPicks = []string{"serverName", "shortId", "spiderX"}

// ServerIdentity is what s is apart from what a panel picks at random: its
// stored outbound without the realityPicks of any realitySettings in it, an
// xhttp downloadSettings included, as canonical JSON. Keys are matched as Xray
// matches them, folding case as strings.EqualFold does, so no spelling of a
// pick survives into the identity and the decoders' guarded keys need no new
// name. Everything else stays: address, port, protocol, credentials,
// transport, and the serverName of plain TLS, which picks the backend on a
// CDN. A record without an outbound, or with one that does not parse, has no
// identity: "" pairs with nothing.
func ServerIdentity(s Server) string {
	if len(s.Outbound) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(s.Outbound, &v); err != nil {
		return ""
	}
	dropRealityPicks(v)
	// Marshalling a map sorts its keys: key order and whitespace do not count.
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// dropRealityPicks removes the realityPicks from every realitySettings object
// in v, however deep.
func dropRealityPicks(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if strings.EqualFold(k, "realitySettings") {
				if rs, ok := child.(map[string]interface{}); ok {
					for rk := range rs {
						if isRealityPick(rk) {
							delete(rs, rk)
						}
					}
				}
			}
			dropRealityPicks(child)
		}
	case []interface{}:
		for _, child := range t {
			dropRealityPicks(child)
		}
	}
}

func isRealityPick(k string) bool {
	for _, name := range realityPicks {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// pairServers pairs each fresh server, in order, with the first stored server
// of the same ServerIdentity not yet paired: pairs[i] is the index in stored
// of fresh[i]'s pair, or -1. A server without an identity pairs with nothing.
func pairServers(stored, fresh []Server) []int {
	free := map[string][]int{}
	for i, s := range stored {
		if id := ServerIdentity(s); id != "" {
			free[id] = append(free[id], i)
		}
	}
	pairs := make([]int, len(fresh))
	for i, f := range fresh {
		pairs[i] = -1
		id := ServerIdentity(f)
		if q := free[id]; id != "" && len(q) > 0 {
			pairs[i], free[id] = q[0], q[1:]
		}
	}
	return pairs
}
