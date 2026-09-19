package subscription

import (
	"errors"
	"testing"
)

func TestCleanName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\U0001F1F3\U0001F1F1 Амстердам, Нидерланды", "Амстердам, Нидерланды"},
		{"\U0001F1EA\U0001F1FA \U0001F680Авто | Лучший сервер ⚡⚡", "Авто Лучший сервер"},
		{"  Türkiye   Istanbul , ", "Türkiye Istanbul"},
		{"Ελλάδα", "Ελλάδα"},
		{"\U0001F1FA\U0001F1F8\U0001F31F", ""},
		// A lead byte without its continuation byte goes alone; the | after
		// it is no letter either.
		{"\xC3|evil", "evil"},
		// Three- and four-byte lead bytes without their continuation bytes
		// drop one byte, not three or four.
		{"\xE2AB", "AB"},
		{"\xF0ABC", "ABC"},
		{"a\x00\tb\nc", "abc"},
	} {
		if got := cleanName(tc.in); got != tc.want {
			t.Errorf("cleanName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsPlaceholder(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1":     true,
		"127.000.0.1":   true,
		"0.0.0.0":       true,
		"::":            true,
		"::1":           true,
		"128.0.0.1":     false,
		"127.0.0.256":   false,
		"127.1":         false,
		"0:0:0:0::1":    false,
		"localhost":     false,
		"198.51.100.10": false,
	} {
		if got := isPlaceholder(addr); got != want {
			t.Errorf("isPlaceholder(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestUnescape(t *testing.T) {
	for _, tc := range []struct {
		in   string
		plus bool
		want string
		err  error
	}{
		{"h2%2Chttp%2F1.1", true, "h2,http/1.1", nil},
		{"a+b%2B", true, "a b+", nil},
		{"a+b%2B", false, "a+b+", nil},
		{"50%", true, "", errBadEscape},
		{"x%2y", true, "", errBadEscape},
		{"%%41", true, "", errBadEscape},
		{"a%00b", true, "", errNUL},
	} {
		got, err := unescape(tc.in, tc.plus)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("unescape(%q, %v) = %q, %v; want %q, %v", tc.in, tc.plus, got, err, tc.want, tc.err)
		}
	}
}

func TestUnescapeName(t *testing.T) {
	if got := unescapeName("100%25%20off%2%zz+plus%00!"); got != "100% off%2%zz plus!" {
		t.Errorf("unescapeName = %q", got)
	}
}

func TestDecodeBase64(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"Pz8/Pw==", "????", true},
		{"Pz8_Pw", "????", true},
		{"Pj4-Pz8_", ">>>???", true},
		{"Pj4+Pz8_", ">>>???", true},
		{"Pj4+\nPz8/\r\n", ">>>???", true},
		{"YQBi", "ab", true},
		{"a", "", false},
		{"ab=c", "", false},
		{"@@@", "", false},
		{"", "", false},
	} {
		got, ok := decodeBase64(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("decodeBase64(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
