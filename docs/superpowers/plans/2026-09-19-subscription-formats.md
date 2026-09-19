# Subscription Formats Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import share links of every protocol the router's Xray runs (vless, vmess, trojan, ss, hysteria2), base64-encoded or plain, and Xray JSON subscriptions, in both the shell and the Go importer, storing every server as a ready Xray outbound.

**Architecture:** Two twin decoders — `router/opt/vpn-director/lib/subscription.sh` (bash, jq, gawk) and the Go package `server/internal/subscription` — turn a subscription body into servers that carry a ready Xray `outbound`. The shared cases in `testdata/subscription/` hold both to the same answers. Both config generators insert the stored outbound as `proxy-out` and have `xray run -test` check a config before it replaces the live one; a record from before outbounds were stored keeps today's VLESS builder. Labels name each server's protocol in the Web UI, the bot and `configure.sh`.

**Tech Stack:** Go 1.25 (standard library only); bash 5, jq 1.8 without oniguruma and gawk from Entware; bats-core with bats-support and bats-assert; Vue 3 + TypeScript (vue-tsc, Vite).

**Spec:** `docs/superpowers/specs/2026-09-18-subscription-formats-design.md` — read it before starting; this plan implements it section by section and names the sections it follows.

## Global Constraints

- **Parity.** Every decoding rule lives in both `router/opt/vpn-director/lib/subscription.sh` and `server/internal/subscription`, and every case in `testdata/subscription/` must decode identically in both (`router/test/unit/subscription.bats`, `server/internal/subscription/fixtures_test.go`). A rule changed on one side only is a bug.
- **jq on the routers.** Entware's jq 1.8.1 is built without oniguruma: shipped jq must not use `test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub` or the two-argument `split`. `label` is a jq keyword; never name a jq function `label`.
- **Shell on the routers.** bash 5 from Entware; scripts run under `set -euo pipefail`. Byte-wise work runs under `LC_ALL=C`. Never `tr '[:upper:]'` (see `.claude/rules/shell-conventions.md`); no `paste`. A function that returns non-zero on purpose is called as `f || :` or inside a condition.
- **Fixtures are synthetic.** Addresses only from 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32; hosts under `example.com`, `example.org`, `example.net`; invented ids, passwords and keys, in the formats Xray checks (a REALITY key is base64url of 32 bytes, a pin 64 hex digits, an SS-2022 key base64 of the cipher's key length). No real provider host, SNI, key or token: the repository is public.
- **Xray.** The routers run Entware's xray-core 26.2.6. Since 2026-06-01 Xray refuses any config with `tlsSettings.allowInsecure: true`. `xray run -test` needs `-format json` for a file whose name does not end in `.json`.
- **Go.** Module `github.com/zinin/vpn-director/server`, `go 1.25.5`, no new dependencies. `go test ./...` compiles `cmd/webui`, which embeds `server/cmd/webui/web/dist`; if that directory is missing, run `make web-embed` once from the repository root. CI runs `go vet ./...` and `go test ./... -count=1` in `server/`.
- **Fixed words.** Skip reasons are exactly `unsupported`, `composite`, `invalid`, `placeholder`. Decode errors are exactly `empty subscription`, `invalid JSON subscription`, `unrecognized subscription format`.
- **Commits.** One per task, conventional style as in `git log` (`feat(scope): …`, `test: …`, `docs: …`); `git add` only the files the task names — the working tree holds unrelated untracked files.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `server/internal/vpnconfig/vpnconfig.go` | `Server.Outbound`; `uuid` omitted when empty | 1 |
| `server/internal/vpnconfig/outbound.go` | `DecodeOutbound`, `OutboundTarget` (the address slot), `Server.Label` | 1 |
| `server/internal/subscription/subscription.go` | `Result`, `Skip`, reasons, decode errors, `Result.add` (placeholder test, naming) | 2 |
| `server/internal/subscription/names.go` | `cleanName` (byte machine), `isPlaceholder` | 2 |
| `server/internal/subscription/text.go` | strict and lenient percent-decoding, base64, JSON reading, `splitList`, `prune` | 2 |
| `server/internal/subscription/decode.go` | `Decode`: container detection | 3, 5 |
| `server/internal/subscription/links.go` | link list, share-link grammar, query, stream settings | 3, 4 |
| `server/internal/subscription/{vless,trojan,vmess,shadowsocks,hysteria2}.go` | one converter per scheme | 3, 4 |
| `server/internal/subscription/xrayjson.go` | Xray JSON: the single proxy outbound, checks, sanitization | 5 |
| `server/internal/subscription/resolve.go` | `Import`, `LookupIPv4`, `DecodeAndResolve`, `DecodeAndResolveLookup` | 6 |
| `server/internal/subscription/summary.go` | `Counts`, `Details`, `Summary`, `NoServers`, `SkippedByReason` | 6 |
| `server/internal/subscription/fixtures_test.go` | runs every shared case through `Decode` | 3 |
| `router/opt/vpn-director/lib/subscription.sh` | the shell twin: `subscription_decode` | 2–5 |
| `router/test/unit/subscription.bats` | runs every shared case through `subscription_decode`; helper tests; jq-regex guard | 2, 3 |
| `testdata/subscription/*.in`, `*.want.json` | the shared cases | 3–5 |
| `server/internal/bot/subfetch.go`, `handler/import.go`, `webapi/handler_servers.go` | callers switch from `vless` to `subscription`; import reports | 6 |
| `server/internal/vless/` | removed | 6 |
| `server/internal/service/xray.go` | `serverOutbound`, `xrayTest` before the rename | 7 |
| `server/internal/subwatch/watch.go` | `ServerForDial` on a stored outbound, `keepHostname` | 8 |
| `server/internal/webapi/handler_servers.go`, `handler/servers.go`, `web/src/*` | protocol labels, no credentials in `/api/servers` | 9 |
| `router/opt/vpn-director/lib/xrayconf.sh`, `configure.sh`, `router/test/mocks/xray` | stored outbound, `xrayconf_validate`, labels | 10 |
| `router/opt/vpn-director/import_server_list.sh`, `install.sh` | the shell importer on the decoder | 11 |
| `CLAUDE.md`, `.claude/rules/*.md`, `README*.md` | documentation | 12 |

Patches are unified diffs against the tree the earlier tasks left; run each block from the repository root as shown. If `git apply` refuses a hunk, the tree differs from what the earlier tasks produce: stop and compare, do not force it.

---

### Task 1: Stored outbound on the server record

Spec sections 4 and 10 (the label). `servers.json` records gain the Xray outbound their import converted; the generators and the watch find the outbound's address through `OutboundTarget`; every list names the protocol through `Label`.

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (the `Server` struct)
- Create: `server/internal/vpnconfig/outbound.go`
- Test: `server/internal/vpnconfig/outbound_test.go`

**Interfaces:**
- Produces: `vpnconfig.Server.Outbound json.RawMessage` (`json:"outbound,omitempty"`); `UUID` tagged `json:"uuid,omitempty"`; `func DecodeOutbound(raw json.RawMessage) (map[string]interface{}, error)` (numbers kept as `json.Number`); `func OutboundTarget(ob map[string]interface{}) map[string]interface{}` — `settings.vnext[0]` for vless/vmess, `settings.servers[0]` for trojan/shadowsocks, else `settings` itself, nil without `settings`; `func (s Server) Label() string`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/vpnconfig/outbound_test.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"testing"
)

func TestDecodeOutbound_KeepsNumbersAsWritten(t *testing.T) {
	ob, err := DecodeOutbound(json.RawMessage(`{"protocol":"vless","settings":{"port":443,"level":1.0}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(ob)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"protocol":"vless","settings":{"level":1.0,"port":443}}` {
		t.Fatalf("round trip %s", out)
	}
	if _, err := DecodeOutbound(json.RawMessage(`null`)); err == nil {
		t.Fatal("null is no outbound")
	}
	if _, err := DecodeOutbound(json.RawMessage(`[1]`)); err == nil {
		t.Fatal("an array is no outbound")
	}
}

func TestOutboundTarget(t *testing.T) {
	for _, tc := range []struct {
		name, outbound, want string
	}{
		{"vless vnext", `{"protocol":"vless","settings":{"vnext":[{"address":"a.example"}]}}`, "a.example"},
		{"vmess vnext", `{"protocol":"vmess","settings":{"vnext":[{"address":"b.example"}]}}`, "b.example"},
		{"trojan servers", `{"protocol":"trojan","settings":{"servers":[{"address":"c.example"}]}}`, "c.example"},
		{"shadowsocks servers", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"d.example"}]}}`, "d.example"},
		{"vless flat", `{"protocol":"vless","settings":{"address":"e.example"}}`, "e.example"},
		{"trojan flat", `{"protocol":"trojan","settings":{"address":"f.example"}}`, "f.example"},
		{"hysteria", `{"protocol":"hysteria","settings":{"address":"g.example","vnext":[{"address":"not-this"}]}}`, "g.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ob, err := DecodeOutbound(json.RawMessage(tc.outbound))
			if err != nil {
				t.Fatal(err)
			}
			target := OutboundTarget(ob)
			if target == nil || target["address"] != tc.want {
				t.Fatalf("target %v, want address %q", target, tc.want)
			}
		})
	}
	if target := OutboundTarget(map[string]interface{}{"protocol": "vless"}); target != nil {
		t.Fatalf("target %v of an outbound without settings", target)
	}
}

func TestServerLabel(t *testing.T) {
	for _, tc := range []struct {
		server Server
		want   string
	}{
		{Server{Security: "reality", Network: "tcp"}, "vless·reality"},
		{Server{}, "vless·tls"},
		{Server{Security: "tls", Network: "ws"}, "vless·ws·tls"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"xhttp","security":"reality"}}`)}, "vless·xhttp·reality"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"raw","security":"none"}}`)}, "vless"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vmess","streamSettings":{"network":"ws"}}`)}, "vmess·ws"},
		{Server{Outbound: json.RawMessage(`{"protocol":"trojan","streamSettings":{"network":"tcp","security":"tls"}}`)}, "trojan·tls"},
		{Server{Outbound: json.RawMessage(`{"protocol":"shadowsocks","streamSettings":{"network":"tcp","security":"none"}}`)}, "ss"},
		{Server{Outbound: json.RawMessage(`{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}`)}, "hysteria2"},
	} {
		if got := tc.server.Label(); got != tc.want {
			t.Errorf("Label() of %+v = %q, want %q", tc.server, got, tc.want)
		}
	}
}

// A record an import writes now carries no UUID of its own; the empty field
// must not appear in servers.json.
func TestServer_NoEmptyUUIDInTheFile(t *testing.T) {
	out, err := json.Marshal(Server{Name: "Oslo", Address: "oslo.example", Port: 443, Outbound: json.RawMessage(`{"protocol":"trojan"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"address":"oslo.example","port":443,"name":"Oslo","ips":null,"outbound":{"protocol":"trojan"}}` {
		t.Fatalf("marshal %s", out)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 ./internal/vpnconfig/`
Expected: FAIL to compile — `undefined: DecodeOutbound`, `OutboundTarget`, `Outbound`, `Label`.

- [ ] **Step 3: Add the field**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/vpnconfig/vpnconfig.go
+++ b/server/internal/vpnconfig/vpnconfig.go
@@ -7,20 +7,25 @@
 	"sort"
 )
 
+// Server is one entry of servers.json. An import writes Name, Address, Port,
+// IPs and Outbound, the Xray outbound the server runs on. The flat VLESS fields
+// below are what imports wrote before Outbound existed: a record that has no
+// Outbound is still read, and generated from them.
 type Server struct {
-	Address     string   `json:"address"`
-	Port        int      `json:"port"`
-	UUID        string   `json:"uuid"`
-	Name        string   `json:"name"`
-	IPs         []string `json:"ips"`
-	Security    string   `json:"security,omitempty"`
-	Network     string   `json:"network,omitempty"`
-	Flow        string   `json:"flow,omitempty"`
-	SNI         string   `json:"sni,omitempty"`
-	Fingerprint string   `json:"fingerprint,omitempty"`
-	PublicKey   string   `json:"public_key,omitempty"`
-	ShortID     string   `json:"short_id,omitempty"`
-	ALPN        []string `json:"alpn,omitempty"`
+	Address     string          `json:"address"`
+	Port        int             `json:"port"`
+	UUID        string          `json:"uuid,omitempty"`
+	Name        string          `json:"name"`
+	IPs         []string        `json:"ips"`
+	Outbound    json.RawMessage `json:"outbound,omitempty"`
+	Security    string          `json:"security,omitempty"`
+	Network     string          `json:"network,omitempty"`
+	Flow        string          `json:"flow,omitempty"`
+	SNI         string          `json:"sni,omitempty"`
+	Fingerprint string          `json:"fingerprint,omitempty"`
+	PublicKey   string          `json:"public_key,omitempty"`
+	ShortID     string          `json:"short_id,omitempty"`
+	ALPN        []string        `json:"alpn,omitempty"`
 }
 
 // ServerIPs returns every non-empty IP across servers, de-duplicated and
PATCH_EOF
```

- [ ] **Step 4: Add the outbound helpers**

Create `server/internal/vpnconfig/outbound.go`:

```go
package vpnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// DecodeOutbound parses a stored outbound. Numbers stay json.Number, so a
// value the subscription wrote goes back out exactly as it came in.
func DecodeOutbound(raw json.RawMessage) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var ob map[string]interface{}
	if err := dec.Decode(&ob); err != nil {
		return nil, err
	}
	if ob == nil {
		return nil, errors.New("outbound is not an object")
	}
	return ob, nil
}

// OutboundTarget returns the object that holds an outbound's address and
// port: settings.vnext[0] for vless and vmess, settings.servers[0] for trojan
// and shadowsocks, and settings itself for the flat form those four also
// accept, and for hysteria, which has no other. Nil when there is no such
// object.
func OutboundTarget(ob map[string]interface{}) map[string]interface{} {
	settings, _ := ob["settings"].(map[string]interface{})
	if settings == nil {
		return nil
	}
	key := ""
	switch ob["protocol"] {
	case "vless", "vmess":
		key = "vnext"
	case "trojan", "shadowsocks":
		key = "servers"
	}
	if list, ok := settings[key].([]interface{}); ok && len(list) > 0 {
		first, _ := list[0].(map[string]interface{})
		return first
	}
	return settings
}

// Label names a server's protocol for a list: "vless·reality",
// "vless·ws·tls", "trojan·tls", "ss", "hysteria2". A record without an
// outbound is a legacy VLESS one, which the generators build as TLS when it
// names no security.
func (s Server) Label() string {
	if len(s.Outbound) == 0 {
		security := s.Security
		if security == "" {
			security = "tls"
		}
		return protocolLabel("vless", s.Network, security)
	}
	var ob struct {
		Protocol       string `json:"protocol"`
		StreamSettings struct {
			Network  string `json:"network"`
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil {
		return "?"
	}
	return protocolLabel(ob.Protocol, ob.StreamSettings.Network, ob.StreamSettings.Security)
}

func protocolLabel(protocol, network, security string) string {
	switch protocol {
	case "shadowsocks":
		return "ss"
	case "hysteria":
		return "hysteria2"
	}
	parts := []string{protocol}
	if network != "" && network != "tcp" && network != "raw" {
		parts = append(parts, network)
	}
	if security != "" && security != "none" {
		parts = append(parts, security)
	}
	return strings.Join(parts, "·")
}
```

- [ ] **Step 5: Run the tests**

Run: `cd server && go test -count=1 ./internal/vpnconfig/ ./internal/webapi/ ./internal/service/`
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/outbound.go server/internal/vpnconfig/outbound_test.go
git commit -m "feat(vpnconfig): store a server's Xray outbound and label its protocol"
```

---

### Task 2: Decoder foundations in both languages

Spec 6.2 (percent-decoding), 6.1 (base64), 6.6 (names, placeholders), 6.7 (skip reasons). The byte rules both decoders share, each with the same unit inputs on both sides. `lib/subscription.sh` enters `router/files.manifest` now: `server/internal/updater/manifest_test.go` fails for any shipped file the manifest does not list.

**Files:**
- Create: `server/internal/subscription/subscription.go`, `names.go`, `text.go`
- Test: `server/internal/subscription/subscription_test.go`
- Create: `router/opt/vpn-director/lib/subscription.sh` (its first part; Tasks 3–5 add the rest)
- Test: `router/test/unit/subscription.bats`
- Modify: `router/files.manifest`

**Interfaces:**
- Produces (Go, package `subscription`): `const ReasonUnsupported, ReasonComposite, ReasonInvalid, ReasonPlaceholder`; `var ErrEmpty, ErrInvalidJSON, ErrUnrecognized`; `type Skip struct{ Name, Reason, Detail string }`; `type Result struct{ Total int; Servers []vpnconfig.Server; Skipped []Skip }`; unexported `entry{name, address string; port int; outbound map[string]interface{}}`, `skipError`, `unsupported(detail)`, `invalid(detail)`, `composite(detail)`, `(*Result).add(n int, e entry, err error)`, `cleanName`, `isPlaceholder`, `asciiSpace`, `unescape(s string, plus bool) (string, error)` with `errBadEscape`/`errNUL`, `unescapeName`, `decodeBase64(s) (string, bool)`, `decodeJSON`, `decodeObject`, `splitList`, `truthy`, `prune`.
- Produces (shell): `_SUB_SPACE`; `_sub_unescape <value> <plus>` (sets `_SUB_U`, returns 1 on a bad escape or `%00`); `_sub_b64 <text>` (prints the bytes); `_sub_trim <text>`; `_sub_clean_names` (stdin `<decode>\t<raw>` lines, stdout one JSON string per line).

- [ ] **Step 1: Write the failing Go tests**

Create `server/internal/subscription/subscription_test.go` (Task 6 adds summary and resolution tests to it):

```go
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
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 ./internal/subscription/`
Expected: FAIL — `no non-test Go files` or `undefined: cleanName`.

- [ ] **Step 3: Implement the Go foundations**

Create `server/internal/subscription/subscription.go`:

```go
// Package subscription reads a VPN subscription — share links, base64 or
// plain, or Xray JSON — into servers that carry a ready Xray outbound.
//
// import_server_list.sh does the same in lib/subscription.sh, and both answer
// to the cases in testdata/subscription at the repository root: an entry
// either side reads differently fails one of the two test suites.
package subscription

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Reasons an entry is skipped.
const (
	ReasonUnsupported = "unsupported"
	ReasonComposite   = "composite"
	ReasonInvalid     = "invalid"
	ReasonPlaceholder = "placeholder"
)

// Errors for a body Decode cannot read at all.
var (
	ErrEmpty        = errors.New("empty subscription")
	ErrInvalidJSON  = errors.New("invalid JSON subscription")
	ErrUnrecognized = errors.New("unrecognized subscription format")
)

// Skip is an entry left out of the result.
type Skip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Result is a decoded subscription. Total counts its entries - link lines or
// JSON configs - and every one of them is either in Servers, with IPs still
// empty, or in Skipped.
type Result struct {
	Total   int
	Servers []vpnconfig.Server
	Skipped []Skip
}

// entry is one converted server, before the placeholder test and naming.
type entry struct {
	name     string // cleaned; empty when the source has none
	address  string // brackets removed
	port     int
	outbound map[string]interface{}
}

// skipError says why an entry is skipped.
type skipError struct {
	reason string
	detail string
}

func (e *skipError) Error() string { return e.reason + ": " + e.detail }

func unsupported(detail string) error { return &skipError{ReasonUnsupported, detail} }
func invalid(detail string) error     { return &skipError{ReasonInvalid, detail} }
func composite(detail string) error   { return &skipError{ReasonComposite, detail} }

// add records the entry at position n (1-based): a server, or a skip when
// err is set or the address is a placeholder. The placeholder test comes last,
// on an entry that passed every other check (spec 6.6).
func (r *Result) add(n int, e entry, err error) {
	if err == nil && isPlaceholder(e.address) {
		err = &skipError{ReasonPlaceholder, e.address}
	}
	if err != nil {
		var se *skipError
		if !errors.As(err, &se) {
			se = &skipError{ReasonInvalid, err.Error()}
		}
		name := e.name
		if name == "" {
			name = "#" + strconv.Itoa(n)
		}
		r.Skipped = append(r.Skipped, Skip{Name: name, Reason: se.reason, Detail: se.detail})
		return
	}
	name := e.name
	if name == "" {
		name = e.address
	}
	raw, _ := json.Marshal(e.outbound)
	r.Servers = append(r.Servers, vpnconfig.Server{
		Name:     name,
		Address:  e.address,
		Port:     e.port,
		Outbound: raw,
	})
}
```

Create `server/internal/subscription/names.go`:

```go
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
```

Create `server/internal/subscription/text.go`:

```go
package subscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
```

- [ ] **Step 4: Run the Go tests**

Run: `cd server && go vet ./internal/subscription/ && go test -count=1 ./internal/subscription/`
Expected: `ok`.

- [ ] **Step 5: Write the failing shell tests**

Create `router/test/unit/subscription.bats` (Task 3 adds the shared-case tests to it):

```bash
#!/usr/bin/env bats
load '../test_helper'

# The cases testdata/subscription holds are the Go importer's as well
# (server/internal/subscription/fixtures_test.go): an entry the two decoders
# read differently fails one of the two suites.
FIXTURES="$PROJECT_ROOT/../testdata/subscription"

setup() {
    source "$LIB_DIR/subscription.sh"
}

# Entware's jq is built without oniguruma: a regex builtin works on a
# workstation and fails on the router. _sub_clean_names is gawk, where sub and
# gsub are native.
@test "lib/subscription.sh: its jq uses no regex builtin" {
    run bash -c "sed '/^_sub_clean_names() {/,/^}/d' '$LIB_DIR/subscription.sh' |
        grep -nE '(^|[^a-zA-Z_])(test|match|capture|scan|splits|sub|gsub)\\(|split\\([^)]*;'"
    assert_failure
}

@test "_sub_unescape: strict %XX, + as a space only in a query" {
    _sub_unescape 'h2%2Chttp%2F1.1' 1
    [ "$_SUB_U" = "h2,http/1.1" ]
    _sub_unescape 'a+b%2B' 1
    [ "$_SUB_U" = "a b+" ]
    _sub_unescape 'a+b%2B' 0
    [ "$_SUB_U" = "a+b+" ]
    _sub_unescape 'back\slash%0A' 0
    [ "$_SUB_U" = $'back\\slash\n' ]
    run _sub_unescape '50%' 1
    assert_failure
    run _sub_unescape 'x%2y' 1
    assert_failure
    run _sub_unescape '%%41' 1
    assert_failure
    run _sub_unescape 'a%00b' 1
    assert_failure
}

@test "_sub_b64: either alphabet, padding optional, whitespace ignored" {
    [ "$(_sub_b64 'Pz8/Pw==')" = "????" ]
    [ "$(_sub_b64 'Pz8_Pw')" = "????" ]
    [ "$(_sub_b64 'Pj4-Pz8_')" = ">>>???" ]
    [ "$(_sub_b64 $'Pj4+\nPz8/\r\n')" = ">>>???" ]
    [ "$(_sub_b64 'YQBi')" = "ab" ]
    run _sub_b64 'a'
    assert_failure
    run _sub_b64 'ab=c'
    assert_failure
    run _sub_b64 '@@@'
    assert_failure
}

# The byte machine of cleanName (server/internal/subscription/names.go).
@test "_sub_clean_names: keeps two-byte letters, drops the rest, as the Go side does" {
    run _sub_clean_names <<< $'0\t\xF0\x9F\x87\xB3\xF0\x9F\x87\xB1 Амстердам, Нидерланды
0\t  Türkiye   Istanbul , 
0\tΕλλάδα
0\t\xC3|evil
0\t\xE2AB
0\t\xF0ABC
1\t100%25%20off%2%zz+plus%00!'
    assert_success
    assert_output '"Амстердам, Нидерланды"
"Türkiye Istanbul"
"Ελλάδα"
"evil"
"AB"
"ABC"
"100 off2zz plus!"'
}
```

- [ ] **Step 6: Run them to see them fail**

Run: `bats router/test/unit/subscription.bats`
Expected: FAIL — `lib/subscription.sh: No such file or directory`.

- [ ] **Step 7: Implement the shell foundations**

Create `router/opt/vpn-director/lib/subscription.sh`:

```bash
#!/usr/bin/env bash
###############################################################################
# lib/subscription.sh - read a VPN subscription into servers that carry a
# ready Xray outbound: share links (vless, vmess, trojan, ss, hysteria2/hy2),
# base64 or plain, or Xray JSON.
#
# The Go importer does the same in server/internal/subscription, and both
# answer to the cases in testdata/subscription at the repository root:
# router/test/unit/subscription.bats runs them here, the Go tests there. A rule
# changed on one side only fails one of the two suites.
#
# subscription_decode reads the body on stdin and prints
#   {"total": N, "servers": [{name, address, port, outbound}],
#    "skipped": [{name, reason, detail}]}
# For a body it cannot read at all it prints the reason on stderr - "empty
# subscription", "invalid JSON subscription" or "unrecognized subscription
# format" - and returns 1.
#
# Entware's jq is built without oniguruma, so the jq here never uses test,
# match, capture, scan, splits, sub, gsub or a two-argument split: bash and
# gawk do the pattern work.
###############################################################################

_SUB_SPACE=$' \t\r\n\v\f'

# _sub_unescape <value> <plus>: decodes %XX into _SUB_U; + is a space when plus
# is 1. A % without two hex digits after it, or %00, returns 1 - unescape in
# server/internal/subscription/text.go.
_sub_unescape() {
    local s=$1 bs='\x'
    if [[ $2 == 1 ]]; then
        s=${s//+/ }
    fi
    if [[ $s != *%* ]]; then
        _SUB_U=$s
        return 0
    fi
    local rest=${s//%[0-9A-Fa-f][0-9A-Fa-f]/}
    if [[ $rest == *%* || $s == *%00* ]]; then
        return 1
    fi
    s=${s//\\/\\\\}
    printf -v _SUB_U '%b' "${s//%/$bs}"
}

# _sub_b64 <text>: decodes base64 of either alphabet, padded or not, ignoring
# whitespace; NUL bytes are dropped. Returns 1 for anything else.
_sub_b64() {
    local s=$1 pad
    s=${s//[$_SUB_SPACE]/}
    s=${s//-/+}
    s=${s//_/\/}
    while [[ $s == *= ]]; do
        s=${s%=}
    done
    [[ -n $s && $s =~ ^[A-Za-z0-9+/]+$ ]] || return 1
    case $(( ${#s} % 4 )) in
        1) return 1 ;;
        2) pad='==' ;;
        3) pad='=' ;;
        *) pad='' ;;
    esac
    printf '%s%s' "$s" "$pad" | base64 -d 2>/dev/null | tr -d '\000'
}

# _sub_trim <text>: prints text without ASCII whitespace at either end.
_sub_trim() {
    local s=$1
    s=${s#"${s%%[!$_SUB_SPACE]*}"}
    printf '%s' "${s%"${s##*[!$_SUB_SPACE]}"}"
}

# _sub_clean_names: "<decode>\t<raw>" lines in, one JSON string per line out.
# The filter of cleanName in server/internal/subscription/names.go, byte for
# byte under LC_ALL=C: ASCII letters, digits, space and .,;:!?()-, and every
# two-byte UTF-8 character; a lead byte without its continuation bytes goes
# alone. With decode 1 the raw name is first percent-decoded leniently, + as
# a space and %00 as nothing. What it keeps holds no quote and no backslash,
# so a pair of quotes around it is a JSON string.
_sub_clean_names() {
    LC_ALL=C gawk '
        function cont(s, i, k,   j, b) {
            for (j = 1; j <= k; j++) {
                b = ord[substr(s, i + j, 1)] + 0
                if (b < 128 || b > 191) return 0
            }
            return 1
        }
        function hexval(h) { return index("0123456789abcdef", tolower(h)) - 1 }
        BEGIN { for (i = 1; i < 256; i++) ord[sprintf("%c", i)] = i }
        {
            s = substr($0, 3); n = length(s)
            if (substr($0, 1, 1) == "1") {
                t = ""
                for (i = 1; i <= n; i++) {
                    c = substr(s, i, 1)
                    if (c == "%" && i + 2 <= n && substr(s, i + 1, 2) ~ /^[0-9A-Fa-f][0-9A-Fa-f]$/) {
                        v = hexval(substr(s, i + 1, 1)) * 16 + hexval(substr(s, i + 2, 1))
                        if (v != 0) t = t sprintf("%c", v)
                        i += 2
                    } else if (c == "+") t = t " "
                    else t = t c
                }
                s = t; n = length(s)
            }
            out = ""
            for (i = 1; i <= n; ) {
                c = substr(s, i, 1); b = ord[c] + 0
                if (c ~ /[A-Za-z0-9 .,;:!?()-]/) { out = out c; i++ }
                else if (b >= 194 && b <= 223 && cont(s, i, 1)) { out = out substr(s, i, 2); i += 2 }
                else if (b >= 224 && b <= 239 && cont(s, i, 2)) i += 3
                else if (b >= 240 && b <= 244 && cont(s, i, 3)) i += 4
                else i++
            }
            gsub(/ +/, " ", out); sub(/^[ ,]+/, "", out); sub(/[ ,]+$/, "", out)
            printf "\"%s\"\n", out
        }'
}
```

- [ ] **Step 8: List the library in the manifest**

```bash
git apply <<'PATCH_EOF'
--- a/router/files.manifest
+++ b/router/files.manifest
@@ -24,6 +24,7 @@
 common   router/opt/vpn-director/lib/tunnel.sh
 common   router/opt/vpn-director/lib/tproxy.sh
 common   router/opt/vpn-director/lib/xrayconf.sh
+common   router/opt/vpn-director/lib/subscription.sh
 common   router/opt/vpn-director/lib/send-email.sh
 common   router/opt/etc/xray/config.json.template
 common   router/opt/etc/init.d/S99vpn-director
PATCH_EOF
```

- [ ] **Step 9: Run the tests**

Run: `bats router/test/unit/subscription.bats && (cd server && go test -count=1 ./internal/subscription/ ./internal/updater/)`
Expected: 4 bats tests pass; both Go packages `ok`.

- [ ] **Step 10: Commit**

```bash
git add server/internal/subscription router/opt/vpn-director/lib/subscription.sh router/test/unit/subscription.bats router/files.manifest
git commit -m "feat(subscription): shared decoding rules for the Go and shell importers"
```

---

### Task 3: Share-link grammar, VLESS and Trojan, and the shared cases

Spec 6.1 (containers and the link list), 6.2 (grammar), 6.3 (stream settings), 6.4 (vless, trojan), 6.6 (placeholders), 13.1 (shared cases). From this task on, `testdata/subscription/` is the contract: both suites run every case in it.

**Files:**
- Create: `server/internal/subscription/decode.go`, `links.go`, `vless.go`, `trojan.go`
- Test: `server/internal/subscription/fixtures_test.go`
- Modify: `router/opt/vpn-director/lib/subscription.sh`, `router/test/unit/subscription.bats`
- Create: `testdata/subscription/` — `links-vless`, `base64-std`, `base64-urlsafe`, `plain-bom`, `error-empty`, `error-clash`, `error-html`, `error-base64-garbage`, `error-not-base64` (`.in` and `.want.json` each)

**Interfaces:**
- Consumes: Task 1 (`vpnconfig.Server.Outbound`), Task 2 (all of it).
- Produces (Go): `func Decode(body string) (Result, error)`; unexported `decodeLinks`, `splitScheme`, `parseLink(scheme, rest) (entry, error)`, `link{name, userinfo, host, port string; hasPort bool; query string}`, `splitLink`, `splitHostPort`, `parsePort`, `params`, `parseQuery`, `(link).portAndQuery()`, `streamSettings(p params, defaultSecurity string)`, `orDefault`, `parseVLESS`, `parseTrojan`.
- Produces (shell): `subscription_decode` (stdin body; stdout Result JSON; stderr + return 1 for a body it cannot read); the globals `_SUB_Q`, `_SUB_V`, `_SUB_RECORDS`, `_SUB_RAWNAMES`, `_SUB_VARS`, `_SUB_KEYS`, `_SUB_JQ_LIB`; `_sub_skip`, `_sub_query`, `_sub_hostport`, `_sub_port`, `_sub_link`, `_sub_port_query`, `_sub_truthy`, `_sub_stream`, `_sub_record <jq outbound>`, `_sub_vless`, `_sub_trojan`, `_sub_entry`, `_sub_links`, `_sub_result`.

- [ ] **Step 1: Add the shared cases**

The `.in` files are bodies as a server sends them; the `.want.json` files hold what both decoders must answer — the error, or `total`, the servers and the skips by name and reason. Two cases need bytes a heredoc cannot carry, so they are written with `printf`:

```bash
mkdir -p testdata/subscription
printf '\xef\xbb\xbf\n  vless://77777777-7777-4777-8777-777777777777@bom.example.com:443?security=none#BOM%%20first\r\n\n' > testdata/subscription/plain-bom.in
printf '  \n\t \r\n' > testdata/subscription/error-empty.in
```

The rest:

```bash
cat > testdata/subscription/links-vless.in <<'FIXTURE_EOF'
vless://11111111-1111-4111-8111-111111111111@nl1.example.com:443?security=reality&encryption=none&fp=firefox&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=www.example.org&pbk=kcUhHqbEFOlkxsNrckk60lJMhNAOI73kTqDtefRYhbY&sid=0123abcd#%F0%9F%87%B3%F0%9F%87%B1%20%D0%90%D0%BC%D1%81%D1%82%D0%B5%D1%80%D0%B4%D0%B0%D0%BC%2C%20%D0%9D%D0%B8%D0%B4%D0%B5%D1%80%D0%BB%D0%B0%D0%BD%D0%B4%D1%8B
vless://11111111-1111-4111-8111-111111111111@ws1.example.com:443?encryption=none&type=ws&fp=firefox&sni=cdn.example.net&flow=&host=cdn.example.net&path=%2Fws%3Fed%3D2048&security=tls#Frankfurt%2C%20Germany%2C%20WS
vless://22222222-2222-4222-8222-222222222222@grpc.example.com:8443?type=grpc&serviceName=svc-name&mode=multi&authority=grpc.example.com&security=tls&sni=grpc.example.com&alpn=h2#gRPC%20Multi
vless://22222222-2222-4222-8222-222222222222@xh.example.com:443?type=xhttp&path=%2Fxh&host=&mode=stream-one&security=reality&sni=www.example.org&fp=chrome&pbk=-kuQWnGk-HAw-8yoIX34SHIMx3KrtR-1SBqBTOc1mfU&sid=&spx=%2F&extra=%7B%22xmux%22%3A%7B%22maxConcurrency%22%3A%2216-32%22%7D%2C%22xPaddingBytes%22%3A%22100-1000%22%2C%22headers%22%3A%7B%7D%7D#XHTTP%20REALITY
vless://22222222-2222-4222-8222-222222222222@hu.example.com:80?type=httpupgrade&path=%2Fup&host=hu.example.com&security=none#HTTPUpgrade
vless://22222222-2222-4222-8222-222222222222@sh.example.com:2083?type=splithttp&path=%2Fsplit&security=tls&alpn=h2%2C%20http%2F1.1&pcs=d39e9a079f6f5254fc05926241f9c9cdce0ddfc5d39b949d248b84b9a3d14faa&vcn=sh.example.com#SplitHTTP%20pinned
vless://33333333-3333-4333-8333-333333333333@[2001:db8::10]:443?type=raw&security=none&encryption=none#IPv6%20raw
FIXTURE_EOF
```

```bash
cat > testdata/subscription/base64-std.in <<'FIXTURE_EOF'
dmxlc3M6Ly82NjY2NjY2Ni02NjY2LTQ2NjYtODY2Ni02NjY2NjY2NjY2NjZAYjY0LmV4YW1wbGUu
Y29tOjQ0Mz90eXBlPXRjcCZzZWN1cml0eT1yZWFsaXR5JnNuaT13d3cuZXhhbXBsZS5vcmcmZnA9
ZmlyZWZveCZwYms9aGhvVDZKRGw4eUl4Q3hRa3l0bS13OFRvTnk2RFRzbjN0OU5qYWFveS1kRSZz
aWQ9MDEmZmxvdz14dGxzLXJwcngtdmlzaW9uI0Jhc2U2NCUyMG9uZQp0cm9qYW46Ly9wJTNFJTNG
JTNFJTNGQGI2NHQuZXhhbXBsZS5jb206NDQzP3NuaT1iNjR0LmV4YW1wbGUuY29tI0Jhc2U2NCUy
MHR3bw==
FIXTURE_EOF
```

```bash
cat > testdata/subscription/base64-urlsafe.in <<'FIXTURE_EOF'
dHJvamFuOi8vcGFzcy13b3JkQHUuZXhhbXBsZS5jb206NDQzP3NuaT11LmV4YW1wbGUuY29tI1VSTC1zYWZlID8_Pz4-Pg
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-clash.in <<'FIXTURE_EOF'
mixed-port: 7890
proxies:
  - name: Server
    type: vless
    server: 198.51.100.40
proxy-groups:
  - name: Auto
    type: url-test
    url: http://www.gstatic.com/generate_204
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-html.in <<'FIXTURE_EOF'
<!doctype html>
<html><head><link rel="icon" href="https://example.com/favicon.ico"></head>
<body>Open this link in your VPN client</body></html>
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-base64-garbage.in <<'FIXTURE_EOF'
not base64 at all
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-not-base64.in <<'FIXTURE_EOF'
@@@ ### !!!
FIXTURE_EOF
```

```bash
cat > testdata/subscription/links-vless.want.json <<'FIXTURE_EOF'
{
  "total": 7,
  "servers": [
    {"address":"nl1.example.com","name":"Амстердам, Нидерланды","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"nl1.example.com","port":443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"11111111-1111-4111-8111-111111111111"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","publicKey":"kcUhHqbEFOlkxsNrckk60lJMhNAOI73kTqDtefRYhbY","serverName":"www.example.org","shortId":"0123abcd"},"security":"reality"}},"port":443},
    {"address":"ws1.example.com","name":"Frankfurt, Germany, WS","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"ws1.example.com","port":443,"users":[{"encryption":"none","id":"11111111-1111-4111-8111-111111111111"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"fingerprint":"firefox","serverName":"cdn.example.net"},"wsSettings":{"host":"cdn.example.net","path":"/ws?ed=2048"}}},"port":443},
    {"address":"grpc.example.com","name":"gRPC Multi","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"grpc.example.com","port":8443,"users":[{"encryption":"none","id":"22222222-2222-4222-8222-222222222222"}]}]},"streamSettings":{"grpcSettings":{"authority":"grpc.example.com","multiMode":true,"serviceName":"svc-name"},"network":"grpc","security":"tls","tlsSettings":{"alpn":["h2"],"serverName":"grpc.example.com"}}},"port":8443},
    {"address":"xh.example.com","name":"XHTTP REALITY","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"xh.example.com","port":443,"users":[{"encryption":"none","id":"22222222-2222-4222-8222-222222222222"}]}]},"streamSettings":{"network":"xhttp","realitySettings":{"fingerprint":"chrome","publicKey":"-kuQWnGk-HAw-8yoIX34SHIMx3KrtR-1SBqBTOc1mfU","serverName":"www.example.org","spiderX":"/"},"security":"reality","xhttpSettings":{"extra":{"xPaddingBytes":"100-1000","xmux":{"maxConcurrency":"16-32"}},"mode":"stream-one","path":"/xh"}}},"port":443},
    {"address":"hu.example.com","name":"HTTPUpgrade","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"hu.example.com","port":80,"users":[{"encryption":"none","id":"22222222-2222-4222-8222-222222222222"}]}]},"streamSettings":{"httpupgradeSettings":{"host":"hu.example.com","path":"/up"},"network":"httpupgrade","security":"none"}},"port":80},
    {"address":"sh.example.com","name":"SplitHTTP pinned","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"sh.example.com","port":2083,"users":[{"encryption":"none","id":"22222222-2222-4222-8222-222222222222"}]}]},"streamSettings":{"network":"xhttp","security":"tls","tlsSettings":{"alpn":["h2","http/1.1"],"pinnedPeerCertSha256":"d39e9a079f6f5254fc05926241f9c9cdce0ddfc5d39b949d248b84b9a3d14faa","verifyPeerCertByName":"sh.example.com"},"xhttpSettings":{"path":"/split"}}},"port":2083},
    {"address":"2001:db8::10","name":"IPv6 raw","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"2001:db8::10","port":443,"users":[{"encryption":"none","id":"33333333-3333-4333-8333-333333333333"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443}
  ],
  "skipped": []
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/base64-std.want.json <<'FIXTURE_EOF'
{
  "total": 2,
  "servers": [
    {"address":"b64.example.com","name":"Base64 one","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"b64.example.com","port":443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"66666666-6666-4666-8666-666666666666"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","publicKey":"hhoT6JDl8yIxCxQkytm-w8ToNy6DTsn3t9Njaaoy-dE","serverName":"www.example.org","shortId":"01"},"security":"reality"}},"port":443},
    {"address":"b64t.example.com","name":"Base64 two","outbound":{"protocol":"trojan","settings":{"servers":[{"address":"b64t.example.com","password":"p>?>?","port":443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"b64t.example.com"}}},"port":443}
  ],
  "skipped": []
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/base64-urlsafe.want.json <<'FIXTURE_EOF'
{
  "total": 1,
  "servers": [
    {"address":"u.example.com","name":"URL-safe ???","outbound":{"protocol":"trojan","settings":{"servers":[{"address":"u.example.com","password":"pass-word","port":443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"u.example.com"}}},"port":443}
  ],
  "skipped": []
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/plain-bom.want.json <<'FIXTURE_EOF'
{
  "total": 1,
  "servers": [
    {"address":"bom.example.com","name":"BOM first","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"bom.example.com","port":443,"users":[{"encryption":"none","id":"77777777-7777-4777-8777-777777777777"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443}
  ],
  "skipped": []
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-empty.want.json <<'FIXTURE_EOF'
{"error": "empty subscription"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-clash.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-html.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-base64-garbage.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-not-base64.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```


- [ ] **Step 2: Write the failing Go test that runs every case**

Create `server/internal/subscription/fixtures_test.go`:

```go
package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureDir holds the cases lib/subscription.sh answers to as well
// (router/test/unit/subscription.bats): one decoder reading an entry
// differently from the other fails one of the two suites.
const fixtureDir = "../../../testdata/subscription"

// fixtureResult is what a .want.json holds: the error, or the servers and
// the skips by name and reason. A skip's detail is prose and is not compared.
func fixtureResult(t *testing.T, body string) interface{} {
	t.Helper()
	r, err := Decode(body)
	var v interface{}
	if err != nil {
		v = map[string]interface{}{"error": err.Error()}
	} else {
		servers := []interface{}{}
		for _, s := range r.Servers {
			servers = append(servers, map[string]interface{}{
				"name": s.Name, "address": s.Address, "port": s.Port, "outbound": s.Outbound,
			})
		}
		skipped := []interface{}{}
		for _, s := range r.Skipped {
			skipped = append(skipped, map[string]interface{}{"name": s.Name, "reason": s.Reason})
		}
		v = map[string]interface{}{"total": r.Total, "servers": servers, "skipped": skipped}
	}
	// Through JSON and back, so numbers and nesting compare the way the
	// .want.json reads.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecode_Fixtures(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(fixtureDir, "*.in"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures in %s: %v", fixtureDir, err)
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".in")
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			wantRaw, err := os.ReadFile(strings.TrimSuffix(in, ".in") + ".want.json")
			if err != nil {
				t.Fatal(err)
			}
			var want interface{}
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("%s.want.json: %v", name, err)
			}
			got := fixtureResult(t, string(body))
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("decoded differently from %s.want.json; got:\n%s", name, gotJSON)
			}
		})
	}
}
```

- [ ] **Step 3: Run it to see it fail**

Run: `cd server && go test -count=1 ./internal/subscription/`
Expected: FAIL to compile — `undefined: Decode`.

- [ ] **Step 4: Implement the Go link decoder**

Create `server/internal/subscription/decode.go` (Task 5 adds Xray JSON):

```go
package subscription

import "strings"

// Decode reads a subscription body (spec 6.1): a plain link list when it holds
// "://", and otherwise base64 of a link list. Its error is ErrEmpty or
// ErrUnrecognized, for a body it cannot read at all; a readable body whose
// entries were all skipped is a Result without servers.
func Decode(body string) (Result, error) {
	// bash drops NUL bytes from what it reads; so does this side.
	body = strings.ReplaceAll(body, "\x00", "")
	body = strings.TrimPrefix(body, "\ufeff")
	body = strings.Trim(body, asciiSpace)
	switch {
	case body == "":
		return Result{}, ErrEmpty
	case strings.Contains(body, "://"):
		return decodeLinks(body)
	}
	text, ok := decodeBase64(body)
	if !ok {
		return Result{}, ErrUnrecognized
	}
	return decodeLinks(text)
}
```

Create `server/internal/subscription/links.go` (Task 4 extends `parseLink`):

```go
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
	case "trojan":
		return parseTrojan(rest)
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
```

Create `server/internal/subscription/vless.go`:

```go
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
```

Create `server/internal/subscription/trojan.go`:

```go
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
```


- [ ] **Step 5: Run the Go tests**

Run: `cd server && go vet ./internal/subscription/ && go test -count=1 ./internal/subscription/`
Expected: `ok` — every case in `testdata/subscription/` decodes as its `.want.json` says.

- [ ] **Step 6: Write the failing shell test that runs every case**

```bash
git apply <<'PATCH_EOF'
--- a/router/test/unit/subscription.bats
+++ b/router/test/unit/subscription.bats
@@ -10,6 +10,47 @@
     source "$LIB_DIR/subscription.sh"
 }
 
+# decode_fixture <in>: what a .want.json holds - the error, or the total, the
+# servers and the skips by name and reason. A skip's detail is prose and is
+# not compared.
+decode_fixture() {
+    local out rc=0
+    out=$(subscription_decode < "$1" 2>"$BATS_TEST_TMPDIR/err") || rc=$?
+    if (( rc != 0 )); then
+        jq -Sn --arg e "$(cat "$BATS_TEST_TMPDIR/err")" '{error: $e}'
+    else
+        printf '%s' "$out" | jq -S '{total, servers, skipped: [.skipped[] | {name, reason}]}'
+    fi
+}
+
+check_fixtures() {
+    local in want got failed=0
+    for in in "$FIXTURES"/*.in; do
+        want=$(jq -S . "${in%.in}.want.json")
+        got=$(decode_fixture "$in")
+        if [[ $got != "$want" ]]; then
+            printf '== %s decoded differently from its .want.json:\n' "${in##*/}"
+            diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") || true
+            failed=1
+        fi
+    done
+    return "$failed"
+}
+
+@test "subscription_decode: every shared case decodes as its .want.json says" {
+    run check_fixtures
+    assert_success
+}
+
+# The routers run with no UTF-8 locale or with one; a byte-wise name filter
+# and ASCII patterns must not care which.
+@test "subscription_decode: the cases decode the same under a UTF-8 locale" {
+    local utf8
+    utf8=$(locale -a 2>/dev/null | grep -i -m1 -E '^(C|en_US)\.utf-?8$') || skip "no UTF-8 locale here"
+    LC_ALL=$utf8 run check_fixtures
+    assert_success
+}
+
 # Entware's jq is built without oniguruma: a regex builtin works on a
 # workstation and fails on the router. _sub_clean_names is gawk, where sub and
 # gsub are native.
PATCH_EOF
```

- [ ] **Step 7: Run it to see it fail**

Run: `bats router/test/unit/subscription.bats`
Expected: the two shared-case tests FAIL — `subscription_decode: command not found`.

- [ ] **Step 8: Implement the shell link decoder**

The patch inserts the globals after `_SUB_SPACE` and appends the grammar, the stream settings, the vless and trojan converters, the link list, the assembly and `subscription_decode`. Names are cleaned in one gawk pass for the whole list; `_sub_result` does the placeholder test in jq.

```bash
git apply <<'PATCH_EOF'
--- a/router/opt/vpn-director/lib/subscription.sh
+++ b/router/opt/vpn-director/lib/subscription.sh
@@ -23,6 +23,43 @@
 
 _SUB_SPACE=$' \t\r\n\v\f'
 
+declare -gA _SUB_Q=() _SUB_V=()
+declare -ga _SUB_RECORDS=() _SUB_RAWNAMES=()
+
+# The jq variables a record is built from. jq refuses a program that names a
+# variable it was not given, so every one of them is always passed.
+_SUB_VARS="net sec sni fp alpn pcs vcn pbk sid spx pqv path host serviceName authority mode extra
+flow user enc addr method password pin obfs obfspw"
+
+# The query keys a converter reads; the rest of a query is never stored.
+_SUB_KEYS=" type security encryption flow sni fp alpn pbk sid spx pqv host path
+headerType serviceName mode authority extra pcs vcn allowInsecure insecure
+plugin obfs obfs-password pinSHA256 "
+
+# jq shared by every converter and by the final assembly.
+# shellcheck disable=SC2016  # $vars are jq's, set with --arg
+_SUB_JQ_LIB='
+def trimsp: if startswith(" ") then .[1:] | trimsp elif endswith(" ") then .[:-1] | trimsp else . end;
+def list: split(",") | map(trimsp);
+def prune: walk(if type == "object" then with_entries(select(.value != "" and .value != [] and .value != {}))
+                elif type == "array" then map(select(. != "" and . != [] and . != {}))
+                else . end);
+def stream:
+  {network: $net, security: $sec}
+  + (if $sec == "tls" then {tlsSettings: {serverName: $sni, fingerprint: $fp, alpn: ($alpn | list),
+                                          pinnedPeerCertSha256: $pcs, verifyPeerCertByName: $vcn}}
+     elif $sec == "reality" then {realitySettings: {serverName: $sni, fingerprint: $fp, publicKey: $pbk,
+                                                    shortId: $sid, spiderX: $spx, mldsa65Verify: $pqv}}
+     else {} end)
+  + (if $net == "ws" then {wsSettings: {path: $path, host: $host}}
+     elif $net == "httpupgrade" then {httpupgradeSettings: {path: $path, host: $host}}
+     elif $net == "grpc" then {grpcSettings: ({serviceName: $serviceName, authority: $authority}
+                                              + (if $mode == "multi" then {multiMode: true} else {} end))}
+     elif $net == "xhttp" then {xhttpSettings: ({path: $path, host: $host, mode: $mode}
+                                                + (if $extra == "" then {} else {extra: ($extra | fromjson)} end))}
+     else {} end);
+'
+
 # _sub_unescape <value> <plus>: decodes %XX into _SUB_U; + is a space when plus
 # is 1. A % without two hex digits after it, or %00, returns 1 - unescape in
 # server/internal/subscription/text.go.
@@ -116,3 +153,305 @@
             printf "\"%s\"\n", out
         }'
 }
+
+# _sub_skip <reason> <detail>: marks the entry skipped.
+_sub_skip() {
+    _SUB_REASON=$1
+    _SUB_DETAIL=$2
+}
+
+# _sub_query <query>: fills _SUB_Q with the keys a converter reads, the first
+# value of a key winning; returns 1 on a bad escape anywhere in the query.
+_sub_query() {
+    _SUB_Q=()
+    local q=$1 part key val
+    while [[ -n $q ]]; do
+        part=${q%%&*}
+        if [[ $part == "$q" ]]; then
+            q=""
+        else
+            q=${q#*&}
+        fi
+        [[ -n $part ]] || continue
+        key=${part%%=*}
+        val=""
+        if [[ $part == *=* ]]; then
+            val=${part#*=}
+        fi
+        _sub_unescape "$key" 1 || return 1
+        key=$_SUB_U
+        _sub_unescape "$val" 1 || return 1
+        val=$_SUB_U
+        if [[ $_SUB_KEYS == *[[:space:]]"$key"[[:space:]]* && -z ${_SUB_Q[$key]+set} ]]; then
+            _SUB_Q[$key]=$val
+        fi
+    done
+}
+
+# _sub_hostport <host[:port]|[ipv6][:port]>: sets _SUB_HOST (brackets
+# removed), _SUB_RAWPORT and _SUB_HASPORT.
+_sub_hostport() {
+    local hp=$1 tail
+    _SUB_HOST="" _SUB_RAWPORT="" _SUB_HASPORT=0
+    if [[ $hp == \[* ]]; then
+        if [[ $hp != *\]* ]]; then
+            _sub_skip invalid "bad IPv6 address"
+            return 1
+        fi
+        _SUB_HOST=${hp#\[}
+        _SUB_HOST=${_SUB_HOST%%\]*}
+        tail=${hp#*\]}
+        if [[ -n $tail ]]; then
+            if [[ $tail != :* ]]; then
+                _sub_skip invalid "bad address"
+                return 1
+            fi
+            _SUB_RAWPORT=${tail#:}
+            _SUB_HASPORT=1
+        fi
+    elif [[ $hp == *:* ]]; then
+        _SUB_HOST=${hp%:*}
+        _SUB_RAWPORT=${hp##*:}
+        _SUB_HASPORT=1
+    else
+        _SUB_HOST=$hp
+    fi
+    if [[ -z $_SUB_HOST ]]; then
+        _sub_skip invalid "missing host"
+        return 1
+    fi
+}
+
+# _sub_port <raw>: one to five digits, 1-65535, into _SUB_PORT.
+_sub_port() {
+    if [[ $1 =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 )); then
+        _SUB_PORT=$((10#$1))
+        return 0
+    fi
+    _sub_skip invalid "bad port \"$1\""
+    return 1
+}
+
+# _sub_link <rest>: the grammar of spec 6.2 up to the host -
+# scheme://userinfo@host[:port][/][?query][#fragment]. Sets _SUB_RAWNAME,
+# _SUB_USER (decoded, + kept), the host fields and _SUB_RAWQUERY.
+_sub_link() {
+    local rest=$1 body main authority
+    body=${rest%%#*}
+    if [[ $body != "$rest" ]]; then
+        _SUB_RAWNAME=${rest#*#}
+    fi
+    main=${body%%\?*}
+    _SUB_RAWQUERY=""
+    if [[ $main != "$body" ]]; then
+        _SUB_RAWQUERY=${body#*\?}
+    fi
+    authority=${main%%/*}
+    if [[ $authority != *@* ]]; then
+        _sub_skip invalid "missing userinfo"
+        return 1
+    fi
+    if ! _sub_unescape "${authority%@*}" 0; then
+        _sub_skip invalid "userinfo: bad percent escape"
+        return 1
+    fi
+    _SUB_USER=$_SUB_U
+    if [[ -z $_SUB_USER ]]; then
+        _sub_skip invalid "missing userinfo"
+        return 1
+    fi
+    _sub_hostport "${authority##*@}"
+}
+
+# _sub_port_query: the port a scheme requires, then the query.
+_sub_port_query() {
+    if [[ $_SUB_HASPORT != 1 ]]; then
+        _sub_skip invalid "missing port"
+        return 1
+    fi
+    _sub_port "$_SUB_RAWPORT" || return 1
+    if ! _sub_query "$_SUB_RAWQUERY"; then
+        _sub_skip invalid "query: bad percent escape"
+        return 1
+    fi
+}
+
+_sub_truthy() {
+    [[ $1 == 1 || $1 == true ]]
+}
+
+# _sub_stream <default security>: the checks of spec 6.3, in its order. Sets
+# the _SUB_V entries the jq stream function reads.
+_sub_stream() {
+    local net=${_SUB_Q[type]:-} sec=${_SUB_Q[security]:-} ht=${_SUB_Q[headerType]:-} k
+    case $net in
+        ''|tcp|raw) net=tcp ;;
+        ws|websocket) net=ws ;;
+        grpc|httpupgrade) ;;
+        xhttp|splithttp) net=xhttp ;;
+        *)
+            _sub_skip unsupported "transport $net"
+            return 1
+            ;;
+    esac
+    if [[ $net == tcp && -n $ht && $ht != none ]]; then
+        _sub_skip unsupported "tcp header $ht"
+        return 1
+    fi
+    [[ -n $sec ]] || sec=$1
+    case $sec in
+        none) ;;
+        tls)
+            if { _sub_truthy "${_SUB_Q[allowInsecure]:-}" || _sub_truthy "${_SUB_Q[insecure]:-}"; } &&
+                [[ -z ${_SUB_Q[pcs]:-} ]]; then
+                _sub_skip unsupported "insecure TLS"
+                return 1
+            fi
+            ;;
+        reality)
+            if [[ -z ${_SUB_Q[pbk]:-} || -z ${_SUB_Q[sni]:-} || -z ${_SUB_Q[fp]:-} ]]; then
+                _sub_skip invalid "reality needs pbk, sni and fp"
+                return 1
+            fi
+            ;;
+        *)
+            _sub_skip unsupported "security $sec"
+            return 1
+            ;;
+    esac
+    if [[ $net == xhttp && -n ${_SUB_Q[extra]:-} ]] &&
+        ! jq -es 'length == 1 and (.[0] | type) == "object"' <<< "${_SUB_Q[extra]}" >/dev/null 2>&1; then
+        _sub_skip invalid "xhttp extra is not a JSON object"
+        return 1
+    fi
+    _SUB_V[net]=$net
+    _SUB_V[sec]=$sec
+    for k in sni fp alpn pcs vcn pbk sid spx pqv path host serviceName authority mode extra flow; do
+        _SUB_V[$k]=${_SUB_Q[$k]:-}
+    done
+}
+
+# _sub_record <jq outbound>: _SUB_REC, the record of a converted server.
+_sub_record() {
+    local -a args=()
+    local k
+    _SUB_V[addr]=$_SUB_HOST
+    for k in $_SUB_VARS; do
+        args+=(--arg "$k" "${_SUB_V[$k]:-}")
+    done
+    _SUB_REC=$(jq -cn "${args[@]}" --argjson port "$_SUB_PORT" \
+        "$_SUB_JQ_LIB"' {address: $addr, port: $port, outbound: ('"$1"' | prune)}')
+}
+
+_sub_vless() {
+    _sub_link "$1" || return 1
+    _sub_port_query || return 1
+    _sub_stream none || return 1
+    _SUB_V[user]=$_SUB_USER
+    _SUB_V[enc]=${_SUB_Q[encryption]:-none}
+    _sub_record '{protocol: "vless", settings: {vnext: [{address: $addr, port: $port,
+        users: [{id: $user, encryption: $enc, flow: $flow}]}]}, streamSettings: stream}'
+}
+
+# Trojan runs over TLS, so a link that names no security gets tls.
+_sub_trojan() {
+    _sub_link "$1" || return 1
+    _sub_port_query || return 1
+    _sub_stream tls || return 1
+    _SUB_V[user]=$_SUB_USER
+    _sub_record '{protocol: "trojan", settings: {servers: [{address: $addr, port: $port,
+        password: $user}]}, streamSettings: stream}'
+}
+
+# _sub_entry <scheme> <rest>: converts one link and keeps its record and raw
+# name for the assembly.
+_sub_entry() {
+    local scheme=${1,,} rest=$2
+    _SUB_RAWNAME="" _SUB_DECODE=1 _SUB_REASON="" _SUB_DETAIL="" _SUB_REC="" _SUB_USER=""
+    _SUB_V=()
+    case $scheme in
+        vless) _sub_vless "$rest" || : ;;
+        trojan) _sub_trojan "$rest" || : ;;
+        *)
+            if [[ $rest == *#* ]]; then
+                _SUB_RAWNAME=${rest#*#}
+            fi
+            _sub_skip unsupported "$scheme"
+            ;;
+    esac
+    if [[ -n $_SUB_REASON || -z $_SUB_REC ]]; then
+        _SUB_REC=$(jq -cn --arg reason "${_SUB_REASON:-invalid}" --arg detail "$_SUB_DETAIL" \
+            '{reason: $reason, detail: $detail}')
+    fi
+    _SUB_RECORDS+=("$_SUB_REC")
+    _SUB_RAWNAMES+=("$_SUB_DECODE"$'\t'"$_SUB_RAWNAME")
+}
+
+# _sub_links <text>: one link per line; other lines are ignored (spec 6.1).
+_sub_links() {
+    local line
+    while IFS= read -r line || [[ -n $line ]]; do
+        line=${line#"${line%%[!$_SUB_SPACE]*}"}
+        line=${line%"${line##*[!$_SUB_SPACE]}"}
+        [[ $line =~ ^([A-Za-z][A-Za-z0-9+.-]*):// ]] || continue
+        _sub_entry "${BASH_REMATCH[1]}" "${line#*://}"
+    done <<< "$1"
+    if (( ${#_SUB_RECORDS[@]} == 0 )); then
+        printf 'unrecognized subscription format\n' >&2
+        return 1
+    fi
+}
+
+# _sub_result: the Result JSON from _SUB_RECORDS and _SUB_RAWNAMES - names,
+# the placeholder test (spec 6.6) and the split into servers and skips.
+_sub_result() {
+    local names
+    names=$(printf '%s\n' "${_SUB_RAWNAMES[@]}" | _sub_clean_names) || return 1
+    {
+        printf '[%s]\n' "${names//$'\n'/,}"
+        printf '%s\n' "${_SUB_RECORDS[@]}"
+    } | jq -cs '
+        def placeholder:
+          . == "::" or . == "::1"
+          or (split(".") as $p
+              | ($p | length) == 4
+                and all($p[]; length >= 1 and length <= 3 and (explode | all(.[]; . >= 48 and . <= 57)))
+                and all($p[]; tonumber <= 255)
+                and (($p[0] | tonumber) == 0 or ($p[0] | tonumber) == 127));
+        .[0] as $names | .[1:] as $recs
+        | [range(0; $recs | length) as $i
+           | $recs[$i] + {name: $names[$i], n: ($i + 1)}
+           | if .reason == null and (.address | placeholder) then
+               . + {reason: "placeholder", detail: .address} else . end] as $all
+        | {total: ($recs | length),
+           servers: [$all[] | select(.reason == null)
+                     | {name: (if .name == "" then .address else .name end), address, port, outbound}],
+           skipped: [$all[] | select(.reason != null)
+                     | {name: (if .name == "" then "#\(.n)" else .name end), reason, detail}]}'
+}
+
+# subscription_decode: reads a subscription on stdin (spec 6.1) and prints
+# the Result JSON; see the header of this file.
+subscription_decode() {
+    local LC_ALL=C body
+    _SUB_RECORDS=() _SUB_RAWNAMES=()
+    # bash cannot hold a NUL; the Go side drops them as well.
+    body=$(tr -d '\000')
+    body=${body#$'\xEF\xBB\xBF'}
+    body=$(_sub_trim "$body")
+    if [[ -z $body ]]; then
+        printf 'empty subscription\n' >&2
+        return 1
+    fi
+    if [[ $body == *'://'* ]]; then
+        _sub_links "$body" || return 1
+    else
+        local text
+        if ! text=$(_sub_b64 "$body"); then
+            printf 'unrecognized subscription format\n' >&2
+            return 1
+        fi
+        _sub_links "$text" || return 1
+    fi
+    _sub_result
+}
PATCH_EOF
```

- [ ] **Step 9: Run the tests**

Run: `bats router/test/unit/subscription.bats && (cd server && go test -count=1 ./internal/subscription/)`
Expected: 6 bats tests pass (the UTF-8 one may say `skip` on a machine without a UTF-8 locale); Go `ok`.

- [ ] **Step 10: Commit**

```bash
git add testdata/subscription server/internal/subscription router/opt/vpn-director/lib/subscription.sh router/test/unit/subscription.bats
git commit -m "feat(subscription): decode vless and trojan links, base64 or plain, in both importers"
```

---

### Task 4: VMess, Shadowsocks and Hysteria2 links

Spec 6.4 (vmess in both forms, ss SIP002 and legacy, hysteria2/hy2), 6.6 (names from `ps`), 6.7.

**Files:**
- Create: `server/internal/subscription/vmess.go`, `shadowsocks.go`, `hysteria2.go`
- Modify: `server/internal/subscription/links.go` (`parseLink`)
- Modify: `router/opt/vpn-director/lib/subscription.sh`
- Create: `testdata/subscription/links-other.{in,want.json}`, `testdata/subscription/links-skips.{in,want.json}`

**Interfaces:**
- Consumes: Task 3.
- Produces (Go): `parseVMess`, `parseVMessURL`, `parseVMessV2rayN`, `vmessEntry`, `jsonPort(n json.Number) (int, bool)` (Task 5 uses it), `ssMethods`, `parseShadowsocks`, `parseHysteria2`.
- Produces (shell): `_sub_vmess_record`, `_sub_vmess`, `_sub_ss`, `_sub_hy2`; `_sub_entry` dispatches them.

- [ ] **Step 1: Add the cases**

```bash
cat > testdata/subscription/links-other.in <<'FIXTURE_EOF'
trojan://secret%2Bpass@tr1.example.com:443#Trojan%20Default
trojan://pa%40ss@tr2.example.com:8443?type=ws&path=%2Ftr&host=tr2.example.com&sni=tr2.example.com&alpn=h2%2Chttp%2F1.1#Trojan%20WS
vmess://eyJ2IjogIjIiLCAicHMiOiAiVk1lc3MgV1MiLCAiYWRkIjogInZtMS5leGFtcGxlLmNvbSIsICJwb3J0IjogNDQzLCAiaWQiOiAiNDQ0NDQ0NDQtNDQ0NC00NDQ0LTg0NDQtNDQ0NDQ0NDQ0NDQ0IiwgImFpZCI6IDAsICJzY3kiOiAiYXV0byIsICJuZXQiOiAid3MiLCAidHlwZSI6ICJub25lIiwgImhvc3QiOiAidm0xLmV4YW1wbGUuY29tIiwgInBhdGgiOiAiL3ZtIiwgInRscyI6ICJ0bHMiLCAic25pIjogInZtMS5leGFtcGxlLmNvbSIsICJhbHBuIjogIiIsICJmcCI6ICJjaHJvbWUifQ==
vmess://eyJ2IjogIjIiLCAicHMiOiAi8J-Hr_Cfh7UgVk1lc3MgZ1JQQyIsICJhZGQiOiAidm0yLmV4YW1wbGUuY29tIiwgInBvcnQiOiAiODQ0MyIsICJpZCI6ICI1NTU1NTU1NS01NTU1LTQ1NTUtODU1NS01NTU1NTU1NTU1NTUiLCAiYWlkIjogNjQsICJzY3kiOiAiIiwgIm5ldCI6ICJncnBjIiwgInR5cGUiOiAibXVsdGkiLCAiaG9zdCI6ICIiLCAicGF0aCI6ICJncnBjLXN2YyIsICJ0bHMiOiAicmVhbGl0eSIsICJzbmkiOiAid3d3LmV4YW1wbGUub3JnIiwgImZwIjogImZpcmVmb3giLCAicGJrIjogIk50QWVETTMzMHVzMTZsWEcwTlJ4VlZZcHd1djQ0amJJZ3NWaDkyWlNVVW8iLCAic2lkIjogImFiIn0
vmess://44444444-4444-4444-8444-444444444444@vm3.example.com:443?type=tcp&security=tls&sni=vm3.example.com&encryption=aes-128-gcm#VMess%20URL
ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpwdy1vbmU@ss1.example.com:8388#SS%20SIP002
ss://2022-blake3-aes-256-gcm:SdwVF7LewaCi46HtswNaHAILOt%2Fl9UT8yKehM44ap60%3D:FHTFlZ5DQ0jQCX%2BBvxTDIG0%2BMe39608ulnXA0TkKS9o%3D@ss2.example.com:8389#SS%202022
ss://YWVzLTI1Ni1nY206bGVnYWN5LXBhc3NAc3MzLmV4YW1wbGUuY29tOjgzOTA=#SS%20Legacy
ss://QUVTLTEyOC1HQ006cHctZm91cg@ss4.example.com:8391/?plugin=obfs-local%3Bobfs%3Dhttp#SS%20Plugin
ss://cmM0LW1kNTpwdy1maXZl@ss5.example.com:8392#SS%20RC4
ss://QUVTLTI1Ni1HQ006cHctc2l4@ss6.example.com:8393/#SS%20Upper%20method
hysteria2://auth-token@hy1.example.com:8443/?sni=hy1.example.com#Hysteria2%20Plain
hy2://auth%2Bplus@hy2.example.com?obfs=salamander&obfs-password=obfs-pw&sni=hy2.example.com&alpn=h3%2Ch2#Hysteria2%20Salamander
hysteria2://auth@hy3.example.com:443?insecure=1&pinSHA256=39%3ADD%3AF9%3A4F%3AC8%3A42%3A18%3AD9%3A02%3ADB%3A65%3A27%3A25%3AFB%3AF4%3ABB%3A31%3A8A%3AAE%3AA3%3A06%3A37%3AE0%3A12%3A4B%3A00%3A4C%3AE7%3A03%3A1F%3A11%3AE6#Hysteria2%20Pinned
hysteria2://auth@hy4.example.com:443?insecure=1#Hysteria2%20Insecure
hysteria2://auth@hy5.example.com:443,5000-6000?sni=hy5.example.com#Hysteria2%20Hopping
hysteria2://auth@hy6.example.com:443?mport=5000-6000&obfs=gost#Hysteria2%20Other%20obfs
HY2://auth@hy7.example.com:8443?obfs=salamander#Hysteria2%20No%20obfs%20password
FIXTURE_EOF
```

```bash
cat > testdata/subscription/links-other.want.json <<'FIXTURE_EOF'
{
  "total": 18,
  "servers": [
    {"address":"tr1.example.com","name":"Trojan Default","outbound":{"protocol":"trojan","settings":{"servers":[{"address":"tr1.example.com","password":"secret+pass","port":443}]},"streamSettings":{"network":"tcp","security":"tls"}},"port":443},
    {"address":"tr2.example.com","name":"Trojan WS","outbound":{"protocol":"trojan","settings":{"servers":[{"address":"tr2.example.com","password":"pa@ss","port":8443}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"alpn":["h2","http/1.1"],"serverName":"tr2.example.com"},"wsSettings":{"host":"tr2.example.com","path":"/tr"}}},"port":8443},
    {"address":"vm1.example.com","name":"VMess WS","outbound":{"protocol":"vmess","settings":{"vnext":[{"address":"vm1.example.com","port":443,"users":[{"id":"44444444-4444-4444-8444-444444444444","security":"auto"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"fingerprint":"chrome","serverName":"vm1.example.com"},"wsSettings":{"host":"vm1.example.com","path":"/vm"}}},"port":443},
    {"address":"vm2.example.com","name":"VMess gRPC","outbound":{"protocol":"vmess","settings":{"vnext":[{"address":"vm2.example.com","port":8443,"users":[{"id":"55555555-5555-4555-8555-555555555555","security":"auto"}]}]},"streamSettings":{"grpcSettings":{"multiMode":true,"serviceName":"grpc-svc"},"network":"grpc","realitySettings":{"fingerprint":"firefox","publicKey":"NtAeDM330us16lXG0NRxVVYpwuv44jbIgsVh92ZSUUo","serverName":"www.example.org","shortId":"ab"},"security":"reality"}},"port":8443},
    {"address":"vm3.example.com","name":"VMess URL","outbound":{"protocol":"vmess","settings":{"vnext":[{"address":"vm3.example.com","port":443,"users":[{"id":"44444444-4444-4444-8444-444444444444","security":"aes-128-gcm"}]}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"vm3.example.com"}}},"port":443},
    {"address":"ss1.example.com","name":"SS SIP002","outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss1.example.com","method":"chacha20-ietf-poly1305","password":"pw-one","port":8388}]}},"port":8388},
    {"address":"ss2.example.com","name":"SS 2022","outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss2.example.com","method":"2022-blake3-aes-256-gcm","password":"SdwVF7LewaCi46HtswNaHAILOt/l9UT8yKehM44ap60=:FHTFlZ5DQ0jQCX+BvxTDIG0+Me39608ulnXA0TkKS9o=","port":8389}]}},"port":8389},
    {"address":"ss3.example.com","name":"SS Legacy","outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss3.example.com","method":"aes-256-gcm","password":"legacy-pass","port":8390}]}},"port":8390},
    {"address":"ss6.example.com","name":"SS Upper method","outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss6.example.com","method":"aes-256-gcm","password":"pw-six","port":8393}]}},"port":8393},
    {"address":"hy1.example.com","name":"Hysteria2 Plain","outbound":{"protocol":"hysteria","settings":{"address":"hy1.example.com","port":8443,"version":2},"streamSettings":{"hysteriaSettings":{"auth":"auth-token","version":2},"network":"hysteria","security":"tls","tlsSettings":{"alpn":["h3"],"serverName":"hy1.example.com"}}},"port":8443},
    {"address":"hy2.example.com","name":"Hysteria2 Salamander","outbound":{"protocol":"hysteria","settings":{"address":"hy2.example.com","port":443,"version":2},"streamSettings":{"finalmask":{"udp":[{"settings":{"password":"obfs-pw"},"type":"salamander"}]},"hysteriaSettings":{"auth":"auth+plus","version":2},"network":"hysteria","security":"tls","tlsSettings":{"alpn":["h3","h2"],"serverName":"hy2.example.com"}}},"port":443},
    {"address":"hy3.example.com","name":"Hysteria2 Pinned","outbound":{"protocol":"hysteria","settings":{"address":"hy3.example.com","port":443,"version":2},"streamSettings":{"hysteriaSettings":{"auth":"auth","version":2},"network":"hysteria","security":"tls","tlsSettings":{"alpn":["h3"],"pinnedPeerCertSha256":"39:DD:F9:4F:C8:42:18:D9:02:DB:65:27:25:FB:F4:BB:31:8A:AE:A3:06:37:E0:12:4B:00:4C:E7:03:1F:11:E6"}}},"port":443}
  ],
  "skipped": [
    {"name":"SS Plugin","reason":"unsupported"},
    {"name":"SS RC4","reason":"unsupported"},
    {"name":"Hysteria2 Insecure","reason":"unsupported"},
    {"name":"Hysteria2 Hopping","reason":"unsupported"},
    {"name":"Hysteria2 Other obfs","reason":"unsupported"},
    {"name":"Hysteria2 No obfs password","reason":"invalid"}
  ]
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/links-skips.in <<'FIXTURE_EOF'
# Provider notice: this line is not a link
STATUS=expires 2026-12-31

tuic://uuid:pw@tu.example.com:443?congestion_control=bbr#TUIC%20Server
ssr://c29tZXRoaW5nLWJhc2U2NA
vless://id@kcp.example.com:443?type=kcp#KCP
vless://id@http.example.com:443?type=tcp&headerType=http#HTTP%20header
vless://id@sec.example.com:443?security=xtls#Old%20XTLS
vless://id@bad.example.com:99999?type=tcp#Bad%20Port
vless://id@noport.example.com?type=tcp#No%20Port
vless://id@pct.example.com:443?type=tcp&path=%ZZ#Bad%20Escape
vless://@nouser.example.com:443#No%20User
vless://id@r.example.com:443?security=reality&sni=a.example.org&fp=chrome#No%20pbk
vless://id@x.example.com:443?type=xhttp&extra=%5B1%5D#Extra%20Array
vless://id@tls.example.com:443?security=tls&allowInsecure=1#Insecure
vless://id@tlsr.example.com:443?security=reality&allowInsecure=1&sni=a.example.org&fp=chrome&pbk=glTDKakoUPbVOd03b0gW7idkUX2l4CNVFK9DMWRIDXo#Reality%20ignores%20allowInsecure
vless://00000000-0000-0000-0000-000000000000@127.0.0.1:1#%D0%9F%D0%BE%D0%B4%D0%BF%D0%B8%D1%81%D0%BA%D0%B0%20%D0%B8%D1%81%D1%82%D0%B5%D0%BA%D0%BB%D0%B0
vless://00000000-0000-0000-0000-000000000000@[::]:1#Placeholder%20v6
vless://id@noname.example.com:443?security=none#🇺🇸🌟✨
vless://id@names.example.com:443#%20%20T%C3%BCrkiye%20%20%20Istanbul%20%2C%20
vless://id@stray.example.com:443#100%25%20off%2%zz+plus
vless://id@trunc.example.com:443#%C3%7Cevil
vless://id@nul.example.com:443#a%00b
vless://id@nulq.example.com:443?sni=a%00b#NUL%20in%20query
vmess://not-base64!!!
FIXTURE_EOF
```

```bash
cat > testdata/subscription/links-skips.want.json <<'FIXTURE_EOF'
{
  "total": 22,
  "servers": [
    {"address":"tlsr.example.com","name":"Reality ignores allowInsecure","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"tlsr.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"chrome","publicKey":"glTDKakoUPbVOd03b0gW7idkUX2l4CNVFK9DMWRIDXo","serverName":"a.example.org"},"security":"reality"}},"port":443},
    {"address":"noname.example.com","name":"noname.example.com","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"noname.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443},
    {"address":"names.example.com","name":"Türkiye Istanbul","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"names.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443},
    {"address":"stray.example.com","name":"100 off2zz plus","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"stray.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443},
    {"address":"trunc.example.com","name":"evil","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"trunc.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443},
    {"address":"nul.example.com","name":"ab","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"nul.example.com","port":443,"users":[{"encryption":"none","id":"id"}]}]},"streamSettings":{"network":"tcp","security":"none"}},"port":443}
  ],
  "skipped": [
    {"name":"TUIC Server","reason":"unsupported"},
    {"name":"#2","reason":"unsupported"},
    {"name":"KCP","reason":"unsupported"},
    {"name":"HTTP header","reason":"unsupported"},
    {"name":"Old XTLS","reason":"unsupported"},
    {"name":"Bad Port","reason":"invalid"},
    {"name":"No Port","reason":"invalid"},
    {"name":"Bad Escape","reason":"invalid"},
    {"name":"No User","reason":"invalid"},
    {"name":"No pbk","reason":"invalid"},
    {"name":"Extra Array","reason":"invalid"},
    {"name":"Insecure","reason":"unsupported"},
    {"name":"Подписка истекла","reason":"placeholder"},
    {"name":"Placeholder v6","reason":"placeholder"},
    {"name":"NUL in query","reason":"invalid"},
    {"name":"#22","reason":"invalid"}
  ]
}
FIXTURE_EOF
```


- [ ] **Step 2: Run both suites to see the new cases fail**

Run: `(cd server && go test -count=1 ./internal/subscription/); bats router/test/unit/subscription.bats`
Expected: FAIL on `links-other` and `links-skips` — vmess, ss and hysteria2 entries come out `unsupported`.

- [ ] **Step 3: Implement the Go converters**

Create `server/internal/subscription/vmess.go`:

```go
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
```

Create `server/internal/subscription/shadowsocks.go`:

```go
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
```

Create `server/internal/subscription/hysteria2.go`:

```go
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
```

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/subscription/links.go
+++ b/server/internal/subscription/links.go
@@ -46,8 +46,14 @@
 	switch scheme {
 	case "vless":
 		return parseVLESS(rest)
+	case "vmess":
+		return parseVMess(rest)
 	case "trojan":
 		return parseTrojan(rest)
+	case "ss":
+		return parseShadowsocks(rest)
+	case "hysteria2", "hy2":
+		return parseHysteria2(rest)
 	}
 	_, frag, _ := strings.Cut(rest, "#")
 	return entry{name: cleanName(unescapeName(frag))}, unsupported(scheme)
PATCH_EOF
```

- [ ] **Step 4: Implement the shell converters**

```bash
git apply <<'PATCH_EOF'
--- a/router/opt/vpn-director/lib/subscription.sh
+++ b/router/opt/vpn-director/lib/subscription.sh
@@ -363,6 +363,227 @@
         password: $user}]}, streamSettings: stream}'
 }
 
+_sub_vmess_record() {
+    _sub_record '{protocol: "vmess", settings: {vnext: [{address: $addr, port: $port,
+        users: [{id: $user, security: $enc}]}]}, streamSettings: stream}'
+}
+
+# Both vmess forms: the URL form, recognized by an @, and the v2rayN form,
+# base64 of a JSON object. Xray speaks VMess AEAD only; alterId is ignored.
+_sub_vmess() {
+    local body=${1%%#*} text fields k vn
+    if [[ $body == *@* ]]; then
+        _sub_link "$1" || return 1
+        _sub_port_query || return 1
+        _sub_stream none || return 1
+        _SUB_V[user]=$_SUB_USER
+        _SUB_V[enc]=${_SUB_Q[encryption]:-auto}
+        _sub_vmess_record
+        return
+    fi
+    _SUB_DECODE=0
+    if ! text=$(_sub_b64 "$body") ||
+        ! fields=$(jq -rs '
+            def f: if type == "string" then . elif type == "number" then tostring else "" end;
+            def nonul: explode | map(select(. != 0)) | implode;
+            if length != 1 or (.[0] | type) != "object" then error("not a vmess object") else .[0] end
+            | (.port | if type == "number" then (if . == floor and . >= 1 and . <= 65535 then ["number", (floor | tostring)] else ["bad", tostring] end)
+                       elif type == "string" then ["string", .] else ["missing", ""] end) as $port
+            | ([.add, .id, .scy, .net, .type, .host, .path, .tls, .sni, .alpn, .fp, .pbk, .sid, .spx]
+               | map(f | index("\u0000") != null) | any) as $nul
+            | "V_ps=\(.ps | f | nonul | @sh) V_add=\(.add | f | nonul | @sh) V_id=\(.id | f | nonul | @sh)",
+              "V_scy=\(.scy | f | nonul | @sh) V_net=\(.net | f | nonul | @sh) V_type=\(.type | f | nonul | @sh)",
+              "V_host=\(.host | f | nonul | @sh) V_path=\(.path | f | nonul | @sh) V_tls=\(.tls | f | nonul | @sh)",
+              "V_sni=\(.sni | f | nonul | @sh) V_alpn=\(.alpn | f | nonul | @sh) V_fp=\(.fp | f | nonul | @sh)",
+              "V_pbk=\(.pbk | f | nonul | @sh) V_sid=\(.sid | f | nonul | @sh) V_spx=\(.spx | f | nonul | @sh)",
+              "V_portkind=\($port[0] | @sh) V_port=\($port[1] | nonul | @sh) V_nul=\(if $nul then 1 else 0 end)"
+        ' <<< "$text" 2>/dev/null); then
+        _sub_skip invalid "vmess link is neither form"
+        return 1
+    fi
+    local V_ps V_add V_id V_scy V_net V_type V_host V_path V_tls V_sni V_alpn V_fp V_pbk V_sid V_spx
+    local V_portkind V_port V_nul
+    eval "$fields"
+    _SUB_RAWNAME=$V_ps
+    if [[ $V_nul == 1 ]]; then
+        _sub_skip invalid "NUL byte"
+        return 1
+    fi
+    if [[ -z $V_id ]]; then
+        _sub_skip invalid "missing id"
+        return 1
+    fi
+    _SUB_HOST=${V_add#\[}
+    _SUB_HOST=${_SUB_HOST%\]}
+    if [[ -z $_SUB_HOST ]]; then
+        _sub_skip invalid "missing address"
+        return 1
+    fi
+    case $V_portkind in
+        number) _SUB_PORT=$V_port ;;
+        string) _sub_port "$V_port" || return 1 ;;
+        bad)
+            _sub_skip invalid "bad port $V_port"
+            return 1
+            ;;
+        *)
+            _sub_skip invalid "missing port"
+            return 1
+            ;;
+    esac
+    _SUB_Q=()
+    _SUB_Q[type]=$V_net
+    _SUB_Q[security]=none
+    if [[ $V_tls == tls || $V_tls == reality ]]; then
+        _SUB_Q[security]=$V_tls
+    fi
+    for k in sni alpn fp pbk sid spx host; do
+        vn=V_$k
+        _SUB_Q[$k]=${!vn}
+    done
+    case $V_net in
+        grpc) _SUB_Q[serviceName]=$V_path _SUB_Q[mode]=$V_type ;;
+        xhttp|splithttp) _SUB_Q[path]=$V_path _SUB_Q[mode]=$V_type ;;
+        ''|tcp|raw) _SUB_Q[headerType]=$V_type ;;
+        *) _SUB_Q[path]=$V_path ;;
+    esac
+    _sub_stream none || return 1
+    _SUB_V[user]=$V_id
+    _SUB_V[enc]=${V_scy:-auto}
+    _sub_vmess_record
+}
+
+# SIP002 ss://userinfo@host:port[/][?plugin=…]#name, userinfo method:password in
+# plain text or base64, and legacy ss://base64(method:password@host:port)#name.
+# Base64 may hold a /, so the userinfo is all of the part before ? up to its
+# last @, and only the host part ends at a /.
+_sub_ss() {
+    local rest=$1 body main query="" ui hostport decoded method password
+    body=${rest%%#*}
+    if [[ $body != "$rest" ]]; then
+        _SUB_RAWNAME=${rest#*#}
+    fi
+    main=${body%%\?*}
+    if [[ $main != "$body" ]]; then
+        query=${body#*\?}
+    fi
+    if [[ $main == *@* ]]; then
+        if ! _sub_unescape "${main%@*}" 0; then
+            _sub_skip invalid "userinfo: bad percent escape"
+            return 1
+        fi
+        ui=$_SUB_U
+        if [[ $ui != *:* ]]; then
+            if ! decoded=$(_sub_b64 "$ui"); then
+                _sub_skip invalid "userinfo is not base64"
+                return 1
+            fi
+            ui=$(_sub_trim "$decoded")
+        fi
+        hostport=${main##*@}
+        hostport=${hostport%%/*}
+    else
+        if ! decoded=$(_sub_b64 "$main"); then
+            _sub_skip invalid "legacy link is not base64"
+            return 1
+        fi
+        decoded=$(_sub_trim "$decoded")
+        if [[ $decoded != *@* ]]; then
+            _sub_skip invalid "legacy link has no @"
+            return 1
+        fi
+        ui=${decoded%@*}
+        hostport=${decoded##*@}
+    fi
+    # A Shadowsocks 2022 password may hold a colon of its own.
+    method=${ui%%:*}
+    password=""
+    if [[ $ui == *:* ]]; then
+        password=${ui#*:}
+    fi
+    if [[ -z $method || -z $password ]]; then
+        _sub_skip invalid "missing method or password"
+        return 1
+    fi
+    _sub_hostport "$hostport" || return 1
+    if [[ $_SUB_HASPORT != 1 ]]; then
+        _sub_skip invalid "missing port"
+        return 1
+    fi
+    _sub_port "$_SUB_RAWPORT" || return 1
+    if ! _sub_query "$query"; then
+        _sub_skip invalid "query: bad percent escape"
+        return 1
+    fi
+    if [[ -n ${_SUB_Q[plugin]:-} ]]; then
+        _sub_skip unsupported "ss plugin"
+        return 1
+    fi
+    method=${method,,}
+    case $method in
+        aes-128-gcm|aes-256-gcm|chacha20-poly1305|chacha20-ietf-poly1305|xchacha20-poly1305|\
+        xchacha20-ietf-poly1305|2022-blake3-aes-128-gcm|2022-blake3-aes-256-gcm|\
+        2022-blake3-chacha20-poly1305|none|plain) ;;
+        *)
+            _sub_skip unsupported "ss method $method"
+            return 1
+            ;;
+    esac
+    _SUB_V[method]=$method
+    _SUB_V[password]=$password
+    _sub_record '{protocol: "shadowsocks", settings: {servers: [{address: $addr, port: $port,
+        method: $method, password: $password}]}}'
+}
+
+# hysteria2://auth@host[:port][/]?query#name, hy2:// alike. The port defaults
+# to 443. Port hopping is left out: its keys moved between Xray 26.2 and 26.3.
+_sub_hy2() {
+    _sub_link "$1" || return 1
+    _SUB_PORT=443
+    if [[ $_SUB_HASPORT == 1 ]]; then
+        if [[ $_SUB_RAWPORT == *[,-]* ]]; then
+            _sub_skip unsupported "port hopping"
+            return 1
+        fi
+        _sub_port "$_SUB_RAWPORT" || return 1
+    fi
+    if ! _sub_query "$_SUB_RAWQUERY"; then
+        _sub_skip invalid "query: bad percent escape"
+        return 1
+    fi
+    local obfs=${_SUB_Q[obfs]:-}
+    case $obfs in
+        '') ;;
+        salamander)
+            if [[ -z ${_SUB_Q[obfs-password]:-} ]]; then
+                _sub_skip invalid "salamander needs obfs-password"
+                return 1
+            fi
+            ;;
+        *)
+            _sub_skip unsupported "hysteria2 obfs $obfs"
+            return 1
+            ;;
+    esac
+    if _sub_truthy "${_SUB_Q[insecure]:-}" && [[ -z ${_SUB_Q[pinSHA256]:-} ]]; then
+        _sub_skip unsupported "insecure TLS"
+        return 1
+    fi
+    _SUB_V[user]=$_SUB_USER
+    _SUB_V[sni]=${_SUB_Q[sni]:-}
+    _SUB_V[alpn]=${_SUB_Q[alpn]:-}
+    _SUB_V[pin]=${_SUB_Q[pinSHA256]:-}
+    _SUB_V[obfs]=$obfs
+    _SUB_V[obfspw]=${_SUB_Q[obfs-password]:-}
+    _sub_record '{protocol: "hysteria", settings: {version: 2, address: $addr, port: $port},
+        streamSettings: ({network: "hysteria", security: "tls",
+            hysteriaSettings: {version: 2, auth: $user},
+            tlsSettings: {serverName: $sni, alpn: ($alpn | list | if length == 0 then ["h3"] else . end),
+                          pinnedPeerCertSha256: $pin}}
+          + (if $obfs == "salamander" then {finalmask: {udp: [{type: "salamander", settings: {password: $obfspw}}]}}
+             else {} end))}'
+}
+
 # _sub_entry <scheme> <rest>: converts one link and keeps its record and raw
 # name for the assembly.
 _sub_entry() {
@@ -371,7 +592,10 @@
     _SUB_V=()
     case $scheme in
         vless) _sub_vless "$rest" || : ;;
+        vmess) _sub_vmess "$rest" || : ;;
         trojan) _sub_trojan "$rest" || : ;;
+        ss) _sub_ss "$rest" || : ;;
+        hysteria2|hy2) _sub_hy2 "$rest" || : ;;
         *)
             if [[ $rest == *#* ]]; then
                 _SUB_RAWNAME=${rest#*#}
PATCH_EOF
```

- [ ] **Step 5: Run the tests**

Run: `(cd server && go vet ./internal/subscription/ && go test -count=1 ./internal/subscription/) && bats router/test/unit/subscription.bats`
Expected: Go `ok`; bats passes.

- [ ] **Step 6: Commit**

```bash
git add testdata/subscription server/internal/subscription router/opt/vpn-director/lib/subscription.sh
git commit -m "feat(subscription): decode vmess, shadowsocks and hysteria2 links"
```

---

### Task 5: Xray JSON subscriptions

Spec 6.1 (a body that starts with `[` or `{`) and 6.5 (the single proxy outbound, composite entries, checks, sanitization).

**Files:**
- Create: `server/internal/subscription/xrayjson.go`
- Modify: `server/internal/subscription/decode.go`
- Modify: `router/opt/vpn-director/lib/subscription.sh`
- Create: `testdata/subscription/` — `xray-array`, `xray-single`, `xray-singbox`, `error-json-invalid`, `error-json-empty`, `error-sip008` (`.in` and `.want.json` each)

**Interfaces:**
- Consumes: Tasks 1–4 (`vpnconfig.OutboundTarget`, `jsonPort`, `decodeJSON`).
- Produces (Go): `decodeXrayJSON`, `xrayEntry`, `helperProtocols`, `proxyProtocols`.
- Produces (shell): `_sub_xray_json <body>`.

- [ ] **Step 1: Add the cases**

```bash
cat > testdata/subscription/xray-array.in <<'FIXTURE_EOF'
[
{"remarks": "🇪🇺 🚀Auto | Best server ⚡⚡", "outbounds": [{"tag": "proxy-a", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.1", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "proxy-b", "protocol": "vless", "settings": {"vnext": [{"address": "a.example.com", "port": 8443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}, {"tag": "block", "protocol": "blackhole"}], "burstObservatory": {"subjectSelector": ["proxy-"]}, "routing": {"balancers": [{"tag": "auto", "selector": ["proxy-"], "strategy": {"type": "leastPing"}}], "rules": [{"type": "field", "network": "tcp,udp", "balancerTag": "auto"}]}},
{"remarks": "🇨🇭⚡Switzerland-1", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.10", "port": 8443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}, {"tag": "block", "protocol": "blackhole"}], "dns": {"servers": ["https://8.8.8.8/dns-query"]}, "inbounds": [{"port": 10808, "protocol": "socks", "tag": "socks"}], "routing": {"rules": [{"type": "field", "network": "udp", "port": "443", "outboundTag": "block"}]}, "log": {"loglevel": "info"}},
{"remarks": "🇳🇱🎮Netherlands GAMING", "outbounds": [{"tag": "proxy", "protocol": "hysteria", "settings": {"address": "198.51.100.11", "port": 8449, "version": 2}, "streamSettings": {"network": "hysteria", "hysteriaSettings": {"version": 2, "auth": "hy-auth"}, "security": "tls", "tlsSettings": {"serverName": "hy.example.com", "fingerprint": "firefox", "alpn": ["h3"]}, "finalmask": {"quicParams": {"debug": false, "congestion": "bbr"}}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "🇨🇦Canada SS", "outbounds": [{"tag": "proxy", "protocol": "shadowsocks", "settings": {"servers": [{"address": "198.51.100.12", "port": 2030, "password": "ss-pass", "method": "chacha20-ietf-poly1305", "uot": false, "UoTVersion": 1}]}, "streamSettings": {"network": "tcp", "tcpSettings": {}, "security": "none"}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "⬇️ Whitelist bypass ⬇️", "outbounds": [{"tag": "proxy-decoy", "protocol": "vless", "settings": {"vnext": [{"address": "wl-decoy.example.com", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "proxy-wl", "protocol": "vless", "settings": {"vnext": [{"address": "203.0.113.40", "port": 9443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none"}]}]}, "streamSettings": {"network": "xhttp", "xhttpSettings": {"mode": "stream-one", "path": "/api/v2/stream"}, "security": "reality", "realitySettings": {"serverName": "wl.example.org", "publicKey": "nXCaBeo--OR9Unsa12bmBEe5Dwy1jDtbqad8MOU7D0w", "shortId": "ff", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Chained through fragment", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.13", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}, "sockopt": {"dialerProxy": "fragment"}}}, {"tag": "fragment", "protocol": "freedom", "settings": {"fragment": {"packets": "tlshello", "length": "100-200"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Flat VLESS", "outbounds": [{"tag": "proxy", "sendThrough": "192.0.2.1", "protocol": "vless", "settings": {"address": "flat.example.com", "port": 443, "id": "99999999-9999-4999-8999-999999999999", "encryption": "none", "flow": "xtls-rprx-vision"}, "streamSettings": {"network": "raw", "security": "tls", "tlsSettings": {"serverName": "flat.example.com", "fingerprint": "chrome"}, "sockopt": {"mark": 255, "interface": "eth0", "tcpFastOpen": true}}, "mux": {"enabled": false, "concurrency": -1}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Insecure TLS", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.14", "port": 443, "password": "tr-pass"}]}, "streamSettings": {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "tr.example.com", "allowInsecure": true}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Pinned TLS", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.15", "port": 443, "password": "tr-pass"}]}, "streamSettings": {"network": "ws", "wsSettings": {"path": "/tr", "host": "tr.example.com"}, "security": "tls", "tlsSettings": {"serverName": "tr.example.com", "allowInsecure": true, "pinnedPeerCertSha256": "524453c7024ff9127ac7314f4c71e97c337b274fc9d5e5a24585dd0ca1508990"}, "sockopt": {"tproxy": "off", "customSockopt": [{"level": "6", "opt": "13", "value": "1"}]}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "WireGuard", "outbounds": [{"tag": "proxy", "protocol": "wireguard", "settings": {"secretKey": "WG-KEY", "peers": [{"endpoint": "198.51.100.16:51820", "publicKey": "WG-PUB"}]}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Only direct", "outbounds": [{"tag": "direct", "protocol": "freedom"}, {"tag": "block", "protocol": "blackhole"}]},
{"remarks": "IPv6 only", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "2001:db8::20", "port": 8443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Two targets", "outbounds": [{"tag": "proxy", "protocol": "vmess", "settings": {"vnext": [{"address": "198.51.100.17", "port": 443, "users": [{"id": "44444444-4444-4444-8444-444444444444", "security": "auto"}]}, {"address": "198.51.100.18", "port": 443, "users": [{"id": "44444444-4444-4444-8444-444444444444", "security": "auto"}]}]}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Reality without a key", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.19", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "⚠️ Subscription expired", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "0.0.0.0", "port": 1, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
"oops",
{"remarks": "No outbounds"},
{"remarks": "Proxy settings chain", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.20", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}, "proxySettings": {"tag": "upstream"}}, {"tag": "upstream", "protocol": "freedom"}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "String port", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.22", "port": "443", "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "  Line\tone\nLine two  ", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.23", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "publicKey": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E", "shortId": "0a1b", "fingerprint": "firefox"}}}, {"tag": "direct", "protocol": "freedom"}]},
{"remarks": "Reality password key", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "198.51.100.24", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none", "flow": "xtls-rprx-vision"}]}]}, "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.org", "shortId": "0a1b", "fingerprint": "firefox", "password": "CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E"}}}, {"tag": "direct", "protocol": "freedom"}]}
]
FIXTURE_EOF
```

```bash
cat > testdata/subscription/xray-array.want.json <<'FIXTURE_EOF'
{
  "total": 21,
  "servers": [
    {"address":"198.51.100.10","name":"Switzerland-1","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"198.51.100.10","port":8443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"88888888-8888-4888-8888-888888888888"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","publicKey":"CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E","serverName":"www.example.org","shortId":"0a1b"},"security":"reality"}},"port":8443},
    {"address":"198.51.100.11","name":"Netherlands GAMING","outbound":{"protocol":"hysteria","settings":{"address":"198.51.100.11","port":8449,"version":2},"streamSettings":{"finalmask":{"quicParams":{"congestion":"bbr","debug":false}},"hysteriaSettings":{"auth":"hy-auth","version":2},"network":"hysteria","security":"tls","tlsSettings":{"alpn":["h3"],"fingerprint":"firefox","serverName":"hy.example.com"}}},"port":8449},
    {"address":"198.51.100.12","name":"Canada SS","outbound":{"protocol":"shadowsocks","settings":{"servers":[{"UoTVersion":1,"address":"198.51.100.12","method":"chacha20-ietf-poly1305","password":"ss-pass","port":2030,"uot":false}]},"streamSettings":{"network":"tcp","security":"none","tcpSettings":{}}},"port":2030},
    {"address":"flat.example.com","name":"Flat VLESS","outbound":{"mux":{"concurrency":-1,"enabled":false},"protocol":"vless","settings":{"address":"flat.example.com","encryption":"none","flow":"xtls-rprx-vision","id":"99999999-9999-4999-8999-999999999999","port":443},"streamSettings":{"network":"raw","security":"tls","sockopt":{"tcpFastOpen":true},"tlsSettings":{"fingerprint":"chrome","serverName":"flat.example.com"}}},"port":443},
    {"address":"198.51.100.15","name":"Pinned TLS","outbound":{"protocol":"trojan","settings":{"servers":[{"address":"198.51.100.15","password":"tr-pass","port":443}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"pinnedPeerCertSha256":"524453c7024ff9127ac7314f4c71e97c337b274fc9d5e5a24585dd0ca1508990","serverName":"tr.example.com"},"wsSettings":{"host":"tr.example.com","path":"/tr"}}},"port":443},
    {"address":"2001:db8::20","name":"IPv6 only","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"2001:db8::20","port":8443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"88888888-8888-4888-8888-888888888888"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","publicKey":"CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E","serverName":"www.example.org","shortId":"0a1b"},"security":"reality"}},"port":8443},
    {"address":"198.51.100.23","name":"LineoneLine two","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"198.51.100.23","port":443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"88888888-8888-4888-8888-888888888888"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","publicKey":"CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E","serverName":"www.example.org","shortId":"0a1b"},"security":"reality"}},"port":443},
    {"address":"198.51.100.24","name":"Reality password key","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"198.51.100.24","port":443,"users":[{"encryption":"none","flow":"xtls-rprx-vision","id":"88888888-8888-4888-8888-888888888888"}]}]},"streamSettings":{"network":"tcp","realitySettings":{"fingerprint":"firefox","password":"CCQ2Zvkmqk5biEuO8fw7k9GeGRp5NYFTBha9LgMsR2E","serverName":"www.example.org","shortId":"0a1b"},"security":"reality"}},"port":443}
  ],
  "skipped": [
    {"name":"Auto Best server","reason":"composite"},
    {"name":"Whitelist bypass","reason":"composite"},
    {"name":"Chained through fragment","reason":"composite"},
    {"name":"Insecure TLS","reason":"unsupported"},
    {"name":"WireGuard","reason":"unsupported"},
    {"name":"Only direct","reason":"unsupported"},
    {"name":"Two targets","reason":"composite"},
    {"name":"Reality without a key","reason":"invalid"},
    {"name":"Subscription expired","reason":"placeholder"},
    {"name":"#16","reason":"invalid"},
    {"name":"No outbounds","reason":"invalid"},
    {"name":"Proxy settings chain","reason":"composite"},
    {"name":"String port","reason":"invalid"}
  ]
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/xray-single.in <<'FIXTURE_EOF'
{"remarks": "Single config", "outbounds": [{"tag": "proxy", "protocol": "vless", "settings": {"vnext": [{"address": "single.example.com", "port": 443, "users": [{"id": "88888888-8888-4888-8888-888888888888", "encryption": "none"}]}]}, "streamSettings": {"network": "ws", "wsSettings": {"path": "/ws", "headers": {"Host": "cdn.example.com"}}, "security": "tls", "tlsSettings": {"serverName": "cdn.example.com"}}}, {"tag": "direct", "protocol": "freedom"}]}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/xray-single.want.json <<'FIXTURE_EOF'
{
  "total": 1,
  "servers": [
    {"address":"single.example.com","name":"Single config","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"single.example.com","port":443,"users":[{"encryption":"none","id":"88888888-8888-4888-8888-888888888888"}]}]},"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"cdn.example.com"},"wsSettings":{"headers":{"Host":"cdn.example.com"},"path":"/ws"}}},"port":443}
  ],
  "skipped": []
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/xray-singbox.in <<'FIXTURE_EOF'
{"log": {"level": "warn"}, "outbounds": [{"type": "selector", "tag": "Internet", "outbounds": ["vless-1"]}, {"type": "vless", "tag": "vless-1", "server": "198.51.100.30", "server_port": 443, "uuid": "88888888-8888-4888-8888-888888888888"}, {"type": "direct", "tag": "direct"}]}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/xray-singbox.want.json <<'FIXTURE_EOF'
{
  "total": 1,
  "servers": [],
  "skipped": [
    {"name":"#1","reason":"unsupported"}
  ]
}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-json-invalid.in <<'FIXTURE_EOF'
[{"remarks": "cut short", "outbounds": [
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-json-invalid.want.json <<'FIXTURE_EOF'
{"error": "invalid JSON subscription"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-json-empty.in <<'FIXTURE_EOF'
[]
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-json-empty.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-sip008.in <<'FIXTURE_EOF'
{"version": 1, "servers": [{"server": "198.51.100.41", "server_port": 8388, "password": "p", "method": "aes-256-gcm"}]}
FIXTURE_EOF
```

```bash
cat > testdata/subscription/error-sip008.want.json <<'FIXTURE_EOF'
{"error": "unrecognized subscription format"}
FIXTURE_EOF
```


- [ ] **Step 2: Run both suites to see the new cases fail**

Run: `(cd server && go test -count=1 ./internal/subscription/); bats router/test/unit/subscription.bats`
Expected: FAIL on the new cases — `Decode` does not know JSON yet, so a JSON body ends `unrecognized subscription format` (and `error-json-invalid` gets that error instead of `invalid JSON subscription`).

- [ ] **Step 3: Implement the Go side**

Create `server/internal/subscription/xrayjson.go`:

```go
package subscription

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// helperProtocols are the outbounds an Xray config carries beside its proxy.
var helperProtocols = map[string]bool{"freedom": true, "blackhole": true, "dns": true, "loopback": true}

// proxyProtocols are the ones a server may run on.
var proxyProtocols = map[string]bool{"vless": true, "vmess": true, "trojan": true, "shadowsocks": true, "hysteria": true}

// decodeXrayJSON reads an array of Xray client configs, or one config (spec
// 6.5).
func decodeXrayJSON(body string) (Result, error) {
	doc, err := decodeJSON(body)
	if err != nil {
		return Result{}, ErrInvalidJSON
	}
	var configs []interface{}
	switch t := doc.(type) {
	case []interface{}:
		configs = t
	case map[string]interface{}:
		if _, ok := t["outbounds"].([]interface{}); ok {
			configs = []interface{}{t}
		}
	}
	if len(configs) == 0 {
		return Result{}, ErrUnrecognized
	}
	r := Result{Total: len(configs)}
	for i, c := range configs {
		e, err := xrayEntry(c)
		r.add(i+1, e, err)
	}
	return r, nil
}

// xrayEntry takes the config's single proxy outbound, checks it and removes
// what could reach past the proxy into the router's own routing.
func xrayEntry(raw interface{}) (entry, error) {
	cfg, ok := raw.(map[string]interface{})
	if !ok {
		return entry{}, invalid("not an Xray config")
	}
	var e entry
	if remarks, ok := cfg["remarks"].(string); ok {
		e.name = cleanName(remarks)
	}
	outbounds, ok := cfg["outbounds"].([]interface{})
	if !ok {
		return e, invalid("not an Xray config")
	}
	var proxies []map[string]interface{}
	for _, o := range outbounds {
		ob, _ := o.(map[string]interface{})
		if protocol, ok := ob["protocol"].(string); ok && !helperProtocols[protocol] {
			proxies = append(proxies, ob)
		}
	}
	switch len(proxies) {
	case 0:
		return e, unsupported("no proxy outbound")
	case 1:
	default:
		return e, composite(strconv.Itoa(len(proxies)) + " proxy outbounds")
	}
	ob := proxies[0]
	protocol := ob["protocol"].(string)
	if !proxyProtocols[protocol] {
		return e, unsupported("protocol " + protocol)
	}
	proxySettings, _ := ob["proxySettings"].(map[string]interface{})
	stream, _ := ob["streamSettings"].(map[string]interface{})
	sockopt, _ := stream["sockopt"].(map[string]interface{})
	if tag, _ := proxySettings["tag"].(string); tag != "" {
		return e, composite("chained")
	}
	if dialer, _ := sockopt["dialerProxy"].(string); dialer != "" {
		return e, composite("chained")
	}
	settings, _ := ob["settings"].(map[string]interface{})
	for _, key := range []string{"vnext", "servers"} {
		if list, ok := settings[key].([]interface{}); ok && len(list) > 1 {
			return e, composite(strconv.Itoa(len(list)) + " targets")
		}
	}
	target := vpnconfig.OutboundTarget(ob)
	address, _ := target["address"].(string)
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	number, _ := target["port"].(json.Number)
	port, ok := jsonPort(number)
	if address == "" || !ok {
		return e, invalid("bad address or port")
	}
	switch security, _ := stream["security"].(string); security {
	case "tls":
		tls, _ := stream["tlsSettings"].(map[string]interface{})
		if insecure, _ := tls["allowInsecure"].(bool); insecure {
			if pin, _ := tls["pinnedPeerCertSha256"].(string); pin == "" {
				return e, unsupported("insecure TLS")
			}
			delete(tls, "allowInsecure")
		}
	case "reality":
		reality, _ := stream["realitySettings"].(map[string]interface{})
		key, _ := reality["publicKey"].(string)
		if key == "" {
			key, _ = reality["password"].(string)
		}
		serverName, _ := reality["serverName"].(string)
		fingerprint, _ := reality["fingerprint"].(string)
		if key == "" || serverName == "" || fingerprint == "" {
			return e, invalid("reality needs publicKey, serverName and fingerprint")
		}
	}
	// A foreign fwmark could collide with ours (0x100 Xray, 0x01 firmware
	// VPN, 0x00ff0000 Tunnel Director), and an interface would route around
	// the WAN.
	delete(ob, "tag")
	delete(ob, "sendThrough")
	if sockopt != nil {
		for _, key := range []string{"mark", "interface", "tproxy", "customSockopt"} {
			delete(sockopt, key)
		}
		if len(sockopt) == 0 {
			delete(stream, "sockopt")
		}
	}
	e.address, e.port, e.outbound = address, port, ob
	return e, nil
}
```

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/subscription/decode.go
+++ b/server/internal/subscription/decode.go
@@ -2,10 +2,11 @@
 
 import "strings"
 
-// Decode reads a subscription body (spec 6.1): a plain link list when it holds
-// "://", and otherwise base64 of a link list. Its error is ErrEmpty or
-// ErrUnrecognized, for a body it cannot read at all; a readable body whose
-// entries were all skipped is a Result without servers.
+// Decode reads a subscription body (spec 6.1): Xray JSON when it starts with
+// [ or {, a plain link list when it holds "://", and otherwise base64 of a link
+// list. Its error is one of ErrEmpty, ErrInvalidJSON and ErrUnrecognized, for
+// a body it cannot read at all; a readable body whose entries were all skipped
+// is a Result without servers.
 func Decode(body string) (Result, error) {
 	// bash drops NUL bytes from what it reads; so does this side.
 	body = strings.ReplaceAll(body, "\x00", "")
@@ -14,6 +15,8 @@
 	switch {
 	case body == "":
 		return Result{}, ErrEmpty
+	case body[0] == '[' || body[0] == '{':
+		return decodeXrayJSON(body)
 	case strings.Contains(body, "://"):
 		return decodeLinks(body)
 	}
PATCH_EOF
```

- [ ] **Step 4: Implement the shell side**

One jq program reads the whole body and prints a record per config; its raw names lose their control characters, which the name filter would drop anyway.

```bash
git apply <<'PATCH_EOF'
--- a/router/opt/vpn-director/lib/subscription.sh
+++ b/router/opt/vpn-director/lib/subscription.sh
@@ -626,6 +626,87 @@
     fi
 }
 
+# _sub_xray_json <body>: an array of Xray configs, or one config (spec 6.5),
+# in one jq program: a record per config, its raw name kept NUL- and
+# control-free for the name filter, which drops those characters anyway.
+_sub_xray_json() {
+    local count out
+    if ! count=$(jq -s 'length' <<< "$1" 2>/dev/null) || [[ $count != 1 ]]; then
+        printf 'invalid JSON subscription\n' >&2
+        return 1
+    fi
+    out=$(jq -c '
+        def obj: if type == "object" then . else {} end;
+        def str: if type == "string" then . else "" end;
+        def rawname: str | explode | map(select(. >= 32 and . != 127)) | implode;
+        def target:
+          (.settings | obj) as $s
+          | (if .protocol == "vless" or .protocol == "vmess" then "vnext"
+             elif .protocol == "trojan" or .protocol == "shadowsocks" then "servers" else "" end) as $key
+          | if $key != "" and ($s[$key] | type) == "array" and ($s[$key] | length) > 0
+            then ($s[$key][0] | if type == "object" then . else {} end) else $s end;
+        def skip($raw; $reason; $detail): {raw: $raw, reason: $reason, detail: $detail};
+        def entry:
+          if type != "object" or (.outbounds | type) != "array" then
+            skip(if type == "object" then (.remarks | rawname) else "" end; "invalid"; "not an Xray config")
+          else
+            (.remarks | rawname) as $raw
+            | [.outbounds[] | select(type == "object") | select((.protocol | type) == "string")
+               | select(.protocol | IN("freedom", "blackhole", "dns", "loopback") | not)] as $proxies
+            | if ($proxies | length) == 0 then skip($raw; "unsupported"; "no proxy outbound")
+              elif ($proxies | length) > 1 then skip($raw; "composite"; "\($proxies | length) proxy outbounds")
+              else $proxies[0] as $ob
+              | ($ob.streamSettings | obj) as $ss
+              | ($ob.settings | obj) as $settings
+              | (($settings.vnext | if type == "array" then length else 0 end) as $v
+                 | ($settings.servers | if type == "array" then length else 0 end) as $sv
+                 | if $v > 1 then $v elif $sv > 1 then $sv else 0 end) as $targets
+              | ($ob | target) as $t
+              | ($t.address | str | ltrimstr("[") | rtrimstr("]")) as $addr
+              | ($t.port | if type == "number" and . == floor and . >= 1 and . <= 65535 then floor else null end) as $port
+              | if ($ob.protocol | IN("vless", "vmess", "trojan", "shadowsocks", "hysteria") | not) then
+                  skip($raw; "unsupported"; "protocol \($ob.protocol)")
+                elif ($ob.proxySettings | obj | .tag | str) != "" or ($ss.sockopt | obj | .dialerProxy | str) != "" then
+                  skip($raw; "composite"; "chained")
+                elif $targets > 1 then skip($raw; "composite"; "\($targets) targets")
+                elif $addr == "" or $port == null then skip($raw; "invalid"; "bad address or port")
+                elif ($ss.security | str) == "tls" and ($ss.tlsSettings | obj | .allowInsecure) == true
+                     and ($ss.tlsSettings | obj | .pinnedPeerCertSha256 | str) == "" then
+                  skip($raw; "unsupported"; "insecure TLS")
+                elif ($ss.security | str) == "reality"
+                     and (($ss.realitySettings | obj) as $r
+                          | (($r.publicKey | str) == "" and ($r.password | str) == "")
+                            or ($r.serverName | str) == "" or ($r.fingerprint | str) == "") then
+                  skip($raw; "invalid"; "reality needs publicKey, serverName and fingerprint")
+                else
+                  {raw: $raw, address: $addr, port: $port,
+                   outbound: ($ob
+                     | del(.tag, .sendThrough)
+                     | if ($ss.security | str) == "tls" and ($ss.tlsSettings | obj | .allowInsecure) == true
+                       then del(.streamSettings.tlsSettings.allowInsecure) else . end
+                     | if (.streamSettings | type) == "object" and (.streamSettings.sockopt | type) == "object"
+                       then .streamSettings.sockopt |= del(.mark, .interface, .tproxy, .customSockopt)
+                            | if (.streamSettings.sockopt | length) == 0 then del(.streamSettings.sockopt) else . end
+                       else . end)}
+                end
+              end
+          end;
+        (if type == "array" then .
+         elif type == "object" and (.outbounds | type) == "array" then [.]
+         else [] end) as $configs
+        | if ($configs | length) == 0 then "unrecognized" else ($configs[] | entry) end
+    ' <<< "$1") || {
+        printf 'invalid JSON subscription\n' >&2
+        return 1
+    }
+    if [[ $out == '"unrecognized"' ]]; then
+        printf 'unrecognized subscription format\n' >&2
+        return 1
+    fi
+    mapfile -t _SUB_RECORDS <<< "$out"
+    mapfile -t _SUB_RAWNAMES <<< "$(printf '%s\n' "${_SUB_RECORDS[@]}" | jq -r '"0\t" + .raw')"
+}
+
 # _sub_result: the Result JSON from _SUB_RECORDS and _SUB_RAWNAMES - names,
 # the placeholder test (spec 6.6) and the split into servers and skips.
 _sub_result() {
@@ -667,15 +748,20 @@
         printf 'empty subscription\n' >&2
         return 1
     fi
-    if [[ $body == *'://'* ]]; then
-        _sub_links "$body" || return 1
-    else
-        local text
-        if ! text=$(_sub_b64 "$body"); then
-            printf 'unrecognized subscription format\n' >&2
-            return 1
-        fi
-        _sub_links "$text" || return 1
-    fi
+    case ${body:0:1} in
+        '['|'{') _sub_xray_json "$body" || return 1 ;;
+        *)
+            if [[ $body == *'://'* ]]; then
+                _sub_links "$body" || return 1
+            else
+                local text
+                if ! text=$(_sub_b64 "$body"); then
+                    printf 'unrecognized subscription format\n' >&2
+                    return 1
+                fi
+                _sub_links "$text" || return 1
+            fi
+            ;;
+    esac
     _sub_result
 }
PATCH_EOF
```

- [ ] **Step 5: Run the tests**

Run: `(cd server && go vet ./internal/subscription/ && go test -count=1 ./internal/subscription/) && bats router/test/unit/subscription.bats`
Expected: Go `ok`; bats passes — all 17 cases decode alike on both sides.

- [ ] **Step 6: Commit**

```bash
git add testdata/subscription server/internal/subscription router/opt/vpn-director/lib/subscription.sh
git commit -m "feat(subscription): import Xray JSON subscriptions"
```

---

### Task 6: Resolution, import reports, and the Go callers

Spec 5 (`Import`), 11 (messages). The bot's `/import`, the Web UI import and the subscription watch switch from `vless` to `subscription`; the `vless` package goes.

**Files:**
- Create: `server/internal/subscription/resolve.go`, `summary.go`
- Test: `server/internal/subscription/subscription_test.go`
- Modify: `server/internal/bot/subfetch.go`; Test: `server/internal/bot/subfetch_test.go`
- Modify: `server/internal/handler/import.go`; Test: `server/internal/handler/import_test.go`
- Modify: `server/internal/webapi/handler_servers.go` (the import handler); Test: `server/internal/webapi/handler_servers_test.go`
- Delete: `server/internal/vless/`

**Interfaces:**
- Consumes: Tasks 2–5.
- Produces: `type Import struct{ Servers []vpnconfig.Server; Total, Parsed int; Skipped []Skip; ResolveErrors int }`; `func LookupIPv4(ctx) func(host string) ([]net.IP, error)`; `func DecodeAndResolve(body string) (Import, error)`; `func DecodeAndResolveLookup(body string, lookup func(string) ([]net.IP, error)) (Import, error)` (nil lookup = default resolver); `(Import).Counts() string` (e.g. `"7 composite, 1 DNS error"`), `.Details(n int) []string` (`"name: detail"` for unsupported and invalid), `.Summary() string` (`"Imported 32 of 40 servers: …"`), `.NoServers() string`, `.SkippedByReason() map[string]int`. The Web UI import answers `{ok, count, total, skipped, dns_errors, summary}`.

- [ ] **Step 1: Write the failing tests**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/subscription/subscription_test.go
+++ b/server/internal/subscription/subscription_test.go
@@ -1,8 +1,14 @@
 package subscription
 
 import (
+	"context"
 	"errors"
+	"net"
+	"reflect"
+	"strings"
 	"testing"
+
+	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
 
 func TestCleanName(t *testing.T) {
@@ -97,3 +103,115 @@
 		}
 	}
 }
+
+func TestImportSummary(t *testing.T) {
+	imp := Import{Total: 40, ResolveErrors: 1}
+	imp.Servers = make([]vpnconfig.Server, 32)
+	for i := 0; i < 7; i++ {
+		imp.Skipped = append(imp.Skipped, Skip{Name: "Auto", Reason: ReasonComposite, Detail: "12 proxy outbounds"})
+	}
+	if got := imp.Counts(); got != "7 composite, 1 DNS error" {
+		t.Errorf("Counts() = %q", got)
+	}
+	if got := imp.Summary(); got != "Imported 32 of 40 servers: 7 composite, 1 DNS error" {
+		t.Errorf("Summary() = %q", got)
+	}
+	if got := imp.SkippedByReason(); !reflect.DeepEqual(got, map[string]int{ReasonUnsupported: 0, ReasonComposite: 7, ReasonInvalid: 0, ReasonPlaceholder: 0}) {
+		t.Errorf("SkippedByReason() = %v", got)
+	}
+	whole := Import{Total: 2, Servers: make([]vpnconfig.Server, 2)}
+	if got := whole.Summary(); got != "Imported 2 servers" {
+		t.Errorf("Summary() = %q", got)
+	}
+}
+
+func TestImportNoServers(t *testing.T) {
+	imp := Import{Total: 6, Skipped: []Skip{
+		{Name: "TUIC", Reason: ReasonUnsupported, Detail: "tuic"},
+		{Name: "Auto", Reason: ReasonComposite, Detail: "3 proxy outbounds"},
+		{Name: "A", Reason: ReasonPlaceholder, Detail: "127.0.0.1"},
+		{Name: "B", Reason: ReasonPlaceholder, Detail: "0.0.0.0"},
+		{Name: "Bad", Reason: ReasonInvalid, Detail: "missing port"},
+		{Name: "KCP", Reason: ReasonUnsupported, Detail: "transport kcp"},
+	}}
+	want := "no supported servers in subscription: 2 unsupported, 1 composite, 1 invalid, 2 placeholders; " +
+		"TUIC: tuic; Bad: missing port; KCP: transport kcp"
+	if got := imp.NoServers(); got != want {
+		t.Errorf("NoServers() =\n%q\nwant\n%q", got, want)
+	}
+}
+
+func TestDecodeAndResolve(t *testing.T) {
+	// IP literals resolve without DNS; the IPv6 one does not resolve over IPv4.
+	body := strings.Join([]string{
+		"vless://uuid-1@203.0.113.10:443?security=none#Oslo",
+		"vless://missing-at-sign:443#Broken",
+		"trojan://pw@198.51.100.7:8443#Paris",
+		"vless://uuid-2@[2001:db8::1]:443#Six",
+	}, "\n")
+
+	imp, err := DecodeAndResolve(body)
+	if err != nil {
+		t.Fatal(err)
+	}
+	if imp.Total != 4 || imp.Parsed != 3 || len(imp.Skipped) != 1 || imp.ResolveErrors != 1 {
+		t.Fatalf("%+v", imp)
+	}
+	if len(imp.Servers) != 2 {
+		t.Fatalf("servers %+v", imp.Servers)
+	}
+	if got := imp.Servers[0]; got.Name != "Oslo" || !reflect.DeepEqual(got.IPs, []string{"203.0.113.10"}) {
+		t.Errorf("first server %+v", got)
+	}
+	if got := imp.Servers[1]; got.Name != "Paris" || !reflect.DeepEqual(got.IPs, []string{"198.51.100.7"}) {
+		t.Errorf("second server %+v", got)
+	}
+}
+
+func TestDecodeAndResolve_PassesTheDecodeErrorThrough(t *testing.T) {
+	if _, err := DecodeAndResolve("<html>not a subscription</html>"); !errors.Is(err, ErrUnrecognized) {
+		t.Fatalf("err %v, want ErrUnrecognized", err)
+	}
+}
+
+func TestDecodeAndResolveLookup(t *testing.T) {
+	var looked []string
+	imp, err := DecodeAndResolveLookup("vless://uuid-1@oslo.example.invalid:443#Oslo", func(host string) ([]net.IP, error) {
+		looked = append(looked, host)
+		return []net.IP{net.ParseIP("2001:db8::5"), net.ParseIP("203.0.113.50")}, nil
+	})
+	if err != nil {
+		t.Fatal(err)
+	}
+	if len(imp.Servers) != 1 || !reflect.DeepEqual(imp.Servers[0].IPs, []string{"203.0.113.50"}) {
+		t.Fatalf("%+v", imp)
+	}
+	if !reflect.DeepEqual(looked, []string{"oslo.example.invalid"}) {
+		t.Fatalf("lookup %v", looked)
+	}
+}
+
+func TestLookupIPv4_ReturnsOnlyIPv4(t *testing.T) {
+	ips, err := LookupIPv4(context.Background())("localhost")
+	if err != nil {
+		t.Skipf("no local resolver for localhost: %v", err)
+	}
+	if len(ips) == 0 {
+		t.Fatal("localhost resolved to nothing")
+	}
+	for _, ip := range ips {
+		if ip.To4() == nil {
+			t.Fatalf("resolved %v; an AF_UNSPEC lookup is what waits out the AAAA half", ip)
+		}
+	}
+}
+
+// The subscription of a router can name tens of hosts, resolved one after the
+// other inside a watch tick. A stop or a shutdown has to be able to end that.
+func TestLookupIPv4_HonoursTheContext(t *testing.T) {
+	ctx, cancel := context.WithCancel(context.Background())
+	cancel()
+	if _, err := LookupIPv4(ctx)("localhost"); err == nil {
+		t.Fatal("a canceled context must end the lookup")
+	}
+}
--- a/server/internal/bot/subfetch_test.go
+++ b/server/internal/bot/subfetch_test.go
@@ -240,8 +240,12 @@
 }
 
 func TestServersFromSubscription(t *testing.T) {
-	if _, err := serversFromSubscriptionLookup([]byte("not base64 !!!"), nil); err == nil || err.Error() != "no VLESS servers" {
-		t.Fatalf("err %v, want no VLESS servers", err)
+	if _, err := serversFromSubscriptionLookup([]byte("not base64 !!!"), nil); err == nil || err.Error() != "unrecognized subscription format" {
+		t.Fatalf("err %v, want unrecognized subscription format", err)
+	}
+	// A readable subscription none of whose entries Xray can run.
+	if _, err := serversFromSubscriptionLookup([]byte("tuic://uuid:pw@203.0.113.10:443#TUIC"), nil); err == nil || err.Error() != "no supported servers" {
+		t.Fatalf("err %v, want no supported servers", err)
 	}
 
 	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443#Oslo"))
--- a/server/internal/handler/import_test.go
+++ b/server/internal/handler/import_test.go
@@ -597,14 +597,13 @@
 	if sender.lastChatID != 222 {
 		t.Errorf("expected chatID 222, got %d", sender.lastChatID)
 	}
-	// Should report no servers found or decode error
-	if !strings.Contains(sender.lastText, "No") && !strings.Contains(sender.lastText, "decode") && !strings.Contains(sender.lastText, "base64") {
-		t.Errorf("expected error about decoding or no servers, got %q", sender.lastText)
+	if !strings.Contains(sender.lastText, "unrecognized subscription format") {
+		t.Errorf("expected the unrecognized format error, got %q", sender.lastText)
 	}
 }
 
 func TestImportHandler_HandleImport_EmptySubscription(t *testing.T) {
-	// Create a subscription with no VLESS URLs
+	// A base64 body with no link in it
 	encoded := base64.StdEncoding.EncodeToString([]byte("just some text\nno vless here"))
 
 	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
@@ -631,8 +630,8 @@
 	if sender.lastChatID != 333 {
 		t.Errorf("expected chatID 333, got %d", sender.lastChatID)
 	}
-	if !strings.Contains(sender.lastText, "No") {
-		t.Errorf("expected 'No VLESS servers' message, got %q", sender.lastText)
+	if !strings.Contains(sender.lastText, "unrecognized subscription format") {
+		t.Errorf("expected the unrecognized format error, got %q", sender.lastText)
 	}
 }
 
@@ -663,3 +662,47 @@
 		t.Error("expected no servers saved for a blocked private URL")
 	}
 }
+
+// An import says what it left out, and why: the counts under the country
+// list, then the entries a user can act on.
+func TestImportHandler_HandleImport_ReportsWhatWasSkipped(t *testing.T) {
+	server := subscriptionServer(t, strings.Join([]string{
+		"vless://uuid-1@203.0.113.10:443?type=tcp#Oslo",
+		"tuic://uuid:pw@203.0.113.11:443#TUIC",
+		"vless://uuid-2@203.0.113.12:443?type=kcp#KCP",
+	}, "\n"))
+	sender := &mockSender{}
+	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
+	h := NewImportHandler(&Deps{Sender: sender, Config: config})
+	h.httpClient = server.Client()
+
+	h.HandleImport(importCommand("/import " + server.URL))
+
+	for _, want := range []string{"Imported 1 of 3 servers:", "2 unsupported", "TUIC: tuic", "KCP: transport kcp"} {
+		if !strings.Contains(sender.lastText, want) {
+			t.Errorf("message %q lacks %q", sender.lastText, want)
+		}
+	}
+	if len(config.savedServers) != 1 || config.savedServers[0].Name != "Oslo" {
+		t.Fatalf("saved %+v", config.savedServers)
+	}
+}
+
+func TestImportHandler_HandleImport_NoSupportedServers(t *testing.T) {
+	server := subscriptionServer(t, "tuic://uuid:pw@203.0.113.11:443#TUIC")
+	sender := &mockSender{}
+	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
+	h := NewImportHandler(&Deps{Sender: sender, Config: config})
+	h.httpClient = server.Client()
+
+	h.HandleImport(importCommand("/import " + server.URL))
+
+	for _, want := range []string{"No supported servers in subscription", "1 unsupported", "TUIC: tuic"} {
+		if !strings.Contains(sender.lastText, want) {
+			t.Errorf("message %q lacks %q", sender.lastText, want)
+		}
+	}
+	if config.savedServers != nil {
+		t.Fatalf("saved %+v", config.savedServers)
+	}
+}
--- a/server/internal/webapi/handler_servers_test.go
+++ b/server/internal/webapi/handler_servers_test.go
@@ -424,18 +424,61 @@
 	}
 }
 
-func TestNoServersMessage(t *testing.T) {
-	if got := noServersMessage(nil); got != "no VLESS servers found in subscription" {
-		t.Errorf("no errors: got %q", got)
-	}
-	two := []error{errors.New("line 1: bad scheme"), errors.New("line 2: missing uuid")}
-	want := "no VLESS servers found in subscription: line 1: bad scheme; line 2: missing uuid"
-	if got := noServersMessage(two); got != want {
-		t.Errorf("two errors: got %q, want %q", got, want)
-	}
-	five := []error{errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4"), errors.New("e5")}
-	if got := noServersMessage(five); got != "no VLESS servers found in subscription: e1; e2; e3" {
-		t.Errorf("five errors must be capped at three: got %q", got)
+// An import says what it left out and why, and the page shows it: a
+// subscription that is all composite entries and TUIC imported nothing and
+// said only "no VLESS servers".
+func TestHandleImportServers_SaysWhatNothingCouldBeImportedFrom(t *testing.T) {
+	deps := newTestDeps(t)
+	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
+	deps.ImportClient = subscriptionHost(t, "tuic://uuid:pw@203.0.113.10:443#TUIC\nssr://c29tZQ")
+
+	code, resp := postImport(t, deps, `{"url":"https://93.184.216.34/s/token"}`)
+
+	want := "no supported servers in subscription: 2 unsupported; TUIC: tuic; #2: ssr"
+	if code != http.StatusBadRequest || resp["error"] != want {
+		t.Fatalf("got %d %v, want 400 %q", code, resp, want)
+	}
+}
+
+func TestHandleImportServers_RefusesAnUnrecognizedBody(t *testing.T) {
+	deps := newTestDeps(t)
+	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
+	deps.ImportClient = subscriptionHost(t, "<!doctype html><html>Open the app</html>")
+
+	code, resp := postImport(t, deps, `{"url":"https://93.184.216.34/s/token"}`)
+
+	if code != http.StatusBadRequest || resp["error"] != "unrecognized subscription format" {
+		t.Fatalf("got %d %v", code, resp)
+	}
+}
+
+func TestHandleImportServers_ReportsTheSkips(t *testing.T) {
+	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
+	deps := newTestDeps(t)
+	deps.Config = mc
+	deps.ImportClient = subscriptionHost(t, strings.Join([]string{
+		"vless://uuid-1@203.0.113.10:443?type=tcp#Oslo",
+		"vless://uuid-2@203.0.113.11:443?type=kcp#KCP",
+		"vless://uuid-3@127.0.0.1:1#Expired",
+	}, "\n"))
+
+	code, resp := postImport(t, deps, `{"url":"https://93.184.216.34/s/token"}`)
+
+	if code != http.StatusOK {
+		t.Fatalf("got %d %v", code, resp)
+	}
+	if resp["count"] != float64(1) || resp["total"] != float64(3) || resp["dns_errors"] != float64(0) {
+		t.Fatalf("response %v", resp)
+	}
+	skipped, _ := resp["skipped"].(map[string]interface{})
+	if skipped["unsupported"] != float64(1) || skipped["placeholder"] != float64(1) || skipped["composite"] != float64(0) {
+		t.Fatalf("skipped %v", skipped)
+	}
+	if resp["summary"] != "Imported 1 of 3 servers: 1 unsupported, 1 placeholder" {
+		t.Fatalf("summary %v", resp["summary"])
+	}
+	if len(mc.savedServers) != 1 || mc.savedServers[0].Name != "Oslo" || len(mc.savedServers[0].Outbound) == 0 {
+		t.Fatalf("saved %+v", mc.savedServers)
 	}
 }
 
PATCH_EOF
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 ./internal/subscription/ ./internal/bot/ ./internal/handler/ ./internal/webapi/`
Expected: FAIL — `undefined: Import`, `DecodeAndResolve`; the bot and Web UI tests still see the VLESS-only messages.

- [ ] **Step 3: Add resolution and the reports**

Create `server/internal/subscription/resolve.go`:

```go
package subscription

import (
	"context"
	"fmt"
	"net"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Import is a decoded subscription after resolution. Servers keeps
// subscription order and holds only the servers whose address resolved;
// Parsed counts the servers Decode returned.
type Import struct {
	Servers       []vpnconfig.Server
	Total         int
	Parsed        int
	Skipped       []Skip
	ResolveErrors int
}

// LookupIPv4 resolves over IPv4 only, through ctx. The router's own resolver is
// reached straight over the WAN, and an AF_UNSPEC lookup there waits out the
// AAAA half that often goes unanswered - glibc's full five seconds per host,
// in a loop over the whole subscription (.claude/rules/shell-conventions.md
// has the measurement). Anything that is not IPv4 is discarded below anyway,
// so the second family is pure waiting.
func LookupIPv4(ctx context.Context) func(host string) ([]net.IP, error) {
	return func(host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
}

// DecodeAndResolve decodes a subscription body and resolves every server
// through the default resolver. The bot's /import and the Web UI import use
// this; a tunneled watch fetch uses DecodeAndResolveLookup.
func DecodeAndResolve(body string) (Import, error) {
	return DecodeAndResolveLookup(body, LookupIPv4(context.Background()))
}

// DecodeAndResolveLookup is DecodeAndResolve with a caller-supplied lookup
// (the watch uses the tunnel DNS path after a tunneled GET). Decode's error
// comes back as is.
func DecodeAndResolveLookup(body string, lookup func(host string) ([]net.IP, error)) (Import, error) {
	decoded, err := Decode(body)
	if err != nil {
		return Import{}, err
	}
	if lookup == nil {
		lookup = LookupIPv4(context.Background())
	}
	imp := Import{Total: decoded.Total, Parsed: len(decoded.Servers), Skipped: decoded.Skipped}
	for _, s := range decoded.Servers {
		ips, err := resolveIPv4(lookup, s.Address)
		if err != nil {
			imp.ResolveErrors++
			continue
		}
		s.IPs = ips
		imp.Servers = append(imp.Servers, s)
	}
	return imp, nil
}

func resolveIPv4(lookup func(host string) ([]net.IP, error), host string) ([]string, error) {
	ips, err := lookup(host)
	if err != nil {
		return nil, err
	}
	var resolved []string
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			resolved = append(resolved, ipv4.String())
		}
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found for %s", host)
	}
	return resolved, nil
}
```

Create `server/internal/subscription/summary.go`:

```go
package subscription

import (
	"fmt"
	"strings"
)

// Counts renders the non-zero skip and DNS counts in a fixed order, e.g.
// "7 composite, 1 DNS error". Empty when nothing was lost.
func (imp Import) Counts() string {
	count := map[string]int{}
	for _, s := range imp.Skipped {
		count[s.Reason]++
	}
	var parts []string
	for _, reason := range []string{ReasonUnsupported, ReasonComposite, ReasonInvalid} {
		if n := count[reason]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, reason))
		}
	}
	if n := count[ReasonPlaceholder]; n > 0 {
		parts = append(parts, plural(n, "placeholder"))
	}
	if imp.ResolveErrors > 0 {
		parts = append(parts, plural(imp.ResolveErrors, "DNS error"))
	}
	return strings.Join(parts, ", ")
}

// SkippedByReason counts the skips of each reason, every reason present.
func (imp Import) SkippedByReason() map[string]int {
	count := map[string]int{ReasonUnsupported: 0, ReasonComposite: 0, ReasonInvalid: 0, ReasonPlaceholder: 0}
	for _, s := range imp.Skipped {
		count[s.Reason]++
	}
	return count
}

// Details lists up to n "name: detail" lines for the unsupported and invalid
// entries - the skips a user can act on.
func (imp Import) Details(n int) []string {
	var lines []string
	for _, s := range imp.Skipped {
		if len(lines) == n {
			break
		}
		if s.Reason == ReasonUnsupported || s.Reason == ReasonInvalid {
			lines = append(lines, s.Name+": "+s.Detail)
		}
	}
	return lines
}

// Summary is the one sentence the Web UI shows after an import:
// "Imported 32 of 40 servers: 7 composite, 1 DNS error", or
// "Imported 40 servers" when nothing was lost.
func (imp Import) Summary() string {
	counts := imp.Counts()
	if counts == "" {
		return fmt.Sprintf("Imported %d servers", len(imp.Servers))
	}
	return fmt.Sprintf("Imported %d of %d servers: %s", len(imp.Servers), imp.Total, counts)
}

// NoServers explains an import that decoded no server: the counts and up to
// three details.
func (imp Import) NoServers() string {
	msg := "no supported servers in subscription"
	if counts := imp.Counts(); counts != "" {
		msg += ": " + counts
	}
	if details := imp.Details(3); len(details) > 0 {
		msg += "; " + strings.Join(details, "; ")
	}
	return msg
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
```

- [ ] **Step 4: Switch the callers**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/bot/subfetch.go
+++ b/server/internal/bot/subfetch.go
@@ -14,7 +14,7 @@
 
 	"github.com/zinin/vpn-director/server/internal/service"
 	"github.com/zinin/vpn-director/server/internal/ssrf"
-	"github.com/zinin/vpn-director/server/internal/vless"
+	"github.com/zinin/vpn-director/server/internal/subscription"
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
 
@@ -60,7 +60,7 @@
 	// IPv4 only and bound to ctx: an AF_UNSPEC lookup of every hostname in the
 	// subscription can hold a watch tick for minutes on this router, and a stop
 	// has to be able to end it.
-	wanLookup := vless.LookupIPv4(ctx)
+	wanLookup := subscription.LookupIPv4(ctx)
 	p, tunnel := subscriptionTunnel(cfgSvc, vpnSvc)
 	var tunnelLookup func(host string) ([]net.IP, error)
 	if tunnel != nil {
@@ -122,7 +122,7 @@
 // IPv4 address. A nil first is the default resolver, bound to ctx.
 func eitherLookup(ctx context.Context, first, second func(host string) ([]net.IP, error)) func(host string) ([]net.IP, error) {
 	if first == nil {
-		first = vless.LookupIPv4(ctx)
+		first = subscription.LookupIPv4(ctx)
 	}
 	if second == nil {
 		return first
@@ -148,14 +148,12 @@
 // watch and keeps the servers whose addresses resolved through lookup, the
 // default resolver when it is nil.
 func serversFromSubscriptionLookup(body []byte, lookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
-	var result vless.Import
-	if lookup == nil {
-		result = vless.DecodeAndResolve(string(body))
-	} else {
-		result = vless.DecodeAndResolveLookup(string(body), lookup)
+	result, err := subscription.DecodeAndResolveLookup(string(body), lookup)
+	if err != nil {
+		return nil, err
 	}
 	if result.Parsed == 0 {
-		return nil, errors.New("no VLESS servers")
+		return nil, errors.New("no supported servers")
 	}
 	if len(result.Servers) == 0 {
 		return nil, errNoResolved
--- a/server/internal/handler/import.go
+++ b/server/internal/handler/import.go
@@ -14,8 +14,8 @@
 	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
 	"github.com/zinin/vpn-director/server/internal/service"
 	"github.com/zinin/vpn-director/server/internal/ssrf"
+	"github.com/zinin/vpn-director/server/internal/subscription"
 	"github.com/zinin/vpn-director/server/internal/telegram"
-	"github.com/zinin/vpn-director/server/internal/vless"
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
 
@@ -35,7 +35,7 @@
 	}
 }
 
-// HandleImport handles /import command - downloads and imports VLESS subscription
+// HandleImport handles /import command - downloads and imports a subscription
 func (h *ImportHandler) HandleImport(msg *tgbotapi.Message) {
 	args := msg.CommandArguments()
 	fetchURL := args
@@ -96,18 +96,19 @@
 		return
 	}
 
-	// Decode VLESS subscription and resolve IPs for each server
-	result := vless.DecodeAndResolve(string(body))
+	// Decode the subscription and resolve IPs for each server
+	result, err := subscription.DecodeAndResolve(string(body))
+	if err != nil {
+		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
+		return
+	}
 	if result.Parsed == 0 {
-		var sb strings.Builder
-		sb.WriteString("No VLESS servers found")
-		if len(result.ParseErrors) > 0 {
-			sb.WriteString("\nErrors:\n")
-			for _, e := range result.ParseErrors {
-				sb.WriteString(fmt.Sprintf("- %s\n", e))
-			}
+		lines := []string{"No supported servers in subscription"}
+		if counts := result.Counts(); counts != "" {
+			lines = append(lines, counts)
 		}
-		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(sb.String()))
+		lines = append(lines, result.Details(3)...)
+		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
 		return
 	}
 
@@ -146,10 +147,12 @@
 		return
 	}
 
-	// Build response with grouped stats
+	// Build response with grouped stats: what was left out, and why, under the
+	// country list.
 	var sb strings.Builder
-	if result.ResolveErrors > 0 || len(result.ParseErrors) > 0 {
-		sb.WriteString(fmt.Sprintf("Imported %d of %d servers:\n", len(result.Servers), result.Parsed))
+	counts := result.Counts()
+	if counts != "" {
+		sb.WriteString(fmt.Sprintf("Imported %d of %d servers:\n", len(result.Servers), result.Total))
 	} else {
 		sb.WriteString(fmt.Sprintf("Imported %d servers:\n", len(result.Servers)))
 	}
@@ -157,17 +160,10 @@
 	groupedStr := groupServersByCountry(result.Servers)
 	sb.WriteString(telegram.EscapeMarkdownV2(groupedStr))
 
-	if result.ResolveErrors > 0 || len(result.ParseErrors) > 0 {
+	if counts != "" {
+		lines := append([]string{counts}, result.Details(3)...)
 		sb.WriteString("\n\n")
-		if result.ResolveErrors > 0 {
-			sb.WriteString(fmt.Sprintf("%d DNS errors", result.ResolveErrors))
-		}
-		if len(result.ParseErrors) > 0 {
-			if result.ResolveErrors > 0 {
-				sb.WriteString(", ")
-			}
-			sb.WriteString(fmt.Sprintf("%d parse errors", len(result.ParseErrors)))
-		}
+		sb.WriteString(telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
 	}
 
 	h.deps.Sender.Send(msg.Chat.ID, sb.String())
--- a/server/internal/webapi/handler_servers.go
+++ b/server/internal/webapi/handler_servers.go
@@ -7,12 +7,11 @@
 	"log/slog"
 	"net/http"
 	"net/url"
-	"strings"
 	"time"
 
 	"github.com/zinin/vpn-director/server/internal/service"
 	"github.com/zinin/vpn-director/server/internal/ssrf"
-	"github.com/zinin/vpn-director/server/internal/vless"
+	"github.com/zinin/vpn-director/server/internal/subscription"
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
 
@@ -222,11 +221,15 @@
 			return
 		}
 
-		// Decode VLESS subscription and resolve IPs. Parse errors travel back to
-		// the user so a rejected link explains itself, as the bot's /import does.
-		result := vless.DecodeAndResolve(string(body))
+		// Decode the subscription and resolve IPs. What was skipped, and why,
+		// travels back to the user, as the bot's /import says it.
+		result, err := subscription.DecodeAndResolve(string(body))
+		if err != nil {
+			jsonError(w, http.StatusBadRequest, err.Error())
+			return
+		}
 		if result.Parsed == 0 {
-			jsonError(w, http.StatusBadRequest, noServersMessage(result.ParseErrors))
+			jsonError(w, http.StatusBadRequest, result.NoServers())
 			return
 		}
 
@@ -265,7 +268,14 @@
 			return
 		}
 
-		jsonOK(w, map[string]interface{}{"ok": true, "count": len(result.Servers)})
+		jsonOK(w, map[string]interface{}{
+			"ok":         true,
+			"count":      len(result.Servers),
+			"total":      result.Total,
+			"skipped":    result.SkippedByReason(),
+			"dns_errors": result.ResolveErrors,
+			"summary":    result.Summary(),
+		})
 	}
 }
 
@@ -299,20 +309,3 @@
 	}
 	return "", errors.New("url is required")
 }
-
-// noServersMessage explains an empty subscription. Up to three parse errors
-// are appended so the user learns why the link was rejected.
-func noServersMessage(errs []error) string {
-	const msg = "no VLESS servers found in subscription"
-	if len(errs) == 0 {
-		return msg
-	}
-	parts := make([]string, 0, 3)
-	for _, e := range errs {
-		if len(parts) == 3 {
-			break
-		}
-		parts = append(parts, e.Error())
-	}
-	return msg + ": " + strings.Join(parts, "; ")
-}
PATCH_EOF
```

- [ ] **Step 5: Remove the VLESS-only package**

```bash
git rm -r server/internal/vless
grep -rn 'internal/vless' server/ && echo "still referenced" || echo "no references left"
```
Expected: `no references left`.

- [ ] **Step 6: Run the whole Go suite**

Run: `cd server && gofmt -l ./internal/subscription ./internal/bot ./internal/handler ./internal/webapi; go vet ./... && go test -count=1 ./...`
Expected: `gofmt` prints nothing; every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add server/internal/subscription server/internal/bot/subfetch.go server/internal/bot/subfetch_test.go server/internal/handler/import.go server/internal/handler/import_test.go server/internal/webapi/handler_servers.go server/internal/webapi/handler_servers_test.go
git commit -m "feat(import): import every supported format and say what was skipped"
```

---

### Task 7: Go generator inserts the stored outbound and has Xray test it

Spec 7 and 8. `GenerateConfig` puts a stored outbound in place unread, keeps `buildOutbound` for legacy records, and renames the temp file only after `xray run -test -format json -c <temp>` exits 0. Tests use `newTestXrayService` (no Xray test) or a fake `xray` on `PATH`.

**Files:**
- Modify: `server/internal/service/xray.go`
- Test: `server/internal/service/xray_test.go`

**Interfaces:**
- Consumes: Task 1 (`vpnconfig.DecodeOutbound`).
- Produces: `XrayService.validate func(path string) error` (unexported; `NewXrayService` sets `xrayTest`); `func xrayTest(path string) error`; `func serverOutbound(server vpnconfig.Server) (interface{}, error)`; `lastLines`; `xrayTestTimeout = 30 * time.Second`. Errors read `xray rejected the config: <last three lines>`.

- [ ] **Step 1: Write the failing tests**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/service/xray_test.go
+++ b/server/internal/service/xray_test.go
@@ -3,8 +3,10 @@
 
 import (
 	"encoding/json"
+	"fmt"
 	"os"
 	"path/filepath"
+	"strings"
 	"testing"
 
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
@@ -12,6 +14,14 @@
 
 const testTemplate = `{"inbounds":[],"outbounds":[],"routing":{}}`
 
+// newTestXrayService is NewXrayService without the xray test, so a test does
+// not depend on whether the machine running it has an xray.
+func newTestXrayService(templatePath, outputPath string) *XrayService {
+	s := NewXrayService(templatePath, outputPath)
+	s.validate = nil
+	return s
+}
+
 func generate(t *testing.T, server vpnconfig.Server) map[string]interface{} {
 	t.Helper()
 	tmpDir := t.TempDir()
@@ -20,7 +30,7 @@
 	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
 		t.Fatal(err)
 	}
-	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
+	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
 		t.Fatalf("GenerateConfig error: %v", err)
 	}
 	content, err := os.ReadFile(outputPath)
@@ -142,7 +152,7 @@
 	templatePath, outputPath := writeTemplate(t)
 	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "tls", SNI: "s", Fingerprint: "chrome"}
 
-	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server, InboundPorts{TProxy: 23456, Socks: 23457}); err != nil {
+	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server, InboundPorts{TProxy: 23456, Socks: 23457}); err != nil {
 		t.Fatalf("GenerateConfig() error = %v", err)
 	}
 
@@ -156,7 +166,7 @@
 	templatePath, outputPath := writeTemplate(t)
 	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "tls", SNI: "s", Fingerprint: "chrome"}
 
-	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
+	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
 		t.Fatalf("GenerateConfig() error = %v", err)
 	}
 
@@ -208,7 +218,7 @@
 
 func TestGenerateConfig_MissingTemplate(t *testing.T) {
 	tmpDir := t.TempDir()
-	svc := NewXrayService(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "out"))
+	svc := newTestXrayService(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "out"))
 	if err := svc.GenerateConfig(vpnconfig.Server{}); err == nil {
 		t.Error("expected error for missing template")
 	}
@@ -220,7 +230,7 @@
 	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
 		t.Fatal(err)
 	}
-	svc := NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
+	svc := newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
 	if err := svc.GenerateConfig(vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Network: "ws", Security: "reality"}); err == nil {
 		t.Error("expected error for unsupported network 'ws'")
 	}
@@ -232,7 +242,7 @@
 	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
 		t.Fatal(err)
 	}
-	svc := NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
+	svc := newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
 	if err := svc.GenerateConfig(vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "xtls"}); err == nil {
 		t.Error("expected error for unsupported security 'xtls'")
 	}
@@ -245,7 +255,7 @@
 		if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
 			t.Fatal(err)
 		}
-		return NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
+		return newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
 	}
 	base := vpnconfig.Server{
 		Address: "1.2.3.4", Port: 443, UUID: "u", Security: "reality",
@@ -283,7 +293,7 @@
 	}
 
 	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u1", Security: "tls"}
-	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
+	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
 		t.Fatalf("GenerateConfig error: %v", err)
 	}
 
@@ -332,3 +342,94 @@
 		t.Errorf("log.access = %v, want %q", got, "none")
 	}
 }
+
+// An import stores the outbound it read; the generator puts it in place as
+// proxy-out without looking into it, whatever the protocol.
+func TestGenerateConfig_StoredOutbound(t *testing.T) {
+	cfg := generate(t, vpnconfig.Server{
+		Name: "Hysteria", Address: "198.51.100.11", Port: 8449,
+		Outbound: json.RawMessage(`{"protocol":"hysteria","settings":{"version":2,"address":"198.51.100.11","port":8449},` +
+			`"streamSettings":{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"a"},` +
+			`"finalmask":{"quicParams":{"congestion":"bbr"}}}}`),
+	})
+	ob := outbound0(t, cfg)
+	if ob["tag"] != "proxy-out" || ob["protocol"] != "hysteria" {
+		t.Fatalf("outbound %v", ob)
+	}
+	ss := ob["streamSettings"].(map[string]interface{})
+	if _, ok := ss["finalmask"]; !ok {
+		t.Fatalf("streamSettings %v; a key the generator does not know must pass through", ss)
+	}
+}
+
+// writeFakeXray puts an xray on PATH that prints msg and exits with code.
+func writeFakeXray(t *testing.T, code int, msg string) {
+	t.Helper()
+	dir := t.TempDir()
+	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\nexit %d\n", msg, code)
+	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte(script), 0755); err != nil {
+		t.Fatal(err)
+	}
+	t.Setenv("PATH", dir)
+}
+
+func testedService(t *testing.T) (*XrayService, string) {
+	t.Helper()
+	tmpDir := t.TempDir()
+	templatePath := filepath.Join(tmpDir, "config.json.template")
+	outputPath := filepath.Join(tmpDir, "config.json")
+	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
+		t.Fatal(err)
+	}
+	if err := os.WriteFile(outputPath, []byte("previous\n"), 0600); err != nil {
+		t.Fatal(err)
+	}
+	return NewXrayService(templatePath, outputPath), outputPath
+}
+
+var storedVLESS = vpnconfig.Server{
+	Name: "Oslo", Address: "oslo.example", Port: 443,
+	Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u","encryption":"none"}]}]}}`),
+}
+
+func TestGenerateConfig_XrayAcceptsTheConfig(t *testing.T) {
+	writeFakeXray(t, 0, "Configuration OK.")
+	svc, outputPath := testedService(t)
+	if err := svc.GenerateConfig(storedVLESS); err != nil {
+		t.Fatal(err)
+	}
+	if content, _ := os.ReadFile(outputPath); string(content) == "previous\n" {
+		t.Fatal("config.json was not replaced")
+	}
+}
+
+// A config Xray refuses would take every Xray client offline at the next
+// restart: it never replaces the running one, and Xray's own words say why.
+func TestGenerateConfig_XrayRejectsTheConfig(t *testing.T) {
+	writeFakeXray(t, 23, `Failed to start: infra/conf: "allowInsecure" has been removed`)
+	svc, outputPath := testedService(t)
+	err := svc.GenerateConfig(storedVLESS)
+	if err == nil || !strings.Contains(err.Error(), "xray rejected the config") || !strings.Contains(err.Error(), "allowInsecure") {
+		t.Fatalf("err %v", err)
+	}
+	if content, _ := os.ReadFile(outputPath); string(content) != "previous\n" {
+		t.Fatalf("config.json %q; a rejected config must not replace it", content)
+	}
+	entries, _ := os.ReadDir(filepath.Dir(outputPath))
+	for _, e := range entries {
+		if name := e.Name(); name != "config.json" && name != "config.json.template" {
+			t.Fatalf("left behind: %s", name)
+		}
+	}
+}
+
+func TestGenerateConfig_WithoutXrayNothingIsTested(t *testing.T) {
+	t.Setenv("PATH", t.TempDir())
+	svc, outputPath := testedService(t)
+	if err := svc.GenerateConfig(storedVLESS); err != nil {
+		t.Fatal(err)
+	}
+	if content, _ := os.ReadFile(outputPath); string(content) == "previous\n" {
+		t.Fatal("config.json was not replaced")
+	}
+}
PATCH_EOF
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 ./internal/service/`
Expected: FAIL to compile — `s.validate undefined`.

- [ ] **Step 3: Implement**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/service/xray.go
+++ b/server/internal/service/xray.go
@@ -2,10 +2,15 @@
 package service
 
 import (
+	"context"
 	"encoding/json"
 	"fmt"
+	"log/slog"
 	"os"
+	"os/exec"
 	"path/filepath"
+	"strings"
+	"time"
 
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
@@ -14,12 +19,58 @@
 type XrayService struct {
 	templatePath string
 	outputPath   string
+	// validate tests a written config before it replaces the live one; nil
+	// skips the test. NewXrayService sets xrayTest.
+	validate func(path string) error
 }
 
 var _ XrayGenerator = (*XrayService)(nil)
 
 func NewXrayService(templatePath, outputPath string) *XrayService {
-	return &XrayService{templatePath: templatePath, outputPath: outputPath}
+	return &XrayService{templatePath: templatePath, outputPath: outputPath, validate: xrayTest}
+}
+
+// xrayTestTimeout bounds one "xray run -test"; a router needs a second or two.
+const xrayTestTimeout = 30 * time.Second
+
+// xrayTest has Xray load the config without starting a server. The outbound
+// may come verbatim from a subscription, and it may name a protocol the
+// installed Xray lacks or a key it refuses - allowInsecure stops Xray from
+// loading any config since 2026-06-01 - and a config Xray rejects takes every
+// Xray client, and the bot, offline until the next switch. S24xray runs
+// "xray run -confdir", which loads only *.json, so the temp config.json.*
+// names its format. Without an xray on PATH - the dev mode, a workstation -
+// there is nothing to test with.
+func xrayTest(path string) error {
+	bin, err := exec.LookPath("xray")
+	if err != nil {
+		slog.Debug("xray not found, config not tested", "path", path)
+		return nil
+	}
+	ctx, cancel := context.WithTimeout(context.Background(), xrayTestTimeout)
+	defer cancel()
+	out, err := exec.CommandContext(ctx, bin, "run", "-test", "-format", "json", "-c", path).CombinedOutput()
+	if ctx.Err() != nil {
+		return fmt.Errorf("xray config test timed out after %s", xrayTestTimeout)
+	}
+	if err != nil {
+		return fmt.Errorf("xray rejected the config: %s", lastLines(string(out), 3))
+	}
+	return nil
+}
+
+// lastLines joins the last n non-empty lines of s with "; ".
+func lastLines(s string, n int) string {
+	var lines []string
+	for _, line := range strings.Split(s, "\n") {
+		if line = strings.TrimSpace(line); line != "" {
+			lines = append(lines, line)
+		}
+	}
+	if len(lines) > n {
+		lines = lines[len(lines)-n:]
+	}
+	return strings.Join(lines, "; ")
 }
 
 type xrayUser struct {
@@ -179,12 +230,31 @@
 	}
 }
 
+// serverOutbound is the proxy-out outbound of a server: the outbound its
+// import stored, tagged, or for a record from before outbounds were stored,
+// the one buildOutbound makes from the flat VLESS fields.
+func serverOutbound(server vpnconfig.Server) (interface{}, error) {
+	if len(server.Outbound) > 0 {
+		ob, err := vpnconfig.DecodeOutbound(server.Outbound)
+		if err != nil {
+			return nil, fmt.Errorf("stored outbound: %w", err)
+		}
+		ob["tag"] = "proxy-out"
+		return ob, nil
+	}
+	if err := validateStreamParams(server); err != nil {
+		return nil, err
+	}
+	return buildOutbound(server), nil
+}
+
 // GenerateConfig parses the (valid-JSON) template and replaces outbounds
-// with a single proxy-out outbound built from the server's stream params.
-// The optional ports keep the inbounds in step with advanced.xray in
-// vpn-director.json; without them the template's ports stand.
+// with the server's proxy-out outbound, then has Xray test the result before
+// it replaces config.json. The optional ports keep the inbounds in step with
+// advanced.xray in vpn-director.json; without them the template's ports stand.
 func (s *XrayService) GenerateConfig(server vpnconfig.Server, ports ...InboundPorts) error {
-	if err := validateStreamParams(server); err != nil {
+	outbound, err := serverOutbound(server)
+	if err != nil {
 		return err
 	}
 	template, err := os.ReadFile(s.templatePath)
@@ -195,7 +265,7 @@
 	if err := json.Unmarshal(template, &cfg); err != nil {
 		return fmt.Errorf("parse template: %w", err)
 	}
-	cfg["outbounds"] = []xrayOutbound{buildOutbound(server)}
+	cfg["outbounds"] = []interface{}{outbound}
 	if len(ports) > 0 {
 		applyInboundPorts(cfg, ports[0])
 	}
@@ -221,6 +291,11 @@
 	if err := tmp.Close(); err != nil {
 		return fmt.Errorf("close temp config: %w", err)
 	}
+	if s.validate != nil {
+		if err := s.validate(tmpName); err != nil {
+			return err
+		}
+	}
 	if err := os.Rename(tmpName, s.outputPath); err != nil {
 		return fmt.Errorf("rename config: %w", err)
 	}
PATCH_EOF
```

- [ ] **Step 4: Run the tests**

Run: `cd server && go vet ./internal/service/ && go test -count=1 ./internal/service/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/service/xray.go server/internal/service/xray_test.go
git commit -m "feat(xray): insert the stored outbound and let Xray test the config first"
```

---

### Task 8: The watch dials a stored outbound by IP

Spec 9. `ServerForDial` writes the resolved IPv4 into the outbound's address slot and keeps the hostname where Xray would otherwise take the name from the address: an empty TLS server name, and for a stream without security an empty ws, httpupgrade or xhttp Host. A legacy record keeps today's path.

**Files:**
- Modify: `server/internal/subwatch/watch.go` (`ServerForDial`, new `keepHostname`)
- Test: `server/internal/subwatch/watch_test.go`

**Interfaces:**
- Consumes: Task 1 (`DecodeOutbound`, `OutboundTarget`).
- Produces: `func ServerForDial(s vpnconfig.Server) vpnconfig.Server` (same signature); `func keepHostname(ob map[string]interface{}, host string)`.

- [ ] **Step 1: Write the failing tests**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/subwatch/watch_test.go
+++ b/server/internal/subwatch/watch_test.go
@@ -406,6 +406,88 @@
 	}
 }
 
+// A stored outbound gets the IP in its own address slot, whatever the
+// protocol keeps it in; the record and the outbound agree on the address.
+func TestServerForDial_WritesTheIPIntoTheOutbound(t *testing.T) {
+	for _, tc := range []struct {
+		name     string
+		outbound string
+		path     []string
+	}{
+		{"vless vnext", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u"}]}]}}`, []string{"settings", "vnext", "0", "address"}},
+		{"vless flat", `{"protocol":"vless","settings":{"address":"oslo.example","port":443,"id":"u"}}`, []string{"settings", "address"}},
+		{"trojan", `{"protocol":"trojan","settings":{"servers":[{"address":"oslo.example","port":443,"password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
+		{"shadowsocks", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"oslo.example","port":8388,"method":"aes-256-gcm","password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
+		{"hysteria", `{"protocol":"hysteria","settings":{"version":2,"address":"oslo.example","port":443}}`, []string{"settings", "address"}},
+	} {
+		t.Run(tc.name, func(t *testing.T) {
+			s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"", "203.0.113.50"}, Outbound: json.RawMessage(tc.outbound)})
+			if s.Address != "203.0.113.50" {
+				t.Fatalf("address %q", s.Address)
+			}
+			var ob interface{}
+			if err := json.Unmarshal(s.Outbound, &ob); err != nil {
+				t.Fatal(err)
+			}
+			v := ob
+			for _, key := range tc.path {
+				switch node := v.(type) {
+				case map[string]interface{}:
+					v = node[key]
+				case []interface{}:
+					v = node[0]
+				}
+			}
+			if v != "203.0.113.50" {
+				t.Fatalf("outbound %s", s.Outbound)
+			}
+		})
+	}
+}
+
+// Dialing an IP must not change the name the server is reached by: an empty
+// TLS server name gets the hostname, and so does the Host of a transport
+// without security; with TLS Xray takes that Host from the server name, and
+// an explicit value stays.
+func TestServerForDial_KeepsTheHostnameWhereTheSourceLeftItToTheAddress(t *testing.T) {
+	dial := func(address, outbound string) map[string]interface{} {
+		t.Helper()
+		s := ServerForDial(vpnconfig.Server{Address: address, IPs: []string{"203.0.113.50"}, Outbound: json.RawMessage(outbound)})
+		var ob map[string]interface{}
+		if err := json.Unmarshal(s.Outbound, &ob); err != nil {
+			t.Fatal(err)
+		}
+		return ob["streamSettings"].(map[string]interface{})
+	}
+	ss := dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"ws","security":"tls","wsSettings":{"path":"/ws"}}}`)
+	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "cdn.example" {
+		t.Fatalf("tls %v", tls)
+	}
+	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
+		t.Fatalf("ws %v; with TLS the Host follows the server name", ws)
+	}
+	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"httpupgrade","security":"none"}}`)
+	if hu := ss["httpupgradeSettings"].(map[string]interface{}); hu["host"] != "cdn.example" {
+		t.Fatalf("httpupgrade %v", hu)
+	}
+	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"ws","wsSettings":{"headers":{"Host":"front.example"}}}}`)
+	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
+		t.Fatalf("ws %v; a Host header the source set stays the Host", ws)
+	}
+	ss = dial("cdn.example", `{"protocol":"trojan","settings":{"servers":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"sni.example"}}}`)
+	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "sni.example" {
+		t.Fatalf("tls %v; an explicit server name stays", tls)
+	}
+	ss = dial("198.51.100.7", `{"protocol":"trojan","settings":{"servers":[{"address":"198.51.100.7","port":443}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
+	if _, ok := ss["tlsSettings"]; ok {
+		t.Fatalf("stream %v; an IP source has no hostname to keep", ss)
+	}
+	ss = dial("oslo.example", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443}]},"streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"serverName":"www.example.org"}}}`)
+	if _, ok := ss["xhttpSettings"]; ok {
+		t.Fatalf("stream %v; REALITY gives xhttp its Host", ss)
+	}
+}
+
 // An endpoint ban takes an address, not the name: a provider's host can resolve
 // to one the router cannot reach and another it can. The walk dialed only the
 // first, and a server whose first address was banned was rejected whole.
PATCH_EOF
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 -run 'TestServerForDial' ./internal/subwatch/`
Expected: FAIL — the outbound keeps `oslo.example`.

- [ ] **Step 3: Implement**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/subwatch/watch.go
+++ b/server/internal/subwatch/watch.go
@@ -2,6 +2,7 @@
 
 import (
 	"context"
+	"encoding/json"
 	"errors"
 	"fmt"
 	"log/slog"
@@ -1108,21 +1109,99 @@
 // entry without one stays without one and is refused as the Web UI refuses it.
 // Web UI /xray keep s.Address and let Xray resolve, so a CDN IP change still
 // works there.
+//
+// A server whose import stored its outbound gets the IP in the outbound's own
+// address slot (vpnconfig.OutboundTarget). Where the source left the name to
+// the address, dialing an IP would change it, so the hostname goes there
+// instead: an empty tlsSettings.serverName, and for a stream without security
+// an empty Host of ws, httpupgrade or xhttp - with TLS, Xray takes that Host
+// from the server name.
 func ServerForDial(s vpnconfig.Server) vpnconfig.Server {
-	host := s.Address
-	for _, ip := range s.IPs {
-		if ip == "" {
-			continue
+	ip := ""
+	for _, v := range s.IPs {
+		if v != "" {
+			ip = v
+			break
 		}
+	}
+	if ip == "" {
+		return s
+	}
+	host := s.Address
+	if len(s.Outbound) == 0 {
 		s.Address = ip
 		if s.SNI == "" && s.Security != "reality" {
 			s.SNI = host
 		}
-		break
+		return s
+	}
+	ob, err := vpnconfig.DecodeOutbound(s.Outbound)
+	if err != nil {
+		return s
+	}
+	target := vpnconfig.OutboundTarget(ob)
+	if target == nil {
+		return s
+	}
+	target["address"] = ip
+	if net.ParseIP(host) == nil {
+		keepHostname(ob, host)
+	}
+	raw, err := json.Marshal(ob)
+	if err != nil {
+		return s
 	}
+	s.Outbound = raw
+	s.Address = ip
 	return s
 }
 
+// keepHostname writes host where the stream would otherwise take the name
+// from an address that is now an IP.
+func keepHostname(ob map[string]interface{}, host string) {
+	ss, _ := ob["streamSettings"].(map[string]interface{})
+	if ss == nil {
+		return
+	}
+	switch security, _ := ss["security"].(string); security {
+	case "tls":
+		tls, _ := ss["tlsSettings"].(map[string]interface{})
+		if tls == nil {
+			tls = map[string]interface{}{}
+			ss["tlsSettings"] = tls
+		}
+		if name, _ := tls["serverName"].(string); name == "" {
+			tls["serverName"] = host
+		}
+	case "", "none":
+		key := ""
+		switch ss["network"] {
+		case "ws", "websocket":
+			key = "wsSettings"
+		case "httpupgrade":
+			key = "httpupgradeSettings"
+		case "xhttp", "splithttp":
+			key = "xhttpSettings"
+		}
+		if key == "" {
+			return
+		}
+		transport, _ := ss[key].(map[string]interface{})
+		if transport == nil {
+			transport = map[string]interface{}{}
+			ss[key] = transport
+		}
+		headers, _ := transport["headers"].(map[string]interface{})
+		if h, _ := transport["host"].(string); h != "" {
+			return
+		}
+		if h, _ := headers["Host"].(string); h != "" {
+			return
+		}
+		transport["host"] = host
+	}
+}
+
 func (w *Watch) tproxyReady() bool {
 	if w.TPROXYReady == nil {
 		return true
PATCH_EOF
```

- [ ] **Step 4: Run the tests**

Run: `cd server && go vet ./internal/subwatch/ && go test -count=1 ./internal/subwatch/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/subwatch/watch.go server/internal/subwatch/watch_test.go
git commit -m "feat(subwatch): dial a stored outbound by IP and keep its hostname"
```

---

### Task 9: Protocol labels in the Web UI and the bot

Spec 10 and 11 (the page's summary line). `GET /api/servers` answers a view without credentials; the Servers tab shows a Protocol column and the last import's `summary`; `/servers` in the bot appends ` · <label>`.

**Files:**
- Modify: `server/internal/webapi/handler_servers.go` (`serverView`, `handleListServers`); Test: `server/internal/webapi/handler_servers_test.go`
- Modify: `server/internal/handler/servers.go` (`buildServersPage`); Test: `server/internal/handler/servers_test.go`
- Modify: `web/src/types.ts`, `web/src/components/ServersTab.vue`

**Interfaces:**
- Consumes: Task 1 (`Label`), Task 6 (the import answer).
- Produces: `/api/servers` items `{name, address, port, ips, protocol}`; TypeScript `Server {name, address, port, ips, protocol}` (no `uuid`), `ImportResponse {ok, count, total, skipped, dns_errors, summary}`.

- [ ] **Step 1: Write the failing Go tests**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/webapi/handler_servers_test.go
+++ b/server/internal/webapi/handler_servers_test.go
@@ -50,6 +50,35 @@
 	}
 }
 
+// The page gets what it shows - and no credential of the record: an import
+// now stores passwords in the outbound, beside the UUID of a legacy record.
+func TestHandleListServers_ShowsTheProtocolAndNoCredentials(t *testing.T) {
+	deps := newTestDeps(t)
+	deps.Config = &mockConfig{servers: []vpnconfig.Server{
+		{Address: "legacy.example.com", Port: 443, UUID: "secret-uuid", Name: "Legacy", IPs: []string{"1.1.1.1"}, Security: "reality", PublicKey: "secret-key"},
+		{Address: "hy.example.com", Port: 8443, Name: "Hy", IPs: []string{"2.2.2.2"},
+			Outbound: json.RawMessage(`{"protocol":"hysteria","settings":{"address":"hy.example.com","port":8443},"streamSettings":{"hysteriaSettings":{"auth":"secret-auth"}}}`)},
+	}}
+	rec := httptest.NewRecorder()
+	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))
+
+	body := rec.Body.String()
+	for _, secret := range []string{"secret-uuid", "secret-key", "secret-auth", "outbound"} {
+		if strings.Contains(body, secret) {
+			t.Fatalf("response carries %q: %s", secret, body)
+		}
+	}
+	var resp struct {
+		Servers []map[string]interface{} `json:"servers"`
+	}
+	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
+		t.Fatal(err)
+	}
+	if len(resp.Servers) != 2 || resp.Servers[0]["protocol"] != "vless·reality" || resp.Servers[1]["protocol"] != "hysteria2" {
+		t.Fatalf("servers %v", resp.Servers)
+	}
+}
+
 func TestHandleListServers_Error(t *testing.T) {
 	deps := newTestDeps(t)
 	deps.Config = &mockConfig{err: errors.New("load failed")}
--- a/server/internal/handler/servers_test.go
+++ b/server/internal/handler/servers_test.go
@@ -2,6 +2,7 @@
 package handler
 
 import (
+	"encoding/json"
 	"errors"
 	"fmt"
 	"strings"
@@ -463,3 +464,21 @@
 		t.Errorf("expected callback to be acknowledged even with nil Message")
 	}
 }
+
+// A subscription now mixes protocols; the list says which one each server
+// runs on.
+func TestBuildServersPage_ShowsTheProtocol(t *testing.T) {
+	servers := []vpnconfig.Server{
+		{Name: "Oslo", Address: "oslo.example.com", IPs: []string{"1.2.3.4"},
+			Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}`)},
+		{Name: "Canada SS", Address: "ss.example.com", IPs: []string{"5.6.7.8"},
+			Outbound: json.RawMessage(`{"protocol":"shadowsocks"}`)},
+	}
+	text, _ := buildServersPage(servers, 0)
+	if !strings.Contains(text, "1\\. Oslo — oslo\\.example\\.com \\(1\\.2\\.3\\.4\\) · vless·ws·tls") {
+		t.Errorf("text %q", text)
+	}
+	if !strings.Contains(text, "2\\. Canada SS — ss\\.example\\.com \\(5\\.6\\.7\\.8\\) · ss") {
+		t.Errorf("text %q", text)
+	}
+}
PATCH_EOF
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -count=1 ./internal/webapi/ ./internal/handler/`
Expected: FAIL — the list carries `uuid`/`outbound` and no `protocol`; the bot line has no label.

- [ ] **Step 3: Implement the Go side**

```bash
git apply <<'PATCH_EOF'
--- a/server/internal/webapi/handler_servers.go
+++ b/server/internal/webapi/handler_servers.go
@@ -15,6 +15,17 @@
 	"github.com/zinin/vpn-director/server/internal/vpnconfig"
 )
 
+// serverView is a server as the Servers tab shows it. The record behind it
+// also holds the outbound and its credentials - an id, a password - and the
+// page needs none of them.
+type serverView struct {
+	Name     string   `json:"name"`
+	Address  string   `json:"address"`
+	Port     int      `json:"port"`
+	IPs      []string `json:"ips"`
+	Protocol string   `json:"protocol"`
+}
+
 // handleListServers returns a handler that lists all imported servers.
 func handleListServers(deps *Deps) http.HandlerFunc {
 	return func(w http.ResponseWriter, _ *http.Request) {
@@ -36,8 +47,16 @@
 		if cfg != nil {
 			active = cfg.Xray.ActiveServer
 		}
+		views := make([]serverView, 0, len(servers))
+		for _, s := range servers {
+			ips := s.IPs
+			if ips == nil {
+				ips = []string{}
+			}
+			views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips, Protocol: s.Label()})
+		}
 		jsonOK(w, map[string]interface{}{
-			"servers":            servers,
+			"servers":            views,
 			"active":             active,
 			"subscription_saved": cfg != nil && cfg.Xray.SubscriptionURL != "",
 		})
--- a/server/internal/handler/servers.go
+++ b/server/internal/handler/servers.go
@@ -146,11 +146,12 @@
 
 	for i := start; i < end; i++ {
 		s := servers[i]
-		sb.WriteString(fmt.Sprintf("%d\\. %s — %s \\(%s\\)\n",
+		sb.WriteString(fmt.Sprintf("%d\\. %s — %s \\(%s\\) · %s\n",
 			i+1,
 			telegram.EscapeMarkdownV2(s.Name),
 			telegram.EscapeMarkdownV2(s.Address),
-			telegram.EscapeMarkdownV2(strings.Join(s.IPs, ", "))))
+			telegram.EscapeMarkdownV2(strings.Join(s.IPs, ", ")),
+			telegram.EscapeMarkdownV2(s.Label())))
 	}
 
 	// Navigation buttons
PATCH_EOF
```

- [ ] **Step 4: Update the page**

```bash
git apply <<'PATCH_EOF'
--- a/web/src/types.ts
+++ b/web/src/types.ts
@@ -1,9 +1,11 @@
+/** A server as GET /api/servers shows it: no credentials. protocol is its
+ *  label, e.g. "vless·reality", "trojan·tls", "ss", "hysteria2". */
 export interface Server {
   name: string
   address: string
   port: number
-  uuid: string
   ips: string[]
+  protocol: string
 }
 
 export interface ClientInfo {
@@ -45,6 +47,11 @@
 export interface ImportResponse {
   ok: boolean
   count: number
+  total: number
+  skipped: Record<'unsupported' | 'composite' | 'invalid' | 'placeholder', number>
+  dns_errors: number
+  /** "Imported 32 of 40 servers: 7 composite, 1 DNS error" */
+  summary: string
 }
 
 export interface IPResponse {
--- a/web/src/components/ServersTab.vue
+++ b/web/src/components/ServersTab.vue
@@ -11,6 +11,8 @@
 const selectLoading = ref(-1)
 const importUrl = ref('')
 const error = ref('')
+// What the last import brought in and what it left out.
+const importSummary = ref('')
 
 async function loadServers() {
   loading.value = true
@@ -67,8 +69,10 @@
     return
   }
   importLoading.value = true
+  importSummary.value = ''
   try {
-    await api.importServers(url)
+    const resp = await api.importServers(url)
+    importSummary.value = resp.data.summary ?? ''
     await loadServers()
   } catch (e: any) {
     alert('Error: ' + (e.response?.data?.error || e.message))
@@ -102,6 +106,7 @@
     </div>
 
     <p v-if="error" class="error-msg">{{ error }}</p>
+    <p v-if="importSummary" style="font-size: 0.875rem;">{{ importSummary }}</p>
 
     <table v-if="servers.length > 0">
       <thead>
@@ -110,6 +115,7 @@
           <th>Name</th>
           <th>Address</th>
           <th>Port</th>
+          <th>Protocol</th>
           <th>Action</th>
         </tr>
       </thead>
@@ -122,6 +128,7 @@
           </td>
           <td>{{ server.address }}</td>
           <td>{{ server.port }}</td>
+          <td>{{ server.protocol }}</td>
           <td>
             <button
               class="btn btn-green"
PATCH_EOF
```

- [ ] **Step 5: Run the tests and the type-checked build**

Run: `(cd server && go vet ./... && go test -count=1 ./internal/webapi/ ./internal/handler/) && (cd web && npm ci && npm run build)`
Expected: Go `ok`; `vue-tsc` reports nothing and Vite prints `✓ built`.

- [ ] **Step 6: Commit**

```bash
git add server/internal/webapi/handler_servers.go server/internal/webapi/handler_servers_test.go server/internal/handler/servers.go server/internal/handler/servers_test.go web/src/types.ts web/src/components/ServersTab.vue
git commit -m "feat(webui,bot): show each server's protocol and keep credentials off the page"
```

---

### Task 10: Shell generator, Xray test and the wizard's list

Spec 7, 8 and 10 for the shell. `xrayconf_build_outbound` passes a stored outbound through; `xrayconf_validate` runs `xray run -test -format json -c <file>` and `configure.sh` calls it before `mv`; the wizard's server list shows the protocol label (`protocol_label`, since `label` is a jq keyword). A mock `xray` makes the tests independent of the machine.

**Files:**
- Modify: `router/opt/vpn-director/lib/xrayconf.sh`; Test: `router/test/unit/xrayconf.bats`
- Create: `router/test/mocks/xray` (executable)
- Modify: `router/opt/vpn-director/configure.sh`; Test: `router/test/unit/configure.bats`

**Interfaces:**
- Produces: `xrayconf_validate <file>` — 0 when Xray accepts the config or no `xray` exists (says so on stderr), 1 with `xrayconf: xray rejected the config: <last three lines>` on stderr. The mock reads `XRAY_MOCK_EXIT` (default 0), `XRAY_MOCK_OUTPUT` (default `Configuration OK.`) and `XRAY_MOCK_LOG` (where its arguments go).

- [ ] **Step 1: Add the mock**

Create `router/test/mocks/xray`:

```bash
#!/bin/bash
# Mock xray for "xray run -test -format json -c FILE": prints XRAY_MOCK_OUTPUT
# (default "Configuration OK.") and exits with XRAY_MOCK_EXIT (default 0). The
# arguments go to XRAY_MOCK_LOG when it is set.
if [[ -n ${XRAY_MOCK_LOG:-} ]]; then
    printf '%s\n' "$*" >> "$XRAY_MOCK_LOG"
fi
printf '%s\n' "${XRAY_MOCK_OUTPUT:-Configuration OK.}"
exit "${XRAY_MOCK_EXIT:-0}"
```

```bash
chmod +x router/test/mocks/xray
```

- [ ] **Step 2: Write the failing tests**

```bash
git apply <<'PATCH_EOF'
--- a/router/test/unit/xrayconf.bats
+++ b/router/test/unit/xrayconf.bats
@@ -131,3 +131,45 @@
     [ "$(printf '%s' "$output" | jq -r '.inbounds[] | select(.tag == "tproxy-in") | .port')" = "12345" ]
     [ "$(printf '%s' "$output" | jq -r '.inbounds[] | select(.tag == "socks-in") | .port')" = "12346" ]
 }
+
+# An import stores the outbound it read, whatever the protocol: it goes into
+# config.json as it is, tagged proxy-out.
+@test "build_outbound: a stored outbound gets the tag and nothing else" {
+    server='{"name":"Hy","address":"198.51.100.11","port":8449,"ips":["198.51.100.11"],"outbound":{"protocol":"hysteria","settings":{"version":2,"address":"198.51.100.11","port":8449},"streamSettings":{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"a"},"finalmask":{"quicParams":{"congestion":"bbr"}}}}}'
+    run xrayconf_build_outbound <<< "$server"
+    [ "$status" -eq 0 ]
+    [ "$(printf '%s' "$output" | jq -c 'del(.tag)' | jq -S .)" = "$(printf '%s' "$server" | jq -S .outbound)" ]
+    [ "$(printf '%s' "$output" | jq -r .tag)" = "proxy-out" ]
+}
+
+@test "generate: a stored outbound replaces the template's outbounds" {
+    server='{"address":"198.51.100.12","port":2030,"outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"198.51.100.12","port":2030,"method":"aes-256-gcm","password":"p"}]}}}'
+    run xrayconf_generate "$PROJECT_ROOT/opt/etc/xray/config.json.template" <<< "$server"
+    [ "$status" -eq 0 ]
+    [ "$(printf '%s' "$output" | jq -c '[.outbounds[] | [.tag, .protocol]]')" = '[["proxy-out","shadowsocks"]]' ]
+}
+
+@test "xrayconf_validate: a config Xray accepts passes, with -format json" {
+    export PATH="$TEST_ROOT/mocks:$PATH" XRAY_MOCK_LOG="$BATS_TEST_TMPDIR/xray.log"
+    printf '{}' > "$BATS_TEST_TMPDIR/config.json.AbC123"
+    run xrayconf_validate "$BATS_TEST_TMPDIR/config.json.AbC123"
+    [ "$status" -eq 0 ]
+    [ "$(cat "$XRAY_MOCK_LOG")" = "run -test -format json -c $BATS_TEST_TMPDIR/config.json.AbC123" ]
+}
+
+@test "xrayconf_validate: a config Xray rejects fails with Xray's last lines" {
+    export PATH="$TEST_ROOT/mocks:$PATH" XRAY_MOCK_EXIT=23
+    export XRAY_MOCK_OUTPUT=$'Xray 26.2.6\nFailed to start: infra/conf: "allowInsecure" has been removed'
+    printf '{}' > "$BATS_TEST_TMPDIR/config.json.AbC123"
+    run xrayconf_validate "$BATS_TEST_TMPDIR/config.json.AbC123"
+    [ "$status" -eq 1 ]
+    [[ $output == *'xray rejected the config: Xray 26.2.6; Failed to start: infra/conf: "allowInsecure" has been removed'* ]]
+}
+
+@test "xrayconf_validate: without an xray the config passes, and says so" {
+    [[ ! -x /opt/sbin/xray ]] || skip "this machine has /opt/sbin/xray"
+    printf '{}' > "$BATS_TEST_TMPDIR/config.json.AbC123"
+    PATH="/usr/bin:/bin" run xrayconf_validate "$BATS_TEST_TMPDIR/config.json.AbC123"
+    [ "$status" -eq 0 ]
+    [[ $output == *"xray not found"* ]]
+}
--- a/router/test/unit/configure.bats
+++ b/router/test/unit/configure.bats
@@ -305,6 +305,56 @@
     assert_output "address,name,port"
 }
 
+# An import stores the server's outbound; the wizard writes it into config.json.
+@test "step_generate_configs: writes the stored outbound of the selected server" {
+    load_wizard
+    write_daemon_config
+    SELECTED_SERVER_JSON='{"name":"Canada SS","address":"198.51.100.12","port":2030,"ips":["198.51.100.12"],"outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"198.51.100.12","port":2030,"method":"aes-256-gcm","password":"p"}]}}}'
+
+    run step_generate_configs
+
+    assert_success
+    run jq -c '[.outbounds[] | [.tag, .protocol]]' "$XRAY_CONFIG_DIR/config.json"
+    assert_output '[["proxy-out","shadowsocks"]]'
+}
+
+# A config Xray rejects would take every Xray client offline at the next
+# restart; the one that runs stays.
+@test "step_generate_configs: a config Xray rejects leaves config.json alone" {
+    load_wizard
+    write_daemon_config
+    printf '%s\n' '{"outbounds":["previous"]}' > "$XRAY_CONFIG_DIR/config.json"
+    export XRAY_MOCK_EXIT=23 XRAY_MOCK_OUTPUT='Failed to start: bad key'
+
+    run step_generate_configs
+
+    assert_failure
+    assert_output --partial "xray rejected the config: Failed to start: bad key"
+    run cat "$XRAY_CONFIG_DIR/config.json"
+    assert_output '{"outbounds":["previous"]}'
+    [[ -z "$(find "$XRAY_CONFIG_DIR" -name 'config.json.??????')" ]]
+}
+
+@test "step_select_xray_server: lists each server with its protocol" {
+    load_wizard
+    cat > "$SERVERS_FILE" <<'JSON'
+[{"name":"Legacy","address":"legacy.example.com","port":443,"ips":["1.2.3.4"],"security":"reality"},
+ {"name":"Old TLS","address":"old.example.com","port":443,"ips":["1.2.3.5"]},
+ {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
+ {"name":"Canada SS","address":"ss.example.com","port":2030,"ips":["1.2.3.7"],"outbound":{"protocol":"shadowsocks"}},
+ {"name":"Gaming","address":"hy.example.com","port":8443,"ips":["1.2.3.8"],"outbound":{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}}]
+JSON
+
+    run step_select_xray_server <<< "3"
+
+    assert_success
+    assert_output --partial "1) Legacy [vless·reality]"
+    assert_output --partial "2) Old TLS [vless·tls]"
+    assert_output --partial "3) Oslo WS [vless·ws·tls]"
+    assert_output --partial "4) Canada SS [ss]"
+    assert_output --partial "5) Gaming [hysteria2]"
+}
+
 # ============================================================================
 # The tunnel prompt lists what the platform has (Merlin here: rt_tables fixture)
 # ============================================================================
PATCH_EOF
```

- [ ] **Step 3: Run them to see them fail**

Run: `bats router/test/unit/xrayconf.bats router/test/unit/configure.bats`
Expected: FAIL — `xrayconf_validate: command not found`; a stored outbound is refused for its missing `uuid`; the list shows no labels.

- [ ] **Step 4: Implement**

```bash
git apply <<'PATCH_EOF'
--- a/router/opt/vpn-director/lib/xrayconf.sh
+++ b/router/opt/vpn-director/lib/xrayconf.sh
@@ -1,18 +1,26 @@
 #!/usr/bin/env bash
 ###############################################################################
 # lib/xrayconf.sh - Build Xray proxy-out outbound + config.json from a server
-# JSON object (as stored in servers.json). Pure jq transforms; no side effects
-# on source. Used by configure.sh; unit-tested via bats.
+# JSON object (as stored in servers.json), and have Xray test the result.
+# Pure jq transforms; no side effects on source. Used by configure.sh;
+# unit-tested via bats.
 # Mirrors the Go generator in server/internal/service/xray.go — keep both in sync.
 ###############################################################################
 
 # xrayconf_build_outbound: reads one server JSON object on stdin,
-# prints the proxy-out outbound JSON object on stdout. Fails (rc=1) on an
-# unsupported network (non-tcp) or security (not tls/reality) instead of
-# emitting a silently-broken outbound. Self-contained: pure jq; errors to stderr.
+# prints the proxy-out outbound JSON object on stdout. A server an import
+# stored with its outbound gets that outbound, tagged: the import
+# (lib/subscription.sh) checked it. A record from before outbounds were stored
+# is built from its flat VLESS fields, and fails (rc=1) on an unsupported
+# network (non-tcp) or security (not tls/reality) instead of emitting a
+# silently-broken outbound. Self-contained: pure jq; errors to stderr.
 xrayconf_build_outbound() {
     local server_json net sec
     server_json="$(cat)"
+    if printf '%s' "$server_json" | jq -e 'type == "object" and (.outbound | type) == "object"' >/dev/null 2>&1; then
+        printf '%s' "$server_json" | jq '.outbound + {tag: "proxy-out"}'
+        return
+    fi
     if ! printf '%s' "$server_json" | jq -e 'type == "object" and (.address // "") != "" and (.uuid // "") != ""' >/dev/null 2>&1; then
         printf 'xrayconf: invalid/empty server JSON (need object with address and uuid)\n' >&2
         return 1
@@ -92,3 +100,29 @@
            end' \
         "$template"
 }
+
+# xrayconf_validate <file>: has Xray load a generated config without starting
+# it, as XrayService.GenerateConfig does (service/xray.go). An outbound can come
+# verbatim from a subscription and name a protocol the installed Xray lacks or
+# a key it refuses - allowInsecure stops Xray from loading any config since
+# 2026-06-01 - and a config Xray rejects takes every Xray client offline at the
+# next restart. S24xray runs "xray run -confdir", which loads only *.json, so a
+# mktemp name needs -format json. Without an xray there is nothing to test
+# with: that is said on stderr, and the config passes.
+xrayconf_validate() {
+    local file="$1" xray out
+    xray=$(type -P xray 2>/dev/null) || xray=""
+    if [[ -z $xray && -x /opt/sbin/xray ]]; then
+        xray=/opt/sbin/xray
+    fi
+    if [[ -z $xray ]]; then
+        printf 'xrayconf: xray not found; %s was not tested\n' "$file" >&2
+        return 0
+    fi
+    if ! out=$("$xray" run -test -format json -c "$file" 2>&1); then
+        printf 'xrayconf: xray rejected the config: %s\n' "$(printf '%s\n' "$out" | awk '
+            NF { line[++n] = $0 }
+            END { for (i = (n > 3 ? n - 2 : 1); i <= n; i++) printf "%s%s", line[i], (i < n ? "; " : "") }')" >&2
+        return 1
+    fi
+}
--- a/router/opt/vpn-director/configure.sh
+++ b/router/opt/vpn-director/configure.sh
@@ -169,11 +169,23 @@
 
     printf "Available servers:\n\n"
 
-    # Read servers from JSON and display
+    # Read servers from JSON and display. The label names the protocol, as the
+    # Web UI and the bot do (vpnconfig.Server.Label): a record without an
+    # outbound is a legacy VLESS one, generated as TLS when it names no
+    # security.
     i=1
-    jq -r '.[] | "\(.name)|\(.address)|\((.ips // []) | join(", "))"' "$SERVERS_FILE" | \
-    while IFS='|' read -r name address ip; do
-        printf "  %2d) %s\n      %s -> %s\n\n" "$i" "$name" "$address" "$ip"
+    jq -r '
+        def protocol_label:
+          (if (.outbound | type) == "object"
+           then [.outbound.protocol, .outbound.streamSettings.network, .outbound.streamSettings.security]
+           else ["vless", .network, (if (.security // "") == "" then "tls" else .security end)] end)
+          | map(. // "") as [$p, $n, $s]
+          | if $p == "shadowsocks" then "ss" elif $p == "hysteria" then "hysteria2"
+            else [$p] + (if $n == "" or $n == "tcp" or $n == "raw" then [] else [$n] end)
+                      + (if $s == "" or $s == "none" then [] else [$s] end) | join("·") end;
+        .[] | "\(.name)|\(.address)|\((.ips // []) | join(", "))|\(protocol_label)"' "$SERVERS_FILE" | \
+    while IFS='|' read -r name address ip label; do
+        printf "  %2d) %s [%s]\n      %s -> %s\n\n" "$i" "$name" "$label" "$address" "$ip"
         i=$((i + 1))
     done
 
@@ -551,19 +563,20 @@
     # Generate to a temp file first, then replace atomically. A '>' redirect would
     # truncate the live config.json before xrayconf_generate runs, so a generator
     # failure (bad params, jq/template error) would leave the router with an empty
-    # config. Write-then-mv keeps the existing config intact on any failure.
+    # config. Write-then-mv keeps the existing config intact on any failure, and
+    # one Xray rejects never replaces it (xrayconf_validate).
     _xray_cfg_tmp=$(mktemp "$XRAY_CONFIG_DIR/config.json.XXXXXX")
     if printf '%s' "$SELECTED_SERVER_JSON" \
         | xrayconf_generate "$XRAY_CONFIG_DIR/config.json.template" \
             "$_tproxy_port" "$_socks_port" \
-        > "$_xray_cfg_tmp"; then
+        > "$_xray_cfg_tmp" && xrayconf_validate "$_xray_cfg_tmp"; then
         mv -f "$_xray_cfg_tmp" "$XRAY_CONFIG_DIR/config.json"
         print_success "Generated $XRAY_CONFIG_DIR/config.json"
     else
         rm -f "$_xray_cfg_tmp"
         flock -u 9
         exec 9>&-
-        print_error "Failed to generate Xray config (invalid server params?); kept existing config.json"
+        print_error "Failed to generate an Xray config for this server, or Xray rejected it; kept existing config.json"
         exit 1
     fi
 
PATCH_EOF
```

- [ ] **Step 5: Run the tests**

Run: `bats router/test/unit/xrayconf.bats router/test/unit/configure.bats`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add router/opt/vpn-director/lib/xrayconf.sh router/opt/vpn-director/configure.sh router/test/mocks/xray router/test/unit/xrayconf.bats router/test/unit/configure.bats
git commit -m "feat(xrayconf): insert the stored outbound and let Xray test the config first"
```

---

### Task 11: The shell importer on the decoder

Spec 5 (shell), 11 (shell messages). `import_server_list.sh` keeps its prompt, download, resolution and publication under the config lock, and leaves reading the list to `subscription_decode`. The tests of the removed VLESS parser go; the shared cases cover what they covered.

**Files:**
- Modify: `router/opt/vpn-director/import_server_list.sh`; Test: `router/test/import_server_list.bats`
- Modify: `install.sh` (the next-steps text)

**Interfaces:**
- Consumes: Task 5 (`subscription_decode`).
- Produces: `step_get_subscription` (sets `SUB_INPUT`, `SUB_RESULT`; one `WARN` per skipped entry), `step_parse_servers` (resolves `SUB_RESULT` into `SERVERS_TMP`; logs `Found N servers in M entries (…)`), `step_publish_servers` (unchanged, now reads `SUB_INPUT`).

- [ ] **Step 1: Write the failing tests**

```bash
git apply <<'PATCH_EOF'
--- a/router/test/import_server_list.bats
+++ b/router/test/import_server_list.bats
@@ -2,370 +2,61 @@
 
 load 'test_helper'
 
-# Test URI for basic parsing (ASCII name, no special chars)
-TEST_URI_BASIC='vless://11111111-2222-3333-4444-555555555555@server1.test.example:8443?type=tcp&security=tls#Prague, Czechia'
-
-# URI with emoji flag + cyrillic name
-TEST_URI_EMOJI_CYRILLIC='vless://11111111-2222-3333-4444-555555555555@server2.test.example:8443?type=tcp&security=tls#🇷🇺 Россия, Москва'
-
-# URI with only emoji (should fallback to hostname)
-TEST_URI_EMOJI_ONLY='vless://11111111-2222-3333-4444-555555555555@server3.test.example:8443?type=tcp&security=tls#🇺🇸🌟✨'
-
-# URI whose fragment is a truncated two-byte sequence followed by a field separator
-# (percent-encoded \xC3|evil): the filter must not carry the | into the output
-TEST_URI_TRUNCATED_UTF8='vless://11111111-2222-3333-4444-555555555555@server2.test.example:8443?type=tcp&security=tls#%C3%7Cevil'
-
-# URI with URL-encoded spaces
-TEST_URI_URLENCODED='vless://11111111-2222-3333-4444-555555555555@server4.test.example:8443?type=tcp&security=tls#New%20York%20City'
-
-# URI with cyrillic only (no emoji)
-TEST_URI_CYRILLIC='vless://11111111-2222-3333-4444-555555555555@server5.test.example:8443?type=tcp&security=tls#Казахстан, Алматы'
-
-# What a real subscription actually sends: the whole fragment percent-encoded,
-# flag emoji included. Decoding only %20 left the hex digits of every other
-# byte in the name, and the filter below keeps digits - so servers.json held
-# "F09F87B3F09F87B1 D090D0BC..." instead of a country.
-TEST_URI_PERCENT_ENCODED='vless://11111111-2222-3333-4444-555555555555@server6.test.example:8443?type=tcp&security=tls#%F0%9F%87%B3%F0%9F%87%B1%20%D0%90%D0%BC%D1%81%D1%82%D0%B5%D1%80%D0%B4%D0%B0%D0%BC%2C%20%D0%9D%D0%B8%D0%B4%D0%B5%D1%80%D0%BB%D0%B0%D0%BD%D0%B4%D1%8B%2C%20Extra'
-
-# ============================================================================
-# parse_vless_uri: Field extraction
-# ============================================================================
-
-@test "parse_vless_uri: extracts server hostname" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_BASIC")
-    server=$(printf '%s' "$result" | cut -d'|' -f1)
-    [ "$server" = "server1.test.example" ]
-}
-
-@test "parse_vless_uri: extracts port number" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_BASIC")
-    port=$(printf '%s' "$result" | cut -d'|' -f2)
-    [ "$port" = "8443" ]
-}
-
-@test "parse_vless_uri: extracts UUID" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_BASIC")
-    uuid=$(printf '%s' "$result" | cut -d'|' -f3)
-    [ "$uuid" = "11111111-2222-3333-4444-555555555555" ]
-}
-
-@test "parse_vless_uri: extracts ASCII name" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_BASIC")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "Prague, Czechia" ]
-}
-
-@test "parse_vless_uri: decodes a percent-encoded name" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_PERCENT_ENCODED")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "Амстердам, Нидерланды, Extra" ]
-}
-
-# The routers have no UTF-8 locale - KeeneticOS has no locale at all - so awk
-# works on bytes there. A character-wise filter splits a two-byte letter in
-# half and keeps whichever byte happens to match an ASCII class, which turned
-# "Амстердам" into "Амс?е?дам" on the device while passing here.
-@test "parse_vless_uri: keeps the name intact in the routers' byte locale" {
-    load_import_server_list
-    result=$(LC_ALL=C parse_vless_uri "$TEST_URI_PERCENT_ENCODED")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "Амстердам, Нидерланды, Extra" ]
-
-    result=$(LC_ALL=C parse_vless_uri "$TEST_URI_EMOJI_CYRILLIC")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "Россия, Москва" ]
-}
-
-@test "parse_vless_uri extracts reality stream params" {
-    load_import_server_list
-    # Real subscription format includes headerType=none before type=tcp — guards the
-    # `_vless_query_get type` lookup against matching `headerType`.
-    uri='vless://uuid@1.2.3.4:443?security=reality&encryption=none&fp=firefox&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=cdn3-87.yahoo.com&pbk=PBKEY&sid=55e6#NL'
-    run parse_vless_uri "$uri"
-    [ "$status" -eq 0 ]
-    # fields: server|port|uuid|name|security|network|flow|sni|fp|pbk|sid|alpn
-    [ "$(printf '%s' "$output" | cut -d'|' -f5)" = "reality" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f6)" = "tcp" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f7)" = "xtls-rprx-vision" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f8)" = "cdn3-87.yahoo.com" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f9)" = "firefox" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f10)" = "PBKEY" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f11)" = "55e6" ]
-}
-
-@test "parse_vless_uri keeps core fields without params" {
-    load_import_server_list
-    run parse_vless_uri 'vless://uuid@1.2.3.4:443#Name'
-    [ "$status" -eq 0 ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f1)" = "1.2.3.4" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f3)" = "uuid" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f5)" = "" ]
-}
-
-# ============================================================================
-# parse_vless_uri: Name handling
-# ============================================================================
-
-@test "parse_vless_uri: filters emoji from name, keeps cyrillic" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_EMOJI_CYRILLIC")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    # Emoji flag should be removed, cyrillic preserved
-    [ "$name" = "Россия, Москва" ]
-}
-
-@test "parse_vless_uri: a truncated two-byte sequence does not smuggle a field separator" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_TRUNCATED_UTF8")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    security=$(printf '%s' "$result" | cut -d'|' -f5)
-    # The lone \xC3 is dropped with its non-continuation byte, so no | shifts the fields
-    [ "$name" = "evil" ]
-    [ "$security" = "tls" ]
-}
-
-@test "parse_vless_uri: falls back to hostname when name is only emoji" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_EMOJI_ONLY")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    # All emoji filtered out, should fallback to server hostname
-    [ "$name" = "server3.test.example" ]
-}
-
-@test "parse_vless_uri: decodes URL-encoded spaces" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_URLENCODED")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "New York City" ]
-}
-
-@test "parse_vless_uri: handles cyrillic-only name" {
-    load_import_server_list
-    result=$(parse_vless_uri "$TEST_URI_CYRILLIC")
-    name=$(printf '%s' "$result" | cut -d'|' -f4)
-    [ "$name" = "Казахстан, Алматы" ]
-}
+# The decoding itself - every scheme, container and skip reason - is
+# router/test/unit/subscription.bats, on the cases the Go importer shares.
+# This file is the script around it: resolution, the report and publication.
 
 # ============================================================================
-# decode_vless_content: Format detection
+# step_parse_servers: resolution and the list it leaves for publication
 # ============================================================================
 
-@test "decode_vless_content: detects plaintext format (single URI)" {
-    load_import_server_list
-    content="vless://uuid@server:443?type=tcp#Name"
-    result=$(decode_vless_content "$content")
-    [ "$result" = "$content" ]
-}
-
-@test "decode_vless_content: detects plaintext format (multiple URIs)" {
-    load_import_server_list
-    content="vless://uuid1@server1:443?type=tcp#Name1
-vless://uuid2@server2:443?type=tcp#Name2"
-    result=$(decode_vless_content "$content")
-    [ "$result" = "$content" ]
+# decode_into_result reads a subscription the way step_get_subscription does.
+decode_into_result() {
+    SUB_RESULT=$(printf '%s' "$1" | subscription_decode)
 }
 
-@test "decode_vless_content: handles plaintext with leading empty lines" {
-    load_import_server_list
-    content="
-
-vless://uuid@server:443?type=tcp#Name"
-    result=$(decode_vless_content "$content")
-    [ "$result" = "$content" ]
-}
-
-@test "decode_vless_content: decodes base64 format" {
-    load_import_server_list
-    plaintext="vless://uuid@server:443?type=tcp#Name"
-    encoded=$(printf '%s' "$plaintext" | base64)
-    result=$(decode_vless_content "$encoded")
-    [ "$result" = "$plaintext" ]
-}
-
-@test "decode_vless_content: decodes base64 with multiple URIs" {
-    load_import_server_list
-    plaintext="vless://uuid1@server1:443#Name1
-vless://uuid2@server2:443#Name2"
-    encoded=$(printf '%s' "$plaintext" | base64)
-    result=$(decode_vless_content "$encoded")
-    [ "$result" = "$plaintext" ]
-}
-
-@test "decode_vless_content: fails on invalid content" {
-    load_import_server_list
-    run decode_vless_content "not-base64-and-not-vless!!!"
-    assert_failure
-}
-
-@test "decode_vless_content: fails on whitespace-only content" {
-    load_import_server_list
-    run decode_vless_content "
-
-    "
-    assert_failure
-}
-
-@test "decode_vless_content: decodes valid base64 even if not VLESS (validation is downstream)" {
-    load_import_server_list
-    plaintext="just some random text"
-    encoded=$(printf '%s' "$plaintext" | base64)
-    result=$(decode_vless_content "$encoded")
-    # Function succeeds - content validation is handled downstream
-    [ "$result" = "$plaintext" ]
-}
-
-@test "decode_vless_content: decodes URL-safe base64 alphabet" {
-    load_import_server_list
-    # "????" standard base64 is "Pz8/Pw=="; the URL-safe form replaces / with _.
-    # The standard decoder rejects _, so this exercises the url-safe fallback.
-    # Use command substitution (not run) so the log line on stderr is excluded.
-    result=$(decode_vless_content "Pz8_Pw==")
-    [ "$result" = "????" ]
-}
-
-@test "decode_vless_content: url-safe base64 maps both - and _ (full alphabet)" {
-    load_import_server_list
-    # ">>>???" standard base64 is "Pj4+Pz8/" (contains BOTH + and /); the URL-safe
-    # form replaces + with - and / with _, so this exercises the full -_ -> +/
-    # mapping. A reversed set (e.g. tr '_-' '+/') decodes to the wrong bytes.
-    result=$(decode_vless_content "Pj4-Pz8_")
-    [ "$result" = ">>>???" ]
-}
-
-# ============================================================================
-# parse_vless_uri: IPv6 literal host
-# ============================================================================
-
-@test "parse_vless_uri: parses bracketed IPv6 host and port" {
-    load_import_server_list
-    run parse_vless_uri 'vless://uuid@[2001:db8::1]:443?type=tcp#v6'
-    [ "$status" -eq 0 ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f1)" = "2001:db8::1" ]
-    [ "$(printf '%s' "$output" | cut -d'|' -f2)" = "443" ]
-}
-
-# ============================================================================
-# _redact_uri: mask UUID for DEBUG logging
-# ============================================================================
-
-@test "_redact_uri: masks UUID and strips fragment, keeps host" {
-    load_import_server_list
-    run _redact_uri 'vless://11111111-2222-3333-4444-555555555555@server.example:443?type=tcp#MyName'
-    [ "$status" -eq 0 ]
-    # UUID is a secret and must not leak into logs
-    [[ "$output" != *11111111-2222-3333-4444-555555555555* ]]
-    # host:port and params are kept; the #fragment is stripped
-    [[ "$output" == *server.example:443* ]]
-    [[ "$output" != *MyName* ]]
-}
-
-# ============================================================================
-# _url_decode / _vless_query_get: percent-decoding (valid %XX like url.ParseQuery;
-# lenient on malformed % — Go rejects, shell keeps literal; see ticket #41)
-# ============================================================================
-
-@test "_url_decode: decodes %XX escapes" {
-    load_import_server_list
-    result=$(_url_decode 'h2%2Chttp/1.1')
-    [ "$result" = "h2,http/1.1" ]
-}
-
-@test "_url_decode: maps + to space, leaves lone/incomplete % intact" {
-    load_import_server_list
-    # Lenient on malformed % (Go's url.ParseQuery would reject these); see #41.
-    [ "$(_url_decode 'a+b')" = "a b" ]
-    [ "$(_url_decode '50%')" = "50%" ]
-    [ "$(_url_decode 'x%2y')" = "x%2y" ]
-}
-
-@test "_vless_query_get: URL-decodes the value (valid %XX like url.ParseQuery)" {
-    load_import_server_list
-    result=$(_vless_query_get 'type=tcp&alpn=h2%2Chttp/1.1' alpn)
-    [ "$result" = "h2,http/1.1" ]
-}
-
-@test "parse_vless_uri: URL-decodes percent-encoded alpn into comma list" {
-    load_import_server_list
-    result=$(parse_vless_uri 'vless://uuid@1.2.3.4:443?type=tcp&alpn=h2%2Chttp/1.1#N')
-    [ "$(printf '%s' "$result" | cut -d'|' -f12)" = "h2,http/1.1" ]
-}
-
-# ============================================================================
-# step_parse_servers: JSON output
-# ============================================================================
-
 # step_parse_servers leaves the list in $SERVERS_TMP for step_publish_servers,
 # so these call it directly: "run" would keep the variable in its subshell.
-@test "step_parse_servers: saves ips array instead of ip" {
+@test "step_parse_servers: every server keeps its outbound and gets its IPs" {
     load_import_server_list
-
-    DATA_DIR="/tmp/bats_test_import_data"
-    mkdir -p "$DATA_DIR"
-
-    # Override VPD_CONFIG to a temp config with our data_dir
-    VPD_CONFIG="/tmp/bats_test_import_data/vpn-director.json"
-    printf '{"data_dir": "%s"}\n' "$DATA_DIR" > "$VPD_CONFIG"
-
-    VLESS_SERVERS="vless://test-uuid@example.com:443?type=tcp#TestServer"
+    VPD_CONFIG="$BATS_TEST_TMPDIR/vpn-director.json"
+    printf '{"data_dir":"%s"}' "$BATS_TEST_TMPDIR/data" > "$VPD_CONFIG"
+    decode_into_result 'vless://test-uuid@example.com:443?security=reality&flow=xtls-rprx-vision&sni=cdn.example.com&pbk=PBK&fp=firefox&sid=sid1#TestServer'
 
     step_parse_servers
 
-    # Check that the list has "ips" array, not "ip" string
-    result=$(jq -r '.[0].ips | type' "$SERVERS_TMP")
-    [ "$result" = "array" ]
-
-    # Check that "ip" field does not exist
-    result=$(jq -r '.[0] | has("ip")' "$SERVERS_TMP")
-    [ "$result" = "false" ]
-
-    # Check the resolved IP is in the ips array
-    result=$(jq -r '.[0].ips[0]' "$SERVERS_TMP")
-    [ "$result" = "93.184.216.34" ]
-
-    rm -rf "$DATA_DIR"
+    run jq -c '.[0] | [.name, .address, .port, .ips]' "$SERVERS_TMP"
+    assert_output '["TestServer","example.com",443,["93.184.216.34"]]'
+    run jq -c '.[0].outbound.streamSettings.realitySettings | [.publicKey, .shortId, .serverName]' "$SERVERS_TMP"
+    assert_output '["PBK","sid1","cdn.example.com"]'
+    run jq -c '.[0] | [has("uuid"), has("security"), has("ip")]' "$SERVERS_TMP"
+    assert_output '[false,false,false]'
 }
 
-@test "step_parse_servers writes reality params to the list" {
+@test "step_parse_servers: a server whose host does not resolve is left out and counted" {
     load_import_server_list
+    VPD_CONFIG="$BATS_TEST_TMPDIR/vpn-director.json"
+    printf '{"data_dir":"%s"}' "$BATS_TEST_TMPDIR/data" > "$VPD_CONFIG"
+    decode_into_result $'vless://u@nx.example.com:443#Gone\nvless://u@5.6.7.8:443#Good\nvless://u@[2001:db8::1]:443#Six\ntuic://u:p@5.6.7.9:443#TUIC'
 
-    tmp_data="$BATS_TEST_TMPDIR/data"
-    mkdir -p "$tmp_data"
-    # get_data_dir reads VPD_CONFIG/VPD_TEMPLATE; override to a temp config
-    cfg="$BATS_TEST_TMPDIR/vpn-director.json"
-    printf '{"data_dir":"%s"}' "$tmp_data" > "$cfg"
-    VPD_CONFIG="$cfg"
-    VLESS_SERVERS='vless://uuid@1.2.3.4:443?security=reality&flow=xtls-rprx-vision&sni=cdn.example.com&pbk=PBK&sid=sid1&type=tcp#NL'
-    step_parse_servers
-    out="$SERVERS_TMP"
-    [ "$(jq -r '.[0].security' "$out")" = "reality" ]
-    [ "$(jq -r '.[0].flow' "$out")" = "xtls-rprx-vision" ]
-    [ "$(jq -r '.[0].public_key' "$out")" = "PBK" ]
-    [ "$(jq -r '.[0].short_id' "$out")" = "sid1" ]
-    [ "$(jq -r '.[0].sni' "$out")" = "cdn.example.com" ]
-    [ "$(jq -r '.[0] | has("alpn")' "$out")" = "false" ]
+    step_parse_servers 2>"$BATS_TEST_TMPDIR/err"
+
+    run jq -c '[.[].name]' "$SERVERS_TMP"
+    assert_output '["Good"]'
+    run cat "$LOG_FILE"
+    assert_output --partial "Found 1 servers in 4 entries (1 unsupported, 2 DNS errors)"
 }
 
-@test "step_parse_servers skips out-of-range port, keeps valid server" {
+@test "step_get_subscription: logs every skipped entry with its reason" {
     load_import_server_list
+    printf '%s\n' 'vless://u@5.6.7.8:443?type=kcp#KCP' 'vless://u@5.6.7.9:443#Good' > "$BATS_TEST_TMPDIR/list.txt"
 
-    tmp_data="$BATS_TEST_TMPDIR/data"
-    mkdir -p "$tmp_data"
-    cfg="$BATS_TEST_TMPDIR/vpn-director.json"
-    printf '{"data_dir":"%s"}' "$tmp_data" > "$cfg"
-    VPD_CONFIG="$cfg"
-    # First URI has an out-of-range port (must be skipped); the second is valid
-    # and must still be saved (one bad entry does not drop the rest).
-    VLESS_SERVERS=$'vless://uuid@1.2.3.4:99999?type=tcp#Bad\nvless://uuid@5.6.7.8:443?type=tcp#Good'
-    step_parse_servers
-    out="$SERVERS_TMP"
-    [ "$(jq length "$out")" -eq 1 ]
-    [ "$(jq -r '.[0].address' "$out")" = "5.6.7.8" ]
-    [ "$(jq -r '.[0].port' "$out")" -eq 443 ]
+    step_get_subscription <<< "$BATS_TEST_TMPDIR/list.txt"
+
+    run cat "$LOG_FILE"
+    assert_output --partial "Skipping KCP: unsupported (transport kcp)"
+    run jq -c '[.servers[].name]' <<< "$SUB_RESULT"
+    assert_output '["Good"]'
 }
 
 # ============================================================================
@@ -543,3 +234,39 @@
     run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
     assert_output '["Old"]'
 }
+
+# The new provider serves no links at all: its subscription is an array of
+# Xray configs. The proxy outbound of each is the server; a config with more
+# than one proxy is skipped as composite.
+@test "import_server_list.sh imports an Xray JSON subscription" {
+    load_import_into
+    write_imported_state
+    cat > "$BATS_TEST_TMPDIR/servers.txt" <<'JSON'
+[{"remarks": "Oslo", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.10", "port": 443, "password": "p"}]}, "streamSettings": {"network": "tcp", "security": "tls"}}, {"tag": "direct", "protocol": "freedom"}]},
+ {"remarks": "Auto", "outbounds": [{"protocol": "vless", "settings": {"address": "198.51.100.11", "port": 443, "id": "u"}}, {"protocol": "vless", "settings": {"address": "198.51.100.12", "port": 443, "id": "u"}}]}]
+JSON
+
+    run_import "$BATS_TEST_TMPDIR/servers.txt"
+
+    assert_success
+    assert_output --partial "Skipping Auto: composite (2 proxy outbounds)"
+    run jq -c '[.[] | [.name, .outbound.protocol, .outbound.settings.servers[0].password, .ips]]' "$VPD_DIR/data/servers.json"
+    assert_output '[["Oslo","trojan","p",["198.51.100.10"]]]'
+    run jq -c '.xray.servers' "$VPD_CONFIG"
+    assert_output '["198.51.100.10"]'
+}
+
+# An HTML page - what some panels answer a browser with - is no subscription;
+# the list the router has stays.
+@test "import_server_list.sh refuses a body it cannot read and keeps the previous list" {
+    load_import_into
+    write_imported_state
+    printf '%s\n' '<!doctype html><html><body>Open this link in your VPN app</body></html>' > "$BATS_TEST_TMPDIR/servers.txt"
+
+    run_import "$BATS_TEST_TMPDIR/servers.txt"
+
+    assert_failure
+    assert_output --partial "Cannot read the subscription: unrecognized subscription format"
+    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
+    assert_output '["Old"]'
+}
PATCH_EOF
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/import_server_list.bats`
Expected: FAIL — `subscription_decode: command not found` in the new tests.

- [ ] **Step 3: Implement**

```bash
git apply <<'PATCH_EOF'
--- a/router/opt/vpn-director/import_server_list.sh
+++ b/router/opt/vpn-director/import_server_list.sh
@@ -26,15 +26,19 @@
 fi
 
 ###############################################################################
-# import_server_list.sh - Import VLESS servers from file/URL
-# Supports both plaintext and base64-encoded VLESS URI lists
-# Run after install.sh to download and parse server list
+# import_server_list.sh - Import servers from a subscription URL or a file:
+# share links (vless, vmess, trojan, ss, hysteria2), base64 or plain, or Xray
+# JSON. lib/subscription.sh reads the list - the same rules the bot and the
+# Web UI import with - and this script resolves the servers and publishes the
+# list under the config lock. Run after install.sh.
 ###############################################################################
 
 # Source common utilities (use BASH_SOURCE for correct path when sourced)
 SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
 # shellcheck source=lib/common.sh
 . "$SCRIPT_DIR/lib/common.sh"
+# shellcheck source=lib/subscription.sh
+. "$SCRIPT_DIR/lib/subscription.sh"
 
 # Paths; VPD_DIR and VPD_CONFIG can be set by the caller, as configure.sh's can
 VPD_DIR="${VPD_DIR:-/opt/vpn-director}"
@@ -50,190 +54,6 @@
     read -r INPUT_RESULT
 }
 
-# Decode VLESS content from base64 or return as-is if plaintext
-# Input: raw content as $1
-# Output: decoded VLESS URIs to stdout
-# Returns: 0 on success, 1 on decode failure
-decode_vless_content() {
-    local content="$1"
-    local first_line
-
-    # Get first non-empty line
-    first_line=$(printf '%s\n' "$content" | grep -v '^[[:space:]]*$' | head -n1)
-
-    if [[ "$first_line" == vless://* ]]; then
-        log "Detected plaintext format"
-        printf '%s' "$content"
-    else
-        log "Detected base64 format, decoding..."
-        # Standard alphabet first; fall back to URL-safe base64 (RFC 4648 §5):
-        # map url-safe -_ to standard +/ and pad to a multiple of 4, then decode.
-        # The tr sets are written '_-' '/+' (dash LAST in SET1, no leading '-' and
-        # no '--') so busybox tr parses them as operands, not options, while the
-        # _->/ and -->+ mapping is preserved. The Go importer accepts url-safe
-        # blobs too, so this keeps shell/Go parity.
-        local decoded b64 pad
-        if decoded=$(printf '%s' "$content" | base64 -d 2>/dev/null); then
-            printf '%s' "$decoded"
-            return 0
-        fi
-        b64=$(printf '%s' "$content" | tr -d '\r\n\t ' | tr '_-' '/+')
-        [[ -n "$b64" ]] || return 1
-        case $(( ${#b64} % 4 )) in
-            2) pad='==' ;;
-            3) pad='=' ;;
-            1) return 1 ;;
-            *) pad='' ;;
-        esac
-        printf '%s%s' "$b64" "$pad" | base64 -d 2>/dev/null || return 1
-    fi
-}
-
-###############################################################################
-# VLESS URI Parser
-###############################################################################
-
-# URL-decode %XX escapes and "+" (space). Matches Go url.ParseQuery on VALID
-# escapes. NOTE: not exact parity on malformed input — Go rejects the whole
-# query on a bad %-escape, whereas this leaves a lone/incomplete/non-hex %
-# intact (more lenient). Real subscriptions are URL-safe, so the two agree on
-# current data; see ticket #41.
-# busybox-safe: validate each %XX as hex via sed, rewrite to \xHH, then let
-# printf '%b' emit the bytes. Backslashes in the data are escaped first so
-# printf cannot misinterpret them. The data is the %b ARGUMENT (never the
-# format string), so a literal % is safe.
-_url_decode() {
-    local s="${1//+/ }"
-    s=$(printf '%s' "$s" | sed -e 's/\\/\\\\/g' -e 's/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')
-    printf '%b' "$s"
-}
-
-# Extract a query parameter value (no external tools), URL-decoded via
-# _url_decode (matches Go url.ParseQuery on valid %XX; lenient on malformed %).
-# _vless_query_get <query_string> <key> -> prints value (empty if absent)
-_vless_query_get() {
-    local q="&$1&" v
-    case "$q" in
-        *"&$2="*)
-            v="${q#*"&$2="}"
-            _url_decode "${v%%&*}"
-            ;;
-    esac
-}
-
-# Redact the UUID credential from a VLESS URI for safe DEBUG logging. Strips the
-# #fragment and replaces the "uuid@" credential with "***@".
-_redact_uri() {
-    local u="${1%%#*}"
-    case "$u" in
-        vless://*@*) printf 'vless://***@%s' "${u#vless://*@}" ;;
-        *)           printf '%s' "$u" ;;
-    esac
-}
-
-# Parse a single VLESS URI and extract components
-# Format: vless://uuid@server:port?params#name
-# Output: pipe-separated fields
-#   server|port|uuid|name|security|network|flow|sni|fp|pbk|sid|alpn
-parse_vless_uri() {
-    local uri="$1"
-    local rest raw_name name uuid server_port server port query
-    local security network flow sni fp pbk sid alpn
-
-    # Remove vless:// prefix
-    rest="${uri#vless://}"
-
-    # Extract name (after #, URL-decoded). Guard against URIs without a
-    # #fragment so the query string is not captured as the name.
-    if [[ "$rest" == *#* ]]; then
-        raw_name="${rest##*#}"
-    else
-        raw_name=""
-    fi
-    # Percent-decode the fragment. A subscription encodes the whole name, so
-    # decoding only %20 left every other byte as its own hex digits - which the
-    # filter below keeps, being digits. That is how "Амстердам" reached
-    # servers.json as "D090D0BCD181D182...". Backslashes are escaped first
-    # because printf %b expands them; then %XX becomes \xXX for %b to turn back
-    # into the byte. A decoded newline would split the record, so drop those.
-    raw_name=$(printf '%s' "$raw_name" |
-        sed 's/\\/\\\\/g; s/+/ /g; s/%\([0-9a-fA-F][0-9a-fA-F]\)/\\x\1/g')
-    raw_name=$(printf '%b' "$raw_name" | tr -d '\n\r')
-
-    # Keep letters, digits, spaces and basic punctuation; drop emoji.
-    # Byte-oriented, and pinned to LC_ALL=C on purpose: the routers have no
-    # UTF-8 locale - KeeneticOS has no locale at all - so awk works on bytes
-    # there. A character-wise filter cuts a two-byte letter in half and keeps
-    # whichever half matches an ASCII class, which turned "Амстердам" into
-    # "Амс?е?дам" on the device while passing on a workstation. Two-byte
-    # sequences are the alphabets (Cyrillic, Greek, accented Latin); the
-    # three- and four-byte ones are the symbols and emoji this drops.
-    name=$(printf '%s' "$raw_name" | LC_ALL=C gawk '{
-        out = ""
-        n = length($0)
-        i = 1
-        while (i <= n) {
-            c = substr($0, i, 1)
-            if (c ~ /[a-zA-Z0-9 .,;:!?()-]/) { out = out c; i++; continue }
-            if (c ~ /[\xC0-\xDF]/) { if (substr($0, i + 1, 1) ~ /[\x80-\xBF]/) { out = out substr($0, i, 2); i += 2 } else { i++ }; continue }
-            if (c ~ /[\xE0-\xEF]/) { i += 3; continue }
-            if (c ~ /[\xF0-\xF4]/) { i += 4; continue }
-            i++
-        }
-        gsub(/^[ ,]+|[ ,]+$/, "", out)
-        print out
-    }')
-    rest="${rest%%#*}"
-
-    # Extract UUID (before @)
-    uuid="${rest%%@*}"
-    rest="${rest#*@}"
-
-    # Extract server:port (before ?). Handle bracketed IPv6 literals
-    # ([2001:db8::1]:443) by splitting on the ] delimiter, not the colon.
-    # NOTE: the stored address drops the brackets (2001:db8::1); the Go parser
-    # (vless/parser.go) keeps them ([2001:db8::1]). Both are equivalent to Xray,
-    # whose ParseAddress strips brackets for the standalone address field.
-    server_port="${rest%%\?*}"
-    case "$server_port" in
-        \[*)
-            server="${server_port#\[}"
-            server="${server%%\]*}"
-            port="${server_port##*\]:}"
-            [[ "$port" == "$server_port" ]] && port=""
-            ;;
-        *)
-            server="${server_port%%:*}"
-            port="${server_port##*:}"
-            ;;
-    esac
-
-    # Extract query string (after ?), then stream params
-    if [[ "$rest" == *\?* ]]; then
-        query="${rest#*\?}"
-    else
-        query=""
-    fi
-
-    security=$(_vless_query_get "$query" security)
-    network=$(_vless_query_get "$query" type)
-    flow=$(_vless_query_get "$query" flow)
-    sni=$(_vless_query_get "$query" sni)
-    fp=$(_vless_query_get "$query" fp)
-    pbk=$(_vless_query_get "$query" pbk)
-    sid=$(_vless_query_get "$query" sid)
-    alpn=$(_vless_query_get "$query" alpn)
-
-    # Fallback: if name is empty after filtering, use server hostname
-    if [[ -z "$name" ]]; then
-        name="$server"
-    fi
-
-    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
-        "$server" "$port" "$uuid" "$name" \
-        "$security" "$network" "$flow" "$sni" "$fp" "$pbk" "$sid" "$alpn"
-}
-
 ###############################################################################
 # Get data directory from config
 ###############################################################################
@@ -255,140 +75,112 @@
 }
 
 ###############################################################################
-# Step 1: Get VLESS file
+# Step 1: Get the subscription
 ###############################################################################
 
-step_get_vless_file() {
-    log -l TRACE "Step 1: VLESS Server List"
+# Reads the subscription into SUB_RESULT, the Result JSON of
+# subscription_decode, and logs every entry it skipped.
+step_get_subscription() {
+    log -l TRACE "Step 1: Subscription"
 
-    printf "Enter path to VLESS file or URL:\n"
-    printf "(Supports plaintext or base64-encoded VLESS URIs)\n\n"
+    printf "Enter a subscription URL or the path to a file:\n"
+    printf "(Share links - vless, vmess, trojan, ss, hysteria2 - base64 or plain, or Xray JSON)\n\n"
 
-    read_input "Path or URL"
-    VLESS_INPUT="$INPUT_RESULT"
+    read_input "URL or path"
+    SUB_INPUT="$INPUT_RESULT"
 
-    if [[ -z "$VLESS_INPUT" ]]; then
+    if [[ -z "$SUB_INPUT" ]]; then
         log -l ERROR "No input provided"
         exit 1
     fi
 
-    # Check if it's a URL or file path
-    case "$VLESS_INPUT" in
+    local content
+    case "$SUB_INPUT" in
         http://*|https://*)
             log "Downloading from URL..."
-            VLESS_CONTENT=$(curl -fsSL --connect-timeout 10 --max-time 60 "$VLESS_INPUT") || {
-                log -l ERROR "Failed to download from $VLESS_INPUT"
+            content=$(curl -fsSL --connect-timeout 10 --max-time 60 "$SUB_INPUT") || {
+                log -l ERROR "Failed to download from $SUB_INPUT"
                 exit 1
             }
             ;;
         *)
-            if [[ ! -f "$VLESS_INPUT" ]]; then
-                log -l ERROR "File not found: $VLESS_INPUT"
+            if [[ ! -f "$SUB_INPUT" ]]; then
+                log -l ERROR "File not found: $SUB_INPUT"
                 exit 1
             fi
-            VLESS_CONTENT=$(cat "$VLESS_INPUT")
+            content=$(cat "$SUB_INPUT")
             ;;
     esac
 
-    # Decode content (auto-detect format: plaintext or base64)
-    VLESS_DECODED=$(decode_vless_content "$VLESS_CONTENT" 2>/dev/null) || {
-        log -l ERROR "Failed to decode content (not valid base64 or plaintext VLESS URIs)"
+    local err
+    err=$(tmp_file)
+    if ! SUB_RESULT=$(printf '%s' "$content" | subscription_decode 2>"$err"); then
+        log -l ERROR "Cannot read the subscription: $(cat "$err")"
         exit 1
-    }
+    fi
 
-    # Count servers
-    SERVER_COUNT=$(printf '%s\n' "$VLESS_DECODED" | grep -c '^vless://' || true)
+    local skipped line
+    skipped=$(printf '%s' "$SUB_RESULT" | jq -r '.skipped[] | "Skipping \(.name): \(.reason) (\(.detail))"')
+    if [[ -n $skipped ]]; then
+        while IFS= read -r line; do
+            log -l WARN "$line"
+        done <<< "$skipped"
+    fi
 
-    if [[ "$SERVER_COUNT" -eq 0 ]]; then
-        log -l ERROR "No VLESS servers found in file"
+    if [[ $(printf '%s' "$SUB_RESULT" | jq '.servers | length') -eq 0 ]]; then
+        log -l ERROR "No supported servers in subscription"
         exit 1
     fi
-
-    log "Found $SERVER_COUNT VLESS servers"
-    VLESS_SERVERS="$VLESS_DECODED"
 }
 
 ###############################################################################
-# Step 2: Parse servers
+# Step 2: Resolve servers
 ###############################################################################
 
 # The list goes to a temp file, and step 3 publishes it. Resolving takes
 # seconds per host, and servers.json written in place sat empty for all of
 # them - and was gone when none resolved.
 step_parse_servers() {
-    log -l TRACE "Step 2: Parsing Servers"
+    log -l TRACE "Step 2: Resolving Servers"
 
     DATA_DIR=$(get_data_dir)
     SERVERS_FILE="$DATA_DIR/servers.json"
     SERVERS_TMP=$(tmp_file)
 
-    # Parse servers, resolve IPs, emit one JSON object per server, slurp to array
-    printf '%s\n' "$VLESS_SERVERS" | grep '^vless://' | while IFS= read -r uri; do
-        log -l DEBUG "URI: $(_redact_uri "$uri")"
-
-        parsed=$(parse_vless_uri "$uri")
-        server=$(printf '%s' "$parsed" | cut -d'|' -f1)
-        port=$(printf '%s' "$parsed" | cut -d'|' -f2)
-        uuid=$(printf '%s' "$parsed" | cut -d'|' -f3)
-        name=$(printf '%s' "$parsed" | cut -d'|' -f4)
-        security=$(printf '%s' "$parsed" | cut -d'|' -f5)
-        network=$(printf '%s' "$parsed" | cut -d'|' -f6)
-        flow=$(printf '%s' "$parsed" | cut -d'|' -f7)
-        sni=$(printf '%s' "$parsed" | cut -d'|' -f8)
-        fp=$(printf '%s' "$parsed" | cut -d'|' -f9)
-        pbk=$(printf '%s' "$parsed" | cut -d'|' -f10)
-        sid=$(printf '%s' "$parsed" | cut -d'|' -f11)
-        alpn=$(printf '%s' "$parsed" | cut -d'|' -f12)
-
-        if [[ -z "$server" ]] || [[ -z "$port" ]] || [[ -z "$uuid" ]]; then
-            log -l WARN "Skipping invalid URI (missing server/port/uuid)"
-            continue
-        fi
-        # Reject non-numeric and out-of-range ports here so a broken entry never
-        # lands in servers.json. 10# forces base-10 so a zero-padded port (e.g.
-        # 0443) is not misread as octal. Arithmetic in an if-condition is exempt
-        # from set -e, and 10#$port only runs once $port is known all-digit.
-        if ! printf '%s' "$port" | grep -qE '^[0-9]+$' || (( 10#$port < 1 || 10#$port > 65535 )); then
-            log -l WARN "Skipping $server: invalid port '$port'"
-            continue
-        fi
-
-        ips_raw=$(resolve_ip -a -q "$server" 2>/dev/null) || ips_raw=""
+    local records server address name ips_raw ips_json dns_errors=0
+    records=$(tmp_file)
+    while IFS= read -r server; do
+        address=$(printf '%s' "$server" | jq -r '.address')
+        name=$(printf '%s' "$server" | jq -r '.name')
+        ips_raw=$(resolve_ip -a -q "$address" 2>/dev/null) || ips_raw=""
         if [[ -z "$ips_raw" ]]; then
-            log -l WARN "Cannot resolve $server, skipping"
+            log -l WARN "Cannot resolve $address, skipping"
+            dns_errors=$((dns_errors + 1))
             continue
         fi
-        ips_oneline=$(printf '%s' "$ips_raw" | tr '\n' ',' | sed 's/,$//')
-        printf "  %s (%s) -> %s\n" "$name" "$server" "$ips_oneline" >&2
-
+        printf "  %s (%s) -> %s\n" "$name" "$address" "$(printf '%s' "$ips_raw" | tr '\n' ',' | sed 's/,$//')" >&2
         ips_json=$(printf '%s\n' "$ips_raw" | jq -R 'select(length > 0)' | jq -s .)
-        if [[ -n "$alpn" ]]; then
-            alpn_json=$(printf '%s' "$alpn" | tr ',' '\n' | jq -R 'select(length > 0)' | jq -s .)
-        else
-            alpn_json='[]'
-        fi
-
-        jq -c -n \
-            --arg address "$server" --argjson port "$port" --arg uuid "$uuid" --arg name "$name" \
-            --argjson ips "$ips_json" \
-            --arg security "$security" --arg network "$network" --arg flow "$flow" \
-            --arg sni "$sni" --arg fingerprint "$fp" --arg public_key "$pbk" --arg short_id "$sid" \
-            --argjson alpn "$alpn_json" \
-            '{address:$address, port:$port, uuid:$uuid, name:$name, ips:$ips,
-              security:$security, network:$network, flow:$flow, sni:$sni,
-              fingerprint:$fingerprint, public_key:$public_key, short_id:$short_id, alpn:$alpn}
-             | with_entries(select(.value != null and .value != "" and .value != []))'
-    done | jq -s '.' > "$SERVERS_TMP"
-
-    # Validate JSON
-    if ! jq empty "$SERVERS_TMP" 2>/dev/null; then
-        log -l ERROR "Generated invalid JSON"
-        cat "$SERVERS_TMP"
-        exit 1
-    fi
+        printf '%s' "$server" | jq -c --argjson ips "$ips_json" '. + {ips: $ips}' >> "$records"
+    done <<< "$(printf '%s' "$SUB_RESULT" | jq -c '.servers[]')"
+    jq -s '.' "$records" > "$SERVERS_TMP"
 
     SERVER_COUNT=$(jq length "$SERVERS_TMP")
 
+    # The counts in the order the bot and the Web UI give them
+    # (subscription.Import.Counts).
+    local counts
+    counts=$(printf '%s' "$SUB_RESULT" | jq -r --argjson dns "$dns_errors" '
+        (reduce .skipped[].reason as $r ({}; .[$r] += 1)) as $c
+        | [ (("unsupported", "composite", "invalid") as $r | select(($c[$r] // 0) > 0) | "\($c[$r]) \($r)"),
+            (($c.placeholder // 0) | if . == 1 then "1 placeholder" elif . > 1 then "\(.) placeholders" else empty end),
+            ($dns | if . == 1 then "1 DNS error" elif . > 1 then "\(.) DNS errors" else empty end) ]
+        | join(", ")')
+    if [[ -n $counts ]]; then
+        log "Found $SERVER_COUNT servers in $(printf '%s' "$SUB_RESULT" | jq '.total') entries ($counts)"
+    else
+        log "Found $SERVER_COUNT servers"
+    fi
+
     if [[ "$SERVER_COUNT" -eq 0 ]]; then
         log -l ERROR "No servers could be resolved"
         exit 1
@@ -415,7 +207,7 @@
 # published, and nothing refreshes it.
 step_publish_servers() {
     local link='del(.xray.subscription_url)'
-    if [[ $VLESS_INPUT == https://* ]]; then
+    if [[ $SUB_INPUT == https://* ]]; then
         # shellcheck disable=SC2016  # $url is jq's, set with --arg below
         link='.xray.subscription_url = $url'
     fi
@@ -446,7 +238,7 @@
     chmod 600 "$list"
     if [[ -f $VPD_CONFIG ]]; then
         config=$(mktemp "$VPD_CONFIG.XXXXXX")
-        if ! jq --argjson ips "$ips" --arg url "$VLESS_INPUT" ".xray.servers = \$ips | $link" \
+        if ! jq --argjson ips "$ips" --arg url "$SUB_INPUT" ".xray.servers = \$ips | $link" \
             "$VPD_CONFIG" > "$config"; then
             rm -f "$list" "$config"
             flock -u 9
@@ -469,10 +261,10 @@
 ###############################################################################
 
 main() {
-    log -l TRACE "Import VLESS Server List"
-    printf "This will download and parse VLESS servers.\n\n"
+    log -l TRACE "Import Server List"
+    printf "This will download and parse the servers of a subscription.\n\n"
 
-    step_get_vless_file
+    step_get_subscription
     step_parse_servers
     step_publish_servers
 
--- a/install.sh
+++ b/install.sh
@@ -591,7 +591,7 @@
     print_header "Installation Complete ($RELEASE_TAG)"
 
     printf "Next steps:\n\n"
-    printf "  1. Import VLESS servers:\n"
+    printf "  1. Import the servers of your subscription:\n"
     printf "     ${GREEN}/opt/vpn-director/import_server_list.sh${NC}\n\n"
     printf "  2. Run configuration wizard:\n"
     printf "     ${GREEN}/opt/vpn-director/configure.sh${NC}\n\n"
PATCH_EOF
```

- [ ] **Step 4: Run the tests**

Run: `bats router/test/import_server_list.bats router/test/unit/entrypoints.bats router/test/unit/install.bats`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/import_server_list.sh router/test/import_server_list.bats install.sh
git commit -m "feat(import): read every supported format in import_server_list.sh"
```

---

### Task 12: Documentation

Spec 14. The architecture table, the Xray outbound section, the bot and Web UI facts, the shared cases, and what the READMEs promise.

**Files:**
- Modify: `CLAUDE.md`, `.claude/rules/xray-tproxy.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`, `.claude/rules/testing.md`, `README.md`, `README.ru.md`

- [ ] **Step 1: Apply the documentation changes**

````bash
git apply <<'PATCH_EOF'
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -58,7 +58,9 @@
 | `router/opt/vpn-director/lib/ipset.sh` | IPSet module: ensure, update, status |
 | `router/opt/vpn-director/lib/tunnel.sh` | Tunnel Director module: apply, stop, status |
 | `router/opt/vpn-director/lib/tproxy.sh` | Xray TPROXY module: apply, stop, status |
-| `router/opt/vpn-director/lib/xrayconf.sh` | Build Xray outbound (REALITY/TLS) + config.json from a server |
+| `router/opt/vpn-director/lib/xrayconf.sh` | Xray config.json from a server's stored outbound (legacy VLESS records built), `xray run -test` before it replaces the live one |
+| `router/opt/vpn-director/lib/subscription.sh` | Subscription decoder: share links (vless, vmess, trojan, ss, hysteria2), base64 or plain, and Xray JSON, into servers with a ready outbound |
+| `testdata/subscription/` | Cases both subscription decoders (shell and Go) must decode alike |
 | `router/opt/etc/init.d/S99vpn-director` | Entware init.d script for startup |
 | `router/jffs/scripts/firewall-start` | Asuswrt-Merlin hook for firewall reload |
 | `router/jffs/scripts/wan-event` | Asuswrt-Merlin hook for WAN events |
@@ -69,6 +71,7 @@
 | `server/cmd/bot/main.go` | Telegram bot daemon: DI, signal handling |
 | `server/cmd/webui/main.go` | Web UI daemon: HTTPS server, DI, dev mode |
 | `server/internal/webapi/` | HTTP API: router, JWT middleware, handlers, response deadlines |
+| `server/internal/subscription/` | Go subscription decoder, the twin of `lib/subscription.sh`; resolution and import summaries |
 | `server/internal/auth/` | Password check against the platform password file, JWT issue and validation |
 | `web/` | Vue 3 SPA, embedded into the webui binary with `go:embed` |
 | `router/opt/vpn-director/setup_telegram_bot.sh` | Bot configuration script |
--- a/.claude/rules/xray-tproxy.md
+++ b/.claude/rules/xray-tproxy.md
@@ -29,29 +29,38 @@
 3. Remaining traffic → TPROXY to Xray port
 4. Xray dokodemo-door inbound → VLESS outbound
 
-## Outbound Generation (REALITY/TLS)
+## Outbound Generation
 
-`config.json.template` is valid JSON with an empty `outbounds: []`. The proxy-out outbound
-is generated per selected server from `servers.json` stream params and injected as `outbounds[0]`:
+`config.json.template` is valid JSON with an empty `outbounds: []`. The selected server's
+outbound goes into it as `outbounds[0]`, tagged `proxy-out`:
 
 - Shell: `lib/xrayconf.sh` (`xrayconf_generate`, jq) — used by `configure.sh`.
-- Go: `service/xray.go` (`buildOutbound` + `encoding/json`) — used by bot wizard and `/xray`.
+- Go: `service/xray.go` (`serverOutbound` + `encoding/json`) — used by the Web UI, the bot wizard,
+  `/xray` and the subscription watch.
 
-Per-server params (parsed from the VLESS URI): `security` (`reality`|`tls`), `network`, `flow`,
-`sni`, `fingerprint` (fp), `public_key` (pbk), `short_id` (sid), `alpn`.
+An import stores each server's Xray outbound in `servers.json` (`outbound`): converted from a share
+link, or taken from an Xray JSON subscription and sanitized — the decoders are `lib/subscription.sh`
+and `server/internal/subscription`, which answer to the same cases in `testdata/subscription/`. The
+generators insert it unread, so any protocol and transport Xray runs works: VLESS (tcp, ws, grpc,
+httpupgrade, xhttp), VMess, Trojan, Shadowsocks, Hysteria2.
+
+A record without `outbound` predates stored outbounds: the generators build a VLESS outbound from
+its flat fields — `security` (`reality`|`tls`), `network`, `flow`, `sni`, `fingerprint`,
+`public_key`, `short_id`, `alpn`:
 
 - `security=reality` -> `realitySettings { serverName, fingerprint, publicKey, shortId }`, user `flow`.
 - `security=tls` -> `tlsSettings { serverName (sni||address), fingerprint?, alpn? }`.
 - empty `security` -> legacy `tlsSettings { alpn:["h2"], serverName:address }`, no flow.
 
-**Migration after upgrade:** the previously generated `/opt/etc/xray/config.json` stays on disk as
-plain TLS until regenerated, so Xray — and the Telegram bot, which proxies its API connection
-through it — cannot connect to a REALITY server until the user acts:
-
-1. Re-run `/import` (or, over SSH if the bot is unreachable through the broken proxy:
-   `/opt/vpn-director/import_server_list.sh`) so params land in `servers.json`.
-2. Re-select the server (`/configure` wizard or `/xray`, or over SSH `/opt/vpn-director/configure.sh`)
-   to regenerate `config.json`.
+The next import rewrites such records; nothing converts them.
+
+**Every config is tested before it replaces the live one:** `xray run -test -format json -c <temp>`
+(`xrayconf_validate` in shell, `xrayTest` in Go). An outbound from a subscription can name a protocol
+the installed Xray lacks, or a key it refuses — since 2026-06-01 Xray loads no config with
+`tlsSettings.allowInsecure: true` — and a config Xray rejects would take every Xray client, and the
+bot, offline at the next restart. The temp file (`config.json.XXXXXX`) does not end in `.json`, which
+keeps `xray -confdir` from loading it and is why `-format json` is needed. Without an `xray` binary
+(dev mode, a workstation) the test is skipped.
 
 ## Configuration
 
--- a/.claude/rules/telegram-bot.md
+++ b/.claude/rules/telegram-bot.md
@@ -72,8 +72,8 @@
 │   │   ├── script.go         # Update script generation
 │   │   ├── version.go        # Semantic version comparison and validation
 │   │   └── update_script.sh.tmpl # The script rendered from the daemon table
-│   ├── vless/                # VLESS protocol
-│   │   └── parser.go         # VLESS URL parser, subscription decoder
+│   ├── subscription/         # Subscription decoder (twin of lib/subscription.sh)
+│   │   └── decode.go         # Decode: share links, Xray JSON; resolution, summaries
 │   ├── vpnconfig/            # VPN Director config
 │   │   └── vpnconfig.go      # vpn-director.json, servers.json
 │   ├── webapi/               # Web UI HTTP API — see webui.md
@@ -91,7 +91,7 @@
 | `/status` | `StatusHandler.HandleStatus` | VPN Director status |
 | `/xray` | `XrayHandler.HandleXray` | Quick server switch (see below) |
 | `/servers` | `ServersHandler.HandleServers` | Server list (paginated) |
-| `/import [url]` | `ImportHandler.HandleImport` | Import VLESS subscription (saved URL if omitted; a body over 1 MiB is refused) |
+| `/import [url]` | `ImportHandler.HandleImport` | Import a subscription (saved URL if omitted; a body over 1 MiB is refused); reports what it skipped and why |
 | `/configure` | `WizardHandler.HandleConfigure` | Configuration wizard |
 | `/restart` | `StatusHandler.HandleRestart` | Restart VPN Director |
 | `/stop` | `StatusHandler.HandleStop` | Stop VPN Director |
@@ -186,7 +186,7 @@
 
 Armed when `xray.subscription_url` is saved and there are effective Xray clients (after subtracting `paused_clients`). A `xray.failover` record arms it with or without a saved link — `import_server_list.sh` clears the link for a list from a file or a plain-http link, typically while Xray is down — and so does a restore whose last apply has not succeeded: without a link the watch still restores the clients, follows the fallback tunnel and says so, but refreshes nothing, and it starts no failover of its own. `--dev` does not start it. The watch starts with the bot process, before Telegram `getMe` succeeds: a down Xray outbound must not block recovery. There is still no watch without the bot daemon. Notifications that fire before `getMe` are queued and flushed when the sender is published. Failover stages TUN_DIR while the clients remain in `xray.clients`, then drops them from Xray only after the fallback tunnel is in `TUN_DIR_TABLES` and `failover_ready` (route, ip rule and PREROUTING jumps installed, every failover client's MARK rule in `TUN_DIR` — an apply that finds one gone rebuilds the chain first). `tunnel_apply` returns 0 with a WARN when that tunnel is not carrying traffic, so boot and hooks do not fail. An already committed failover still refreshes the subscription if that tunnel cannot be reapplied. `tunnel_apply` returns 1 when the failover tunnel's route or ip rule is not installed, so the watch does not drop Xray membership; it still records the config hash so the next apply does not `tunnel_stop` (which would take TUN_DIR down for every client). `tunnel_apply` does not fail the failover readiness check when the fallback tunnel has no effective clients (all paused or deleted). A restore whose fallback tunnel key is gone does not put those addresses back on Xray. Restore puts snapshot addresses back on Xray and removes from the tunnel only those that were appended at stage time (`failover.added`); addresses that were already on that tunnel stay there.
 
-Every 30s the bot probes `https://www.gstatic.com/generate_204` through Xray SOCKS (`127.0.0.1:<socks_port>`, default 12346). Success is HTTP 204. After 3 minutes of consecutive failures it moves those clients onto the first Tunnel Director exit (same filter as PathManager: not `main`, has clients, platform lists it connected with an iface; no Telegram probe). `tunnel.sh` emits MARK rules for `xray.failover.clients` first so overlapping earlier rules (including `main`) do not send those snapshot addresses to WAN; other tunnels keep JSON order. It then refreshes the saved subscription (SSRF WAN first, then `DialPath` through the tunnel the clients are on — `xray.failover.tunnel` while it is still an exit, otherwise the first exit (`vpnconfig.FailoverTDExit`); VLESS hostnames resolve over the same path that fetched the body, IPv4 only and bound to the tick's context (`vless.LookupIPv4`, the default for `/import` and the Web UI import too): an AF_UNSPEC lookup of every host in a subscription waits out the unanswered AAAA half, five seconds each, and a stop could not end it. A body that arrives over the WAN resolves each host with the WAN resolver and, for a host it does not answer, with the tunnel's, which asks 8.8.8.8 over the interface; one none of whose hosts answered on either falls through to the tunnel's own download; the watch walk dials those IPv4 addresses, every address of a server in turn before the next server (`perAddress`: a ban takes an address, not the name), while Web UI and `/xray` keep the hostname in vnext so CDN/DDNS still resolves), and tries the server the user chose first — the entry with its name, address and port, or else the first entry with its name, since a subscription that rotates endpoints gives a name a new address every day — then the rest in list order. The walk's own records keep that choice in `xray.preferred_server` for as long as `active_server` names another server (`vpnconfig.RecordWalkedServer`, from the first record that leaves its name until one comes back to it), so a walk cut short by a bot restart or a stop starts the next wave from the user's server and returns to it; a selection in the Web UI, `/xray` or either wizard ends it. A live SOCKS probe restores the addresses that were taken off Xray, including those that already sat on the fallback tunnel — while failed over the health probe runs every tick, so a recovered outbound or a Web UI Select does not wait on the subscription host. Restore stages onto Xray only snapshot addresses still on the fallback tunnel, and neither stages nor drops tunnel membership until `tproxy_apply` has written `/tmp/xray_tproxy/ready` (SOCKS 204 does not prove LAN TPROXY; a soft-fail apply must not strip TUN_DIR). Waiting before the stage is the point: the PREROUTING jumps are installed even when the platform's own rules are not, and Xray wins over TUN_DIR, so a client put back into `xray.clients` too early is intercepted by a TPROXY that cannot carry it while the tunnel it is still on carries nothing. While the marker is missing the clients stay on the fallback tunnel and only the apply is retried, on the import cadence. The marker is checked once more after the apply that drops the tunnel membership, because that apply is the one that has to keep TPROXY up: if it soft-fails, the restored addresses go back onto the tunnel as a committed failover (`vpnconfig.ApplyFailoverSnapshot`, only what the restore moved) and the restore is retried once the marker returns, rather than leaving them with neither the proxy nor the tunnel and nothing to try again. The marker also needs the platform's own rules: on Keenetic a missing mangle INPUT accept drops proxied HTTPS in `_NDM_HTTP_INPUT_TLS_`, which the SOCKS probe cannot see. A full `vpn-director.sh stop` (bot `/stop`, `POST /api/stop`) writes `/tmp/vpn-director/stopped` before it tears anything down; a full `apply`, `restart` and `update` remove it — the firewall hooks and the daily update included, `apply --dry-run` not, and neither does an apply or restart of one component (`restart xray` after a Web UI server switch turns Xray back on, not the rest). The watch does nothing while that file exists. Its own applies and Xray restarts pass `--unless-stopped`, which the script checks right after taking the lock, so a stop that finishes mid-tick, or takes the lock ahead of a queued watch apply, stays in force; the watch also re-checks the marker after every wait, a restart the script skipped included, and ends the tick without writes or messages. The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between. While a tick waits it looks for the marker every second (`stopPoll`) and cancels what it is waiting on, so a stop does not sit out a download, the resolution behind it or a probe. One download and the resolution of every host in it share a 3-minute deadline (`FetchTimeout`), and a fetch whose context ended part-way through the resolution returns the context's error rather than the servers resolved so far: a list cut short is never published. The wait for the config lock is covered the same way for every config write of the watch: the guard the walk hands `Generate`, and `Watch.update` for the stage, commit, restore, retarget and publication writes, check the marker first, under that lock, so a stop that finishes while a write is queued refuses it instead of leaving it for the next manual apply. A deleted or wizard-moved address is not put back. If TPROXY cannot be installed, apply retries on the import cadence, not every 30s. It publishes `servers.json` and `xray.servers` in one config-lock update (`vpnconfig.PublishServers`), as the Web UI and `/import` do, so two importers cannot leave one file from each. `import_server_list.sh` publishes under the same lock: it resolves the list into a temp file, writes nothing until it holds the lock, then replaces `servers.json`, `xray.servers` and the saved link together. An https link is saved, and a file or a plain-http link — neither of which the watch or the Web UI fetches — clears the saved one, so the next refresh does not bring the old link's list back. An import that cannot get the lock within 30 seconds, or whose list has no usable server, leaves the previous one whole. That update also checks, under the same lock, that `xray.subscription_url` is still the link this wave downloaded: a subscription saved while the download was in flight abandons the wave instead of publishing a list the saved link did not produce, and the next tick fetches the new one without waiting out the import window. The walk's guard makes the same check before every server it writes, the return to the preferred server included, and so does the look before a restore: a link saved while the walk runs ends it where it is, with no return and no message, and the new link gets the next tick. The restore's own writes — the stage and the drop of tunnel membership — carry the same guard under the config lock, so a Web UI or `/xray` selection that commits after that look, while a write waits for the lock or between the two, ends the walk there too: clients the stage handed back to Xray leave it again if they had left it before (`unstageRestore`), and the next tick probes the server that runs now. The tick's own restore carries a guard for the server its probe tested (`probedServer`) for the same reason. A walk that is still fetching or probing abandons if `xray.active_server` changes to a record it did not just write (Web UI / `/xray` Select). The record carries a write counter (`seq`) that every record moves on, so re-selecting the server already running counts too; `GenerateAndRecordWalkedServer` returns the counter it wrote inside the locked transaction, and the walk compares against that — reading it back after the lock was released would adopt a selection committed in between as the walk's own. Before each write that check runs as the guard of `GenerateAndRecordWalkedServer`, under the config lock the write takes, so a selection committed just before it is refused rather than overwritten; the return to the preferred server after an all-dead walk carries the same guard. The import walk still runs when SOCKS is down. If the fallback cannot be applied while the move is still staged, the watch keeps probing and refreshing the subscription, and a healthy probe rolls the stage back. Watch `Generate` writes config.json from a tunnel-resolved IPv4 (`ServerForDial`, which fills an empty TLS server name with the hostname; a REALITY entry without one keeps none and is refused, as the Web UI refuses it) and records the subscription hostname in `xray.active_server` so the Web UI Active badge still matches `servers.json`. Web UI and `/xray` keep the hostname in vnext. A failed restore-Apply writes that snapshot back; it does not move unrelated Xray clients. Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored). A failed import keeps `servers.json`. A refresh that finds no live server backs the next one off to 10, 20, then 30 minutes after the walk ends; a failed download retries every 5 minutes (and resets that backoff), and a restore resets the interval.
+Every 30s the bot probes `https://www.gstatic.com/generate_204` through Xray SOCKS (`127.0.0.1:<socks_port>`, default 12346). Success is HTTP 204. After 3 minutes of consecutive failures it moves those clients onto the first Tunnel Director exit (same filter as PathManager: not `main`, has clients, platform lists it connected with an iface; no Telegram probe). `tunnel.sh` emits MARK rules for `xray.failover.clients` first so overlapping earlier rules (including `main`) do not send those snapshot addresses to WAN; other tunnels keep JSON order. It then refreshes the saved subscription (SSRF WAN first, then `DialPath` through the tunnel the clients are on — `xray.failover.tunnel` while it is still an exit, otherwise the first exit (`vpnconfig.FailoverTDExit`); server hostnames resolve over the same path that fetched the body, IPv4 only and bound to the tick's context (`subscription.LookupIPv4`, the default for `/import` and the Web UI import too): an AF_UNSPEC lookup of every host in a subscription waits out the unanswered AAAA half, five seconds each, and a stop could not end it. A body that arrives over the WAN resolves each host with the WAN resolver and, for a host it does not answer, with the tunnel's, which asks 8.8.8.8 over the interface; one none of whose hosts answered on either falls through to the tunnel's own download; the watch walk dials those IPv4 addresses, every address of a server in turn before the next server (`perAddress`: a ban takes an address, not the name), while Web UI and `/xray` keep the hostname in vnext so CDN/DDNS still resolves), and tries the server the user chose first — the entry with its name, address and port, or else the first entry with its name, since a subscription that rotates endpoints gives a name a new address every day — then the rest in list order. The walk's own records keep that choice in `xray.preferred_server` for as long as `active_server` names another server (`vpnconfig.RecordWalkedServer`, from the first record that leaves its name until one comes back to it), so a walk cut short by a bot restart or a stop starts the next wave from the user's server and returns to it; a selection in the Web UI, `/xray` or either wizard ends it. A live SOCKS probe restores the addresses that were taken off Xray, including those that already sat on the fallback tunnel — while failed over the health probe runs every tick, so a recovered outbound or a Web UI Select does not wait on the subscription host. Restore stages onto Xray only snapshot addresses still on the fallback tunnel, and neither stages nor drops tunnel membership until `tproxy_apply` has written `/tmp/xray_tproxy/ready` (SOCKS 204 does not prove LAN TPROXY; a soft-fail apply must not strip TUN_DIR). Waiting before the stage is the point: the PREROUTING jumps are installed even when the platform's own rules are not, and Xray wins over TUN_DIR, so a client put back into `xray.clients` too early is intercepted by a TPROXY that cannot carry it while the tunnel it is still on carries nothing. While the marker is missing the clients stay on the fallback tunnel and only the apply is retried, on the import cadence. The marker is checked once more after the apply that drops the tunnel membership, because that apply is the one that has to keep TPROXY up: if it soft-fails, the restored addresses go back onto the tunnel as a committed failover (`vpnconfig.ApplyFailoverSnapshot`, only what the restore moved) and the restore is retried once the marker returns, rather than leaving them with neither the proxy nor the tunnel and nothing to try again. The marker also needs the platform's own rules: on Keenetic a missing mangle INPUT accept drops proxied HTTPS in `_NDM_HTTP_INPUT_TLS_`, which the SOCKS probe cannot see. A full `vpn-director.sh stop` (bot `/stop`, `POST /api/stop`) writes `/tmp/vpn-director/stopped` before it tears anything down; a full `apply`, `restart` and `update` remove it — the firewall hooks and the daily update included, `apply --dry-run` not, and neither does an apply or restart of one component (`restart xray` after a Web UI server switch turns Xray back on, not the rest). The watch does nothing while that file exists. Its own applies and Xray restarts pass `--unless-stopped`, which the script checks right after taking the lock, so a stop that finishes mid-tick, or takes the lock ahead of a queued watch apply, stays in force; the watch also re-checks the marker after every wait, a restart the script skipped included, and ends the tick without writes or messages. The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between. While a tick waits it looks for the marker every second (`stopPoll`) and cancels what it is waiting on, so a stop does not sit out a download, the resolution behind it or a probe. One download and the resolution of every host in it share a 3-minute deadline (`FetchTimeout`), and a fetch whose context ended part-way through the resolution returns the context's error rather than the servers resolved so far: a list cut short is never published. The wait for the config lock is covered the same way for every config write of the watch: the guard the walk hands `Generate`, and `Watch.update` for the stage, commit, restore, retarget and publication writes, check the marker first, under that lock, so a stop that finishes while a write is queued refuses it instead of leaving it for the next manual apply. A deleted or wizard-moved address is not put back. If TPROXY cannot be installed, apply retries on the import cadence, not every 30s. It publishes `servers.json` and `xray.servers` in one config-lock update (`vpnconfig.PublishServers`), as the Web UI and `/import` do, so two importers cannot leave one file from each. `import_server_list.sh` publishes under the same lock: it resolves the list into a temp file, writes nothing until it holds the lock, then replaces `servers.json`, `xray.servers` and the saved link together. An https link is saved, and a file or a plain-http link — neither of which the watch or the Web UI fetches — clears the saved one, so the next refresh does not bring the old link's list back. An import that cannot get the lock within 30 seconds, or whose list has no usable server, leaves the previous one whole. That update also checks, under the same lock, that `xray.subscription_url` is still the link this wave downloaded: a subscription saved while the download was in flight abandons the wave instead of publishing a list the saved link did not produce, and the next tick fetches the new one without waiting out the import window. The walk's guard makes the same check before every server it writes, the return to the preferred server included, and so does the look before a restore: a link saved while the walk runs ends it where it is, with no return and no message, and the new link gets the next tick. The restore's own writes — the stage and the drop of tunnel membership — carry the same guard under the config lock, so a Web UI or `/xray` selection that commits after that look, while a write waits for the lock or between the two, ends the walk there too: clients the stage handed back to Xray leave it again if they had left it before (`unstageRestore`), and the next tick probes the server that runs now. The tick's own restore carries a guard for the server its probe tested (`probedServer`) for the same reason. A walk that is still fetching or probing abandons if `xray.active_server` changes to a record it did not just write (Web UI / `/xray` Select). The record carries a write counter (`seq`) that every record moves on, so re-selecting the server already running counts too; `GenerateAndRecordWalkedServer` returns the counter it wrote inside the locked transaction, and the walk compares against that — reading it back after the lock was released would adopt a selection committed in between as the walk's own. Before each write that check runs as the guard of `GenerateAndRecordWalkedServer`, under the config lock the write takes, so a selection committed just before it is refused rather than overwritten; the return to the preferred server after an all-dead walk carries the same guard. The import walk still runs when SOCKS is down. If the fallback cannot be applied while the move is still staged, the watch keeps probing and refreshing the subscription, and a healthy probe rolls the stage back. Watch `Generate` writes config.json from a tunnel-resolved IPv4 (`ServerForDial`: the IPv4 goes into the stored outbound's own address slot, `vpnconfig.OutboundTarget`, and the hostname into an empty TLS server name or, for a stream without security, an empty ws/httpupgrade/xhttp Host; a REALITY entry without a server name keeps none and is refused, as the Web UI refuses it) and records the subscription hostname in `xray.active_server` so the Web UI Active badge still matches `servers.json`. Web UI and `/xray` keep the hostname in vnext. A failed restore-Apply writes that snapshot back; it does not move unrelated Xray clients. Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored). A failed import keeps `servers.json`. A refresh that finds no live server backs the next one off to 10, 20, then 30 minutes after the walk ends; a failed download retries every 5 minutes (and resets that backoff), and a restore resets the interval.
 
 A failover moves only the Xray clients Tunnel Director can carry (`vpnconfig.TDCarries`: an IPv4 address or CIDR that iptables and ipset read as written — no leading zeros — inside RFC1918, the test `is_ipv4_net` and `is_lan_ip` make in the shell). The others stay on Xray, where a dead outbound takes them nowhere rather than out through the WAN; with none it can carry, the death is announced as one with no fallback. Xray clients added, re-added or resumed while a failover lasts join it the way the snapshot did (`ExtendXrayFailover`: onto the tunnel, then off Xray once TUN_DIR has them). The record carries `committed`, set by `CommitXrayFailover` and kept by the restore stage — a record from before the field counts as committed while its snapshot is off Xray. A restore says "back on Xray" only for a failover whose clients had left Xray, and one that does not hold goes back to the state it started from: committed, or a stage that never was. A restore stage whose apply loses the TPROXY marker takes the clients off Xray again, and the retry of a failed last restore apply checks the marker as the first try does. While Xray stays down, a committed failover asks the platform about its tunnel once a minute (`FallbackCheck`) and looks for `failover_ready`. A tunnel the platform still lists but Tunnel Director no longer sends the clients into — the last apply withheld the marker, and an apply of the watch's own did not bring it back — counts as gone, unless the failover has nobody left to carry (`vpnconfig.FailoverCarries`: every snapshot address paused or off the tunnel). Gone for a minute (`FallbackDownAfter`) — an empty tunnel list or a failed lookup is no answer — the clients move to the next exit, or with none left back to Xray, announced as a death with no fallback. An unready fallback is retried every 5 minutes and replaced by the next exit this round has not tried; after a round in which none became ready the next one waits 10, 20, then 30 minutes. What one episode learned about its fallbacks ends with it. The three minutes before a death count from the first miss of a working outbound: a pick without a fallback and a `/stop` start them again, and a failover keeps them counting. The Web UI and the bot's `/clients` take an address they add or delete out of the failover record, and the wizard keeps in it only what it puts on Xray, so an assignment a user makes during a failover is not undone by the restore.
 
--- a/.claude/rules/webui.md
+++ b/.claude/rules/webui.md
@@ -47,8 +47,8 @@
 | GET | `/api/ip` | External IP |
 | GET | `/api/version` | Build version and commit |
 | GET | `/api/platform` | `vpn-director.sh platform`: firmware, password file, LAN/WAN interfaces, tunnels; 503 when the script cannot answer |
-| GET | `/api/servers` | Xray server list plus `active`, the recorded server, and `subscription_saved` |
-| POST | `/api/servers/active`, `/api/servers/import` | Select the active server (`index` plus the `name`, `address` and `port` the page showed there; 409 "server list changed" when the list has another server at that index); import a subscription (`url` empty reuses the saved URL; a body over 1 MiB is refused) |
+| GET | `/api/servers` | Xray server list — name, address, port, IPs and the protocol label (`vless·reality`, `ss`, `hysteria2`), no credentials — plus `active`, the recorded server, and `subscription_saved` |
+| POST | `/api/servers/active`, `/api/servers/import` | Select the active server (`index` plus the `name`, `address` and `port` the page showed there; 409 "server list changed" when the list has another server at that index); import a subscription (`url` empty reuses the saved URL; a body over 1 MiB is refused; the answer carries `count`, `total`, `skipped` by reason, `dns_errors` and the `summary` the page shows) |
 | GET/POST/DELETE | `/api/clients` | LAN clients; a POST route must be xray, a tunnel already in the config, or a tunnel `/api/platform` lists; 503 when the platform cannot answer for a route outside the config |
 | POST | `/api/clients/pause`, `/api/clients/resume` | Pause and resume a client |
 | GET/POST | `/api/excludes/sets` | Country exclusion sets |
--- a/.claude/rules/testing.md
+++ b/.claude/rules/testing.md
@@ -48,6 +48,7 @@
 │   ├── pgrep                # Mock process lookup
 │   ├── insmod               # Mock insmod (Keenetic modules)
 │   ├── cru                  # Mock cru (Merlin cron)
+│   ├── xray                 # Mock "xray run -test" (XRAY_MOCK_EXIT, XRAY_MOCK_OUTPUT, XRAY_MOCK_LOG)
 │   └── keenetic/
 │       └── curl             # RCI fixtures, per-test PATH
 ├── unit/                    # Unit tests for lib/ modules
@@ -59,7 +60,9 @@
 │   ├── platform_keenetic.bats
 │   ├── hooks.bats           # NDM hooks
 │   ├── install.bats
-│   └── configure.bats
+│   ├── configure.bats
+│   ├── subscription.bats    # lib/subscription.sh on the shared cases in testdata/subscription
+│   └── xrayconf.bats        # lib/xrayconf.sh
 ├── integration/             # Integration tests
 │   ├── vpn_director.bats    # Tests for vpn-director.sh CLI
 │   └── ipset_sources.bats   # Tests for IPSet download sources
@@ -170,3 +173,15 @@
 `load_common` now saves the trap before sourcing and restores it afterwards,
 and `teardown()` calls `_cleanup_tmp` in its place. If you add a helper that
 sources a module some other way, keep the trap.
+
+## Shared subscription cases
+
+`testdata/subscription/` at the repository root holds `<case>.in` (a body as served) and
+`<case>.want.json` (the error, or `total`, the servers and the skips by name and reason).
+`router/test/unit/subscription.bats` runs every case through `lib/subscription.sh`, and
+`server/internal/subscription/fixtures_test.go` through the Go decoder: a new case is two files,
+and both decoders must pass it. The cases are synthetic — documentation addresses
+(192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), `example.com` hosts, made-up keys in
+the formats Xray checks — because the repository is public and a provider's real hosts would be a
+ready blocklist. Entware's jq has no regex builtins; `subscription.bats` fails when
+`lib/subscription.sh` uses one.
--- a/README.md
+++ b/README.md
@@ -6,7 +6,7 @@
 
 ## Features
 
-- **Xray TPROXY**: Transparent proxy for selected LAN clients via VLESS
+- **Xray TPROXY**: Transparent proxy for selected LAN clients via VLESS, VMess, Trojan, Shadowsocks or Hysteria2
 - **Tunnel Director**: Route traffic through OpenVPN/WireGuard by destination
 - **Country-based routing**: Route traffic directly or through VPN based on destination geography
 - **Web UI**: HTTPS web interface for managing VPN Director from the browser
@@ -26,7 +26,7 @@
 
 After installation:
 
-1. Import VLESS servers (optional):
+1. Import the servers of your subscription (optional):
    ```bash
    /opt/vpn-director/import_server_list.sh
    ```
@@ -199,7 +199,7 @@
 | `/status` | VPN Director status |
 | `/xray` | Switch Xray server |
 | `/servers` | Server list |
-| `/import <url>` | Import VLESS subscription (auto-syncs xray.servers) |
+| `/import <url>` | Import a subscription (auto-syncs xray.servers) |
 | `/exclude` | Manage excluded IPs/CIDRs |
 | `/clients` | Manage VPN clients |
 | `/configure` | Configuration wizard |
@@ -222,7 +222,9 @@
 
 ### Xray TPROXY
 
-Traffic from specified LAN clients is transparently redirected through Xray using TPROXY. The proxy uses VLESS protocol over TLS to connect to your VPN server.
+Traffic from specified LAN clients is transparently redirected through Xray using TPROXY. Xray reaches the server you select with the protocol your subscription gives it.
+
+Subscriptions it reads: share links (`vless://`, `vmess://`, `trojan://`, `ss://`, `hysteria2://` / `hy2://`), base64-encoded or plain, and Xray JSON - the array of Xray configs panels such as Remnawave and Marzban give Xray clients. An entry Xray cannot run - TUIC, SSR, a balancer, a chained config - is skipped, and the import says why.
 
 ### Tunnel Director
 
--- a/README.ru.md
+++ b/README.ru.md
@@ -6,7 +6,7 @@
 
 ## Возможности
 
-- **Xray TPROXY**: Прозрачный прокси для выбранных LAN-клиентов через VLESS
+- **Xray TPROXY**: Прозрачный прокси для выбранных LAN-клиентов через VLESS, VMess, Trojan, Shadowsocks или Hysteria2
 - **Tunnel Director**: Маршрутизация трафика через OpenVPN/WireGuard по назначению
 - **Маршрутизация по странам**: Направление трафика напрямую или через VPN в зависимости от географии назначения
 - **Веб-интерфейс**: HTTPS веб-интерфейс для управления VPN Director из браузера
@@ -26,7 +26,7 @@
 
 После установки:
 
-1. Импортируйте VLESS-серверы (опционально):
+1. Импортируйте серверы вашей подписки (опционально):
    ```bash
    /opt/vpn-director/import_server_list.sh
    ```
@@ -199,7 +199,7 @@
 | `/status` | Статус VPN Director |
 | `/xray` | Переключение сервера Xray |
 | `/servers` | Список серверов |
-| `/import <url>` | Импорт VLESS-подписки (авто-синхронизация xray.servers) |
+| `/import <url>` | Импорт подписки (авто-синхронизация xray.servers) |
 | `/exclude` | Управление исключёнными IP/CIDR |
 | `/clients` | Управление VPN-клиентами |
 | `/configure` | Мастер настройки |
@@ -222,7 +222,9 @@
 
 ### Xray TPROXY
 
-Трафик от указанных LAN-клиентов прозрачно перенаправляется через Xray с помощью TPROXY. Прокси использует протокол VLESS поверх TLS для подключения к вашему VPN-серверу.
+Трафик от указанных LAN-клиентов прозрачно перенаправляется через Xray с помощью TPROXY. До выбранного сервера Xray добирается по тому протоколу, который указан в подписке.
+
+Какие подписки понимает импорт: share-ссылки (`vless://`, `vmess://`, `trojan://`, `ss://`, `hysteria2://` / `hy2://`) в base64 или обычным текстом, а также Xray JSON — массив конфигов Xray, который панели вроде Remnawave и Marzban отдают Xray-клиентам. Запись, которую Xray запустить не может (TUIC, SSR, балансировщик, цепочка), пропускается, и импорт сообщает причину.
 
 ### Tunnel Director
 
PATCH_EOF
````

- [ ] **Step 2: Check that no stale name is left**

Run: `grep -rn 'internal/vless\|vless\.LookupIPv4\|No VLESS servers\|Import VLESS' CLAUDE.md .claude/rules README.md README.ru.md install.sh router/opt server/internal web/src`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md .claude/rules/xray-tproxy.md .claude/rules/telegram-bot.md .claude/rules/webui.md .claude/rules/testing.md README.md README.ru.md
git commit -m "docs: subscription formats, stored outbounds and the Xray config test"
```

---

### Task 13: Final verification

Spec 13. Everything CI runs, then an optional check against a real Xray, then the device check.

- [ ] **Step 1: The whole Go suite, as CI runs it**

Run: `cd server && go vet ./... && go test ./... -count=1`
Expected: every package `ok`.

- [ ] **Step 2: The whole bats suite, as CI runs it**

Run: `bats router/test/*.bats router/test/unit router/test/integration`
Expected: no `not ok` line (578+ tests at the time of writing).

- [ ] **Step 3: The Web UI build**

Run: `cd web && npm ci && npm run build`
Expected: `✓ built`.

- [ ] **Step 4 (optional, needs network): every case's servers load in Xray 26.2.6**

The routers run Entware's xray-core 26.2.6; every server a shared case yields must load in it. This downloads the release into a temporary directory:

```bash
tmp=$(mktemp -d)
curl -sSL -o "$tmp/xray.zip" https://github.com/XTLS/Xray-core/releases/download/v26.2.6/Xray-linux-64.zip
python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extract('xray', sys.argv[2])" "$tmp/xray.zip" "$tmp"
chmod +x "$tmp/xray"
for w in testdata/subscription/*.want.json; do
    jq -c '.servers[]? | {name, outbound}' "$w" | while IFS= read -r s; do
        jq --argjson s "$s" '.outbounds = [$s.outbound + {tag: "proxy-out"}]' \
            router/opt/etc/xray/config.json.template > "$tmp/config.json.test"
        "$tmp/xray" run -test -format json -c "$tmp/config.json.test" >/dev/null 2>&1 ||
            echo "REJECTED ${w##*/}: $(jq -r .name <<< "$s")"
    done
done
rm -rf "$tmp"
```
Expected: no `REJECTED` line.

- [ ] **Step 5: Device check — only with the owner's permission**

Ask the owner before touching the router. On the author's router:
1. Install the build (the owner's usual update path), then import both real subscriptions — once with `/opt/vpn-director/import_server_list.sh` over SSH, once from the Web UI. The Web UI shows `Imported 32 of 40 servers: 7 composite, 1 DNS error` for the Xray JSON one (counts may move with the provider's list).
2. Select a VLESS, a Hysteria2 and a Shadowsocks server in turn; from a LAN client in `xray.clients`, check traffic through each (for example `curl -4 https://ifconfig.me` shows the server's exit).
3. Break the running server (for example select one whose port is closed) and watch the bot's subscription watch fail over and walk to a working server of any protocol.

- [ ] **Step 6: Before opening the pull request**

The repository keeps design and plan documents out of pull requests (the owner's instructions): when the branch is ready, `git rm -r docs/superpowers/` and commit that, so the documents stay in the branch history only.
