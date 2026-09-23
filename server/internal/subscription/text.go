package subscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// asciiSpace is the whitespace Decode trims: the shell importer's set.
const asciiSpace = " \t\r\n\v\f"

var (
	errBadEscape = errors.New("bad percent escape")
	errNUL       = errors.New("NUL byte")
)

// unescape decodes %XX escapes strictly: a % without two hex digits after it
// is an error, and so is %00, since the shell importer cannot hold a NUL in a
// variable. plus turns + into a space, as a query does and userinfo does not.
func unescape(s string, plus bool) (string, error) {
	if !strings.ContainsRune(s, '%') && !(plus && strings.ContainsRune(s, '+')) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", errBadEscape
			}
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if v == 0 {
				return "", errNUL
			}
			b.WriteByte(v)
			i += 2
		case c == '+' && plus:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// unescapeName decodes a name leniently: a valid escape becomes its byte, %00
// becomes nothing, anything else stays as written, and + is a space. A name
// is only shown, so a stray % costs nothing.
func unescapeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			if v := unhex(s[i+1])<<4 | unhex(s[i+2]); v != 0 {
				b.WriteByte(v)
			}
			i += 2
		case c == '+':
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// decodeBase64 reads either alphabet, padded or not, and ignores whitespace -
// the shell importer maps the URL-safe characters onto the standard ones the
// same way, so a text mixing the two decodes on both sides. NUL bytes are
// dropped, as bash drops them.
func decodeBase64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		switch {
		case strings.ContainsRune(asciiSpace, r):
			return -1
		case r == '-':
			return '+'
		case r == '_':
			return '/'
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if s == "" || len(s)%4 == 1 || strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/") != "" {
		return "", false
	}
	s += strings.Repeat("=", (4-len(s)%4)%4)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return strings.ReplaceAll(string(b), "\x00", ""), true
}

// decodeJSON reads exactly one JSON value, numbers as json.Number.
func decodeJSON(s string) (interface{}, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

// decodeObject reads a JSON object.
func decodeObject(s string) (map[string]interface{}, error) {
	v, err := decodeJSON(s)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// splitList splits a comma list, trims spaces around each item and drops the
// empty ones.
func splitList(s string) []interface{} {
	out := []interface{}{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.Trim(item, " "); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// truthy is how share links spell a set flag.
func truthy(v string) bool { return v == "1" || v == "true" }

// guardedKeys holds every key the decoders, the sanitizer, the generators and
// the watch read or write, spelled as they look it up. It must grow when one
// of them starts reading a new key: keyCase refuses an entry that spells one
// of these otherwise. guard in lib/subscription.sh is the twin.
var guardedKeys = []string{
	"protocol", "settings", "vnext", "servers", "address", "port", "streamSettings", "network",
	"security", "tlsSettings", "realitySettings", "serverName", "alpn", "fingerprint", "publicKey",
	"password", "allowInsecure", "pinnedPeerCertSha256", "masterKeyLog", "sockopt", "echSockopt",
	"mark", "interface", "tproxy", "customSockopt", "dialerProxy", "proxySettings", "tag",
	"sendThrough", "xhttpSettings", "splithttpSettings", "wsSettings", "httpupgradeSettings",
	"grpcSettings", "authority", "host", "mode", "extra", "downloadSettings", "scMaxEachPostBytes",
}

// keyCase returns the first key in v, however deep and in byte order, that
// Xray reads as one of guardedKeys without being spelled that way, and the
// name it reads it as; "", "" when there is none. Every lookup here, in the
// sanitizer and in the generators is exact, but Xray loads its config with
// encoding/json, whose field matching folds case as strings.EqualFold does -
// the long s (U+017F) is an s to it and the Kelvin sign (U+212A) a k - so
// "Sockopt", "MasterKeyLog" or "marK" would pass every check and still reach
// Xray. Such an entry is refused, not repaired. What a "headers" object holds
// is left alone: Xray reads it as a map, and a header's name is the panel's
// to spell. The keys are sorted because a map has no order, so keycase in
// lib/subscription.sh names the same one.
func keyCase(v interface{}) (key, canonical string) {
	keys := miscased(v, nil)
	if len(keys) == 0 {
		return "", ""
	}
	sort.Strings(keys)
	return keys[0], foldsTo(keys[0])
}

// miscased appends to keys every key in v, however deep, that folds to a
// guarded name without being it, and returns them.
func miscased(v interface{}, keys []string) []string {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if canonical := foldsTo(k); canonical != "" && canonical != k {
				keys = append(keys, k)
			}
			if k != "headers" {
				keys = miscased(child, keys)
			}
		}
	case []interface{}:
		for _, child := range t {
			keys = miscased(child, keys)
		}
	}
	return keys
}

// foldsTo returns the guarded name k matches as encoding/json matches a key
// to a field, or "" when it matches none. No two guarded names fold alike, so
// a guarded name matches itself only.
func foldsTo(k string) string {
	for _, name := range guardedKeys {
		if strings.EqualFold(k, name) {
			return name
		}
	}
	return ""
}

// lowerStreamNames lowercases the string value of every security and network
// key in v, however deep: Xray lowercases a stream's network and security
// before it reads them (TransportProtocol.Build and StreamConfig.Build in
// 26.2.6), so "TLS" is a tls stream to it, and to every check here once
// lowered. What a "headers" object holds is left alone: Xray reads it as a
// map, and a header named network keeps its value. lower_names in
// lib/subscription.sh is the twin.
func lowerStreamNames(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if k == "headers" {
				continue
			}
			if s, ok := child.(string); ok && (k == "security" || k == "network") {
				t[k] = asciiLower(s)
				continue
			}
			lowerStreamNames(child)
		}
	case []interface{}:
		for _, child := range t {
			lowerStreamNames(child)
		}
	}
}

// asciiLower lowercases A-Z only, as jq's ascii_downcase does in the shell
// twin: a value with any other letter matches nothing Xray knows either way.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// socketKeys are the keys that hold an Xray SocketConfig: a stream's sockopt,
// and tlsSettings.echSockopt, the socket the ECH config query is dialed on.
var socketKeys = []string{"sockopt", "echSockopt"}

// scrubSockopt removes from every sockopt and echSockopt object in v, however
// deep, the keys that could route around our own rules, and drops one left
// empty. Depth matters: an xhttp "extra" carries whatever the subscription
// wrote, and Xray reads a downloadSettings stream config - sockopt and
// tlsSettings included - out of it. A foreign fwmark could collide with ours
// (0x100 Xray, 0x01 firmware VPN, 0x00ff0000 Tunnel Director), and an
// interface would route around the WAN. dropKeyLog, run beside it, takes out
// masterKeyLog. Keys are looked up as spelled: keyCase has already refused an
// entry that spells one of them any other way Xray would still read.
func scrubSockopt(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		// Children first, the order jq's walk gives scrub_sockopt in the shell
		// twin: a sockopt this one holds must be scrubbed, and dropped if that
		// left it empty, before this level weighs whether its own is empty.
		for _, child := range t {
			scrubSockopt(child)
		}
		for _, socketKey := range socketKeys {
			if sockopt, ok := t[socketKey].(map[string]interface{}); ok {
				for _, key := range []string{"mark", "interface", "tproxy", "customSockopt"} {
					delete(sockopt, key)
				}
				if len(sockopt) == 0 {
					delete(t, socketKey)
				}
			}
		}
	case []interface{}:
		for _, child := range t {
			scrubSockopt(child)
		}
	}
}

// hasDialerProxy reports whether any sockopt or echSockopt in v, however
// deep, names an outbound to dial through. A sockopt at the top of an entry
// already makes it composite; one inside an xhttp "extra", or the echSockopt
// of a tlsSettings, is the same chain in another place.
func hasDialerProxy(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, socketKey := range socketKeys {
			if sockopt, ok := t[socketKey].(map[string]interface{}); ok {
				if dialer, _ := sockopt["dialerProxy"].(string); dialer != "" {
					return true
				}
			}
		}
		for _, child := range t {
			if hasDialerProxy(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range t {
			if hasDialerProxy(child) {
				return true
			}
		}
	}
	return false
}

// dropKeyLog deletes masterKeyLog from every tlsSettings and realitySettings
// object in v, however deep, whatever the stream's security, as drop_keylog
// does in lib/subscription.sh. Xray opens that path to append every session's
// keys to, creating it 0644 (GetTLSConfig in 26.2.6, and REALITY alike): a
// file on the router the subscription picks, and traffic anyone who reads it
// can decrypt.
func dropKeyLog(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, child := range t {
			dropKeyLog(child)
		}
		for _, key := range []string{"tlsSettings", "realitySettings"} {
			if settings, ok := t[key].(map[string]interface{}); ok {
				delete(settings, "masterKeyLog")
			}
		}
	case []interface{}:
		for _, child := range t {
			dropKeyLog(child)
		}
	}
}

// sanitizeTLS deletes allowInsecure from every tls stream in v, however deep,
// and reports whether one of them named no pinnedPeerCertSha256 to replace it:
// Xray has loaded no config carrying the flag since 2026-06-01, and a stream
// inside an xhttp "extra" fails the load exactly like the one at the top.
// Only a tls stream is read for it - Xray ignores tlsSettings under any other
// security, and a stray flag there costs nothing (checked against 26.2.6).
// The key goes whatever its value, as drop_insecure does in
// lib/subscription.sh: only the boolean true asks for an insecure dial, but a
// panel's "allowInsecure": false is noise, and a string "true" is worse - Xray
// reads the field as a bool and refuses the whole config over one.
func sanitizeTLS(v interface{}) bool {
	insecure := false
	switch t := v.(type) {
	case map[string]interface{}:
		if security, _ := t["security"].(string); security == "tls" {
			if tls, ok := t["tlsSettings"].(map[string]interface{}); ok {
				if flag, _ := tls["allowInsecure"].(bool); flag {
					if pin, _ := tls["pinnedPeerCertSha256"].(string); pin == "" {
						insecure = true
					}
				}
				delete(tls, "allowInsecure")
			}
		}
		for _, child := range t {
			if sanitizeTLS(child) {
				insecure = true
			}
		}
	case []interface{}:
		for _, child := range t {
			if sanitizeTLS(child) {
				insecure = true
			}
		}
	}
	return insecure
}

// prune removes empty strings, arrays and objects, innermost first, so an
// object that only held empty values goes too (spec 6.3). It changes v in
// place and returns it.
func prune(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, e := range t {
			if e = prune(e); isEmpty(e) {
				delete(t, k)
			} else {
				t[k] = e
			}
		}
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, e := range t {
			if e = prune(e); !isEmpty(e) {
				out = append(out, e)
			}
		}
		return out
	}
	return v
}

func isEmpty(v interface{}) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case []interface{}:
		return len(t) == 0
	case map[string]interface{}:
		return len(t) == 0
	}
	return false
}
