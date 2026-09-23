package subscription

import "strings"

// ssMethods are the ciphers Xray 26.2.6 accepts, lowercased.
var ssMethods = map[string]bool{
	"aes-128-gcm":                   true,
	"aes-256-gcm":                   true,
	"chacha20-poly1305":             true,
	"chacha20-ietf-poly1305":        true,
	"xchacha20-poly1305":            true,
	"xchacha20-ietf-poly1305":       true,
	"2022-blake3-aes-128-gcm":       true,
	"2022-blake3-aes-256-gcm":       true,
	"2022-blake3-chacha20-poly1305": true,
	"none":                          true,
	"plain":                         true,
}

// parseShadowsocks converts SIP002 ss://userinfo@host:port[/][?plugin=…]#name,
// where userinfo is method:password in plain text or in base64, and the legacy
// ss://base64(method:password@host:port)#name. Base64 may hold a /, so the
// userinfo is all of the part before ? up to its last @, and only the host
// part ends at a /.
func parseShadowsocks(rest string) (entry, error) {
	body, frag, _ := strings.Cut(rest, "#")
	e := entry{name: cleanName(unescapeName(frag))}
	main, query, _ := strings.Cut(body, "?")
	var userinfo, hostport string
	if at := strings.LastIndex(main, "@"); at >= 0 {
		ui, err := unescape(main[:at], false)
		if err != nil {
			return e, invalid("userinfo: " + err.Error())
		}
		if !strings.Contains(ui, ":") {
			decoded, ok := decodeBase64(ui)
			if !ok {
				return e, invalid("userinfo is not base64")
			}
			ui = strings.Trim(decoded, asciiSpace)
		}
		userinfo = ui
		hostport, _, _ = strings.Cut(main[at+1:], "/")
	} else {
		decoded, ok := decodeBase64(main)
		if !ok {
			return e, invalid("legacy link is not base64")
		}
		decoded = strings.Trim(decoded, asciiSpace)
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return e, invalid("legacy link has no @")
		}
		userinfo, hostport = decoded[:at], decoded[at+1:]
	}
	// A Shadowsocks 2022 password may hold a colon of its own.
	method, password, _ := strings.Cut(userinfo, ":")
	if method == "" || password == "" {
		return e, invalid("missing method or password")
	}
	host, rawPort, hasPort, err := splitHostPort(hostport)
	if err != nil {
		return e, err
	}
	if !hasPort {
		return e, invalid("missing port")
	}
	port, err := parsePort(rawPort)
	if err != nil {
		return e, err
	}
	p, err := parseQuery(query)
	if err != nil {
		return e, err
	}
	if p["plugin"] != "" {
		return e, unsupported("ss plugin")
	}
	method = strings.ToLower(method)
	if !ssMethods[method] {
		return e, unsupported("ss method " + method)
	}
	e.address, e.port = host, port
	e.outbound = prune(map[string]interface{}{
		"protocol": "shadowsocks",
		"settings": map[string]interface{}{"servers": []interface{}{
			map[string]interface{}{"address": host, "port": port, "method": method, "password": password},
		}},
	}).(map[string]interface{})
	return e, nil
}
