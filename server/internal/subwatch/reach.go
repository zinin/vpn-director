package subwatch

import (
	"context"
	"net"
	"strings"
	"sync"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// dialIP is the IPv4 address a perAddress copy is dialed at: its one resolved
// address, or an address that is an IPv4 literal itself. "" when it has none -
// a hostname nothing resolved - and the copy then has nothing to check.
func dialIP(c vpnconfig.Server) string {
	for _, ip := range c.IPs {
		if ip != "" {
			return ipv4(ip)
		}
	}
	return ipv4(c.Address)
}

func ipv4(s string) string {
	if ip := net.ParseIP(s).To4(); ip != nil {
		return ip.String()
	}
	return ""
}

// dialable is the copies that have an IPv4 address to dial, in order.
func dialable(copies []vpnconfig.Server) []vpnconfig.Server {
	var out []vpnconfig.Server
	for _, c := range copies {
		if dialIP(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// reachable is the copies whose address accepts a TCP connection, in the order
// given. Every address is dialed at once, so one look costs a ReachTimeout at
// most however many addresses a server has.
func (w *Watch) reachable(ctx context.Context, copies []vpnconfig.Server) []vpnconfig.Server {
	ctx, cancel := context.WithTimeout(ctx, ReachTimeout)
	defer cancel()
	up := make([]bool, len(copies))
	var wg sync.WaitGroup
	for i, c := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up[i] = w.Reachable(ctx, dialIP(c), c.Port)
		}()
	}
	wg.Wait()
	var out []vpnconfig.Server
	for i, c := range copies {
		if up[i] {
			out = append(out, c)
		}
	}
	return out
}

// tcpChecked reports whether a TCP dial says anything about a server. A
// Hysteria2 outbound speaks QUIC: its port accepts no TCP connection, and a
// port that does accept one - a masquerade site beside it - says nothing about
// the proxy behind it. Three transports leave TCP as well: the Hysteria
// transport is QUIC under any protocol, mKCP runs over UDP, and xhttp over TLS
// whose alpn is exactly ["h3"] dials HTTP/3 over QUIC (decideHTTPVersion in
// Xray 26.2.6). The network and security names are read as Xray reads them,
// lowercased (TransportProtocol.Build and StreamConfig.Build), so "KCP" is
// mKCP and "TLS" is TLS. Everything else is dialed: every other protocol and
// transport an import stores runs over TCP, a legacy record without an
// outbound is VLESS over TCP, and an outbound that cannot be read gives no
// reason not to.
func tcpChecked(s vpnconfig.Server) bool {
	ob, err := vpnconfig.DecodeOutbound(s.Outbound)
	if err != nil {
		// A legacy record, which has no outbound, or one that cannot be read.
		return true
	}
	if protocol, _ := ob["protocol"].(string); protocol == "hysteria" {
		return false
	}
	ss, _ := ob["streamSettings"].(map[string]interface{})
	network, _ := ss["network"].(string)
	security, _ := ss["security"].(string)
	network, security = strings.ToLower(network), strings.ToLower(security)
	switch network {
	case "hysteria", "kcp", "mkcp":
		return false
	case "xhttp", "splithttp":
		tls, _ := ss["tlsSettings"].(map[string]interface{})
		alpn, _ := tls["alpn"].([]interface{})
		if security == "tls" && len(alpn) == 1 && alpn[0] == "h3" {
			return false
		}
	}
	return true
}

// reachControls are dialed when a look finds the active server down. A WAN that
// works reaches one of them - Cloudflare and Google answered all through the
// outages the fast rule is for - so when neither accepts, the look says nothing
// about the server: the WAN itself is down, and a tunnel over it would carry
// nothing either.
var reachControls = []vpnconfig.Server{
	{Address: "1.1.1.1", Port: 443},
	{Address: "8.8.8.8", Port: 443},
}

// activeServerDown reports whether the server active_server names accepts no
// TCP connection on any address its servers.json entry lists while the WAN
// reaches a control address. Every look without an answer is false: no record,
// no entry, no IPv4 address to dial, no control accepting either, a look a
// stop cut short, a server no TCP dial can see (tcpChecked), or a watch
// without LoadServers or Reachable. The controls are dialed only once the
// server's addresses have all failed.
func (w *Watch) activeServerDown(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if w.LoadServers == nil || w.Reachable == nil || cfg == nil || cfg.Xray.ActiveServer == nil {
		return false
	}
	servers, err := w.LoadServers()
	if err != nil {
		return false
	}
	i := chosenIndex(servers, cfg.Xray.ActiveServer)
	if i < 0 || !tcpChecked(servers[i]) {
		return false
	}
	copies := dialable(perAddress(servers[i : i+1]))
	if len(copies) == 0 {
		return false
	}
	if len(w.reachable(ctx, copies)) > 0 {
		return false
	}
	return len(w.reachable(ctx, reachControls)) > 0 && ctx.Err() == nil
}

// checkReach adds this tick's look at the active server to the streak of the
// current failSince: one more check that found it down, or the end of the
// streak - until failSince starts over - for a check that did not.
func (w *Watch) checkReach(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.downChecks < 0 {
		return
	}
	if w.activeServerDown(ctx, cfg) {
		w.downChecks++
		return
	}
	w.downChecks = -1
}
