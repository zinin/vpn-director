package subscription

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
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

func TestImportSummary(t *testing.T) {
	imp := Import{Total: 40, ResolveErrors: 1}
	imp.Servers = make([]vpnconfig.Server, 32)
	for i := 0; i < 7; i++ {
		imp.Skipped = append(imp.Skipped, Skip{Name: "Auto", Reason: ReasonComposite, Detail: "12 proxy outbounds"})
	}
	if got := imp.Counts(); got != "7 composite, 1 DNS error" {
		t.Errorf("Counts() = %q", got)
	}
	if got := imp.Summary(); got != "Imported 32 of 40 servers: 7 composite, 1 DNS error" {
		t.Errorf("Summary() = %q", got)
	}
	if got := imp.SkippedByReason(); !reflect.DeepEqual(got, map[string]int{ReasonUnsupported: 0, ReasonComposite: 7, ReasonInvalid: 0, ReasonPlaceholder: 0}) {
		t.Errorf("SkippedByReason() = %v", got)
	}
	whole := Import{Total: 2, Servers: make([]vpnconfig.Server, 2)}
	if got := whole.Summary(); got != "Imported 2 servers" {
		t.Errorf("Summary() = %q", got)
	}
}

func TestImportNoServers(t *testing.T) {
	imp := Import{Total: 6, Skipped: []Skip{
		{Name: "TUIC", Reason: ReasonUnsupported, Detail: "tuic"},
		{Name: "Auto", Reason: ReasonComposite, Detail: "3 proxy outbounds"},
		{Name: "A", Reason: ReasonPlaceholder, Detail: "127.0.0.1"},
		{Name: "B", Reason: ReasonPlaceholder, Detail: "0.0.0.0"},
		{Name: "Bad", Reason: ReasonInvalid, Detail: "missing port"},
		{Name: "KCP", Reason: ReasonUnsupported, Detail: "transport kcp"},
	}}
	want := "no supported servers in subscription: 2 unsupported, 1 composite, 1 invalid, 2 placeholders; " +
		"TUIC: tuic; Bad: missing port; KCP: transport kcp"
	if got := imp.NoServers(); got != want {
		t.Errorf("NoServers() =\n%q\nwant\n%q", got, want)
	}
}

// A detail embeds subscription text as written, and a name keeps the C1
// controls: neither may take an escape sequence or a line break to Telegram.
func TestImportDetailsDropControls(t *testing.T) {
	imp := Import{Total: 1, Skipped: []Skip{
		{Name: "A\u0085B", Reason: ReasonUnsupported, Detail: "transport \x1b[31mX\nfake"},
	}}
	want := []string{"AB: transport [31mXfake"}
	if got := imp.Details(3); !reflect.DeepEqual(got, want) {
		t.Errorf("Details(3) = %q, want %q", got, want)
	}
}

func TestDecodeAndResolve(t *testing.T) {
	// IP literals resolve without DNS; the IPv6 one does not resolve over IPv4.
	body := strings.Join([]string{
		"vless://uuid-1@203.0.113.10:443?security=none#Oslo",
		"vless://missing-at-sign:443#Broken",
		"trojan://pw@198.51.100.7:8443#Paris",
		"vless://uuid-2@[2001:db8::1]:443#Six",
	}, "\n")

	imp, err := DecodeAndResolve(body)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Total != 4 || imp.Parsed != 3 || len(imp.Skipped) != 1 || imp.ResolveErrors != 1 {
		t.Fatalf("%+v", imp)
	}
	if len(imp.Servers) != 2 {
		t.Fatalf("servers %+v", imp.Servers)
	}
	if got := imp.Servers[0]; got.Name != "Oslo" || !reflect.DeepEqual(got.IPs, []string{"203.0.113.10"}) {
		t.Errorf("first server %+v", got)
	}
	if got := imp.Servers[1]; got.Name != "Paris" || !reflect.DeepEqual(got.IPs, []string{"198.51.100.7"}) {
		t.Errorf("second server %+v", got)
	}
}

func TestDecodeAndResolve_PassesTheDecodeErrorThrough(t *testing.T) {
	if _, err := DecodeAndResolve("<html>not a subscription</html>"); !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("err %v, want ErrUnrecognized", err)
	}
}

func TestDecodeAndResolveLookup(t *testing.T) {
	var looked []string
	imp, err := DecodeAndResolveLookup("vless://uuid-1@oslo.example.invalid:443#Oslo", func(host string) ([]net.IP, error) {
		looked = append(looked, host)
		return []net.IP{net.ParseIP("2001:db8::5"), net.ParseIP("203.0.113.50")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Servers) != 1 || !reflect.DeepEqual(imp.Servers[0].IPs, []string{"203.0.113.50"}) {
		t.Fatalf("%+v", imp)
	}
	if !reflect.DeepEqual(looked, []string{"oslo.example.invalid"}) {
		t.Fatalf("lookup %v", looked)
	}
}

func TestLookupIPv4_ReturnsOnlyIPv4(t *testing.T) {
	ips, err := LookupIPv4(context.Background())("localhost")
	if err != nil {
		t.Skipf("no local resolver for localhost: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("localhost resolved to nothing")
	}
	for _, ip := range ips {
		if ip.To4() == nil {
			t.Fatalf("resolved %v; an AF_UNSPEC lookup is what waits out the AAAA half", ip)
		}
	}
}

// The subscription of a router can name tens of hosts, resolved one after the
// other inside a watch tick. A stop or a shutdown has to be able to end that.
func TestLookupIPv4_HonoursTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LookupIPv4(ctx)("localhost"); err == nil {
		t.Fatal("a canceled context must end the lookup")
	}
}

// A skip's detail is the twin of the shell's, which interpolates the port as
// written (_sub_port in lib/subscription.sh). Escaping it - strconv.Quote
// turns a quote into \" and a backslash into \\ - would make the two disagree
// about the same link.
func TestDecode_BadPortDetailIsThePortAsWritten(t *testing.T) {
	res, err := Decode(`vless://u@h.example.com:4"4\5#Bad port`)
	if err != nil {
		t.Fatal(err)
	}
	want := `bad port "4"4\5"`
	if len(res.Skipped) != 1 || res.Skipped[0].Detail != want {
		t.Fatalf("skipped %+v, want one skip detailed %s", res.Skipped, want)
	}
}
