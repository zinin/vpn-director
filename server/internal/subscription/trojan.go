package subscription

// parseTrojan converts trojan://password@host:port?query#name. Trojan runs
// over TLS, so a link that names no security gets tls.
func parseTrojan(rest string) (entry, error) {
	l, err := splitLink(rest)
	if err != nil {
		return entry{name: l.name}, err
	}
	port, p, err := l.portAndQuery()
	if err != nil {
		return entry{name: l.name}, err
	}
	ss, err := streamSettings(p, "tls")
	if err != nil {
		return entry{name: l.name}, err
	}
	ob := map[string]interface{}{
		"protocol": "trojan",
		"settings": map[string]interface{}{"servers": []interface{}{
			map[string]interface{}{"address": l.host, "port": port, "password": l.userinfo},
		}},
		"streamSettings": ss,
	}
	return entry{name: l.name, address: l.host, port: port, outbound: prune(ob).(map[string]interface{})}, nil
}
