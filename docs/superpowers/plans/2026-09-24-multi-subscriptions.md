# Multiple Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep up to ten subscriptions on the router, each in its own file; let the user run any server of any of them; and have the subscription watch look for a live server across all of them when the running one dies.

**Architecture:** A subscription is `<data_dir>/subscriptions/<id>.json`: its link, its status and its servers. `vpnconfig` owns the file format and five locked operations (add, refresh, record an error, rename, delete) written against a `ConfigUpdate` and a `SubscriptionFiles`, so the daemons, the watch and the tests share one implementation. `service` wraps them for the Web UI and the bot together with the one download path they share, and `lib/substore.sh` is their twin for the shell. The watch downloads every subscription at once and walks the servers in the hybrid order — the chosen server and two more of its subscription, then one server of each subscription in turn — skipping any server whose outbound is identical to one already tried.

**Tech Stack:** Go 1.25 (standard library only), Vue 3 + TypeScript (Vite, vue-tsc), bash + jq 1.8 without oniguruma + BusyBox, bats.

**Spec:** `docs/superpowers/specs/2026-09-24-multi-subscriptions-design.md`. Read it before any task; "spec 5.3" below means its section 5.3.

## Global Constraints

- Go module `github.com/zinin/vpn-director/server`. No new Go or npm dependency.
- A subscription file is `<data_dir>/subscriptions/<id>.json`, mode 0600, in a directory of mode 0700. An id is 8 lowercase hex digits; a file named anything else is not a subscription.
- At most 10 subscriptions (`vpnconfig.MaxSubscriptions`).
- A name is 1 to 32 code points once the spaces (U+0020 only) around it are trimmed, has no control character (C0, DEL, C1), and is unique under ASCII case folding. The default is the link's host name (a file's base name in the shell); a taken default becomes `-2`, `-3`, …, cut so that the suffix fits in 32.
- Times in the files are RFC 3339, UTC, whole seconds: `2026-09-24T18:00:00Z`.
- Every write to `subscriptions/` happens under the config lock `.vpn-director.json.lock`: in Go inside `UpdateVPNConfig` (or the watch's `update`), in the shell with flock on FD 9.
- `OwnFirst = 3`. The walk's rotation starts with the subscription after the chosen server's.
- No subscription link — it carries a token — goes into any message, log line, API answer, fixture, commit message or test output. Show the host at most. Fixtures are synthetic: `example.*` hosts and documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24).
- Shell: a sourced library starts with `#!/usr/bin/env bash`; use `[[ ]]`; Entware's jq has no regex builtins, so `test`, `match`, `capture`, `scan`, `split/2`, `splits`, `sub` and `gsub` never appear in `lib/*.sh`. `lib/substore.sh` stands alone — `configure.sh` does not source `lib/common.sh`, so the library must not call `log`.
- Bot texts keep the language each handler uses today: `/xray` answers in Russian, everything else in English.
- The owner's working rules: run Go and npm build and test commands through the `claude-forge:build-runner` agent; run bats and shellcheck directly; the full bats suite takes minutes — run it in the background and read the counts from its log. Never write to the local memory directory. Commit each task on `feature/multi-subscriptions`; do not push without asking.

## Review Focus

1. **One server name in two subscriptions.** Both providers can call a server `Germany-1`. Selecting it in the Web UI, `/xray` or the wizard must run the one in the chosen subscription; the Active badge and the `✓` mark only that one; the walk's fallback by name never crosses subscriptions. Tests: Task 1 (`RecordWalkedServer`), Task 5 (select), Task 8 (fingerprint), Task 9 (wizard pick), Task 10 (`chosenIndex`, `walkOrder`).
2. **A subscription file edited or broken by hand.** Invalid JSON, an `id` that is not the file's name, a stray temp or backup file: every reader skips it (the daemons with a WARN), and nobody fails because of it. Tests: Task 1, Task 13.
3. **State left by the previous release.** `xray.subscription_url` in the config, `servers.json` on disk, `active_server` without `subscription`: nothing crashes, no server shows as active, the watch stays unarmed until a subscription exists, the first subscription write removes `servers.json` (Go and shell), and the next daemon write of the config drops the old key. Tests: Task 3, Task 11, Task 12, Task 14.
4. **A subscription deleted while its refresh is downloading.** From the Web UI, the bot, the watch's wave or the shell, the publication is refused and the file is not recreated. Tests: Task 2, Task 11, Task 14.
5. **Names that differ only in case, and names outside ASCII.** `Alpha` and `alpha` collide in Go and in the shell; `Бета` and `бета` do not (ASCII folding only); a default host longer than 32 characters is cut with its suffix inside the limit — the same answers on both sides. Tests: Task 1 and Task 13, on the same inputs.

## File Structure

| Path | Responsibility |
|---|---|
| `server/internal/vpnconfig/substore.go` (new) | `Subscription`, the file store (load, save, delete), ids, names, flattening, the legacy `servers.json` |
| `server/internal/vpnconfig/subops.go` (new) | The locked operations: add, refresh, record an error, rename, delete |
| `server/internal/vpnconfig/vpnconfig.go` | `Server.Subscription`, `ActiveServer.Subscription`, `RecordWalkedServer`; later loses `SubscriptionURL` and the `servers.json` helpers |
| `server/internal/vpnconfig/failover.go` | `Armed(cfg, subscriptions)` |
| `server/internal/vpnconfig/publish.go` | Keeps only `ServersSaved`; the single-list publication goes |
| `server/internal/service/config.go`, `interfaces.go` | `ConfigStore` gains the subscription files; `LoadServers` flattens them |
| `server/internal/service/subscriptions.go` (new) | The daemons' download path and the operations they call |
| `server/internal/service/publish.go` | Deleted at the end |
| `server/internal/webapi/handler_subscriptions.go` (new) | `/api/subscriptions` routes |
| `server/internal/webapi/handler_servers.go` | Grouped list, selection by subscription; the import route goes |
| `server/internal/handler/subs.go` (new) | `/subs` with refresh, rename and delete |
| `server/internal/handler/import.go`, `xray.go`, `servers.go` | `/import`, the two-step `/xray`, `/servers` by subscription |
| `server/internal/wizard/server.go`, `state.go` | The two-step server choice of `/configure` |
| `server/internal/bot/router.go`, `bot.go` | `/subs`, `/cancel`, the text dispatch, the watch wiring |
| `server/internal/subwatch/order.go` (new) | The hybrid walk order and the dedupe key |
| `server/internal/subwatch/watch.go`, `return.go`, `reach.go` | The wave over all subscriptions, the guards, the messages |
| `web/src/types.ts`, `api.ts`, `components/ServersTab.vue`, `components/StatusTab.vue` | The Subscriptions card, servers grouped by subscription, the running server's label |
| `router/opt/vpn-director/lib/substore.sh` (new) | The shell twin of the store |
| `router/opt/vpn-director/import_server_list.sh` | The subscription menu |
| `router/opt/vpn-director/configure.sh` | The two-step server choice |
| `router/files.manifest` | Ships `lib/substore.sh` |
| `testdata/substore/*.json` (new) | Synthetic subscription files both test suites read |

Task order keeps every commit building and its tests green. Between Task 3 and Task 12 the Go readers already read the subscription files while some writers still write `servers.json`; that intermediate state never ships.

---

### Task 1: The subscription store in `vpnconfig`

**Files:**
- Create: `testdata/substore/0a1b2c3d.json`, `testdata/substore/1b2c3d4e.json`, `testdata/substore/2c3d4e5f.json`
- Create: `server/internal/vpnconfig/substore.go`
- Create: `server/internal/vpnconfig/substore_test.go`
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`Server`, `ActiveServer`, `NewActiveServer`, `RecordWalkedServer`)
- Modify: `server/internal/vpnconfig/vpnconfig_test.go`

**Interfaces:**
- Consumes: `vpnconfig.Server`, `ServerIPs`, `writeFileAtomic` (all existing).
- Produces:
  - `const MaxSubscriptions = 10`, `const MaxSubscriptionName = 32`
  - `type Subscription struct { ID string; Name string; URL string; Added time.Time; Refreshed time.Time; Error string; Servers []Server }` (JSON `id`, `name`, `url,omitempty`, `added`, `refreshed`, `error,omitempty`, `servers`)
  - `func (s Subscription) Static() bool`, `func (s Subscription) Host() string`
  - `func SubscriptionsDir(dataDir string) string`
  - `func ValidSubscriptionID(id string) bool`
  - `func LoadSubscriptions(dir string) ([]Subscription, error)`
  - `func SaveSubscription(dir string, sub Subscription) error`
  - `func DeleteSubscriptionFile(dir, id string) error`
  - `func RemoveLegacyServers(dataDir string) error`
  - `func NewSubscriptionID(subs []Subscription) (string, error)`
  - `var ErrSubscriptionName`; `func CleanSubscriptionName(name string) (string, error)`
  - `func SubscriptionNameTaken(subs []Subscription, name, exceptID string) bool`
  - `func DefaultSubscriptionName(subs []Subscription, base string) string`
  - `func FindSubscription(subs []Subscription, id string) int`
  - `func AllServers(subs []Subscription) []Server`, `func SubscriptionIPs(subs []Subscription) []string`
  - `Server.Subscription string` (`json:"-"`), `ActiveServer.Subscription string` (`json:"subscription,omitempty"`)

- [ ] **Step 1: Write the shared fixtures**

These three files are read by Go here and by bats in Task 13. They are synthetic.

`testdata/substore/1b2c3d4e.json` — a static list added first:

```json
{
  "id": "1b2c3d4e",
  "name": "Beta",
  "added": "2026-09-24T17:00:00Z",
  "refreshed": "2026-09-24T17:00:00Z",
  "servers": [
    {
      "address": "198.51.100.20",
      "port": 8443,
      "name": "Germany-1",
      "ips": ["198.51.100.20"],
      "outbound": {
        "protocol": "trojan",
        "settings": {"servers": [{"address": "198.51.100.20", "port": 8443, "password": "fixture-password"}]},
        "streamSettings": {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "beta.example.net"}}
      }
    }
  ]
}
```

`testdata/substore/0a1b2c3d.json` — a subscription with a link, whose last refresh failed:

```json
{
  "id": "0a1b2c3d",
  "name": "Alpha",
  "url": "https://sub.example.com/s/fixture-token",
  "added": "2026-09-24T18:00:00Z",
  "refreshed": "2026-09-24T18:05:00Z",
  "error": "download failed: HTTP 403",
  "servers": [
    {
      "address": "a.example.com",
      "port": 443,
      "name": "Oslo",
      "ips": ["192.0.2.10"],
      "outbound": {
        "protocol": "vless",
        "settings": {"vnext": [{"address": "a.example.com", "port": 443, "users": [{"id": "00000000-0000-4000-8000-000000000001", "encryption": "none"}]}]},
        "streamSettings": {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "a.example.com"}}
      }
    },
    {
      "address": "b.example.com",
      "port": 443,
      "name": "Germany-1",
      "ips": ["192.0.2.11"],
      "outbound": {
        "protocol": "vless",
        "settings": {"vnext": [{"address": "b.example.com", "port": 443, "users": [{"id": "00000000-0000-4000-8000-000000000002", "encryption": "none"}]}]},
        "streamSettings": {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "b.example.com"}}
      }
    }
  ]
}
```

`testdata/substore/2c3d4e5f.json` — added at the same moment as Alpha, so the id decides:

```json
{
  "id": "2c3d4e5f",
  "name": "Gamma",
  "url": "https://panel.example.org/sub/fixture-token",
  "added": "2026-09-24T18:00:00Z",
  "refreshed": "2026-09-24T18:00:00Z",
  "servers": [
    {
      "address": "203.0.113.30",
      "port": 443,
      "name": "Riga",
      "ips": ["203.0.113.30"],
      "outbound": {
        "protocol": "shadowsocks",
        "settings": {"servers": [{"address": "203.0.113.30", "port": 443, "method": "chacha20-ietf-poly1305", "password": "fixture-password"}]}
      }
    }
  ]
}
```

- [ ] **Step 2: Write the failing store tests**

Create `server/internal/vpnconfig/substore_test.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeSubFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// testdata/substore is read by the bats suite too (router/test/unit/substore.bats):
// both sides list the same subscriptions in the same order.
func TestLoadSubscriptions_ReadsTheSharedFixtures(t *testing.T) {
	subs, err := LoadSubscriptions(filepath.Join("..", "..", "..", "testdata", "substore"))
	if err != nil {
		t.Fatal(err)
	}
	var ids, names []string
	for _, s := range subs {
		ids = append(ids, s.ID)
		names = append(names, s.Name)
	}
	// Ordered by added, then by id: Beta came first, Alpha and Gamma at one
	// moment, and 0a1b2c3d sorts before 2c3d4e5f.
	if want := []string{"1b2c3d4e", "0a1b2c3d", "2c3d4e5f"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	if want := []string{"Beta", "Alpha", "Gamma"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if !subs[0].Static() || subs[1].Static() {
		t.Fatalf("static: Beta %v, Alpha %v", subs[0].Static(), subs[1].Static())
	}
	if subs[1].Host() != "sub.example.com" || subs[1].Error != "download failed: HTTP 403" {
		t.Fatalf("Alpha: host %q, error %q", subs[1].Host(), subs[1].Error)
	}
	for _, sub := range subs {
		for _, s := range sub.Servers {
			if s.Subscription != sub.ID {
				t.Fatalf("server %q of %s carries subscription %q", s.Name, sub.ID, s.Subscription)
			}
		}
	}
	want := []string{"192.0.2.10", "192.0.2.11", "198.51.100.20", "203.0.113.30"}
	if got := SubscriptionIPs(subs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ips %v, want %v", got, want)
	}
}

func TestLoadSubscriptions_AMissingDirectoryHoldsNone(t *testing.T) {
	subs, err := LoadSubscriptions(filepath.Join(t.TempDir(), "subscriptions"))
	if err != nil || subs != nil {
		t.Fatalf("got %v, %v; want none and no error", subs, err)
	}
}

// Only <8 hex digits>.json is a subscription. The temp file of an atomic
// write, a hand-made backup, a directory and a file whose id is not its name
// are not, and none of them may fail the readers of the good one.
func TestLoadSubscriptions_SkipsWhatIsNoSubscription(t *testing.T) {
	dir := t.TempDir()
	good := `{"id":"0a1b2c3d","name":"Alpha","url":"https://sub.example.com/s/t","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}`
	writeSubFile(t, dir, "0a1b2c3d.json", good)
	writeSubFile(t, dir, ".0a1b2c3d.json.tmp-123", good)
	writeSubFile(t, dir, "0a1b2c3d.json.Ab12Cd", good)
	writeSubFile(t, dir, "0A1B2C3D.json", strings.Replace(good, "0a1b2c3d", "0A1B2C3D", 1))
	writeSubFile(t, dir, "backup.json", good)
	writeSubFile(t, dir, "1b2c3d4e.json", `{"id":"1b2c3d4e",`)
	writeSubFile(t, dir, "2c3d4e5f.json", strings.Replace(good, "0a1b2c3d", "ffffffff", 1))
	if err := os.Mkdir(filepath.Join(dir, "3d4e5f6a.json"), 0700); err != nil {
		t.Fatal(err)
	}

	subs, err := LoadSubscriptions(dir)

	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].ID != "0a1b2c3d" {
		t.Fatalf("got %+v, want 0a1b2c3d alone", subs)
	}
}

func TestSaveSubscription_WritesAFileOnlyItsOwnerReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subscriptions")
	at := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	sub := Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: at, Refreshed: at,
		Servers: []Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}, Subscription: "0a1b2c3d"}}}

	if err := SaveSubscription(dir, sub); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "0a1b2c3d.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s: mode %o, want %o", path, got, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "0a1b2c3d.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["added"] != "2026-09-24T18:00:00Z" {
		t.Fatalf("added %v, want RFC 3339 UTC seconds", raw["added"])
	}
	// The file names its subscription once; its servers do not repeat it.
	if strings.Contains(string(data), `"subscription"`) {
		t.Fatalf("a server repeats its subscription: %s", data)
	}
	subs, err := LoadSubscriptions(dir)
	if err != nil || len(subs) != 1 || subs[0].Servers[0].Subscription != "0a1b2c3d" || !subs[0].Added.Equal(at) {
		t.Fatalf("read back %+v, %v", subs, err)
	}
}

func TestSaveSubscription_RefusesAnIDTheStoreWouldNotRead(t *testing.T) {
	for _, id := range []string{"", "0A1B2C3D", "0a1b2c3", "0a1b2c3d0", "../0a1b2c"} {
		if err := SaveSubscription(t.TempDir(), Subscription{ID: id}); err == nil {
			t.Fatalf("id %q was saved", id)
		}
	}
}

func TestDeleteSubscriptionFile_AFileAlreadyGoneIsNoError(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "0a1b2c3d.json", "{}")
	for i := 0; i < 2; i++ {
		if err := DeleteSubscriptionFile(dir, "0a1b2c3d"); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "0a1b2c3d.json")); !os.IsNotExist(err) {
		t.Fatal("the file is still there")
	}
}

func TestRemoveLegacyServers(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "servers.json", "[]")
	for i := 0; i < 2; i++ {
		if err := RemoveLegacyServers(dir); err != nil {
			t.Fatalf("removal %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "servers.json")); !os.IsNotExist(err) {
		t.Fatal("servers.json is still there")
	}
}

func TestCleanSubscriptionName(t *testing.T) {
	long := strings.Repeat("я", MaxSubscriptionName)
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"  Alpha  ", "Alpha", true},
		{"Бета VPN", "Бета VPN", true},
		{long, long, true}, // 32 characters, 64 bytes
		{long + "я", "", false},
		{"   ", "", false},
		{"Al\tpha", "", false},
		{"\tAlpha", "", false}, // only spaces are trimmed; a tab is a control character
		{"Al\u0085pha", "", false},
		{"Al\x7fpha", "", false},
		{"\xffAlpha", "", false},
	} {
		got, err := CleanSubscriptionName(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("CleanSubscriptionName(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrSubscriptionName) {
			t.Errorf("CleanSubscriptionName(%q): %v is not ErrSubscriptionName", tc.in, err)
		}
	}
}

// jq's ascii_downcase is the fold the shell applies, and the daemons have to
// agree with it on which names collide (router/test/unit/substore.bats asks
// the same questions).
func TestSubscriptionNameTaken_FoldsASCIIOnly(t *testing.T) {
	subs := []Subscription{{ID: "0a1b2c3d", Name: "Beta"}, {ID: "1b2c3d4e", Name: "Бета"}}
	for _, tc := range []struct {
		name, except string
		taken        bool
	}{
		{"beta", "", true},
		{"BETA", "", true},
		{"beta", "0a1b2c3d", false}, // its own name
		{"бета", "", false},         // Cyrillic is not folded
		{"Бета", "", true},
		{"Gamma", "", false},
	} {
		if got := SubscriptionNameTaken(subs, tc.name, tc.except); got != tc.taken {
			t.Errorf("SubscriptionNameTaken(%q, except %q) = %v, want %v", tc.name, tc.except, got, tc.taken)
		}
	}
}

func TestDefaultSubscriptionName(t *testing.T) {
	host40 := strings.Repeat("a", 36) + ".com"
	for _, tc := range []struct {
		taken []string
		base  string
		want  string
	}{
		{nil, "sub.example.com", "sub.example.com"},
		{[]string{"Sub.Example.com"}, "sub.example.com", "sub.example.com-2"},
		{[]string{"sub.example.com", "sub.example.com-2"}, "sub.example.com", "sub.example.com-3"},
		{nil, host40, host40[:32]},
		{[]string{host40[:32]}, host40, host40[:30] + "-2"},
		{nil, strings.Repeat("a", 31) + " bc", strings.Repeat("a", 31)},
		{nil, "  list\x07.txt ", "list.txt"},
		{nil, "", "subscription"},
		{nil, " \x01 ", "subscription"},
	} {
		var subs []Subscription
		for i, n := range tc.taken {
			subs = append(subs, Subscription{ID: fmt.Sprintf("%08x", i), Name: n})
		}
		if got := DefaultSubscriptionName(subs, tc.base); got != tc.want {
			t.Errorf("DefaultSubscriptionName(%v, %q) = %q, want %q", tc.taken, tc.base, got, tc.want)
		}
	}
}

func TestNewSubscriptionID_IsValidAndFree(t *testing.T) {
	subs := []Subscription{{ID: "0a1b2c3d"}}
	for i := 0; i < 100; i++ {
		id, err := NewSubscriptionID(subs)
		if err != nil {
			t.Fatal(err)
		}
		if !ValidSubscriptionID(id) || id == "0a1b2c3d" {
			t.Fatalf("id %q", id)
		}
	}
}

func TestAllServers_KeepsSubscriptionOrderAndMarksEachServer(t *testing.T) {
	subs := []Subscription{
		{ID: "0a1b2c3d", Servers: []Server{{Name: "A1", IPs: []string{"192.0.2.2"}}, {Name: "A2", IPs: []string{"192.0.2.1", ""}}}},
		{ID: "1b2c3d4e", Servers: []Server{{Name: "B1", IPs: []string{"192.0.2.1"}}}},
	}

	all := AllServers(subs)

	var got []string
	for _, s := range all {
		got = append(got, s.Subscription+"/"+s.Name)
	}
	if want := []string{"0a1b2c3d/A1", "0a1b2c3d/A2", "1b2c3d4e/B1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ips := SubscriptionIPs(subs); !reflect.DeepEqual(ips, []string{"192.0.2.1", "192.0.2.2"}) {
		t.Fatalf("ips %v", ips)
	}
	if subs[0].Servers[0].Subscription != "" {
		t.Fatal("AllServers changed the subscriptions it read")
	}
	if FindSubscription(subs, "1b2c3d4e") != 1 || FindSubscription(subs, "ffffffff") != -1 {
		t.Fatal("FindSubscription")
	}
}

func TestSubscriptionHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sub.example.com/s/token":      "sub.example.com",
		"https://sub.example.com:8443/s/token": "sub.example.com",
		"":                                     "",
	} {
		if got := (Subscription{URL: raw}).Host(); got != want {
			t.Errorf("Host of %q = %q, want %q", raw, got, want)
		}
	}
}
```

Append to `server/internal/vpnconfig/vpnconfig_test.go` (add `encoding/json` and `strings` to its imports if they are missing):

```go
// Two providers can both name a server Germany-1. A walk that moves from one
// to the other has moved away from the user's choice, and the record keeps
// that choice aside, as it does for a move to another name.
func TestRecordWalkedServer_TheSameNameInAnotherSubscriptionIsAMove(t *testing.T) {
	cfg := &VPNDirectorConfig{}
	cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "Germany-1", Address: "a.example.com", Port: 443, Seq: 4}

	RecordWalkedServer(cfg, Server{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "b.example.net", Port: 443})

	p := cfg.Xray.PreferredServer
	if p == nil || p.Subscription != "0a1b2c3d" || p.Name != "Germany-1" || p.Seq != 0 {
		t.Fatalf("preferred %+v, want the user's Germany-1 of 0a1b2c3d", p)
	}
	if a := cfg.Xray.ActiveServer; a.Subscription != "1b2c3d4e" || a.Seq != 5 {
		t.Fatalf("active %+v", a)
	}

	// Back on the user's server - its subscription and name, at a new address.
	RecordWalkedServer(cfg, Server{Subscription: "0a1b2c3d", Name: "Germany-1", Address: "c.example.com", Port: 443})

	if cfg.Xray.PreferredServer != nil {
		t.Fatalf("preferred %+v, want none", cfg.Xray.PreferredServer)
	}
}

func TestNewActiveServer_RecordsTheSubscription(t *testing.T) {
	a := NewActiveServer(Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "a.example.com", Port: 443, UUID: "secret"})

	if *a != (ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "a.example.com", Port: 443}) {
		t.Fatalf("got %+v", *a)
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"subscription":"0a1b2c3d"`) || strings.Contains(string(data), "secret") {
		t.Fatalf("marshaled %s", data)
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `cd server && go test ./internal/vpnconfig/...`
Expected: FAIL — `undefined: LoadSubscriptions`, `unknown field Subscription in struct literal`.

- [ ] **Step 4: Add the subscription to the server records**

In `server/internal/vpnconfig/vpnconfig.go`, add to `Server` after `ALPN`:

```go
	// Subscription is the id of the subscription file the server came from.
	// LoadSubscriptions fills it in; the file does not repeat it.
	Subscription string `json:"-"`
```

Add to `ActiveServer` between `Port` and `Seq`:

```go
	// Subscription is the id of the server's subscription. Two subscriptions
	// can name a server alike, and a record without it - one written before
	// subscriptions - matches no server.
	Subscription string `json:"subscription,omitempty"`
```

Replace `NewActiveServer` and `RecordWalkedServer`:

```go
// NewActiveServer records the fields of s that identify it to a reader. The
// rest - UUID, REALITY keys - would put credentials in a file the Web UI hands
// out over /api/config.
func NewActiveServer(s Server) *ActiveServer {
	return &ActiveServer{Name: s.Name, Address: s.Address, Port: s.Port, Subscription: s.Subscription}
}
```

```go
// RecordWalkedServer names s as the running server for the subscription walk.
// The walk tries servers nobody chose, and one cut short - the bot restarted, a
// stop - leaves active_server on one of them. So the server the user chose is
// kept in preferred_server from the first record that leaves its choice until
// one comes back to it. A new address under the same name is no move away: a
// subscription that rotates endpoints gives a name one every day. The same
// name in another subscription is another server.
func RecordWalkedServer(cfg *VPNDirectorConfig, s Server) {
	x := &cfg.Xray
	switch {
	case x.PreferredServer != nil && sameChoice(x.PreferredServer, s):
		x.PreferredServer = nil
	case x.PreferredServer == nil && x.ActiveServer != nil && !sameChoice(x.ActiveServer, s):
		chosen := *x.ActiveServer
		chosen.Seq = 0
		x.PreferredServer = &chosen
	}
	x.ActiveServer = RecordActiveServer(x.ActiveServer, s)
}

// sameChoice reports whether a names the choice s is: its subscription and its name.
func sameChoice(a *ActiveServer, s Server) bool {
	return a.Subscription == s.Subscription && a.Name == s.Name
}
```

- [ ] **Step 5: Write the store**

Create `server/internal/vpnconfig/substore.go`:

```go
package vpnconfig

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxSubscriptions is how many subscriptions a router keeps: the watch
// downloads them all at once, and the bot's first keyboard lists them all.
const MaxSubscriptions = 10

// MaxSubscriptionName is the longest name, in characters (code points).
const MaxSubscriptionName = 32

// Subscription is one file under <data_dir>/subscriptions: a link, what came of
// its last refresh, and the servers it serves. A subscription without a link
// is a static list the shell imported from a file or a plain-http link, and
// nothing refreshes it. lib/substore.sh reads and writes the same files.
type Subscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url,omitempty"`
	Added     time.Time `json:"added"`
	Refreshed time.Time `json:"refreshed"`
	Error     string    `json:"error,omitempty"`
	Servers   []Server  `json:"servers"`
}

// Static reports a list without a link.
func (s Subscription) Static() bool { return s.URL == "" }

// Host is the host name of the link. Lists and summaries show it in place of
// the link, whose path carries the subscription token. Empty for a static list.
func (s Subscription) Host() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// SubscriptionsDir is where dataDir keeps the subscription files.
func SubscriptionsDir(dataDir string) string {
	return filepath.Join(dataDir, "subscriptions")
}

// ValidSubscriptionID reports an id of 8 lowercase hex digits, the only names
// the store reads or writes.
func ValidSubscriptionID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LoadSubscriptions reads every <id>.json in dir, ordered by Added, then by id.
// A missing directory holds no subscription. A file named otherwise - the temp
// file of an atomic write among them - is no subscription; a file that does not
// parse, or whose id is not its name, is skipped with a warning rather than
// failing every reader. Each server carries the id of its file.
func LoadSubscriptions(dir string) ([]Subscription, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var subs []Subscription
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidSubscriptionID(id) || e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted since the listing
		}
		if err != nil {
			return nil, err
		}
		var sub Subscription
		if err := json.Unmarshal(data, &sub); err != nil {
			slog.Warn("Skipping a subscription file that does not parse", "file", e.Name(), "error", err)
			continue
		}
		if sub.ID != id {
			slog.Warn("Skipping a subscription file whose id is not its name", "file", e.Name(), "id", sub.ID)
			continue
		}
		for i := range sub.Servers {
			sub.Servers[i].Subscription = id
		}
		subs = append(subs, sub)
	}
	sort.SliceStable(subs, func(i, j int) bool {
		if !subs[i].Added.Equal(subs[j].Added) {
			return subs[i].Added.Before(subs[j].Added)
		}
		return subs[i].ID < subs[j].ID
	})
	return subs, nil
}

// SaveSubscription writes sub to dir/<id>.json, mode 0600 - the file holds the
// link and every server's credentials - through a temp file and a rename, so a
// reader sees the old file or the new one. The caller holds the config lock.
func SaveSubscription(dir string, sub Subscription) error {
	if !ValidSubscriptionID(sub.ID) {
		return fmt.Errorf("invalid subscription id %q", sub.ID)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sub, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, sub.ID+".json"), append(data, '\n'))
}

// DeleteSubscriptionFile removes dir/<id>.json. One that is already gone is no
// error. The caller holds the config lock.
func DeleteSubscriptionFile(dir, id string) error {
	if !ValidSubscriptionID(id) {
		return fmt.Errorf("invalid subscription id %q", id)
	}
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveLegacyServers deletes dataDir/servers.json, the single list of earlier
// releases. Nothing reads it any more, and every write of a subscription takes
// it away.
func RemoveLegacyServers(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, "servers.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// NewSubscriptionID draws a random id that no subscription in subs has.
func NewSubscriptionID(subs []Subscription) (string, error) {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b[:])
		if FindSubscription(subs, id) < 0 {
			return id, nil
		}
	}
}

// ErrSubscriptionName is a name the rules refuse; the error says which rule.
var ErrSubscriptionName = errors.New("invalid subscription name")

// CleanSubscriptionName trims the spaces around name and checks the rest: 1 to
// MaxSubscriptionName characters, none of them a control character (C0, DEL,
// C1). lib/substore.sh applies the same rules.
func CleanSubscriptionName(name string) (string, error) {
	name = strings.Trim(name, " ")
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: not UTF-8", ErrSubscriptionName)
	}
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return "", fmt.Errorf("%w: empty", ErrSubscriptionName)
	case n > MaxSubscriptionName:
		return "", fmt.Errorf("%w: longer than %d characters", ErrSubscriptionName, MaxSubscriptionName)
	}
	for _, r := range name {
		if isControl(r) {
			return "", fmt.Errorf("%w: it has a control character", ErrSubscriptionName)
		}
	}
	return name, nil
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// foldASCII lower-cases A to Z and nothing else: the fold jq's ascii_downcase
// applies, so the shell and the daemons agree on which names collide.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// SubscriptionNameTaken reports whether a subscription other than exceptID
// already has name, under ASCII case folding.
func SubscriptionNameTaken(subs []Subscription, name, exceptID string) bool {
	folded := foldASCII(name)
	for _, s := range subs {
		if s.ID != exceptID && foldASCII(s.Name) == folded {
			return true
		}
	}
	return false
}

// DefaultSubscriptionName is base - a link's host, a file's name - cut to
// MaxSubscriptionName characters, or base-2, base-3 and so on, cut so that the
// suffix fits, whichever no subscription in subs has yet. Control characters
// and the spaces around base go first; an empty base is "subscription".
// lib/substore.sh makes the same choice.
func DefaultSubscriptionName(subs []Subscription, base string) string {
	base = strings.Trim(strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(base, "")), " ")
	if base == "" {
		base = "subscription"
	}
	for n := 1; ; n++ {
		suffix := ""
		if n > 1 {
			suffix = "-" + strconv.Itoa(n)
		}
		name := strings.TrimRight(cutRunes(base, MaxSubscriptionName-utf8.RuneCountInString(suffix)), " ") + suffix
		if !SubscriptionNameTaken(subs, name, "") {
			return name
		}
	}
}

// cutRunes is s cut to its first n characters.
func cutRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// FindSubscription is the index of the subscription with id in subs, or -1.
func FindSubscription(subs []Subscription, id string) int {
	for i, s := range subs {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// AllServers is every server of every subscription, in subscription order,
// each carrying the id of its subscription.
func AllServers(subs []Subscription) []Server {
	var out []Server
	for _, sub := range subs {
		for _, s := range sub.Servers {
			s.Subscription = sub.ID
			out = append(out, s)
		}
	}
	return out
}

// SubscriptionIPs is xray.servers for subs: every address of every server, the
// set TPROXY_BYPASS takes.
func SubscriptionIPs(subs []Subscription) []string {
	return ServerIPs(AllServers(subs))
}
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `cd server && go test ./internal/vpnconfig/... && gofmt -l internal/vpnconfig`
Expected: PASS, and `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add testdata/substore server/internal/vpnconfig/substore.go server/internal/vpnconfig/substore_test.go \
  server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go
git commit -m "feat(vpnconfig): keep each subscription in a file of its own"
```

---

### Task 2: The locked subscription operations

**Files:**
- Create: `server/internal/vpnconfig/subops.go`
- Create: `server/internal/vpnconfig/subops_test.go`

**Interfaces:**
- Consumes (Task 1): `Subscription`, `FindSubscription`, `SubscriptionIPs`, `NewSubscriptionID`, `CleanSubscriptionName`, `SubscriptionNameTaken`, `DefaultSubscriptionName`, `MaxSubscriptions`; existing `ServersSaved`, `ErrServersSaved`.
- Produces:
  - `type ConfigUpdate func(func(*VPNDirectorConfig) error) error`
  - `type SubscriptionFiles struct { Load func() ([]Subscription, error); Save func(Subscription) error; Delete func(id string) error }`
  - `var ErrSubscriptionGone, ErrSubscriptionLimit, ErrSubscriptionNameTaken, ErrSubscriptionStatic, ErrSaveSubscription`
  - `func AddSubscription(update ConfigUpdate, files SubscriptionFiles, rawURL, name string, servers []Server, now time.Time) (sub Subscription, existed bool, err error)`
  - `func RefreshSubscription(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error)`
  - `func RecordSubscriptionError(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, since time.Time, msg string) error`
  - `func RenameSubscription(update ConfigUpdate, files SubscriptionFiles, id, name string) (Subscription, error)`
  - `func DeleteSubscription(update ConfigUpdate, files SubscriptionFiles, id string) (activeWasIn bool, err error)`

- [ ] **Step 1: Write the failing tests**

Create `server/internal/vpnconfig/subops_test.go`:

```go
package vpnconfig

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// memStore is one data directory in memory: its subscription files and its
// config. update is the locked read-modify-write the daemons do - fn on a copy
// of the config, kept only when fn and the config write succeed.
type memStore struct {
	cfg           VPNDirectorConfig
	subs          []Subscription
	saveErr       error // the subscription file write fails
	configErr     error // the config write after fn fails
	inUpdate      bool
	writesOutside int // file writes made outside update: none may happen
}

func (m *memStore) update(fn func(*VPNDirectorConfig) error) error {
	m.inUpdate = true
	defer func() { m.inUpdate = false }()
	cfg := m.cfg
	if err := fn(&cfg); err != nil {
		return err
	}
	if m.configErr != nil {
		return m.configErr
	}
	m.cfg = cfg
	return nil
}

func (m *memStore) files() SubscriptionFiles {
	return SubscriptionFiles{
		Load: func() ([]Subscription, error) {
			out := make([]Subscription, len(m.subs))
			copy(out, m.subs)
			return out, nil
		},
		Save: func(s Subscription) error {
			if !m.inUpdate {
				m.writesOutside++
			}
			if m.saveErr != nil {
				return m.saveErr
			}
			for i := range m.subs {
				if m.subs[i].ID == s.ID {
					m.subs[i] = s
					return nil
				}
			}
			m.subs = append(m.subs, s)
			return nil
		},
		Delete: func(id string) error {
			if !m.inUpdate {
				m.writesOutside++
			}
			for i := range m.subs {
				if m.subs[i].ID == id {
					m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
					return nil
				}
			}
			return nil
		},
	}
}

var t0 = time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)

func oslo() []Server {
	return []Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}}}
}

func TestAddSubscription_ANewLinkIsNamedAfterItsHost(t *testing.T) {
	m := &memStore{}

	sub, existed, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0.Add(1500*time.Millisecond))

	if err != nil || existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
	if !ValidSubscriptionID(sub.ID) || sub.Name != "sub.example.com" || sub.URL != "https://sub.example.com/s/t" {
		t.Fatalf("sub %+v", sub)
	}
	// UTC, whole seconds: the shell writes the same.
	if !sub.Added.Equal(t0.Add(time.Second)) || !sub.Refreshed.Equal(sub.Added) || sub.Added.Location() != time.UTC {
		t.Fatalf("added %v, refreshed %v", sub.Added, sub.Refreshed)
	}
	if len(m.subs) != 1 || m.subs[0].ID != sub.ID || len(m.subs[0].Servers) != 1 {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.10"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
	if m.writesOutside != 0 {
		t.Fatal("a file was written outside the config lock")
	}
}

func TestAddSubscription_ASavedLinkIsRefreshedAndRenamed(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: t0, Refreshed: t0, Error: "download failed: HTTP 403"}}}
	riga := []Server{{Name: "Riga", Address: "r.example.com", Port: 443, IPs: []string{"192.0.2.20"}}}

	sub, existed, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "Main", riga, t0.Add(time.Hour))

	if err != nil || !existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
	if sub.ID != "0a1b2c3d" || sub.Name != "Main" || sub.Error != "" || !sub.Added.Equal(t0) || !sub.Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("sub %+v", sub)
	}
	if len(m.subs) != 1 || m.subs[0].Servers[0].Name != "Riga" || m.subs[0].Name != "Main" {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
}

func TestAddSubscription_RefusesATakenName(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"},
		{ID: "1b2c3d4e", Name: "Beta", URL: "https://other.example.net/s/u"},
	}}

	if _, _, err := AddSubscription(m.update, m.files(), "https://third.example.org/s/v", "alpha", oslo(), t0); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a new link under a taken name: %v", err)
	}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "BETA", oslo(), t0); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a saved link renamed to a taken name: %v", err)
	}
	if len(m.subs) != 2 || m.subs[0].Name != "Alpha" || m.subs[0].Servers != nil {
		t.Fatalf("a refused add wrote %+v", m.subs)
	}
}

func TestAddSubscription_RefusesANameTheRulesRefuse(t *testing.T) {
	m := &memStore{}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "\tAlpha", oslo(), t0); !errors.Is(err, ErrSubscriptionName) {
		t.Fatalf("err %v", err)
	}
	if len(m.subs) != 0 {
		t.Fatalf("wrote %+v", m.subs)
	}
}

func TestAddSubscription_ATakenDefaultGetsASuffix(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "sub.example.com", URL: "https://sub.example.com/s/one"}}}

	sub, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/two", "", oslo(), t0)

	if err != nil || sub.Name != "sub.example.com-2" {
		t.Fatalf("sub %+v, err %v", sub, err)
	}
}

func TestAddSubscription_RefusesAnEleventh(t *testing.T) {
	m := &memStore{}
	for i := 0; i < MaxSubscriptions; i++ {
		m.subs = append(m.subs, Subscription{ID: fmt.Sprintf("%08x", i), Name: fmt.Sprintf("s%d", i), URL: fmt.Sprintf("https://s%d.example.com/s", i)})
	}

	if _, _, err := AddSubscription(m.update, m.files(), "https://new.example.com/s", "", oslo(), t0); !errors.Is(err, ErrSubscriptionLimit) {
		t.Fatalf("err %v", err)
	}
	// A saved link is a refresh, which the limit does not stop.
	if _, existed, err := AddSubscription(m.update, m.files(), "https://s3.example.com/s", "", oslo(), t0); err != nil || !existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
}

// The file is out once the config write fails: the importers say "saved" for
// this failure only.
func TestAddSubscription_SaysWhichHalfLanded(t *testing.T) {
	m := &memStore{configErr: errors.New("disk full")}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0); !errors.Is(err, ErrServersSaved) || len(m.subs) != 1 {
		t.Fatalf("err %v, files %d", err, len(m.subs))
	}

	m = &memStore{saveErr: errors.New("disk full")}
	_, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0)
	if !errors.Is(err, ErrSaveSubscription) || errors.Is(err, ErrServersSaved) {
		t.Fatalf("err %v", err)
	}
}

// A download takes long enough for its subscription to be deleted meanwhile,
// or deleted and its link added again under a new id: publishing it then would
// bring back a file the user removed.
func TestRefreshSubscription_ADeletedSubscriptionStaysDeleted(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "1b2c3d4e", Name: "Alpha", URL: "https://sub.example.com/s/t"}}}

	if _, err := RefreshSubscription(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", oslo(), t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := RefreshSubscription(m.update, m.files(), "1b2c3d4e", "https://sub.example.com/s/other", oslo(), t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("another link: %v", err)
	}
	if len(m.subs) != 1 || m.subs[0].Servers != nil || m.cfg.Xray.Servers != nil {
		t.Fatalf("wrote %+v, %v", m.subs, m.cfg.Xray.Servers)
	}
}

func TestRefreshSubscription_ReplacesTheListAndClearsTheError(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: t0, Refreshed: t0, Error: "download failed: timeout"},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []Server{{Name: "Riga", IPs: []string{"192.0.2.20"}}}},
	}}

	sub, err := RefreshSubscription(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", oslo(), t0.Add(time.Hour))

	if err != nil || sub.Error != "" || !sub.Refreshed.Equal(t0.Add(time.Hour)) || !sub.Added.Equal(t0) {
		t.Fatalf("sub %+v, err %v", sub, err)
	}
	// xray.servers covers every subscription, not just the one refreshed.
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.10", "192.0.2.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
}

func TestRefreshSubscription_AStaticListHasNothingToRefresh(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "1b2c3d4e", Name: "Beta"}}}
	if _, err := RefreshSubscription(m.update, m.files(), "1b2c3d4e", "", oslo(), t0); !errors.Is(err, ErrSubscriptionStatic) {
		t.Fatalf("err %v", err)
	}
}

func TestRecordSubscriptionError_KeepsTheList(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", URL: "https://sub.example.com/s/t", Refreshed: t0, Servers: oslo()}}}

	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: HTTP 403"); err != nil {
		t.Fatal(err)
	}

	if m.subs[0].Error != "download failed: HTTP 403" || len(m.subs[0].Servers) != 1 || !m.subs[0].Refreshed.Equal(t0) {
		t.Fatalf("file %+v", m.subs[0])
	}
}

// Two refreshes can overlap: the Web UI's that succeeded and the watch's that
// began before it and failed. The older one does not mark the newer list failed.
func TestRecordSubscriptionError_ANewerRefreshIsNotMarkedFailed(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", URL: "https://sub.example.com/s/t", Refreshed: t0.Add(time.Minute)}}}

	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: timeout"); err != nil {
		t.Fatal(err)
	}

	if m.subs[0].Error != "" {
		t.Fatalf("error %q written over a newer refresh", m.subs[0].Error)
	}
}

func TestRecordSubscriptionError_ADeletedSubscriptionStaysDeleted(t *testing.T) {
	m := &memStore{}
	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "x"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("err %v", err)
	}
	if len(m.subs) != 0 {
		t.Fatalf("wrote %+v", m.subs)
	}
}

func TestRenameSubscription(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha"}, {ID: "1b2c3d4e", Name: "Beta"}}}

	if sub, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", " ALPHA "); err != nil || sub.Name != "ALPHA" {
		t.Fatalf("its own name in capitals: %+v, %v", sub, err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", "beta"); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "ffffffff", "Gamma"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("no such subscription: %v", err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", ""); !errors.Is(err, ErrSubscriptionName) {
		t.Fatalf("an empty name: %v", err)
	}
	if m.subs[0].Name != "ALPHA" || m.subs[1].Name != "Beta" {
		t.Fatalf("files %+v", m.subs)
	}
}

func TestDeleteSubscription_EndsTheChoiceKeptFromItAndSaysWhatRuns(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []Server{{Name: "Oslo", IPs: []string{"192.0.2.10"}}}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []Server{{Name: "Riga", IPs: []string{"192.0.2.20"}}}},
	}}
	m.cfg.Xray.Servers = []string{"192.0.2.10", "192.0.2.20"}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}
	m.cfg.Xray.PreferredServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}

	active, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d")

	if err != nil || !active {
		t.Fatalf("active %v, err %v", active, err)
	}
	if len(m.subs) != 1 || m.subs[0].ID != "1b2c3d4e" {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.20"}) || m.cfg.Xray.PreferredServer != nil {
		t.Fatalf("xray.servers %v, preferred %+v", m.cfg.Xray.Servers, m.cfg.Xray.PreferredServer)
	}
	// The running Xray is left alone, and so is its record.
	if a := m.cfg.Xray.ActiveServer; a == nil || a.Subscription != "0a1b2c3d" {
		t.Fatalf("active %+v", a)
	}
	if _, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("a second delete: %v", err)
	}
}

func TestDeleteSubscription_KeepsAChoiceFromAnotherSubscription(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha"}, {ID: "1b2c3d4e", Name: "Beta"}}}
	m.cfg.Xray.PreferredServer = &ActiveServer{Subscription: "1b2c3d4e", Name: "Riga"}

	active, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d")

	if err != nil || active || m.cfg.Xray.PreferredServer == nil {
		t.Fatalf("active %v, err %v, preferred %+v", active, err, m.cfg.Xray.PreferredServer)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd server && go test ./internal/vpnconfig/ -run 'Subscription'`
Expected: FAIL — `undefined: AddSubscription`, `undefined: SubscriptionFiles`.

- [ ] **Step 3: Write the operations**

Create `server/internal/vpnconfig/subops.go`:

```go
package vpnconfig

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ConfigUpdate is a locked update of vpn-director.json: lock, load, fn, save,
// unlock. The daemons pass service.ConfigStore.UpdateVPNConfig; the watch
// passes its own update, which also refuses a write once VPN Director is
// stopped.
type ConfigUpdate func(func(*VPNDirectorConfig) error) error

// SubscriptionFiles reaches the subscription files of one data directory. The
// operations below call it only inside the update they are given, so every
// write to the files happens under the config lock.
type SubscriptionFiles struct {
	Load   func() ([]Subscription, error)
	Save   func(Subscription) error
	Delete func(id string) error
}

var (
	// ErrSubscriptionGone is a write for a subscription that no longer exists
	// with the link the caller read: it was deleted, or deleted and its link
	// added again under another id.
	ErrSubscriptionGone = errors.New("the subscription was deleted or changed")
	// ErrSubscriptionLimit refuses an add past MaxSubscriptions.
	ErrSubscriptionLimit = fmt.Errorf("at most %d subscriptions", MaxSubscriptions)
	// ErrSubscriptionNameTaken refuses a name another subscription has.
	ErrSubscriptionNameTaken = errors.New("another subscription has that name")
	// ErrSubscriptionStatic refuses a refresh of a static list.
	ErrSubscriptionStatic = errors.New("a static list has no link to refresh")
	// ErrSaveSubscription marks a failure of the subscription file itself. The
	// config was not written either.
	ErrSaveSubscription = errors.New("save subscription")
)

// stamp is how the files keep time: UTC, whole seconds.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AddSubscription publishes servers, downloaded from rawURL, as a new
// subscription named name - its host when name is empty - under update's lock.
// A link already saved, compared as written, makes it a refresh of that
// subscription instead, and a rename too when name is given; existed says so.
// A name the rules refuse, or another subscription has, refuses the whole call.
//
// xray.servers is recomputed in the same update. A failure after the file was
// written carries ErrServersSaved: the list is out, the config beside it is not.
func AddSubscription(update ConfigUpdate, files SubscriptionFiles, rawURL, name string, servers []Server, now time.Time) (sub Subscription, existed bool, err error) {
	if rawURL == "" {
		return Subscription{}, false, errors.New("a subscription needs a link")
	}
	if name != "" {
		if name, err = CleanSubscriptionName(name); err != nil {
			return Subscription{}, false, err
		}
	}
	saved := false
	err = update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := -1
		for j, s := range subs {
			if s.URL == rawURL {
				i = j
				break
			}
		}
		if i >= 0 {
			existed = true
			sub = subs[i]
			if name != "" && name != sub.Name {
				if SubscriptionNameTaken(subs, name, sub.ID) {
					return ErrSubscriptionNameTaken
				}
				sub.Name = name
			}
		} else {
			if len(subs) >= MaxSubscriptions {
				return ErrSubscriptionLimit
			}
			n := name
			switch {
			case n == "":
				n = DefaultSubscriptionName(subs, hostOf(rawURL))
			case SubscriptionNameTaken(subs, n, ""):
				return ErrSubscriptionNameTaken
			}
			id, err := NewSubscriptionID(subs)
			if err != nil {
				return err
			}
			sub = Subscription{ID: id, Name: n, URL: rawURL, Added: stamp(now)}
			i = len(subs)
			subs = append(subs, sub)
		}
		sub.Servers = servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
		}
		return nil
	})
	switch {
	case err == nil:
		return sub, existed, nil
	case saved:
		return sub, existed, ServersSaved(err)
	default:
		return Subscription{}, existed, err
	}
}

// RefreshSubscription replaces the servers of subscription id with servers,
// downloaded from rawURL, under update's lock, and clears its error - only
// while that subscription still exists with that link: ErrSubscriptionGone
// otherwise, and nothing is written. xray.servers is recomputed in the same
// update.
func RefreshSubscription(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error) {
	if rawURL == "" {
		return Subscription{}, ErrSubscriptionStatic
	}
	var sub Subscription
	saved := false
	err := update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		sub = subs[i]
		sub.Servers = servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
		}
		return nil
	})
	switch {
	case err == nil:
		return sub, nil
	case saved:
		return sub, ServersSaved(err)
	default:
		return Subscription{}, err
	}
}

// RecordSubscriptionError notes msg, why a refresh of subscription id from
// rawURL failed; the list stays. It writes nothing when the subscription is
// gone or has another link (ErrSubscriptionGone), and nothing when its
// Refreshed is no longer since, the time the caller read before it began: a
// refresh that succeeded meanwhile is not marked failed by an older one.
func RecordSubscriptionError(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, since time.Time, msg string) error {
	return update(func(*VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		if !subs[i].Refreshed.Equal(since) {
			return nil
		}
		sub := subs[i]
		sub.Error = msg
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		return nil
	})
}

// RenameSubscription gives subscription id the name name, under update's lock.
func RenameSubscription(update ConfigUpdate, files SubscriptionFiles, id, name string) (Subscription, error) {
	name, err := CleanSubscriptionName(name)
	if err != nil {
		return Subscription{}, err
	}
	var sub Subscription
	err = update(func(*VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 {
			return ErrSubscriptionGone
		}
		if SubscriptionNameTaken(subs, name, id) {
			return ErrSubscriptionNameTaken
		}
		sub = subs[i]
		sub.Name = name
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		return nil
	})
	return sub, err
}

// DeleteSubscription removes subscription id under update's lock, recomputes
// xray.servers without it and ends a preferred_server that names it. The
// running Xray is left alone: activeWasIn says active_server names the
// subscription, for the caller to tell the user to select another server. A
// failure after the file was removed carries ErrServersSaved.
func DeleteSubscription(update ConfigUpdate, files SubscriptionFiles, id string) (activeWasIn bool, err error) {
	deleted := false
	err = update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 {
			return ErrSubscriptionGone
		}
		rest := append(append([]Subscription{}, subs[:i]...), subs[i+1:]...)
		if err := files.Delete(id); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		deleted = true
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(rest)
			if p := cfg.Xray.PreferredServer; p != nil && p.Subscription == id {
				cfg.Xray.PreferredServer = nil
			}
			activeWasIn = cfg.Xray.ActiveServer != nil && cfg.Xray.ActiveServer.Subscription == id
		}
		return nil
	})
	if err != nil && deleted {
		return activeWasIn, ServersSaved(err)
	}
	return activeWasIn, err
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd server && go test ./internal/vpnconfig/... && gofmt -l internal/vpnconfig`
Expected: PASS, `gofmt -l` silent.

- [ ] **Step 5: Commit**

```bash
git add server/internal/vpnconfig/subops.go server/internal/vpnconfig/subops_test.go
git commit -m "feat(vpnconfig): add, refresh, rename and delete a subscription under the config lock"
```

---

### Task 3: `ConfigStore` reads and writes the subscription files

**Files:**
- Modify: `server/internal/service/interfaces.go` (`ConfigStore`)
- Modify: `server/internal/service/config.go` (`SubscriptionsDir`, `LoadSubscriptions`, `SaveSubscription`, `DeleteSubscription`, `LoadServers`)
- Modify: `server/internal/service/config_test.go`
- Modify (every `ConfigStore` fake gains the three methods): `server/internal/webapi/test_helpers_test.go` (`mockConfig`), `server/internal/handler/status_test.go` (`mockConfigStore`), `server/internal/handler/clients_test.go` (`mockConfigClients`), `server/internal/wizard/server_test.go` (`mockConfigStore`), `server/internal/wizard/apply_test.go` (`trackingConfigStore`), `server/internal/service/activeserver_test.go` (`stubStore`)

**Interfaces:**
- Consumes (Task 1): `vpnconfig.SubscriptionsDir`, `LoadSubscriptions`, `SaveSubscription`, `DeleteSubscriptionFile`, `RemoveLegacyServers`, `AllServers`.
- Produces:
  - `ConfigStore.LoadSubscriptions() ([]vpnconfig.Subscription, error)`
  - `ConfigStore.SaveSubscription(vpnconfig.Subscription) error`
  - `ConfigStore.DeleteSubscription(id string) error`
  - `(*ConfigService).SubscriptionsDir() (string, error)`
  - `(*ConfigService).LoadServers()` now returns `vpnconfig.AllServers` of the subscriptions (each server carries its subscription id).

`SaveServers` stays in the interface until Task 12: the importers that still call it are rewritten in Tasks 5, 7 and 11.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/service/config_test.go` (add `fmt` to its imports):

```go
// newStoreWithData is a ConfigService whose config names dataDir.
func newStoreWithData(t *testing.T) (*ConfigService, string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	writeTestConfig(t, dir, fmt.Sprintf(`{"data_dir": %q}`, data))
	return NewConfigService(dir, filepath.Join(dir, "default-data")), data
}

func TestConfigService_SubscriptionsLiveInTheDataDirectory(t *testing.T) {
	svc, data := newStoreWithData(t)
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t",
		Servers: []vpnconfig.Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}}}}

	if err := svc.SaveSubscription(sub); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(data, "subscriptions", "0a1b2c3d.json")); err != nil {
		t.Fatal(err)
	}
	subs, err := svc.LoadSubscriptions()
	if err != nil || len(subs) != 1 || subs[0].Name != "Alpha" {
		t.Fatalf("subs %+v, err %v", subs, err)
	}
	servers, err := svc.LoadServers()
	if err != nil || len(servers) != 1 || servers[0].Subscription != "0a1b2c3d" {
		t.Fatalf("servers %+v, err %v", servers, err)
	}
	if err := svc.DeleteSubscription("0a1b2c3d"); err != nil {
		t.Fatal(err)
	}
	if subs, err := svc.LoadSubscriptions(); err != nil || len(subs) != 0 {
		t.Fatalf("after delete: %+v, %v", subs, err)
	}
}

// servers.json is the single list of earlier releases. Nothing reads it now,
// and the first write of a subscription takes it away.
func TestConfigService_ASubscriptionWriteRemovesTheOldServerList(t *testing.T) {
	svc, data := newStoreWithData(t)
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "servers.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(data, "servers.json")); !os.IsNotExist(err) {
		t.Fatal("servers.json survived the first subscription write")
	}
}

// A router fresh from the previous release has servers.json and no
// subscription: nothing reads the old list, and no server is listed.
func TestConfigService_NoSubscriptionListsNoServer(t *testing.T) {
	svc, data := newStoreWithData(t)
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "servers.json"), []byte(`[{"name":"Old","address":"a.example.com","port":443}]`), 0600); err != nil {
		t.Fatal(err)
	}

	servers, err := svc.LoadServers()

	if err != nil || len(servers) != 0 {
		t.Fatalf("servers %+v, err %v", servers, err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd server && go test ./internal/service/ -run 'Subscription|NoSubscription'`
Expected: FAIL — `svc.SaveSubscription undefined`.

- [ ] **Step 3: Extend the interface and the service**

In `server/internal/service/interfaces.go`, add to `ConfigStore` after `SaveServers`:

```go
	// LoadSubscriptions reads every subscription file, ordered by when it was
	// added; LoadServers is their servers, flattened in that order.
	LoadSubscriptions() ([]vpnconfig.Subscription, error)
	// SaveSubscription and DeleteSubscription write one subscription file.
	// Call them only inside UpdateVPNConfig, so every write to the files
	// happens under the config lock - vpnconfig's subscription operations do.
	SaveSubscription(vpnconfig.Subscription) error
	DeleteSubscription(id string) error
```

In `server/internal/service/config.go` (add `log/slog` to the imports), replace `LoadServers` and add the rest after it:

```go
// SubscriptionsDir is where the data directory keeps the subscription files.
func (s *ConfigService) SubscriptionsDir() (string, error) {
	dataDir, err := s.DataDir()
	if err != nil {
		return "", err
	}
	return vpnconfig.SubscriptionsDir(dataDir), nil
}

// LoadSubscriptions reads every subscription, in order (vpnconfig.LoadSubscriptions).
func (s *ConfigService) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	dir, err := s.SubscriptionsDir()
	if err != nil {
		return nil, err
	}
	return vpnconfig.LoadSubscriptions(dir)
}

// SaveSubscription writes one subscription file. The caller holds the config
// lock. It also takes away the servers.json of earlier releases.
func (s *ConfigService) SaveSubscription(sub vpnconfig.Subscription) error {
	dataDir, err := s.DataDir()
	if err != nil {
		return err
	}
	if err := vpnconfig.SaveSubscription(vpnconfig.SubscriptionsDir(dataDir), sub); err != nil {
		return err
	}
	s.removeLegacyServers(dataDir)
	return nil
}

// DeleteSubscription removes one subscription file. The caller holds the
// config lock. It also takes away the servers.json of earlier releases.
func (s *ConfigService) DeleteSubscription(id string) error {
	dataDir, err := s.DataDir()
	if err != nil {
		return err
	}
	if err := vpnconfig.DeleteSubscriptionFile(vpnconfig.SubscriptionsDir(dataDir), id); err != nil {
		return err
	}
	s.removeLegacyServers(dataDir)
	return nil
}

// removeLegacyServers is best effort: the subscription write it follows has
// happened, and a servers.json left behind is read by nobody.
func (s *ConfigService) removeLegacyServers(dataDir string) {
	if err := vpnconfig.RemoveLegacyServers(dataDir); err != nil {
		slog.Warn("Failed to remove the servers.json of an earlier release", "error", err)
	}
}

// LoadServers is every server of every subscription, in subscription order,
// each carrying its subscription's id (vpnconfig.AllServers).
func (s *ConfigService) LoadServers() ([]vpnconfig.Server, error) {
	subs, err := s.LoadSubscriptions()
	if err != nil {
		return nil, err
	}
	return vpnconfig.AllServers(subs), nil
}
```

- [ ] **Step 4: Give every fake the three methods**

`server/internal/webapi/test_helpers_test.go` — add fields to `mockConfig` and the methods below it (add `sync` to the imports if the file lacks it):

```go
	// subs are the subscription files. UpdateVPNConfig serializes on upd as
	// the flock does, and subsMu guards subs: RefreshAll runs one refresh per
	// subscription at once.
	subs    []vpnconfig.Subscription
	subsErr error // LoadSubscriptions fails
	upd     sync.Mutex
	subsMu  sync.Mutex
```

```go
func (m *mockConfig) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	if m.subsErr != nil {
		return nil, m.subsErr
	}
	out := make([]vpnconfig.Subscription, len(m.subs))
	for i, s := range m.subs {
		for j := range s.Servers {
			s.Servers[j].Subscription = s.ID
		}
		out[i] = s
	}
	return out, nil
}

func (m *mockConfig) SaveSubscription(sub vpnconfig.Subscription) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == sub.ID {
			m.subs[i] = sub
			return nil
		}
	}
	m.subs = append(m.subs, sub)
	return nil
}

func (m *mockConfig) DeleteSubscription(id string) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == id {
			m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
			return nil
		}
	}
	return nil
}
```

and make `m.upd.Lock()` and `defer m.upd.Unlock()` the first two statements of `mockConfig.UpdateVPNConfig`, above its `if m.updateErr != nil` check; the rest of the method stays as it is.

`server/internal/handler/status_test.go` — `mockConfigStore` gets a `subs []vpnconfig.Subscription` field and:

```go
func (m *mockConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	return m.subs, m.err
}
func (m *mockConfigStore) SaveSubscription(vpnconfig.Subscription) error { return m.err }
func (m *mockConfigStore) DeleteSubscription(string) error              { return m.err }
```

`server/internal/handler/clients_test.go` — `mockConfigClients`:

```go
func (m *mockConfigClients) LoadSubscriptions() ([]vpnconfig.Subscription, error) { return nil, nil }
func (m *mockConfigClients) SaveSubscription(vpnconfig.Subscription) error         { return nil }
func (m *mockConfigClients) DeleteSubscription(string) error                        { return nil }
```

`server/internal/wizard/server_test.go` — `mockConfigStore` gets a `subs []vpnconfig.Subscription` field and:

```go
func (m *mockConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	return m.subs, m.err
}
func (m *mockConfigStore) SaveSubscription(vpnconfig.Subscription) error { return m.err }
func (m *mockConfigStore) DeleteSubscription(string) error              { return m.err }
```

`server/internal/wizard/apply_test.go` — `trackingConfigStore` gets a `subs []vpnconfig.Subscription` field and:

```go
func (m *trackingConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	return m.subs, m.loadErr
}
func (m *trackingConfigStore) SaveSubscription(vpnconfig.Subscription) error { return m.saveErr }
func (m *trackingConfigStore) DeleteSubscription(string) error              { return m.saveErr }
```

`server/internal/service/activeserver_test.go` — `stubStore`:

```go
func (s *stubStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) { return nil, nil }
func (s *stubStore) SaveSubscription(vpnconfig.Subscription) error         { return nil }
func (s *stubStore) DeleteSubscription(string) error                        { return nil }
```

`mockConfigStoreForImport` in `handler/import_test.go` embeds `mockConfigStore` and needs nothing.

- [ ] **Step 5: Run the whole suite**

Run: `cd server && go vet ./... && go test ./... && gofmt -l .`
Expected: PASS; `gofmt -l` lists at most `internal/ssrf/ssrf_test.go` and `internal/wizard/handler.go`, which were unformatted on `master` before this work.

- [ ] **Step 6: Commit**

```bash
git add server/internal/service server/internal/webapi/test_helpers_test.go server/internal/handler/status_test.go \
  server/internal/handler/clients_test.go server/internal/wizard/server_test.go server/internal/wizard/apply_test.go
git commit -m "feat(service): read and write the subscription files through ConfigStore"
```

---

### Task 4: The daemons' download path and subscription operations

**Files:**
- Create: `server/internal/service/subscriptions.go`
- Create: `server/internal/service/subscriptions_test.go`

**Interfaces:**
- Consumes: Task 2's operations and errors; Task 3's `ConfigStore`; existing `ssrf.IsPrivateHost`, `ssrf.ErrBlockedAddress`, `subscription.DecodeAndResolveLookup`, `subscription.LookupIPv4`, `subscription.Import` (`Summary`, `NoServers`).
- Produces:
  - `const MaxSubscriptionBody = 1 << 20`
  - `var ErrSubscriptionURL`
  - `type DownloadError struct{ Err error }` — the subscription did not arrive (the Web UI answers 502)
  - `type BodyError struct{ Err error }` — it arrived and holds nothing usable (400)
  - `func ValidateSubscriptionURL(raw string) error`
  - `func DownloadSubscription(ctx context.Context, client *http.Client, rawURL string) (subscription.Import, error)`
  - `type SubscriptionResult struct { ID, Name string; Existed bool; Import subscription.Import; Err error }` with `func (r SubscriptionResult) Line() string`
  - `func SubscriptionFilesOf(store ConfigStore) vpnconfig.SubscriptionFiles`
  - `func AddSubscription(ctx context.Context, store ConfigStore, client *http.Client, rawURL, name string) SubscriptionResult`
  - `func RefreshSubscription(ctx context.Context, store ConfigStore, client *http.Client, id string) SubscriptionResult`
  - `func RefreshAllSubscriptions(ctx context.Context, store ConfigStore, client *http.Client) ([]SubscriptionResult, error)`
  - `func RenameSubscription(store ConfigStore, id, name string) error`
  - `func DeleteSubscription(store ConfigStore, id string) (activeWasIn bool, err error)`

- [ ] **Step 1: Write the failing tests**

Create `server/internal/service/subscriptions_test.go`:

```go
package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// memConfigStore is a ConfigStore over memory. UpdateVPNConfig serializes on
// upd as the flock does, and mu guards the data: RefreshAll refreshes every
// subscription at once.
type memConfigStore struct {
	upd  sync.Mutex
	mu   sync.Mutex
	cfg  *vpnconfig.VPNDirectorConfig
	subs []vpnconfig.Subscription
}

func newMemConfigStore(subs ...vpnconfig.Subscription) *memConfigStore {
	return &memConfigStore{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
}

func (m *memConfigStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := *m.cfg
	return &cfg, nil
}
func (m *memConfigStore) LoadServers() ([]vpnconfig.Server, error) {
	subs, _ := m.LoadSubscriptions()
	return vpnconfig.AllServers(subs), nil
}
func (m *memConfigStore) SaveServers([]vpnconfig.Server) error { return errors.New("no servers.json") }
func (m *memConfigStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	cfg, _ := m.LoadVPNConfig()
	if err := fn(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return nil
}
func (m *memConfigStore) DataDir() (string, error) { return "/tmp/test-data", nil }
func (m *memConfigStore) DataDirOrDefault() string { return "/tmp/test-data" }
func (m *memConfigStore) ScriptsDir() string       { return "/tmp/test-scripts" }
func (m *memConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vpnconfig.Subscription, len(m.subs))
	copy(out, m.subs)
	return out, nil
}
func (m *memConfigStore) SaveSubscription(sub vpnconfig.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == sub.ID {
			m.subs[i] = sub
			return nil
		}
	}
	m.subs = append(m.subs, sub)
	return nil
}
func (m *memConfigStore) DeleteSubscription(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == id {
			m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
			return nil
		}
	}
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// subscriptionHost answers every request with the handler's answer, whatever
// host the URL names: the checks in front of the download see the public
// address a test uses.
func subscriptionHost(t *testing.T, h http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})}
}

func serve(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

var osloBody = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))

const publicLink = "https://93.184.216.34/s/token"

func TestAddSubscription_SavesTheListUnderItsHost(t *testing.T) {
	store := newMemConfigStore()

	res := AddSubscription(context.Background(), store, subscriptionHost(t, serve(osloBody)), publicLink, "")

	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Name != "93.184.216.34" || res.Existed || len(res.Import.Servers) != 1 || !vpnconfig.ValidSubscriptionID(res.ID) {
		t.Fatalf("result %+v", res)
	}
	if got := res.Line(); got != "93.184.216.34: Imported 1 servers" {
		t.Fatalf("line %q", got)
	}
	if len(store.subs) != 1 || store.subs[0].URL != publicLink {
		t.Fatalf("files %+v", store.subs)
	}
	if !reflect.DeepEqual(store.cfg.Xray.Servers, []string{"203.0.113.10"}) {
		t.Fatalf("xray.servers %v", store.cfg.Xray.Servers)
	}
}

func TestAddSubscription_RefusesALinkTheDaemonsDoNotDownload(t *testing.T) {
	store := newMemConfigStore()
	for _, raw := range []string{"http://sub.example.com/s", "https://127.0.0.1/s", "https://192.168.1.1/s", "ftp://sub.example.com/s", "no link", "https:///s"} {
		if res := AddSubscription(context.Background(), store, nil, raw, ""); !errors.Is(res.Err, ErrSubscriptionURL) {
			t.Errorf("%q: %v", raw, res.Err)
		}
	}
	if len(store.subs) != 0 {
		t.Fatalf("wrote %+v", store.subs)
	}
}

func TestAddSubscription_ATakenNameIsRefused(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"})

	res := AddSubscription(context.Background(), store, subscriptionHost(t, serve(osloBody)), publicLink, "alpha")

	if !errors.Is(res.Err, vpnconfig.ErrSubscriptionNameTaken) || len(store.subs) != 1 {
		t.Fatalf("err %v, files %d", res.Err, len(store.subs))
	}
}

// A body over the cap is refused, never cut: cut, a base64 list decodes to a
// shorter one, which would be published as the subscription.
func TestDownloadSubscription_ABodyOverTheCapDidNotArrive(t *testing.T) {
	line := "vless://uuid-1@203.0.113.10:443?type=tcp#Oslo\n"
	body := base64.StdEncoding.EncodeToString([]byte(strings.Repeat(line, MaxSubscriptionBody/len(line))))

	_, err := DownloadSubscription(context.Background(), subscriptionHost(t, serve(body)), publicLink)

	var de *DownloadError
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("err %v", err)
	}
}

func TestDownloadSubscription_AnHTTPErrorDidNotArrive(t *testing.T) {
	forbidden := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }

	_, err := DownloadSubscription(context.Background(), subscriptionHost(t, forbidden), publicLink)

	var de *DownloadError
	if !errors.As(err, &de) || err.Error() != "download failed: HTTP 403" {
		t.Fatalf("err %v", err)
	}
}

func TestDownloadSubscription_ABodyWithoutAServerSaysWhy(t *testing.T) {
	for body, want := range map[string]string{
		"tuic://u:p@203.0.113.9:443#TUIC":     "no supported servers",
		"vless://u@[2001:db8::1]:443#SixOnly": "could not resolve IP for any server",
	} {
		_, err := DownloadSubscription(context.Background(), subscriptionHost(t, serve(body)), publicLink)
		var be *BodyError
		if !errors.As(err, &be) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// A *url.Error's text is the whole URL, and the subscription token sits in its path.
func TestDownloadError_NamesNoLink(t *testing.T) {
	err := downloadError(&url.Error{Op: "Get", URL: "https://sub.example.com/s/secret-token", Err: errors.New("i/o timeout")})
	if strings.Contains(err.Error(), "secret-token") || err.Error() != "download failed: i/o timeout" {
		t.Fatalf("%q", err)
	}
	blocked := downloadError(&url.Error{Op: "Get", URL: "https://x/s/t", Err: fmt.Errorf("dial: %w: 10.0.0.1", ssrf.ErrBlockedAddress)})
	if strings.Contains(blocked.Error(), "10.0.0.1") {
		t.Fatalf("the blocked address reached the text: %q", blocked)
	}
}

func TestRefreshSubscription_AFailedDownloadRecordsWhyAndKeepsTheList(t *testing.T) {
	old := []vpnconfig.Server{{Name: "Old", Address: "old.example.com", Port: 443, IPs: []string{"192.0.2.1"}}}
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: publicLink, Servers: old})
	forbidden := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }

	res := RefreshSubscription(context.Background(), store, subscriptionHost(t, forbidden), "0a1b2c3d")

	if res.Err == nil || res.Line() != "Alpha: download failed: HTTP 403" {
		t.Fatalf("result %+v, line %q", res, res.Line())
	}
	if s := store.subs[0]; s.Error != "download failed: HTTP 403" || len(s.Servers) != 1 || s.Servers[0].Name != "Old" {
		t.Fatalf("file %+v", s)
	}
}

func TestRefreshSubscription_UnknownAndStatic(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	if res := RefreshSubscription(context.Background(), store, nil, "0a1b2c3d"); !errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) {
		t.Fatalf("unknown: %v", res.Err)
	}
	if res := RefreshSubscription(context.Background(), store, nil, "1b2c3d4e"); !errors.Is(res.Err, vpnconfig.ErrSubscriptionStatic) {
		t.Fatalf("static: %v", res.Err)
	}
}

func TestRefreshAllSubscriptions_OneResultPerLinkInOrder(t *testing.T) {
	store := newMemConfigStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
		vpnconfig.Subscription{ID: "2c3d4e5f", Name: "Gamma", URL: "https://93.184.216.34/b"},
	)
	client := subscriptionHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(osloBody))
	})

	results, err := RefreshAllSubscriptions(context.Background(), store, client)

	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	want := []string{"Alpha: Imported 1 servers", "Gamma: download failed: HTTP 403"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

func TestDeleteSubscription_SaysTheRunningServerCameFromIt(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}

	active, err := DeleteSubscription(store, "0a1b2c3d")

	if err != nil || !active || len(store.subs) != 0 {
		t.Fatalf("active %v, err %v, files %d", active, err, len(store.subs))
	}
}

func TestRenameSubscription_Refusals(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	if err := RenameSubscription(store, "0a1b2c3d", "BETA"); !errors.Is(err, vpnconfig.ErrSubscriptionNameTaken) {
		t.Fatalf("taken: %v", err)
	}
	if err := RenameSubscription(store, "0a1b2c3d", "Main"); err != nil || store.subs[0].Name != "Main" {
		t.Fatalf("err %v, files %+v", err, store.subs)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd server && go test ./internal/service/ -run 'Subscription|Download'`
Expected: FAIL — `undefined: AddSubscription`, `undefined: DownloadSubscription`.

- [ ] **Step 3: Write the service**

Create `server/internal/service/subscriptions.go`:

```go
// internal/service/subscriptions.go
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subscription"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// MaxSubscriptionBody is the largest subscription taken, in bytes. A larger
// one is refused, never cut: cut, a base64 list decodes to a shorter one, and
// that would be published as the subscription.
const MaxSubscriptionBody = 1 << 20

// ErrSubscriptionURL refuses a link the daemons do not download: not https, or
// naming a private or reserved host.
var ErrSubscriptionURL = errors.New("invalid subscription URL")

// DownloadError is a subscription that did not arrive: the connection, the
// HTTP status or the size cap. The Web UI answers it with 502.
type DownloadError struct{ Err error }

func (e *DownloadError) Error() string {
	if errors.Is(e.Err, ssrf.ErrBlockedAddress) {
		// The dial guard's error carries the resolved internal address, and
		// this text goes to the browser and the chat.
		return "download failed: URL resolved to a private or reserved address"
	}
	return "download failed: " + e.Err.Error()
}

func (e *DownloadError) Unwrap() error { return e.Err }

// downloadError wraps a failed download. A *url.Error gives up its URL first:
// its text is the whole link, token included.
func downloadError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &DownloadError{Err: err}
}

// BodyError is a subscription that arrived and holds no server the router can
// use: nothing it decodes, or nothing whose host resolves. 400 in the Web UI.
type BodyError struct{ Err error }

func (e *BodyError) Error() string { return e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

// ValidateSubscriptionURL refuses what the daemons do not download: anything
// but an https link, and a link to a private or reserved host. The dial guard
// of ssrf.NewClient is the authoritative check and also defeats DNS
// rebinding; this one gives the clear message early.
func ValidateSubscriptionURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("%w: use an https:// link", ErrSubscriptionURL)
	}
	if ssrf.IsPrivateHost(u.Hostname()) {
		return fmt.Errorf("%w: it must not point to a private or loopback address", ErrSubscriptionURL)
	}
	return nil
}

// DownloadSubscription fetches rawURL through client - the SSRF-hardened one -
// then decodes the body and resolves every host over IPv4, bound to ctx. The
// Web UI and the bot's /import download here; the watch has its own path (the
// WAN, then the tunnel).
func DownloadSubscription(ctx context.Context, client *http.Client, rawURL string) (subscription.Import, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return subscription.Import{}, fmt.Errorf("%w: use an https:// link", ErrSubscriptionURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return subscription.Import{}, downloadError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return subscription.Import{}, &DownloadError{Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	// One byte past the cap tells a list that is too long from one that fits exactly.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubscriptionBody+1))
	if err != nil {
		return subscription.Import{}, downloadError(err)
	}
	if len(body) > MaxSubscriptionBody {
		return subscription.Import{}, &DownloadError{Err: errors.New("subscription exceeds 1 MiB")}
	}
	imp, err := subscription.DecodeAndResolveLookup(string(body), subscription.LookupIPv4(ctx))
	switch {
	case err != nil:
		return imp, &BodyError{Err: err}
	case imp.Parsed == 0:
		return imp, &BodyError{Err: errors.New(imp.NoServers())}
	case len(imp.Servers) == 0:
		return imp, &BodyError{Err: errors.New("could not resolve IP for any server")}
	}
	return imp, nil
}

// SubscriptionResult is what one add or refresh came to, for the Web UI and
// the bot to report.
type SubscriptionResult struct {
	ID      string
	Name    string
	Existed bool                // an add that refreshed a saved link
	Import  subscription.Import // what the download decoded to
	Err     error
}

// Line is the one line a result reads as: "Alpha: Imported 32 of 40 servers:
// 7 composite" or "Alpha: download failed: HTTP 403".
func (r SubscriptionResult) Line() string {
	name := r.Name
	if name == "" {
		name = "subscription"
	}
	if r.Err != nil {
		return name + ": " + r.Err.Error()
	}
	return name + ": " + r.Import.Summary()
}

// SubscriptionFilesOf is store's subscription files, for vpnconfig's operations.
func SubscriptionFilesOf(store ConfigStore) vpnconfig.SubscriptionFiles {
	return vpnconfig.SubscriptionFiles{Load: store.LoadSubscriptions, Save: store.SaveSubscription, Delete: store.DeleteSubscription}
}

// AddSubscription downloads rawURL and saves it as a subscription named name,
// its host when name is empty. A link already saved is refreshed instead
// (vpnconfig.AddSubscription).
func AddSubscription(ctx context.Context, store ConfigStore, client *http.Client, rawURL, name string) SubscriptionResult {
	res := SubscriptionResult{}
	if err := ValidateSubscriptionURL(rawURL); err != nil {
		res.Err = err
		return res
	}
	if name != "" {
		clean, err := vpnconfig.CleanSubscriptionName(name)
		if err != nil {
			res.Err = err
			return res
		}
		res.Name = clean
	}
	imp, err := DownloadSubscription(ctx, client, rawURL)
	res.Import = imp
	if err != nil {
		res.Err = err
		return res
	}
	sub, existed, err := vpnconfig.AddSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), rawURL, res.Name, imp.Servers, time.Now())
	res.Existed, res.Err = existed, err
	if sub.ID != "" {
		res.ID, res.Name = sub.ID, sub.Name
	}
	return res
}

// RefreshSubscription downloads the saved link of subscription id again and
// publishes what arrived. A download that fails records why in the
// subscription's error, and the list stays.
func RefreshSubscription(ctx context.Context, store ConfigStore, client *http.Client, id string) SubscriptionResult {
	subs, err := store.LoadSubscriptions()
	if err != nil {
		return SubscriptionResult{ID: id, Err: err}
	}
	i := vpnconfig.FindSubscription(subs, id)
	if i < 0 {
		return SubscriptionResult{ID: id, Err: vpnconfig.ErrSubscriptionGone}
	}
	return refresh(ctx, store, client, subs[i])
}

func refresh(ctx context.Context, store ConfigStore, client *http.Client, sub vpnconfig.Subscription) SubscriptionResult {
	res := SubscriptionResult{ID: sub.ID, Name: sub.Name, Existed: true}
	if sub.Static() {
		res.Err = vpnconfig.ErrSubscriptionStatic
		return res
	}
	imp, err := DownloadSubscription(ctx, client, sub.URL)
	res.Import = imp
	if err != nil {
		res.Err = err
		rerr := vpnconfig.RecordSubscriptionError(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, sub.Refreshed, err.Error())
		if rerr != nil && !errors.Is(rerr, vpnconfig.ErrSubscriptionGone) {
			slog.Warn("Failed to record why a subscription did not refresh", "subscription", sub.Name, "error", rerr)
		}
		return res
	}
	_, res.Err = vpnconfig.RefreshSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, imp.Servers, time.Now())
	return res
}

// RefreshAllSubscriptions refreshes every subscription with a link at once and
// answers one result each, in subscription order. Static lists are left out.
func RefreshAllSubscriptions(ctx context.Context, store ConfigStore, client *http.Client) ([]SubscriptionResult, error) {
	subs, err := store.LoadSubscriptions()
	if err != nil {
		return nil, err
	}
	var linked []vpnconfig.Subscription
	for _, s := range subs {
		if !s.Static() {
			linked = append(linked, s)
		}
	}
	results := make([]SubscriptionResult, len(linked))
	var wg sync.WaitGroup
	for i, s := range linked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = refresh(ctx, store, client, s)
		}()
	}
	wg.Wait()
	return results, nil
}

// RenameSubscription gives subscription id the name name.
func RenameSubscription(store ConfigStore, id, name string) error {
	_, err := vpnconfig.RenameSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), id, name)
	return err
}

// DeleteSubscription removes subscription id; activeWasIn says the running
// server came from it (vpnconfig.DeleteSubscription).
func DeleteSubscription(store ConfigStore, id string) (activeWasIn bool, err error) {
	return vpnconfig.DeleteSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), id)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd server && go test ./internal/service/... && go vet ./internal/service/ && gofmt -l internal/service`
Expected: PASS, nothing from `go vet` and `gofmt -l`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/service/subscriptions.go server/internal/service/subscriptions_test.go
git commit -m "feat(service): one download path and the subscription operations for both daemons"
```

---

### Task 5: Web UI API — `/api/subscriptions`, servers by subscription

**Files:**
- Create: `server/internal/webapi/handler_subscriptions.go`
- Create: `server/internal/webapi/handler_subscriptions_test.go`
- Modify: `server/internal/webapi/handler_servers.go` (list grouped, select by subscription; the import route and its helpers go)
- Modify: `server/internal/webapi/handler_servers_test.go`
- Modify: `server/internal/webapi/test_helpers_test.go` (receives `roundTripFunc`, `subscriptionHost`, `osloSubscription` from `handler_servers_test.go`)
- Modify: `server/internal/webapi/router.go`, `server/internal/webapi/deadline.go`

**Interfaces:**
- Consumes (Task 4): `service.AddSubscription`, `RefreshSubscription`, `RefreshAllSubscriptions`, `RenameSubscription`, `DeleteSubscription`, `SubscriptionResult`, `DownloadError`, `BodyError`, `ErrSubscriptionURL`; (Task 2) the `vpnconfig.ErrSubscription*` errors; (Task 1) `vpnconfig.FindSubscription`, `SubscriptionIPs`.
- Produces the HTTP API of spec 6.1:
  - `GET /api/subscriptions` → `{"subscriptions": [{id, name, host, static, servers, added, refreshed, error?}]}`
  - `POST /api/subscriptions` body `{url, name}` → `{ok, id, name, existed, summary, count, total, skipped, dns_errors}`
  - `POST /api/subscriptions/refresh[?id=]` → `{"results": [{id, name, existed, summary, error? | count, total, skipped, dns_errors}]}`
  - `POST /api/subscriptions/rename?id=` body `{name}` → `{ok}`
  - `DELETE /api/subscriptions?id=` → `{ok, active_removed}`
  - `GET /api/servers` → `{"subscriptions": [{id, name, servers: [serverView]}], "active": ActiveServer|null}`
  - `POST /api/servers/active` body `{subscription, index, name, address, port}`

Every mutating route takes `Deps.OpMutex` through `lockLongOp`, as the spec asks; the download inside a subscription route runs under it with its own 90-second bound (`subscriptionTimeout`), inside the route's two-minute write deadline.

- [ ] **Step 1: Move the test helpers**

Cut `roundTripFunc`, its `RoundTrip` method, `subscriptionHost` and `osloSubscription` from `handler_servers_test.go` and paste them unchanged at the end of `test_helpers_test.go`; move their imports (`encoding/base64`, `net/http/httptest`, `net/url`) with them.

- [ ] **Step 2: Write the failing tests for the subscription routes**

Create `server/internal/webapi/handler_subscriptions_test.go`:

```go
package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// alphaBeta is two subscriptions that both name a server Germany-1.
func alphaBeta() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Servers: []vpnconfig.Server{
			{Name: "Oslo", Address: "a.example.com", Port: 443, UUID: "uuid-1", IPs: []string{"192.0.2.10"}},
			{Name: "Germany-1", Address: "b.example.com", Port: 443, UUID: "uuid-2", IPs: []string{"192.0.2.11"}},
		}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{
			{Name: "Germany-1", Address: "198.51.100.20", Port: 8443, UUID: "uuid-3", IPs: []string{"198.51.100.20"}},
		}},
	}
}

func subsDeps(t *testing.T, subs ...vpnconfig.Subscription) (*Deps, *mockConfig) {
	t.Helper()
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
	deps := newTestDeps(t)
	deps.Config = mc
	return deps, mc
}

// call serves one request and decodes the JSON answer.
func call(t *testing.T, h http.HandlerFunc, method, target, body string) (int, map[string]interface{}) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, reader))
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func TestHandleListSubscriptions_ShowsTheHostAndNoLink(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	rec := httptest.NewRecorder()

	handleListSubscriptions(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/subscriptions", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "/s/t") || strings.Contains(body, "uuid-") {
		t.Fatalf("%d %s", rec.Code, body)
	}
	var resp struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	s := resp.Subscriptions
	if len(s) != 2 || s[0].Host != "sub.example.com" || s[0].Servers != 2 || s[0].Static || !s[1].Static || s[1].Host != "" {
		t.Fatalf("%+v", s)
	}
}

func TestHandleListSubscriptions_NoneIsAnEmptyList(t *testing.T) {
	deps, _ := subsDeps(t)
	rec := httptest.NewRecorder()

	handleListSubscriptions(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/subscriptions", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"subscriptions":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAddSubscription_SavesAndSummarizes(t *testing.T) {
	deps, mc := subsDeps(t)
	deps.ImportClient = subscriptionHost(t, osloSubscription)

	code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/token","name":"Alpha"}`)

	if code != http.StatusOK || resp["name"] != "Alpha" || resp["summary"] != "Alpha: Imported 1 servers" || resp["count"] != float64(1) || resp["existed"] != false {
		t.Fatalf("%d %v", code, resp)
	}
	if len(mc.subs) != 1 || mc.subs[0].URL != "https://93.184.216.34/s/token" {
		t.Fatalf("files %+v", mc.subs)
	}
	if !reflect.DeepEqual(mc.cfg.Xray.Servers, []string{"203.0.113.10"}) {
		t.Fatalf("xray.servers %v", mc.cfg.Xray.Servers)
	}
}

func TestHandleAddSubscription_Refusals(t *testing.T) {
	deps, mc := subsDeps(t, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"})
	deps.ImportClient = subscriptionHost(t, osloSubscription)
	for body, want := range map[string]int{
		`{"url":""}`:                           http.StatusBadRequest,
		`{"url":"http://93.184.216.34/s"}`:     http.StatusBadRequest,
		`{"url":"https://10.0.0.1/s"}`:         http.StatusBadRequest,
		`{"url":"https://93.184.216.34/s","name":"ALPHA"}`: http.StatusBadRequest,
		`{"url":"https://93.184.216.34/s","name":"\tx"}`:   http.StatusBadRequest,
		`not json`:                             http.StatusBadRequest,
	} {
		if code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", body); code != want {
			t.Errorf("%s: %d %v, want %d", body, code, resp, want)
		}
	}
	if len(mc.subs) != 1 {
		t.Fatalf("a refused add wrote %+v", mc.subs)
	}
}

func TestHandleAddSubscription_ADownloadThatFailedIs502AndNamesNoLink(t *testing.T) {
	deps, _ := subsDeps(t)
	deps.ImportClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/secret-token"}`)

	if code != http.StatusBadGateway || strings.Contains(fmt.Sprint(resp), "secret-token") {
		t.Fatalf("%d %v", code, resp)
	}
}

func TestHandleRefreshSubscriptions(t *testing.T) {
	deps, mc := subsDeps(t,
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
	)
	deps.ImportClient = subscriptionHost(t, osloSubscription)
	h := handleRefreshSubscriptions(deps)

	code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=0a1b2c3d", "")
	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 1 || results[0].(map[string]interface{})["summary"] != "Alpha: Imported 1 servers" {
		t.Fatalf("one: %d %v", code, resp)
	}
	if len(mc.subs[0].Servers) != 1 {
		t.Fatalf("files %+v", mc.subs)
	}

	// All of them: the static list is left out.
	code, resp = call(t, h, "POST", "/api/subscriptions/refresh", "")
	if results, _ := resp["results"].([]interface{}); code != http.StatusOK || len(results) != 1 {
		t.Fatalf("all: %d %v", code, resp)
	}

	if code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=ffffffff", ""); code != http.StatusNotFound {
		t.Fatalf("unknown: %d %v", code, resp)
	}
	if code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=1b2c3d4e", ""); code != http.StatusBadRequest {
		t.Fatalf("static: %d %v", code, resp)
	}
}

// A refresh whose download fails answers 200: the result says why, and the
// subscription keeps its list and records the failure.
func TestHandleRefreshSubscriptions_AFailedDownloadIsAResult(t *testing.T) {
	deps, mc := subsDeps(t, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a",
		Servers: []vpnconfig.Server{{Name: "Old", IPs: []string{"192.0.2.1"}}}})
	deps.ImportClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	code, resp := call(t, handleRefreshSubscriptions(deps), "POST", "/api/subscriptions/refresh?id=0a1b2c3d", "")

	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 1 || results[0].(map[string]interface{})["error"] != "download failed: connection refused" {
		t.Fatalf("%d %v", code, resp)
	}
	if mc.subs[0].Error != "download failed: connection refused" || mc.subs[0].Servers[0].Name != "Old" {
		t.Fatalf("file %+v", mc.subs[0])
	}
}

func TestHandleRenameSubscription(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	h := handleRenameSubscription(deps)

	if code, resp := call(t, h, "POST", "/api/subscriptions/rename?id=0a1b2c3d", `{"name":"Main"}`); code != http.StatusOK || mc.subs[0].Name != "Main" {
		t.Fatalf("%d %v", code, resp)
	}
	for _, tc := range []struct {
		target, body string
		want         int
	}{
		{"/api/subscriptions/rename?id=0a1b2c3d", `{"name":"beta"}`, http.StatusBadRequest},
		{"/api/subscriptions/rename?id=0a1b2c3d", `{"name":""}`, http.StatusBadRequest},
		{"/api/subscriptions/rename", `{"name":"X"}`, http.StatusBadRequest},
		{"/api/subscriptions/rename?id=ffffffff", `{"name":"X"}`, http.StatusNotFound},
	} {
		if code, resp := call(t, h, "POST", tc.target, tc.body); code != tc.want {
			t.Errorf("%s %s: %d %v, want %d", tc.target, tc.body, code, resp, tc.want)
		}
	}
}

func TestHandleDeleteSubscription_SaysTheRunningServerCameFromIt(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	mc.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "a.example.com", Port: 443}
	h := handleDeleteSubscription(deps)

	code, resp := call(t, h, "DELETE", "/api/subscriptions?id=0a1b2c3d", "")

	if code != http.StatusOK || resp["active_removed"] != true || len(mc.subs) != 1 {
		t.Fatalf("%d %v, files %d", code, resp, len(mc.subs))
	}
	if !reflect.DeepEqual(mc.cfg.Xray.Servers, []string{"198.51.100.20"}) {
		t.Fatalf("xray.servers %v", mc.cfg.Xray.Servers)
	}
	if code, resp := call(t, h, "DELETE", "/api/subscriptions?id=0a1b2c3d", ""); code != http.StatusNotFound {
		t.Fatalf("again: %d %v", code, resp)
	}
}

func TestSubscriptionRoutesAreRegistered(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	h := NewRouter(deps, nil)
	token := newTestToken(t, deps)
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve("GET", "/api/subscriptions", ""); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := serve("POST", "/api/subscriptions/rename?id=0a1b2c3d", `{"name":"Main"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := serve("DELETE", "/api/subscriptions?id=1b2c3d4e", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	// The import route of the single subscription is gone.
	if rec := serve("POST", "/api/servers/import", `{"url":""}`); rec.Code != http.StatusNotFound {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
}
```

- [ ] **Step 3: Rewrite the servers tests for the grouped list and the selection**

In `handler_servers_test.go`:
- Delete every `TestHandleImportServers_*`, `TestDownloadErrMessage`, `TestDownloadErrMessage_DropsURL`, `TestResolveSubscriptionURL`, `TestHandleListServers_SubscriptionSaved` and the `postImport` helper. Keep the `TestSyncXrayServers_*` tests: they test `service.PublishServers`, which Task 12 removes together with them.
- In every remaining `TestHandleListServers_*` and `TestHandleSelectServer_*` test, replace `servers: []vpnconfig.Server{...}` in the `mockConfig` literal with `subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{...}}}` (the same servers), and in `TestHandleListServers_Error` replace `err:` with `subsErr:`.
- The list tests decode `Subscriptions []struct{ Servers []... }` and read `resp.Subscriptions[0].Servers` where they read `resp.Servers`.
- Every `POST /api/servers/active` body gains `"subscription":"0a1b2c3d",`.
- Add `reflect` to the file's imports, and drop the ones only the deleted tests used (`ssrf`, and `encoding/base64` and `net/url` if Step 1 took their last users away); `go vet` names any left over.

Then add:

```go
// Two subscriptions name a server Germany-1. The one the page showed in Beta
// is the one that runs, and the record names Beta.
func TestHandleSelectServer_TheSameNameInAnotherSubscription(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)

	code, resp := call(t, handleSelectServer(deps), "POST", "/api/servers/active",
		`{"subscription":"1b2c3d4e","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`)

	if code != http.StatusOK {
		t.Fatalf("%d %v", code, resp)
	}
	a := mc.cfg.Xray.ActiveServer
	if a == nil || a.Subscription != "1b2c3d4e" || a.Address != "198.51.100.20" {
		t.Fatalf("active %+v", a)
	}
	// xray.servers covers every subscription.
	if want := []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"}; !reflect.DeepEqual(mc.cfg.Xray.Servers, want) {
		t.Fatalf("xray.servers %v, want %v", mc.cfg.Xray.Servers, want)
	}
}

// A subscription deleted, or an index that names another server in its list,
// is a list that changed since the page was drawn.
func TestHandleSelectServer_AChangedListIsAConflict(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	for _, body := range []string{
		`{"subscription":"ffffffff","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`,
		`{"subscription":"0a1b2c3d","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`,
	} {
		if code, resp := call(t, handleSelectServer(deps), "POST", "/api/servers/active", body); code != http.StatusConflict {
			t.Errorf("%s: %d %v", body, code, resp)
		}
	}
}

func TestHandleListServers_GroupsBySubscriptionAndNamesTheActive(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	mc.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "198.51.100.20", Port: 8443}
	rec := httptest.NewRecorder()

	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))

	var resp struct {
		Subscriptions []struct {
			ID      string       `json:"id"`
			Name    string       `json:"name"`
			Servers []serverView `json:"servers"`
		} `json:"subscriptions"`
		Active *vpnconfig.ActiveServer `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	g := resp.Subscriptions
	if len(g) != 2 || g[0].Name != "Alpha" || len(g[0].Servers) != 2 || g[1].ID != "1b2c3d4e" || g[1].Servers[0].Name != "Germany-1" {
		t.Fatalf("%+v", g)
	}
	if resp.Active == nil || resp.Active.Subscription != "1b2c3d4e" {
		t.Fatalf("active %+v", resp.Active)
	}
	if strings.Contains(rec.Body.String(), "uuid-") || strings.Contains(rec.Body.String(), "/s/t") {
		t.Fatalf("a credential or the link reached the page: %s", rec.Body)
	}
}
```

- [ ] **Step 4: Run the tests to see them fail**

Run: `cd server && go test ./internal/webapi/`
Expected: FAIL — `undefined: handleListSubscriptions`, and the list and select tests fail on the old handlers.

- [ ] **Step 5: Write the subscription routes**

In `server/internal/webapi/deadline.go`, below `importDeadline`, add:

```go
	// subscriptionTimeout bounds the downloads of one subscription route and
	// the resolution of every host in them. It stays inside importDeadline, so
	// the route can still answer when it runs out.
	subscriptionTimeout = 90 * time.Second
```

Create `server/internal/webapi/handler_subscriptions.go`:

```go
package webapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// subscriptionView is a subscription as the page shows it: the host stands in
// for the link, whose path carries the subscription token.
type subscriptionView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Static    bool      `json:"static"`
	Servers   int       `json:"servers"`
	Added     time.Time `json:"added"`
	Refreshed time.Time `json:"refreshed"`
	Error     string    `json:"error,omitempty"`
}

func newSubscriptionView(s vpnconfig.Subscription) subscriptionView {
	return subscriptionView{ID: s.ID, Name: s.Name, Host: s.Host(), Static: s.Static(), Servers: len(s.Servers),
		Added: s.Added, Refreshed: s.Refreshed, Error: s.Error}
}

// importClient is the client subscription downloads go through: the
// SSRF-hardened one, unless a test put its own in Deps.
func importClient(deps *Deps) *http.Client {
	if deps.ImportClient != nil {
		return deps.ImportClient
	}
	return ssrf.NewClient(10 * time.Second)
}

// resultView is one add or refresh as the page reads it: the line it shows
// and, for a success, the counts that line is made of.
func resultView(r service.SubscriptionResult) map[string]interface{} {
	v := map[string]interface{}{"id": r.ID, "name": r.Name, "existed": r.Existed, "summary": r.Line()}
	if r.Err != nil {
		v["error"] = r.Err.Error()
		return v
	}
	v["count"] = len(r.Import.Servers)
	v["total"] = r.Import.Total
	v["skipped"] = r.Import.SkippedByReason()
	v["dns_errors"] = r.Import.ResolveErrors
	return v
}

// subscriptionErrorStatus is the status a failed subscription route answers
// with: 502 for a subscription that did not arrive, 400 for a request or a
// body the rules refuse, 404 for a subscription that is not there.
func subscriptionErrorStatus(err error) int {
	var de *service.DownloadError
	var be *service.BodyError
	switch {
	case errors.As(err, &de):
		return http.StatusBadGateway
	case errors.As(err, &be),
		errors.Is(err, service.ErrSubscriptionURL),
		errors.Is(err, vpnconfig.ErrSubscriptionName),
		errors.Is(err, vpnconfig.ErrSubscriptionNameTaken),
		errors.Is(err, vpnconfig.ErrSubscriptionLimit),
		errors.Is(err, vpnconfig.ErrSubscriptionStatic):
		return http.StatusBadRequest
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// subscriptionErrorText is what the page shows for err. A failure after the
// file was written says the subscription is saved: the user must not redo an
// add that happened.
func subscriptionErrorText(err error) string {
	switch {
	case errors.Is(err, vpnconfig.ErrServersSaved):
		return "subscription saved, but xray.servers sync failed: " + err.Error()
	case errors.Is(err, service.ErrConfigLockTimeout):
		return "config is busy; nothing was saved"
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		return "no such subscription"
	}
	return err.Error()
}

// subscriptionID is the id an action names: ?id=, which must be there.
func subscriptionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonError(w, http.StatusBadRequest, "id is required")
		return "", false
	}
	return id, true
}

// handleListSubscriptions answers every subscription, in order, without links.
func handleListSubscriptions(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load subscriptions")
			return
		}
		views := make([]subscriptionView, 0, len(subs))
		for _, s := range subs {
			views = append(views, newSubscriptionView(s))
		}
		jsonOK(w, map[string]interface{}{"subscriptions": views})
	}
}

type addSubscriptionRequest struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

// handleAddSubscription downloads a link and saves it as a subscription; a
// link already saved is refreshed instead (service.AddSubscription).
func handleAddSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req addSubscriptionRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.URL = strings.TrimSpace(req.URL)
		if req.URL == "" {
			jsonError(w, http.StatusBadRequest, "url is required")
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		ctx, cancel := context.WithTimeout(context.Background(), subscriptionTimeout)
		defer cancel()
		res := service.AddSubscription(ctx, deps.Config, importClient(deps), req.URL, req.Name)
		if res.Err != nil {
			jsonError(w, subscriptionErrorStatus(res.Err), subscriptionErrorText(res.Err))
			return
		}
		v := resultView(res)
		v["ok"] = true
		jsonOK(w, v)
	}
}

// handleRefreshSubscriptions refreshes the subscription ?id= names, or every
// subscription with a link. A download that fails is a result, not an error:
// the answer is 200 and the result says why.
func handleRefreshSubscriptions(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		ctx, cancel := context.WithTimeout(context.Background(), subscriptionTimeout)
		defer cancel()
		var results []service.SubscriptionResult
		if id == "" {
			all, err := service.RefreshAllSubscriptions(ctx, deps.Config, importClient(deps))
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to load subscriptions")
				return
			}
			results = all
		} else {
			res := service.RefreshSubscription(ctx, deps.Config, importClient(deps), id)
			if errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) || errors.Is(res.Err, vpnconfig.ErrSubscriptionStatic) {
				jsonError(w, subscriptionErrorStatus(res.Err), subscriptionErrorText(res.Err))
				return
			}
			results = []service.SubscriptionResult{res}
		}
		views := make([]map[string]interface{}, 0, len(results))
		for _, res := range results {
			views = append(views, resultView(res))
		}
		jsonOK(w, map[string]interface{}{"results": views})
	}
}

type renameSubscriptionRequest struct {
	Name string `json:"name"`
}

// handleRenameSubscription gives the subscription ?id= names a new name.
func handleRenameSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := subscriptionID(w, r)
		if !ok {
			return
		}
		var req renameSubscriptionRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		if err := service.RenameSubscription(deps.Config, id, req.Name); err != nil {
			jsonError(w, subscriptionErrorStatus(err), subscriptionErrorText(err))
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// handleDeleteSubscription removes the subscription ?id= names. The running
// Xray is left alone; active_removed says it came from that subscription.
func handleDeleteSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := subscriptionID(w, r)
		if !ok {
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		active, err := service.DeleteSubscription(deps.Config, id)
		if err != nil {
			jsonError(w, subscriptionErrorStatus(err), subscriptionErrorText(err))
			return
		}
		jsonOK(w, map[string]bool{"ok": true, "active_removed": active})
	}
}
```

- [ ] **Step 6: Group the server list and select by subscription**

In `server/internal/webapi/handler_servers.go`:

Delete `importServersRequest`, `handleImportServers`, `downloadErrMessage` and `resolveSubscriptionURL`, and drop the imports only they used (`io`, `net/url`, `time`, `ssrf`, `subscription`).

Add after `serverView`:

```go
// subscriptionServers is one subscription's servers as the Servers tab shows them.
type subscriptionServers struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Servers []serverView `json:"servers"`
}
```

Replace `handleListServers`:

```go
// handleListServers answers the servers of every subscription, grouped, and
// the record of which one runs.
func handleListServers(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		// The list on its own cannot say which entry is running: a
		// subscription routinely puts many names behind one address:port, and
		// two subscriptions can use one name. The answer is the record a
		// selection leaves in vpn-director.json, and nil travels as null.
		cfg, err := deps.Config.LoadVPNConfig()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load vpn config")
			return
		}
		var active *vpnconfig.ActiveServer
		if cfg != nil {
			active = cfg.Xray.ActiveServer
		}
		groups := make([]subscriptionServers, 0, len(subs))
		for _, sub := range subs {
			views := make([]serverView, 0, len(sub.Servers))
			for _, s := range sub.Servers {
				ips := s.IPs
				if ips == nil {
					ips = []string{}
				}
				views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips, Protocol: s.Label()})
			}
			groups = append(groups, subscriptionServers{ID: sub.ID, Name: sub.Name, Servers: views})
		}
		jsonOK(w, map[string]interface{}{"subscriptions": groups, "active": active})
	}
}
```

In `selectServerRequest`, add before `Index`:

```go
	// Subscription is the id of the subscription the page showed the server
	// in; Index counts within that subscription's list.
	Subscription string `json:"subscription"`
```

In `handleSelectServer`, replace the block from `servers, err := deps.Config.LoadServers()` through the identity check with:

```go
		// Load the list after the lock so a concurrent import cannot change
		// which server this index names between the bounds check and the write.
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		si := vpnconfig.FindSubscription(subs, req.Subscription)
		if si < 0 {
			jsonError(w, http.StatusConflict, "server list changed")
			return
		}
		servers := subs[si].Servers
		if *req.Index < 0 || *req.Index >= len(servers) {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("index out of range: %d (have %d servers)", *req.Index, len(servers)))
			return
		}

		server := servers[*req.Index]
		server.Subscription = subs[si].ID
		if server.Name != req.Name || server.Address != req.Address || server.Port != req.Port {
			jsonError(w, http.StatusConflict, "server list changed")
			return
		}
```

and in the `UpdateVPNConfig` closure below it replace `cfg.Xray.Servers = vpnconfig.ServerIPs(servers)` with `cfg.Xray.Servers = vpnconfig.SubscriptionIPs(subs)`.

In `server/internal/webapi/router.go`, replace the servers block of `registerProtectedRoutes` with:

```go
	// Servers and subscriptions
	mux.HandleFunc("GET /api/servers", handleListServers(deps))
	mux.HandleFunc("POST /api/servers/active", handleSelectServer(deps))
	mux.HandleFunc("GET /api/subscriptions", handleListSubscriptions(deps))
	mux.HandleFunc("POST /api/subscriptions", handleAddSubscription(deps))
	mux.HandleFunc("POST /api/subscriptions/refresh", handleRefreshSubscriptions(deps))
	mux.HandleFunc("POST /api/subscriptions/rename", handleRenameSubscription(deps))
	mux.HandleFunc("DELETE /api/subscriptions", handleDeleteSubscription(deps))
```

- [ ] **Step 7: Run the tests to see them pass**

Run: `cd server && go vet ./internal/webapi/ && go test ./internal/webapi/ && gofmt -l internal/webapi`
Expected: PASS, nothing from `go vet` or `gofmt -l`.

- [ ] **Step 8: Commit**

```bash
git add server/internal/webapi
git commit -m "feat(webui): subscription routes, and servers grouped and selected by subscription"
```

---

### Task 6: Web UI pages — subscriptions and grouped servers

**Files:**
- Modify: `web/src/types.ts`
- Modify: `web/src/api.ts`
- Modify: `web/src/components/ServersTab.vue` (rewritten)
- Modify: `web/src/components/StatusTab.vue`

**Interfaces:**
- Consumes: Task 5's API.
- Produces (TypeScript): `ActiveServer.subscription?`, `SubscriptionServers`, `ServersResponse { subscriptions, active }`, `Subscription`, `SubscriptionsResponse`, `SubscriptionResult`, `RefreshResponse`, `DeleteSubscriptionResponse`; `api.getSubscriptions`, `api.addSubscription`, `api.refreshSubscription`, `api.renameSubscription`, `api.deleteSubscription`, `api.selectServer(subscription, index, server)`.

The SPA has no unit tests; `npm run build` runs `vue-tsc` and is the check, and Task 17 looks at the pages in a browser.

- [ ] **Step 1: Types**

In `web/src/types.ts`, give `ActiveServer` a subscription:

```ts
/** The server the running Xray config was generated from. Null until something
 *  selects one: config.json holds only the outbound, and a subscription puts
 *  many names behind one address:port, so it cannot be read back into a name. */
export interface ActiveServer {
  name: string
  address: string
  port: number
  /** The id of the server's subscription. A record from before subscriptions
   *  has none and matches no server. */
  subscription?: string
}
```

Replace `ServersResponse` and `ImportResponse` with:

```ts
/** One subscription's servers, as GET /api/servers groups them. */
export interface SubscriptionServers {
  id: string
  name: string
  servers: Server[] | null
}

/** Go marshals a nil slice as null: the `?? []` guards at the call sites are
 *  load-bearing, and the nullable types keep them that way. */
export interface ServersResponse {
  subscriptions: SubscriptionServers[] | null
  active: ActiveServer | null
}

/** A subscription as GET /api/subscriptions shows it: the host stands in for
 *  the link, whose path carries the token. A static list has no link. */
export interface Subscription {
  id: string
  name: string
  host: string
  static: boolean
  servers: number
  added: string
  refreshed: string
  error?: string
}

export interface SubscriptionsResponse {
  subscriptions: Subscription[] | null
}

/** One add or refresh. summary is the line to show, e.g.
 *  "Alpha: Imported 32 of 40 servers: 7 composite, 1 DNS error". */
export interface SubscriptionResult {
  ok?: boolean
  id: string
  name: string
  existed: boolean
  summary: string
  error?: string
  count?: number
  total?: number
  skipped?: Record<'unsupported' | 'composite' | 'invalid' | 'placeholder', number>
  dns_errors?: number
}

export interface RefreshResponse {
  results: SubscriptionResult[] | null
}

export interface DeleteSubscriptionResponse {
  ok: boolean
  /** The running server came from the deleted subscription. */
  active_removed: boolean
}
```

- [ ] **Step 2: API client**

In `web/src/api.ts`, drop `ImportResponse` from the type imports and add `DeleteSubscriptionResponse`, `RefreshResponse`, `SubscriptionResult`, `SubscriptionsResponse`. Replace the Servers block with:

```ts
  // Servers and subscriptions
  getServers: () =>
    api.get<ServersResponse>('/api/servers'),
  // The server as the page shows it at that index of that subscription: the
  // list can change before the click, and the router answers 409 when the
  // index names another server or the subscription is gone.
  selectServer: (subscription: string, index: number, server: Server) =>
    api.post<OkResponse>('/api/servers/active', {
      subscription,
      index,
      name: server.name,
      address: server.address,
      port: server.port,
    }),
  getSubscriptions: () =>
    api.get<SubscriptionsResponse>('/api/subscriptions'),
  addSubscription: (url: string, name: string) =>
    api.post<SubscriptionResult>('/api/subscriptions', { url, name }),
  // No id refreshes every subscription that has a link.
  refreshSubscription: (id?: string) =>
    api.post<RefreshResponse>('/api/subscriptions/refresh', null, { params: id ? { id } : {} }),
  renameSubscription: (id: string, name: string) =>
    api.post<OkResponse>('/api/subscriptions/rename', { name }, { params: { id } }),
  deleteSubscription: (id: string) =>
    api.delete<DeleteSubscriptionResponse>('/api/subscriptions', { params: { id } }),
```

- [ ] **Step 3: The Servers tab**

Replace `web/src/components/ServersTab.vue` with:

```vue
<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ActiveServer, Server, Subscription, SubscriptionServers } from '../types'

const subscriptions = ref<Subscription[]>([])
const groups = ref<SubscriptionServers[]>([])
const active = ref<ActiveServer | null>(null)
const loading = ref(false)
// What is running: 'add', 'refresh-all', 'refresh:<id>', 'rename:<id>',
// 'delete:<id>', 'select:<id>:<index>'. One thing at a time.
const busy = ref('')
const error = ref('')
const addUrl = ref('')
const addName = ref('')
const renaming = ref('')
const renameText = ref('')
// What the last add or refresh came to, a line per subscription.
const summaries = ref<string[]>([])

function errorText(e: any): string {
  return e?.response?.data?.error || e?.message || 'unknown error'
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [subsRes, serversRes] = await Promise.all([api.getSubscriptions(), api.getServers()])
    subscriptions.value = subsRes.data.subscriptions ?? []
    groups.value = serversRes.data.subscriptions ?? []
    active.value = serversRes.data.active ?? null
  } catch (e: any) {
    error.value = errorText(e)
  } finally {
    loading.value = false
  }
}

// "2 h ago" for a time the router wrote in UTC.
function ago(iso: string): string {
  const t = Date.parse(iso)
  if (isNaN(t)) return ''
  const s = Math.max(0, Math.round((Date.now() - t) / 1000))
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return `${Math.floor(s / 86400)} d ago`
}

// The subscription and the name as well as the endpoint: two subscriptions
// can both name a server Germany-1, and eight servers of one can share an
// address:port.
function isActive(sub: string, server: Server): boolean {
  const a = active.value
  return (
    a !== null &&
    a.subscription === sub &&
    a.name === server.name &&
    a.address === server.address &&
    a.port === server.port
  )
}

function runsFrom(sub: string): boolean {
  return active.value?.subscription === sub
}

async function run(what: string, fn: () => Promise<void>) {
  busy.value = what
  try {
    await fn()
  } catch (e: any) {
    alert('Error: ' + errorText(e))
  } finally {
    busy.value = ''
  }
}

function addSubscription() {
  if (!addUrl.value.trim()) return
  return run('add', async () => {
    summaries.value = []
    const resp = await api.addSubscription(addUrl.value.trim(), addName.value.trim())
    summaries.value = [resp.data.summary]
    addUrl.value = ''
    addName.value = ''
    await load()
  })
}

function refresh(id?: string) {
  return run(id ? 'refresh:' + id : 'refresh-all', async () => {
    summaries.value = []
    const resp = await api.refreshSubscription(id)
    summaries.value = (resp.data.results ?? []).map((r) => r.summary)
    await load()
  })
}

function startRename(sub: Subscription) {
  renaming.value = sub.id
  renameText.value = sub.name
}

function saveRename(sub: Subscription) {
  return run('rename:' + sub.id, async () => {
    await api.renameSubscription(sub.id, renameText.value.trim())
    renaming.value = ''
    await load()
  })
}

function remove(sub: Subscription) {
  const warn = runsFrom(sub.id)
    ? '\n\nThe running Xray server comes from it. Xray keeps running it until you select another.'
    : ''
  if (!confirm(`Delete the subscription ${sub.name} and its ${sub.servers} servers?${warn}`)) return
  return run('delete:' + sub.id, async () => {
    const resp = await api.deleteSubscription(sub.id)
    if (resp.data.active_removed) {
      alert('The running server is no longer in any subscription. Select another server.')
    }
    await load()
  })
}

async function selectServer(group: SubscriptionServers, index: number) {
  const server = group.servers?.[index]
  if (!server) return
  busy.value = `select:${group.id}:${index}`
  try {
    await api.selectServer(group.id, index, server)
    alert(`Server selected: ${group.name} / ${server.name}`)
    await load()
  } catch (e: any) {
    if (e.response?.status === 409) {
      // A refresh on the router - the bot's subscription watch, an import in
      // another tab - moved the list since it was shown.
      alert('The server list changed; it has been reloaded. Select the server again.')
      await load()
    } else {
      alert('Error: ' + errorText(e))
    }
  } finally {
    busy.value = ''
  }
}

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="card-title">Subscriptions</div>

    <table v-if="subscriptions.length > 0">
      <thead>
        <tr>
          <th>Name</th>
          <th>Host</th>
          <th>Servers</th>
          <th>Refreshed</th>
          <th>Status</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="sub in subscriptions" :key="sub.id">
          <td>
            <template v-if="renaming === sub.id">
              <input
                v-model="renameText"
                type="text"
                maxlength="32"
                style="width: 10rem;"
                @keyup.enter="saveRename(sub)"
                @keyup.esc="renaming = ''"
              />
              <button class="btn btn-green" :disabled="!!busy" @click="saveRename(sub)">✓</button>
              <button class="btn btn-blue" @click="renaming = ''">✕</button>
            </template>
            <template v-else>{{ sub.name }}</template>
          </td>
          <td>{{ sub.static ? 'static list' : sub.host }}</td>
          <td>{{ sub.servers }}</td>
          <td>{{ ago(sub.refreshed) }}</td>
          <td>
            <span v-if="sub.error" class="badge badge-red" :title="sub.error">{{ sub.error }}</span>
            <span v-else class="badge badge-green">OK</span>
          </td>
          <td style="white-space: nowrap;">
            <button
              class="btn btn-blue"
              :disabled="!!busy || sub.static"
              :title="sub.static ? 'A static list has no link to refresh' : 'Refresh'"
              @click="refresh(sub.id)"
            >
              {{ busy === 'refresh:' + sub.id ? '...' : '⟳' }}
            </button>
            <button class="btn btn-blue" :disabled="!!busy" title="Rename" @click="startRename(sub)">✎</button>
            <button class="btn btn-red" :disabled="!!busy" title="Delete" @click="remove(sub)">
              {{ busy === 'delete:' + sub.id ? '...' : '🗑' }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else-if="!loading" style="color: #999; font-size: 0.875rem;">
      No subscriptions yet. Add one below.
    </p>

    <div class="actions" style="display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap; margin-top: 0.75rem;">
      <input v-model="addUrl" type="text" placeholder="https://... subscription URL" style="flex: 2; min-width: 200px;" />
      <input v-model="addName" type="text" maxlength="32" placeholder="Name (optional)" style="flex: 1; min-width: 120px;" />
      <button class="btn btn-primary" :disabled="!!busy || !addUrl.trim()" @click="addSubscription">
        {{ busy === 'add' ? '...' : '+ Add' }}
      </button>
      <button
        class="btn btn-blue"
        :disabled="!!busy || subscriptions.every((s) => s.static)"
        @click="refresh()"
      >
        {{ busy === 'refresh-all' ? '...' : '⟳ Refresh all' }}
      </button>
      <button class="btn btn-blue" :disabled="loading" @click="load">
        {{ loading ? '...' : '↻ Reload' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>
    <p v-for="(line, i) in summaries" :key="i" style="font-size: 0.875rem; margin: 0.25rem 0;">{{ line }}</p>
  </div>

  <div class="card">
    <div class="card-title">Servers</div>
    <details
      v-for="group in groups"
      :key="group.id"
      :open="runsFrom(group.id) || groups.length === 1"
      style="margin-bottom: 0.75rem;"
    >
      <summary style="cursor: pointer; font-weight: 600;">
        {{ group.name }} — {{ (group.servers ?? []).length }} servers
        <span v-if="runsFrom(group.id)" class="badge badge-green" style="margin-left: 0.5rem;">running</span>
      </summary>
      <table>
        <thead>
          <tr>
            <th>#</th>
            <th>Name</th>
            <th>Address</th>
            <th>Port</th>
            <th>Protocol</th>
            <th>Action</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(server, idx) in group.servers ?? []" :key="idx">
            <td>{{ idx + 1 }}</td>
            <td>
              {{ server.name }}
              <span v-if="isActive(group.id, server)" class="badge badge-green" style="margin-left: 0.5rem;">Active</span>
            </td>
            <td>{{ server.address }}</td>
            <td>{{ server.port }}</td>
            <td>{{ server.protocol }}</td>
            <td>
              <button class="btn btn-green" :disabled="!!busy" @click="selectServer(group, idx)">
                {{ busy === `select:${group.id}:${idx}` ? '...' : 'Select' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </details>
    <p v-if="groups.length === 0 && !loading" style="color: #999; font-size: 0.875rem;">
      No servers found. Add a subscription to get started.
    </p>
  </div>
</template>
```

- [ ] **Step 4: The running server's label on the Status tab**

In `web/src/components/StatusTab.vue`, import `ServersResponse` beside `ActiveServer`, add `const activeLabel = ref('')` next to `activeServer`, and add above `loadStatus`:

```ts
// "Beta / Germany-1": the running server under its subscription's name, or
// its own name when no subscription holds it any more - deleted since, or a
// record from before subscriptions.
function serverLabel(data: ServersResponse): string {
  const a = data.active
  if (!a) return ''
  const name = a.name || a.address
  const group = (data.subscriptions ?? []).find((g) => g.id === a.subscription)
  return group ? `${group.name} / ${name}` : `${name} — not in any subscription`
}
```

In `loadStatus`, the fulfilled branch of `serversRes` becomes:

```ts
  if (serversRes.status === 'fulfilled') {
    activeServer.value = serversRes.value.data.active ?? null
    activeLabel.value = serverLabel(serversRes.value.data)
    serverLoaded.value = true
  } else {
```

and in the template the card's first line shows `{{ activeLabel }}` in place of `{{ activeServer.name || activeServer.address }}`.

- [ ] **Step 5: Build**

Run: `cd web && npm run build`
Expected: `vue-tsc -b` reports no error and Vite writes `web/dist`.

- [ ] **Step 6: Commit**

```bash
git add web/src/types.ts web/src/api.ts web/src/components/ServersTab.vue web/src/components/StatusTab.vue
git commit -m "feat(webui): the Subscriptions card and servers grouped by subscription"
```

---

### Task 7: Bot — `/import` for subscriptions and the new `/subs`

**Files:**
- Create: `server/internal/handler/substore_test.go` (the handler tests' subscription store, sender and HTTP client)
- Create: `server/internal/handler/subs.go`, `server/internal/handler/subs_test.go`
- Modify: `server/internal/handler/import.go` (rewritten), `server/internal/handler/import_test.go` (rewritten)
- Modify: `server/internal/handler/misc.go` (`/start` help)
- Modify: `server/internal/bot/router.go`, `server/internal/bot/router_test.go`, `server/internal/bot/bot.go`

**Interfaces:**
- Consumes (Task 4): `service.AddSubscription`, `RefreshSubscription`, `RefreshAllSubscriptions`, `RenameSubscription`, `DeleteSubscription`, `SubscriptionResult`; (Task 2) `vpnconfig.ErrSubscriptionGone`, `ErrServersSaved`.
- Produces:
  - `handler.NewSubsHandler(deps *Deps) *SubsHandler` with `HandleSubs(*tgbotapi.Message)`, `HandleCancel(*tgbotapi.Message)`, `HandleCallback(*tgbotapi.CallbackQuery)`, `HandleTextInput(*tgbotapi.Message) bool`, `ClearState(chatID int64)`; fields `httpClient *http.Client`, `now func() time.Time` (tests set both).
  - Callback data: `subs:r:<id>` refresh, `subs:n:<id>` rename, `subs:d:<id>` delete (asks), `subs:dy:<id>` delete confirmed, `subs:dn` keep.
  - `bot.SubsRouterHandler`; `bot.NewRouter(status, servers, import_, misc, update, wizard, xray, exclude, clients, subs)`.
  - `ImportHandler.httpClient` stays the test hook.
  - Test helpers for Tasks 8 and 9's handler tests: `newSubsStore(subs ...vpnconfig.Subscription) *subsStore`, `recordingSender` (`last()`, `all()`, `buttons()`, `edits`), `servingClient(t, http.HandlerFunc) *http.Client`, `serveBody(string) http.HandlerFunc`, `roundTripFunc`, `osloSubscription`.

- [ ] **Step 1: The handler tests' helpers**

Create `server/internal/handler/substore_test.go`:

```go
package handler

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// subsStore is a ConfigStore over memory for the handlers that read and write
// subscriptions. UpdateVPNConfig serializes on upd as the flock does, and mu
// guards the data: /import refreshes every subscription at once.
type subsStore struct {
	upd  sync.Mutex
	mu   sync.Mutex
	cfg  *vpnconfig.VPNDirectorConfig
	subs []vpnconfig.Subscription
}

func newSubsStore(subs ...vpnconfig.Subscription) *subsStore {
	return &subsStore{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
}

func (m *subsStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg, nil
}
func (m *subsStore) LoadServers() ([]vpnconfig.Server, error) {
	subs, _ := m.LoadSubscriptions()
	return vpnconfig.AllServers(subs), nil
}
func (m *subsStore) SaveServers([]vpnconfig.Server) error { return nil }
func (m *subsStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	return fn(m.cfg)
}
func (m *subsStore) DataDir() (string, error) { return "/data", nil }
func (m *subsStore) DataDirOrDefault() string { return "/data" }
func (m *subsStore) ScriptsDir() string       { return "/scripts" }
func (m *subsStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vpnconfig.Subscription, len(m.subs))
	for i, s := range m.subs {
		servers := make([]vpnconfig.Server, len(s.Servers))
		for j, srv := range s.Servers {
			srv.Subscription = s.ID
			servers[j] = srv
		}
		s.Servers = servers
		out[i] = s
	}
	return out, nil
}
func (m *subsStore) SaveSubscription(sub vpnconfig.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == sub.ID {
			m.subs[i] = sub
			return nil
		}
	}
	m.subs = append(m.subs, sub)
	return nil
}
func (m *subsStore) DeleteSubscription(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == id {
			m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
			return nil
		}
	}
	return nil
}

// recordingSender keeps every text it is given, sent or edited, and the last keyboard.
type recordingSender struct {
	mu       sync.Mutex
	texts    []string
	keyboard tgbotapi.InlineKeyboardMarkup
	edits    int
}

func (s *recordingSender) add(text string, kb *tgbotapi.InlineKeyboardMarkup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	if kb != nil {
		s.keyboard = *kb
	}
}
func (s *recordingSender) Send(_ int64, text string) error      { s.add(text, nil); return nil }
func (s *recordingSender) SendPlain(_ int64, text string) error { s.add(text, nil); return nil }
func (s *recordingSender) SendLongPlain(_ int64, text string) error {
	s.add(text, nil)
	return nil
}
func (s *recordingSender) SendWithKeyboard(_ int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	s.add(text, &kb)
	return nil
}
func (s *recordingSender) SendCodeBlock(_ int64, header, content string) error {
	s.add(header+"\n"+content, nil)
	return nil
}
func (s *recordingSender) EditMessage(_ int64, _ int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	s.add(text, &kb)
	s.mu.Lock()
	s.edits++
	s.mu.Unlock()
	return nil
}
func (s *recordingSender) AckCallback(string) error { return nil }

func (s *recordingSender) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

func (s *recordingSender) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.texts, "\n")
}

// buttons is the callback data of every button of the last keyboard, in order.
func (s *recordingSender) buttons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, row := range s.keyboard.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != nil {
				out = append(out, *b.CallbackData)
			}
		}
	}
	return out
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// servingClient answers every request with h, whatever host the URL names: the
// checks in front of a download see the public address a test uses.
func servingClient(t *testing.T, h http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})}
}

func serveBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

var osloSubscription = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))
```

- [ ] **Step 2: Write the failing `/import` tests**

Replace `server/internal/handler/import_test.go` with:

```go
package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func importCommand(text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 123},
		Text:     text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 7}},
	}
}

func importHandler(t *testing.T, store *subsStore, h http.HandlerFunc) (*ImportHandler, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	imp := NewImportHandler(&Deps{Sender: sender, Config: store})
	imp.httpClient = servingClient(t, h)
	return imp, sender
}

func TestImport_AddsASubscriptionUnderTheNameGiven(t *testing.T) {
	store := newSubsStore()
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import https://93.184.216.34/s/token Alpha VPN"))

	if len(store.subs) != 1 || store.subs[0].Name != "Alpha VPN" || store.subs[0].URL != "https://93.184.216.34/s/token" {
		t.Fatalf("files %+v", store.subs)
	}
	if got := sender.last(); !strings.Contains(got, "Alpha VPN: Imported 1 servers:") || strings.Contains(got, "/s/token") {
		t.Fatalf("reply %q", got)
	}
}

func TestImport_ASavedLinkIsRefreshedAndSaysSo(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/token"})
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import https://93.184.216.34/s/token"))

	if len(store.subs) != 1 || len(store.subs[0].Servers) != 1 || store.subs[0].Name != "Alpha" {
		t.Fatalf("files %+v", store.subs)
	}
	if !strings.Contains(sender.last(), "saved already") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestImport_AloneRefreshesEverySubscription(t *testing.T) {
	store := newSubsStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
		vpnconfig.Subscription{ID: "2c3d4e5f", Name: "Gamma", URL: "https://93.184.216.34/b"},
	)
	h, sender := importHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(osloSubscription))
	})

	h.HandleImport(importCommand("/import"))

	got := sender.last()
	if !strings.Contains(got, "Alpha: Imported 1 servers") || !strings.Contains(got, "Gamma: download failed: HTTP 403") || strings.Contains(got, "Beta") {
		t.Fatalf("reply %q", got)
	}
	if store.subs[2].Error != "download failed: HTTP 403" {
		t.Fatalf("Gamma %+v", store.subs[2])
	}
}

func TestImport_AloneWithoutALinkExplains(t *testing.T) {
	h, sender := importHandler(t, newSubsStore(vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"}), serveBody(""))

	h.HandleImport(importCommand("/import"))

	if !strings.Contains(sender.last(), "Usage") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestImport_RefusesAPlainHTTPLink(t *testing.T) {
	store := newSubsStore()
	h, sender := importHandler(t, store, serveBody(osloSubscription))

	h.HandleImport(importCommand("/import http://93.184.216.34/s/token"))

	if len(store.subs) != 0 || !strings.Contains(sender.last(), "nothing was imported") {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
}

// A *url.Error's text is the whole link, and the token must not reach the chat.
func TestImport_AFailedDownloadNamesNoLink(t *testing.T) {
	store := newSubsStore()
	sender := &recordingSender{}
	h := NewImportHandler(&Deps{Sender: sender, Config: store})
	h.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	h.HandleImport(importCommand("/import https://93.184.216.34/s/secret-token"))

	if strings.Contains(sender.all(), "secret-token") || !strings.Contains(sender.last(), "download failed: connection refused") {
		t.Fatalf("messages %q", sender.all())
	}
}
```

- [ ] **Step 3: Write the failing `/subs` tests**

Create `server/internal/handler/subs_test.go`:

```go
package handler

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var subsNow = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

func subsHandler(store *subsStore) (*SubsHandler, *recordingSender) {
	sender := &recordingSender{}
	h := NewSubsHandler(&Deps{Sender: sender, Config: store})
	h.now = func() time.Time { return subsNow }
	return h, sender
}

func subsCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: "cb", Data: data, Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}
}

func chatText(s string) *tgbotapi.Message {
	return &tgbotapi.Message{Text: s, Chat: &tgbotapi.Chat{ID: 123}}
}

func TestSubs_ListsEachSubscriptionWithItsButtons(t *testing.T) {
	at := subsNow.Add(-2 * time.Hour)
	store := newSubsStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/token", Refreshed: at, Servers: make([]vpnconfig.Server, 32)},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Refreshed: at, Error: "download failed: HTTP 403", Servers: make([]vpnconfig.Server, 5)},
	)
	h, sender := subsHandler(store)

	h.HandleSubs(chatText("/subs"))

	got := sender.last()
	if !strings.Contains(got, `Alpha — sub\.example\.com — 32 servers — 2 h ago — OK`) || strings.Contains(got, "token") {
		t.Fatalf("list %q", got)
	}
	if !strings.Contains(got, "Beta — static list — 5 servers — 2 h ago — download failed: HTTP 403") {
		t.Fatalf("list %q", got)
	}
	want := []string{"subs:r:0a1b2c3d", "subs:n:0a1b2c3d", "subs:d:0a1b2c3d", "subs:n:1b2c3d4e", "subs:d:1b2c3d4e"}
	if got := sender.buttons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons %v, want %v", got, want)
	}
}

func TestSubs_NoSubscriptionSaysHowToAddOne(t *testing.T) {
	h, sender := subsHandler(newSubsStore())

	h.HandleSubs(chatText("/subs"))

	if !strings.Contains(sender.last(), "/import") || len(sender.buttons()) != 0 {
		t.Fatalf("reply %q, buttons %v", sender.last(), sender.buttons())
	}
}

func TestSubs_RefreshButtonRefreshesThatSubscription(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/token"})
	h, sender := subsHandler(store)
	h.httpClient = servingClient(t, serveBody(osloSubscription))

	h.HandleCallback(subsCallback("subs:r:0a1b2c3d"))

	if len(store.subs[0].Servers) != 1 || !strings.Contains(sender.all(), "Alpha: Imported 1 servers") {
		t.Fatalf("files %+v, messages %q", store.subs, sender.all())
	}
	if sender.edits == 0 {
		t.Fatal("the list was not redrawn")
	}
}

func TestSubs_RenameTakesTheNextMessage(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	if !strings.Contains(sender.last(), "new name for Alpha") {
		t.Fatalf("prompt %q", sender.last())
	}

	// A taken name is an answer the rename refuses: it is taken, and it uses the prompt up.
	if !h.HandleTextInput(chatText("beta")) || store.subs[0].Name != "Alpha" || !strings.Contains(sender.last(), "Not renamed") {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
	if h.HandleTextInput(chatText("Main")) {
		t.Fatal("a text without a prompt was taken")
	}

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	if !h.HandleTextInput(chatText(" Main ")) || store.subs[0].Name != "Main" {
		t.Fatalf("files %+v, reply %q", store.subs, sender.last())
	}
}

func TestSubs_ARenameWaitsFiveMinutesAtMost(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, _ := subsHandler(store)
	now := subsNow
	h.now = func() time.Time { return now }

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	now = now.Add(renameWait + time.Second)

	if h.HandleTextInput(chatText("Main")) || store.subs[0].Name != "Alpha" {
		t.Fatalf("a late answer renamed %+v", store.subs)
	}
}

func TestSubs_ClearStateEndsTheRename(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, _ := subsHandler(store)

	h.HandleCallback(subsCallback("subs:n:0a1b2c3d"))
	h.ClearState(123)

	if h.HandleTextInput(chatText("Main")) {
		t.Fatal("the text was taken after ClearState")
	}
}

func TestSubs_DeleteAsksFirstAndWarnsAboutTheRunningServer(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: make([]vpnconfig.Server, 2)})
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:d:0a1b2c3d"))

	if len(store.subs) != 1 || !strings.Contains(sender.last(), "running Xray server comes from it") {
		t.Fatalf("files %d, question %q", len(store.subs), sender.last())
	}
	if got := sender.buttons(); !reflect.DeepEqual(got, []string{"subs:dy:0a1b2c3d", "subs:dn"}) {
		t.Fatalf("buttons %v", got)
	}

	h.HandleCallback(subsCallback("subs:dy:0a1b2c3d"))

	if len(store.subs) != 0 || !strings.Contains(sender.all(), "no longer in any subscription") {
		t.Fatalf("files %d, messages %q", len(store.subs), sender.all())
	}
}

func TestSubs_NoKeepsTheSubscription(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	h, sender := subsHandler(store)

	h.HandleCallback(subsCallback("subs:d:0a1b2c3d"))
	h.HandleCallback(subsCallback("subs:dn"))

	if len(store.subs) != 1 || !reflect.DeepEqual(sender.buttons(), []string{"subs:n:0a1b2c3d", "subs:d:0a1b2c3d"}) {
		t.Fatalf("files %d, buttons %v", len(store.subs), sender.buttons())
	}
}
```

- [ ] **Step 4: Run the tests to see them fail**

Run: `cd server && go test ./internal/handler/`
Expected: FAIL — `undefined: NewSubsHandler`, and the import tests fail on the old handler.

- [ ] **Step 5: Rewrite `/import`**

Replace `server/internal/handler/import.go` with:

```go
// internal/handler/import.go
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// importTimeout bounds one /import, or one refresh from /subs: the downloads
// and the resolution of every host in them.
const importTimeout = 3 * time.Minute

// ImportHandler handles /import. "/import <url> [name]" adds a subscription -
// or refreshes the one saved with that link - and "/import" alone refreshes
// every subscription that has a link.
type ImportHandler struct {
	deps       *Deps
	httpClient *http.Client
}

// NewImportHandler creates an ImportHandler that downloads through the
// SSRF-hardened client.
func NewImportHandler(deps *Deps) *ImportHandler {
	return &ImportHandler{deps: deps, httpClient: ssrf.NewClient(30 * time.Second)}
}

// HandleImport handles /import.
func (h *ImportHandler) HandleImport(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()

	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		h.refreshAll(ctx, chatID)
		return
	}
	// The name is the rest of the line: it may hold spaces.
	rawURL, name, _ := strings.Cut(args, " ")
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Loading the subscription..."))
	res := service.AddSubscription(ctx, h.deps.Config, h.httpClient, rawURL, strings.TrimSpace(name))
	if res.Err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(importFailure(res.Err)))
		return
	}
	h.deps.Sender.Send(chatID, importReport(res))
}

// refreshAll refreshes every subscription that has a link, a line each.
func (h *ImportHandler) refreshAll(ctx context.Context, chatID int64) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	linked := 0
	for _, s := range subs {
		if !s.Static() {
			linked++
		}
	}
	if linked == 0 {
		h.deps.Sender.Send(chatID, "Usage: `/import <url> [name]` adds a subscription; `/import` alone refreshes them all")
		return
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Refreshing %d subscriptions...", linked)))
	results, err := service.RefreshAllSubscriptions(ctx, h.deps.Config, h.httpClient)
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	lines := make([]string, 0, len(results))
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
}

// importFailure is what /import says when an add failed. Only a failure after
// the file was written says the subscription is saved; every other one saved
// nothing.
func importFailure(err error) string {
	if errors.Is(err, vpnconfig.ErrServersSaved) {
		return fmt.Sprintf("Warning: the subscription is saved, but xray.servers sync failed: %v", err)
	}
	return fmt.Sprintf("Import failed, nothing was imported: %v", err)
}

// importReport is the reply to an add: what came in, by country, and what was
// left out and why.
func importReport(res service.SubscriptionResult) string {
	imp := res.Import
	counts := imp.Counts()
	head := fmt.Sprintf("%s: Imported %d servers:", res.Name, len(imp.Servers))
	if counts != "" {
		head = fmt.Sprintf("%s: Imported %d of %d servers:", res.Name, len(imp.Servers), imp.Total)
	}
	var sb strings.Builder
	sb.WriteString(telegram.EscapeMarkdownV2(head) + "\n")
	sb.WriteString(telegram.EscapeMarkdownV2(groupServersByCountry(imp.Servers)))
	if counts != "" {
		lines := append([]string{counts}, imp.Details(3)...)
		sb.WriteString("\n\n" + telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
	}
	if res.Existed {
		sb.WriteString("\n\n" + telegram.EscapeMarkdownV2("The link was saved already; its list was refreshed."))
	}
	return sb.String()
}
```

- [ ] **Step 6: Write `/subs`**

Create `server/internal/handler/subs.go`:

```go
// internal/handler/subs.go
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// renameWait is how long a rename waits for the new name.
const renameWait = 5 * time.Minute

// SubsHandler handles /subs: a line per subscription with buttons to refresh,
// rename and delete it. A rename takes the chat's next text message.
type SubsHandler struct {
	deps       *Deps
	httpClient *http.Client
	now        func() time.Time

	mu      sync.Mutex
	renames map[int64]pendingRename // chat -> the rename waiting for its name
}

type pendingRename struct {
	id, name string
	until    time.Time
}

// NewSubsHandler creates a SubsHandler that downloads through the
// SSRF-hardened client.
func NewSubsHandler(deps *Deps) *SubsHandler {
	return &SubsHandler{
		deps:       deps,
		httpClient: ssrf.NewClient(30 * time.Second),
		now:        time.Now,
		renames:    map[int64]pendingRename{},
	}
}

// noButtons is a keyboard without a button, the one an edited message ends with.
func noButtons() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
}

// HandleSubs sends the list.
func (h *SubsHandler) HandleSubs(msg *tgbotapi.Message) {
	text, kb, err := h.list()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, text)
		return
	}
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, kb)
}

// list is the /subs message: a line per subscription and a row of buttons each.
func (h *SubsHandler) list() (string, tgbotapi.InlineKeyboardMarkup, error) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if len(subs) == 0 {
		return telegram.EscapeMarkdownV2("No subscriptions. Add one with /import <url> [name]"), tgbotapi.InlineKeyboardMarkup{}, nil
	}
	var sb strings.Builder
	sb.WriteString(telegram.EscapeMarkdownV2("Subscriptions:"))
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		sb.WriteString("\n" + telegram.EscapeMarkdownV2(subLine(s, h.now())))
		if !s.Static() {
			kb.Button("⟳ "+s.Name, "subs:r:"+s.ID)
		}
		kb.Button("✎ "+s.Name, "subs:n:"+s.ID)
		kb.Button("🗑 "+s.Name, "subs:d:"+s.ID)
		kb.Row()
	}
	return sb.String(), kb.Build(), nil
}

// subLine is one subscription on the list:
// "Alpha — sub.example.com — 32 servers — 2 h ago — OK".
func subLine(s vpnconfig.Subscription, now time.Time) string {
	where := s.Host()
	if s.Static() {
		where = "static list"
	}
	status := "OK"
	if s.Error != "" {
		status = s.Error
	}
	return fmt.Sprintf("%s — %s — %d servers — %s — %s", s.Name, where, len(s.Servers), timeAgo(now, s.Refreshed), status)
}

// timeAgo is how long before now t was, the way a list says it: "2 h ago".
func timeAgo(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
	}
}

// HandleCallback handles subs:r:<id> (refresh), subs:n:<id> (rename),
// subs:d:<id> (delete; asks first), subs:dy:<id> (delete confirmed) and
// subs:dn (keep).
func (h *SubsHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.Message.Chat == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	action, id, _ := strings.Cut(strings.TrimPrefix(cb.Data, "subs:"), ":")
	switch action {
	case "r":
		h.refresh(chatID, msgID, id)
	case "n":
		h.askName(chatID, id)
	case "d":
		h.askDelete(chatID, msgID, id)
	case "dy":
		h.remove(chatID, msgID, id)
	case "dn":
		h.redraw(chatID, msgID)
	}
}

func (h *SubsHandler) find(id string) (vpnconfig.Subscription, bool) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return vpnconfig.Subscription{}, false
	}
	i := vpnconfig.FindSubscription(subs, id)
	if i < 0 {
		return vpnconfig.Subscription{}, false
	}
	return subs[i], true
}

func (h *SubsHandler) gone(chatID int64) {
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("That subscription is gone"))
}

// redraw puts the current list into the message that carries the buttons.
func (h *SubsHandler) redraw(chatID int64, msgID int) {
	text, kb, err := h.list()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		kb = noButtons()
	}
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

func (h *SubsHandler) refresh(chatID int64, msgID int, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()
	res := service.RefreshSubscription(ctx, h.deps.Config, h.httpClient, id)
	if errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) {
		h.gone(chatID)
	} else {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(res.Line()))
	}
	h.redraw(chatID, msgID)
}

func (h *SubsHandler) askName(chatID int64, id string) {
	sub, ok := h.find(id)
	if !ok {
		h.gone(chatID)
		return
	}
	h.mu.Lock()
	h.renames[chatID] = pendingRename{id: id, name: sub.Name, until: h.now().Add(renameWait)}
	h.mu.Unlock()
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Send the new name for %s (or /cancel)", sub.Name)))
}

func (h *SubsHandler) askDelete(chatID int64, msgID int, id string) {
	sub, ok := h.find(id)
	if !ok {
		h.gone(chatID)
		h.redraw(chatID, msgID)
		return
	}
	text := fmt.Sprintf("Delete %s and its %d servers?", sub.Name, len(sub.Servers))
	if h.runsFrom(id) {
		text += " The running Xray server comes from it; Xray keeps running it until you select another."
	}
	kb := telegram.NewKeyboard().Button("Yes, delete", "subs:dy:"+id).Button("No", "subs:dn").Row().Build()
	h.deps.Sender.EditMessage(chatID, msgID, telegram.EscapeMarkdownV2(text), kb)
}

// runsFrom reports whether active_server names subscription id.
func (h *SubsHandler) runsFrom(id string) bool {
	cfg, err := h.deps.Config.LoadVPNConfig()
	return err == nil && cfg != nil && cfg.Xray.ActiveServer != nil && cfg.Xray.ActiveServer.Subscription == id
}

func (h *SubsHandler) remove(chatID int64, msgID int, id string) {
	sub, _ := h.find(id)
	active, err := service.DeleteSubscription(h.deps.Config, id)
	var text string
	switch {
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		text = "That subscription is gone"
	case errors.Is(err, vpnconfig.ErrServersSaved):
		text = fmt.Sprintf("Deleted %s, but xray.servers sync failed: %v", sub.Name, err)
	case err != nil:
		text = fmt.Sprintf("Delete failed: %v", err)
	case active:
		text = fmt.Sprintf("Deleted %s. The running server came from it and is no longer in any subscription: select another with /xray", sub.Name)
	default:
		text = "Deleted " + sub.Name
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(text))
	h.redraw(chatID, msgID)
}

// HandleTextInput takes the new name a rename waits for, and reports whether
// msg was one: a pending rename comes before the wizard. An answer the rename
// refuses uses the prompt up all the same.
func (h *SubsHandler) HandleTextInput(msg *tgbotapi.Message) bool {
	chatID := msg.Chat.ID
	h.mu.Lock()
	p, ok := h.renames[chatID]
	delete(h.renames, chatID)
	h.mu.Unlock()
	if !ok || h.now().After(p.until) {
		return false
	}
	err := service.RenameSubscription(h.deps.Config, p.id, msg.Text)
	var text string
	switch {
	case err == nil:
		text = fmt.Sprintf("Renamed %s to %s", p.name, strings.Trim(msg.Text, " "))
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		text = "That subscription is gone"
	default:
		text = fmt.Sprintf("Not renamed: %v. Tap ✎ to try again", err)
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(text))
	return true
}

// HandleCancel ends a rename that waits for its name.
func (h *SubsHandler) HandleCancel(msg *tgbotapi.Message) {
	h.ClearState(msg.Chat.ID)
	h.deps.Sender.Send(msg.Chat.ID, "Cancelled")
}

// ClearState forgets the rename the chat was asked to name.
func (h *SubsHandler) ClearState(chatID int64) {
	h.mu.Lock()
	delete(h.renames, chatID)
	h.mu.Unlock()
}
```

- [ ] **Step 7: Route `/subs`, `/cancel`, the callbacks and the rename's text**

In `server/internal/bot/router.go`, add the interface:

```go
// SubsRouterHandler defines methods for /subs and its rename prompt.
type SubsRouterHandler interface {
	HandleSubs(msg *tgbotapi.Message)
	HandleCancel(msg *tgbotapi.Message)
	HandleCallback(cb *tgbotapi.CallbackQuery)
	// HandleTextInput takes the name a rename waits for and reports whether it did.
	HandleTextInput(msg *tgbotapi.Message) bool
	ClearState(chatID int64)
}
```

Add `subs SubsRouterHandler` as the last field of `Router` and the last parameter of `NewRouter` (assign it). At the top of `RouteMessage`:

```go
	// Any command ends a rename that waits for its name: the next text is no
	// longer an answer to it.
	if r.subs != nil && msg.IsCommand() {
		r.subs.ClearState(msg.Chat.ID)
	}
```

Add the cases:

```go
	case "subs":
		r.subs.HandleSubs(msg)
	case "cancel":
		r.subs.HandleCancel(msg)
```

and begin the `default:` branch with:

```go
		// A rename waiting for its name takes the text first: its prompt is
		// the last question the user was asked.
		if r.subs != nil && r.subs.HandleTextInput(msg) {
			return
		}
```

In `RouteCallback`, before the wizard fallback:

```go
	if strings.HasPrefix(cb.Data, "subs:") {
		r.subs.HandleCallback(cb)
		return
	}
```

In `server/internal/bot/bot.go`: create `subsHandler := handler.NewSubsHandler(deps)` beside the other handlers and pass it last to `NewRouter`. In `RegisterCommands`, change the import entry and add one:

```go
		{Command: "import", Description: "Add a subscription, or refresh them all"},
		{Command: "subs", Description: "Subscriptions: refresh, rename, delete"},
```

In `server/internal/handler/misc.go`, the help lines become:

```
/xray \- quick server switch
/servers \- server list
/subs \- subscriptions: refresh, rename, delete
/import \[url\] \[name\] \- add a subscription; alone: refresh them all
```

Append to `server/internal/bot/router_test.go`:

```go
type mockSubsHandler struct {
	subsCalled, cancelCalled, callbackCalled, textCalled bool
	cleared                                              int
	takes                                                bool // what HandleTextInput answers
}

func (m *mockSubsHandler) HandleSubs(*tgbotapi.Message)           { m.subsCalled = true }
func (m *mockSubsHandler) HandleCancel(*tgbotapi.Message)         { m.cancelCalled = true }
func (m *mockSubsHandler) HandleCallback(*tgbotapi.CallbackQuery) { m.callbackCalled = true }
func (m *mockSubsHandler) HandleTextInput(*tgbotapi.Message) bool {
	m.textCalled = true
	return m.takes
}
func (m *mockSubsHandler) ClearState(int64) { m.cleared++ }

func TestRouter_RouteMessage_Subs(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s}

	router.RouteMessage(msgWithCommand("/subs"))

	if !s.subsCalled {
		t.Error("expected HandleSubs to be called")
	}
}

func TestRouter_RouteMessage_Cancel(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s}

	router.RouteMessage(msgWithCommand("/cancel"))

	if !s.cancelCalled {
		t.Error("expected HandleCancel to be called")
	}
}

// Any command ends a rename that waits for its name.
func TestRouter_AnyCommandEndsAPendingRename(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s, status: &mockStatusHandler{}}

	router.RouteMessage(msgWithCommand("/status"))

	if s.cleared != 1 {
		t.Errorf("ClearState called %d times, want 1", s.cleared)
	}
}

// A rename waiting for its name takes the text before the wizard does.
func TestRouter_APendingRenameTakesTheTextFirst(t *testing.T) {
	s := &mockSubsHandler{takes: true}
	w, e, c := &mockWizardHandler{}, &mockExcludeHandler{}, &mockClientsHandler{}
	router := &Router{subs: s, wizard: w, exclude: e, clients: c}

	router.RouteMessage(&tgbotapi.Message{Text: "Main", Chat: &tgbotapi.Chat{ID: 123}})

	if !s.textCalled || w.textCalled || e.textCalled || c.textInputCalled {
		t.Errorf("subs %v, wizard %v, exclude %v, clients %v", s.textCalled, w.textCalled, e.textCalled, c.textInputCalled)
	}
}

func TestRouter_TextWithoutARenameGoesToTheWizard(t *testing.T) {
	s := &mockSubsHandler{}
	w, e, c := &mockWizardHandler{}, &mockExcludeHandler{}, &mockClientsHandler{}
	router := &Router{subs: s, wizard: w, exclude: e, clients: c}

	router.RouteMessage(&tgbotapi.Message{Text: "192.168.1.100", Chat: &tgbotapi.Chat{ID: 123}})

	if !w.textCalled {
		t.Error("expected wizard.HandleTextInput to be called")
	}
}

func TestRouter_RouteCallback_Subs(t *testing.T) {
	s := &mockSubsHandler{}
	router := &Router{subs: s, wizard: &mockWizardHandler{}}

	router.RouteCallback(&tgbotapi.CallbackQuery{Data: "subs:r:0a1b2c3d"})

	if !s.callbackCalled {
		t.Error("expected subs.HandleCallback to be called")
	}
}
```

If a test in `router_test.go` calls `NewRouter` with nine handlers, add `&mockSubsHandler{}` as the tenth.

- [ ] **Step 8: Run the tests to see them pass**

Run: `cd server && go vet ./internal/handler/ ./internal/bot/ && go test ./internal/handler/ ./internal/bot/ && gofmt -l internal/handler internal/bot`
Expected: PASS; nothing from `go vet` or `gofmt -l`.

- [ ] **Step 9: Commit**

```bash
git add server/internal/handler server/internal/bot/router.go server/internal/bot/router_test.go server/internal/bot/bot.go
git commit -m "feat(bot): /import adds or refreshes subscriptions, /subs refreshes, renames and deletes them"
```

---

### Task 8: Bot — `/xray` in two steps, `/servers` by subscription

**Files:**
- Modify: `server/internal/handler/xray.go` (rewritten), `server/internal/handler/xray_test.go` (rewritten)
- Modify: `server/internal/handler/servers.go`, `server/internal/handler/servers_test.go`

**Interfaces:**
- Consumes: Task 7's test helpers (`newSubsStore`, `recordingSender`); existing `service.GenerateAndRecordActiveServer`, `mockVPNDirector` (status_test.go), `mockXrayGenerator` (xray_test.go).
- Produces:
  - `const xrayPerPage = 30`
  - `func serverFingerprint(s vpnconfig.Server) string` — 8 hex digits of sha256 of `subscription|name|address|port`
  - `func xraySubscriptions(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup)`
  - `func xrayServersPage(sub vpnconfig.Subscription, page int, back bool, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup)`
  - Callbacks: `xray:subs`, `xray:sub:<id>:<page>`, `xray:select:<id>:<index>:<fingerprint>`
  - `type serverLine struct { sub string; number int; server vpnconfig.Server }`, `func serverLines(subs []vpnconfig.Subscription) []serverLine`, `func buildServersPage(lines []serverLine, subCount, page int) (string, tgbotapi.InlineKeyboardMarkup)`

- [ ] **Step 1: Write the failing `/xray` tests**

Replace `server/internal/handler/xray_test.go` with the file below. It keeps `mockXrayGenerator`, which other tests in the package use.

```go
// internal/handler/xray_test.go
package handler

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockXrayGenerator for testing
type mockXrayGenerator struct {
	lastServer vpnconfig.Server
	err        error
}

func (m *mockXrayGenerator) GenerateConfig(server vpnconfig.Server, _ ...service.InboundPorts) error {
	m.lastServer = server
	return m.err
}

// servers is n servers named S1..Sn at a.example.com, ports 1..n.
func servers(n int) []vpnconfig.Server {
	out := make([]vpnconfig.Server, n)
	for i := range out {
		out[i] = vpnconfig.Server{Name: fmt.Sprintf("S%d", i+1), Address: "a.example.com", Port: i + 1, IPs: []string{"192.0.2.1"}}
	}
	return out
}

// twoGermanies is two subscriptions that both name a server Germany-1.
func twoGermanies() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de.example.com", Port: 443}}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de.example.com", Port: 443}}},
	}
}

func xrayCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: "cb", Data: data, Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}
}

func xrayHandler(store *subsStore) (*XrayHandler, *recordingSender, *mockXrayGenerator, *mockVPNDirector) {
	sender, gen, vpn := &recordingSender{}, &mockXrayGenerator{}, &mockVPNDirector{}
	return NewXrayHandler(&Deps{Sender: sender, Config: store, Xray: gen, VPN: vpn}), sender, gen, vpn
}

func selectData(sub vpnconfig.Subscription, i int) string {
	s := sub.Servers[i]
	s.Subscription = sub.ID
	return fmt.Sprintf("xray:select:%s:%d:%s", sub.ID, i, serverFingerprint(s))
}

func TestXray_OneSubscriptionOpensOnItsServers(t *testing.T) {
	h, sender, _, _ := xrayHandler(newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(2)}))

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	b := sender.buttons()
	if len(b) != 2 || !strings.HasPrefix(b[0], "xray:select:0a1b2c3d:0:") {
		t.Fatalf("buttons %v", b)
	}
}

func TestXray_SeveralSubscriptionsOpenOnTheSubscriptions(t *testing.T) {
	store := newSubsStore(twoGermanies()...)
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "de.example.com", Port: 443}
	h, sender, _, _ := xrayHandler(store)

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if got := sender.buttons(); !reflect.DeepEqual(got, []string{"xray:sub:0a1b2c3d:0", "xray:sub:1b2c3d4e:0"}) {
		t.Fatalf("buttons %v", got)
	}
	rows := sender.keyboard.InlineKeyboard
	if rows[0][0].Text != "Alpha (1)" || rows[1][0].Text != "✓ Beta (1)" {
		t.Fatalf("labels %q, %q", rows[0][0].Text, rows[1][0].Text)
	}
}

func TestXray_ASubscriptionIsListedThirtyServersAPage(t *testing.T) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(62)}

	_, kb := xrayServersPage(sub, 0, true, nil)
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			data = append(data, *b.CallbackData)
		}
	}
	if len(data) != 32 || data[30] != "xray:sub:0a1b2c3d:1" || data[31] != "xray:subs" {
		t.Fatalf("page 1: %d buttons, tail %v", len(data), data[30:])
	}

	_, kb = xrayServersPage(sub, 2, false, nil)
	last := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	if len(last) != 1 || *last[0].CallbackData != "xray:sub:0a1b2c3d:1" {
		t.Fatalf("page 3 navigation %v", last)
	}
}

// Two subscriptions can list the very same name, address and port: the
// fingerprint tells their buttons apart.
func TestXray_TheFingerprintTellsSubscriptionsApart(t *testing.T) {
	a, b := twoGermanies()[0].Servers[0], twoGermanies()[1].Servers[0]
	a.Subscription, b.Subscription = "0a1b2c3d", "1b2c3d4e"
	if serverFingerprint(a) == serverFingerprint(b) {
		t.Fatal("one fingerprint for two subscriptions")
	}
}

func TestXray_SelectsTheServerOfTheSubscriptionTapped(t *testing.T) {
	subs := twoGermanies()
	store := newSubsStore(subs...)
	h, sender, gen, _ := xrayHandler(store)

	h.HandleCallback(xrayCallback(selectData(subs[1], 0)))

	if gen.lastServer.Subscription != "1b2c3d4e" {
		t.Fatalf("generated %+v", gen.lastServer)
	}
	if a := store.cfg.Xray.ActiveServer; a == nil || a.Subscription != "1b2c3d4e" {
		t.Fatalf("active %+v", a)
	}
	if !strings.Contains(sender.last(), "Beta / Germany") {
		t.Fatalf("reply %q", sender.last())
	}
}

// A keyboard sent before subscriptions indexes a list that is gone, and a
// button whose server moved names another one. Neither switches anything.
func TestXray_AStaleButtonSwitchesNothing(t *testing.T) {
	subs := twoGermanies()
	for _, data := range []string{
		"xray:select:0",
		"xray:select:0:abcd1234",
		"xray:select:0a1b2c3d:0:00000000",
		"xray:select:ffffffff:0:00000000",
		"xray:select:0a1b2c3d:5:00000000",
	} {
		h, sender, gen, _ := xrayHandler(newSubsStore(subs...))
		h.HandleCallback(xrayCallback(data))
		if gen.lastServer.Name != "" || !strings.Contains(sender.last(), "run /xray again") {
			t.Errorf("%s: generated %+v, reply %q", data, gen.lastServer, sender.last())
		}
	}
}

func TestXray_AGenerationThatFailsIsReported(t *testing.T) {
	subs := twoGermanies()
	h, sender, gen, vpn := xrayHandler(newSubsStore(subs...))
	gen.err = errors.New("xray rejected the config")

	h.HandleCallback(xrayCallback(selectData(subs[0], 0)))

	if !strings.Contains(sender.last(), "xray rejected the config") || vpn.restartCalled {
		t.Fatalf("reply %q, restarted %v", sender.last(), vpn.restartCalled)
	}
}

func TestXray_NoServer(t *testing.T) {
	h, sender, _, _ := xrayHandler(newSubsStore())

	h.HandleXray(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if !strings.Contains(sender.last(), "/import") || len(sender.buttons()) != 0 {
		t.Fatalf("reply %q", sender.last())
	}
}
```

`mockVPNDirector` in `status_test.go` has no `restartCalled` field: add `restartCalled bool` to it and set it in its `RestartXray` method (`m.restartCalled = true; return nil`).

- [ ] **Step 2: Write the failing `/servers` tests**

In `server/internal/handler/servers_test.go`: keep `mockSenderWithKeyboard`, `TestExtractCountry` and `TestGroupServersByCountry` (the `/import` reply still groups by country). Delete the rest — every other test drives `buildServersPage` with a `[]vpnconfig.Server` or reads servers through `mockConfigStore.servers` — and add:

```go
func TestBuildServersPage_HeadsEachSubscriptionAndNumbersWithinIt(t *testing.T) {
	subs := []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(14)},
		{ID: "1b2c3d4e", Name: "Beta", Servers: servers(3)},
	}

	text, kb := buildServersPage(serverLines(subs), len(subs), 0)

	if !strings.Contains(text, "*Alpha*") || !strings.Contains(text, "*Beta*") || !strings.Contains(text, "in 2 subscriptions, page 1/2") {
		t.Fatalf("page 1: %q", text)
	}
	// Beta's first server is number 1 of Beta, on the page after Alpha's 14.
	if !strings.Contains(text, "\n1\\. S1") || strings.Count(text, "S1 —") != 2 {
		t.Fatalf("page 1 numbering: %q", text)
	}
	if len(kb.InlineKeyboard) != 1 {
		t.Fatalf("navigation %v", kb.InlineKeyboard)
	}

	// A page that starts in the middle of a subscription names it again.
	text, _ = buildServersPage(serverLines(subs), len(subs), 1)
	if !strings.Contains(text, "*Beta*") || !strings.Contains(text, "3\\. S3") {
		t.Fatalf("page 2: %q", text)
	}
}

func TestServersHandler_NoServer(t *testing.T) {
	sender := &recordingSender{}
	h := NewServersHandler(&Deps{Sender: sender, Config: newSubsStore()})

	h.HandleServers(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if !strings.Contains(sender.last(), "/import") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestServersHandler_PageButtonsTurnThePage(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(20)})
	sender := &recordingSender{}
	h := NewServersHandler(&Deps{Sender: sender, Config: store})

	h.HandleCallback(&tgbotapi.CallbackQuery{ID: "cb", Data: "servers:page:1", Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}})

	if !strings.Contains(sender.last(), "page 2/2") || !strings.Contains(sender.last(), "16\\. S16") {
		t.Fatalf("page %q", sender.last())
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `cd server && go test ./internal/handler/`
Expected: FAIL — `undefined: xrayServersPage`, `undefined: serverLines`.

- [ ] **Step 4: Rewrite `/xray`**

Replace `server/internal/handler/xray.go` with:

```go
// internal/handler/xray.go
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// xrayPerPage is how many servers one /xray page lists, two a row. A message
// holds a limited number of buttons, and two subscriptions already bring 94
// servers.
const xrayPerPage = 30

// XrayHandler handles /xray: the subscription first, then its server.
type XrayHandler struct {
	deps *Deps
}

// NewXrayHandler creates a new XrayHandler
func NewXrayHandler(deps *Deps) *XrayHandler {
	return &XrayHandler{deps: deps}
}

// HandleXray shows the subscriptions to choose from - or, with one, its servers.
func (h *XrayHandler) HandleXray(msg *tgbotapi.Message) {
	text, kb, err := h.firstStep()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, text)
		return
	}
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, kb)
}

// firstStep is what /xray opens with, and what « Back returns to.
func (h *XrayHandler) firstStep() (string, tgbotapi.InlineKeyboardMarkup, error) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if len(vpnconfig.AllServers(subs)) == 0 {
		return telegram.EscapeMarkdownV2("Серверы не найдены. Используйте /import для импорта"), tgbotapi.InlineKeyboardMarkup{}, nil
	}
	active := h.active()
	if len(subs) == 1 {
		text, kb := xrayServersPage(subs[0], 0, false, active)
		return text, kb, nil
	}
	text, kb := xraySubscriptions(subs, active)
	return text, kb, nil
}

func (h *XrayHandler) active() *vpnconfig.ActiveServer {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil || cfg == nil {
		return nil
	}
	return cfg.Xray.ActiveServer
}

// xraySubscriptions is the first step: a button per subscription with its
// server count, the running server's subscription checked.
func xraySubscriptions(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		if len(s.Servers) == 0 {
			continue
		}
		label := fmt.Sprintf("%s (%d)", s.Name, len(s.Servers))
		if active != nil && active.Subscription == s.ID {
			label = "✓ " + label
		}
		kb.Button(label, "xray:sub:"+s.ID+":0").Row()
	}
	return telegram.EscapeMarkdownV2("Выберите подписку:"), kb.Build()
}

// xrayServersPage is one page of a subscription's servers, two a row, with ◀ ▶
// between pages and « Back to the subscriptions when back is set.
func xrayServersPage(sub vpnconfig.Subscription, page int, back bool, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
	pages := max(1, (len(sub.Servers)+xrayPerPage-1)/xrayPerPage)
	page = max(0, min(page, pages-1))
	start := page * xrayPerPage
	end := min(start+xrayPerPage, len(sub.Servers))
	kb := telegram.NewKeyboard()
	for i := start; i < end; i++ {
		s := sub.Servers[i]
		s.Subscription = sub.ID
		label := fmt.Sprintf("%d. %s", i+1, s.Name)
		if active != nil && active.Subscription == s.Subscription && active.Name == s.Name && active.Address == s.Address && active.Port == s.Port {
			label = "✓ " + label
		}
		kb.Button(label, fmt.Sprintf("xray:select:%s:%d:%s", sub.ID, i, serverFingerprint(s)))
	}
	kb.Columns(2)
	if page > 0 {
		kb.Button("◀", fmt.Sprintf("xray:sub:%s:%d", sub.ID, page-1))
	}
	if page < pages-1 {
		kb.Button("▶", fmt.Sprintf("xray:sub:%s:%d", sub.ID, page+1))
	}
	if back {
		kb.Button("« Back", "xray:subs")
	}
	kb.Row()
	text := fmt.Sprintf("%s: выберите сервер", sub.Name)
	if pages > 1 {
		text += fmt.Sprintf(" (стр. %d/%d)", page+1, pages)
	}
	return telegram.EscapeMarkdownV2(text), kb.Build()
}

// serverFingerprint names a server in a button: the first 8 hex digits of
// sha256("subscription|name|address|port"). The list can change between /xray
// and the tap - the subscription watch rotates endpoints, a refresh replaces a
// list - and the index alone then names another server.
func serverFingerprint(s vpnconfig.Server) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", s.Subscription, s.Name, s.Address, s.Port)))
	return hex.EncodeToString(sum[:4])
}

// HandleCallback handles xray:subs (back to the subscriptions),
// xray:sub:<id>:<page> (a page of servers) and
// xray:select:<id>:<index>:<fingerprint> (the switch). A button of a keyboard
// sent before subscriptions - xray:select:<index>[:<fingerprint>] - indexes a
// list that is gone and is answered as a changed one.
func (h *XrayHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	switch data := cb.Data; {
	case data == "xray:subs":
		text, kb, err := h.firstStep()
		if err != nil {
			h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
			return
		}
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "xray:sub:"):
		id, pageText, _ := strings.Cut(strings.TrimPrefix(data, "xray:sub:"), ":")
		page, _ := strconv.Atoi(pageText)
		subs, err := h.deps.Config.LoadSubscriptions()
		if err != nil {
			h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
			return
		}
		i := vpnconfig.FindSubscription(subs, id)
		if i < 0 {
			h.stale(chatID, msgID)
			return
		}
		text, kb := xrayServersPage(subs[i], page, len(subs) > 1, h.active())
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "xray:select:"):
		h.selectServer(chatID, msgID, strings.TrimPrefix(data, "xray:select:"))
	}
}

// stale replaces a keyboard whose server the list no longer has at that place.
func (h *XrayHandler) stale(chatID int64, msgID int) {
	text := telegram.EscapeMarkdownV2("The server list has changed since these buttons were sent; run /xray again")
	h.deps.Sender.EditMessage(chatID, msgID, text, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
}

// selectServer switches Xray to the server <id>:<index>:<fingerprint> names.
func (h *XrayHandler) selectServer(chatID int64, msgID int, arg string) {
	parts := strings.Split(arg, ":")
	if len(parts) != 3 || !vpnconfig.ValidSubscriptionID(parts[0]) {
		h.stale(chatID, msgID)
		return
	}
	idx, err := strconv.Atoi(parts[1])
	if err != nil {
		h.stale(chatID, msgID)
		return
	}
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	si := vpnconfig.FindSubscription(subs, parts[0])
	if si < 0 || idx < 0 || idx >= len(subs[si].Servers) {
		h.stale(chatID, msgID)
		return
	}
	server := subs[si].Servers[idx]
	server.Subscription = subs[si].ID
	if parts[2] != serverFingerprint(server) {
		h.stale(chatID, msgID)
		return
	}

	// The generated inbound has to listen where the TPROXY rules send traffic,
	// so the ports come from advanced.xray rather than from the template.
	var ports service.InboundPorts
	vpnCfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(vpnCfg)

	// Generate the Xray config and record which server it came from, both
	// under the config lock: the Web UI reads that record to name the running
	// server, and a switch made there at the same moment must not be able to
	// leave the two disagreeing.
	generated, err := service.GenerateAndRecordActiveServer(h.deps.Config, h.deps.Xray, server, ports)
	if !generated {
		// config.json is untouched, so there is nothing to restart into.
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	if err != nil {
		// The switch itself worked and the user has nothing to do about a
		// lock timeout, so this stays in the log.
		slog.Warn("Failed to record the active server", "server", server.Name, "error", err)
	}

	if err := h.deps.VPN.RestartXray(); err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка перезапуска: %v", err)))
		return
	}

	successText := telegram.EscapeMarkdownV2(fmt.Sprintf("✓ Переключено на %s / %s", subs[si].Name, server.Name))
	h.deps.Sender.EditMessage(chatID, msgID, successText, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
}
```

- [ ] **Step 5: `/servers` by subscription**

In `server/internal/handler/servers.go`, replace `HandleServers`, `HandleCallback` and `buildServersPage` (keep `serversPerPage`, `extractCountry`, `groupServersByCountry`):

```go
// serverLine is one server on a /servers page: its subscription's name and its
// number within that subscription.
type serverLine struct {
	sub    string
	number int
	server vpnconfig.Server
}

// serverLines is every server of subs, in subscription order.
func serverLines(subs []vpnconfig.Subscription) []serverLine {
	var lines []serverLine
	for _, sub := range subs {
		for i, s := range sub.Servers {
			lines = append(lines, serverLine{sub: sub.Name, number: i + 1, server: s})
		}
	}
	return lines
}

// HandleServers handles /servers: every server, a header per subscription, in pages.
func (h *ServersHandler) HandleServers(msg *tgbotapi.Message) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Error: %v", err)))
		return
	}
	lines := serverLines(subs)
	if len(lines) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("No servers. Use /import to add a subscription."))
		return
	}
	text, keyboard := buildServersPage(lines, len(subs), 0)
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, keyboard)
}

// HandleCallback handles servers pagination callbacks (servers:page:N)
func (h *ServersHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	// Acknowledge callback
	h.deps.Sender.AckCallback(cb.ID)

	// Guard against nil Message (inline mode callbacks)
	if cb.Message == nil {
		return
	}

	var page int
	if _, err := fmt.Sscanf(cb.Data, "servers:page:%d", &page); err != nil {
		// noop button clicked
		return
	}
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return
	}
	lines := serverLines(subs)
	if len(lines) == 0 {
		return
	}
	text, keyboard := buildServersPage(lines, len(subs), page)
	h.deps.Sender.EditMessage(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
}

// buildServersPage builds one page of the list, a header wherever a
// subscription starts and at the top of the page, with the navigation keyboard.
func buildServersPage(lines []serverLine, subCount, page int) (string, tgbotapi.InlineKeyboardMarkup) {
	if len(lines) == 0 {
		return "No servers available\\.", tgbotapi.NewInlineKeyboardMarkup()
	}

	totalPages := (len(lines) + serversPerPage - 1) / serversPerPage
	page = max(0, min(page, totalPages-1))
	start := page * serversPerPage
	end := min(start+serversPerPage, len(lines))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🖥 *Servers* \\(%d\\) in %d subscriptions, page %d/%d:\n",
		len(lines), subCount, page+1, totalPages))

	for i := start; i < end; i++ {
		l := lines[i]
		if i == start || l.sub != lines[i-1].sub {
			sb.WriteString("\n*" + telegram.EscapeMarkdownV2(l.sub) + "*\n")
		}
		s := l.server
		sb.WriteString(fmt.Sprintf("%d\\. %s — %s \\(%s\\) · %s\n",
			l.number,
			telegram.EscapeMarkdownV2(s.Name),
			telegram.EscapeMarkdownV2(s.Address),
			telegram.EscapeMarkdownV2(strings.Join(s.IPs, ", ")),
			telegram.EscapeMarkdownV2(s.Label())))
	}

	// Navigation buttons
	var buttons []tgbotapi.InlineKeyboardButton
	if page > 0 {
		buttons = append(buttons,
			tgbotapi.NewInlineKeyboardButtonData("← Prev", fmt.Sprintf("servers:page:%d", page-1)))
	}
	buttons = append(buttons,
		tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, totalPages), "servers:noop"))
	if page < totalPages-1 {
		buttons = append(buttons,
			tgbotapi.NewInlineKeyboardButtonData("Next →", fmt.Sprintf("servers:page:%d", page+1)))
	}

	return sb.String(), tgbotapi.NewInlineKeyboardMarkup(buttons)
}
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `cd server && go vet ./internal/handler/ && go test ./internal/handler/ && gofmt -l internal/handler`
Expected: PASS; nothing from `go vet` or `gofmt -l`.

- [ ] **Step 7: Commit**

```bash
git add server/internal/handler
git commit -m "feat(bot): /xray picks a subscription, then its server; /servers heads each subscription"
```

---

### Task 9: Wizard step 1 — subscription, then server

**Files:**
- Modify: `server/internal/wizard/server.go`, `server/internal/wizard/server_test.go`
- Modify: `server/internal/wizard/state.go`, `server/internal/wizard/state_test.go`
- Modify: `server/internal/wizard/confirm.go`
- Modify: `server/internal/wizard/apply_test.go` (`trackingConfigStore.LoadServers`)

**Interfaces:**
- Consumes: Task 1's `vpnconfig.AllServers`, `FindSubscription`, `ActiveServer.Subscription`; Task 3's `ConfigStore.LoadSubscriptions`.
- Produces:
  - `const serverPerPage = 30`
  - `func subscriptionChoice(subs []vpnconfig.Subscription) (string, tgbotapi.InlineKeyboardMarkup)`
  - `func serverPage(sub vpnconfig.Subscription, page int, back bool) (string, tgbotapi.InlineKeyboardMarkup)`
  - Callbacks: `server:subs`, `server:sub:<id>:<page>`, `server:<id>:<index>` (the pick)
  - `State.picks` compares the subscription too.

- [ ] **Step 1: Let the wizard's fakes answer from subscriptions**

In `server/internal/wizard/server_test.go`, make `mockConfigStore.LoadServers` flatten `subs` when a test sets them, so the tests that still set `servers` keep working:

```go
func (m *mockConfigStore) LoadServers() ([]vpnconfig.Server, error) {
	if m.subs != nil {
		return vpnconfig.AllServers(m.subs), m.err
	}
	return m.servers, m.err
}
```

Do the same in `apply_test.go` for `trackingConfigStore.LoadServers` (with `m.loadErr`).

- [ ] **Step 2: Write the failing tests**

In `server/internal/wizard/server_test.go`, convert `TestServerStep_Render`, `TestServerStep_HandleCallback` and `TestServerStep_GridLayoutWithManyServers`: every `servers: X` in a `mockConfigStore` becomes `subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: X}}`; every callback `server:N` becomes `server:0a1b2c3d:N`; the cases that expected "Invalid server index" for an index out of range now expect `The server list has changed` and an edited keyboard. Then add:

```go
// twoSubs is two subscriptions that both name a server Germany-1.
func twoSubs() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{
			{Name: "Oslo", Address: "a.example.com", Port: 443},
			{Name: "Germany-1", Address: "de.example.com", Port: 443},
		}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de.example.com", Port: 443}}},
	}
}

func buttonData(kb *tgbotapi.InlineKeyboardMarkup) []string {
	var out []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			out = append(out, *b.CallbackData)
		}
	}
	return out
}

func TestServerStep_SeveralSubscriptionsOpenOnTheSubscriptions(t *testing.T) {
	sender := &mockSender{}
	step := NewServerStep(&StepDeps{Sender: sender, Config: &mockConfigStore{subs: twoSubs()}}, nil)

	step.Render(123, NewManager().Start(123))

	if got := buttonData(sender.lastKeyboard); !reflect.DeepEqual(got, []string{"server:sub:0a1b2c3d:0", "server:sub:1b2c3d4e:0", "cancel"}) {
		t.Fatalf("buttons %v", got)
	}
}

func TestServerStep_OneSubscriptionOpensOnItsServers(t *testing.T) {
	sender := &mockSender{}
	step := NewServerStep(&StepDeps{Sender: sender, Config: &mockConfigStore{subs: twoSubs()[:1]}}, nil)

	step.Render(123, NewManager().Start(123))

	if got := buttonData(sender.lastKeyboard); !reflect.DeepEqual(got, []string{"server:0a1b2c3d:0", "server:0a1b2c3d:1", "cancel"}) {
		t.Fatalf("buttons %v", got)
	}
}

// Both subscriptions list Germany-1 at one address: the pick records the one tapped.
func TestServerStep_PicksTheServerOfTheSubscriptionTapped(t *testing.T) {
	var next *State
	step := NewServerStep(&StepDeps{Sender: &mockSender{}, Config: &mockConfigStore{subs: twoSubs()}}, func(_ int64, s *State) { next = s })
	state := NewManager().Start(123)

	step.HandleCallback(&tgbotapi.CallbackQuery{Data: "server:1b2c3d4e:0", Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}, state)

	if next == nil || state.Picked == nil || state.Picked.Subscription != "1b2c3d4e" || state.GetStep() != StepExclusions {
		t.Fatalf("picked %+v, step %v", state.Picked, state.GetStep())
	}
	if i := state.PickedIndex(vpnconfig.AllServers(twoSubs())); i != 2 {
		t.Fatalf("picked index %d, want 2 (Beta's Germany-1 in the flattened list)", i)
	}
}

// A button of a keyboard sent before subscriptions names a list that is gone.
func TestServerStep_AnOldButtonStartsStepOneAgain(t *testing.T) {
	sender := &mockSender{}
	step := NewServerStep(&StepDeps{Sender: sender, Config: &mockConfigStore{subs: twoSubs()}}, nil)
	state := NewManager().Start(123)

	step.HandleCallback(&tgbotapi.CallbackQuery{Data: "server:1", Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}}, state)

	if state.Picked != nil || !strings.Contains(sender.lastText, "The server list has changed") {
		t.Fatalf("picked %+v, text %q", state.Picked, sender.lastText)
	}
	if got := buttonData(sender.lastKeyboard); len(got) != 3 || got[0] != "server:sub:0a1b2c3d:0" {
		t.Fatalf("buttons %v", got)
	}
}

func TestServerStep_ThirtyServersAPage(t *testing.T) {
	var many []vpnconfig.Server
	for i := 0; i < 62; i++ {
		many = append(many, vpnconfig.Server{Name: fmt.Sprintf("S%d", i+1), Address: "a.example.com", Port: i + 1})
	}

	_, kb := serverPage(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Main", Servers: many}, 1, true)

	got := buttonData(&kb)
	if len(got) != 34 || got[0] != "server:0a1b2c3d:30" || got[30] != "server:sub:0a1b2c3d:0" || got[31] != "server:sub:0a1b2c3d:2" || got[32] != "server:subs" || got[33] != "cancel" {
		t.Fatalf("buttons %v", got)
	}
}
```

Add `fmt`, `reflect` and `strings` to the file's imports if it lacks them. In `server/internal/wizard/state_test.go`, add:

```go
func TestState_PickedIndexTellsSubscriptionsApart(t *testing.T) {
	servers := []vpnconfig.Server{
		{Subscription: "0a1b2c3d", Name: "Germany-1", Address: "de.example.com", Port: 443},
		{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "de.example.com", Port: 443},
	}
	s := NewManager().Start(123)
	s.PickServer(0, servers[1]) // a stale hint: the pick is Beta's

	if got := s.PickedIndex(servers); got != 1 {
		t.Fatalf("PickedIndex %d, want 1", got)
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `cd server && go test ./internal/wizard/`
Expected: FAIL — `undefined: serverPage`; the pick and index tests fail.

- [ ] **Step 4: Compare the subscription when looking the pick up**

In `server/internal/wizard/state.go`:

```go
func (s *State) picks(srv vpnconfig.Server) bool {
	return srv.Subscription == s.Picked.Subscription && srv.Name == s.Picked.Name &&
		srv.Address == s.Picked.Address && srv.Port == s.Picked.Port
}
```

and change the doc comment of `PickedIndex` to "by subscription, name, address and port".

- [ ] **Step 5: Rewrite the server step**

In `server/internal/wizard/server.go`, replace `Render` and `HandleCallback` (keep `NewServerStep`, `HandleMessage`, `clearWizard`, `getServerGridColumns`) and add `strconv`, `strings` and `vpnconfig` to the imports:

```go
// serverPerPage is how many servers one page of step 1 lists.
const serverPerPage = 30

// Render shows the subscriptions to choose from - or, with one, its servers.
func (s *ServerStep) Render(chatID int64, state *State) {
	subs, err := s.deps.Config.LoadSubscriptions()
	if err != nil || len(vpnconfig.AllServers(subs)) == 0 {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("No servers found. Use /import"))
		s.clearWizard(chatID)
		return
	}
	text, kb := firstServerPage(subs)
	s.deps.Sender.SendWithKeyboard(chatID, text, kb)
}

// firstServerPage is what step 1 opens with: the subscriptions, or with one
// subscription its servers.
func firstServerPage(subs []vpnconfig.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	if len(subs) == 1 {
		return serverPage(subs[0], 0, false)
	}
	return subscriptionChoice(subs)
}

// subscriptionChoice is a button per subscription that has servers.
func subscriptionChoice(subs []vpnconfig.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	total := 0
	for _, sub := range subs {
		if len(sub.Servers) == 0 {
			continue
		}
		total += len(sub.Servers)
		kb.Button(fmt.Sprintf("%s (%d)", sub.Name, len(sub.Servers)), "server:sub:"+sub.ID+":0").Row()
	}
	kb.Button("Cancel", "cancel").Row()
	return telegram.EscapeMarkdownV2(fmt.Sprintf("Step 1/4: Select a subscription (%d servers)", total)), kb.Build()
}

// serverPage is one page of a subscription's servers, with ◀ ▶ between pages
// and « Back to the subscriptions when back is set.
func serverPage(sub vpnconfig.Subscription, page int, back bool) (string, tgbotapi.InlineKeyboardMarkup) {
	pages := max(1, (len(sub.Servers)+serverPerPage-1)/serverPerPage)
	page = max(0, min(page, pages-1))
	start := page * serverPerPage
	end := min(start+serverPerPage, len(sub.Servers))
	kb := telegram.NewKeyboard()
	for i := start; i < end; i++ {
		kb.Button(fmt.Sprintf("%d. %s", i+1, sub.Servers[i].Name), fmt.Sprintf("server:%s:%d", sub.ID, i))
	}
	kb.Columns(getServerGridColumns(end - start))
	if page > 0 {
		kb.Button("◀", fmt.Sprintf("server:sub:%s:%d", sub.ID, page-1))
	}
	if page < pages-1 {
		kb.Button("▶", fmt.Sprintf("server:sub:%s:%d", sub.ID, page+1))
	}
	if back {
		kb.Button("« Back", "server:subs")
	}
	kb.Row()
	kb.Button("Cancel", "cancel").Row()
	text := fmt.Sprintf("Step 1/4: Select Xray server of %s (%d available)", sub.Name, len(sub.Servers))
	if pages > 1 {
		text += fmt.Sprintf(", page %d/%d", page+1, pages)
	}
	return telegram.EscapeMarkdownV2(text), kb.Build()
}

// flatIndex is where subs[si].Servers[i] sits in the flattened list
// ConfigStore.LoadServers answers, the one steps 4 and the apply read.
func flatIndex(subs []vpnconfig.Subscription, si, i int) int {
	for _, sub := range subs[:si] {
		i += len(sub.Servers)
	}
	return i
}

// HandleCallback processes step 1's buttons: server:subs, server:sub:<id>:<page>
// and server:<id>:<index>, the pick. A button whose subscription or server is
// gone - server:<index> from before subscriptions among them - starts step 1
// again.
func (s *ServerStep) HandleCallback(cb *tgbotapi.CallbackQuery, state *State) {
	if !strings.HasPrefix(cb.Data, "server:") || cb.Message == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	subs, err := s.deps.Config.LoadSubscriptions()
	if err != nil {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Failed to load servers"))
		return
	}
	if len(vpnconfig.AllServers(subs)) == 0 {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("No servers found. Use /import"))
		s.clearWizard(chatID)
		return
	}
	switch rest := strings.TrimPrefix(cb.Data, "server:"); {
	case rest == "subs":
		text, kb := firstServerPage(subs)
		s.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	case strings.HasPrefix(rest, "sub:"):
		id, pageText, _ := strings.Cut(strings.TrimPrefix(rest, "sub:"), ":")
		page, _ := strconv.Atoi(pageText)
		if si := vpnconfig.FindSubscription(subs, id); si >= 0 {
			text, kb := serverPage(subs[si], page, len(subs) > 1)
			s.deps.Sender.EditMessage(chatID, msgID, text, kb)
			return
		}
	default:
		id, idxText, found := strings.Cut(rest, ":")
		idx, err := strconv.Atoi(idxText)
		si := vpnconfig.FindSubscription(subs, id)
		if found && err == nil && si >= 0 && idx >= 0 && idx < len(subs[si].Servers) {
			srv := subs[si].Servers[idx]
			srv.Subscription = subs[si].ID
			state.PickServer(flatIndex(subs, si, idx), srv)
			state.SetStep(StepExclusions)
			// Default: include ru in exclusions
			state.SetExclusion("ru", true)
			if s.next != nil {
				s.next(chatID, state)
			}
			return
		}
	}
	text, kb := firstServerPage(subs)
	s.deps.Sender.EditMessage(chatID, msgID, telegram.EscapeMarkdownV2("The server list has changed; select again.")+"\n\n"+text, kb)
}
```

- [ ] **Step 6: Name the subscription in step 4**

In `server/internal/wizard/confirm.go`, after `servers, err := s.deps.Config.LoadServers()` and its check, add:

```go
	// The subscription names, for "Beta / Germany-1": two subscriptions can
	// share a server name. A store that cannot list them leaves the name alone.
	subNames := map[string]string{}
	if subs, err := s.deps.Config.LoadSubscriptions(); err == nil {
		for _, sub := range subs {
			subNames[sub.ID] = sub.Name
		}
	}
```

and where it prints the selected server:

```go
	if serverIndex >= 0 {
		srv := servers[serverIndex]
		label := srv.Name
		if n := subNames[srv.Subscription]; n != "" {
			label = n + " / " + srv.Name
		}
		sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("Xray server: %s (%s)", label, strings.Join(srv.IPs, ", "))) + "\n")
	} else if name := state.PickedName(); name != "" {
```

- [ ] **Step 7: Run the tests to see them pass**

Run: `cd server && go vet ./internal/wizard/ && go test ./internal/wizard/ && gofmt -l internal/wizard`
Expected: PASS; `gofmt -l` lists at most `internal/wizard/handler.go`, which was unformatted on `master` before this work.

- [ ] **Step 8: Commit**

```bash
git add server/internal/wizard
git commit -m "feat(wizard): step 1 picks a subscription, then its server"
```

---

### Task 10: The walk's order and the dedupe key

**Files:**
- Create: `server/internal/subwatch/order.go`, `server/internal/subwatch/order_test.go`
- Modify: `server/internal/subwatch/watch.go` (`sameServer`, `activeID`, `serverID`, `chosenIndex`)

**Interfaces:**
- Consumes: Task 1's `vpnconfig.Subscription`, `Server.Subscription`, `ActiveServer.Subscription`; existing `pickOrder`, `chosenIndex`, `ServerForDial`.
- Produces:
  - `const OwnFirst = 3`
  - `func walkOrder(subs []vpnconfig.Subscription, chosen *vpnconfig.ActiveServer) (order []vpnconfig.Server, chosenFirst bool)`
  - `func dialKey(c vpnconfig.Server) string`
  - `sameServer`, `chosenIndex`, `activeID` and `serverID` compare or carry the subscription.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/subwatch/order_test.go`:

```go
package subwatch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// orderSub is a subscription whose servers are named <letter>1..<letter>n,
// the letter being the id's first character upper-cased.
func orderSub(id string, n int) vpnconfig.Subscription {
	letter := strings.ToUpper(id[:1])
	s := vpnconfig.Subscription{ID: id, Name: letter}
	for i := 1; i <= n; i++ {
		s.Servers = append(s.Servers, vpnconfig.Server{Name: fmt.Sprintf("%s%d", letter, i), Address: fmt.Sprintf("%s%d.example", id[:1], i), Port: 443})
	}
	return s
}

func chosenOf(sub, name string) *vpnconfig.ActiveServer {
	return &vpnconfig.ActiveServer{Subscription: sub, Name: name, Address: strings.ToLower(name) + ".example", Port: 443}
}

func orderNames(subs []vpnconfig.Subscription, chosen *vpnconfig.ActiveServer) ([]string, bool) {
	order, first := walkOrder(subs, chosen)
	var out []string
	for _, s := range order {
		out = append(out, s.Name)
	}
	return out, first
}

// spec 5.3: the chosen server, two more of its subscription, then one server
// of each subscription in turn, starting after the chosen server's.
func TestWalkOrder_TheChosenServerTwoNeighboursThenOneOfEach(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 7), orderSub("bbbbbbbb", 4)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A5"))

	want := []string{"A5", "A1", "A2", "B1", "A3", "B2", "A4", "B3", "A6", "B4", "A7"}
	if !reflect.DeepEqual(got, want) || !first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

func TestWalkOrder_TheRotationStartsAfterTheChosenSubscription(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 4), orderSub("bbbbbbbb", 4), orderSub("cccccccc", 4)}

	got, _ := orderNames(subs, chosenOf("bbbbbbbb", "B1"))

	want := []string{"B1", "B2", "B3", "C1", "A1", "B4", "C2", "A2", "C3", "A3", "C4", "A4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// With one subscription the order is today's pickOrder: the chosen server,
// then the rest of the list.
func TestWalkOrder_WithOneSubscriptionItIsPickOrder(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 5)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A4"))

	if want := []string{"A4", "A1", "A2", "A3", "A5"}; !reflect.DeepEqual(got, want) || !first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

// Nothing chosen, a record from before subscriptions, and a subscription
// deleted since: the turns start at the first subscription.
func TestWalkOrder_WithoutAChosenSubscriptionTheTurnsStartAtTheFirst(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 2), orderSub("bbbbbbbb", 2)}
	for _, chosen := range []*vpnconfig.ActiveServer{nil, chosenOf("", "A1"), chosenOf("dddddddd", "A1")} {
		got, first := orderNames(subs, chosen)
		if want := []string{"A1", "B1", "A2", "B2"}; !reflect.DeepEqual(got, want) || first {
			t.Errorf("chosen %+v: got %v (first %v), want %v", chosen, got, first, want)
		}
	}
}

// The chosen server left its list, but its subscription is there: the walk
// still gives that provider its OwnFirst servers first.
func TestWalkOrder_AChosenServerThatIsGoneKeepsItsSubscriptionFirst(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 5), orderSub("bbbbbbbb", 2)}

	got, first := orderNames(subs, chosenOf("aaaaaaaa", "A9"))

	if want := []string{"A1", "A2", "A3", "B1", "A4", "B2", "A5"}; !reflect.DeepEqual(got, want) || first {
		t.Fatalf("got %v (first %v), want %v", got, first, want)
	}
}

// Two subscriptions name a server Germany-1. A choice made in Beta, at an
// address its list has since rotated, is Beta's Germany-1 - never Alpha's.
func TestWalkOrder_TheSameNameInAnotherSubscriptionIsNotTheChoice(t *testing.T) {
	a := vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", Servers: []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443},
		{Name: "Germany-1", Address: "de-a.example", Port: 443},
	}}
	b := vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{{Name: "Germany-1", Address: "de-b.example", Port: 443}}}
	chosen := &vpnconfig.ActiveServer{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "de-rotated.example", Port: 443}

	order, first := walkOrder([]vpnconfig.Subscription{a, b}, chosen)

	if !first || order[0].Subscription != "bbbbbbbb" || order[0].Address != "de-b.example" {
		t.Fatalf("first %v, order[0] %+v", first, order[0])
	}
}

func TestWalkOrder_MarksEachServerWithItsSubscription(t *testing.T) {
	subs := []vpnconfig.Subscription{orderSub("aaaaaaaa", 2), orderSub("bbbbbbbb", 1)}

	order, _ := walkOrder(subs, nil)

	for _, s := range order {
		if s.Subscription != strings.ToLower(s.Name[:1])+strings.Repeat(strings.ToLower(s.Name[:1]), 7) {
			t.Fatalf("server %s carries %q", s.Name, s.Subscription)
		}
	}
	if subs[0].Servers[0].Subscription != "" {
		t.Fatal("walkOrder changed the subscriptions it read")
	}
}

func TestDialKey_OneEndpointUnderTwoNamesIsOneServer(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	a := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: ob}
	b := a
	b.Name = "Germany-2"
	c := a
	c.IPs = []string{"192.0.2.2"}

	if dialKey(a) != dialKey(b) {
		t.Fatal("two names on one endpoint dial differently")
	}
	if dialKey(a) == dialKey(c) {
		t.Fatal("two addresses dial alike")
	}
	if dialKey(vpnconfig.Server{Name: "Legacy", Address: "l.example", Port: 443}) != "" {
		t.Fatal("a record without an outbound has a key")
	}
}
```

Append to `server/internal/subwatch/watch_test.go`, beside the `TestPickOrder_*` tests:

```go
func TestChosenIndex_TheNameFallbackStaysInItsSubscription(t *testing.T) {
	servers := []vpnconfig.Server{
		{Subscription: "aaaaaaaa", Name: "Germany-1", Address: "de-a.example", Port: 443},
		{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "de-b.example", Port: 443},
	}
	chosen := &vpnconfig.ActiveServer{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "de-rotated.example", Port: 443}

	if i := chosenIndex(servers, chosen); i != 1 {
		t.Fatalf("chosenIndex %d, want 1", i)
	}
	// A record from before subscriptions names no server of one.
	if i := chosenIndex(servers, &vpnconfig.ActiveServer{Name: "Germany-1", Address: "de-a.example", Port: 443}); i != -1 {
		t.Fatalf("a record without a subscription matched %d", i)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd server && go test ./internal/subwatch/ -run 'WalkOrder|DialKey|ChosenIndex'`
Expected: FAIL — `undefined: walkOrder`, `undefined: dialKey`; `TestChosenIndex_*` fails on the subscription-blind `chosenIndex`.

- [ ] **Step 3: Carry the subscription in the identity helpers**

In `server/internal/subwatch/watch.go`, replace `sameServer`, `activeID`, `serverID` and `chosenIndex`:

```go
func sameServer(s vpnconfig.Server, a *vpnconfig.ActiveServer) bool {
	if a == nil || s.Subscription != a.Subscription || s.Name != a.Name {
		return false
	}
	if a.Address == "" && a.Port == 0 {
		return true
	}
	return s.Address == a.Address && s.Port == a.Port
}

func activeID(a *vpnconfig.ActiveServer) string {
	if a == nil {
		return ""
	}
	if a.Address == "" && a.Port == 0 {
		return a.Subscription + "\x1f" + a.Name
	}
	return a.Subscription + "\x1f" + a.Name + "\x1f" + a.Address + "\x1f" + strconv.Itoa(a.Port)
}

func serverID(s vpnconfig.Server) string {
	return s.Subscription + "\x1f" + s.Name + "\x1f" + s.Address + "\x1f" + strconv.Itoa(s.Port)
}

// chosenIndex is where servers has the server a names: the entry of its
// subscription with its name, address and port, or else the first entry of its
// subscription with its name - a subscription that rotates endpoints gives a
// name a new address every day, and the name is what the user chose. Another
// subscription's server of the same name is never it. -1 when the list has
// neither.
func chosenIndex(servers []vpnconfig.Server, a *vpnconfig.ActiveServer) int {
	if a == nil || a.Name == "" {
		return -1
	}
	byName := -1
	for i, s := range servers {
		if s.Subscription != a.Subscription || s.Name != a.Name {
			continue
		}
		if sameServer(s, a) {
			return i
		}
		if byName < 0 {
			byName = i
		}
	}
	return byName
}
```

- [ ] **Step 4: Write the order and the key**

Create `server/internal/subwatch/order.go`:

```go
package subwatch

import "github.com/zinin/vpn-director/server/internal/vpnconfig"

// OwnFirst is how many servers of the chosen server's subscription a walk
// tries, the chosen one included, before it takes one server of each
// subscription in turn. A server that fails alone usually has a neighbour that
// works; a provider that fails as a whole - expired, blocked - costs no more
// than these few tries before another provider's first server.
const OwnFirst = 3

// walkOrder is the order a walk tries the servers of subs in (spec 5.3): the
// chosen server - found by its subscription, name, address and port, or by
// subscription and name - then the next servers of its subscription in list
// order, OwnFirst of them in all; then one server of each subscription in
// turn, in subscription order, starting with the one after the chosen
// server's; the chosen subscription's remaining servers take their turns too.
// Without the chosen server's subscription the turns start at the first. With
// one subscription this is pickOrder. chosenFirst reports that the chosen
// server leads the order. Every server carries its subscription's id.
func walkOrder(subs []vpnconfig.Subscription, chosen *vpnconfig.ActiveServer) (order []vpnconfig.Server, chosenFirst bool) {
	if len(subs) == 0 {
		return nil, false
	}
	queues := make([][]vpnconfig.Server, len(subs))
	own := -1
	for i, sub := range subs {
		q := make([]vpnconfig.Server, len(sub.Servers))
		for j, s := range sub.Servers {
			s.Subscription = sub.ID
			q[j] = s
		}
		queues[i] = q
		if own < 0 && chosen != nil && sub.ID == chosen.Subscription {
			own = i
		}
	}
	start := 0
	if own >= 0 {
		chosenFirst = chosenIndex(queues[own], chosen) >= 0
		q := pickOrder(queues[own], chosen)
		n := min(OwnFirst, len(q))
		order = append(order, q[:n]...)
		queues[own] = q[n:]
		start = (own + 1) % len(subs)
	}
	for placed := true; placed; {
		placed = false
		for k := range subs {
			i := (start + k) % len(subs)
			if len(queues[i]) == 0 {
				continue
			}
			order = append(order, queues[i][0])
			queues[i] = queues[i][1:]
			placed = true
		}
	}
	return order, chosenFirst
}

// dialKey is what a walk's copy of a server dials: its outbound with the
// address in place, as Generate writes it (ServerForDial). Copies with one key
// are one server to Xray whatever their names - one provider lists 62 names on
// 9 endpoints - and a walk tries each key once. A record without an outbound
// has no key and is never taken for another.
func dialKey(c vpnconfig.Server) string {
	if len(c.Outbound) == 0 {
		return ""
	}
	return string(ServerForDial(c).Outbound)
}
```

- [ ] **Step 5: Run the whole package**

Run: `cd server && go test ./internal/subwatch/ && gofmt -l internal/subwatch`
Expected: PASS — the existing tests carry no subscription on either side, so the identity helpers answer them as before; `gofmt -l` silent.

- [ ] **Step 6: Commit**

```bash
git add server/internal/subwatch/order.go server/internal/subwatch/order_test.go server/internal/subwatch/watch.go server/internal/subwatch/watch_test.go
git commit -m "feat(subwatch): the hybrid walk order and one try per endpoint"
```

---

### Task 11: The watch over every subscription

**Files:**
- Modify: `server/internal/vpnconfig/failover.go` (`Armed`), `server/internal/vpnconfig/failover_test.go`
- Modify: `server/internal/subwatch/watch.go`, `server/internal/subwatch/return.go`, `server/internal/subwatch/reach.go`
- Modify: `server/internal/subwatch/watch_test.go`, `server/internal/subwatch/return_test.go`, `server/internal/subwatch/reach_test.go`
- Create: `server/internal/subwatch/wave_test.go`
- Modify: `server/internal/bot/bot.go` (the watch's wiring)

**Interfaces:**
- Consumes: Task 2's `vpnconfig.RefreshSubscription`, `RecordSubscriptionError`, `SubscriptionFiles`, `ErrSubscriptionGone`; Task 10's `walkOrder`, `dialKey`; Task 3's `ConfigService.LoadSubscriptions`, `SaveSubscription`.
- Produces:
  - `vpnconfig.Armed(cfg *VPNDirectorConfig, subscriptions int) bool`
  - `Watch.LoadSubscriptions func() ([]vpnconfig.Subscription, error)`, `Watch.SaveSubscription func(vpnconfig.Subscription) error`; `Watch.LoadServers` and `Watch.SaveServers` are gone
  - `walkGuard(sub, link, started, lastRecorded string, expectedSeq int)`, `walkOwns(cfg, started, lastRecorded, expectedSeq)`, `walkOwnsNow(started, lastRecorded, expectedSeq)`, `walkEnded(err error) bool`
  - Messages: `msgNoLive = "No live server in any subscription"`, `msgRefreshFailed` plus the failed names; a server is named `<subscription> / <server>`.

- [ ] **Step 1: `Armed` counts subscriptions**

In `server/internal/vpnconfig/failover.go`, replace `Armed`:

```go
// Armed reports whether the subscription watch has work. Effective Xray
// clients and at least one subscription - a static list included - arm it for
// a failover of its own. A failover record arms it with or without either: the
// clients the record took off Xray still have to come back.
func Armed(cfg *VPNDirectorConfig, subscriptions int) bool {
	if cfg == nil {
		return false
	}
	if cfg.Xray.Failover != nil {
		return true
	}
	return subscriptions > 0 && len(EffectiveXrayClients(cfg)) > 0
}
```

In `failover_test.go`, every `Armed(cfg)` becomes `Armed(cfg, 1)` where the test's config had a `SubscriptionURL`, and `Armed(cfg, 0)` where it had none; drop `SubscriptionURL` from those configs. Add:

```go
// A link saved by an earlier release arms nothing: only subscription files do.
func TestArmed_NeedsASubscription(t *testing.T) {
	cfg := &VPNDirectorConfig{Xray: XrayConfig{Clients: []string{"192.168.1.8"}}}
	if Armed(cfg, 0) || !Armed(cfg, 1) {
		t.Fatalf("Armed(0) %v, Armed(1) %v", Armed(cfg, 0), Armed(cfg, 1))
	}
}
```

- [ ] **Step 2: Move the watch's tests onto subscriptions**

In `server/internal/subwatch/watch_test.go`:

1. Give `fake` two fields and three helpers:

```go
	subs   []vpnconfig.Subscription // the subscription files; nil takes baseSubs
	noSubs bool                     // no subscription at all
```

```go
// baseSubs is the one subscription the older tests assume. Its id and name are
// empty, so the ActiveServer records those tests write - which name no
// subscription - still match its servers, and the messages name the servers
// alone, as they did.
func baseSubs() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{{URL: "https://cdn.example/s/token"}}
}

// cloneSubs hands out what production's LoadSubscriptions does: fresh copies,
// every server carrying its subscription's id.
func cloneSubs(subs []vpnconfig.Subscription) []vpnconfig.Subscription {
	out := make([]vpnconfig.Subscription, len(subs))
	for i, s := range subs {
		s.Servers = append([]vpnconfig.Server(nil), s.Servers...)
		for j := range s.Servers {
			s.Servers[j].Subscription = s.ID
		}
		out[i] = s
	}
	return out
}

// subsOf serves servers as the one subscription of baseSubs.
func subsOf(servers []vpnconfig.Server) func() ([]vpnconfig.Subscription, error) {
	return func() ([]vpnconfig.Subscription, error) {
		subs := baseSubs()
		subs[0].Servers = servers
		return cloneSubs(subs), nil
	}
}
```

   and add to the `Watch` literal in `f.watch()`:

```go
		LoadSubscriptions: func() ([]vpnconfig.Subscription, error) {
			if f.noSubs {
				return nil, nil
			}
			if f.subs == nil {
				f.subs = baseSubs()
			}
			return cloneSubs(f.subs), nil
		},
		SaveSubscription: func(s vpnconfig.Subscription) error {
			for i := range f.subs {
				if f.subs[i].ID == s.ID {
					f.subs[i] = s
					return nil
				}
			}
			f.subs = append(f.subs, s)
			return nil
		},
```

2. `baseCfg()` loses its `SubscriptionURL` line.
3. Delete every line that sets `SaveServers` to a no-op:

```bash
sed -i '/w\.SaveServers = func(\[\]vpnconfig\.Server) error { return nil }/d' server/internal/subwatch/watch_test.go
```

4. In the tests that count or inspect saves — `TestTick_StopDuringTheSubscriptionFetchWritesNothing`, `TestTick_AStopEndsAFetchThatIsStillRunning`, `TestTick_StopWhileThePublicationWaitsWritesNothing`, `TestTick_ImportAndRestoreOnLiveServer`, `TestTick_FailedApplyRetrySkipsImport`, `TestTick_ImportPublishesServersUnderTheConfigLock` — replace the multi-line `w.SaveServers = func(...) {...}` with a wrapper around the fake's own save that keeps the old body:

```go
	save := w.SaveSubscription
	w.SaveSubscription = func(s vpnconfig.Subscription) error {
		saves++ // the old body: saves++, saved++ or savedUnderLock = inUpdate
		return save(s)
	}
```

5. `TestTick_FailedFetchDoesNotSave` becomes `TestTick_AFailedFetchKeepsTheListAndRecordsWhy`: a failed download now writes the subscription's error once per wave. With the wrapper of item 4 counting into `saved`, assert `saved == 1`, `f.subs[0].Error == "cdn down"` and `f.subs[0].Servers == nil`; keep its message assertions.
6. Replace `f.cfg.Xray.SubscriptionURL = ""` with `f.noSubs = true` and `f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/token"` with `f.noSubs = false`. In the tests that did so, "without a link" becomes "without a subscription", in names and comments.
7. Delete `TestTick_ImportDoesNotPublishAfterTheSubscriptionChanged`, `TestTick_WalkAbandonsWhenTheSavedLinkChanges`, `TestTick_NoLiveWalkDoesNotReturnAfterTheLinkChanged` and `TestTick_WalkDoesNotRestoreOnTheOldListAfterTheLinkChanged`: a link no longer changes under a subscription, and `wave_test.go` below pins what a deleted subscription does instead.
8. Every literal `"No live server in the subscription"` becomes `"No live server in any subscription"`.

In `return_test.go` and `reach_test.go`, `w.LoadServers = func() ([]vpnconfig.Server, error) { return X, nil }` becomes `w.LoadSubscriptions = subsOf(X)`, and the unreadable case becomes `w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) { return nil, errors.New("no such file") }` (its case name: `"subscriptions unreadable"`).

- [ ] **Step 3: Write the failing wave tests**

Create `server/internal/subwatch/wave_test.go`:

```go
package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// subOf is a subscription whose servers carry the given names, one address each.
func subOf(id, name, url string, servers ...string) vpnconfig.Subscription {
	s := vpnconfig.Subscription{ID: id, Name: name, URL: url}
	for i, n := range servers {
		s.Servers = append(s.Servers, vpnconfig.Server{Name: n, Address: strings.ToLower(n) + ".example", Port: 443, IPs: []string{fmt.Sprintf("203.0.113.%d", 10+i)}})
	}
	return s
}

// fetchFrom serves each link the list subs holds for it, and fails the links in down.
func fetchFrom(subs []vpnconfig.Subscription, down ...string) func(context.Context, string) ([]vpnconfig.Server, error) {
	lists := map[string][]vpnconfig.Server{}
	for _, s := range subs {
		lists[s.URL] = s.Servers
	}
	return func(_ context.Context, url string) ([]vpnconfig.Server, error) {
		for _, d := range down {
			if d == url {
				return nil, errors.New("HTTP 403")
			}
		}
		return lists[url], nil
	}
}

// walkRig is a failed-over watch whose Generate records each server it is
// handed as "<subscription>/<name>" and names it in active_server, and whose
// probe passes once the walk has written live.
func walkRig(f *fake, live string, events *[]string) *Watch {
	w := runningWatch(f.watch())
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		*events = append(*events, s.Subscription+"/"+s.Name)
		vpnconfig.RecordWalkedServer(f.cfg, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		a := f.cfg.Xray.ActiveServer
		if len(*events) > 0 && a != nil && a.Subscription+"/"+a.Name == live {
			return nil
		}
		return errProbe
	}
	return w
}

func waveFake(subs ...vpnconfig.Subscription) *fake {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0), subs: subs}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: subs[0].ID, Name: subs[0].Servers[0].Name,
		Address: subs[0].Servers[0].Address, Port: 443}
	return f
}

// Alpha fails as a whole. After OwnFirst of its servers the walk turns to
// Beta, and the clients come back on Beta's first live server.
func TestWave_TheWalkReachesTheOtherSubscription(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1", "A2", "A3", "A4", "A5")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1", "B2", "B3")
	f := waveFake(alpha, beta)
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "A2", Address: "a2.example", Port: 443}
	var events []string
	w := walkRig(f, "bbbbbbbb/B2", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})

	w.Tick(context.Background())

	want := []string{"aaaaaaaa/A2", "aaaaaaaa/A1", "aaaaaaaa/A3", "bbbbbbbb/B1", "aaaaaaaa/A4", "bbbbbbbb/B2"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
	if n := countNotes(f.notes, "LAN clients back on Xray; server Beta / B2"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// Every subscription downloads at once: a slow provider does not hold the others back.
func TestWave_EverySubscriptionDownloadsAtOnce(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "", &events)
	started := make(chan string, 2)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		started <- url
		// Each download waits until both have started: one after the other
		// would wait here for ever, and the test's deadline would say so.
		for len(started) < 2 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		return fetchFrom([]vpnconfig.Subscription{alpha, beta})(ctx, url)
	}
	done := make(chan struct{})
	go func() { w.Tick(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the downloads ran one after the other")
	}
}

// When no download arrived - the WAN is the likely cause - there is no walk:
// the message names the subscriptions, the lists stay, each records why, and
// the next wave comes ImportRetry later.
func TestWave_NoWalkWhenEveryDownloadFailed(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "", &events)
	var fetches atomic.Int32 // one goroutine per subscription calls it
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL, beta.URL)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		fetches.Add(1)
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if len(events) != 0 {
		t.Fatalf("walked %v after every download failed", events)
	}
	if n := countNotes(f.notes, "Subscription refresh failed: Alpha, Beta; still on tunnel:ovpnc2"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
	if f.subs[0].Error != "HTTP 403" || len(f.subs[0].Servers) != 1 {
		t.Fatalf("Alpha %+v", f.subs[0])
	}

	f.now = f.now.Add(ImportRetry - time.Second)
	w.Tick(context.Background())
	f.now = f.now.Add(time.Second)
	w.Tick(context.Background())
	if n := fetches.Load(); n != 4 {
		t.Fatalf("fetches %d, want 2 waves of 2", n)
	}
}

// Alpha's panel is down while its servers work: its last list is walked.
func TestWave_AFailedDownloadIsWalkedFromItsLastList(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(beta, alpha)
	var events []string
	w := walkRig(f, "aaaaaaaa/A1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL)

	w.Tick(context.Background())

	if want := []string{"bbbbbbbb/B1", "aaaaaaaa/A1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
	if f.subs[1].Error != "HTTP 403" {
		t.Fatalf("Alpha %+v", f.subs[1])
	}
	if n := countNotes(f.notes, "Subscription refresh failed"); n != 0 {
		t.Fatalf("a partial failure reached Telegram: %v", f.notes)
	}
}

// A refresh from the Web UI that succeeds while the watch's download of the
// same subscription fails is not marked failed by the older download.
func TestWave_AnOlderFailureDoesNotMarkANewerRefresh(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta}, alpha.URL)
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		if url == alpha.URL {
			f.subs[0].Refreshed = time.Unix(1_700_000_100, 0).UTC() // the Web UI refreshed Alpha meanwhile
		}
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if f.subs[0].Error != "" {
		t.Fatalf("Alpha marked failed over a newer refresh: %q", f.subs[0].Error)
	}
}

// A download that decodes to no server keeps the list it would have replaced.
func TestWave_ADownloadWithoutAServerKeepsTheList(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	f := waveFake(alpha)
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { return nil, nil }

	w.Tick(context.Background())

	if len(f.subs[0].Servers) != 1 || f.subs[0].Error == "" {
		t.Fatalf("Alpha %+v", f.subs[0])
	}
}

// A static list has nothing to download, and its servers are walked.
func TestWave_AStaticListIsWalkedWithoutADownload(t *testing.T) {
	gamma := subOf("cccccccc", "Gamma", "", "G1", "G2")
	f := waveFake(gamma)
	var events []string
	w := walkRig(f, "cccccccc/G2", &events)
	fetches := 0
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { fetches++; return nil, nil }

	w.Tick(context.Background())

	if fetches != 0 || !reflect.DeepEqual(events, []string{"cccccccc/G1", "cccccccc/G2"}) || f.cfg.Xray.Failover != nil {
		t.Fatalf("fetches %d, walk %v, failover %+v", fetches, events, f.cfg.Xray.Failover)
	}
}

// Alpha is deleted while its download runs: its list is not published - the
// file is not brought back - and only Beta is walked.
func TestWave_ASubscriptionDeletedWhileItDownloadsIsNotPublished(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	fetch := fetchFrom([]vpnconfig.Subscription{alpha, beta})
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		if url == alpha.URL {
			f.subs = f.subs[1:]
		}
		return fetch(ctx, url)
	}

	w.Tick(context.Background())

	if len(f.subs) != 1 || f.subs[0].ID != "bbbbbbbb" {
		t.Fatalf("files %+v", f.subs)
	}
	if !reflect.DeepEqual(events, []string{"bbbbbbbb/B1"}) {
		t.Fatalf("walk %v", events)
	}
}

// Alpha is deleted while the walk is on its first server: the walk does not
// write Alpha's other servers, and goes on with Beta.
func TestWave_TheWalkSkipsASubscriptionDeletedUnderIt(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1", "A2")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})
	generate := w.Generate
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		ok, seq, err := generate(s, guard)
		if s.Name == "A1" {
			f.subs = f.subs[1:]
		}
		return ok, seq, err
	}

	w.Tick(context.Background())

	if want := []string{"aaaaaaaa/A1", "bbbbbbbb/B1"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the clients are still on the tunnel")
	}
}

// The walk's live server belongs to a subscription deleted after its probe:
// it runs and answers, and the clients come back to it all the same.
func TestWave_ARestoreDoesNotWaitForThePickedServersSubscription(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "A1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "B1")
	f := waveFake(alpha, beta)
	var events []string
	w := walkRig(f, "bbbbbbbb/B1", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})
	probe := w.Probe
	w.Probe = func(ctx context.Context, port int) error {
		err := probe(ctx, port)
		if err == nil {
			f.subs = f.subs[:1] // Beta goes
		}
		return err
	}

	w.Tick(context.Background())

	if f.cfg.Xray.Failover != nil {
		t.Fatal("the restore waited for a deleted subscription")
	}
}

// One provider lists many names on one endpoint. A server whose outbound, with
// its address in place, was tried already is not tried again.
func TestWave_OneEndpointIsTriedOnce(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]}}`)
	alpha := vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: []vpnconfig.Server{
		{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"198.51.100.20"}, Outbound: ob},
		{Name: "Germany-2", Address: "de.example", Port: 443, IPs: []string{"198.51.100.20"}, Outbound: ob},
		{Name: "Germany-3", Address: "de.example", Port: 443, IPs: []string{"198.51.100.21"}, Outbound: ob},
	}}
	f := waveFake(alpha)
	var events []string
	w := walkRig(f, "aaaaaaaa/Germany-3", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha})

	w.Tick(context.Background())

	if want := []string{"aaaaaaaa/Germany-1", "aaaaaaaa/Germany-3"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("walk %v, want %v", events, want)
	}
}

// Both subscriptions name a server Germany-1; the user chose Beta's. The walk
// starts from Beta's, and returns to it when nothing is live - never Alpha's.
func TestWave_TheSameNameInAnotherSubscriptionIsNotTheChosenServer(t *testing.T) {
	alpha := subOf("aaaaaaaa", "Alpha", "https://a.example/s/token", "Germany-1")
	beta := subOf("bbbbbbbb", "Beta", "https://b.example/s/token", "Germany-1")
	f := waveFake(alpha, beta)
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "bbbbbbbb", Name: "Germany-1", Address: "germany-1.example", Port: 443}
	var events []string
	w := walkRig(f, "", &events)
	w.Fetch = fetchFrom([]vpnconfig.Subscription{alpha, beta})

	w.Tick(context.Background())

	if len(events) < 2 || events[0] != "bbbbbbbb/Germany-1" || events[len(events)-1] != "bbbbbbbb/Germany-1" {
		t.Fatalf("walk %v", events)
	}
}

// Xray clients without a subscription: nothing to walk, and no failover of the watch's own.
func TestWave_NoSubscriptionArmsNothing(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0), noSubs: true}
	w := f.watch()

	tickUntilDead(w, f)

	if f.cfg.Xray.Failover != nil || f.applies != 0 {
		t.Fatalf("failover %+v, applies %d with no subscription", f.cfg.Xray.Failover, f.applies)
	}
}

func TestWave_AStaticListArmsTheWatch(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0),
		subs: []vpnconfig.Subscription{subOf("cccccccc", "Gamma", "", "G1")}}
	w := f.watch()

	tickUntilDead(w, f)

	if f.cfg.Xray.Failover == nil {
		t.Fatal("a static list did not arm the watch")
	}
}
```

Append to `return_test.go`:

```go
// A subscription deleted while a return looks at its server: the switch is
// refused under the lock, and nothing is written.
func TestTick_AReturnWritesNoServerOfADeletedSubscription(t *testing.T) {
	r := newReturnRig(returnServers())
	subs := []vpnconfig.Subscription{{ID: "aaaaaaaa", Name: "Alpha", URL: "https://a.example/s/token", Servers: returnServers()}}
	r.f.cfg.Xray.ActiveServer.Subscription = "aaaaaaaa"
	r.f.cfg.Xray.PreferredServer.Subscription = "aaaaaaaa"
	deleted := false
	r.w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) {
		if deleted {
			return nil, nil
		}
		return cloneSubs(subs), nil
	}
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.w.Reachable = func(_ context.Context, ip string, _ int) bool {
		deleted = true // the user deletes Alpha while the return looks at Oslo
		return r.up[ip] || controlUp(ip)
	}

	r.tick()

	if len(r.events) != 0 {
		t.Fatalf("events %v; a server of a deleted subscription was written", r.events)
	}
}
```

- [ ] **Step 4: Run the tests to see them fail**

Run: `cd server && go test ./internal/subwatch/ ./internal/vpnconfig/`
Expected: FAIL — `unknown field LoadSubscriptions in struct literal`, `not enough arguments in call to vpnconfig.Armed`.

- [ ] **Step 5: The watch's fields, arming and helpers**

In `server/internal/subwatch/watch.go`:

Replace the `SaveServers` and `LoadServers` fields with:

```go
	// LoadSubscriptions reads every subscription; SaveSubscription writes one.
	// The watch writes only inside UpdateVPN, through vpnconfig's operations.
	LoadSubscriptions func() ([]vpnconfig.Subscription, error)
	SaveSubscription  func(vpnconfig.Subscription) error
```

Replace the four message constants for refresh and no-live with:

```go
	msgRefreshFailed    = "Subscription refresh failed"
	msgStillOnTunnel    = "; still on tunnel:%s"
	msgRefreshFailedOn  = msgRefreshFailed + msgStillOnTunnel
	msgNoLive           = "No live server in any subscription"
	msgNoLiveOn         = msgNoLive + msgStillOnTunnel
```

In `Tick`, right after the config is loaded, count the subscriptions once and use the count at both `Armed` calls:

```go
	subs := w.subscriptionCount()
	// A restore whose last apply failed is work the watch has started, as a
	// failover is: neither waits for a subscription or for Xray clients.
	if !vpnconfig.Armed(cfg, subs) && !w.pendingApply {
```

```go
	// A pending restore is all an unarmed watch finishes: a failover of its own
	// needs a subscription to walk and Xray clients to move.
	if !vpnconfig.Armed(cfg, subs) {
```

Both `w.announceRestored(activeName(cfg))` become `w.announceRestored(w.activeLabel(cfg))`. Delete `activeName`, and add these helpers at the end of the file:

```go
// loadSubscriptions is every subscription, or none without LoadSubscriptions.
func (w *Watch) loadSubscriptions() ([]vpnconfig.Subscription, error) {
	if w.LoadSubscriptions == nil {
		return nil, nil
	}
	return w.LoadSubscriptions()
}

// loadServers is every server of every subscription, each carrying its
// subscription's id: the list the reach look and the returns search.
func (w *Watch) loadServers() ([]vpnconfig.Server, error) {
	subs, err := w.loadSubscriptions()
	if err != nil {
		return nil, err
	}
	return vpnconfig.AllServers(subs), nil
}

// subscriptionCount is how many subscriptions there are. Subscriptions that
// cannot be read count as none: there is nothing to walk.
func (w *Watch) subscriptionCount() int {
	subs, err := w.loadSubscriptions()
	if err != nil {
		slog.Warn("Failed to read the subscriptions", "error", err)
		return 0
	}
	return len(subs)
}

// files is the subscription files, for vpnconfig's operations.
func (w *Watch) files() vpnconfig.SubscriptionFiles {
	return vpnconfig.SubscriptionFiles{
		Load: w.loadSubscriptions,
		Save: func(s vpnconfig.Subscription) error {
			if w.SaveSubscription == nil {
				return nil
			}
			return w.SaveSubscription(s)
		},
	}
}

// subscriptionHolds reports whether subscription id still exists with link.
// Subscriptions that cannot be read say nothing, and refuse nothing.
func (w *Watch) subscriptionHolds(id, link string) bool {
	subs, err := w.loadSubscriptions()
	if err != nil {
		return true
	}
	i := vpnconfig.FindSubscription(subs, id)
	return i >= 0 && subs[i].URL == link
}

// subscriptionNames maps each subscription's id to its name.
func subscriptionNames(subs []vpnconfig.Subscription) map[string]string {
	names := make(map[string]string, len(subs))
	for _, s := range subs {
		names[s.ID] = s.Name
	}
	return names
}

// label is how a message names a server: "<subscription> / <server>", or the
// server alone when its subscription has no name to show.
func label(names map[string]string, sub, name string) string {
	if n := names[sub]; n != "" {
		return n + " / " + name
	}
	return name
}

// activeLabel names the server active_server records, as the messages name servers.
func (w *Watch) activeLabel(cfg *vpnconfig.VPNDirectorConfig) string {
	if cfg == nil || cfg.Xray.ActiveServer == nil {
		return "unknown"
	}
	a := cfg.Xray.ActiveServer
	subs, _ := w.loadSubscriptions()
	return label(subscriptionNames(subs), a.Subscription, a.Name)
}

// refreshFailedText is the message for a wave in which no download arrived,
// with the names of the subscriptions that failed.
func refreshFailedText(names []string) string {
	var shown []string
	for _, n := range names {
		if n != "" {
			shown = append(shown, n)
		}
	}
	if len(shown) == 0 {
		return msgRefreshFailed
	}
	return msgRefreshFailed + ": " + strings.Join(shown, ", ")
}
```

Replace `notifyRefreshFailed`:

```go
func (w *Watch) notifyRefreshFailed(cfg *vpnconfig.VPNDirectorConfig, names []string) {
	msg := refreshFailedText(names)
	if committedFailover(cfg) {
		msg += fmt.Sprintf(msgStillOnTunnel, failoverTunnel(cfg))
	}
	w.notify(noteRefreshFailed, msg)
}
```

- [ ] **Step 6: The guards**

Replace `walkOwns`, `walkOwnsNow`, `walkGuard`, `endsWalk` and `walkEnded`:

```go
// walkOwns is nil while the walk may still write, and errSuperseded once
// active_server names a server the walk did not record.
func walkOwns(cfg *vpnconfig.VPNDirectorConfig, started, lastRecorded string, expectedSeq int) error {
	if superseded(cfg, started, lastRecorded, expectedSeq) {
		return errSuperseded
	}
	return nil
}

// walkOwnsNow is walkOwns on a fresh read of the config, for the checks of
// the walk that write nothing themselves.
func (w *Watch) walkOwnsNow(started, lastRecorded string, expectedSeq int) error {
	if w.LoadVPN == nil {
		return nil
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return nil
	}
	return walkOwns(cfg, started, lastRecorded, expectedSeq)
}

// walkGuard is the guard every write of the walk carries. It runs under the
// config lock Generate takes, after whatever wait that lock cost: a /stop that
// finished meanwhile refuses the write there - a new config.json and
// active_server on a stopped router would take effect on the next manual
// apply - and so does a Web UI or /xray selection that committed after the
// walk last read the config, instead of being written over. With sub set, the
// server's subscription must still exist with link, the one the walk read: a
// server of a subscription deleted meanwhile is not written
// (vpnconfig.ErrSubscriptionGone).
func (w *Watch) walkGuard(sub, link, started, lastRecorded string, expectedSeq int) func(*vpnconfig.VPNDirectorConfig) error {
	return func(cfg *vpnconfig.VPNDirectorConfig) error {
		if w.stopped() {
			return errStopped
		}
		if err := walkOwns(cfg, started, lastRecorded, expectedSeq); err != nil {
			return err
		}
		if sub != "" && !w.subscriptionHolds(sub, link) {
			return vpnconfig.ErrSubscriptionGone
		}
		return nil
	}
}

// endsWalk is an error after which the walk writes nothing more: a stop or a
// newer selection.
func endsWalk(err error) bool {
	return errors.Is(err, errStopped) || errors.Is(err, errSuperseded)
}

// walkEnded reports whether err ends the walk.
func (w *Watch) walkEnded(err error) bool {
	if !endsWalk(err) {
		return false
	}
	if errors.Is(err, errSuperseded) {
		slog.Info("Subscription walk abandoned; a newer server was selected")
	}
	return true
}
```

In `returnToPreferred`, right after its `if endsWalk(err) { return err }`, add:

```go
	if errors.Is(err, vpnconfig.ErrSubscriptionGone) {
		slog.Info("No return to the preferred server; its subscription was deleted", "server", s.Name)
		return nil
	}
```

- [ ] **Step 7: The wave**

Replace `maybeImportAndPick` with these three functions (the file already imports `net/url`, `strings` and `sync`):

```go
// maybeImportAndPick runs a wave on the import cadence (spec 5.2): every
// subscription with a link downloads at once, each list that arrives is
// published, and the walk looks for a live server across every subscription.
func (w *Watch) maybeImportAndPick(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.Fetch == nil || w.stopped() {
		return
	}
	subs, err := w.loadSubscriptions()
	if err != nil {
		slog.Warn("Failed to read the subscriptions for a refresh", "error", err)
		return
	}
	if len(subs) == 0 {
		// A failover outlives its subscriptions and is still seen through, but
		// nothing refreshes and nothing is walked.
		return
	}
	now := w.Now()
	if !w.lastImport.IsZero() && now.Sub(w.lastImport) < w.importInterval() {
		return
	}
	prevImport := w.lastImport
	w.lastImport = now

	failed, walk := w.refreshSubscriptions(ctx, subs)
	// The downloads block for as long as the slowest host takes. A /stop that
	// finished meanwhile ends the wave before anything more is written, and a
	// wave that did not happen leaves its window to the next one.
	if w.stopped() || ctx.Err() != nil {
		w.lastImport = prevImport
		return
	}
	if !walk {
		// Every download failed - the WAN, most likely - and a walk would cost
		// an Xray restart per server for nothing. An all-dead walk may have
		// backed the waves off to 10/20/30m; a failed download retries every
		// ImportRetry.
		w.importRetry = 0
		w.notifyRefreshFailed(cfg, failed)
		return
	}
	if w.Generate == nil {
		return
	}
	// The lists as published: this wave's downloads, and the last list of each
	// subscription whose download failed - a provider's panel can be down
	// while its servers work.
	subs, err = w.loadSubscriptions()
	if err != nil {
		slog.Warn("Failed to read the subscriptions for the walk", "error", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	w.walk(ctx, cfg, subs)
}

// refreshSubscriptions downloads every subscription of subs that has a link,
// all at once, each within FetchTimeout, and publishes each list that arrived
// under that subscription's guard: it still exists with the link downloaded
// (vpnconfig.RefreshSubscription). A download that fails leaves the list and
// records why, unless a refresh from elsewhere succeeded meanwhile. failed
// names the subscriptions that did not refresh; walk is false when none did
// while some had a link. A stop or an ended context returns at once, writing
// nothing more: the caller looks for both before it reads either result.
func (w *Watch) refreshSubscriptions(ctx context.Context, subs []vpnconfig.Subscription) (failed []string, walk bool) {
	type download struct {
		servers []vpnconfig.Server
		err     error
	}
	results := make([]download, len(subs))
	var wg sync.WaitGroup
	links := 0
	for i, s := range subs {
		if s.Static() {
			continue
		}
		links++
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
			defer cancel()
			servers, err := w.Fetch(fetchCtx, s.URL)
			if err == nil && len(servers) == 0 {
				err = errors.New("no servers")
			}
			results[i] = download{servers, err}
		}()
	}
	wg.Wait()
	if links == 0 {
		return nil, true
	}
	published := 0
	for i, s := range subs {
		if s.Static() {
			continue
		}
		if ctx.Err() != nil || w.stopped() {
			return failed, false
		}
		if err := results[i].err; err != nil {
			// A *url.Error carries the whole subscription URL, token included.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			slog.Warn("Subscription refresh failed", "subscription", s.Name, "error", err)
			failed = append(failed, s.Name)
			rerr := vpnconfig.RecordSubscriptionError(w.update, w.files(), s.ID, s.URL, s.Refreshed, err.Error())
			if rerr != nil && !errors.Is(rerr, errStopped) && !errors.Is(rerr, vpnconfig.ErrSubscriptionGone) {
				slog.Warn("Failed to record why the subscription did not refresh", "subscription", s.Name, "error", rerr)
			}
			continue
		}
		_, err := vpnconfig.RefreshSubscription(w.update, w.files(), s.ID, s.URL, results[i].servers, w.Now())
		switch {
		case err == nil:
			published++
			slog.Info("Subscription refreshed", "subscription", s.Name, "servers", len(results[i].servers))
		case errors.Is(err, errStopped):
			return failed, false
		case errors.Is(err, vpnconfig.ErrSubscriptionGone):
			slog.Info("Subscription refresh dropped; the subscription was deleted while it downloaded", "subscription", s.Name)
		default:
			slog.Warn("Failed to publish the refreshed subscription", "subscription", s.Name, "error", err)
			failed = append(failed, s.Name)
		}
	}
	return failed, published > 0 || len(failed) == 0
}

// walk tries the servers of subs in walkOrder, each address once per outbound
// (dialKey), and brings the clients back to Xray on the first live one. With
// none live it returns Xray to the server the user chose and backs the next
// wave off.
func (w *Watch) walk(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription) {
	var active, chosen *vpnconfig.ActiveServer
	if cfg != nil {
		active = cfg.Xray.ActiveServer
		chosen = cfg.Xray.PreferredServer
	}
	// A walk cut short leaves active_server on a server it was only trying, and
	// preferred_server then keeps the one the user chose.
	if chosen == nil {
		chosen = active
	}
	started := activeID(active)
	startedSeq := vpnconfig.ActiveSeq(active)
	socks := w.socksPort(cfg)
	names := subscriptionNames(subs)
	links := make(map[string]string, len(subs))
	for _, s := range subs {
		links[s.ID] = s.URL
	}
	order, chosenFirst := walkOrder(subs, chosen)
	var preferred *vpnconfig.Server
	if chosenFirst {
		preferred = &order[0]
	}
	tried := 0
	lastGenerated := ""
	// lastRecorded is what active_server names: lastGenerated, unless the record
	// of that config.json failed to save. The checks for a newer selection
	// compare with it, or the walk's own unsaved write reads as someone else's.
	lastRecorded := ""
	// lastSeq is the counter of the walk's own last record, or the one it
	// started from: any other value in the config is someone else's write.
	lastSeq := startedSeq
	gone := map[string]bool{}
	seen := map[string]bool{}
	for _, s := range perAddress(order) {
		if ctx.Err() != nil || w.stopped() {
			return
		}
		if gone[s.Subscription] {
			continue
		}
		if key := dialKey(s); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		generated, seq, err := w.Generate(s, w.walkGuard(s.Subscription, links[s.Subscription], started, lastRecorded, lastSeq))
		if errors.Is(err, vpnconfig.ErrSubscriptionGone) {
			slog.Info("Walk skips a subscription deleted while it runs", "subscription", names[s.Subscription])
			gone[s.Subscription] = true
			continue
		}
		if w.walkEnded(err) {
			return
		}
		if err != nil || !generated {
			slog.Debug("Generating Xray config for server failed", "server", s.Name, "generated", generated, "error", err)
		}
		if !generated {
			continue
		}
		lastGenerated = serverID(s)
		if err == nil {
			lastRecorded = lastGenerated
		}
		lastSeq = seq
		tried++
		if ctx.Err() != nil {
			return
		}
		if err := w.restartXray(); err != nil {
			if errors.Is(err, errStopped) {
				return
			}
			slog.Debug("Xray restart failed", "server", s.Name, "error", err)
			continue
		}
		w.AfterRestart(SettleAfterRestart)
		if err := w.Probe(ctx, socks); err != nil {
			slog.Debug("Subscription server probe failed", "server", s.Name, "ips", s.IPs, "error", err)
			continue
		}
		name := label(names, s.Subscription, s.Name)
		slog.Info("Subscription server picked", "server", name, "ips", s.IPs)
		w.lastPicked = &s
		if w.walkEnded(w.walkOwnsNow(started, lastRecorded, lastSeq)) {
			return
		}
		// Only a committed failover left Xray. Staged clients never left, so
		// "back on Xray" would be a false message. The restore does not look
		// at the subscription (spec 5.4): the server runs and answers.
		done, committed, refused := w.commitRestore(cfg, w.walkGuard("", "", started, lastRecorded, lastSeq))
		if w.walkEnded(refused) || !done {
			return
		}
		if committed {
			w.announceRestored(name)
		} else {
			w.notify(noteRestored, fmt.Sprintf(msgPicked, name))
		}
		return
	}
	if w.stopped() {
		return
	}
	slog.Info("No live server in any subscription", "tried", tried)
	if w.walkEnded(w.walkOwnsNow(started, lastRecorded, lastSeq)) {
		return
	}
	if preferred != nil && lastGenerated != "" && lastGenerated != serverID(*preferred) {
		guard := w.walkGuard(preferred.Subscription, links[preferred.Subscription], started, lastRecorded, lastSeq)
		if w.walkEnded(w.returnToPreferred(*preferred, guard)) {
			return
		}
		if w.stopped() {
			return
		}
	}
	if tried > 0 {
		// Every tried server cost an Xray restart and a config write; on a large
		// all-dead subscription back-to-back waves would never stop doing that.
		w.lastImport = w.Now()
		w.importRetry = min(2*w.importInterval(), ImportRetryMax)
		slog.Info("Next subscription refresh backed off", "after", w.importRetry)
	}
	w.notifyNoLive(cfg)
}
```

`net/url` stays in the imports for the `*url.Error` above.

- [ ] **Step 8: The returns and the reach look**

In `server/internal/subwatch/return.go`:

- In `maybeReturn`, the nil check becomes `if w.LoadSubscriptions == nil || w.Reachable == nil || w.Generate == nil {`, and the "already there" check compares the subscription:

```go
	if active == nil || (active.Subscription == preferred.Subscription && active.Name == preferred.Name) {
		return
	}
```

- Replace its `servers, err := w.LoadServers()` block with:

```go
	subs, err := w.loadSubscriptions()
	if err != nil {
		slog.Warn("Failed to read the subscriptions for the return to the preferred server", "error", err)
		return
	}
	servers := vpnconfig.AllServers(subs)
```

   and its last line with `w.tryReturn(ctx, cfg, subs, servers, candidates)`.

- `tryReturn` takes `subs []vpnconfig.Subscription` after `cfg`, builds the switcher from it, and names the server in its message:

```go
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, servers, candidates []vpnconfig.Server) {
	before := cfg.Xray.ActiveServer
	links := make(map[string]string, len(subs))
	for _, s := range subs {
		links[s.ID] = s.URL
	}
	names := subscriptionNames(subs)
	sw := &switcher{
		w:       w,
		links:   links,
		started: activeID(before),
		seq:     vpnconfig.ActiveSeq(before),
		socks:   w.socksPort(cfg),
	}
```

   In its body: the forward loop calls `sw.to(ctx, c, true)`, the way back `sw.to(ctx, c, false)`; the look before the message is `endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq))`; the message is `fmt.Sprintf(msgReturned, label(names, c.Subscription, c.Name))`.

- `switcher` loses `rawURL` and gains `links map[string]string // subscription id -> the link the attempt read`. `to` becomes:

```go
// to writes c as the running server, restarts Xray and probes it. With holds,
// c's subscription must still exist with the link the attempt read: a switch
// to a server of a subscription deleted meanwhile ends the attempt. The way
// back to the server that ran before is not held to that - it is the server
// that ran. live is a probe that passed; ended is a write the guard refused, a
// restart a stop skipped, or a context or stop that ended the attempt, after
// which nothing more may be written.
func (s *switcher) to(ctx context.Context, c vpnconfig.Server, holds bool) (live, ended bool) {
	w := s.w
	if ctx.Err() != nil || w.stopped() {
		return false, true
	}
	sub, link := "", ""
	if holds {
		sub, link = c.Subscription, s.links[c.Subscription]
	}
	generated, seq, err := w.Generate(c, w.walkGuard(sub, link, s.started, s.lastRecorded, s.seq))
	if endsWalk(err) || errors.Is(err, vpnconfig.ErrSubscriptionGone) {
		return false, true
	}
	if !generated {
		slog.Warn("Generating Xray config for server failed", "server", c.Name, "error", err)
		return false, false
	}
	s.wrote = true
	if err == nil {
		s.lastRecorded = serverID(c)
	}
	s.seq = seq
	if ctx.Err() != nil {
		return false, true
	}
	if err := w.restartXray(); err != nil {
		if errors.Is(err, errStopped) {
			return false, true
		}
		slog.Warn("Xray restart failed", "server", c.Name, "error", err)
		return false, false
	}
	w.AfterRestart(SettleAfterRestart)
	if err := w.Probe(ctx, s.socks); err != nil {
		// A probe a cancelled context or a stop cut short says nothing about
		// the server.
		if ctx.Err() != nil || w.stopped() {
			return false, true
		}
		slog.Info("Server probe failed", "server", c.Name, "ips", c.IPs, "error", err)
		return false, false
	}
	return true, false
}
```

In `server/internal/subwatch/reach.go`, `activeServerDown` starts:

```go
	if w.LoadSubscriptions == nil || w.Reachable == nil || cfg == nil || cfg.Xray.ActiveServer == nil {
		return false
	}
	servers, err := w.loadServers()
```

and its doc comment says "without LoadSubscriptions" and "no subscription lists it" where it says servers.json.

In `server/internal/bot/bot.go`, the watch literal replaces its two lines

```go
			SaveServers:  configSvc.SaveServers,
			LoadServers:  configSvc.LoadServers,
```

with

```go
			LoadSubscriptions: configSvc.LoadSubscriptions,
			SaveSubscription:  configSvc.SaveSubscription,
```

- [ ] **Step 9: Run the tests to see them pass**

Run: `cd server && go vet ./... && go test ./... && go test -race ./internal/subwatch/ ./internal/service/ && gofmt -l .`
Expected: PASS everywhere. The race run covers the parallel downloads; a race it reports inside a test fake that predates this work is fixed in the fake (an `atomic` counter), never by giving up the parallel downloads. `gofmt -l` lists at most the two files that were unformatted on `master`.

- [ ] **Step 10: Commit**

```bash
git add server/internal/vpnconfig/failover.go server/internal/vpnconfig/failover_test.go server/internal/subwatch server/internal/bot/bot.go
git commit -m "feat(subwatch): refresh every subscription at once and walk them all"
```

---

### Task 12: Remove the single link and `servers.json` from the Go code

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`XrayConfig.SubscriptionURL`, `LoadServers`, `SaveServers` go), `server/internal/vpnconfig/vpnconfig_test.go`
- Modify: `server/internal/vpnconfig/publish.go` (only `ErrServersSaved`, `savedError`, `ServersSaved` stay), `server/internal/vpnconfig/publish_test.go`
- Modify: `server/internal/vpnconfig/failover_test.go`
- Delete: `server/internal/service/publish.go`, `server/internal/service/publish_test.go`
- Modify: `server/internal/service/config.go`, `server/internal/service/interfaces.go`, `server/internal/service/config_test.go`
- Modify: `server/internal/webapi/handler_logs.go`, `server/internal/webapi/handler_logs_test.go`, `server/internal/webapi/handler_servers_test.go`
- Modify (drop `SaveServers` from the fakes): `server/internal/webapi/test_helpers_test.go`, `server/internal/handler/status_test.go`, `server/internal/handler/clients_test.go`, `server/internal/handler/substore_test.go`, `server/internal/wizard/server_test.go`, `server/internal/wizard/apply_test.go`, `server/internal/service/activeserver_test.go`, `server/internal/service/subscriptions_test.go`
- Modify: every Go comment that still names `servers.json` as the list (see Step 5)

**Interfaces:**
- Consumes: nothing new.
- Produces: `ConfigStore` without `SaveServers`; `vpnconfig.XrayConfig` without `SubscriptionURL`. A daemon's next write of the config drops the `subscription_url` key an earlier release left, and `/api/config` never serializes it (spec 3.4).

- [ ] **Step 1: Write the failing test**

Append to `server/internal/vpnconfig/vpnconfig_test.go`:

```go
// An earlier release kept the one subscription link in xray.subscription_url.
// Nothing reads it now, and the next write of the config by a daemon drops it:
// a link carries its subscription's token.
func TestSaveVPNDirectorConfig_DropsTheLinkOfEarlierReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpn-director.json")
	if err := os.WriteFile(path, []byte(`{"data_dir":"/data","xray":{"clients":["192.168.1.8"],"subscription_url":"https://sub.example.com/s/secret-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "subscription_url") || strings.Contains(string(data), "secret-token") {
		t.Fatalf("the old link survived a write: %s", data)
	}
	if !strings.Contains(string(data), "192.168.1.8") {
		t.Fatalf("the write lost the config: %s", data)
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `cd server && go test ./internal/vpnconfig/ -run DropsTheLink`
Expected: FAIL — the file still carries `subscription_url`.

- [ ] **Step 3: Remove the single-list code**

1. `vpnconfig.go`: delete the `SubscriptionURL` field of `XrayConfig`, and the functions `LoadServers(path)` and `SaveServers(path, servers)`.
2. `publish.go`: delete `ErrSubscriptionChanged`, `SubscriptionUnchanged`, `ErrSaveServers` and `PublishServers`. What stays reads:

```go
package vpnconfig

import "errors"

// ErrServersSaved marks a failure that came after a subscription file was
// written - or removed - by one of the operations in subops.go: the list is
// out, and only the config it is read against is not. Every other failure
// changed nothing, so an importer says "saved" for this one only.
var ErrServersSaved = errors.New("servers saved")

// savedError is a failure after the file was written. It is ErrServersSaved
// to errors.Is and reads as its cause, which is what the importers show.
type savedError struct{ cause error }

func (e *savedError) Error() string   { return e.cause.Error() }
func (e *savedError) Unwrap() []error { return []error{ErrServersSaved, e.cause} }

// ServersSaved marks err as a failure that came after the file was written.
func ServersSaved(err error) error { return &savedError{cause: err} }
```

3. Delete `server/internal/service/publish.go` and `server/internal/service/publish_test.go`.
4. `service/config.go`: delete `SaveServers`. `service/interfaces.go`: delete `SaveServers([]vpnconfig.Server) error` from `ConfigStore`.
5. `webapi/handler_logs.go`: delete `redacted.Xray.SubscriptionURL = ""` and say in the comment above the copy that only `jwt_secret` needs blanking now — subscription links live in their own files.
6. Every fake's `SaveServers` method goes, with the fields only it used (`savedServers` in `webapi/test_helpers_test.go`; `serversErr`, `savedServers`, `serversUnderLock` in `service/activeserver_test.go` once no test reads them).

- [ ] **Step 4: Remove the tests of what went**

- `vpnconfig/vpnconfig_test.go`: delete the tests of `LoadServers`/`SaveServers` (the first five tests of the file, which open `servers.json` paths). In the test near line 564 that writes a server list only to look at the file mode, write a subscription instead: `SaveSubscription(dir, Subscription{ID: "0a1b2c3d", Name: "Alpha"})` and stat `dir/0a1b2c3d.json`. `TestXrayConfig_PersistsSubscriptionURLAndFailover` becomes `TestXrayConfig_PersistsFailover`, without the URL.
- `vpnconfig/publish_test.go`: keep `TestRecordActiveServer_CountsWrites`; delete the `TestPublishServers_*` tests and `TestSubscriptionUnchanged`.
- `vpnconfig/failover_test.go`: drop the `SubscriptionURL` lines left from Task 11; `TestXrayConfig_OmitsSubscriptionURLAndFailoverWhenEmpty` becomes `TestXrayConfig_OmitsFailoverWhenEmpty`.
- `service/config_test.go`: delete `TestConfigService_SaveServers_CreatesDir`.
- `webapi/handler_servers_test.go`: delete the `TestSyncXrayServers_*` tests (five of them, `TestSyncXrayServers_WritesSubscriptionURL` included).
- `webapi/handler_logs_test.go`: drop the `SubscriptionURL` value from the test config and the assertion that blanks it.
- In every file this step touches, drop the imports only the deleted tests used; `go vet` names any left over.

- [ ] **Step 5: Leave no stale mention**

Run from the repository root:

```bash
grep -rn "SubscriptionURL\|SaveServers\|PublishServers\|PublishImport\|SubscriptionUnchanged\|ErrSubscriptionChanged\|ErrSaveServers" server/ --include=*.go
grep -rn "servers\.json" server/ --include=*.go
```

Expected: the first prints nothing. The second prints only `RemoveLegacyServers` and its callers and tests (`vpnconfig/substore.go`, `service/config.go` and their tests); every other comment that still calls `servers.json` the list — in `subwatch/watch.go`, `subwatch/reach.go`, `subwatch/return.go`, `service/activeserver.go` — now says "the subscription files" or "its subscription's list".

- [ ] **Step 6: Run the whole suite**

Run: `cd server && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: PASS; `gofmt -l` lists at most the two files that were unformatted on `master`.

- [ ] **Step 7: Commit**

```bash
git add -A server/
git commit -m "refactor: drop the single subscription link and servers.json"
```

---

### Task 13: `lib/substore.sh` — the shell twin of the store

**Files:**
- Create: `router/opt/vpn-director/lib/substore.sh`
- Create: `router/test/unit/substore.bats`
- Modify: `router/files.manifest`

**Interfaces:**
- Consumes: `testdata/substore/*.json` (Task 1).
- Produces (all self-contained, no `lib/common.sh`):
  - `SUBSTORE_MAX=10`, `SUBSTORE_NAME_MAX=32`
  - `substore_dir <data_dir>`, `substore_valid_id <id>`, `substore_list <dir>` (a JSON array, ordered), `substore_new_id <dir>`
  - `substore_clean_name <name>` (prints the name or fails with the reason on stderr), `substore_name_taken <subs_json> <name> [except_id]`, `substore_default_name <subs_json> <base>`
  - `substore_ips <subs_json>`, `substore_write <dir> <subscription_json>`, `substore_delete <dir> <id>`
  - `substore_lock <config>`, `substore_unlock`, `substore_sync_config <config> <subs_json> [jq filter]`

- [ ] **Step 1: Write the failing tests**

Create `router/test/unit/substore.bats`:

```bash
#!/usr/bin/env bats
load '../test_helper'

# testdata/substore is read by the Go store too
# (server/internal/vpnconfig/substore_test.go): both list the same
# subscriptions in the same order, and apply the same name rules.
FIXTURES="$PROJECT_ROOT/../testdata/substore"

setup() {
    source "$LIB_DIR/substore.sh"
}

@test "substore_list: the shared fixtures, in the order the daemons list them" {
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$FIXTURES' | jq -c '[.[] | [.id, .name]]'"
    assert_success
    assert_output '[["1b2c3d4e","Beta"],["0a1b2c3d","Alpha"],["2c3d4e5f","Gamma"]]'
}

@test "substore_list: no directory is no subscription" {
    run substore_list "$BATS_TEST_TMPDIR/none"
    assert_success
    assert_output '[]'
}

# A temp file of an atomic write, a backup, a directory and a file whose id is
# not its name are no subscription; a broken file is skipped with a warning.
@test "substore_list: skips what is no subscription" {
    local dir="$BATS_TEST_TMPDIR/subscriptions" good
    mkdir -p "$dir/3d4e5f6a.json"
    good='{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    printf '%s' "$good" > "$dir/0a1b2c3d.json"
    printf '%s' "$good" > "$dir/.0a1b2c3d.json.tmp-123"
    printf '%s' "$good" > "$dir/0a1b2c3d.json.Ab12Cd"
    printf '%s' "$good" > "$dir/backup.json"
    printf '%s' "${good/0a1b2c3d/ffffffff}" > "$dir/2c3d4e5f.json"
    printf '{"id":"1b2c3d4e",' > "$dir/1b2c3d4e.json"

    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' 2>'$BATS_TEST_TMPDIR/err' | jq -c '[.[].id]'"

    assert_success
    assert_output '["0a1b2c3d"]'
    run cat "$BATS_TEST_TMPDIR/err"
    assert_output --partial "Skipping subscription file 1b2c3d4e.json"
    assert_output --partial "Skipping subscription file 2c3d4e5f.json"
}

@test "substore_valid_id: 8 lowercase hex digits" {
    substore_valid_id 0a1b2c3d
    ! substore_valid_id 0A1B2C3D
    ! substore_valid_id 0a1b2c3
    ! substore_valid_id 0a1b2c3d0
    ! substore_valid_id ../0a1b2
}

@test "substore_new_id: a valid id no file has" {
    local id
    id=$(substore_new_id "$BATS_TEST_TMPDIR")
    substore_valid_id "$id"
    [[ ! -e $BATS_TEST_TMPDIR/$id.json ]]
}

# The same table as TestCleanSubscriptionName in Go.
@test "substore_clean_name: the daemons' rules" {
    local long
    long=$(printf 'я%.0s' $(seq 1 32))
    [[ $(substore_clean_name '  Alpha  ') == "Alpha" ]]
    [[ $(substore_clean_name 'Бета VPN') == "Бета VPN" ]]
    [[ $(substore_clean_name "$long") == "$long" ]]
    run substore_clean_name "${long}я"
    assert_failure
    assert_output --partial "longer than 32"
    run substore_clean_name '   '
    assert_failure
    run substore_clean_name $'Al\tpha'
    assert_failure
    run substore_clean_name $'\tAlpha'
    assert_failure
    run substore_clean_name $'Al\xc2\x85pha'   # U+0085, a C1 control, as UTF-8 bytes
    assert_failure
    run substore_clean_name $'Al\x7fpha'
    assert_failure
}

# The same table as TestSubscriptionNameTaken_FoldsASCIIOnly in Go.
@test "substore_name_taken: ASCII case folding only" {
    local subs='[{"id":"0a1b2c3d","name":"Beta"},{"id":"1b2c3d4e","name":"Бета"}]'
    substore_name_taken "$subs" beta
    substore_name_taken "$subs" BETA
    ! substore_name_taken "$subs" beta 0a1b2c3d
    ! substore_name_taken "$subs" бета
    substore_name_taken "$subs" Бета
    ! substore_name_taken "$subs" Gamma
}

# The same table as TestDefaultSubscriptionName in Go.
@test "substore_default_name: the daemons' choice" {
    local host40 a31
    host40="$(printf 'a%.0s' $(seq 1 36)).com"
    a31=$(printf 'a%.0s' $(seq 1 31))
    [[ $(substore_default_name '[]' sub.example.com) == "sub.example.com" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"Sub.Example.com"}]' sub.example.com) == "sub.example.com-2" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"sub.example.com"},{"id":"00000001","name":"sub.example.com-2"}]' sub.example.com) == "sub.example.com-3" ]]
    [[ $(substore_default_name '[]' "$host40") == "${host40:0:32}" ]]
    [[ $(substore_default_name "[{\"id\":\"00000000\",\"name\":\"${host40:0:32}\"}]" "$host40") == "${host40:0:30}-2" ]]
    [[ $(substore_default_name '[]' "$a31 bc") == "$a31" ]]
    [[ $(substore_default_name '[]' $'  list\x07.txt ') == "list.txt" ]]
    [[ $(substore_default_name '[]' '') == "subscription" ]]
    [[ $(substore_default_name '[]' $' \x01 ') == "subscription" ]]
}

@test "substore_ips: every address of every server, sorted, once" {
    run substore_ips "$(substore_list "$FIXTURES")"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"]'
}

@test "substore_write: a file only its owner reads, in a directory only its owner opens" {
    local dir="$BATS_TEST_TMPDIR/data/subscriptions"
    substore_write "$dir" '{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    run stat -c %a "$dir" "$dir/0a1b2c3d.json"
    assert_output $'700\n600'
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' | jq -r '.[0].name'"
    assert_output "Alpha"
    run ls -A "$dir"
    assert_output "0a1b2c3d.json"
}

@test "substore_write: refuses an id the store would not read" {
    run substore_write "$BATS_TEST_TMPDIR" '{"id":"../x","name":"X"}'
    assert_failure
}

@test "substore_delete: removes the file of that id only" {
    local dir="$BATS_TEST_TMPDIR/s"
    mkdir -p "$dir"
    printf '{}' > "$dir/0a1b2c3d.json"
    printf '{}' > "$dir/1b2c3d4e.json"
    substore_delete "$dir" 0a1b2c3d
    run ls -A "$dir"
    assert_output "1b2c3d4e.json"
}

@test "substore_lock: waits for another holder, then gives up" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{}' > "$config"
    flock "$BATS_TEST_TMPDIR/.vpn-director.json.lock" sleep 30 3>&- &
    local holder=$!
    sleep 0.5
    VPD_CONFIG_LOCK_WAIT=1 run substore_lock "$config"
    pkill -P "$holder" 2>/dev/null || true
    kill "$holder" 2>/dev/null || true
    assert_failure
}

@test "substore_sync_config: xray.servers in step, the old link gone, the filter applied" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '%s' '{"webui":{"jwt_secret":"s"},"xray":{"clients":["192.168.1.8"],"servers":["9.9.9.9"],"subscription_url":"https://old.example/s/t","preferred_server":{"subscription":"0a1b2c3d","name":"Oslo"}}}' > "$config"

    substore_sync_config "$config" "$(substore_list "$FIXTURES")" 'if (.xray.preferred_server.subscription // "") == "0a1b2c3d" then del(.xray.preferred_server) else . end'

    run jq -c '[.xray.servers, (.xray | has("subscription_url")), (.xray | has("preferred_server")), .webui.jwt_secret, .xray.clients]' "$config"
    assert_output '[["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"],false,false,"s",["192.168.1.8"]]'
    run stat -c %a "$config"
    assert_output "600"
}

@test "substore_sync_config: no config yet is nothing to keep in step" {
    run substore_sync_config "$BATS_TEST_TMPDIR/none.json" '[]'
    assert_success
    [[ ! -e $BATS_TEST_TMPDIR/none.json ]]
}

# Entware's jq is built without oniguruma: a regex builtin works on a
# workstation and fails on the router.
@test "lib/substore.sh: its jq uses no regex builtin" {
    run grep -nE '(^|[^a-zA-Z_])(test|match|capture|scan|splits|sub|gsub)\(|split\([^)]*;' "$LIB_DIR/substore.sh"
    assert_failure
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats unit/substore.bats`
Expected: FAIL — `lib/substore.sh: No such file or directory`.

- [ ] **Step 3: Write the library**

Create `router/opt/vpn-director/lib/substore.sh`:

```bash
#!/usr/bin/env bash
# shellcheck shell=bash

###############################################################################
# substore.sh - the subscription files: <data_dir>/subscriptions/<id>.json, one
# per subscription, holding its link, its status and its servers. The Go
# daemons read and write the same files (server/internal/vpnconfig/substore.go)
# under the same lock, and apply the same rules to ids and names.
#
# Self-contained: configure.sh sources it without lib/common.sh, so nothing
# here calls log(); warnings go to stderr. Entware's jq is built without
# oniguruma, so nothing here uses a regex builtin.
###############################################################################

# The most subscriptions a router keeps, and the longest name in characters.
SUBSTORE_MAX=10
SUBSTORE_NAME_MAX=32

# substore_dir <data_dir> - where the data directory keeps the subscriptions.
substore_dir() {
    printf '%s/subscriptions\n' "$1"
}

# substore_valid_id <id> - an id of 8 lowercase hex digits, the only names the
# store reads or writes.
substore_valid_id() {
    [[ ${#1} -eq 8 && $1 != *[!0123456789abcdef]* ]]
}

# substore_list <dir> - every subscription as one JSON array, ordered by added,
# then by id, as the daemons order them. A file named anything but
# <8 hex digits>.json is no subscription - a temp file of an atomic write
# among them; one that does not parse, or whose id is not its name, is skipped
# with a warning.
substore_list() {
    local dir=$1 file name id sub
    local -a found=()
    if [[ -d $dir ]]; then
        for file in "$dir"/*.json; do
            [[ -f $file ]] || continue
            name=${file##*/}
            id=${name%.json}
            substore_valid_id "$id" || continue
            if ! sub=$(jq -ce --arg id "$id" 'select(type == "object" and .id == $id)' "$file" 2>/dev/null) || [[ -z $sub ]]; then
                printf 'Skipping subscription file %s: it does not parse, or its id is not its name\n' "$name" >&2
                continue
            fi
            found+=("$sub")
        done
    fi
    if [[ ${#found[@]} -eq 0 ]]; then
        printf '[]\n'
        return 0
    fi
    printf '%s\n' "${found[@]}" | jq -cs 'sort_by(.added, .id)'
}

# substore_new_id <dir> - a random id no file in <dir> has yet.
substore_new_id() {
    local id
    while :; do
        id=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
        if substore_valid_id "$id" && [[ ! -e $1/$id.json ]]; then
            break
        fi
    done
    printf '%s\n' "$id"
}

# substore_clean_name <name> - <name> without the spaces around it, when what is
# left is 1 to 32 characters and none of them a control character (C0, DEL,
# C1); otherwise it fails with the reason on stderr.
# vpnconfig.CleanSubscriptionName applies the same rules.
substore_clean_name() {
    local name=$1 verdict
    name=${name#"${name%%[! ]*}"}
    name=${name%"${name##*[! ]}"}
    verdict=$(jq -rn --arg n "$name" --argjson max "$SUBSTORE_NAME_MAX" '
        ($n | explode) as $c
        | if ($c | length) == 0 then "empty"
          elif ($c | length) > $max then "long"
          elif ($c | any(. < 32 or . == 127 or (. >= 128 and . <= 159))) then "control"
          else "ok" end')
    case $verdict in
        ok) printf '%s\n' "$name" ;;
        empty) printf 'The name is empty\n' >&2; return 1 ;;
        long) printf 'The name is longer than %d characters\n' "$SUBSTORE_NAME_MAX" >&2; return 1 ;;
        *) printf 'The name has a control character\n' >&2; return 1 ;;
    esac
}

# substore_name_taken <subs_json> <name> [except_id] - whether a subscription
# other than except_id has <name> under ASCII case folding: jq's
# ascii_downcase, the fold the daemons apply too.
substore_name_taken() {
    jq -e --arg n "$2" --arg except "${3:-}" \
        'any(.[]; .id != $except and ((.name | ascii_downcase) == ($n | ascii_downcase)))' <<< "$1" >/dev/null
}

# substore_default_name <subs_json> <base> - <base> (a link's host, a file's
# name) cut to 32 characters, or <base>-2, <base>-3 and so on, cut so the suffix
# fits, whichever no subscription has yet. Control characters and the spaces
# around <base> go first; an empty <base> is "subscription".
# vpnconfig.DefaultSubscriptionName makes the same choice.
substore_default_name() {
    jq -rn --argjson subs "$1" --arg base "$2" --argjson max "$SUBSTORE_NAME_MAX" '
        def trim_spaces: explode
            | until(length == 0 or .[0] != 32; .[1:])
            | until(length == 0 or .[-1] != 32; .[:-1])
            | implode;
        ($base | explode | map(select(. >= 32 and (. < 127 or . > 159))) | implode | trim_spaces) as $b0
        | (if $b0 == "" then "subscription" else $b0 end) as $b
        | [$subs[].name | ascii_downcase] as $taken
        | first(range(1; $max + 100) as $n
            | (if $n == 1 then "" else "-\($n)" end) as $suffix
            | (($b | .[0:($max - ($suffix | length))]) | trim_spaces) + $suffix
            | select(. as $cand | ($taken | any(. == ($cand | ascii_downcase))) | not))'
}

# substore_ips <subs_json> - xray.servers for these subscriptions: every address
# of every server, sorted, each once - the set TPROXY_BYPASS takes.
substore_ips() {
    jq -c '[.[].servers[]?.ips[]? | select(. != "")] | unique' <<< "$1"
}

# substore_write <dir> <subscription_json> - writes <dir>/<id>.json: a temp file
# beside it, mode 600 - it holds the link and every server's credentials - then
# a rename, so a reader sees the old file or the new one. The caller holds the
# config lock.
substore_write() {
    local dir=$1 sub=$2 id tmp
    id=$(jq -r '.id' <<< "$sub")
    if ! substore_valid_id "$id"; then
        printf 'Invalid subscription id: %s\n' "$id" >&2
        return 1
    fi
    mkdir -p "$dir"
    chmod 700 "$dir"
    tmp=$(mktemp "$dir/$id.json.XXXXXX")
    if ! jq . <<< "$sub" > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod 600 "$tmp"
    mv -f "$tmp" "$dir/$id.json"
}

# substore_delete <dir> <id> - removes <dir>/<id>.json. The caller holds the lock.
substore_delete() {
    substore_valid_id "$2" || return 1
    rm -f "$1/$2.json"
}

# substore_lock <config> - takes the lock the daemons and configure.sh take
# around vpn-director.json, on FD 9, waiting up to VPD_CONFIG_LOCK_WAIT seconds
# (30). BusyBox flock has no -w, hence the loop.
substore_lock() {
    local waited=0
    exec 9>"${1%/*}/.${1##*/}.lock"
    until flock -n 9; do
        if [[ $waited -ge ${VPD_CONFIG_LOCK_WAIT:-30} ]]; then
            exec 9>&-
            return 1
        fi
        [[ $waited -eq 0 ]] && printf 'Waiting for the config lock...\n' >&2
        sleep 1
        waited=$((waited + 1))
    done
}

# substore_unlock - releases what substore_lock took.
substore_unlock() {
    flock -u 9
    exec 9>&-
}

# substore_sync_config <config> <subs_json> [jq filter] - under the lock the
# caller holds: xray.servers for <subs_json>, the single link of earlier
# releases gone, and <filter> on top. With no config yet - before configure.sh
# has run - there is nothing to keep in step.
substore_sync_config() {
    local config=$1 subs=$2 filter=${3:-.} ips tmp
    [[ -f $config ]] || return 0
    ips=$(substore_ips "$subs")
    tmp=$(mktemp "$config.XXXXXX")
    if ! jq --argjson ips "$ips" ".xray.servers = \$ips | del(.xray.subscription_url) | $filter" "$config" > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod 600 "$tmp"
    mv -f "$tmp" "$config"
}
```

In `router/files.manifest`, after the `lib/subscription.sh` line, add:

```
common   router/opt/vpn-director/lib/substore.sh
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats unit/substore.bats && shellcheck ../opt/vpn-director/lib/substore.sh`
Expected: every test passes; shellcheck prints nothing.

Then the manifest test of the updater, which fails for a file under `router/` the manifest does not list:
Run: `cd server && go test ./internal/updater/ -run Manifest`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/substore.sh router/test/unit/substore.bats router/files.manifest
git commit -m "feat(shell): lib/substore.sh, the subscription files for the shell"
```

---

### Task 14: `import_server_list.sh` — the subscription menu

**Files:**
- Modify: `router/opt/vpn-director/import_server_list.sh`
- Modify: `router/test/import_server_list.bats`

**Interfaces:**
- Consumes: Task 13's `substore_*`; the existing `lib/subscription.sh`, `resolve_ip`, `log`, `tmp_file`.
- Produces: the menu of spec 8.1. Functions the tests call: `step_get_subscription` (prompts, then `fetch_subscription`), `fetch_subscription <link or path>`, `step_parse_servers` (fills `$SERVERS_TMP`, reusing a path the caller set), `fail <message>`, `run_action <function> [args]` (sets `ACTION_RC`), `publish_add`, `refresh_one`, `publish_refresh`, `record_refresh_error`, `menu`.

The shell refreshes one subscription after another; the daemons refresh in parallel. A menu action runs in a subshell, so its `exit 1` ends the action and not the menu; a first run with no subscription ends the script with the action's status, as today's import does.

- [ ] **Step 1: Write the failing tests**

In `router/test/import_server_list.bats`, keep the `step_parse_servers` and `step_get_subscription` tests of the first section and the helpers `load_import_into`, `write_list_file`, `write_oversized_list`, `serve_list_file`, `hold_config_lock` and `release_config_lock`. Replace `run_import` with one that answers the prompts a line per argument:

```bash
# run_import runs import_server_list.sh the way a user does: each argument is
# the answer to one prompt, and the end of the input quits the menu.
run_import() {
    run env IMPORT_TEST_MODE=0 bash "$SCRIPTS_DIR/import_server_list.sh" < <(printf '%s\n' "$@")
}

# subs_dir is where the scratch router keeps its subscriptions.
subs_dir() {
    printf '%s/data/subscriptions' "$VPD_DIR"
}

# write_config writes a config the way configure.sh leaves one: a data
# directory, the Web UI's secret and one Xray client.
write_config() {
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data, webui: {jwt_secret: "secret"}, xray: {clients: ["192.168.50.10"]}}' > "$VPD_CONFIG"
}

# write_list_file_named <file> <link...> writes another list file.
write_list_file_named() {
    local file=$1
    shift
    printf '%s\n' "$@" > "$file"
}

# serve_failing_download puts a curl first on PATH that fails every download,
# as a host that answers 403 does.
serve_failing_download() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/sh\nexit 22\n' > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}
```

Replace every test of the "Publishing" section with:

```bash
# A first run has no subscription: the menu opens on Add. The list becomes a
# subscription file, 600 in a 700 directory, and xray.servers follows it.
@test "import_server_list.sh: a first run adds a subscription and brings the config in step" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -c '[.name, has(\"url\"), [.servers[].name]]' '$(subs_dir)'/*.json"
    assert_output '["servers",false,["A","B","C"]]'
    run jq -c '[.xray.servers, .xray.clients, .webui.jwt_secret]' "$VPD_CONFIG"
    assert_output '[["1.2.3.4","5.6.7.8"],["192.168.50.10"],"secret"]'
    run bash -c "stat -c %a '$(subs_dir)' '$(subs_dir)'/*.json"
    assert_output $'700\n600'
}

@test "import_server_list.sh: an https link is saved and names the subscription after its host" {
    load_import_into
    write_config
    write_list_file
    serve_list_file

    run_import "https://cdn.example/s/b" ""

    assert_success
    run bash -c "jq -c '[.name, .url]' '$(subs_dir)'/*.json"
    assert_output '["cdn.example","https://cdn.example/s/b"]'
}

@test "import_server_list.sh: a name given is the subscription's name" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" "  My List  "

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "My List"
}

# The single list and link of the previous release go with the first
# subscription written; the rest of the config stays.
@test "import_server_list.sh: removes servers.json and xray.subscription_url of the previous release" {
    load_import_into
    write_imported_state
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    [[ ! -e $VPD_DIR/data/servers.json ]]
    run jq -c '[(.xray | has("subscription_url")), .xray.clients]' "$VPD_CONFIG"
    assert_output '[false,["192.168.50.10"]]'
}

@test "import_server_list.sh: Add puts a second subscription beside the first" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "Beta" q

    assert_success
    # Both were added within one second, and the random ids order them then.
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$(subs_dir)' | jq -c '[.[].name] | sort'"
    assert_output '["Alpha","Beta"]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["1.2.3.4","5.6.7.8","9.9.9.9"]'
}

@test "import_server_list.sh: the same https link refreshes its subscription" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"

    run_import a "https://cdn.example/s/b" "" q

    assert_success
    assert_output --partial "saved already"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Alpha"
}

@test "import_server_list.sh: refuses a name another subscription has, whatever its case" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "ALPHA" q

    assert_output --partial "Another subscription is named ALPHA"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
}

# A refresh whose download fails keeps the list and says why in the file,
# as the daemons do.
@test "import_server_list.sh: Refresh records why a download failed and keeps the list" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    serve_failing_download

    run_import r 1 q

    run bash -c "jq -c '[.error, (.servers | length)]' '$(subs_dir)'/*.json"
    assert_output '["Failed to download the subscription",3]'
}

# Deleted while its download runs: the list is not published, and the file
# is not brought back.
@test "import_server_list.sh: a subscription deleted while it downloads stays deleted" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    printf '#!/bin/sh\nrm -f "%s"/*.json\ncat "%s"\n' "$(subs_dir)" "$BATS_TEST_TMPDIR/servers.txt" > "$BATS_TEST_TMPDIR/bin/curl"

    run_import r 1 q

    assert_output --partial "deleted or changed while it downloaded"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
}

@test "import_server_list.sh: Rename" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import n 1 "Main" q

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Main"
}

# The choice the watch kept from the deleted subscription ends with it; the
# running Xray is left alone, and the user is told.
@test "import_server_list.sh: Delete ends the choice kept from it and warns about the running server" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"
    local id
    id=$(jq -r '.id' "$(subs_dir)"/*.json)
    jq --arg id "$id" '.xray.active_server = {subscription: $id, name: "A"} | .xray.preferred_server = {subscription: $id, name: "B"}' \
        "$VPD_CONFIG" > "$VPD_CONFIG.new" && mv "$VPD_CONFIG.new" "$VPD_CONFIG"

    run_import d 1 y q

    assert_success
    assert_output --partial "The running Xray server came from Alpha"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
    run jq -c '[(.xray | has("preferred_server")), .xray.active_server.name, .xray.servers]' "$VPD_CONFIG"
    assert_output '[false,"A",[]]'
}

# The Web UI, the bot and configure.sh write under one lock, and so does the
# watch. Nothing is written before the lock is held.
@test "import_server_list.sh: publishes nothing while another writer holds the config lock" {
    load_import_into
    write_config
    write_list_file
    hold_config_lock
    export VPD_CONFIG_LOCK_WAIT=1

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""
    release_config_lock

    assert_failure
    assert_output --partial "nothing was imported"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
    run jq -c '.xray | has("servers")' "$VPD_CONFIG"
    assert_output "false"
}

@test "import_server_list.sh: a list without a usable server adds nothing" {
    load_import_into
    write_config
    printf '%s\n' 'vless://uuid@5.6.7.8:99999?type=tcp#Bad' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# Before configure.sh has run there is no config, and none is created.
@test "import_server_list.sh: creates no config" {
    load_import_into
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data}' > "$VPD_DIR/vpn-director.json.template"
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "jq -c '[.servers[].name]' '$(subs_dir)'/*.json"
    assert_output '["A","B","C"]'
    [[ ! -e $VPD_CONFIG ]]
}
```

The five tests left in that section become:

```bash
# The new provider serves no links at all: its subscription is an array of
# Xray configs. The proxy outbound of each is the server; a config with more
# than one proxy is skipped as composite.
@test "import_server_list.sh: imports an Xray JSON subscription" {
    load_import_into
    write_config
    cat > "$BATS_TEST_TMPDIR/servers.txt" <<'JSON'
[{"remarks": "Oslo", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.10", "port": 443, "password": "p"}]}, "streamSettings": {"network": "tcp", "security": "tls"}}, {"tag": "direct", "protocol": "freedom"}]},
 {"remarks": "Auto", "outbounds": [{"protocol": "vless", "settings": {"address": "198.51.100.11", "port": 443, "id": "u"}}, {"protocol": "vless", "settings": {"address": "198.51.100.12", "port": 443, "id": "u"}}]}]
JSON

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    assert_output --partial "Skipping Auto: composite (2 proxy outbounds)"
    run bash -c "jq -c '[.servers[] | [.name, .outbound.protocol, .outbound.settings.servers[0].password, .ips]]' '$(subs_dir)'/*.json"
    assert_output '[["Oslo","trojan","p",["198.51.100.10"]]]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["198.51.100.10"]'
}

# An HTML page - what some panels answer a browser with - is no subscription.
@test "import_server_list.sh: refuses a body it cannot read" {
    load_import_into
    write_config
    printf '%s\n' '<!doctype html><html><body>Open this link in your VPN app</body></html>' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    assert_output --partial "Cannot read the subscription: unrecognized subscription format"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# The daemons refuse a subscription over 1 MiB rather than cut it short, and
# so does the router.
@test "import_server_list.sh: refuses a subscription file over 1 MiB" {
    load_import_into
    write_config
    write_oversized_list

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl --max-filesize stops a transfer it knows to be too large with exit 63:
# the subscription's size, not a download that failed.
@test "import_server_list.sh: refuses a download curl stops at 1 MiB" {
    load_import_into
    write_config
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<MOCK
#!/bin/sh
printf '%s\n' "\$*" > "$BATS_TEST_TMPDIR/curl.args"
exit 63
MOCK
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    run cat "$BATS_TEST_TMPDIR/curl.args"
    assert_output --partial -- "--max-filesize 1048576"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl before 8.4.0 does not stop a transfer whose size it did not know in
# advance, so what it downloaded is measured again.
@test "import_server_list.sh: refuses a download over 1 MiB that curl let through" {
    load_import_into
    write_config
    write_oversized_list
    serve_list_file

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats import_server_list.bats`
Expected: FAIL — the script still writes `servers.json` and has no menu.

- [ ] **Step 3: Rewrite the script around the menu**

In `router/opt/vpn-director/import_server_list.sh`:

1. Source the store after `lib/subscription.sh`:

```bash
# shellcheck source=lib/substore.sh
. "$SCRIPT_DIR/lib/substore.sh"
```

2. `read_input` tolerates the end of the input, which quits the menu:

```bash
read_input() {
    printf "%s: " "$1" >&2
    read -r INPUT_RESULT || INPUT_RESULT=""
}
```

3. Add below `read_input`:

```bash
# fail <message> - logs <message> as an error, leaves it in $FAIL_REASON_FILE
# for a refresh to record, and ends the action it runs in.
fail() {
    log -l ERROR "$1"
    if [[ -n ${FAIL_REASON_FILE:-} ]]; then
        printf '%s' "$1" > "$FAIL_REASON_FILE"
    fi
    exit 1
}

# run_action <function> [args] - runs one action in a subshell with errexit
# on: its fail or exit ends the action and not the menu. ACTION_RC is its
# status. Never call it on the left of || or &&: bash turns errexit off inside.
run_action() {
    set +e
    ( set -e; "$@" )
    ACTION_RC=$?
    set -e
}
```

4. Split the first step: `step_get_subscription` keeps its prompt and hands the answer on; everything from the `if [[ -z "$SUB_INPUT" ]]` check down moves into `fetch_subscription`, with every `log -l ERROR "…"` followed by `exit 1` turned into `fail "…"`:

```bash
step_get_subscription() {
    log -l TRACE "Step 1: Subscription"

    printf "Enter a subscription URL or the path to a file:\n"
    printf "(Share links - vless, vmess, trojan, ss, hysteria2 - base64 or plain, or Xray JSON)\n\n"

    read_input "URL or path"
    fetch_subscription "$INPUT_RESULT"
}

# fetch_subscription <link or path> - downloads the link or reads the file into
# SUB_RESULT, the Result JSON of subscription_decode, and logs every entry it
# skipped. SUB_INPUT keeps <link or path>.
fetch_subscription() {
    SUB_INPUT=$1

    if [[ -z "$SUB_INPUT" ]]; then
        fail "No input provided"
    fi

    # The largest subscription taken, in bytes: 1 MiB, the cap of the daemons'
    # download (service.MaxSubscriptionBody). A larger one is refused, never
    # cut short: cut, a base64 list decodes to a shorter one.
    local -r max_bytes=1048576
    local content rc=0
    case "$SUB_INPUT" in
        http://*|https://*)
            log "Downloading from URL..."
            content=$(curl -fsSL --connect-timeout 10 --max-time 60 --max-filesize "$max_bytes" "$SUB_INPUT") || rc=$?
            if (( rc == 63 )); then
                # curl's "maximum file size exceeded"
                fail "Subscription exceeds 1 MiB; nothing was imported"
            elif (( rc != 0 )); then
                fail "Failed to download the subscription"
            fi
            # curl before 8.4.0 does not stop a transfer whose size it did not
            # know in advance.
            if (( $(printf '%s' "$content" | wc -c) > max_bytes )); then
                fail "Subscription exceeds 1 MiB; nothing was imported"
            fi
            ;;
        *)
            if [[ ! -f "$SUB_INPUT" ]]; then
                fail "File not found: $SUB_INPUT"
            fi
            if (( $(wc -c < "$SUB_INPUT") > max_bytes )); then
                fail "Subscription exceeds 1 MiB; nothing was imported"
            fi
            content=$(cat "$SUB_INPUT")
            ;;
    esac

    local err
    err=$(tmp_file)
    if ! SUB_RESULT=$(printf '%s' "$content" | subscription_decode 2>"$err"); then
        fail "Cannot read the subscription: $(cat "$err")"
    fi

    local skipped line
    skipped=$(printf '%s' "$SUB_RESULT" | jq -r "$JQ_PRINTABLE"'
        .skipped[] | "Skipping \(.name | printable): \(.reason) (\(.detail | printable))"')
    if [[ -n $skipped ]]; then
        while IFS= read -r line; do
            log -l WARN "$line"
        done <<< "$skipped"
    fi

    if [[ $(printf '%s' "$SUB_RESULT" | jq '.servers | length') -eq 0 ]]; then
        fail "No supported servers in subscription"
    fi
}
```

5. `step_parse_servers` reuses a `$SERVERS_TMP` the caller set, and no longer names `servers.json`: replace its first lines with

```bash
step_parse_servers() {
    log -l TRACE "Step 2: Resolving Servers"

    SERVERS_TMP=${SERVERS_TMP:-$(tmp_file)}
```

   (the `DATA_DIR=$(get_data_dir)` and `SERVERS_FILE=…` lines go), and its last check becomes `fail "No servers could be resolved"`.

6. Replace `step_publish_servers` and `main` with:

```bash
###############################################################################
# Publishing: every write under the config lock
###############################################################################

# now - the time as the subscription files keep it: UTC, whole seconds.
now() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

# store_and_sync <subscription_json> - under the lock the caller holds: writes
# the file, brings xray.servers in step, takes away what the previous release
# kept (servers.json, xray.subscription_url), and releases the lock.
store_and_sync() {
    if ! substore_write "$SUB_DIR" "$1"; then
        substore_unlock
        fail "Failed to write the subscription; nothing was saved"
    fi
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")"; then
        substore_unlock
        fail "The subscription is saved, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    substore_unlock
}

# publish_add <input> <name> <base> - saves $SERVERS_TMP as a subscription. An
# https link already saved makes it a refresh of that subscription, and a
# rename too when <name> is given; a file or a plain-http link is a static
# list, always a new one. <base> names a new subscription when <name> is empty.
publish_add() {
    local input=$1 name=$2 base=$3 url="" subs id sub stamp
    [[ $input == https://* ]] && url=$input
    stamp=$(now)
    if ! substore_lock "$VPD_CONFIG"; then
        fail "Config is locked by the Web UI or the bot; nothing was imported. Run the import again"
    fi
    subs=$(substore_list "$SUB_DIR")
    id=""
    if [[ -n $url ]]; then
        id=$(jq -r --arg u "$url" 'first(.[] | select(.url == $u) | .id) // ""' <<< "$subs")
    fi
    if [[ -n $id ]]; then
        sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$subs")
        if [[ -n $name && $name != "$(jq -r '.name' <<< "$sub")" ]]; then
            if substore_name_taken "$subs" "$name" "$id"; then
                substore_unlock
                fail "Another subscription is named $name; nothing was imported"
            fi
            sub=$(jq -c --arg n "$name" '.name = $n' <<< "$sub")
        fi
        log "The link is saved already as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub"); its list is refreshed"
    else
        if (( $(jq length <<< "$subs") >= SUBSTORE_MAX )); then
            substore_unlock
            fail "There are $SUBSTORE_MAX subscriptions already; delete one first"
        fi
        if [[ -z $name ]]; then
            name=$(substore_default_name "$subs" "$base")
        elif substore_name_taken "$subs" "$name"; then
            substore_unlock
            fail "Another subscription is named $name; nothing was imported"
        fi
        id=$(substore_new_id "$SUB_DIR")
        sub=$(jq -nc --arg id "$id" --arg n "$name" --arg u "$url" --arg now "$stamp" \
            '{id: $id, name: $n, added: $now} + (if $u == "" then {} else {url: $u} end)')
    fi
    sub=$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$stamp" \
        '.servers = $servers[0] | .refreshed = $now | del(.error)' <<< "$sub")
    store_and_sync "$sub"
    log "Saved $SERVER_COUNT servers as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")"
}

# publish_refresh <id> <link> <name> - saves $SERVERS_TMP as the list of
# subscription <id>, only while it still exists with <link>: one deleted, or
# deleted and added again, while its download ran is not brought back.
publish_refresh() {
    local id=$1 url=$2 name=$3 sub
    if ! substore_lock "$VPD_CONFIG"; then
        fail "Config is locked by the Web UI or the bot; $name was not refreshed"
    fi
    sub=$(jq -c --arg id "$id" --arg u "$url" 'first(.[] | select(.id == $id and .url == $u)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -z $sub ]]; then
        substore_unlock
        fail "$name was deleted or changed while it downloaded; nothing was written"
    fi
    store_and_sync "$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$(now)" \
        '.servers = $servers[0] | .refreshed = $now | del(.error)' <<< "$sub")"
    log "Refreshed $name: $SERVER_COUNT servers"
}

# record_refresh_error <id> <link> <refreshed> <reason> - notes why a refresh
# failed; the list stays. Nothing is written when the subscription is gone,
# has another link, or was refreshed since <refreshed> by someone else.
record_refresh_error() {
    local id=$1 url=$2 seen=$3 reason=$4 sub
    substore_lock "$VPD_CONFIG" || return 0
    sub=$(jq -c --arg id "$id" --arg u "$url" --arg seen "$seen" \
        'first(.[] | select(.id == $id and .url == $u and (.refreshed // "") == $seen)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -n $sub ]]; then
        substore_write "$SUB_DIR" "$(jq -c --arg r "$reason" '.error = $r' <<< "$sub")" || true
    fi
    substore_unlock
}

###############################################################################
# The menu
###############################################################################

# show_subscriptions <subs_json> - the subscriptions, numbered, a line each.
show_subscriptions() {
    printf '\nSubscriptions:\n'
    jq -r "$JQ_PRINTABLE"'
        def host: (split("/")[2] // "") | split("@") | last | split(":")[0];
        to_entries[]
        | .key as $i | .value
        | "  \($i + 1)) \(.name | printable)   \(if (.url // "") == "" then "static list" else (.url | host | printable) end)   \(.servers | length) servers   "
          + (if (.error // "") != "" then "error: \(.error | printable)"
             else "refreshed \((.refreshed // "") | .[0:10]) \((.refreshed // "") | .[11:16])" end)' <<< "$1"
}

# pick_subscription <subs_json> <prompt> - asks for a number of the list and
# prints the id at it; fails for an answer that is no number of it.
pick_subscription() {
    local count
    count=$(jq length <<< "$1")
    read_input "$2 [1-$count]"
    if [[ $INPUT_RESULT =~ ^[0-9]+$ ]] && (( INPUT_RESULT >= 1 && INPUT_RESULT <= count )); then
        jq -r ".[$((INPUT_RESULT - 1))].id" <<< "$1"
        return 0
    fi
    fail "There is no subscription number $INPUT_RESULT"
}

# menu_add - asks for a link or a file and a name, downloads and resolves the
# list, and saves it (publish_add).
menu_add() {
    local base name=""
    step_get_subscription
    case $SUB_INPUT in
        http://*|https://*) base=$(jq -Rr '(split("/")[2] // "") | split("@") | last | split(":")[0]' <<< "$SUB_INPUT") ;;
        *) base=${SUB_INPUT##*/}; base=${base%.*} ;;
    esac
    read_input "Name (Enter for $base)"
    if [[ -n $INPUT_RESULT ]]; then
        name=$(substore_clean_name "$INPUT_RESULT") || fail "The name was refused; nothing was imported"
    fi
    step_parse_servers
    publish_add "$SUB_INPUT" "$name" "$base"
}

# fetch_and_resolve <link> - one refresh's download and resolution.
fetch_and_resolve() {
    fetch_subscription "$1"
    step_parse_servers
}

# refresh_one <subscription_json> - downloads the subscription's link again
# and publishes the list. A download or a list that fails is recorded in the
# subscription, whose list stays, and ends the action.
refresh_one() {
    local sub=$1 id url seen name reason
    id=$(jq -r '.id' <<< "$sub")
    url=$(jq -r '.url' <<< "$sub")
    seen=$(jq -r '.refreshed // ""' <<< "$sub")
    name=$(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")
    log "Refreshing $name"
    SERVERS_TMP=$(tmp_file)
    FAIL_REASON_FILE=$(tmp_file)
    run_action fetch_and_resolve "$url"
    if (( ACTION_RC != 0 )); then
        reason=$(cat "$FAIL_REASON_FILE")
        record_refresh_error "$id" "$url" "$seen" "${reason:-the refresh failed}"
        exit 1
    fi
    FAIL_REASON_FILE=""
    SERVER_COUNT=$(jq length "$SERVERS_TMP")
    publish_refresh "$id" "$url" "$name"
}

# linked_subscriptions <subs_json> - those that have a link to refresh.
linked_subscriptions() {
    jq -c 'map(select((.url // "") != ""))' <<< "$1"
}

menu_refresh() {
    local linked id
    linked=$(linked_subscriptions "$1")
    [[ $(jq length <<< "$linked") -gt 0 ]] || fail "No subscription has a link to refresh"
    show_subscriptions "$linked"
    id=$(pick_subscription "$linked" "Refresh subscription")
    refresh_one "$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$linked")"
}

menu_refresh_all() {
    local sub total=0 failed=0
    while IFS= read -r sub; do
        total=$((total + 1))
        run_action refresh_one "$sub"
        if (( ACTION_RC != 0 )); then
            failed=$((failed + 1))
        fi
    done < <(jq -c '.[]' <<< "$(linked_subscriptions "$1")")
    log "Refreshed $((total - failed)) of $total subscriptions"
}

menu_rename() {
    local subs=$1 id sub name
    id=$(pick_subscription "$subs" "Rename subscription")
    read_input "New name"
    name=$(substore_clean_name "$INPUT_RESULT") || fail "The name was refused; nothing was renamed"
    substore_lock "$VPD_CONFIG" || fail "Config is locked by the Web UI or the bot; nothing was renamed"
    subs=$(substore_list "$SUB_DIR")
    sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id)) // empty' <<< "$subs")
    if [[ -z $sub ]]; then
        substore_unlock
        fail "That subscription is gone"
    fi
    if substore_name_taken "$subs" "$name" "$id"; then
        substore_unlock
        fail "Another subscription is named $name"
    fi
    store_and_sync "$(jq -c --arg n "$name" '.name = $n' <<< "$sub")"
    log "Renamed to $name"
}

menu_delete() {
    local subs=$1 id name active=""
    id=$(pick_subscription "$subs" "Delete subscription")
    name=$(jq -r --arg id "$id" "$JQ_PRINTABLE"' first(.[] | select(.id == $id)) | .name | printable' <<< "$subs")
    read_input "Delete $name and its servers? [y/N]"
    if [[ $INPUT_RESULT != [yY]* ]]; then
        log "Nothing was deleted"
        return 0
    fi
    substore_lock "$VPD_CONFIG" || fail "Config is locked by the Web UI or the bot; nothing was deleted"
    substore_delete "$SUB_DIR" "$id"
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")" \
        "if (.xray.preferred_server.subscription // \"\") == \"$id\" then del(.xray.preferred_server) else . end"; then
        substore_unlock
        fail "$name is deleted, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    if [[ -f $VPD_CONFIG ]]; then
        active=$(jq -r '.xray.active_server.subscription // ""' "$VPD_CONFIG")
    fi
    substore_unlock
    log "Deleted $name"
    if [[ $active == "$id" ]]; then
        log -l WARN "The running Xray server came from $name. Xray keeps running it until you select another (configure.sh, the Web UI or /xray)"
    fi
}

menu() {
    local subs
    while :; do
        subs=$(substore_list "$SUB_DIR")
        show_subscriptions "$subs"
        printf '\na) Add  r) Refresh  R) Refresh all  n) Rename  d) Delete  q) Quit\n'
        read_input "Choice"
        case $INPUT_RESULT in
            a) run_action menu_add ;;
            r) run_action menu_refresh "$subs" ;;
            R) run_action menu_refresh_all "$subs" ;;
            n) run_action menu_rename "$subs" ;;
            d) run_action menu_delete "$subs" ;;
            q|Q|"") return 0 ;;
            *) printf 'Unknown choice: %s\n' "$INPUT_RESULT" ;;
        esac
    done
}

###############################################################################
# Main
###############################################################################

main() {
    log -l TRACE "Import Server List"
    printf "Subscriptions: the server lists this router can run.\n"

    DATA_DIR=$(get_data_dir)
    SUB_DIR=$(substore_dir "$DATA_DIR")
    if [[ $(substore_list "$SUB_DIR" | jq length) -eq 0 ]]; then
        # A first run: there is nothing to choose from yet.
        run_action menu_add
        if (( ACTION_RC != 0 )); then
            exit "$ACTION_RC"
        fi
    fi
    menu

    log -l TRACE "Import Complete"
    printf "Run /opt/vpn-director/configure.sh to select a server and finish the setup.\n"
}
```

   Update the header comment of the script: it keeps the subscriptions of `lib/substore.sh` — adds, refreshes, renames and deletes them under the config lock — instead of publishing one list.

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats import_server_list.bats && shellcheck ../opt/vpn-director/import_server_list.sh`
Expected: every test passes; shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/import_server_list.sh router/test/import_server_list.bats
git commit -m "feat(shell): import_server_list.sh adds, refreshes, renames and deletes subscriptions"
```

---

### Task 15: `configure.sh` — subscription, then server

**Files:**
- Modify: `router/opt/vpn-director/configure.sh`
- Modify: `router/test/unit/configure.bats`

**Interfaces:**
- Consumes: Task 13's `substore_dir`, `substore_list`, `substore_ips`.
- Produces: `check_subscriptions` (sets `SUB_DIR` and `SUBS_JSON`, the subscriptions that have servers), `SELECTED_SUBSCRIPTION_ID`; `active_server` carries `subscription`; `xray.servers` is the union of every subscription's addresses.

- [ ] **Step 1: Write the failing tests**

In `router/test/unit/configure.bats`, `load_wizard` replaces its two `SERVERS_FILE` lines with:

```bash
    SUBS_JSON='[{"id":"0a1b2c3d","name":"Main","servers":[{"address":"1.2.3.4","ips":["1.2.3.4"]}]}]'
    SELECTED_SUBSCRIPTION_ID="0a1b2c3d"
```

The three `step_select_xray_server` tests hand their list over as one subscription instead of `$SERVERS_FILE`; their assertions stay:

```bash
@test "step_select_xray_server: lists each server with its protocol" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Legacy","address":"legacy.example.com","port":443,"ips":["1.2.3.4"],"security":"reality"},
 {"name":"Old TLS","address":"old.example.com","port":443,"ips":["1.2.3.5"]},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"Canada SS","address":"ss.example.com","port":2030,"ips":["1.2.3.7"],"outbound":{"protocol":"shadowsocks"}},
 {"name":"Gaming","address":"hy.example.com","port":8443,"ips":["1.2.3.8"],"outbound":{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Legacy [vless·reality]"
    assert_output --partial "2) Old TLS [vless·tls]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) Canada SS [ss]"
    assert_output --partial "5) Gaming [hysteria2]"
}

@test "step_select_xray_server: an outbound it cannot read is listed as ?" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Broken","address":"broken.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":"tcp"}},
 {"name":"Null","address":"null.example.com","port":443,"ips":["1.2.3.5"],"outbound":null},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"No protocol","address":"noproto.example.com","port":443,"ips":["1.2.3.7"],"outbound":{"streamSettings":{"network":"ws","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Broken [?]"
    assert_output --partial "2) Null [?]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) No protocol [?]"
}

# A name and an outbound are subscription text: an escape sequence in either
# must not reach the terminal. The wizard's own colours are escape sequences
# too, so they are switched off: any ESC left in the output came from the list.
@test "step_select_xray_server: control characters from a subscription do not reach the terminal" {
    load_wizard
    RED='' GREEN='' YELLOW='' BLUE='' NC=''
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Bad\u001b[31mName","address":"bad.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"\u001b]0;owned\u0007tls"}}}]
JSON
)

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial $'\x1b'
    assert_output --partial "1) Bad[31mName [vless·ws·]0;ownedtls]"
}
```

`step_generate_configs: records the server the Xray config was built from` also asserts the subscription:

```bash
    run jq -r '.xray.active_server | "\(.name)|\(.address)|\(.port)|\(.subscription)"' "$VPD_DIR/vpn-director.json"
    assert_output "Осло, Норвегия, Extra|1.2.3.4|443|0a1b2c3d"
```

Add:

```bash
two_subscriptions='[
  {"id":"0a1b2c3d","name":"Alpha","servers":[
    {"name":"Oslo","address":"a.example.com","port":443,"ips":["192.0.2.10"]},
    {"name":"Germany-1","address":"b.example.com","port":443,"ips":["192.0.2.11"]}]},
  {"id":"1b2c3d4e","name":"Beta","servers":[
    {"name":"Germany-1","address":"198.51.100.20","port":8443,"ips":["198.51.100.20"]}]}]'

@test "step_select_xray_server: with several subscriptions it asks for the subscription first" {
    load_wizard
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    run step_select_xray_server < <(printf '2\n1\n')

    assert_success
    assert_output --partial "1) Alpha (2 servers)"
    assert_output --partial "2) Beta (1 servers)"
    assert_output --partial "Selected: Germany-1 (198.51.100.20)"
}

@test "step_select_xray_server: one subscription goes straight to its servers" {
    load_wizard

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial "Select subscription"
}

@test "step_generate_configs: xray.servers covers every subscription" {
    load_wizard
    write_daemon_config
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    run step_generate_configs

    assert_success
    run jq -c '.xray.servers' "$VPD_DIR/vpn-director.json"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20"]'
}

@test "check_subscriptions: reads the shared fixtures" {
    load_wizard
    mkdir -p "$VPD_DIR/data/subscriptions"
    cp "$PROJECT_ROOT/../testdata/substore/"*.json "$VPD_DIR/data/subscriptions/"
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"

    check_subscriptions > /dev/null

    [[ $(jq -c '[.[].name]' <<< "$SUBS_JSON") == '["Beta","Alpha","Gamma"]' ]]
}

@test "check_subscriptions: no subscription sends the user to the import" {
    load_wizard
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"

    run check_subscriptions

    assert_failure
    assert_output --partial "Run import_server_list.sh first"
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats unit/configure.bats`
Expected: FAIL — `check_subscriptions: command not found`, and the selection tests still read `$SERVERS_FILE`.

- [ ] **Step 3: Write the two-step choice**

In `router/opt/vpn-director/configure.sh`:

1. Source the store after `lib/xrayconf.sh`:

```bash
# Library: the subscription files (self-contained, like xrayconf.sh)
. "$VPD_DIR/lib/substore.sh"
```

2. Add `SELECTED_SUBSCRIPTION_ID=""` to the "Temporary storage" variables.
3. Replace `check_servers_file` with:

```bash
# check_subscriptions - reads the subscriptions that have servers into
# SUBS_JSON, and stops the wizard when there is none.
check_subscriptions() {
    DATA_DIR=$(get_data_dir)
    SUB_DIR=$(substore_dir "$DATA_DIR")
    SUBS_JSON=$(substore_list "$SUB_DIR" | jq -c 'map(select((.servers // []) | length > 0))')

    local subs servers
    subs=$(jq length <<< "$SUBS_JSON")
    servers=$(jq '[.[].servers[]] | length' <<< "$SUBS_JSON")
    if [[ $subs -eq 0 ]]; then
        print_error "No subscription with servers in $SUB_DIR"
        print_info "Run import_server_list.sh first"
        exit 1
    fi

    print_success "Found $servers servers in $subs subscription(s)"
}
```

4. `step_select_xray_server` asks for the subscription first when there are several, then lists that subscription's servers exactly as today, reading `"$servers_json"` where it read `"$SERVERS_FILE"`:

```bash
step_select_xray_server() {
    print_header "Step 1: Select Xray Server"

    local count k=0 servers_json
    count=$(jq length <<< "$SUBS_JSON")
    if [[ $count -gt 1 ]]; then
        printf "Subscriptions:\n\n"
        i=1
        jq -r "$JQ_PRINTABLE"' .[] | "\(.name | printable)|\(.servers | length)"' <<< "$SUBS_JSON" | \
        while IFS='|' read -r name n; do
            printf "  %2d) %s (%s servers)\n" "$i" "$name" "$n"
            i=$((i + 1))
        done
        printf "\n"
        while true; do
            printf "Select subscription [1-%d]: " "$count"
            read -r choice
            if [[ $choice -ge 1 ]] 2>/dev/null && [[ $choice -le $count ]] 2>/dev/null; then
                break
            fi
            print_error "Invalid choice. Enter a number between 1 and $count"
        done
        k=$((choice - 1))
    fi
    SELECTED_SUBSCRIPTION_ID=$(jq -r ".[$k].id" <<< "$SUBS_JSON")
    servers_json=$(jq -c ".[$k].servers" <<< "$SUBS_JSON")

    printf "Available servers:\n\n"

    # Read servers from JSON and display. The label names the protocol, as the
    # Web UI and the bot do (vpnconfig.Server.Label): a record without an
    # outbound is a legacy VLESS one, generated as TLS when it names no
    # security.
    i=1
    jq -r "$JQ_PRINTABLE"'
        def protocol_label:
          # An outbound is stored as the subscription wrote it, so nothing says
          # its streamSettings is an object: indexing one that is not ends jq,
          # and with it the whole list. Server.Label in vpnconfig/outbound.go
          # answers "?" to an outbound it cannot read; so does this. Only a
          # record with no outbound key at all is the legacy one - jq reads a
          # null the way it reads a missing key, and a null outbound is no
          # outbound: the generators reject it, so it is a "?" too. So is an
          # outbound that names no protocol, as it is for Server.Label: there
          # is nothing to label, and "·ws·tls" is no label.
          try (
            (if (has("outbound") | not)
             then ["vless", .network, (if (.security // "") == "" then "tls" else .security end)]
             elif (.outbound | type) != "object" then error("not an outbound")
             else [.outbound.protocol, .outbound.streamSettings.network, .outbound.streamSettings.security] end)
            | map(if . == null then "" elif type == "string" then . else error("not a string") end) as [$p, $n, $s]
            | if $p == "" then "?"
              elif $p == "shadowsocks" then "ss" elif $p == "hysteria" then "hysteria2"
              else [$p] + (if $n == "" or $n == "tcp" or $n == "raw" then [] else [$n] end)
                        + (if $s == "" or $s == "none" then [] else [$s] end) | join("·") end
          ) catch "?";
        .[] | "\(.name | printable)|\(.address | printable)|\((.ips // []) | join(", "))|\(protocol_label | printable)"' <<< "$servers_json" | \
    while IFS='|' read -r name address ip label; do
        printf "  %2d) %s [%s]\n      %s -> %s\n\n" "$i" "$name" "$label" "$address" "$ip"
        i=$((i + 1))
    done

    total=$(jq length <<< "$servers_json")

    while true; do
        printf "Select server [1-%d]: " "$total"
        read -r choice

        if [[ $choice -ge 1 ]] 2>/dev/null && [[ $choice -le $total ]] 2>/dev/null; then
            break
        fi
        print_error "Invalid choice. Enter a number between 1 and $total"
    done

    # Get selected server data (jq uses 0-based index)
    idx=$((choice - 1))
    SELECTED_SERVER_ADDRESS=$(jq -r ".[$idx].address" <<< "$servers_json")
    SELECTED_SERVER_PORT=$(jq -r ".[$idx].port" <<< "$servers_json")
    SELECTED_SERVER_JSON=$(jq -c ".[$idx]" <<< "$servers_json")
    selected_name=$(jq -r "$JQ_PRINTABLE .[$idx].name | printable" <<< "$servers_json")

    print_success "Selected: $selected_name ($SELECTED_SERVER_ADDRESS)"
}
```

5. In `step_generate_configs`, `xray_servers_json` comes from every subscription, and the record names the subscription:

```bash
    # xray.servers: every address of every subscription (TPROXY_BYPASS).
    xray_servers_json=$(substore_ips "${SUBS_JSON:-[]}")
```

```bash
    xray_active_server_json=$(printf '%s' "$SELECTED_SERVER_JSON" \
        | jq -c --arg sub "${SELECTED_SUBSCRIPTION_ID:-}" \
            '{name: (.name // ""), address: (.address // ""), port: (.port // 0)}
             + (if $sub == "" then {} else {subscription: $sub} end)')
```

6. `main` calls `check_subscriptions` where it called `check_servers_file` (its comment: "Validate that a subscription exists").

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats unit/configure.bats && shellcheck ../opt/vpn-director/configure.sh`
Expected: every test passes; shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/configure.sh router/test/unit/configure.bats
git commit -m "feat(shell): configure.sh picks a subscription, then its server"
```

---

### Task 16: Documentation

**Files:**
- Modify: `CLAUDE.md`, `README.md`
- Modify: `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`, `.claude/rules/xray-tproxy.md`, `.claude/rules/testing.md`
- Modify: `router/opt/vpn-director/lib/xrayconf.sh` (header comment)

**Interfaces:** none; the docs describe what Tasks 1–15 built, and the spec is their source.

- [ ] **Step 1: `CLAUDE.md`**

- Commands: the block's last entry becomes

```bash
# Subscriptions: add, refresh, rename, delete (a menu)
/opt/vpn-director/import_server_list.sh
```

- Architecture table, two new rows beside `lib/subscription.sh` and `testdata/subscription/`:

```
| `router/opt/vpn-director/lib/substore.sh` | Subscription files (`<data_dir>/subscriptions/<id>.json`): order, ids, names, the write under the config lock; the twin of `vpnconfig/substore.go` |
| `testdata/substore/` | Synthetic subscription files both stores (shell and Go) must list alike |
```

- Data storage: "`data_dir` in vpn-director.json (default: `/opt/vpn-director/data`) — `subscriptions/<id>.json` (a file per subscription: its link, its status, its servers), ipset dumps".

- [ ] **Step 2: `README.md`**

- After installation, step 1 becomes: "Add the servers of your subscriptions (optional). The script is a menu: add, refresh, rename and delete subscriptions — up to ten, each a link or a file:" followed by the same command.
- The CLI block's `# Import servers` becomes `# Subscriptions: add, refresh, rename, delete`.
- Bot commands: `| /import <url> [name] | Add a subscription, or refresh the one saved with that link; /import alone refreshes them all |` and a new row `| /subs | Subscriptions: refresh, rename, delete |`.
- "How It Works → Xray TPROXY": after the paragraph on the formats, add: "Up to ten subscriptions live side by side; you pick the running server from any of them. When it dies, the bot's subscription watch moves the clients onto a Tunnel Director tunnel, refreshes every subscription at once and walks their servers — the chosen one and two more of its subscription, then one server of each subscription in turn — until one answers, and brings the clients back on it."

- [ ] **Step 3: `.claude/rules/telegram-bot.md`**

- The tree: `handler/subs.go  # /subs: refresh, rename, delete a subscription` and `subwatch/order.go  # The hybrid walk order and the dedupe key`.
- Commands table: `/import [url] [name]` — "Add a subscription, or refresh the one saved with that link; alone, refresh every subscription (a body over 1 MiB is refused)"; new rows `/subs` (`SubsHandler.HandleSubs`, "Subscriptions with refresh, rename and delete buttons; a rename takes the chat's next message") and `/cancel` (`SubsHandler.HandleCancel`, "Ends a rename that waits for its name").
- Configuration Wizard, step 1: "Server Selection — a subscription, then one of its servers (the first step is skipped with one subscription), 30 a page".
- Server switch (`/xray`): rewrite for two steps — `xray:sub:<id>:<page>`, `xray:subs`, `xray:select:<id>:<index>:<fingerprint>` with the fingerprint over `subscription|name|address|port`; a keyboard sent before subscriptions answers "server list changed".
- Subscription watch: bring every statement in line with spec 5. Replace these, and keep everything else of the section as it is:
  - Arming: "Armed when at least one subscription exists — a static list included — and there are effective Xray clients (after subtracting `paused_clients`). A `xray.failover` record arms it with or without a subscription, and so does a restore whose last apply has not succeeded: without a subscription the watch still restores the clients, follows the fallback tunnel and says so, but refreshes and walks nothing."
  - The look at the active server dials "every IPv4 address its subscription's entry lists (`chosenIndex`: its subscription, then its name, as the walk finds it)"; "`servers.json` unreadable" becomes "the subscriptions unreadable".
  - "It then refreshes the saved subscription (…)" becomes "It then runs a wave: every subscription with a link downloads at once, each within `FetchTimeout` (…)" — the WAN-then-tunnel path, the IPv4 resolution and the 3-minute deadline apply to each download as before.
  - The walk order: "the chosen server — its subscription's entry with its name, address and port, or else that subscription's first entry with its name — then the next servers of its subscription until `OwnFirst` (3) are placed, then one server of each subscription in turn, starting after the chosen server's subscription (`walkOrder`); a copy whose outbound, with its IPv4 in place, was already tried in the wave is skipped (`dialKey`)".
  - Publication: each arriving list is published with `xray.servers` in one config-lock update, only while its subscription still exists with the downloaded link (`vpnconfig.RefreshSubscription`); a failed download records its reason in the subscription's `error` (`vpnconfig.RecordSubscriptionError`, not over a newer `refreshed`) and keeps the list; the walk runs when at least one download arrived or no subscription has a link, a failed subscription is walked from its last list, and with every download failed there is no walk and "Subscription refresh failed: A, B" is sent.
  - Guards: the walk's guard refuses a server whose subscription is gone or has another link (`vpnconfig.ErrSubscriptionGone`), and the walk skips the rest of that subscription; the restore after a live probe does not look at the subscription; the return to the preferred server checks the preferred server's subscription.
  - Delete every sentence about `xray.subscription_url`, "a link saved while the walk runs", `vpnconfig.PublishServers`, `SubscriptionUnchanged`, and `import_server_list.sh` publishing `servers.json` with the saved link.
  - Messages: servers are named `<subscription> / <server>`; "No live server in any subscription"; a wave in which only some downloads failed sends nothing.
  - The returns: "the preferred server's entry in its subscription accepts TCP"; "while no subscription lists the server that runs".

- [ ] **Step 4: `.claude/rules/webui.md`**

- API table: the rows of spec 6.1 (`GET/POST /api/subscriptions`, `POST /api/subscriptions/refresh`, `POST /api/subscriptions/rename`, `DELETE /api/subscriptions`, the grouped `GET /api/servers`, `POST /api/servers/active` with `subscription`); `POST /api/servers/import` goes; `/api/config` "with `jwt_secret` blanked — subscription links live in their own files".
- The `active_server` paragraph: the record names its subscription too, since two subscriptions can name a server alike; a record from before subscriptions matches no server.
- The paragraph on the two server routes: the subscription routes write one subscription file and `xray.servers` in one config-lock update through `service.AddSubscription`, `RefreshSubscription`, `RefreshAllSubscriptions`, `RenameSubscription` and `DeleteSubscription`; a refresh publishes only while its subscription still exists with the link it downloaded; a failed download is a result (200) that says why, and the subscription records it. Drop the sentences about `PublishImport`, the saved link and `subscription_saved`.
- The paragraph on what an import says: "An add says the subscription is saved only when its file was written; a failure after that point carries `vpnconfig.ErrServersSaved`". The 1 MiB rule stays.
- `POST /api/servers/active` names the subscription as well as the index; a subscription that is gone is a 409 as a moved server is.
- Authentication: "`GET /api/config` blanks `jwt_secret`. Subscription links, whose paths carry tokens, are never in the config."

- [ ] **Step 5: `.claude/rules/xray-tproxy.md`, `.claude/rules/testing.md`, `lib/xrayconf.sh`**

- `xray-tproxy.md`: "An import stores each server's Xray outbound in its subscription's file (`<data_dir>/subscriptions/<id>.json`, `servers[].outbound`)" where it says `servers.json`.
- `testing.md`: `unit/substore.bats` in the tree, and after "Shared subscription cases" a paragraph: "`testdata/substore/` holds synthetic subscription files. `router/test/unit/substore.bats` and `server/internal/vpnconfig/substore_test.go` both read them and must list them alike — the same order, the same names — and both apply the same name rules to the same inputs. Entware's jq has no regex builtins; `substore.bats` fails when `lib/substore.sh` uses one."
- `lib/xrayconf.sh` header: "JSON object (as a subscription file stores it)".

- [ ] **Step 6: Check nothing stale is left**

Run:

```bash
grep -rn "servers\.json\|subscription_url\|PublishServers\|PublishImport\|SubscriptionUnchanged\|subscription_saved" CLAUDE.md README.md .claude/rules/ router/opt/vpn-director/
```

Expected: only the lines that say the old `servers.json` and `xray.subscription_url` are removed (the watch and store paragraphs, `import_server_list.sh`'s `rm -f "$DATA_DIR/servers.json"` and `del(.xray.subscription_url)` in `lib/substore.sh`).

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md README.md .claude/rules router/opt/vpn-director/lib/xrayconf.sh
git commit -m "docs: several subscriptions, the wave over them, and their files"
```

---

### Task 17: Final verification and the device check

**Files:** none new; `docs/superpowers/` leaves the branch in Step 6.

- [ ] **Step 1: The Go suite, vet and format**

Run (through `claude-forge:build-runner`): `cd server && go vet ./... && go test ./... -count=1 && go test -race ./internal/subwatch/ ./internal/service/ ./internal/webapi/ ./internal/handler/ && gofmt -l .`
Expected: PASS; `gofmt -l` lists at most `internal/ssrf/ssrf_test.go` and `internal/wizard/handler.go`.

- [ ] **Step 2: Leftovers**

Run:

```bash
grep -rn "SubscriptionURL\|SaveServers\|PublishServers\|PublishImport" server/ --include=*.go
grep -rn "servers\.json" server/ router/opt/ --include=*.go --include=*.sh
```

Expected: the first prints nothing; the second prints only the removal of the legacy file (`RemoveLegacyServers` and its callers, `rm -f "$DATA_DIR/servers.json"`).

- [ ] **Step 3: The shell suites and shellcheck**

Run in the background and read the counts from the log: `cd router/test && bats -r . > "$TMPDIR/bats.log" 2>&1; tail -5 "$TMPDIR/bats.log"` (with `TMPDIR` the session's scratchpad directory).
Expected: `0 failures`.
Run: `shellcheck router/opt/vpn-director/*.sh router/opt/vpn-director/lib/*.sh`
Expected: nothing new against `master`.

- [ ] **Step 4: Builds**

Run: `cd web && npm run build`, then from the repository root `make build-webui && make build-all`.
Expected: both daemons build for arm64 and arm; the SPA is embedded.

- [ ] **Step 5: The Web UI in dev mode**

Run `cd server && go run ./cmd/webui --dev`, log in as `admin`/`admin`, and open the Servers tab: the Subscriptions card says there is none yet, the Servers card says to add one, and `GET /api/subscriptions` answers `{"subscriptions":[]}`. Stop the server.

- [ ] **Step 6: Remove the plan documents from the branch**

The owner's rule: `docs/superpowers/` must not be in the pull request's diff; the documents stay in the branch history.

```bash
git rm -r docs/superpowers/
git commit -m "chore: remove superpowers docs from feature branch"
```

- [ ] **Step 7: The device check — only with the owner's explicit permission**

Ask the owner first. With a yes, on the owner's router:
1. Install the build.
2. Ask the owner for the two real subscription links at this point. Never write them — or a host, SNI, key or id from their bodies — into the repository, a commit, a log excerpt or a fixture.
3. Add one subscription in the Web UI and the other with `import_server_list.sh`; check both lists, their hosts, counts and statuses.
4. Select a server of each subscription in turn (Web UI, `/xray`, `configure.sh`); from a LAN client in `xray.clients`, check traffic through each.
5. Break the running server and watch the watch move the clients to the tunnel, refresh both subscriptions at once and walk into the other subscription; check the Telegram messages name `<subscription> / <server>`.
6. Rename and delete a subscription from the bot (`/subs`), and a delete of the running server's subscription (the warning, the running Xray left alone).
7. The bot's keyboards: two steps in `/xray` and `/configure`, pages of 30, `« Back`.

- [ ] **Step 8: Ask before pushing**

Report the results and ask the owner whether to push `feature/multi-subscriptions` and open the pull request.
