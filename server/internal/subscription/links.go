package subscription

import (
	"strconv"
	"strings"
)

// decodeLinks reads a link list: one link per line, other lines ignored
// (spec 6.1).
func decodeLinks(text string) (Result, error) {
	var r Result
	for _, line := range strings.Split(text, "\n") {
		scheme, rest, ok := splitScheme(strings.Trim(line, asciiSpace))
		if !ok {
			continue
		}
		r.Total++
		e, err := parseLink(scheme, rest)
		r.add(r.Total, e, err)
	}
	if r.Total == 0 {
		return Result{}, ErrUnrecognized
	}
	return r, nil
}

// splitScheme splits "scheme://rest" when the scheme matches
// [A-Za-z][A-Za-z0-9+.-]*, and lowercases the scheme.
func splitScheme(line string) (scheme, rest string, ok bool) {
	i := strings.Index(line, "://")
	if i <= 0 {
		return "", "", false
	}
	for k := 0; k < i; k++ {
		c := line[k]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (k == 0 || !(c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-')) {
			return "", "", false
		}
	}
	return strings.ToLower(line[:i]), line[i+3:], true
}

// parseLink converts one link by its scheme.
func parseLink(scheme, rest string) (entry, error) {
	switch scheme {
	case "vless":
		return parseVLESS(rest)
	case "vmess":
		return parseVMess(rest)
	case "trojan":
		return parseTrojan(rest)
	case "ss":
		return parseShadowsocks(rest)
	case "hysteria2", "hy2":
		return parseHysteria2(rest)
	}
	_, frag, _ := strings.Cut(rest, "#")
	return entry{name: cleanName(unescapeName(frag))}, unsupported(scheme)
}

// link is a share link split by the grammar of spec 6.2:
// scheme://userinfo@host[:port][/][?query][#fragment].
type link struct {
	name     string // the fragment, decoded and cleaned
	userinfo string // percent-decoded, + kept
	host     string // brackets removed
	port     string // as written
	hasPort  bool
	query    string // as written
}

// splitLink applies the grammar up to the host. The port and the query are
// left to the caller, because hysteria2 reads the port its own way before
// either is checked.
func splitLink(rest string) (link, error) {
	var l link
	body, frag, _ := strings.Cut(rest, "#")
	l.name = cleanName(unescapeName(frag))
	main, query, _ := strings.Cut(body, "?")
	l.query = query
	authority, _, _ := strings.Cut(main, "/")
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return l, invalid("missing userinfo")
	}
	userinfo, err := unescape(authority[:at], false)
	if err != nil {
		return l, invalid("userinfo: " + err.Error())
	}
	if userinfo == "" {
		return l, invalid("missing userinfo")
	}
	l.userinfo = userinfo
	l.host, l.port, l.hasPort, err = splitHostPort(authority[at+1:])
	return l, err
}

// splitHostPort splits host[:port] and [ipv6][:port].
func splitHostPort(hostport string) (host, port string, hasPort bool, err error) {
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", false, invalid("bad IPv6 address")
		}
		host = hostport[1:end]
		tail := hostport[end+1:]
		switch {
		case tail == "":
		case strings.HasPrefix(tail, ":"):
			port, hasPort = tail[1:], true
		default:
			return "", "", false, invalid("bad address")
		}
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, port, hasPort = hostport[:i], hostport[i+1:], true
	} else {
		host = hostport
	}
	if host == "" {
		return "", "", false, invalid("missing host")
	}
	return host, port, hasPort, nil
}

// parsePort reads a port of one to five decimal digits, 1-65535.
func parsePort(s string) (int, error) {
	if s == "" || len(s) > 5 || strings.Trim(s, "0123456789") != "" {
		return 0, invalid("bad port " + strconv.Quote(s))
	}
	n, _ := strconv.Atoi(s)
	if n < 1 || n > 65535 {
		return 0, invalid("bad port " + strconv.Quote(s))
	}
	return n, nil
}

// params are a link's query values; a repeated key keeps its first value.
type params map[string]string

// parseQuery splits a query on & and decodes keys and values, + as a space.
// An empty key is ignored.
func parseQuery(q string) (params, error) {
	p := params{}
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err := unescape(k, true)
		if err != nil {
			return nil, invalid("query: " + err.Error())
		}
		val, err := unescape(v, true)
		if err != nil {
			return nil, invalid("query: " + err.Error())
		}
		if _, seen := p[key]; key != "" && !seen {
			p[key] = val
		}
	}
	return p, nil
}

// portAndQuery finishes the grammar for a scheme that needs a port.
func (l link) portAndQuery() (int, params, error) {
	if !l.hasPort {
		return 0, nil, invalid("missing port")
	}
	port, err := parsePort(l.port)
	if err != nil {
		return 0, nil, err
	}
	p, err := parseQuery(l.query)
	return port, p, err
}

// streamSettings builds an outbound's streamSettings from share-link
// parameters (spec 6.3). defaultSecurity is "none", or "tls" for trojan.
func streamSettings(p params, defaultSecurity string) (map[string]interface{}, error) {
	network := p["type"]
	switch network {
	case "", "tcp", "raw":
		network = "tcp"
	case "ws", "websocket":
		network = "ws"
	case "grpc", "httpupgrade":
	case "xhttp", "splithttp":
		network = "xhttp"
	default:
		return nil, unsupported("transport " + network)
	}
	if ht := p["headerType"]; network == "tcp" && ht != "" && ht != "none" {
		return nil, unsupported("tcp header " + ht)
	}
	security := p["security"]
	if security == "" {
		security = defaultSecurity
	}
	ss := map[string]interface{}{"network": network, "security": security}
	switch security {
	case "none":
	case "tls":
		if (truthy(p["allowInsecure"]) || truthy(p["insecure"])) && p["pcs"] == "" {
			return nil, unsupported("insecure TLS")
		}
		ss["tlsSettings"] = map[string]interface{}{
			"serverName":           p["sni"],
			"fingerprint":          p["fp"],
			"alpn":                 splitList(p["alpn"]),
			"pinnedPeerCertSha256": p["pcs"],
			"verifyPeerCertByName": p["vcn"],
		}
	case "reality":
		if p["pbk"] == "" || p["sni"] == "" || p["fp"] == "" {
			return nil, invalid("reality needs pbk, sni and fp")
		}
		ss["realitySettings"] = map[string]interface{}{
			"serverName":    p["sni"],
			"fingerprint":   p["fp"],
			"publicKey":     p["pbk"],
			"shortId":       p["sid"],
			"spiderX":       p["spx"],
			"mldsa65Verify": p["pqv"],
		}
	default:
		return nil, unsupported("security " + security)
	}
	switch network {
	case "ws":
		ss["wsSettings"] = map[string]interface{}{"path": p["path"], "host": p["host"]}
	case "httpupgrade":
		ss["httpupgradeSettings"] = map[string]interface{}{"path": p["path"], "host": p["host"]}
	case "grpc":
		grpc := map[string]interface{}{"serviceName": p["serviceName"], "authority": p["authority"]}
		if p["mode"] == "multi" {
			grpc["multiMode"] = true
		}
		ss["grpcSettings"] = grpc
	case "xhttp":
		xhttp := map[string]interface{}{"path": p["path"], "host": p["host"], "mode": p["mode"]}
		if raw := p["extra"]; raw != "" {
			extra, err := decodeObject(raw)
			if err != nil {
				return nil, invalid("xhttp extra is not a JSON object")
			}
			if hasDialerProxy(extra) {
				return nil, composite("chained")
			}
			if sanitizeTLS(extra) {
				return nil, unsupported("insecure TLS")
			}
			scrubSockopt(extra)
			xhttp["extra"] = extra
		}
		ss["xhttpSettings"] = xhttp
	}
	return ss, nil
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
