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
// what could reach past the proxy into the router's own routing. Settings that
// hold a non-null address beside a non-empty list under the key the protocol
// uses (vnext for vless and vmess, servers for trojan and shadowsocks) are
// invalid: Xray dials the flat address whenever one is set - all four builders
// replace the list with it (26.2.6) - while OutboundTarget reads the list, so
// the stored address, its IPs and the watch's TCP checks would describe a host
// Xray does not dial. _sub_xray_json in lib/subscription.sh skips it alike. So
// is a target port whose literal is not plain decimal digits: every outbound
// port field is a uint16, which encoding/json does not fill from 443.0, 1e2 or
// 4.43e2, and the entry keeps its literal as written, so it would import and
// then fail "xray run -test" whenever it was selected. jsonPort reads the
// value only; the v2rayN vmess path, which builds its outbound from that
// integer, keeps taking 443.0. The shell sees the number as jq prints it, so a
// literal jq normalizes (4.43e2) is stored there with the normalized port.
// encoding/json refuses nan, NaN, Infinity, .5, 1., +1 and 0443 as well
// ("invalid JSON subscription", or an invalid entry inside an xhttp extra or a
// vmess object), which jq reads as numbers, so the shell imports such a body;
// it is documented and left, as the port literals are.
// Xray lowercases a protocol, a network and a security before it reads them
// (LoadWithID, TransportProtocol.Build and StreamConfig.Build in 26.2.6), so
// every outbound's protocol, and every network and security of the proxy
// ("headers" excepted, lowerStreamNames), is lowercased before any check reads
// it and stored so: "TLS" is a tls stream to the sanitizer, the label,
// tcpChecked and keepHostname alike. A share link that spells its type or
// security otherwise is unsupported; the extra an xhttp link carries is
// Xray's JSON, and streamSettings lowers it the same way.
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
		if protocol, ok := ob["protocol"].(string); ok {
			// Xray lowercases the protocol: "Freedom" is a helper to it.
			protocol = asciiLower(protocol)
			ob["protocol"] = protocol
			if !helperProtocols[protocol] {
				proxies = append(proxies, ob)
			}
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
	if k, c := keyCase(ob); k != "" {
		return e, invalid(`key "` + k + `" is spelled "` + c + `"`)
	}
	lowerStreamNames(ob)
	protocol := ob["protocol"].(string)
	if !proxyProtocols[protocol] {
		return e, unsupported("protocol " + protocol)
	}
	proxySettings, _ := ob["proxySettings"].(map[string]interface{})
	stream, _ := ob["streamSettings"].(map[string]interface{})
	if tag, _ := proxySettings["tag"].(string); tag != "" {
		return e, composite("chained")
	}
	if hasDialerProxy(ob) {
		return e, composite("chained")
	}
	settings, _ := ob["settings"].(map[string]interface{})
	for _, key := range []string{"vnext", "servers"} {
		if list, ok := settings[key].([]interface{}); ok && len(list) > 1 {
			return e, composite(strconv.Itoa(len(list)) + " targets")
		}
	}
	if flat, ok := settings["address"]; ok && flat != nil {
		listKey := ""
		switch protocol {
		case "vless", "vmess":
			listKey = "vnext"
		case "trojan", "shadowsocks":
			listKey = "servers"
		}
		if list, _ := settings[listKey].([]interface{}); listKey != "" && len(list) > 0 {
			return e, invalid("flat address beside a server list")
		}
	}
	target := vpnconfig.OutboundTarget(ob)
	address, _ := target["address"].(string)
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	number, _ := target["port"].(json.Number)
	port, ok := jsonPort(number)
	if literal := number.String(); literal == "" || strings.Trim(literal, "0123456789") != "" {
		ok = false
	}
	if address == "" || !ok {
		return e, invalid("bad address or port")
	}
	if sanitizeTLS(ob) {
		return e, unsupported("insecure TLS")
	}
	switch security, _ := stream["security"].(string); security {
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
	dropKeyLog(ob)
	e.address, e.port, e.outbound = address, port, ob
	return e, nil
}
