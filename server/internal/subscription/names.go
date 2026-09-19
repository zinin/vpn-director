package subscription

import (
	"strconv"
	"strings"
)

// cleanName keeps what a server list can show anywhere: ASCII letters, digits,
// the space and .,;:!?()-, and every two-byte UTF-8 character (U+0080-U+07FF:
// Cyrillic, Greek, accented Latin). Emoji and other symbols go.
//
// It walks bytes, not runes, exactly as the gawk filter of lib/subscription.sh
// does on routers without a UTF-8 locale, so the two agree on invalid UTF-8
// too: a lead byte without its continuation bytes is dropped alone. Runs of
// spaces collapse; spaces and commas at either end go.
func cleanName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isNameASCII(c):
			b.WriteByte(c)
			i++
		case c >= 0xC2 && c <= 0xDF && continued(s, i, 1):
			b.WriteString(s[i : i+2])
			i += 2
		case c >= 0xE0 && c <= 0xEF && continued(s, i, 2):
			i += 3
		case c >= 0xF0 && c <= 0xF4 && continued(s, i, 3):
			i += 4
		default:
			i++
		}
	}
	collapsed := strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == ' ' }), " ")
	return strings.Trim(collapsed, " ,")
}

func isNameASCII(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		strings.IndexByte(" .,;:!?()-", c) >= 0
}

// continued reports whether the n bytes after s[i] are all continuation bytes.
func continued(s string, i, n int) bool {
	if i+n >= len(s) {
		return false
	}
	for k := 1; k <= n; k++ {
		if s[i+k] < 0x80 || s[i+k] > 0xBF {
			return false
		}
	}
	return true
}

// isPlaceholder reports the addresses panels give the fake entries that carry
// a notice such as "subscription expired": an IPv4 literal in 0.0.0.0/8 or
// 127.0.0.0/8, or the IPv6 literal "::" or "::1" written just so. The test is
// textual - the one lib/subscription.sh makes.
func isPlaceholder(addr string) bool {
	if addr == "::" || addr == "::1" {
		return true
	}
	parts := strings.Split(addr, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 || strings.Trim(p, "0123456789") != "" {
			return false
		}
		if v, _ := strconv.Atoi(p); v > 255 {
			return false
		}
	}
	first, _ := strconv.Atoi(parts[0])
	return first == 0 || first == 127
}
