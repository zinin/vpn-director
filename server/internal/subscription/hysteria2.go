package subscription

import "strings"

// parseHysteria2 converts hysteria2://auth@host[:port][/]?query#name (hy2:// is
// the same). The port defaults to 443. Port hopping is left out: its keys
// moved between Xray 26.2 and 26.3, and no one config runs on both.
func parseHysteria2(rest string) (entry, error) {
	l, err := splitLink(rest)
	if err != nil {
		return entry{name: l.name}, err
	}
	port := 443
	if l.hasPort {
		if strings.ContainsAny(l.port, ",-") {
			return entry{name: l.name}, unsupported("port hopping")
		}
		if port, err = parsePort(l.port); err != nil {
			return entry{name: l.name}, err
		}
	}
	p, err := parseQuery(l.query)
	if err != nil {
		return entry{name: l.name}, err
	}
	switch obfs := p["obfs"]; obfs {
	case "":
	case "salamander":
		if p["obfs-password"] == "" {
			return entry{name: l.name}, invalid("salamander needs obfs-password")
		}
	default:
		return entry{name: l.name}, unsupported("hysteria2 obfs " + obfs)
	}
	if truthy(p["insecure"]) && p["pinSHA256"] == "" {
		return entry{name: l.name}, unsupported("insecure TLS")
	}
	alpn := splitList(p["alpn"])
	if len(alpn) == 0 {
		alpn = []interface{}{"h3"}
	}
	ss := map[string]interface{}{
		"network":          "hysteria",
		"security":         "tls",
		"hysteriaSettings": map[string]interface{}{"version": 2, "auth": l.userinfo},
		"tlsSettings": map[string]interface{}{
			"serverName":           p["sni"],
			"alpn":                 alpn,
			"pinnedPeerCertSha256": p["pinSHA256"],
		},
	}
	if p["obfs"] == "salamander" {
		ss["finalmask"] = map[string]interface{}{"udp": []interface{}{
			map[string]interface{}{"type": "salamander", "settings": map[string]interface{}{"password": p["obfs-password"]}},
		}}
	}
	ob := map[string]interface{}{
		"protocol":       "hysteria",
		"settings":       map[string]interface{}{"version": 2, "address": l.host, "port": port},
		"streamSettings": ss,
	}
	return entry{name: l.name, address: l.host, port: port, outbound: prune(ob).(map[string]interface{})}, nil
}
