// Package endpoint names what the subscription watch's walk and the server
// monitor dial: one address of one server, with that address in its outbound.
// The walk, the monitor, the Web UI and the bot all derive endpoints here, so
// a status the monitor reports is a status of exactly what the walk would try.
package endpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// WANControls are dialed when every server looks dead at once: a WAN that
// works reaches one of them, so none accepting means the WAN is down, not the
// servers.
var WANControls = []vpnconfig.Server{
	{Address: "1.1.1.1", Port: 443},
	{Address: "8.8.8.8", Port: 443},
}

// PerAddress lists each server once for every address it resolved to, each copy
// with that address alone, so the walk dials them one after another; a server
// with none is listed as it is. An endpoint ban takes an address, not the name:
// a host can resolve to one the router cannot reach and another it can, and
// dialing only the first rejected the whole server.
func PerAddress(servers []vpnconfig.Server) []vpnconfig.Server {
	out := make([]vpnconfig.Server, 0, len(servers))
	for _, s := range servers {
		n := len(out)
		for _, ip := range s.IPs {
			if ip == "" {
				continue
			}
			c := s
			c.IPs = []string{ip}
			out = append(out, c)
		}
		if len(out) == n {
			out = append(out, s)
		}
	}
	return out
}

// ServerForDial uses a tunnel-resolved IPv4 for vnext so Xray does not go
// back to the system resolver. A TLS server name keeps the hostname. A REALITY
// one is the site the handshake borrows, never the proxy's own host, so an
// entry without one stays without one and is refused as the Web UI refuses it.
// Web UI /xray keep s.Address and let Xray resolve, so a CDN IP change still
// works there.
//
// A server whose import stored its outbound gets the IP in the outbound's own
// address slot (vpnconfig.OutboundTarget). Where the source left the name to
// the address, dialing an IP would change it, so the hostname goes there
// instead: an empty tlsSettings.serverName, and for a stream without security
// an empty Host of ws or httpupgrade, an empty Host of the xhttpSettings or
// splithttpSettings the record has - Xray reads the former over the latter
// and drops the other - or an empty grpcSettings.authority, which a
// cleartext gRPC stream otherwise takes from the address. With TLS, Xray
// takes that Host, and gRPC's authority, from the server name.
//
// The download host of an xhttp extra (downloadSettings.address) keeps its
// name: the record's IPs are the main address's, and Xray resolves that host
// itself through the system resolver. So with the WAN resolver silent, a
// server whose download host is another name is judged dead although its main
// address resolved; looking that host up over the tunnel is a separate task.
func ServerForDial(s vpnconfig.Server) vpnconfig.Server {
	ip := ""
	for _, v := range s.IPs {
		if v != "" {
			ip = v
			break
		}
	}
	if ip == "" {
		return s
	}
	host := s.Address
	if len(s.Outbound) == 0 {
		s.Address = ip
		if s.SNI == "" && s.Security != "reality" {
			s.SNI = host
		}
		return s
	}
	ob, err := vpnconfig.DecodeOutbound(s.Outbound)
	if err != nil {
		return s
	}
	target := vpnconfig.OutboundTarget(ob)
	if target == nil {
		return s
	}
	target["address"] = ip
	if net.ParseIP(host) == nil {
		keepHostname(ob, host)
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return s
	}
	s.Outbound = raw
	s.Address = ip
	return s
}

// keepHostname writes host where the stream would otherwise take the name
// from an address that is now an IP. The xhttp Host goes into the
// xhttpSettings or splithttpSettings the record has: Xray reads xhttpSettings
// over splithttpSettings and drops the other, so a new xhttpSettings beside a
// splithttpSettings would dial without its path, mode and extra. A cleartext
// gRPC stream takes its :authority from the address when
// grpcSettings.authority is empty, so the hostname goes there.
func keepHostname(ob map[string]interface{}, host string) {
	ss, _ := ob["streamSettings"].(map[string]interface{})
	if ss == nil {
		return
	}
	switch security, _ := ss["security"].(string); security {
	case "tls":
		tls, _ := ss["tlsSettings"].(map[string]interface{})
		if tls == nil {
			tls = map[string]interface{}{}
			ss["tlsSettings"] = tls
		}
		if name, _ := tls["serverName"].(string); name == "" {
			tls["serverName"] = host
		}
	case "", "none":
		key := ""
		switch ss["network"] {
		case "ws", "websocket":
			key = "wsSettings"
		case "httpupgrade":
			key = "httpupgradeSettings"
		case "xhttp", "splithttp":
			key = "xhttpSettings"
			if _, ok := ss[key].(map[string]interface{}); !ok {
				if _, ok := ss["splithttpSettings"].(map[string]interface{}); ok {
					key = "splithttpSettings"
				}
			}
		case "grpc":
			grpc, _ := ss["grpcSettings"].(map[string]interface{})
			if grpc == nil {
				grpc = map[string]interface{}{}
				ss["grpcSettings"] = grpc
			}
			if authority, _ := grpc["authority"].(string); authority == "" {
				grpc["authority"] = host
			}
			return
		}
		if key == "" {
			return
		}
		transport, _ := ss[key].(map[string]interface{})
		if transport == nil {
			transport = map[string]interface{}{}
			ss[key] = transport
		}
		headers, _ := transport["headers"].(map[string]interface{})
		if h, _ := transport["host"].(string); h != "" {
			return
		}
		// Xray's ws builder takes a host header in any case.
		for key, value := range headers {
			if h, _ := value.(string); strings.EqualFold(key, "host") && h != "" {
				return
			}
		}
		transport["host"] = host
	}
}

// DialKey is what a walk's copy of a server dials: its outbound with the
// address in place, as Generate writes it (ServerForDial). Copies with one key
// are one server to Xray whatever their names - one provider lists 62 names on
// 9 endpoints - and a walk tries each key once. A record without an outbound
// has no key and is never taken for another.
func DialKey(c vpnconfig.Server) string {
	if len(c.Outbound) == 0 {
		return ""
	}
	return string(ServerForDial(c).Outbound)
}

// legacyDial is what a record from before outbounds were stored dials: the
// flat fields the generator builds its VLESS outbound from.
type legacyDial struct {
	Address     string   `json:"address"`
	Port        int      `json:"port"`
	UUID        string   `json:"uuid"`
	Security    string   `json:"security"`
	Network     string   `json:"network"`
	Flow        string   `json:"flow"`
	SNI         string   `json:"sni"`
	Fingerprint string   `json:"fingerprint"`
	PublicKey   string   `json:"public_key"`
	ShortID     string   `json:"short_id"`
	ALPN        []string `json:"alpn"`
}

// Key names the copy c (one address of one server, as PerAddress lists it) in
// the monitor's results: the hex SHA-256 of what it dials. Copies with one
// DialKey share a key; a record without an outbound hashes the flat fields
// the generator builds its outbound from. A key carries no credential, which
// a DialKey does.
func Key(c vpnconfig.Server) string {
	material := DialKey(c)
	if material == "" {
		d := ServerForDial(c)
		raw, _ := json.Marshal(legacyDial{
			Address: d.Address, Port: d.Port, UUID: d.UUID, Security: d.Security,
			Network: d.Network, Flow: d.Flow, SNI: d.SNI, Fingerprint: d.Fingerprint,
			PublicKey: d.PublicKey, ShortID: d.ShortID, ALPN: d.ALPN,
		})
		material = "legacy:" + string(raw)
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// Keys is the key of every endpoint of s, one per address, in PerAddress
// order.
func Keys(s vpnconfig.Server) []string {
	copies := PerAddress([]vpnconfig.Server{s})
	keys := make([]string, len(copies))
	for i, c := range copies {
		keys[i] = Key(c)
	}
	return keys
}
