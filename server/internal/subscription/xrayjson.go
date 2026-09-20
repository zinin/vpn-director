package subscription

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// helperProtocols are the outbounds an Xray config carries beside its proxy.
var helperProtocols = map[string]bool{"freedom": true, "blackhole": true, "dns": true, "loopback": true}

// proxyProtocols are the ones a server may run on.
var proxyProtocols = map[string]bool{"vless": true, "vmess": true, "trojan": true, "shadowsocks": true, "hysteria": true}

// decodeXrayJSON reads an array of Xray client configs, or one config (spec
// 6.5).
func decodeXrayJSON(body string) (Result, error) {
	doc, err := decodeJSON(body)
	if err != nil {
		return Result{}, ErrInvalidJSON
	}
	var configs []interface{}
	switch t := doc.(type) {
	case []interface{}:
		configs = t
	case map[string]interface{}:
		if _, ok := t["outbounds"].([]interface{}); ok {
			configs = []interface{}{t}
		}
	}
	if len(configs) == 0 {
		return Result{}, ErrUnrecognized
	}
	r := Result{Total: len(configs)}
	for i, c := range configs {
		e, err := xrayEntry(c)
		r.add(i+1, e, err)
	}
	return r, nil
}

// xrayEntry takes the config's single proxy outbound, checks it and removes
// what could reach past the proxy into the router's own routing.
func xrayEntry(raw interface{}) (entry, error) {
	cfg, ok := raw.(map[string]interface{})
	if !ok {
		return entry{}, invalid("not an Xray config")
	}
	var e entry
	if remarks, ok := cfg["remarks"].(string); ok {
		e.name = cleanName(remarks)
	}
	outbounds, ok := cfg["outbounds"].([]interface{})
	if !ok {
		return e, invalid("not an Xray config")
	}
	var proxies []map[string]interface{}
	for _, o := range outbounds {
		ob, _ := o.(map[string]interface{})
		if protocol, ok := ob["protocol"].(string); ok && !helperProtocols[protocol] {
			proxies = append(proxies, ob)
		}
	}
	switch len(proxies) {
	case 0:
		return e, unsupported("no proxy outbound")
	case 1:
	default:
		return e, composite(strconv.Itoa(len(proxies)) + " proxy outbounds")
	}
	ob := proxies[0]
	protocol := ob["protocol"].(string)
	if !proxyProtocols[protocol] {
		return e, unsupported("protocol " + protocol)
	}
	proxySettings, _ := ob["proxySettings"].(map[string]interface{})
	stream, _ := ob["streamSettings"].(map[string]interface{})
	sockopt, _ := stream["sockopt"].(map[string]interface{})
	if tag, _ := proxySettings["tag"].(string); tag != "" {
		return e, composite("chained")
	}
	if dialer, _ := sockopt["dialerProxy"].(string); dialer != "" {
		return e, composite("chained")
	}
	settings, _ := ob["settings"].(map[string]interface{})
	for _, key := range []string{"vnext", "servers"} {
		if list, ok := settings[key].([]interface{}); ok && len(list) > 1 {
			return e, composite(strconv.Itoa(len(list)) + " targets")
		}
	}
	target := vpnconfig.OutboundTarget(ob)
	address, _ := target["address"].(string)
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	number, _ := target["port"].(json.Number)
	port, ok := jsonPort(number)
	if address == "" || !ok {
		return e, invalid("bad address or port")
	}
	switch security, _ := stream["security"].(string); security {
	case "tls":
		tls, _ := stream["tlsSettings"].(map[string]interface{})
		if insecure, _ := tls["allowInsecure"].(bool); insecure {
			if pin, _ := tls["pinnedPeerCertSha256"].(string); pin == "" {
				return e, unsupported("insecure TLS")
			}
			delete(tls, "allowInsecure")
		}
	case "reality":
		reality, _ := stream["realitySettings"].(map[string]interface{})
		key, _ := reality["publicKey"].(string)
		if key == "" {
			key, _ = reality["password"].(string)
		}
		serverName, _ := reality["serverName"].(string)
		fingerprint, _ := reality["fingerprint"].(string)
		if key == "" || serverName == "" || fingerprint == "" {
			return e, invalid("reality needs publicKey, serverName and fingerprint")
		}
	}
	// Routing is VPN Director's own: an entry keeps neither an outbound tag
	// nor a source address, and no sockopt of its own (scrubSockopt says why).
	delete(ob, "tag")
	delete(ob, "sendThrough")
	scrubSockopt(ob)
	e.address, e.port, e.outbound = address, port, ob
	return e, nil
}
