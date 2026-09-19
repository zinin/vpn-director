package subscription

// parseVLESS converts vless://id@host:port?query#name.
func parseVLESS(rest string) (entry, error) {
	l, err := splitLink(rest)
	if err != nil {
		return entry{name: l.name}, err
	}
	port, p, err := l.portAndQuery()
	if err != nil {
		return entry{name: l.name}, err
	}
	ss, err := streamSettings(p, "none")
	if err != nil {
		return entry{name: l.name}, err
	}
	user := map[string]interface{}{
		"id":         l.userinfo,
		"encryption": orDefault(p["encryption"], "none"),
		"flow":       p["flow"],
	}
	ob := map[string]interface{}{
		"protocol": "vless",
		"settings": map[string]interface{}{"vnext": []interface{}{
			map[string]interface{}{"address": l.host, "port": port, "users": []interface{}{user}},
		}},
		"streamSettings": ss,
	}
	return entry{name: l.name, address: l.host, port: port, outbound: prune(ob).(map[string]interface{})}, nil
}
