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

// upgradePicks are the wsSettings and httpupgradeSettings keys some panels
// generate anew for every download, to mask the traffic: the Host and the
// path. Xray also takes the Host from the headers, so a Host there is the
// same pick. Unlike the REALITY picks they can route, so only looseIdentity
// drops them.
var upgradePicks = []string{"host", "path"}

// ServerIdentity is what s is apart from what a panel picks at random: its
// stored outbound without the realityPicks of any realitySettings in it, an
// xhttp downloadSettings included, as canonical JSON. Keys are matched as Xray
// matches them, folding case as strings.EqualFold does, so no spelling of a
// pick survives into the identity and the decoders' guarded keys need no new
// name. Everything else stays: address, port, protocol, credentials,
// transport - the WebSocket and HTTPUpgrade Host and path too, which tell
// apart servers one front routes by them (looseIdentity) - and the serverName
// of plain TLS, which picks the backend on a CDN. A record without an
// outbound, or with one that does not parse or is no JSON object (null, a
// string, a number, an array: DecodeOutbound refuses them too), has no
// identity: "" pairs with nothing.
func ServerIdentity(s Server) string {
	return identity(s, false)
}

// looseIdentity is ServerIdentity without the upgradePicks of any wsSettings
// or httpupgradeSettings in it either, nor the Host of their headers, and
// without those headers once nothing else is left in them, so an emptied
// headers counts as none. Some panels generate the WebSocket and HTTPUpgrade
// Host and path anew for every download, to mask the traffic, and to
// ServerIdentity every download would bring new servers. But one front often
// routes servers by the path alone - one UUID for every inbound, /de to one
// country and /nl to another - and then the loose identity of such twins is
// one: pairServers pairs by it only under the same name, so twins never trade
// outbounds. The cost: an operator who really moves such a server's Host or
// path is not taken up by the periodic refresh, and its stored copy stays
// until a manual refresh or the wave, which take fresh copies. Everything else
// stays, as in ServerIdentity: the xhttp and splithttp Host and path, gRPC, the
// TLS serverName, address, port and credentials.
func looseIdentity(s Server) string {
	return identity(s, true)
}

// identity is ServerIdentity, or looseIdentity when loose.
func identity(s Server, loose bool) string {
	if len(s.Outbound) == 0 {
		return ""
	}
	// A null leaves the map nil; a string, a number or an array does not
	// decode into it at all.
	var ob map[string]interface{}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil || ob == nil {
		return ""
	}
	dropPicks(ob, loose)
	// Marshalling a map sorts its keys: key order and whitespace do not count.
	out, err := json.Marshal(ob)
	if err != nil {
		return ""
	}
	return string(out)
}

// dropPicks removes the realityPicks from every realitySettings object in v,
// however deep, and, when loose, the upgradePicks from every wsSettings and
// httpupgradeSettings object (dropUpgradePicks).
func dropPicks(v interface{}, loose bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if obj, ok := child.(map[string]interface{}); ok {
				switch {
				case strings.EqualFold(k, "realitySettings"):
					dropKeys(obj, realityPicks)
				case loose && (strings.EqualFold(k, "wsSettings") || strings.EqualFold(k, "httpupgradeSettings")):
					dropUpgradePicks(obj)
				}
			}
			dropPicks(child, loose)
		}
	case []interface{}:
		for _, child := range t {
			dropPicks(child, loose)
		}
	}
}

// dropUpgradePicks removes the upgradePicks from settings, a wsSettings or an
// httpupgradeSettings object, and the Host from its headers, and then the
// headers when they are empty: a panel that sends the Host there on one
// download and leaves the headers out on the next lists one server.
func dropUpgradePicks(settings map[string]interface{}) {
	dropKeys(settings, upgradePicks)
	for k, v := range settings {
		if headers, ok := v.(map[string]interface{}); ok && strings.EqualFold(k, "headers") {
			dropKeys(headers, []string{"Host"})
			if len(headers) == 0 {
				delete(settings, k)
			}
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

// pairServers pairs each fresh server with a stored one not yet paired:
// pairs[i] is the index in stored of fresh[i]'s pair, or -1. First each fresh
// server, in order, pairs with the first stored server of the same
// ServerIdentity; then each one still without a pair, in order, with the
// first stored server of the same looseIdentity and the same name. The strict
// pass goes first, so a server that kept its Host and path takes its own copy
// before a twin of its name whose Host and path were generated anew can. A
// server without an identity pairs with nothing.
func pairServers(stored, fresh []Server) []int {
	pairs := make([]int, len(fresh))
	paired := make([]bool, len(stored))
	strict := map[string][]int{}
	for i, s := range stored {
		if id := ServerIdentity(s); id != "" {
			strict[id] = append(strict[id], i)
		}
	}
	for i, f := range fresh {
		pairs[i] = -1
		id := ServerIdentity(f)
		if q := strict[id]; id != "" && len(q) > 0 {
			pairs[i], strict[id] = q[0], q[1:]
			paired[q[0]] = true
		}
	}
	type named struct{ name, id string }
	loose := map[named][]int{}
	for i, s := range stored {
		if id := looseIdentity(s); id != "" && !paired[i] {
			k := named{s.Name, id}
			loose[k] = append(loose[k], i)
		}
	}
	for i, f := range fresh {
		if pairs[i] >= 0 {
			continue
		}
		k := named{f.Name, looseIdentity(f)}
		if q := loose[k]; k.id != "" && len(q) > 0 {
			pairs[i], loose[k] = q[0], q[1:]
		}
	}
	return pairs
}
