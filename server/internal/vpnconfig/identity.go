package vpnconfig

import (
	"encoding/json"
	"strings"
)

// randomPicks are the keys a panel may fill at random on every download, by
// the settings object that holds them, and those of its headers object. Any
// value of them reaches the same server, so none tells one server from
// another.
var randomPicks = []struct {
	settings string
	keys     []string
	headers  []string
}{
	// 3x-ui and Marzban pick the server name from serverNames and the short
	// id from shortIds anew for each download, and spiderX is the client's
	// own path on the borrowed site. Any value the server lists works.
	{"realitySettings", []string{"serverName", "shortId", "spiderX"}, nil},
	// Some panels generate the WebSocket and HTTPUpgrade Host and path anew
	// for every download, to mask the traffic. Xray also takes the Host from
	// the headers, so a Host there is the same pick.
	{"wsSettings", []string{"host", "path"}, []string{"Host"}},
	{"httpupgradeSettings", []string{"host", "path"}, []string{"Host"}},
}

// ServerIdentity is what s is apart from what a panel picks at random: its
// stored outbound without the randomPicks of any settings object in it - the
// REALITY picks, and the WebSocket and HTTPUpgrade Host and path - an xhttp
// downloadSettings included, as canonical JSON. Keys are matched as Xray
// matches them, folding case as strings.EqualFold does, so no spelling of a
// pick survives into the identity and the decoders' guarded keys need no new
// name. Everything else stays: address, port, protocol, credentials, the rest
// of the transport - the xhttp and splithttp Host and path, the gRPC service
// name and authority - and the serverName of plain TLS, which picks the
// backend on a CDN. The cost: a pairing keeps the stored copy, so an operator
// who really moves a ws or httpupgrade server's Host or path is not taken up
// by the periodic refresh; its stored copy stays until a manual refresh or the
// wave, which take fresh copies, as a REALITY pick the admin removes does. A
// record without an outbound, or with one that does not parse or is no JSON
// object (null, a string, a number, an array: DecodeOutbound refuses them
// too), has no identity: "" pairs with nothing.
func ServerIdentity(s Server) string {
	if len(s.Outbound) == 0 {
		return ""
	}
	// A null leaves the map nil; a string, a number or an array does not
	// decode into it at all.
	var ob map[string]interface{}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil || ob == nil {
		return ""
	}
	dropRandomPicks(ob)
	// Marshalling a map sorts its keys: key order and whitespace do not count.
	out, err := json.Marshal(ob)
	if err != nil {
		return ""
	}
	return string(out)
}

// dropRandomPicks removes the randomPicks from every settings object in v
// that holds them, and from its headers object, however deep.
func dropRandomPicks(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if obj, ok := child.(map[string]interface{}); ok {
				for _, p := range randomPicks {
					if !strings.EqualFold(k, p.settings) {
						continue
					}
					dropKeys(obj, p.keys)
					for hk, h := range obj {
						if headers, ok := h.(map[string]interface{}); ok && strings.EqualFold(hk, "headers") {
							dropKeys(headers, p.headers)
						}
					}
				}
			}
			dropRandomPicks(child)
		}
	case []interface{}:
		for _, child := range t {
			dropRandomPicks(child)
		}
	}
}

// dropKeys removes from obj every key that folds to one of names.
func dropKeys(obj map[string]interface{}, names []string) {
	for k := range obj {
		for _, name := range names {
			if strings.EqualFold(k, name) {
				delete(obj, k)
				break
			}
		}
	}
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
