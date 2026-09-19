package subscription

import (
	"encoding/json"
	"math"
	"strings"
)

// parseVMess converts both vmess forms: the URL form
// vmess://id@host:port?query#name, recognized by the @, and the v2rayN form,
// base64 of a JSON object. Xray speaks VMess AEAD only, so alterId is ignored.
func parseVMess(rest string) (entry, error) {
	body, _, _ := strings.Cut(rest, "#")
	if strings.Contains(body, "@") {
		return parseVMessURL(rest)
	}
	return parseVMessV2rayN(body)
}

func parseVMessURL(rest string) (entry, error) {
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
	return vmessEntry(l.name, l.host, port, l.userinfo, orDefault(p["encryption"], "auto"), ss), nil
}

func parseVMessV2rayN(body string) (entry, error) {
	text, ok := decodeBase64(body)
	if !ok {
		return entry{}, invalid("vmess link is neither form")
	}
	obj, err := decodeObject(text)
	if err != nil {
		return entry{}, invalid("vmess link is neither form")
	}
	// A number counts as its decimal text; anything but a string or a number
	// is empty.
	field := func(key string) string {
		switch v := obj[key].(type) {
		case string:
			return v
		case json.Number:
			return v.String()
		}
		return ""
	}
	name := cleanName(field("ps"))
	for _, key := range []string{"add", "id", "scy", "net", "type", "host", "path", "tls", "sni", "alpn", "fp", "pbk", "sid", "spx"} {
		if strings.ContainsRune(field(key), 0) {
			return entry{name: name}, invalid("NUL byte in " + key)
		}
	}
	id := field("id")
	if id == "" {
		return entry{name: name}, invalid("missing id")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(field("add"), "["), "]")
	if host == "" {
		return entry{name: name}, invalid("missing address")
	}
	var port int
	switch v := obj["port"].(type) {
	case json.Number:
		port, ok = jsonPort(v)
		if !ok {
			return entry{name: name}, invalid("bad port " + v.String())
		}
	case string:
		if port, err = parsePort(v); err != nil {
			return entry{name: name}, err
		}
	default:
		return entry{name: name}, invalid("missing port")
	}
	security := "none"
	if tls := field("tls"); tls == "tls" || tls == "reality" {
		security = tls
	}
	p := params{
		"type":     field("net"),
		"security": security,
		"sni":      field("sni"),
		"alpn":     field("alpn"),
		"fp":       field("fp"),
		"pbk":      field("pbk"),
		"sid":      field("sid"),
		"spx":      field("spx"),
		"host":     field("host"),
	}
	switch field("net") {
	case "grpc":
		p["serviceName"], p["mode"] = field("path"), field("type")
	case "xhttp", "splithttp":
		p["path"], p["mode"] = field("path"), field("type")
	case "", "tcp", "raw":
		p["headerType"] = field("type")
	default:
		p["path"] = field("path")
	}
	ss, err := streamSettings(p, "none")
	if err != nil {
		return entry{name: name}, err
	}
	return vmessEntry(name, host, port, id, orDefault(field("scy"), "auto"), ss), nil
}

func vmessEntry(name, host string, port int, id, security string, ss map[string]interface{}) entry {
	ob := map[string]interface{}{
		"protocol": "vmess",
		"settings": map[string]interface{}{"vnext": []interface{}{
			map[string]interface{}{"address": host, "port": port, "users": []interface{}{
				map[string]interface{}{"id": id, "security": security},
			}},
		}},
		"streamSettings": ss,
	}
	return entry{name: name, address: host, port: port, outbound: prune(ob).(map[string]interface{})}
}

// jsonPort reads a JSON number that is a whole port, 1-65535.
func jsonPort(n json.Number) (int, bool) {
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f < 1 || f > 65535 {
		return 0, false
	}
	return int(f), true
}
