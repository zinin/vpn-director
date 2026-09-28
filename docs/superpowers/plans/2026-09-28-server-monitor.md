# Server Monitor, Stage 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new daemon, `vpn-director-watchd`, checks every endpoint of every subscription through a prober of its own - a second Xray process - once a minute, and the Web UI and the bot show each server's status.

**Architecture:** `internal/endpoint` takes the walk's endpoint helpers out of `subwatch`, so the walk, the monitor, the Web UI and the bot derive one key per endpoint the same way. `internal/monitor` holds the engine: it builds the endpoint set, runs Xray as `vpn-director-probe` (a hard link, never under the name `xray`) with one SOCKS inbound and an account per endpoint, schedules a check per endpoint, guards against a dead WAN, isolates outbounds Xray refuses or crashes on, and keeps its state across restarts. `internal/watchdapi` is the contract: HTTP over a unix socket, a client with a 2 s bound, and `Health`, which folds a server's endpoints into one status. `cmd/watchd` wires them; the updater, `install.sh`, the init script and the release build ship the daemon; `webapi`, the SPA and the bot's handlers read the socket.

**Tech Stack:** Go 1.25 (module `github.com/zinin/vpn-director/server`, `golang.org/x/net/proxy` for SOCKS5), Xray-core from Entware on the router (tested against 26.3.27), Vue 3 + TypeScript (vue-tsc), POSIX sh for the init script, Bash + bats-core for `install.sh`.

**Spec:** `docs/superpowers/specs/2026-09-28-server-monitor-design.md` - read it first. Sections 4.3 (the prober's name), 4.4 (the probe) and 4.5 (the schedule and the WAN guard) explain choices the code below makes without repeating why.

## Global Constraints

- Daemon name `vpn-director-watchd`: binary `/opt/vpn-director/vpn-director-watchd`, init script `S98vpn-director-watchd`, log `/tmp/vpn-director-watchd.log`, release assets `vpn-director-watchd-{arm64,arm,mipsle}`, updater table name `vpn-director-watchd` placed between the bot and the Web UI (the Web UI must stay the table's last entry: `TestSelfUpdate_ChecksTheLockAgainBeforeTheScript` depends on it).
- No command line of the daemon or the prober contains `xray`. The prober runs as `/opt/vpn-director/vpn-director-probe`, a hard link to the `xray` on PATH (a copy where a hard link cannot cross filesystems), never a symbolic link, with its config at `/tmp/vpn-director/probe/config.json`.
- The probe is `http://www.gstatic.com/generate_204`: HTTP 204 within 10 s is a success; a failed attempt is retried once after 2 s.
- Schedule: a live endpoint every `interval` (default `1m`, at least `10s`) within ±10 % jitter; a dead one after `2 × interval`, the pause doubling up to `dead_interval_max` (default `30m`, at least `2 × interval`); `concurrency` workers (default 8, 1 to 32).
- WAN guard: at least 5 completed checks - or every checkable endpoint when there are fewer - within 30 s, all failed, and no control answered within 30 s: dial `1.1.1.1:443` and `8.8.8.8:443` (TCP, 3 s). None accepting: state `wan_down`, the failures undone, controls dialed every 15 s.
- Socket `/tmp/vpn-director/watchd.sock`, mode 0600, no TCP listener: `GET /v1/monitor`, `POST /v1/monitor/check`. State file `/tmp/vpn-director/watchd-state.json`, mode 0600, saved every minute when changed and at shutdown.
- An endpoint's key is the hex SHA-256 of what it dials. No response, log line or state file carries a credential; the last segment of an Xray error chain (it quotes the offending value) is never stored or logged.
- The Web UI stays in English. The bot's new words are exactly: `живы`, `мониторинг не запущен`, and the state notes of Task 14.
- `gofmt -l` lists nothing but the two files it lists on master (`internal/ssrf/ssrf_test.go`, `internal/wizard/handler.go`). `go vet ./...` is clean. The existing tests pass unchanged except where a task says otherwise.
- `cmd/webui` embeds `web/dist`: before `go vet ./...` or `go test ./...` in a fresh checkout, run `make web-embed` from the repository root once.
- Stage files by name. Never touch or stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, the other files under `docs/superpowers/plans/`, `review.diff` or `test_exit.sh`: they are the owner's.
- Commit messages follow the house style: `type(scope): subject`, a blank line, one plain paragraph, no trailers.

## Review Focus

1. A subscription refresh replaces the endpoints while checks are on their way: the replaced prober's answers must count for nothing, and the new set must be checked. Pinned in Task 7 (`TestMonitor_AnswersOfAReplacedProberAreDropped`).
2. The WAN goes down while every server is fine: no server may turn dead, and every server is checked again once a control answers. Pinned in Task 7 (`TestMonitor_AWANOutageFreezesTheStatuses`).
3. The daemon restarts (an update) while servers are in their dead pauses: statuses and pauses come back, nothing is checked before its time, and a key that left the set meanwhile is dropped. Pinned in Task 7 (`TestMonitor_TheStateSurvivesARestart`).
4. The server list changes between the page's `GET /api/servers` and `GET /api/monitor`, or before a `↻`: no status is pinned on another server, and the check answers 409. Pinned in Task 12 (`TestHandleMonitorCheck_AChangedListIs409`, `TestHandleListServers_CarriesTheFingerprint`).
5. The first update from the previous release: the new daemon was never running, yet it must start; a daemon the owner stopped must stay stopped. Pinned in Task 8 (`TestGenerateScript_StartsADaemonNewWithTheRelease`).

## Beyond the Spec

Decided while planning; each follows from a requirement of the spec or closes a gap it left:

- `GET /api/servers` gains `fingerprint`: the page matches its rows with those of `GET /api/monitor` by fingerprint (spec 4.7), so it needs its own servers' fingerprints.
- `vpnconfig.ServerFingerprint` replaces the bot's private `serverFingerprint`, with the same formula, so that both daemons compute it.
- A monitor that answers but does not check (stopped, disabled, no xray, WAN down, prober error) is named on the bot's `/servers` page as it is on the Web UI's state line, so old statuses are not read as fresh ones.
- The prober's crash loop is bounded: 5 crashes within 10 minutes back off like a failed start (1, 2, then 5 minutes).
- The Logs tab and `/logs` gain the source `watchd`.
- `.gitignore` gains the dev run's socket, state, log and prober config.

## File Structure

| File | Change | Task |
|---|---|---|
| `server/internal/endpoint/endpoint.go`, `endpoint_test.go` | new: `PerAddress`, `ServerForDial`, `DialKey` moved from `subwatch`; `Key`, `Keys`, `WANControls` | 1 |
| `server/internal/subwatch/{watch,order,reach,return}.go`, `watch_test.go`, `order_test.go` | call `internal/endpoint`; moved tests leave | 1 |
| `server/internal/bot/bot.go` | `endpoint.ServerForDial` (1); the monitor client for the handlers (14) | 1, 14 |
| `server/internal/watchdapi/{types,health,server,client}.go`, `health_test.go`, `api_test.go` | new: the socket contract | 2 |
| `server/internal/vpnconfig/vpnconfig.go`, `vpnconfig_test.go` | `MonitorConfig` (3); `ServerFingerprint` (12) | 3, 12 |
| `router/opt/vpn-director/vpn-director.json.template` | the `monitor` section | 3 |
| `server/internal/monitor/settings.go`, `settings_test.go` | new: `Settings`, `SettingsFrom` | 3 |
| `server/internal/service/xray.go`, `xray_test.go` | `OutboundJSON` | 4 |
| `server/internal/monitor/endpoints.go`, `endpoints_test.go` | new: `Endpoint`, `Build` | 4 |
| `server/internal/monitor/{probeconfig,check}.go`, tests, `testdata/probe_config.golden.json` | new: the prober's config and one check | 5 |
| `server/internal/monitor/{process,process_linux,process_other,xray,fake}.go`, `xray_test.go` | new: the prober process, `XrayLauncher`, `FakeLauncher` | 6 |
| `server/internal/monitor/{entry,monitor,store}.go`, `entry_test.go`, `harness_test.go`, `monitor_test.go`, `robust_test.go` | new: the engine | 7 |
| `server/internal/updater/{updater.go,update_script.sh.tmpl}`, tests, golden | the table entry; a new daemon starts after its first update | 8 |
| `server/internal/paths/paths.go`, `paths_test.go` | the daemon's paths | 9 |
| `server/internal/monitor/{wan,stamp}.go`, `stamp_test.go` | new: `WANUp`, `Stamp` | 9 |
| `server/cmd/watchd/main.go` | new: the daemon | 9 |
| `router/opt/etc/init.d/S98vpn-director-watchd`, `router/files.manifest`, `Makefile`, `server/Makefile`, `.github/workflows/telegram-bot.yml` | shipping | 10 |
| `install.sh`, `router/test/unit/install.bats` | `download_watchd`, `start_watchd` | 11 |
| `server/internal/webapi/{handler_monitor,handler_servers,router}.go`, tests; `server/cmd/webui/main.go`; `server/internal/handler/xray.go`, `xray_test.go` | `/api/monitor`, `/api/monitor/check`, the fingerprint in `/api/servers` | 12 |
| `web/src/{types.ts,api.ts,style.css}`, `web/src/components/{ServersTab,LogsTab}.vue` | the Health column, checks, the monitor line, the `watchd` log | 13 |
| `server/internal/handler/{health,handler,servers,xray,misc}.go`, tests | marks in `/servers` and `/xray`, `/logs watchd` | 14 |
| `CLAUDE.md`, `.claude/rules/{watchd,webui,telegram-bot}.md`, `README.md`, `README.ru.md` | documentation | 15 |

How the tests see the world: the engine's tests (`internal/monitor/harness_test.go`) drive `refresh`, `tick`, `apply` and `crashed` by hand on a fake clock, with a `fakeLauncher` whose sessions answer from a per-key script; they never start Xray. `internal/monitor/check_test.go` runs a small SOCKS5 server in the test itself. `TestXrayLauncher_RealXray` runs only when an `xray` binary is on PATH and is skipped otherwise, as in CI. The API tests serve a real unix socket from a short `os.MkdirTemp` directory, because a socket path holds at most 108 bytes.

---

### Task 1: `internal/endpoint` - one definition of what the walk dials

The monitor must check exactly what the subscription watch's walk would dial, and the Web UI and the bot must name the same endpoints. So the walk's helpers leave `subwatch` for a package of their own, and it gains the monitor's key. Behaviour does not change: every `subwatch` test passes as it is, less the ones that move.

**Files:**
- Create: `server/internal/endpoint/endpoint.go`
- Create: `server/internal/endpoint/endpoint_test.go`
- Modify: `server/internal/subwatch/watch.go` (remove `perAddress`, `ServerForDial`, `keepHostname`)
- Modify: `server/internal/subwatch/order.go` (remove `dialKey`)
- Modify: `server/internal/subwatch/reach.go`, `server/internal/subwatch/return.go`
- Modify: `server/internal/subwatch/watch_test.go`, `server/internal/subwatch/order_test.go` (five tests leave)
- Modify: `server/internal/bot/bot.go:135`

**Interfaces:**
- Consumes: `vpnconfig.Server`, `vpnconfig.DecodeOutbound`, `vpnconfig.OutboundTarget` (unchanged).
- Produces:
  - `endpoint.PerAddress(servers []vpnconfig.Server) []vpnconfig.Server` - was `subwatch.perAddress`, unchanged.
  - `endpoint.ServerForDial(s vpnconfig.Server) vpnconfig.Server` - was `subwatch.ServerForDial`, unchanged.
  - `endpoint.DialKey(c vpnconfig.Server) string` - was `subwatch.dialKey`, unchanged: `""` for a record without an outbound.
  - `endpoint.Key(c vpnconfig.Server) string` - 64 lowercase hex digits for any copy, a legacy record included.
  - `endpoint.Keys(s vpnconfig.Server) []string` - one key per `PerAddress` copy of `s`, in that order.
  - `endpoint.WANControls []vpnconfig.Server` - `1.1.1.1:443`, `8.8.8.8:443`.

- [ ] **Step 1: Write the failing tests of the new functions**

Create `server/internal/endpoint/endpoint_test.go`:

```go
package endpoint

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestPerAddress_ACopyPerAddressAndServersWithoutOneAsTheyAre(t *testing.T) {
	got := PerAddress([]vpnconfig.Server{
		{Name: "Two", Address: "two.example", IPs: []string{"192.0.2.1", "", "192.0.2.2"}},
		{Name: "None", Address: "none.example"},
	})
	if len(got) != 3 || got[0].IPs[0] != "192.0.2.1" || got[1].IPs[0] != "192.0.2.2" || got[2].Name != "None" {
		t.Fatalf("copies %+v", got)
	}
	if len(got[0].IPs) != 1 {
		t.Fatalf("a copy keeps %v, want its one address", got[0].IPs)
	}
}

// Two names on one endpoint are one server to Xray and one line in the
// monitor's results; two addresses of one name are two. The key never carries
// the credential the outbound holds.
func TestKey_OneEndpointOneKeyWithoutItsCredential(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"secret-uuid","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	a := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: ob}
	b := a
	b.Name, b.Subscription = "Germany-2", "1b2c3d4e"
	c := a
	c.IPs = []string{"192.0.2.2"}

	if Key(a) != Key(b) {
		t.Fatal("two names on one endpoint have two keys")
	}
	if Key(a) == Key(c) {
		t.Fatal("two addresses share a key")
	}
	if k := Key(a); len(k) != 64 || strings.Contains(k, "secret") {
		t.Fatalf("key %q, want 64 hex digits", k)
	}
}

// A record from before outbounds were stored dials its flat fields; they make
// its key, so two such records with other credentials are two endpoints.
func TestKey_ALegacyRecordIsKeyedByItsFlatFields(t *testing.T) {
	a := vpnconfig.Server{Name: "Legacy", Address: "l.example", Port: 443, UUID: "u1", Security: "reality",
		PublicKey: "pk", SNI: "www.example.org", Fingerprint: "chrome", IPs: []string{"192.0.2.9"}}
	b := a
	b.UUID = "u2"
	if Key(a) == "" || Key(a) == Key(b) {
		t.Fatalf("keys %q and %q", Key(a), Key(b))
	}
	if DialKey(a) != "" {
		t.Fatal("a legacy record got a DialKey; the walk would take it for another")
	}
}

func TestKeys_OnePerAddressInPerAddressOrder(t *testing.T) {
	s := vpnconfig.Server{Name: "Two", Address: "two.example", Port: 443, IPs: []string{"192.0.2.1", "192.0.2.2"},
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"two.example","port":443,"password":"p"}]}}`)}
	copies := PerAddress([]vpnconfig.Server{s})
	if keys := Keys(s); len(keys) != 2 || keys[0] != Key(copies[0]) || keys[1] != Key(copies[1]) {
		t.Fatalf("keys %v", keys)
	}
	if keys := Keys(vpnconfig.Server{Name: "None", Address: "none.example", Port: 443}); len(keys) != 1 {
		t.Fatalf("a server without an address has keys %v, want one", keys)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/endpoint/ -count=1`
Expected: FAIL - `undefined: PerAddress`, `undefined: Key`, `undefined: DialKey`, `undefined: Keys`.

- [ ] **Step 3: Create the package**

Create `server/internal/endpoint/endpoint.go`. `PerAddress`, `ServerForDial`, `keepHostname` and `DialKey` are the bodies and comments of `subwatch.perAddress`, `subwatch.ServerForDial`, `subwatch.keepHostname` and `subwatch.dialKey`, renamed where exported:

```go
// Package endpoint names what the subscription watch's walk and the server
// monitor dial: one address of one server, with that address in its outbound.
// The walk, the monitor, the Web UI and the bot all derive endpoints here, so
// a status the monitor reports is a status of exactly what the walk would try.
package endpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// WANControls are dialed when every server looks dead at once: a WAN that
// works reaches one of them, so none accepting means the WAN is down, not the
// servers.
var WANControls = []vpnconfig.Server{
	{Address: "1.1.1.1", Port: 443},
	{Address: "8.8.8.8", Port: 443},
}

// PerAddress lists each server once for every address it resolved to, each copy
// with that address alone, so the walk dials them one after another; a server
// with none is listed as it is. An endpoint ban takes an address, not the name:
// a host can resolve to one the router cannot reach and another it can, and
// dialing only the first rejected the whole server.
func PerAddress(servers []vpnconfig.Server) []vpnconfig.Server {
	out := make([]vpnconfig.Server, 0, len(servers))
	for _, s := range servers {
		n := len(out)
		for _, ip := range s.IPs {
			if ip == "" {
				continue
			}
			c := s
			c.IPs = []string{ip}
			out = append(out, c)
		}
		if len(out) == n {
			out = append(out, s)
		}
	}
	return out
}

// ServerForDial uses a tunnel-resolved IPv4 for vnext so Xray does not go
// back to the system resolver. A TLS server name keeps the hostname. A REALITY
// one is the site the handshake borrows, never the proxy's own host, so an
// entry without one stays without one and is refused as the Web UI refuses it.
// Web UI /xray keep s.Address and let Xray resolve, so a CDN IP change still
// works there.
//
// A server whose import stored its outbound gets the IP in the outbound's own
// address slot (vpnconfig.OutboundTarget). Where the source left the name to
// the address, dialing an IP would change it, so the hostname goes there
// instead: an empty tlsSettings.serverName, and for a stream without security
// an empty Host of ws or httpupgrade, an empty Host of the xhttpSettings or
// splithttpSettings the record has - Xray reads the former over the latter
// and drops the other - or an empty grpcSettings.authority, which a
// cleartext gRPC stream otherwise takes from the address. With TLS, Xray
// takes that Host, and gRPC's authority, from the server name.
//
// The download host of an xhttp extra (downloadSettings.address) keeps its
// name: the record's IPs are the main address's, and Xray resolves that host
// itself through the system resolver. So with the WAN resolver silent, a
// server whose download host is another name is judged dead although its main
// address resolved; looking that host up over the tunnel is a separate task.
func ServerForDial(s vpnconfig.Server) vpnconfig.Server {
	ip := ""
	for _, v := range s.IPs {
		if v != "" {
			ip = v
			break
		}
	}
	if ip == "" {
		return s
	}
	host := s.Address
	if len(s.Outbound) == 0 {
		s.Address = ip
		if s.SNI == "" && s.Security != "reality" {
			s.SNI = host
		}
		return s
	}
	ob, err := vpnconfig.DecodeOutbound(s.Outbound)
	if err != nil {
		return s
	}
	target := vpnconfig.OutboundTarget(ob)
	if target == nil {
		return s
	}
	target["address"] = ip
	if net.ParseIP(host) == nil {
		keepHostname(ob, host)
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return s
	}
	s.Outbound = raw
	s.Address = ip
	return s
}

// keepHostname writes host where the stream would otherwise take the name
// from an address that is now an IP. The xhttp Host goes into the
// xhttpSettings or splithttpSettings the record has: Xray reads xhttpSettings
// over splithttpSettings and drops the other, so a new xhttpSettings beside a
// splithttpSettings would dial without its path, mode and extra. A cleartext
// gRPC stream takes its :authority from the address when
// grpcSettings.authority is empty, so the hostname goes there.
func keepHostname(ob map[string]interface{}, host string) {
	ss, _ := ob["streamSettings"].(map[string]interface{})
	if ss == nil {
		return
	}
	switch security, _ := ss["security"].(string); security {
	case "tls":
		tls, _ := ss["tlsSettings"].(map[string]interface{})
		if tls == nil {
			tls = map[string]interface{}{}
			ss["tlsSettings"] = tls
		}
		if name, _ := tls["serverName"].(string); name == "" {
			tls["serverName"] = host
		}
	case "", "none":
		key := ""
		switch ss["network"] {
		case "ws", "websocket":
			key = "wsSettings"
		case "httpupgrade":
			key = "httpupgradeSettings"
		case "xhttp", "splithttp":
			key = "xhttpSettings"
			if _, ok := ss[key].(map[string]interface{}); !ok {
				if _, ok := ss["splithttpSettings"].(map[string]interface{}); ok {
					key = "splithttpSettings"
				}
			}
		case "grpc":
			grpc, _ := ss["grpcSettings"].(map[string]interface{})
			if grpc == nil {
				grpc = map[string]interface{}{}
				ss["grpcSettings"] = grpc
			}
			if authority, _ := grpc["authority"].(string); authority == "" {
				grpc["authority"] = host
			}
			return
		}
		if key == "" {
			return
		}
		transport, _ := ss[key].(map[string]interface{})
		if transport == nil {
			transport = map[string]interface{}{}
			ss[key] = transport
		}
		headers, _ := transport["headers"].(map[string]interface{})
		if h, _ := transport["host"].(string); h != "" {
			return
		}
		// Xray's ws builder takes a host header in any case.
		for key, value := range headers {
			if h, _ := value.(string); strings.EqualFold(key, "host") && h != "" {
				return
			}
		}
		transport["host"] = host
	}
}

// DialKey is what a walk's copy of a server dials: its outbound with the
// address in place, as Generate writes it (ServerForDial). Copies with one key
// are one server to Xray whatever their names - one provider lists 62 names on
// 9 endpoints - and a walk tries each key once. A record without an outbound
// has no key and is never taken for another.
func DialKey(c vpnconfig.Server) string {
	if len(c.Outbound) == 0 {
		return ""
	}
	return string(ServerForDial(c).Outbound)
}

// legacyDial is what a record from before outbounds were stored dials: the
// flat fields the generator builds its VLESS outbound from.
type legacyDial struct {
	Address     string   `json:"address"`
	Port        int      `json:"port"`
	UUID        string   `json:"uuid"`
	Security    string   `json:"security"`
	Network     string   `json:"network"`
	Flow        string   `json:"flow"`
	SNI         string   `json:"sni"`
	Fingerprint string   `json:"fingerprint"`
	PublicKey   string   `json:"public_key"`
	ShortID     string   `json:"short_id"`
	ALPN        []string `json:"alpn"`
}

// Key names the copy c (one address of one server, as PerAddress lists it) in
// the monitor's results: the hex SHA-256 of what it dials. Copies with one
// DialKey share a key; a record without an outbound hashes the flat fields
// the generator builds its outbound from. A key carries no credential, which
// a DialKey does.
func Key(c vpnconfig.Server) string {
	material := DialKey(c)
	if material == "" {
		d := ServerForDial(c)
		raw, _ := json.Marshal(legacyDial{
			Address: d.Address, Port: d.Port, UUID: d.UUID, Security: d.Security,
			Network: d.Network, Flow: d.Flow, SNI: d.SNI, Fingerprint: d.Fingerprint,
			PublicKey: d.PublicKey, ShortID: d.ShortID, ALPN: d.ALPN,
		})
		material = "legacy:" + string(raw)
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// Keys is the key of every endpoint of s, one per address, in PerAddress
// order.
func Keys(s vpnconfig.Server) []string {
	copies := PerAddress([]vpnconfig.Server{s})
	keys := make([]string, len(copies))
	for i, c := range copies {
		keys[i] = Key(c)
	}
	return keys
}
```

- [ ] **Step 4: Move the walk's tests of these functions**

Cut these functions, each with the comment above it, out of `server/internal/subwatch/watch_test.go` and paste them, unchanged, at the end of `server/internal/endpoint/endpoint_test.go`:

- `TestServerForDial_UsesResolvedIPKeepsHostnameSNI`
- `TestServerForDial_LeavesARealitySNIEmpty`
- `TestServerForDial_WritesTheIPIntoTheOutbound`
- `TestServerForDial_KeepsTheHostnameWhereTheSourceLeftItToTheAddress`

Cut `TestDialKey_OneEndpointUnderTwoNamesIsOneServer` out of `server/internal/subwatch/order_test.go` and paste it after them, replacing its three `dialKey(` calls with `DialKey(`.

- [ ] **Step 5: Point `subwatch` and the bot at the package**

In `server/internal/subwatch/watch.go`:
- delete the functions `perAddress`, `ServerForDial` and `keepHostname` with their comments;
- replace `for _, s := range perAddress(order) {` with `for _, s := range endpoint.PerAddress(order) {`;
- replace `key := dialKey(s)` with `key := endpoint.DialKey(s)`;
- in the comments, replace `// (dialKey), and brings` with `// (endpoint.DialKey), and brings` and `// A copy whose dialKey the walk` with `// A copy whose endpoint.DialKey the walk`;
- add the import `"github.com/zinin/vpn-director/server/internal/endpoint"` and remove `"encoding/json"`, which nothing uses any more.

In `server/internal/subwatch/order.go`, delete `dialKey` with its comment.

In `server/internal/subwatch/reach.go`, keep the comment above `reachControls` and replace its definition:

```go
var reachControls = []vpnconfig.Server{
	{Address: "1.1.1.1", Port: 443},
	{Address: "8.8.8.8", Port: 443},
}
```

with:

```go
var reachControls = endpoint.WANControls
```

Also replace `dialable(perAddress(servers[i : i+1]))` with `dialable(endpoint.PerAddress(servers[i : i+1]))`, replace `is the IPv4 address a perAddress copy is dialed at` with `is the IPv4 address an endpoint.PerAddress copy is dialed at`, and add the `endpoint` import.

In `server/internal/subwatch/return.go`, replace both `perAddress(` with `endpoint.PerAddress(` and add the `endpoint` import.

In `server/internal/subwatch/watch_test.go`, replace `current = ServerForDial(s).Address` with `current = endpoint.ServerForDial(s).Address` and add the `endpoint` import. In `server/internal/subwatch/order_test.go`, remove the `"encoding/json"` import: the moved test was its only user.

In `server/internal/bot/bot.go`, replace `subwatch.ServerForDial(s)` with `endpoint.ServerForDial(s)` and add the `endpoint` import.

- [ ] **Step 6: Format, vet and test**

Run: `cd server && gofmt -w internal/endpoint internal/subwatch internal/bot && gofmt -l internal/endpoint internal/subwatch internal/bot && go vet ./internal/endpoint/... ./internal/subwatch/... ./internal/bot/... && go test ./internal/endpoint/... ./internal/subwatch/... ./internal/bot/... -count=1`
Expected: `gofmt -l` prints nothing; `ok` for all three packages.

- [ ] **Step 7: Commit**

```bash
git add server/internal/endpoint/endpoint.go server/internal/endpoint/endpoint_test.go \
  server/internal/subwatch/watch.go server/internal/subwatch/order.go server/internal/subwatch/reach.go \
  server/internal/subwatch/return.go server/internal/subwatch/watch_test.go server/internal/subwatch/order_test.go \
  server/internal/bot/bot.go
git commit -m "refactor(endpoint): one definition of what the walk dials

PerAddress, ServerForDial and DialKey leave subwatch for internal/endpoint, with their tests, so the server monitor, the Web UI and the bot derive endpoints exactly as the walk does. The package adds Key, the hex SHA-256 of what a copy dials (a legacy record hashes its flat fields), Keys, one per address, and WANControls, which the watch's reachControls now is."
```

### Task 2: `internal/watchdapi` - the socket contract

The contract between `vpn-director-watchd` and the daemons that ask it: the wire types, `Health`, an HTTP server on a unix socket and its client. Nothing uses it yet; Task 7 implements `Source`, Tasks 12 and 14 use `API`.

**Files:**
- Create: `server/internal/watchdapi/types.go`, `health.go`, `server.go`, `client.go`
- Test: `server/internal/watchdapi/health_test.go`, `api_test.go`

**Interfaces:**
- Produces:
  - `type Status string` with `StatusUnknown`, `StatusAlive`, `StatusDead`, `StatusRejected` (`"unknown"`, `"alive"`, `"dead"`, `"rejected"`).
  - `type State string` with `StateOK`, `StateStopped`, `StateDisabled`, `StateNoXray`, `StateWANDown`, `StateProberError`, `StateNotRunning` (`"ok"`, `"stopped"`, `"disabled"`, `"no_xray"`, `"wan_down"`, `"prober_error"`, `"not_running"`).
  - `type EndpointState struct { Status; LatencyMS int64; CheckedAt, NextAt, Since time.Time; Fails int; Error string }`.
  - `type Snapshot struct { State; Message string; IntervalSeconds, LagSeconds int; UpdatedAt time.Time; Endpoints map[string]EndpointState }`.
  - `var ErrNotActive error`.
  - `type ServerHealth struct { Status; LatencyMS int64; CheckedAt, Since, NextAt time.Time; Error string }` and `func Health(keys []string, snap Snapshot) ServerHealth`.
  - `type Source interface { Snapshot() Snapshot; Request(keys []string) (int, error) }`, `func NewHandler(src Source) http.Handler`, `func Serve(ctx context.Context, path string, src Source) error`.
  - `type API interface { Monitor(ctx) (Snapshot, error); Check(ctx, keys []string) (int, error) }`, `type Client`, `func NewClient(path string) *Client`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/watchdapi/health_test.go`:

```go
package watchdapi

import (
	"testing"
	"time"
)

func snap(states map[string]EndpointState) Snapshot {
	return Snapshot{State: StateOK, Endpoints: states}
}

// A server lives while any of its addresses does: the walk dials every one.
func TestHealth_AliveWhenAnyAddressIsAliveWithTheBestLatency(t *testing.T) {
	s := snap(map[string]EndpointState{
		"a": {Status: StatusDead, Error: "timeout"},
		"b": {Status: StatusAlive, LatencyMS: 300},
		"c": {Status: StatusAlive, LatencyMS: 120},
	})
	h := Health([]string{"a", "b", "c"}, s)
	if h.Status != StatusAlive || h.LatencyMS != 120 {
		t.Fatalf("health %+v, want alive at 120 ms", h)
	}
}

// Without a live address, one not yet checked leaves the question open: the
// server is unknown, not dead. A key the monitor has not seen yet - a refresh
// it has not read - counts as not checked.
func TestHealth_UnknownBeforeDead(t *testing.T) {
	s := snap(map[string]EndpointState{"a": {Status: StatusDead}})
	if h := Health([]string{"a", "gone"}, s); h.Status != StatusUnknown {
		t.Fatalf("health %+v, want unknown", h)
	}
	if h := Health([]string{"a"}, s); h.Status != StatusDead {
		t.Fatalf("health %+v, want dead", h)
	}
}

func TestHealth_DeadAsTheLatestCheckFoundIt(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	s := snap(map[string]EndpointState{
		"a": {Status: StatusDead, CheckedAt: t0, Error: "timeout"},
		"b": {Status: StatusDead, CheckedAt: t0.Add(time.Minute), Error: "HTTP 403"},
		"c": {Status: StatusRejected, Error: "crashes Xray"},
	})
	if h := Health([]string{"a", "b", "c"}, s); h.Status != StatusDead || h.Error != "HTTP 403" {
		t.Fatalf("health %+v, want dead with the latest error", h)
	}
	if h := Health([]string{"c"}, s); h.Status != StatusRejected || h.Error != "crashes Xray" {
		t.Fatalf("health %+v, want rejected with its reason", h)
	}
}

func TestHealth_NoKeysIsUnknown(t *testing.T) {
	if h := Health(nil, snap(nil)); h.Status != StatusUnknown {
		t.Fatalf("health %+v", h)
	}
}
```

Create `server/internal/watchdapi/api_test.go`:

```go
package watchdapi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeSource is a monitor the socket serves in these tests.
type fakeSource struct {
	mu       sync.Mutex
	snap     Snapshot
	err      error
	requests [][]string
}

func (f *fakeSource) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSource) Request(keys []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, keys)
	if f.err != nil {
		return 0, f.err
	}
	if len(keys) == 0 {
		return len(f.snap.Endpoints), nil
	}
	return len(keys), nil
}

// serve starts Serve on a socket in a short temp dir - a unix socket path
// holds at most 108 bytes, more than t.TempDir leaves for a long test name -
// and returns its client.
func serve(t *testing.T, src Source) (*Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "watchd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, src) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return NewClient(path), path
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the socket never answered")
	return nil, ""
}

func TestClient_MonitorReadsTheSnapshot(t *testing.T) {
	want := Snapshot{
		State: StateOK, IntervalSeconds: 60,
		UpdatedAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
		Endpoints: map[string]EndpointState{"k": {Status: StatusAlive, LatencyMS: 142}},
	}
	c, _ := serve(t, &fakeSource{snap: want})

	got, err := c.Monitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %+v, want %+v", got, want)
	}
}

func TestClient_CheckQueuesTheKeysOrEveryEndpoint(t *testing.T) {
	src := &fakeSource{snap: Snapshot{Endpoints: map[string]EndpointState{"a": {}, "b": {}, "c": {}}}}
	c, _ := serve(t, src)

	if n, err := c.Check(context.Background(), []string{"a"}); err != nil || n != 1 {
		t.Fatalf("Check(a) = %d, %v", n, err)
	}
	if n, err := c.Check(context.Background(), nil); err != nil || n != 3 {
		t.Fatalf("Check() = %d, %v", n, err)
	}
	if len(src.requests) != 2 || !reflect.DeepEqual(src.requests[0], []string{"a"}) || len(src.requests[1]) != 0 {
		t.Fatalf("requests %v", src.requests)
	}
}

func TestClient_CheckOfAStoppedMonitorIsErrNotActive(t *testing.T) {
	c, _ := serve(t, &fakeSource{err: ErrNotActive, snap: Snapshot{State: StateStopped}})

	if _, err := c.Check(context.Background(), nil); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Check() error %v, want ErrNotActive", err)
	}
}

// The socket is root's alone: every daemon here runs as root, and nobody else
// may queue checks or read which servers exist.
func TestServe_TheSocketIsMode0600(t *testing.T) {
	_, path := serve(t, &fakeSource{})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("socket mode %o, want 600", mode)
	}
}

// A socket file an earlier run left behind does not keep the daemon from
// listening again.
func TestServe_ReplacesAStaleSocketFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "watchd.sock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, &fakeSource{}) }()
	var client *Client
	for i := 0; i < 100 && client == nil; i++ {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			client = NewClient(path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		cancel()
		t.Fatalf("Serve did not listen over the stale file: %v", <-done)
	}
	if _, err := client.Monitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve() = %v", err)
	}
}

// A daemon that is not there answers nothing, fast.
func TestClient_NoDaemonIsAnErrorWithinTheBound(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "absent.sock"))
	start := time.Now()
	if _, err := c.Monitor(context.Background()); err == nil {
		t.Fatal("Monitor() of an absent socket succeeded")
	}
	if d := time.Since(start); d > clientTimeout {
		t.Fatalf("took %s", d)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/watchdapi/ -count=1`
Expected: FAIL - `undefined: Snapshot`, `undefined: Health`, `undefined: Serve`, and more.

- [ ] **Step 3: Write the package**

Create `server/internal/watchdapi/types.go`:

```go
// Package watchdapi is the contract between vpn-director-watchd and the
// daemons that ask it: the state of the server monitor, served over a unix
// socket, a client for it, and Health, which folds the states of a server's
// endpoints into the server's status.
package watchdapi

import (
	"errors"
	"time"
)

// Status is what the monitor knows of one endpoint.
type Status string

const (
	// StatusUnknown is an endpoint not checked yet.
	StatusUnknown Status = "unknown"
	// StatusAlive is an endpoint whose last check got its answer.
	StatusAlive Status = "alive"
	// StatusDead is an endpoint whose last check failed, after its retry.
	StatusDead Status = "dead"
	// StatusRejected is an endpoint the prober does not hold: the generator or
	// Xray refused its outbound, or it crashed Xray.
	StatusRejected Status = "rejected"
)

// State is what the monitor as a whole is doing.
type State string

const (
	StateOK          State = "ok"
	StateStopped     State = "stopped"      // VPN Director is stopped
	StateDisabled    State = "disabled"     // monitor.enabled is false
	StateNoXray      State = "no_xray"      // no xray on PATH
	StateWANDown     State = "wan_down"     // no control address answers
	StateProberError State = "prober_error" // the prober cannot start; Message says why
	// StateNotRunning is no answer of the daemon's: the Web UI and the bot
	// report it when the socket does not answer.
	StateNotRunning State = "not_running"
)

// EndpointState is the monitor's record of one endpoint.
type EndpointState struct {
	Status Status `json:"status"`
	// LatencyMS is the time to the response headers of the last success.
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	NextAt    time.Time `json:"next_at"`
	// Since is when the endpoint took its status.
	Since time.Time `json:"since"`
	// Fails counts failed checks in a row.
	Fails int `json:"fails"`
	// Error is why the last check failed, or why the endpoint is rejected.
	Error string `json:"error,omitempty"`
}

// Snapshot is GET /v1/monitor: the whole state, endpoints by key.
type Snapshot struct {
	State           State                    `json:"state"`
	Message         string                   `json:"message,omitempty"`
	IntervalSeconds int                      `json:"interval_seconds"`
	LagSeconds      int                      `json:"lag_seconds"`
	UpdatedAt       time.Time                `json:"updated_at"`
	Endpoints       map[string]EndpointState `json:"endpoints"`
}

// ErrNotActive is a check asked of a monitor that is stopped or disabled.
var ErrNotActive = errors.New("the monitor is stopped or disabled")
```

Create `server/internal/watchdapi/health.go`:

```go
package watchdapi

import "time"

// ServerHealth is a server's status folded from its endpoints (Health).
type ServerHealth struct {
	Status    Status    `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	Since     time.Time `json:"since"`
	NextAt    time.Time `json:"next_at"`
	Error     string    `json:"error,omitempty"`
}

// Health folds the endpoints of one server - its keys, one per address
// (endpoint.Keys) - into one status: alive when any address is alive, with the
// best latency; otherwise unknown when any address is not yet checked, or not
// in snap at all; otherwise dead when any is dead, as the latest check found
// it; otherwise rejected.
func Health(keys []string, snap Snapshot) ServerHealth {
	var alive, unknown, dead, rejected *EndpointState
	for _, k := range keys {
		st, ok := snap.Endpoints[k]
		if !ok || st.Status == "" {
			st = EndpointState{Status: StatusUnknown}
		}
		switch st.Status {
		case StatusAlive:
			if alive == nil || st.LatencyMS < alive.LatencyMS {
				alive = &st
			}
		case StatusDead:
			if dead == nil || st.CheckedAt.After(dead.CheckedAt) {
				dead = &st
			}
		case StatusRejected:
			if rejected == nil {
				rejected = &st
			}
		default:
			if unknown == nil {
				unknown = &st
			}
		}
	}
	pick := alive
	for _, next := range []*EndpointState{unknown, dead, rejected} {
		if pick == nil {
			pick = next
		}
	}
	if pick == nil {
		return ServerHealth{Status: StatusUnknown}
	}
	return ServerHealth{
		Status:    pick.Status,
		LatencyMS: pick.LatencyMS,
		CheckedAt: pick.CheckedAt,
		Since:     pick.Since,
		NextAt:    pick.NextAt,
		Error:     pick.Error,
	}
}
```

Create `server/internal/watchdapi/server.go`:

```go
package watchdapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Source is what the socket serves: the monitor, in the daemon.
type Source interface {
	Snapshot() Snapshot
	// Request makes the endpoints of keys - every endpoint when keys is empty -
	// due ahead of the rest and answers how many it queued; ErrNotActive
	// while the monitor is stopped or disabled.
	Request(keys []string) (int, error)
}

type checkRequest struct {
	Keys []string `json:"keys"`
}

type checkResponse struct {
	Queued int `json:"queued"`
}

type errorResponse struct {
	Error string `json:"error"`
	State State  `json:"state,omitempty"`
}

// NewHandler serves GET /v1/monitor and POST /v1/monitor/check for src.
func NewHandler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/monitor", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, src.Snapshot())
	})
	mux.HandleFunc("POST /v1/monitor/check", func(w http.ResponseWriter, r *http.Request) {
		var req checkRequest
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		// An empty body is a check of every endpoint, like {}.
		if err := json.NewDecoder(body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
			return
		}
		n, err := src.Request(req.Keys)
		if errors.Is(err, ErrNotActive) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error(), State: src.Snapshot().State})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, checkResponse{Queued: n})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve answers the API on the unix socket at path until ctx ends. A socket
// file an earlier run left behind is removed first; the new one is mode 0600,
// so only root - every daemon here runs as root - can ask.
func Serve(ctx context.Context, path string, src Source) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{Handler: NewHandler(src), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

Create `server/internal/watchdapi/client.go`:

```go
package watchdapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// API is what the Web UI and the bot ask of vpn-director-watchd. *Client
// implements it; the handlers' tests fake it.
type API interface {
	Monitor(ctx context.Context) (Snapshot, error)
	// Check queues the endpoints of keys, every endpoint when keys is empty,
	// and answers how many; ErrNotActive while the monitor is stopped or
	// disabled.
	Check(ctx context.Context, keys []string) (int, error)
}

// clientTimeout bounds every request, so a hung daemon cannot stall a page or
// a command. A var so a test can shorten it.
var clientTimeout = 2 * time.Second

// Client asks the daemon over its unix socket.
type Client struct {
	http *http.Client
}

var _ API = (*Client)(nil)

// NewClient returns a client of the socket at path.
func NewClient(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// Monitor returns the monitor's state.
func (c *Client) Monitor(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://watchd/v1/monitor", nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("vpn-director-watchd answered %d", resp.StatusCode)
	}
	var s Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// Check queues the endpoints of keys, every endpoint when keys is empty.
func (c *Client) Check(ctx context.Context, keys []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	body, err := json.Marshal(checkRequest{Keys: keys})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://watchd/v1/monitor/check", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		var r checkResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return 0, err
		}
		return r.Queued, nil
	case http.StatusConflict:
		return 0, ErrNotActive
	default:
		return 0, fmt.Errorf("vpn-director-watchd answered %d", resp.StatusCode)
	}
}
```

- [ ] **Step 4: Run the tests, with the race detector too**

Run: `cd server && go vet ./internal/watchdapi/ && go test ./internal/watchdapi/ -count=1 && go test -race ./internal/watchdapi/ -count=3`
Expected: `ok` both times, no race report.

- [ ] **Step 5: Commit**

```bash
git add server/internal/watchdapi/types.go server/internal/watchdapi/health.go server/internal/watchdapi/server.go \
  server/internal/watchdapi/client.go server/internal/watchdapi/health_test.go server/internal/watchdapi/api_test.go
git commit -m "feat(watchdapi): the server monitor's socket contract

The types vpn-director-watchd serves, HTTP over a unix socket of mode 0600 (GET /v1/monitor, POST /v1/monitor/check, 409 while the monitor is stopped or disabled), a client bounding every request by 2 s, and Health, which folds a server's endpoints into one status: alive when any address is, else unknown, else dead, else rejected."
```

### Task 3: The `monitor` section and its settings

`vpn-director.json` gains a `monitor` section. It goes into the Go struct - a daemon's write drops every key `VPNDirectorConfig` does not know - and into the template, which `configure.sh` merges under the existing config, so the defaults arrive with an update. `monitor.SettingsFrom` fills in defaults and bounds.

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`MonitorConfig`, the `Monitor` field)
- Test: `server/internal/vpnconfig/vpnconfig_test.go`
- Modify: `router/opt/vpn-director/vpn-director.json.template`
- Create: `server/internal/monitor/settings.go` (the package doc lives here)
- Test: `server/internal/monitor/settings_test.go`

**Interfaces:**
- Produces:
  - `vpnconfig.MonitorConfig { Enabled *bool; Interval, DeadIntervalMax string; Concurrency int; LogLevel string }` (JSON `enabled`, `interval`, `dead_interval_max`, `concurrency`, `log_level`), and `VPNDirectorConfig.Monitor *MonitorConfig` (`json:"monitor,omitempty"`).
  - `monitor.Settings { Enabled bool; Interval, DeadMax time.Duration; Concurrency int; LogLevel string }`.
  - `monitor.SettingsFrom(c *vpnconfig.MonitorConfig) (Settings, []string)` - the second value is one warning per value out of bounds.
  - Constants `monitor.DefaultInterval` (1m), `DefaultDeadMax` (30m), `DefaultConcurrency` (8), `MinInterval` (10s), `MaxConcurrency` (32).

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/vpnconfig/vpnconfig_test.go`:

```go
// A daemon's write keeps the monitor section - VPNDirectorConfig drops every
// key it does not know - and a config without one stays without one.
func TestVPNDirectorConfig_KeepsTheMonitorSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpn-director.json")
	if err := os.WriteFile(path, []byte(`{"data_dir":"/d","monitor":{"enabled":false,"interval":"2m"},"xray":{},"tunnel_director":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor == nil || cfg.Monitor.Enabled == nil || *cfg.Monitor.Enabled || cfg.Monitor.Interval != "2m" {
		t.Fatalf("monitor %+v", cfg.Monitor)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"interval": "2m"`) || !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("saved %s", data)
	}

	if err := os.WriteFile(path, []byte(`{"data_dir":"/d","xray":{},"tunnel_director":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadVPNDirectorConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "monitor") {
		t.Fatalf("a section nobody wrote appeared: %s", data)
	}
}
```

Create `server/internal/monitor/settings_test.go`:

```go
package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestSettingsFrom_NoSectionIsTheDefaults(t *testing.T) {
	s, warns := SettingsFrom(nil)
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want || len(warns) != 0 {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

func TestSettingsFrom_ReadsEveryKey(t *testing.T) {
	off := false
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Enabled: &off, Interval: "2m", DeadIntervalMax: "1h", Concurrency: 4, LogLevel: "debug"})
	want := Settings{Enabled: false, Interval: 2 * time.Minute, DeadMax: time.Hour, Concurrency: 4, LogLevel: "debug"}
	if s != want || len(warns) != 0 {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

// A value out of bounds takes its default and says so; it never stops the
// monitor.
func TestSettingsFrom_AValueOutOfBoundsTakesItsDefault(t *testing.T) {
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "5s", DeadIntervalMax: "soon", Concurrency: 100})
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want {
		t.Fatalf("settings %+v", s)
	}
	if len(warns) != 3 || !strings.Contains(warns[0], "monitor.interval") || !strings.Contains(warns[1], "monitor.dead_interval_max") || !strings.Contains(warns[2], "monitor.concurrency") {
		t.Fatalf("warnings %v", warns)
	}
}

// A dead endpoint's first pause is twice the interval, so the cap is never
// below that, even when the default cap is.
func TestSettingsFrom_TheCapIsAtLeastTwiceTheInterval(t *testing.T) {
	s, _ := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "20m"})
	if s.DeadMax != 40*time.Minute {
		t.Fatalf("cap %s, want 40m", s.DeadMax)
	}
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "20m", DeadIntervalMax: "30m"})
	if s.DeadMax != 40*time.Minute || len(warns) != 1 {
		t.Fatalf("cap %s, warnings %v", s.DeadMax, warns)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/vpnconfig/ ./internal/monitor/ -count=1`
Expected: FAIL - `cfg.Monitor undefined`, `undefined: SettingsFrom`, `undefined: Settings`.

- [ ] **Step 3: Add the section to the config**

In `server/internal/vpnconfig/vpnconfig.go`, replace:

```go
type VPNDirectorConfig struct {
	DataDir        string                 `json:"data_dir"`
	WebUI          WebUIConfig            `json:"webui,omitempty"`
```

with:

```go
// MonitorConfig is the monitor section: how vpn-director-watchd checks the
// servers of every subscription. Every key is optional, and the daemon fills
// in its defaults (monitor.SettingsFrom). Enabled is a pointer so that a
// missing key reads as the default, true.
type MonitorConfig struct {
	Enabled         *bool  `json:"enabled,omitempty"`
	Interval        string `json:"interval,omitempty"`
	DeadIntervalMax string `json:"dead_interval_max,omitempty"`
	Concurrency     int    `json:"concurrency,omitempty"`
	LogLevel        string `json:"log_level,omitempty"`
}

type VPNDirectorConfig struct {
	DataDir        string                 `json:"data_dir"`
	WebUI          WebUIConfig            `json:"webui,omitempty"`
	Monitor        *MonitorConfig         `json:"monitor,omitempty"`
```

In `router/opt/vpn-director/vpn-director.json.template`, insert the section after `webui`:

```json
  "webui": {
    "port": 8444,
    "cert_file": "/opt/vpn-director/certs/server.crt",
    "key_file": "/opt/vpn-director/certs/server.key",
    "jwt_secret": "",
    "log_level": "info"
  },
  "monitor": {
    "enabled": true,
    "interval": "1m",
    "dead_interval_max": "30m",
    "concurrency": 8,
    "log_level": "info"
  },
  "tunnel_director": {
```

- [ ] **Step 4: Write the settings**

Create `server/internal/monitor/settings.go`:

```go
// Package monitor checks, minute by minute, whether every server of every
// subscription carries traffic. A separate Xray process, the prober, holds one
// outbound per endpoint (one address of one server, as the walk dials it), and
// a check fetches ProbeURL through it. The live Xray is never touched.
package monitor

import (
	"fmt"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Defaults and bounds of the monitor section.
const (
	DefaultInterval    = time.Minute
	DefaultDeadMax     = 30 * time.Minute
	DefaultConcurrency = 8
	MinInterval        = 10 * time.Second
	MaxConcurrency     = 32
)

// Settings is the monitor section with its defaults filled in.
type Settings struct {
	Enabled bool
	// Interval is the pause between two checks of a live endpoint.
	Interval time.Duration
	// DeadMax caps the pause of a dead endpoint, which doubles from
	// 2 × Interval.
	DeadMax     time.Duration
	Concurrency int
	LogLevel    string
}

// SettingsFrom resolves the monitor section. A missing key takes its default,
// and so does a value out of bounds, with a warning naming it.
func SettingsFrom(c *vpnconfig.MonitorConfig) (Settings, []string) {
	s := Settings{Enabled: true, Interval: DefaultInterval, DeadMax: DefaultDeadMax, Concurrency: DefaultConcurrency}
	var warns []string
	if c == nil {
		return s, nil
	}
	if c.Enabled != nil {
		s.Enabled = *c.Enabled
	}
	s.LogLevel = c.LogLevel
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil || d < MinInterval {
			warns = append(warns, fmt.Sprintf("monitor.interval %q is not a duration of at least %s; using %s", c.Interval, MinInterval, DefaultInterval))
		} else {
			s.Interval = d
		}
	}
	s.DeadMax = max(DefaultDeadMax, 2*s.Interval)
	if c.DeadIntervalMax != "" {
		d, err := time.ParseDuration(c.DeadIntervalMax)
		if err != nil || d < 2*s.Interval {
			warns = append(warns, fmt.Sprintf("monitor.dead_interval_max %q is not a duration of at least twice the interval; using %s", c.DeadIntervalMax, s.DeadMax))
		} else {
			s.DeadMax = d
		}
	}
	if c.Concurrency != 0 {
		if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
			warns = append(warns, fmt.Sprintf("monitor.concurrency %d is not between 1 and %d; using %d", c.Concurrency, MaxConcurrency, DefaultConcurrency))
		} else {
			s.Concurrency = c.Concurrency
		}
	}
	return s, warns
}
```

- [ ] **Step 5: Run the tests**

Run: `cd server && gofmt -l internal/vpnconfig internal/monitor && go test ./internal/vpnconfig/ ./internal/monitor/ -count=1 && jq -e '.monitor.interval == "1m"' ../router/opt/vpn-director/vpn-director.json.template`
Expected: `gofmt -l` prints nothing; `ok` for both packages; `true`.

- [ ] **Step 6: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go \
  router/opt/vpn-director/vpn-director.json.template server/internal/monitor/settings.go server/internal/monitor/settings_test.go
git commit -m "feat(monitor): the monitor section and its settings

vpn-director.json gains a monitor section (enabled, interval, dead_interval_max, concurrency, log_level), in the Go struct so that a daemon's write keeps it and in the template so that configure.sh brings the defaults. SettingsFrom resolves it: a missing key takes its default, a value out of bounds its default with a warning, and a dead endpoint's cap is never below twice the interval."
```

### Task 4: The endpoint set

`monitor.Build` turns the subscriptions into the endpoints the prober holds: one per key, each with the outbound `Generate` would write, the active server's first. What `Generate` refuses - the xhttp outbounds that make Xray panic on their first dial - is no endpoint but a refusal with its reason, through the same code (`service.OutboundJSON` wraps `serverOutbound`).

**Files:**
- Modify: `server/internal/service/xray.go` (add `OutboundJSON`)
- Test: `server/internal/service/xray_test.go`
- Create: `server/internal/monitor/endpoints.go`
- Test: `server/internal/monitor/endpoints_test.go`

**Interfaces:**
- Consumes: `endpoint.PerAddress`, `endpoint.ServerForDial`, `endpoint.Key`, `endpoint.Keys` (Task 1).
- Produces:
  - `service.OutboundJSON(server vpnconfig.Server, tag string) (json.RawMessage, error)`.
  - `monitor.Endpoint { Key string; Label string; Outbound json.RawMessage }` - `Label` is `"<subscription> / <server>"`.
  - `monitor.Build(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer, outbound func(vpnconfig.Server) (json.RawMessage, error)) (eps []Endpoint, refused map[string]string)`.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/service/xray_test.go`:

```go
// The monitor's prober holds the outbound Generate would write, under a tag of
// its own, and refuses what Generate refuses.
func TestOutboundJSON_TagsTheOutboundGenerateWouldWrite(t *testing.T) {
	stored := vpnconfig.Server{Name: "Oslo", Address: "192.0.2.10", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"192.0.2.10","port":443,"password":"p"}]},"tag":"proxy-out"}`)}
	raw, err := OutboundJSON(stored, "m7")
	if err != nil {
		t.Fatal(err)
	}
	var ob map[string]interface{}
	if err := json.Unmarshal(raw, &ob); err != nil {
		t.Fatal(err)
	}
	if ob["tag"] != "m7" || ob["protocol"] != "trojan" {
		t.Fatalf("outbound %s", raw)
	}

	legacy := vpnconfig.Server{Name: "Legacy", Address: "192.0.2.11", Port: 443, UUID: "u", Security: "tls", SNI: "l.example"}
	raw, err = OutboundJSON(legacy, "m8")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ob); err != nil {
		t.Fatal(err)
	}
	if ob["tag"] != "m8" || ob["protocol"] != "vless" {
		t.Fatalf("legacy outbound %s", raw)
	}

	refused := vpnconfig.Server{Name: "X", Address: "192.0.2.12", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.12","port":443,"users":[{"id":"u"}]}]},"streamSettings":{"network":"xhttp","security":"tls","xhttpSettings":{"extra":{"downloadSettings":{}}}}}`)}
	if _, err := OutboundJSON(refused, "m9"); err == nil || !strings.Contains(err.Error(), "downloadSettings without an address") {
		t.Fatalf("err %v, want the refusal Generate makes", err)
	}
}
```

Create `server/internal/monitor/endpoints_test.go`:

```go
package monitor

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func trojan(address string) json.RawMessage {
	return json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + address + `","port":443,"password":"p"}]}}`)
}

// outboundAsIs stands in for service.OutboundJSON: the stored outbound, and a
// refusal for a server named "Refused".
func outboundAsIs(s vpnconfig.Server) (json.RawMessage, error) {
	if s.Name == "Refused" {
		return nil, errors.New("stored outbound: xhttp downloadSettings without an address")
	}
	return s.Outbound, nil
}

// One provider lists many names on few endpoints: the monitor checks each
// endpoint once, whatever names and subscriptions share it, and every address
// of a server apart.
func TestBuild_OneEndpointPerKey(t *testing.T) {
	de := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1", "192.0.2.2"}, Outbound: trojan("de.example")}
	twin := de
	twin.Name = "Germany-2"
	subs := []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{de, twin}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{de}},
	}

	eps, refused := Build(subs, nil, outboundAsIs)

	if len(eps) != 2 || len(refused) != 0 {
		t.Fatalf("endpoints %+v, refused %v", eps, refused)
	}
	keys := endpoint.Keys(de)
	if eps[0].Key != keys[0] || eps[1].Key != keys[1] || eps[0].Label != "Alpha / Germany-1" {
		t.Fatalf("endpoints %+v, want the keys %v", eps, keys)
	}
	var ob map[string]interface{}
	if err := json.Unmarshal(eps[1].Outbound, &ob); err != nil {
		t.Fatal(err)
	}
	servers := ob["settings"].(map[string]interface{})["servers"].([]interface{})
	if addr := servers[0].(map[string]interface{})["address"]; addr != "192.0.2.2" {
		t.Fatalf("second endpoint dials %v, want its own address", addr)
	}
}

// At a rebuild the running server's endpoints are checked first.
func TestBuild_TheActiveServerComesFirst(t *testing.T) {
	a := vpnconfig.Server{Name: "Oslo", Address: "a.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: trojan("a.example")}
	b := vpnconfig.Server{Name: "Riga", Address: "b.example", Port: 443, IPs: []string{"192.0.2.2"}, Outbound: trojan("b.example")}
	subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{a, b}}}
	active := &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Riga", Address: "b.example", Port: 443}

	eps, _ := Build(subs, active, outboundAsIs)

	if len(eps) != 2 || eps[0].Label != "Alpha / Riga" {
		t.Fatalf("endpoints %+v", eps)
	}
}

func TestBuild_ARefusedOutboundIsNoEndpoint(t *testing.T) {
	bad := vpnconfig.Server{Name: "Refused", Address: "x.example", Port: 443, IPs: []string{"192.0.2.9"}, Outbound: trojan("x.example")}
	subs := []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: []vpnconfig.Server{bad}}}

	eps, refused := Build(subs, nil, outboundAsIs)

	key := endpoint.Keys(bad)[0]
	if len(eps) != 0 || refused[key] != "stored outbound: xhttp downloadSettings without an address" {
		t.Fatalf("endpoints %+v, refused %v", eps, refused)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/service/ ./internal/monitor/ -count=1`
Expected: FAIL - `undefined: OutboundJSON`, `undefined: Build`, `undefined: Endpoint`.

- [ ] **Step 3: Write `OutboundJSON`**

In `server/internal/service/xray.go`, insert above the comment of `downloadWithoutAddress`:

```go
// OutboundJSON is the outbound Generate would write for server, as JSON and
// tagged tag: the outbound its import stored, or for a record from before
// outbounds were stored the one built from its flat VLESS fields. It refuses
// what Generate refuses. The server monitor's prober holds one per endpoint.
func OutboundJSON(server vpnconfig.Server, tag string) (json.RawMessage, error) {
	ob, err := serverOutbound(server)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["tag"] = tag
	return json.Marshal(m)
}
```

- [ ] **Step 4: Write `Build`**

Create `server/internal/monitor/endpoints.go`:

```go
package monitor

import (
	"encoding/json"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Endpoint is one address of one server as the prober dials it.
type Endpoint struct {
	Key string
	// Label names the first server that dials it, "<subscription> / <server>",
	// for the log.
	Label string
	// Outbound is what Generate would write for it; the prober tags it.
	Outbound json.RawMessage
}

// Build lists the endpoints of subs: each server once per address
// (endpoint.PerAddress), the address in its outbound (endpoint.ServerForDial),
// one endpoint per key. The endpoints of the active server come first. A
// server whose outbound the generator refuses is no endpoint: refused maps its
// key to the reason.
func Build(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer, outbound func(vpnconfig.Server) (json.RawMessage, error)) (eps []Endpoint, refused map[string]string) {
	refused = map[string]string{}
	seen := map[string]bool{}
	var first, rest []Endpoint
	for _, sub := range subs {
		for _, s := range sub.Servers {
			s.Subscription = sub.ID
			isActive := active != nil && active.Subscription == sub.ID && active.Name == s.Name &&
				active.Address == s.Address && active.Port == s.Port
			for _, c := range endpoint.PerAddress([]vpnconfig.Server{s}) {
				key := endpoint.Key(c)
				if seen[key] {
					continue
				}
				seen[key] = true
				ob, err := outbound(endpoint.ServerForDial(c))
				if err != nil {
					refused[key] = err.Error()
					continue
				}
				ep := Endpoint{Key: key, Label: sub.Name + " / " + s.Name, Outbound: ob}
				if isActive {
					first = append(first, ep)
				} else {
					rest = append(rest, ep)
				}
			}
		}
	}
	return append(first, rest...), refused
}
```

- [ ] **Step 5: Run the tests**

Run: `cd server && gofmt -l internal/service internal/monitor && go test ./internal/service/ ./internal/monitor/ -count=1`
Expected: `gofmt -l` prints nothing; `ok` for both packages.

- [ ] **Step 6: Commit**

```bash
git add server/internal/service/xray.go server/internal/service/xray_test.go \
  server/internal/monitor/endpoints.go server/internal/monitor/endpoints_test.go
git commit -m "feat(monitor): the endpoint set

Build lists each server once per address, as the walk dials it, one endpoint per key, the active server's first. service.OutboundJSON gives each the outbound Generate would write; what Generate refuses is no endpoint but a refusal with its reason, through the same code."
```

### Task 5: The prober's config and one check

The config the prober runs - a blackhole first, one outbound per endpoint, one SOCKS inbound with an account per endpoint, a rule per account - and one check through it. The tests run a small SOCKS5 server of their own; no Xray is needed.

**Files:**
- Create: `server/internal/monitor/probeconfig.go`, `server/internal/monitor/check.go`
- Test: `server/internal/monitor/probeconfig_test.go`, `server/internal/monitor/check_test.go`
- Create (generated): `server/internal/monitor/testdata/probe_config.golden.json`

**Interfaces:**
- Consumes: `monitor.Endpoint` (Task 4).
- Produces:
  - `type account struct{ User, Pass string }`, `newAccounts(n int) ([]account, error)` - users `e0`..`e<n-1>`, 32-hex-digit random passwords.
  - `probeConfig(eps []Endpoint, accounts []account, port int) ([]byte, error)`.
  - `const ProbeURL = "http://www.gstatic.com/generate_204"`, `var checkTimeout = 10 * time.Second`.
  - `probeGet(ctx, socksAddr, user, pass, url string) (time.Duration, error)` and `classify(err error) string` (`"HTTP <code>"`, `"timeout"`, `"connection closed"`).
  - Test helpers later tasks use: `twoEndpoints() []Endpoint` (keys `k0`, `k1`) in `probeconfig_test.go`; `socks5Server(t, user, pass) string` and `answering(t, h) string` in `check_test.go`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/monitor/probeconfig_test.go`:

```go
package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func twoEndpoints() []Endpoint {
	return []Endpoint{
		{Key: "k0", Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"192.0.2.1","port":443,"password":"p"}]}}`)},
		{Key: "k1", Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.2","port":443,"users":[{"id":"u","encryption":"none"}]}]},"tag":"proxy-out"}`)},
	}
}

// The golden file is the exact config the prober runs. Regenerate with:
//
//	UPDATE_GOLDEN=1 go test ./internal/monitor -run TestProbeConfig_Golden -count=1
func TestProbeConfig_Golden(t *testing.T) {
	got, err := probeConfig(twoEndpoints(), []account{{User: "e0", Pass: "p0"}, {User: "e1", Pass: "p1"}}, 20000)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "probe_config.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with UPDATE_GOLDEN=1)", err)
	}
	if string(want) != string(got) {
		t.Errorf("config differs from %s:\n%s", golden, got)
	}
}

// Nothing leaves the router directly: traffic no rule names goes to the
// blackhole, Xray's first outbound, and each account reaches its own
// outbound alone.
func TestProbeConfig_EveryAccountReachesItsOwnOutboundAndNothingElse(t *testing.T) {
	raw, err := probeConfig(twoEndpoints(), []account{{User: "e0", Pass: "p0"}, {User: "e1", Pass: "p1"}}, 20000)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Inbounds []struct {
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Settings struct {
				Auth string `json:"auth"`
			} `json:"settings"`
		} `json:"inbounds"`
		Outbounds []struct {
			Tag      string `json:"tag"`
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
		Routing struct {
			Rules []struct {
				User        []string `json:"user"`
				OutboundTag string   `json:"outboundTag"`
			} `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outbounds) != 3 || cfg.Outbounds[0].Protocol != "blackhole" || cfg.Outbounds[1].Tag != "m0" || cfg.Outbounds[2].Tag != "m1" {
		t.Fatalf("outbounds %+v", cfg.Outbounds)
	}
	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Listen != "127.0.0.1" || cfg.Inbounds[0].Port != 20000 || cfg.Inbounds[0].Settings.Auth != "password" {
		t.Fatalf("inbounds %+v", cfg.Inbounds)
	}
	if len(cfg.Routing.Rules) != 2 || cfg.Routing.Rules[1].User[0] != "e1" || cfg.Routing.Rules[1].OutboundTag != "m1" {
		t.Fatalf("rules %+v", cfg.Routing.Rules)
	}
}

func TestNewAccounts_OnePerEndpointWithItsOwnPassword(t *testing.T) {
	a, err := newAccounts(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || a[0].User != "e0" || a[2].User != "e2" || a[0].Pass == a[1].Pass || len(a[0].Pass) != 32 {
		t.Fatalf("accounts %+v", a)
	}
}
```

Create `server/internal/monitor/check_test.go`:

```go
package monitor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// socks5Server is a SOCKS5 proxy for these tests, standing in for the prober:
// one account, CONNECT only, and it dials the target itself.
func socks5Server(t *testing.T, user, pass string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(c, user, pass)
		}
	}()
	return l.Addr().String()
}

func serveSOCKS5(c net.Conn, user, pass string) {
	defer c.Close()
	b := make([]byte, 256)
	read := func(n int) []byte {
		if _, err := io.ReadFull(c, b[:n]); err != nil {
			return nil
		}
		return b[:n]
	}
	if h := read(2); h == nil || read(int(h[1])) == nil { // VER NMETHODS METHODS
		return
	}
	c.Write([]byte{5, 2}) // username/password
	h := read(2)          // VER ULEN
	if h == nil {
		return
	}
	u := string(read(int(h[1])))
	p := string(read(int(read(1)[0])))
	if u != user || p != pass {
		c.Write([]byte{1, 1})
		return
	}
	c.Write([]byte{1, 0})
	req := read(4) // VER CMD RSV ATYP
	if req == nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		host = net.IP(append([]byte(nil), read(4)...)).String()
	case 3:
		host = string(read(int(read(1)[0])))
	default:
		return
	}
	pb := read(2)
	port := int(pb[0])<<8 | int(pb[1])
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(target, c)
	io.Copy(c, target)
}

func answering(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/generate_204"
}

func TestProbeGet_A204ThroughTheAccountIsASuccess(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	latency, err := probeGet(context.Background(), proxyAddr, "e0", "secret", url)
	if err != nil || latency <= 0 {
		t.Fatalf("probeGet() = %s, %v", latency, err)
	}
}

// A portal page, a block page, a redirect: anything but 204 is no internet.
func TestProbeGet_AnyOtherAnswerIsAFailure(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	for code, h := range map[int]http.HandlerFunc{
		403: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		302: func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		200: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>login</html>")) },
	} {
		_, err := probeGet(context.Background(), proxyAddr, "e0", "secret", answering(t, h))
		if got, want := classify(err), "HTTP "+strconv.Itoa(code); got != want {
			t.Errorf("answer %d classified %q (%v), want %q", code, got, err, want)
		}
	}
}

// The prober refuses another account's password, so a local process cannot
// borrow it.
func TestProbeGet_AnotherPasswordIsRefused(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	if _, err := probeGet(context.Background(), proxyAddr, "e0", "guess", url); err == nil {
		t.Fatal("a wrong password got through")
	}
}

func TestClassify_ATimeoutIsATimeout(t *testing.T) {
	old := checkTimeout
	checkTimeout = 200 * time.Millisecond
	defer func() { checkTimeout = old }()
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	_, err := probeGet(context.Background(), proxyAddr, "e0", "secret", url)
	if got := classify(err); got != "timeout" {
		t.Fatalf("classified %q (%v), want timeout", got, err)
	}
	if got := classify(errors.New("read: connection reset by peer")); got != "connection closed" {
		t.Fatalf("classified %q", got)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/monitor/ -count=1`
Expected: FAIL - `undefined: probeConfig`, `undefined: account`, `undefined: probeGet`, `undefined: classify`.

- [ ] **Step 3: Write the config and the check**

Create `server/internal/monitor/probeconfig.go`:

```go
package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// account is the SOCKS login of one endpoint: its traffic goes to its
// outbound alone.
type account struct {
	User string
	Pass string
}

// newAccounts gives each of n endpoints an account e<i> with a random
// password, new at every start, so no local process can use the prober as a
// proxy to every server.
func newAccounts(n int) ([]account, error) {
	accounts := make([]account, n)
	for i := range accounts {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		accounts[i] = account{User: fmt.Sprintf("e%d", i), Pass: hex.EncodeToString(b)}
	}
	return accounts, nil
}

// probeConfig is the prober's Xray config. The first outbound is a blackhole:
// Xray sends whatever matches no rule to its first outbound, so nothing leaves
// the router directly. One outbound per endpoint follows, tagged m<i>. One
// SOCKS inbound on 127.0.0.1:port takes a password and holds accounts[i] for
// eps[i], and a rule sends each account's traffic to its outbound. Xray logs
// to stderr, which the daemon reads; the live Xray's log stays untouched.
func probeConfig(eps []Endpoint, accounts []account, port int) ([]byte, error) {
	outbounds := []interface{}{map[string]interface{}{"tag": "block", "protocol": "blackhole"}}
	users := make([]interface{}, 0, len(eps))
	rules := make([]interface{}, 0, len(eps))
	for i, ep := range eps {
		var ob map[string]interface{}
		if err := json.Unmarshal(ep.Outbound, &ob); err != nil {
			return nil, fmt.Errorf("outbound of %s: %w", ep.Key, err)
		}
		tag := fmt.Sprintf("m%d", i)
		ob["tag"] = tag
		outbounds = append(outbounds, ob)
		users = append(users, map[string]interface{}{"user": accounts[i].User, "pass": accounts[i].Pass})
		rules = append(rules, map[string]interface{}{"type": "field", "user": []string{accounts[i].User}, "outboundTag": tag})
	}
	return json.MarshalIndent(map[string]interface{}{
		"log": map[string]interface{}{"loglevel": "warning", "access": "none"},
		"inbounds": []interface{}{map[string]interface{}{
			"tag": "probe-in", "listen": "127.0.0.1", "port": port, "protocol": "socks",
			"settings": map[string]interface{}{"auth": "password", "udp": false, "accounts": users},
		}},
		"outbounds": outbounds,
		"routing":   map[string]interface{}{"domainStrategy": "AsIs", "rules": rules},
	}, "", "  ")
}
```

Create `server/internal/monitor/check.go`:

```go
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

// ProbeURL is what a check fetches through an endpoint. Plain HTTP: the watch
// probes https://www.gstatic.com/generate_204, but a TLS handshake with that
// host costs 6.8 KB of the ~12 KB a check would take, and the two disagree
// only on a server that blocks outbound port 80.
const ProbeURL = "http://www.gstatic.com/generate_204"

// checkTimeout bounds one attempt. A var so a test can shorten it.
var checkTimeout = 10 * time.Second

// statusError is an answer other than 204.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// probeGet fetches url through the SOCKS5 proxy at socksAddr with the account
// user:pass and answers the time to the response headers: the full cost of a
// new connection, the server's handshake and the request both. A redirect is
// not followed; anything but 204 is a failure.
func probeGet(ctx context.Context, socksAddr, user, pass, url string) (time.Duration, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, &proxy.Auth{User: user, Password: pass}, &net.Dialer{Timeout: checkTimeout})
	if err != nil {
		return 0, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return 0, errors.New("socks dialer lacks DialContext")
	}
	tr := &http.Transport{DialContext: cd.DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: checkTimeout}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       checkTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	latency := time.Since(start)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 0, &statusError{code: resp.StatusCode}
	}
	return latency, nil
}

// classify names a failed check for the page: "HTTP <code>", "timeout" or
// "connection closed" - Xray drops the SOCKS connection when its outbound
// fails. Nothing of the error's own text: it can quote the proxy's address.
func classify(err error) string {
	var se *statusError
	var ne net.Error
	switch {
	case errors.As(err, &se):
		return se.Error()
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "connection closed"
	}
}
```

- [ ] **Step 4: Generate the golden file and review it**

Run: `cd server && UPDATE_GOLDEN=1 go test ./internal/monitor -run TestProbeConfig_Golden -count=1 && cat internal/monitor/testdata/probe_config.golden.json`
Expected: `ok`, and exactly this file:

```json
{
  "inbounds": [
    {
      "listen": "127.0.0.1",
      "port": 20000,
      "protocol": "socks",
      "settings": {
        "accounts": [
          {
            "pass": "p0",
            "user": "e0"
          },
          {
            "pass": "p1",
            "user": "e1"
          }
        ],
        "auth": "password",
        "udp": false
      },
      "tag": "probe-in"
    }
  ],
  "log": {
    "access": "none",
    "loglevel": "warning"
  },
  "outbounds": [
    {
      "protocol": "blackhole",
      "tag": "block"
    },
    {
      "protocol": "trojan",
      "settings": {
        "servers": [
          {
            "address": "192.0.2.1",
            "password": "p",
            "port": 443
          }
        ]
      },
      "tag": "m0"
    },
    {
      "protocol": "vless",
      "settings": {
        "vnext": [
          {
            "address": "192.0.2.2",
            "port": 443,
            "users": [
              {
                "encryption": "none",
                "id": "u"
              }
            ]
          }
        ]
      },
      "tag": "m1"
    }
  ],
  "routing": {
    "domainStrategy": "AsIs",
    "rules": [
      {
        "outboundTag": "m0",
        "type": "field",
        "user": [
          "e0"
        ]
      },
      {
        "outboundTag": "m1",
        "type": "field",
        "user": [
          "e1"
        ]
      }
    ]
  }
}
```

- [ ] **Step 5: Run the tests**

Run: `cd server && gofmt -l internal/monitor && go vet ./internal/monitor/ && go test ./internal/monitor/ -count=1`
Expected: `gofmt -l` prints nothing; `ok`.

- [ ] **Step 6: Commit**

```bash
git add server/internal/monitor/probeconfig.go server/internal/monitor/check.go \
  server/internal/monitor/probeconfig_test.go server/internal/monitor/check_test.go \
  server/internal/monitor/testdata/probe_config.golden.json
git commit -m "feat(monitor): the prober's config and one check

The prober's Xray config starts with a blackhole, so nothing matching no rule leaves the router directly, and holds one outbound per endpoint behind one SOCKS inbound on 127.0.0.1 with an account per endpoint and a random password per start. A check fetches http://www.gstatic.com/generate_204 through the endpoint's account: 204 within 10 s is alive, anything else is HTTP <code>, a timeout or a closed connection."
```

### Task 6: The prober process

Running Xray as the prober, safely. It runs as `vpn-director-probe`, a hard link to the `xray` on PATH: under the name `xray` it would make Entware's `S24xray start` answer "already running" while the live Xray is dead (`rc.func` asks `pidof xray`), and a symbolic link does not hide it because BusyBox `pidof` also compares the resolved `/proc/PID/exe`. The process dies with the daemon (`Pdeathsig`, which follows the thread that forked it, so that goroutine locks its thread). A config Xray refuses names the endpoint whose outbound it could not build; the reason kept drops the last segment of Xray's error chain, which quotes the offending value. `FakeLauncher` stands in for Xray in dev mode.

**Files:**
- Create: `server/internal/monitor/process.go`, `process_linux.go`, `process_other.go`
- Create: `server/internal/monitor/xray.go`, `server/internal/monitor/fake.go`
- Test: `server/internal/monitor/xray_test.go`

**Interfaces:**
- Consumes: `probeConfig`, `newAccounts`, `probeGet`, `ProbeURL`, and the test helpers `twoEndpoints`, `answering` (Task 5).
- Produces:
  - `type Launcher interface { Ready() error; Start(ctx, eps []Endpoint) (Session, error); Test(ctx, eps []Endpoint) error }`.
  - `type Session interface { Check(ctx, key string) (time.Duration, error); Exited() <-chan struct{}; Stop() }` - `Exited` closes when the prober ends, on its own or by `Stop`.
  - `var ErrNoXray error`; `type RefusedError struct { Key, Reason string }`.
  - `type XrayLauncher struct { ProbeBinary, ConfigDir, ProbeURL string }` implementing `Launcher`.
  - `func EnsureProbeBinary(xray, link string) error`, `func KillLeftovers(bin string)`.
  - `type FakeLauncher struct{}` implementing `Launcher`.
  - Vars a test may shorten: `startTimeout` (10 s), `testTimeout` (30 s).

- [ ] **Step 1: Write the failing tests**

Create `server/internal/monitor/xray_test.go`. `TestXrayLauncher_RealXray` runs only where an `xray` is on PATH; in CI and on a workstation without one it is skipped.

```go
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Xray's own words for an outbound it cannot build, the key it quotes last.
const refusalLine = `Failed to start: main: failed to load config files: [config.json] > infra/conf: failed to build outbound config with tag m1 > infra/conf: failed to build stream settings for outbound detour > infra/conf: Failed to build REALITY config. > infra/conf: invalid "password": not-a-key`

func TestRefusedTag_NamesTheEndpointAndDropsTheQuotedValue(t *testing.T) {
	i, reason, ok := refusedTag("some line\n" + refusalLine + "\n")
	if !ok || i != 1 {
		t.Fatalf("refusedTag() = %d, %q, %v", i, reason, ok)
	}
	if reason != "failed to build stream settings for outbound detour > Failed to build REALITY config." {
		t.Fatalf("reason %q", reason)
	}
	if _, _, ok := refusedTag("Failed to start: main: failed to create server"); ok {
		t.Fatal("a failure that names no tag was pinned on an endpoint")
	}
	if got := refusalReason("infra/conf: failed to build outbound config with tag m0 > infra/conf: invalid \"password\": secret"); got != "Xray refused the outbound" || strings.Contains(got, "secret") {
		t.Fatalf("reason %q", got)
	}
	if got := lastLine("a\n" + refusalLine + "\n"); strings.Contains(got, "not-a-key") {
		t.Fatalf("lastLine kept the value: %q", got)
	}
}

// script writes an executable shell script standing in for xray.
func script(t *testing.T, dir, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "vpn-director-probe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestXrayLauncher_ARefusedOutboundNamesItsEndpoint(t *testing.T) {
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo '"+refusalLine+"' >&2\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}

	_, err := l.Start(context.Background(), twoEndpoints())

	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Key != "k1" || strings.Contains(refused.Reason, "not-a-key") {
		t.Fatalf("Start() error %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "probe", "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config %v, %v: it holds every server's credentials", info, err)
	}
}

func TestXrayLauncher_AProberThatNeverListensIsStopped(t *testing.T) {
	old := startTimeout
	startTimeout = 300 * time.Millisecond
	defer func() { startTimeout = old }()
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo 'starting' >&2\nexec sleep 30"), ConfigDir: filepath.Join(dir, "probe")}

	start := time.Now()
	_, err := l.Start(context.Background(), twoEndpoints())
	if err == nil || !strings.Contains(err.Error(), "did not listen") {
		t.Fatalf("Start() error %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Start() took %s; the sleeping prober was not stopped", d)
	}
}

func TestXrayLauncher_TestReportsTheRefusalWithoutTheValue(t *testing.T) {
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo '"+refusalLine+"'\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}

	err := l.Test(context.Background(), twoEndpoints())
	if err == nil || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("Test() error %v", err)
	}
}

// The probe binary is a hard link to xray, and follows it when Entware
// replaces the file.
func TestEnsureProbeBinary_LinksAndFollowsAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	xray := filepath.Join(dir, "xray")
	link := filepath.Join(dir, "vpn-director-probe")
	if err := os.WriteFile(xray, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProbeBinary(xray, link); err != nil {
		t.Fatal(err)
	}
	same := func() bool {
		xi, _ := os.Stat(xray)
		li, err := os.Stat(link)
		return err == nil && os.SameFile(xi, li)
	}
	if !same() {
		t.Fatal("the probe binary is not a hard link to xray")
	}

	// opkg writes a new file: a new inode behind the name.
	if err := os.Remove(xray); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xray, []byte("v2, longer"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProbeBinary(xray, link); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(link); !same() || string(data) != "v2, longer" {
		t.Fatalf("the probe binary still runs %q", data)
	}
}

// With a real xray on PATH, the prober reaches each endpoint's own outbound:
// freedom answers, blackhole does not. KillLeftovers ends a prober left behind.
func TestXrayLauncher_RealXray(t *testing.T) {
	xray, err := exec.LookPath("xray")
	if err != nil {
		t.Skip("xray not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "vpn-director-probe")
	if err := EnsureProbeBinary(xray, bin); err != nil {
		t.Fatal(err)
	}
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	l := &XrayLauncher{ProbeBinary: bin, ConfigDir: filepath.Join(dir, "probe"), ProbeURL: url}
	eps := []Endpoint{
		{Key: "free", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
		{Key: "hole", Outbound: json.RawMessage(`{"protocol":"blackhole"}`)},
	}

	sess, err := l.Start(context.Background(), eps)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	if _, err := sess.Check(context.Background(), "free"); err != nil {
		t.Fatalf("freedom: %v", err)
	}
	if _, err := sess.Check(context.Background(), "hole"); err == nil {
		t.Fatal("blackhole answered")
	}

	KillLeftovers(bin)
	select {
	case <-sess.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("KillLeftovers left the prober running")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/monitor/ -count=1`
Expected: FAIL - `undefined: refusedTag`, `undefined: XrayLauncher`, `undefined: EnsureProbeBinary`, and more.

- [ ] **Step 3: Write the process**

Create `server/internal/monitor/process.go`:

```go
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errExited is a prober that exited before it listened.
var errExited = errors.New("the prober exited")

// tail keeps the last lines written to it: the prober's stderr, whose last
// lines say why it stopped.
type tail struct {
	mu    sync.Mutex
	lines []string
	part  []byte
	max   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part = append(t.part, p...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, string(t.part[:i]))
		t.part = t.part[i+1:]
		if len(t.lines) > t.max {
			t.lines = t.lines[len(t.lines)-t.max:]
		}
	}
	return len(p), nil
}

// String is the kept lines and any unfinished one.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := append([]string(nil), t.lines...)
	if len(t.part) > 0 {
		lines = append(lines, string(t.part))
	}
	return strings.Join(lines, "\n")
}

// process is a running prober.
type process struct {
	cmd    *exec.Cmd
	out    *tail
	exited chan struct{}
}

// startProcess runs bin with args. The child gets SIGKILL when its parent
// thread dies (Pdeathsig), and Pdeathsig follows the thread that forked the
// child, not the process: the goroutine that starts the child locks its
// thread and holds it until the child is gone.
func startProcess(bin string, args ...string) (*process, error) {
	p := &process{out: &tail{max: 20}, exited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		cmd := exec.Command(bin, args...)
		cmd.Stdout = p.out
		cmd.Stderr = p.out
		cmd.SysProcAttr = childAttr()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		p.cmd = cmd
		started <- nil
		_ = cmd.Wait()
		close(p.exited)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return p, nil
}

// waitListening waits until 127.0.0.1:port accepts, the process exits
// (errExited), ctx ends or timeout passes.
func (p *process) waitListening(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		if c, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-p.exited:
			return errExited
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the prober did not listen within %s", timeout)
		}
	}
}

// stop asks the process to end, and kills it 2 s later.
func (p *process) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}
```

Create `server/internal/monitor/process_linux.go`:

```go
package monitor

import "syscall"

// childAttr kills the prober with the daemon: a prober left behind would hold
// its memory and a port until the next reboot.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
```

Create `server/internal/monitor/process_other.go`:

```go
//go:build !linux

package monitor

import "syscall"

// childAttr has no Pdeathsig outside Linux; the daemon runs on Linux routers
// only, and a workstation build runs the fake prober.
func childAttr() *syscall.SysProcAttr {
	return nil
}
```

- [ ] **Step 4: Write the launchers**

Create `server/internal/monitor/xray.go`:

```go
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Launcher starts probers. XrayLauncher runs Xray; FakeLauncher stands in for
// it in dev mode, and the tests fake it.
type Launcher interface {
	// Ready is nil when a prober can start; ErrNoXray when there is no xray.
	Ready() error
	// Start runs a prober holding eps. A config Xray refuses comes back as a
	// *RefusedError when Xray names the endpoint.
	Start(ctx context.Context, eps []Endpoint) (Session, error)
	// Test has Xray load a config holding eps without starting it.
	Test(ctx context.Context, eps []Endpoint) error
}

// Session is a running prober.
type Session interface {
	// Check fetches ProbeURL through the endpoint of key once and answers the
	// time to the response headers.
	Check(ctx context.Context, key string) (time.Duration, error)
	// Exited is closed when the prober ends, on its own or by Stop.
	Exited() <-chan struct{}
	Stop()
}

// ErrNoXray is a router without xray on PATH.
var ErrNoXray = errors.New("xray not found on PATH")

// RefusedError is a prober that did not start because Xray refused the
// outbound of Key.
type RefusedError struct {
	Key    string
	Reason string
}

func (e *RefusedError) Error() string {
	return "xray refused the outbound of " + e.Key + ": " + e.Reason
}

// startTimeout bounds the wait for a new prober to listen; testTimeout one
// "xray run -test". Vars so a test can shorten them.
var (
	startTimeout = 10 * time.Second
	testTimeout  = 30 * time.Second
)

// XrayLauncher runs the prober as ProbeBinary, a hard link to the xray on PATH
// under a name of its own (EnsureProbeBinary), with its config in ConfigDir.
type XrayLauncher struct {
	ProbeBinary string
	ConfigDir   string
	// ProbeURL is what a check fetches; empty means ProbeURL.
	ProbeURL string
}

// Ready links ProbeBinary to the xray on PATH, again after Entware upgraded it.
func (l *XrayLauncher) Ready() error {
	xray, err := exec.LookPath("xray")
	if err != nil {
		return ErrNoXray
	}
	return EnsureProbeBinary(xray, l.ProbeBinary)
}

// Start writes the config for eps and runs the prober on a port free at
// start.
func (l *XrayLauncher) Start(ctx context.Context, eps []Endpoint) (Session, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	accounts, err := newAccounts(len(eps))
	if err != nil {
		return nil, err
	}
	path, err := l.writeConfig("config.json", eps, accounts, port)
	if err != nil {
		return nil, err
	}
	proc, err := startProcess(l.ProbeBinary, "run", "-format", "json", "-c", path)
	if err != nil {
		return nil, err
	}
	if err := proc.waitListening(ctx, port, startTimeout); err != nil {
		proc.stop()
		out := proc.out.String()
		if i, reason, ok := refusedTag(out); ok && i < len(eps) {
			return nil, &RefusedError{Key: eps[i].Key, Reason: reason}
		}
		return nil, fmt.Errorf("%w: %s", err, lastLine(out))
	}
	s := &xraySession{proc: proc, addr: fmt.Sprintf("127.0.0.1:%d", port), url: l.ProbeURL, accounts: make(map[string]account, len(eps))}
	if s.url == "" {
		s.url = ProbeURL
	}
	for i, ep := range eps {
		s.accounts[ep.Key] = accounts[i]
	}
	return s, nil
}

// Test has Xray load a config holding eps.
func (l *XrayLauncher) Test(ctx context.Context, eps []Endpoint) error {
	accounts, err := newAccounts(len(eps))
	if err != nil {
		return err
	}
	path, err := l.writeConfig("test.json", eps, accounts, 1)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, l.ProbeBinary, "run", "-test", "-format", "json", "-c", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray refused the config: %s", lastLine(string(out)))
	}
	return nil
}

// writeConfig writes the prober's config as name in ConfigDir, mode 0600: it
// holds every server's credentials.
func (l *XrayLauncher) writeConfig(name string, eps []Endpoint, accounts []account, port int) (string, error) {
	cfg, err := probeConfig(eps, accounts, port)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.ConfigDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(l.ConfigDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, cfg, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// xraySession is a running Xray prober.
type xraySession struct {
	proc     *process
	addr     string
	url      string
	accounts map[string]account
}

func (s *xraySession) Check(ctx context.Context, key string) (time.Duration, error) {
	a, ok := s.accounts[key]
	if !ok {
		return 0, fmt.Errorf("the prober holds no endpoint %s", key)
	}
	return probeGet(ctx, s.addr, a.User, a.Pass, s.url)
}

func (s *xraySession) Exited() <-chan struct{} { return s.proc.exited }

func (s *xraySession) Stop() { s.proc.stop() }

// freePort is a TCP port on 127.0.0.1 that nothing listens on now.
func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// refusedTagRE finds the endpoint Xray could not build: it stops at the first
// such outbound and names only its tag.
var refusedTagRE = regexp.MustCompile(`failed to build outbound config with tag m(\d+)`)

// refusedTag reads the index of the endpoint whose outbound Xray refused, and
// why, from the prober's output.
func refusedTag(out string) (int, string, bool) {
	for _, line := range strings.Split(out, "\n") {
		m := refusedTagRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return i, refusalReason(line), true
	}
	return 0, "", false
}

// refusalReason keeps what Xray says went wrong without the value it quotes:
// the segments of its error chain after "with tag m<i>", less the last one,
// which quotes the offending value - a key, as like as not
// (`invalid "password": not-a-key`) - each without its "infra/conf: ".
func refusalReason(line string) string {
	segs := strings.Split(line, " > ")
	start := 0
	for i, s := range segs {
		if strings.Contains(s, "failed to build outbound config with tag") {
			start = i + 1
		}
	}
	if len(segs)-1 <= start {
		return "Xray refused the outbound"
	}
	keep := make([]string, 0, len(segs)-1-start)
	for _, s := range segs[start : len(segs)-1] {
		keep = append(keep, strings.TrimPrefix(strings.TrimSpace(s), "infra/conf: "))
	}
	return strings.Join(keep, " > ")
}

// lastLine is the last non-empty line of out, without the last segment of an
// error chain, which can quote a value from the config.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if i := strings.LastIndex(line, " > "); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return "no output"
	}
	return line
}

// EnsureProbeBinary makes link the file xray names, under a name no "pidof
// xray", "killall xray" or monit rule matching "xray" can take for the live
// Xray: rc.func's start answers "already running" when pidof finds any xray,
// and a symbolic link does not hide it, because BusyBox pidof also compares
// the resolved /proc/PID/exe. It links again when xray changed (an Entware
// upgrade) and copies where a hard link cannot cross filesystems, keeping
// xray's modification time so the next call sees the copy as current.
func EnsureProbeBinary(xray, link string) error {
	xi, err := os.Stat(xray)
	if err != nil {
		return err
	}
	if li, err := os.Stat(link); err == nil && (os.SameFile(xi, li) || li.Size() == xi.Size() && li.ModTime().Equal(xi.ModTime())) {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Link(xray, tmp); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(xray, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Chtimes(tmp, xi.ModTime(), xi.ModTime()); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// KillLeftovers kills every process running bin: a prober an earlier daemon
// left behind when it died too fast for Pdeathsig.
func KillLeftovers(bin string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		if exe == bin || exe == bin+" (deleted)" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
```

Create `server/internal/monitor/fake.go`:

```go
package monitor

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// FakeLauncher stands in for Xray in dev mode: every endpoint answers the same
// way every time, by the first byte of its key - most alive with a latency of
// 50-560 ms, some dead, a few refused.
type FakeLauncher struct{}

// Ready is always nil.
func (FakeLauncher) Ready() error { return nil }

// Start refuses the first endpoint whose key starts below 0x0d.
func (FakeLauncher) Start(_ context.Context, eps []Endpoint) (Session, error) {
	for _, ep := range eps {
		if fakeByte(ep.Key) < 0x0d {
			return nil, &RefusedError{Key: ep.Key, Reason: "fake: Xray refused the outbound"}
		}
	}
	return &fakeSession{exited: make(chan struct{})}, nil
}

// Test is always nil.
func (FakeLauncher) Test(context.Context, []Endpoint) error { return nil }

type fakeSession struct {
	exited chan struct{}
	once   sync.Once
}

// Check answers after the key's latency; a key below 0x33 fails.
func (s *fakeSession) Check(ctx context.Context, key string) (time.Duration, error) {
	b := fakeByte(key)
	latency := time.Duration(50+2*int(b)) * time.Millisecond
	select {
	case <-time.After(latency):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if b < 0x33 {
		return 0, errors.New("fake: connection closed")
	}
	return latency, nil
}

func (s *fakeSession) Exited() <-chan struct{} { return s.exited }

func (s *fakeSession) Stop() { s.once.Do(func() { close(s.exited) }) }

func fakeByte(key string) byte {
	if len(key) < 2 {
		return 0xff
	}
	b, err := hex.DecodeString(key[:2])
	if err != nil {
		return 0xff
	}
	return b[0]
}
```

- [ ] **Step 5: Run the tests**

Run: `cd server && gofmt -l internal/monitor && go vet ./internal/monitor/ && go test ./internal/monitor/ -count=1 -v -run 'TestRefusedTag|TestXrayLauncher|TestEnsureProbeBinary'`
Expected: `gofmt -l` prints nothing; every test PASS, `TestXrayLauncher_RealXray` SKIP ("xray not on PATH") unless an xray is installed.

If you have an xray binary (the `Xray-linux-64.zip` of any recent release of XTLS/Xray-core), run the real test once with it on PATH: `PATH=/path/to/dir-with-xray:$PATH go test ./internal/monitor -run TestXrayLauncher_RealXray -count=1 -v`. Expected: PASS - freedom answers, blackhole does not, `KillLeftovers` ends the prober.

- [ ] **Step 6: Commit**

```bash
git add server/internal/monitor/process.go server/internal/monitor/process_linux.go server/internal/monitor/process_other.go \
  server/internal/monitor/xray.go server/internal/monitor/fake.go server/internal/monitor/xray_test.go
git commit -m "feat(monitor): the prober process

The prober runs as vpn-director-probe, a hard link to the xray on PATH, so neither rc.func's pidof xray nor the monit rule matching xray takes it for the live Xray; it dies with the daemon through Pdeathsig from a locked thread. A config Xray refuses names the endpoint it could not build, and the kept reason drops the last segment of Xray's error chain, which quotes the offending value. FakeLauncher answers by key in dev mode."
```

### Task 7: The engine

The monitor itself: one record per endpoint (`entry`), the loop that refreshes the set every minute, hands due checks to workers and records their answers, and the state file. Only `Run`'s goroutine changes the session and the entries' set; workers never touch the monitor's state and send their answers back on a channel, so an answer of a prober that was replaced or crashed meanwhile is dropped by comparing sessions. The engine also guards against a dead WAN, rejects outbounds Xray refuses (bisecting with `xray run -test` when Xray names none), pins a crash on its endpoint, and backs off a prober that cannot start.

The tests drive `refresh`, `tick`, `apply` and `crashed` by hand on a fake clock (`harness_test.go`); one test runs `Run` on the real clock.

**Files:**
- Create: `server/internal/monitor/entry.go`, `server/internal/monitor/monitor.go`, `server/internal/monitor/store.go`
- Test: `server/internal/monitor/entry_test.go`, `harness_test.go`, `monitor_test.go`, `robust_test.go`

**Interfaces:**
- Consumes: `Settings`, `SettingsFrom` (Task 3); `Endpoint` (Task 4); `classify` (Task 5); `Launcher`, `Session`, `RefusedError`, `ErrNoXray` (Task 6); `watchdapi.Snapshot`, `EndpointState`, `State*`, `Status*`, `ErrNotActive` (Task 2).
- Produces:
  - `type Deps struct { Settings func() (Settings, error); Endpoints func() ([]Endpoint, map[string]string, error); Launcher Launcher; Stopped func() bool; WANUp func(ctx context.Context) bool; StatePath string; OnSettings func(Settings); Now func() time.Time; Jitter func() float64 }`.
  - `func New(d Deps) *Monitor`, `func (m *Monitor) Run(ctx context.Context)`.
  - `func (m *Monitor) Snapshot() watchdapi.Snapshot` and `func (m *Monitor) Request(keys []string) (int, error)` - `*Monitor` implements `watchdapi.Source`.
  - `func (m *Monitor) Check(ctx context.Context, keys []string) (map[string]watchdapi.EndpointState, error)` - waits for answers from after the call (stage 3 uses it).
  - Constants `RefreshEvery`, `SaveEvery` (1m), `ControlEvery` (15s), `GuardWindow` (30s), `GuardMin` (5), `MaxRefusals` (20), `CrashLimit` (5), `CrashWindow` (10m).

- [ ] **Step 1: Write the failing test of one endpoint's record**

Create `server/internal/monitor/entry_test.go`:

```go
package monitor

import (
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var t0 = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func minuteSettings() Settings {
	return Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
}

func TestEntry_ALiveEndpointComesBackAfterTheIntervalWithinTheJitter(t *testing.T) {
	for _, jitter := range []float64{-1, 0, 0.999} {
		e := &entry{}
		if !e.succeed(t0, 142*time.Millisecond, minuteSettings(), jitter) {
			t.Fatal("a first success changed nothing")
		}
		wait := e.st.NextAt.Sub(t0)
		if wait < 54*time.Second || wait > 66*time.Second {
			t.Fatalf("jitter %v: next check in %s, want 60s ± 10%%", jitter, wait)
		}
		if e.st.Status != watchdapi.StatusAlive || e.st.LatencyMS != 142 || e.st.Since != t0 || e.st.CheckedAt != t0 {
			t.Fatalf("state %+v", e.st)
		}
	}
}

// A dead endpoint waits twice the interval, then twice its last pause, up to
// the cap; one success brings it back to the interval.
func TestEntry_TheDeadPauseDoublesToItsCapAndASuccessResetsIt(t *testing.T) {
	e := &entry{}
	now := t0
	var pauses []time.Duration
	for i := 0; i < 7; i++ {
		e.fail(now, "timeout", minuteSettings())
		pauses = append(pauses, e.st.NextAt.Sub(now))
		now = e.st.NextAt
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if pauses[i] != want[i] {
			t.Fatalf("pauses %v, want %v", pauses, want)
		}
	}
	if e.st.Fails != 7 || e.st.Since != t0 || e.st.Error != "timeout" {
		t.Fatalf("state %+v", e.st)
	}

	e.succeed(now, time.Second, minuteSettings(), 0)
	e.fail(now.Add(time.Minute), "timeout", minuteSettings())
	if got := e.st.NextAt.Sub(now.Add(time.Minute)); got != 2*time.Minute {
		t.Fatalf("pause after a success %s, want 2m", got)
	}
}

// The latency of the last success stays on a dead endpoint: the page shows it
// no longer, the record keeps it.
func TestEntry_AFailureKeepsTheLastLatency(t *testing.T) {
	e := &entry{}
	e.succeed(t0, 120*time.Millisecond, minuteSettings(), 0)
	if changed := e.fail(t0.Add(time.Minute), "HTTP 403", minuteSettings()); !changed {
		t.Fatal("alive to dead reported no change")
	}
	if e.st.LatencyMS != 120 || e.st.Since != t0.Add(time.Minute) {
		t.Fatalf("state %+v", e.st)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd server && go test ./internal/monitor/ -run TestEntry -count=1`
Expected: FAIL - `undefined: entry`.

- [ ] **Step 3: Write the record**

Create `server/internal/monitor/entry.go`:

```go
package monitor

import (
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// jitterShare is how far a live endpoint's next check may move, as a share of
// the interval, so checks spread over the minute instead of arriving in waves.
const jitterShare = 0.1

// entry is the monitor's record of one endpoint.
type entry struct {
	ep Endpoint
	st watchdapi.EndpointState
	// pause is a dead endpoint's current wait before its next check; zero
	// while it is not dead.
	pause time.Duration
	// sticky marks a rejection that holds until the outbound changes - Xray
	// refused it, or it crashed Xray - as opposed to one the generator makes,
	// which every refresh decides again.
	sticky bool
	// inFlight is set while a worker checks the endpoint.
	inFlight bool
	// urgent puts the endpoint ahead of the rest (Request).
	urgent bool
}

// checkable reports an endpoint the prober holds.
func (e *entry) checkable() bool {
	return e.st.Status != watchdapi.StatusRejected
}

// succeed records a check that got its answer; jitter is in [-1, 1). It
// reports whether the status changed.
func (e *entry) succeed(now time.Time, latency time.Duration, s Settings, jitter float64) bool {
	changed := e.st.Status != watchdapi.StatusAlive
	if changed {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusAlive
	e.st.LatencyMS = latency.Milliseconds()
	e.st.CheckedAt = now
	e.st.Fails = 0
	e.st.Error = ""
	e.pause = 0
	e.st.NextAt = now.Add(time.Duration(float64(s.Interval) * (1 + jitterShare*jitter)))
	return changed
}

// fail records a check that failed after its retry: the endpoint is dead and
// waits 2 × Interval, then twice its last pause, up to DeadMax. It reports
// whether the status changed. The latency of the last success stays.
func (e *entry) fail(now time.Time, reason string, s Settings) bool {
	changed := e.st.Status != watchdapi.StatusDead
	if changed {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusDead
	e.st.CheckedAt = now
	e.st.Fails++
	e.st.Error = reason
	if e.pause == 0 {
		e.pause = 2 * s.Interval
	} else {
		e.pause *= 2
	}
	e.pause = min(e.pause, s.DeadMax)
	e.st.NextAt = now.Add(e.pause)
	return changed
}

// reject takes the endpoint out of the prober. A sticky rejection holds until
// the outbound changes, which makes a new key.
func (e *entry) reject(now time.Time, reason string, sticky bool) {
	if e.st.Status != watchdapi.StatusRejected {
		e.st.Since = now
	}
	e.st.Status = watchdapi.StatusRejected
	e.st.Error = reason
	e.st.NextAt = time.Time{}
	e.pause = 0
	e.sticky = sticky
	e.inFlight = false
	e.urgent = false
}
```

Run: `cd server && go test ./internal/monitor/ -run TestEntry -count=1`
Expected: `ok`.

- [ ] **Step 4: Write the failing tests of the engine**

Create `server/internal/monitor/harness_test.go` (the fake launcher and the step-by-step driver):

```go
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeLauncher is a prober for the engine's tests. Each Check of a key takes
// the next answer of script[key] - nil is alive - and is alive once the script
// runs out; a key in crashOn crashes its session; refuse and opaque make Start
// fail, naming the key or not; gate, when set, holds every Check until it is
// closed.
type fakeLauncher struct {
	mu      sync.Mutex
	ready   error
	refuse  map[string]string
	opaque  map[string]bool
	fail    error // every Start fails with it, and Test passes
	script  map[string][]error
	crashOn map[string]bool
	gate    chan struct{}
	latency time.Duration
	starts  [][]string
	checks  map[string]int
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{refuse: map[string]string{}, opaque: map[string]bool{}, script: map[string][]error{},
		crashOn: map[string]bool{}, checks: map[string]int{}, latency: 100 * time.Millisecond}
}

func (f *fakeLauncher) Ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeLauncher) Start(_ context.Context, eps []Endpoint) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, len(eps))
	for i, ep := range eps {
		keys[i] = ep.Key
	}
	f.starts = append(f.starts, keys)
	if f.fail != nil {
		return nil, f.fail
	}
	for _, ep := range eps {
		if reason, ok := f.refuse[ep.Key]; ok {
			return nil, &RefusedError{Key: ep.Key, Reason: reason}
		}
	}
	for _, ep := range eps {
		if f.opaque[ep.Key] {
			return nil, errors.New("the prober exited: Failed to start")
		}
	}
	return &fakeSession2{l: f, exited: make(chan struct{})}, nil
}

func (f *fakeLauncher) Test(_ context.Context, eps []Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ep := range eps {
		if f.opaque[ep.Key] {
			return errors.New("xray refused the config")
		}
	}
	return nil
}

func (f *fakeLauncher) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *fakeLauncher) checkCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks[key]
}

type fakeSession2 struct {
	l      *fakeLauncher
	exited chan struct{}
	once   sync.Once
}

func (s *fakeSession2) Check(ctx context.Context, key string) (time.Duration, error) {
	s.l.mu.Lock()
	gate := s.l.gate
	s.l.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.l.mu.Lock()
	s.l.checks[key]++
	crash := s.l.crashOn[key]
	var err error
	if script := s.l.script[key]; len(script) > 0 {
		err, s.l.script[key] = script[0], script[1:]
	}
	latency := s.l.latency
	s.l.mu.Unlock()
	if crash {
		s.Stop()
		return 0, errors.New("EOF")
	}
	if err != nil {
		return 0, err
	}
	return latency, nil
}

func (s *fakeSession2) Exited() <-chan struct{} { return s.exited }

func (s *fakeSession2) Stop() { s.once.Do(func() { close(s.exited) }) }

// harness drives a monitor step by step on a fake clock.
type harness struct {
	t        *testing.T
	ctx      context.Context
	m        *Monitor
	l        *fakeLauncher
	now      time.Time
	settings Settings
	eps      []Endpoint
	refused  map[string]string
	stopped  bool
	wan      bool
	wanDials int
}

// fast shortens the engine's real waits for a test.
func fast(t *testing.T) {
	t.Helper()
	oldRetry, oldGrace := retryAfter, crashGrace
	retryAfter, crashGrace = 0, 20*time.Millisecond
	t.Cleanup(func() { retryAfter, crashGrace = oldRetry, oldGrace })
}

func eps(keys ...string) []Endpoint {
	out := make([]Endpoint, len(keys))
	for i, k := range keys {
		out[i] = Endpoint{Key: k, Label: "Alpha / " + k, Outbound: json.RawMessage(`{"protocol":"freedom"}`)}
	}
	return out
}

func newHarness(t *testing.T, keys ...string) *harness {
	t.Helper()
	fast(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, l: newFakeLauncher(), now: t0, settings: minuteSettings(), eps: eps(keys...), wan: true}
	h.m = New(h.deps(""))
	return h
}

func (h *harness) deps(statePath string) Deps {
	return Deps{
		Settings:  func() (Settings, error) { return h.settings, nil },
		Endpoints: func() ([]Endpoint, map[string]string, error) { return h.eps, h.refused, nil },
		Launcher:  h.l,
		Stopped:   func() bool { return h.stopped },
		WANUp:     func(context.Context) bool { h.wanDials++; return h.wan },
		StatePath: statePath,
		Now:       func() time.Time { return h.now },
		Jitter:    func() float64 { return 0 },
	}
}

func (h *harness) inFlight() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	n := 0
	for _, e := range h.m.entries {
		if e.inFlight {
			n++
		}
	}
	return n
}

// at moves the clock to t0+d and runs the loop's steps there: a refresh when
// asked, then dispatches and answers until no check is in flight.
func (h *harness) at(d time.Duration, refresh bool) {
	h.t.Helper()
	h.now = t0.Add(d)
	if refresh {
		h.m.refresh(h.ctx, h.now)
	}
	for i := 0; i < 1000; i++ {
		h.m.tick(h.ctx, h.now)
		if s := h.m.session; s != nil && exited(s) {
			h.m.crashed(h.ctx)
			continue
		}
		if h.inFlight() == 0 {
			return
		}
		select {
		case r := <-h.m.results:
			h.m.apply(h.ctx, r)
		case <-time.After(5 * time.Second):
			h.t.Fatal("no answer came")
		}
	}
	h.t.Fatal("the loop did not settle")
}

func (h *harness) state(key string) watchdapi.EndpointState {
	h.t.Helper()
	st, ok := h.m.Snapshot().Endpoints[key]
	if !ok {
		h.t.Fatalf("no endpoint %s", key)
	}
	return st
}

var errTimeout = context.DeadlineExceeded
```

Create `server/internal/monitor/monitor_test.go`:

```go
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func TestMonitor_ChecksEveryEndpointAndEachOnItsOwnSchedule(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")

	h.at(0, true)
	for _, k := range []string{"k1", "k2", "k3"} {
		st := h.state(k)
		if st.Status != watchdapi.StatusAlive || st.LatencyMS != 100 || st.NextAt != t0.Add(time.Minute) {
			t.Fatalf("%s: %+v", k, st)
		}
	}
	h.at(30*time.Second, false)
	if n := h.l.checkCount("k1"); n != 1 {
		t.Fatalf("k1 checked %d times by 30 s, want once", n)
	}
	h.at(time.Minute, false)
	if n := h.l.checkCount("k1"); n != 2 {
		t.Fatalf("k1 checked %d times by 1 min, want twice", n)
	}
	if h.l.startCount() != 1 {
		t.Fatalf("prober started %d times, want once", h.l.startCount())
	}
}

// One lost packet does not flip a status: a failure is retried once, and only
// a second failure fails the check.
func TestMonitor_AFailureIsRetriedOnceBeforeItCounts(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.script["k1"] = []error{errTimeout}
	h.l.script["k2"] = []error{errTimeout, errTimeout}

	h.at(0, true)

	if st := h.state("k1"); st.Status != watchdapi.StatusAlive || h.l.checkCount("k1") != 2 {
		t.Fatalf("k1 %+v after %d attempts, want alive after two", st, h.l.checkCount("k1"))
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusDead || st.Error != "timeout" || st.Fails != 1 {
		t.Fatalf("k2 %+v, want dead of a timeout", st)
	}
}

func TestMonitor_ADeadEndpointBacksOff(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.script["k1"] = []error{errTimeout, errTimeout, errTimeout, errTimeout}

	h.at(0, true)
	if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.NextAt != t0.Add(2*time.Minute) {
		t.Fatalf("after the first failure %+v", st)
	}
	h.at(time.Minute, false)
	if n := h.l.checkCount("k1"); n != 2 {
		t.Fatalf("a dead endpoint was checked again within its pause: %d attempts", n)
	}
	h.at(2*time.Minute, false)
	if st := h.state("k1"); st.NextAt != t0.Add(6*time.Minute) || st.Fails != 2 {
		t.Fatalf("after the second failure %+v, want the next check 4 min later", st)
	}
}

// A dead endpoint's next check can be 30 minutes away; asked for, it is
// checked now.
func TestMonitor_RequestPutsAnEndpointAheadOfItsSchedule(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.at(0, true)

	n, err := h.m.Request([]string{"k1", "absent"})
	if err != nil || n != 1 {
		t.Fatalf("Request() = %d, %v", n, err)
	}
	h.at(10*time.Second, false)
	if st := h.state("k1"); st.Status != watchdapi.StatusAlive || st.CheckedAt != t0.Add(10*time.Second) {
		t.Fatalf("k1 %+v, want checked at 10 s", st)
	}
	if h.l.checkCount("k2") != 1 {
		t.Fatal("k2 was checked ahead of its schedule")
	}
}

func TestMonitor_RequestWhileStoppedIsErrNotActive(t *testing.T) {
	h := newHarness(t, "k1")
	h.stopped = true
	h.at(0, true)

	if _, err := h.m.Request(nil); !errors.Is(err, watchdapi.ErrNotActive) {
		t.Fatalf("Request() error %v, want ErrNotActive", err)
	}
}

// Review focus: a subscription refresh that replaces the endpoints while
// checks are on their way. The answers of the replaced prober count for
// nothing, and the new set is checked.
func TestMonitor_AnswersOfAReplacedProberAreDropped(t *testing.T) {
	h := newHarness(t, "k1", "k2")
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)
	if h.inFlight() != 2 {
		t.Fatalf("%d checks in flight, want 2", h.inFlight())
	}

	h.eps = eps("k2", "k3")
	h.m.refresh(h.ctx, t0)
	close(h.l.gate)
	for i := 0; i < 2; i++ {
		h.m.apply(h.ctx, <-h.m.results)
	}
	if _, ok := h.m.Snapshot().Endpoints["k1"]; ok {
		t.Fatal("k1 is still listed after it left the set")
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusUnknown {
		t.Fatalf("k2 %+v; the replaced prober's answer counted", st)
	}
	h.l.mu.Lock()
	h.l.gate = nil
	h.l.mu.Unlock()
	h.at(0, false)
	if st := h.state("k3"); st.Status != watchdapi.StatusAlive {
		t.Fatalf("k3 %+v, want checked by the new prober", st)
	}
	if h.l.startCount() != 2 {
		t.Fatalf("prober started %d times, want twice", h.l.startCount())
	}
}

// Run drives the same steps on a real clock; Check waits for an answer from
// after the call.
func TestRun_ChecksAndAnswersCheck(t *testing.T) {
	fast(t)
	l := newFakeLauncher()
	l.latency = time.Millisecond
	statePath := filepath.Join(t.TempDir(), "state.json")
	m := New(Deps{
		Settings:  func() (Settings, error) { return minuteSettings(), nil },
		Endpoints: func() ([]Endpoint, map[string]string, error) { return eps("k1", "k2"), nil, nil },
		Launcher:  l,
		Stopped:   func() bool { return false },
		WANUp:     func(context.Context) bool { return true },
		StatePath: statePath,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	got, err := m.Check(checkCtx, []string{"k1"})
	stop()
	cancel()
	<-done
	if err != nil || got["k1"].Status != watchdapi.StatusAlive {
		t.Fatalf("Check() = %+v, %v", got, err)
	}
	data, err := os.ReadFile(statePath)
	var saved savedState
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Entries["k1"].State.Status != watchdapi.StatusAlive {
		t.Fatalf("state saved at shutdown: %s, %v", data, err)
	}
}

// At a rebuild every new endpoint is due at once; the active server's, which
// Build puts first, is checked first.
func TestMonitor_TheActiveServerIsCheckedFirst(t *testing.T) {
	h := newHarness(t, "k3", "k1", "k2")
	h.settings.Concurrency = 1
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)
	h.m.tick(h.ctx, t0)

	h.m.mu.Lock()
	first := h.m.entries["k3"].inFlight
	h.m.mu.Unlock()
	close(h.l.gate)
	if !first {
		t.Fatal("the first endpoint of the set is not the first one checked")
	}
}
```

Create `server/internal/monitor/robust_test.go`:

```go
package monitor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Review focus: the WAN goes down while every server is fine. The checks that
// failed on it are undone, statuses stay as they were, and every server is
// checked again once a control answers.
func TestMonitor_AWANOutageFreezesTheStatuses(t *testing.T) {
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	h := newHarness(t, keys...)
	h.at(0, true)

	for _, k := range keys {
		h.l.script[k] = []error{errTimeout, errTimeout}
	}
	h.wan = false
	h.at(time.Minute, false)

	if s := h.m.Snapshot(); s.State != watchdapi.StateWANDown {
		t.Fatalf("state %s, want wan_down", s.State)
	}
	for _, k := range keys {
		if st := h.state(k); st.Status != watchdapi.StatusAlive {
			t.Fatalf("%s %+v; an outage turned a server dead", k, st)
		}
	}
	dials := h.wanDials
	h.at(time.Minute+15*time.Second, false)
	if h.wanDials != dials+1 {
		t.Fatalf("controls dialed %d times more in 15 s, want once", h.wanDials-dials)
	}

	h.wan = true
	for _, k := range keys {
		h.l.script[k] = nil
	}
	h.at(time.Minute+30*time.Second, false)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK {
		t.Fatalf("state %s after a control answered", s.State)
	}
	for _, k := range keys {
		if st := h.state(k); st.CheckedAt != t0.Add(time.Minute+30*time.Second) {
			t.Fatalf("%s %+v, want checked the moment the WAN came back", k, st)
		}
	}
}

// Every server dead with the WAN up is the truth, and the controls are not
// asked again for every failure that follows.
func TestMonitor_AllDeadWithTheWANUpAreDead(t *testing.T) {
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	h := newHarness(t, keys...)
	for _, k := range keys {
		h.l.script[k] = []error{errTimeout, errTimeout}
	}

	h.at(0, true)

	for _, k := range keys {
		if st := h.state(k); st.Status != watchdapi.StatusDead {
			t.Fatalf("%s %+v, want dead", k, st)
		}
	}
	if h.wanDials != 1 {
		t.Fatalf("controls dialed %d times, want once", h.wanDials)
	}
}

// Xray names the outbound it refuses: that endpoint is rejected, and the
// prober starts with the rest.
func TestMonitor_ARefusedOutboundIsRejectedAndTheRestAreChecked(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.l.refuse["k2"] = "failed to build stream settings for outbound detour > Failed to build REALITY config."

	h.at(0, true)

	st := h.state("k2")
	if st.Status != watchdapi.StatusRejected || !strings.Contains(st.Error, "REALITY") {
		t.Fatalf("k2 %+v", st)
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 2 || got[0] != "k1" || got[1] != "k3" {
		t.Fatalf("the prober holds %v", got)
	}
	if h.state("k1").Status != watchdapi.StatusAlive || h.state("k3").Status != watchdapi.StatusAlive {
		t.Fatal("the other endpoints were not checked")
	}
	// The rejection holds at the next refresh: the outbound is the same.
	h.at(time.Minute, true)
	if h.state("k2").Status != watchdapi.StatusRejected {
		t.Fatal("the rejection did not hold")
	}
}

// Xray's error names no endpoint: halves are tested down to the one it
// refuses.
func TestMonitor_AnUnnamedRefusalIsFoundByHalves(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3", "k4", "k5")
	h.l.opaque["k4"] = true

	h.at(0, true)

	if st := h.state("k4"); st.Status != watchdapi.StatusRejected || st.Error != "Xray refused the outbound" {
		t.Fatalf("k4 %+v", st)
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 4 {
		t.Fatalf("the prober holds %v", got)
	}
}

// A prober that cannot start is retried after 1, then 2 minutes, not on
// every refresh.
func TestMonitor_AProberThatCannotStartBacksOff(t *testing.T) {
	h := newHarness(t, "k1")
	h.l.fail = errors.New("the prober did not listen within 10s: address already in use")

	h.at(0, true)
	s := h.m.Snapshot()
	if s.State != watchdapi.StateProberError || !strings.Contains(s.Message, "address already in use") {
		t.Fatalf("snapshot %+v", s)
	}
	h.at(30*time.Second, true)
	if h.l.startCount() != 1 {
		t.Fatalf("%d starts within the first minute, want 1", h.l.startCount())
	}
	h.at(time.Minute, true)
	h.at(2*time.Minute, true)
	if h.l.startCount() != 2 {
		t.Fatalf("%d starts by 2 min, want 2: the second wait is 2 min", h.l.startCount())
	}
	h.l.fail = nil
	h.at(3*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK || h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatalf("snapshot %+v", s)
	}
}

// The endpoint that crashes Xray is found, checked alone, and left out; the
// others are checked again.
func TestMonitor_ACrashIsPinnedOnItsEndpoint(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.l.crashOn["k2"] = true

	h.at(0, true)

	if st := h.state("k2"); st.Status != watchdapi.StatusRejected || st.Error != "crashes Xray" {
		t.Fatalf("k2 %+v", st)
	}
	for _, k := range []string{"k1", "k3"} {
		if st := h.state(k); st.Status != watchdapi.StatusAlive {
			t.Fatalf("%s %+v", k, st)
		}
	}
	if got := h.l.starts[len(h.l.starts)-1]; len(got) != 2 {
		t.Fatalf("the prober holds %v after the crash", got)
	}
}

func TestMonitor_StoppedDisabledAndNoXrayPauseTheChecks(t *testing.T) {
	h := newHarness(t, "k1")
	h.at(0, true)

	h.stopped = true
	h.at(time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateStopped || h.m.session != nil {
		t.Fatalf("state %s, session %v", s.State, h.m.session)
	}
	if h.l.checkCount("k1") != 1 || h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatal("a stop checked or forgot the server")
	}

	h.stopped = false
	h.settings.Enabled = false
	h.at(2*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateDisabled {
		t.Fatalf("state %s, want disabled", s.State)
	}

	h.settings.Enabled = true
	h.l.ready = ErrNoXray
	h.at(3*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateNoXray {
		t.Fatalf("state %s, want no_xray", s.State)
	}

	h.l.ready = nil
	h.at(4*time.Minute, true)
	if s := h.m.Snapshot(); s.State != watchdapi.StateOK || h.l.checkCount("k1") != 2 {
		t.Fatalf("state %s, %d checks: the checks did not resume", s.State, h.l.checkCount("k1"))
	}
}

// The generator's refusal is decided again at every refresh.
func TestMonitor_AGeneratorRefusalIsRejectedUntilItGoes(t *testing.T) {
	h := newHarness(t, "k1")
	h.refused = map[string]string{"k9": "stored outbound: xhttp downloadSettings without an address"}

	h.at(0, true)
	if st := h.state("k9"); st.Status != watchdapi.StatusRejected || h.l.checkCount("k9") != 0 {
		t.Fatalf("k9 %+v", st)
	}
	h.refused = nil
	h.eps = eps("k1", "k9")
	h.at(time.Minute, true)
	if st := h.state("k9"); st.Status != watchdapi.StatusAlive {
		t.Fatalf("k9 %+v after the refusal went", st)
	}
}

// Review focus: a restart of the daemon - an update - keeps every status and
// every pause; a key that left the set while it was down is dropped.
func TestMonitor_TheStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchd-state.json")
	h := newHarness(t, "k1", "k2", "k3")
	h.m = New(h.deps(path))
	h.l.script["k1"] = []error{errTimeout, errTimeout}
	h.at(0, true)
	h.m.shutdown()

	h.eps = eps("k1", "k2")
	h.m = New(h.deps(path))
	h.m.restore()
	h.at(30*time.Second, true)

	if st := h.state("k1"); st.Status != watchdapi.StatusDead || st.NextAt != t0.Add(2*time.Minute) {
		t.Fatalf("k1 %+v, want dead until 2 min", st)
	}
	if st := h.state("k2"); st.Status != watchdapi.StatusAlive || st.NextAt != t0.Add(time.Minute) {
		t.Fatalf("k2 %+v, want alive until 1 min", st)
	}
	if _, ok := h.m.Snapshot().Endpoints["k3"]; ok {
		t.Fatal("k3 came back after it left the set")
	}
	if h.l.checkCount("k2") != 1 {
		t.Fatal("a restart checked a live server before its time")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file %v, %v", info, err)
	}
}

func TestMonitor_ABrokenStateFileIsNoState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchd-state.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "k1")
	h.m = New(h.deps(path))
	h.m.restore()
	h.at(0, true)
	if h.state("k1").Status != watchdapi.StatusAlive {
		t.Fatal("a broken state file stopped the monitor")
	}
}

// Checks that wait longer than an interval are reported.
func TestMonitor_ChecksFallingBehindAreReported(t *testing.T) {
	h := newHarness(t, "k1", "k2", "k3")
	h.settings.Concurrency = 1
	h.l.gate = make(chan struct{})
	h.m.refresh(h.ctx, t0)

	h.now = t0.Add(3 * time.Minute)
	h.m.tick(h.ctx, h.now)
	if lag := h.m.Snapshot().LagSeconds; lag != 180 {
		t.Fatalf("lag %d s, want 180", lag)
	}
	close(h.l.gate)
}

// One start rejects at most MaxRefusals endpoints: a set that keeps being
// refused is a prober error, retried later, not an endless loop of starts.
func TestMonitor_TwentyRefusalsInARowStopTheStart(t *testing.T) {
	var keys []string
	for i := 0; i < MaxRefusals+5; i++ {
		keys = append(keys, fmt.Sprintf("k%02d", i))
	}
	h := newHarness(t, keys...)
	for _, k := range keys {
		h.l.refuse[k] = "Xray refused the outbound"
	}

	h.at(0, true)

	s := h.m.Snapshot()
	if s.State != watchdapi.StateProberError || !strings.Contains(s.Message, "20 outbounds in a row") {
		t.Fatalf("snapshot state %s, message %q", s.State, s.Message)
	}
	rejected := 0
	for _, st := range s.Endpoints {
		if st.Status == watchdapi.StatusRejected {
			rejected++
		}
	}
	if rejected != MaxRefusals || h.l.startCount() != MaxRefusals {
		t.Fatalf("%d rejected after %d starts, want %d of each", rejected, h.l.startCount(), MaxRefusals)
	}
}
```

- [ ] **Step 5: Run them to see them fail**

Run: `cd server && go test ./internal/monitor/ -count=1`
Expected: FAIL - `undefined: New`, `undefined: Deps`, `undefined: savedState`, and more.

- [ ] **Step 6: Write the engine and its state file**

Create `server/internal/monitor/monitor.go`:

```go
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Cadences of the monitor's loop.
const (
	// RefreshEvery is how often the monitor rereads its settings and the
	// subscriptions and looks at the state of VPN Director.
	RefreshEvery = time.Minute
	// SaveEvery is how often a changed state is saved.
	SaveEvery = time.Minute
	// ControlEvery is how often the control addresses are dialed while the
	// WAN is down.
	ControlEvery = 15 * time.Second
	// GuardWindow and GuardMin: when at least GuardMin checks, or every
	// endpoint when there are fewer, completed within GuardWindow and all of
	// them failed, the monitor asks whether the WAN is up.
	GuardWindow = 30 * time.Second
	GuardMin    = 5
	// MaxRefusals bounds the endpoints one start of the prober may reject.
	MaxRefusals = 20
	// CrashLimit crashes within CrashWindow stop the restarts for a backoff.
	CrashLimit  = 5
	CrashWindow = 10 * time.Minute
)

// retryAfter is the wait before a failed attempt's retry, crashGrace the time
// a lone suspect's prober gets to crash after its check, and proberBackoff the
// waits after failed starts. Vars so a test can shorten them.
var (
	retryAfter    = 2 * time.Second
	crashGrace    = time.Second
	proberBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
)

// Deps is what a monitor runs on. Settings, Endpoints, Launcher, Stopped and
// WANUp are required.
type Deps struct {
	// Settings reads the monitor section, resolved.
	Settings func() (Settings, error)
	// Endpoints builds the endpoint set (Build) from the subscriptions.
	Endpoints func() ([]Endpoint, map[string]string, error)
	Launcher  Launcher
	// Stopped reports VPN Director stopped (/tmp/vpn-director/stopped).
	Stopped func() bool
	// WANUp reports whether a control address accepts.
	WANUp func(ctx context.Context) bool
	// StatePath is where the state survives a restart; empty keeps none.
	StatePath string
	// OnSettings sees every resolved Settings, e.g. for the log level.
	OnSettings func(Settings)
	// Now and Jitter default to time.Now and a uniform value in [-1, 1).
	Now    func() time.Time
	Jitter func() float64
}

// Monitor checks every endpoint of every subscription through a prober. Run
// drives it; Snapshot, Request and Check are safe to call from other
// goroutines. Only Run's goroutine changes the session and the entries' set.
type Monitor struct {
	d Deps

	mu          sync.Mutex
	settings    Settings
	state       watchdapi.State
	message     string
	entries     map[string]*entry
	order       []string       // the endpoints' keys in Build order
	pos         map[string]int // each key's place in order: the active server's first
	session     Session
	setKey      string                // the keys the session holds
	restored    map[string]savedEntry // state read at startup, until the first refresh takes it
	recent      []outcome             // completed checks within GuardWindow
	controlOK   time.Time             // when a control last answered
	nextControl time.Time             // the next control dial while the WAN is down
	proberFails int                   // failed starts in a row
	proberAt    time.Time             // no start before this
	crashes     []time.Time
	lag         time.Duration
	lagWarned   bool
	dirty       bool
	lastSave    time.Time
	updated     time.Time
	warned      map[string]string
	changed     chan struct{} // closed and replaced whenever a result lands

	results chan result
	wake    chan struct{}
}

// result is a worker's answer for one endpoint.
type result struct {
	sess    Session
	key     string
	latency time.Duration
	err     error
}

// outcome is a completed check the WAN guard looks back on; a failure keeps
// the entry as it was before it, so an outage can be undone.
type outcome struct {
	at     time.Time
	ok     bool
	key    string
	before entry
}

// New returns a monitor over d; Run starts it.
func New(d Deps) *Monitor {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Jitter == nil {
		d.Jitter = func() float64 { return rand.Float64()*2 - 1 }
	}
	s, _ := SettingsFrom(nil)
	return &Monitor{
		d:        d,
		settings: s,
		state:    watchdapi.StateOK,
		entries:  map[string]*entry{},
		warned:   map[string]string{},
		changed:  make(chan struct{}),
		results:  make(chan result, MaxConcurrency),
		wake:     make(chan struct{}, 1),
	}
}

// Run checks until ctx ends, then stops the prober and saves the state.
func (m *Monitor) Run(ctx context.Context) {
	m.restore()
	var nextRefresh time.Time
	for {
		now := m.d.Now()
		if !now.Before(nextRefresh) {
			m.refresh(ctx, now)
			nextRefresh = now.Add(RefreshEvery)
		}
		m.tick(ctx, now)
		var exited <-chan struct{}
		if m.session != nil {
			exited = m.session.Exited()
		}
		timer := time.NewTimer(m.untilNext(now, nextRefresh))
		select {
		case <-ctx.Done():
			timer.Stop()
			m.shutdown()
			return
		case r := <-m.results:
			m.apply(ctx, r)
		case <-m.wake:
		case <-exited:
			m.crashed(ctx)
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Snapshot is the monitor's state for GET /v1/monitor.
func (m *Monitor) Snapshot() watchdapi.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	eps := make(map[string]watchdapi.EndpointState, len(m.entries))
	for k, e := range m.entries {
		eps[k] = e.st
	}
	return watchdapi.Snapshot{
		State:           m.state,
		Message:         m.message,
		IntervalSeconds: int(m.settings.Interval / time.Second),
		LagSeconds:      int(m.lag / time.Second),
		UpdatedAt:       m.updated,
		Endpoints:       eps,
	}
}

// Request makes the endpoints of keys - every endpoint when keys is empty -
// due ahead of the rest and answers how many it queued; an endpoint already
// under check counts, since its answer is coming. Unknown keys are ignored.
func (m *Monitor) Request(keys []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == watchdapi.StateStopped || m.state == watchdapi.StateDisabled {
		return 0, watchdapi.ErrNotActive
	}
	n := 0
	mark := func(e *entry) {
		if !e.checkable() {
			return
		}
		if !e.inFlight {
			e.urgent = true
		}
		n++
	}
	if len(keys) == 0 {
		for _, e := range m.entries {
			mark(e)
		}
	} else {
		for _, k := range keys {
			if e := m.entries[k]; e != nil {
				mark(e)
			}
		}
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return n, nil
}

// Check makes the endpoints of keys due now and waits until each has an
// answer from after the call, or ctx ends; it returns their states as they
// stand then. A key not in the set yet is waited for too. The watch's
// failover (stage 3) calls it in-process, with a deadline.
func (m *Monitor) Check(ctx context.Context, keys []string) (map[string]watchdapi.EndpointState, error) {
	start := m.d.Now()
	if _, err := m.Request(keys); err != nil {
		return nil, err
	}
	for {
		m.mu.Lock()
		out := make(map[string]watchdapi.EndpointState, len(keys))
		done := true
		for _, k := range keys {
			e := m.entries[k]
			if e == nil {
				// Not in the set yet: a refresh has still to read it.
				done = false
				continue
			}
			out[k] = e.st
			if e.checkable() && e.st.CheckedAt.Before(start) {
				done = false
			}
		}
		ch := m.changed
		m.mu.Unlock()
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ch:
		}
	}
}

// refresh rereads the settings and the subscriptions, looks at the state of
// VPN Director and starts, keeps or stops the prober.
func (m *Monitor) refresh(ctx context.Context, now time.Time) {
	s, err := m.d.Settings()
	if err != nil {
		m.warnOnce("settings", "Monitor cannot read its settings; keeping the last ones", err)
		s = m.settingsNow()
	} else {
		m.warnOnce("settings", "", nil)
	}
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
	if m.d.OnSettings != nil {
		m.d.OnSettings(s)
	}
	switch {
	case !s.Enabled:
		m.idle(watchdapi.StateDisabled, "")
		return
	case m.d.Stopped():
		m.idle(watchdapi.StateStopped, "")
		return
	}
	if err := m.d.Launcher.Ready(); err != nil {
		if errors.Is(err, ErrNoXray) {
			m.idle(watchdapi.StateNoXray, err.Error())
		} else {
			m.idle(watchdapi.StateProberError, err.Error())
		}
		return
	}
	eps, refused, err := m.d.Endpoints()
	if err != nil {
		m.warnOnce("endpoints", "Monitor cannot read the subscriptions; keeping the last list", err)
		if m.state != watchdapi.StateWANDown {
			m.ensureSession(ctx, now)
		}
		return
	}
	m.warnOnce("endpoints", "", nil)
	m.merge(now, eps, refused)
	if m.state == watchdapi.StateWANDown {
		return
	}
	m.ensureSession(ctx, now)
}

func (m *Monitor) settingsNow() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// merge takes a new endpoint set: new endpoints are due at once - or as the
// saved state has them - gone ones leave, and the generator's refusals are
// decided again.
func (m *Monitor) merge(now time.Time, eps []Endpoint, refused map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*entry, len(eps)+len(refused))
	order := make([]string, 0, len(eps))
	for _, ep := range eps {
		e := m.entries[ep.Key]
		if e == nil {
			e = m.fromRestored(ep.Key, now)
		}
		e.ep = ep
		if e.st.Status == watchdapi.StatusRejected && !e.sticky {
			// The generator refused it before and does no longer.
			e.st = watchdapi.EndpointState{Status: watchdapi.StatusUnknown, NextAt: now, Since: now}
		}
		next[ep.Key] = e
		order = append(order, ep.Key)
	}
	for key, reason := range refused {
		e := m.entries[key]
		if e == nil {
			e = m.fromRestored(key, now)
		}
		e.ep = Endpoint{Key: key}
		e.reject(now, reason, false)
		next[key] = e
	}
	if len(next) != len(m.entries) || !sameKeys(next, m.entries) {
		m.dirty = true
		m.updated = now
	}
	m.entries = next
	m.order = order
	m.pos = make(map[string]int, len(order))
	for i, k := range order {
		m.pos[k] = i
	}
	m.restored = nil
}

func sameKeys(a, b map[string]*entry) bool {
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// fromRestored is the entry the saved state has for key, or a new one due now.
func (m *Monitor) fromRestored(key string, now time.Time) *entry {
	if r, ok := m.restored[key]; ok {
		return &entry{st: r.State, pause: r.Pause, sticky: r.Sticky}
	}
	return &entry{st: watchdapi.EndpointState{Status: watchdapi.StatusUnknown, NextAt: now, Since: now}}
}

// checkableSet is the endpoints the prober holds, in Build order.
func (m *Monitor) checkableSet() []Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	var set []Endpoint
	for _, k := range m.order {
		if e := m.entries[k]; e != nil && e.checkable() {
			set = append(set, e.ep)
		}
	}
	return set
}

func keysOf(eps []Endpoint) string {
	keys := make([]string, len(eps))
	for i, ep := range eps {
		keys[i] = ep.Key
	}
	return strings.Join(keys, ",")
}

// ensureSession runs a prober holding the checkable set, unless one holds it
// already. Xray refusing an outbound rejects that endpoint and starts again.
func (m *Monitor) ensureSession(ctx context.Context, now time.Time) {
	set := m.checkableSet()
	if m.session != nil && keysOf(set) == m.setKey {
		return
	}
	if len(set) == 0 {
		m.stopSession()
		m.setState(watchdapi.StateOK, "")
		return
	}
	if now.Before(m.proberAt) {
		return
	}
	m.stopSession()
	for range MaxRefusals {
		if ctx.Err() != nil {
			return
		}
		sess, err := m.d.Launcher.Start(ctx, set)
		if err == nil {
			m.mu.Lock()
			m.session, m.setKey = sess, keysOf(set)
			m.mu.Unlock()
			m.proberFails = 0
			m.setState(watchdapi.StateOK, "")
			return
		}
		if ctx.Err() != nil {
			return
		}
		var refused *RefusedError
		bad, reason := "", ""
		if errors.As(err, &refused) {
			bad, reason = refused.Key, refused.Reason
		} else if bad = m.bisect(ctx, set); bad != "" {
			reason = "Xray refused the outbound"
		}
		if bad == "" {
			m.startFailed(now, err)
			return
		}
		m.rejectKey(now, bad, reason)
		set = without(set, bad)
		if len(set) == 0 {
			m.setState(watchdapi.StateOK, "")
			return
		}
	}
	m.startFailed(now, fmt.Errorf("Xray refused %d outbounds in a row", MaxRefusals))
}

// bisect finds an endpoint whose outbound Xray refuses when its error names
// none: halves of set are tested with "xray run -test" down to one endpoint.
// It answers "" when set loads as a whole, or no single endpoint fails alone.
func (m *Monitor) bisect(ctx context.Context, set []Endpoint) string {
	if m.d.Launcher.Test(ctx, set) == nil {
		return ""
	}
	for len(set) > 1 {
		half := set[:len(set)/2]
		if m.d.Launcher.Test(ctx, half) != nil {
			set = half
		} else {
			set = set[len(set)/2:]
		}
	}
	if m.d.Launcher.Test(ctx, set) == nil {
		return ""
	}
	return set[0].Key
}

func without(set []Endpoint, key string) []Endpoint {
	out := make([]Endpoint, 0, len(set))
	for _, ep := range set {
		if ep.Key != key {
			out = append(out, ep)
		}
	}
	return out
}

func (m *Monitor) rejectKey(now time.Time, key, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return
	}
	e.reject(now, reason, true)
	m.dirty = true
	m.updated = now
	slog.Warn("Monitor: Xray refused a server", "server", e.ep.Label, "reason", reason)
}

// startFailed records a prober that did not start and backs off 1, 2, then 5
// minutes before the next try.
func (m *Monitor) startFailed(now time.Time, err error) {
	wait := proberBackoff[min(m.proberFails, len(proberBackoff)-1)]
	m.proberFails++
	m.proberAt = now.Add(wait)
	m.setState(watchdapi.StateProberError, err.Error())
	slog.Warn("Monitor: the prober did not start", "error", err, "retry_in", wait)
}

// stopSession stops the prober; the answers still on their way are dropped.
func (m *Monitor) stopSession() {
	if m.session == nil {
		return
	}
	sess := m.session
	m.mu.Lock()
	m.session, m.setKey = nil, ""
	for _, e := range m.entries {
		e.inFlight = false
	}
	m.mu.Unlock()
	sess.Stop()
}

// idle stops the checks for state: the statuses stay as they were.
func (m *Monitor) idle(state watchdapi.State, message string) {
	m.stopSession()
	m.mu.Lock()
	m.recent = nil
	m.mu.Unlock()
	m.setState(state, message)
}

func (m *Monitor) setState(state watchdapi.State, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == state && m.message == message {
		return
	}
	m.state, m.message = state, message
	m.updated = m.d.Now()
	slog.Info("Monitor state", "state", state, "message", message)
}

// tick dials the controls while the WAN is down, hands due checks to free
// workers and saves a changed state now and then.
func (m *Monitor) tick(ctx context.Context, now time.Time) {
	if m.state == watchdapi.StateWANDown && !now.Before(m.nextControl) {
		if m.d.WANUp(ctx) {
			m.mu.Lock()
			for _, e := range m.entries {
				if e.checkable() {
					e.st.NextAt = now
				}
			}
			m.recent = nil
			m.controlOK = now
			m.mu.Unlock()
			m.setState(watchdapi.StateOK, "")
			slog.Info("Monitor: the WAN is back; checking every server")
			m.ensureSession(ctx, now)
		} else {
			m.nextControl = now.Add(ControlEvery)
		}
	}
	if m.state == watchdapi.StateOK && m.session != nil {
		m.dispatch(ctx, now)
	}
	if m.dirty && now.Sub(m.lastSave) >= SaveEvery {
		m.save(now)
	}
}

// dispatch hands the due endpoints - urgent ones first, then the longest due,
// then in Build order, the active server's first - to free workers, and
// measures how long the next one waits.
func (m *Monitor) dispatch(ctx context.Context, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := 0
	var due []*entry
	for _, e := range m.entries {
		switch {
		case e.inFlight:
			busy++
		case e.checkable() && (e.urgent || !e.st.NextAt.After(now)):
			due = append(due, e)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].urgent != due[j].urgent {
			return due[i].urgent
		}
		if !due[i].st.NextAt.Equal(due[j].st.NextAt) {
			return due[i].st.NextAt.Before(due[j].st.NextAt)
		}
		return m.pos[due[i].ep.Key] < m.pos[due[j].ep.Key]
	})
	free := max(0, m.settings.Concurrency-busy)
	n := min(free, len(due))
	for _, e := range due[:n] {
		e.inFlight, e.urgent = true, false
		go m.check(ctx, m.session, e.ep.Key)
	}
	m.lag = 0
	if waiting := due[n:]; len(waiting) > 0 && !waiting[0].urgent {
		m.lag = now.Sub(waiting[0].st.NextAt)
	}
	switch {
	case m.lag > m.settings.Interval && !m.lagWarned:
		m.lagWarned = true
		slog.Warn("Monitor: checks are falling behind; raise monitor.concurrency or monitor.interval", "lag", m.lag)
	case m.lag <= m.settings.Interval:
		m.lagWarned = false
	}
}

// check is a worker: one attempt, and after a failure one retry retryAfter
// later - unless the prober went meanwhile, a crash that is not the server's.
func (m *Monitor) check(ctx context.Context, sess Session, key string) {
	latency, err := sess.Check(ctx, key)
	if err != nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-sess.Exited():
		case <-time.After(retryAfter):
			latency, err = sess.Check(ctx, key)
		}
	}
	select {
	case m.results <- result{sess: sess, key: key, latency: latency, err: err}:
	case <-ctx.Done():
	}
}

func exited(s Session) bool {
	select {
	case <-s.Exited():
		return true
	default:
		return false
	}
}

// apply records a worker's answer. An answer of a prober that is gone, or that
// crashed, or one that comes while the WAN is down counts for nothing.
func (m *Monitor) apply(ctx context.Context, r result) {
	m.mu.Lock()
	e := m.entries[r.key]
	if e == nil || r.sess != m.session || exited(r.sess) {
		// A replaced prober's flags went with it; a crashed one's in-flight
		// endpoints are the suspects crashed() looks at.
		m.mu.Unlock()
		return
	}
	e.inFlight = false
	if m.state != watchdapi.StateOK {
		m.mu.Unlock()
		return
	}
	now := m.d.Now()
	trip := false
	if r.err == nil {
		if e.succeed(now, r.latency, m.settings, m.d.Jitter()) {
			slog.Info("Monitor: server alive", "server", e.ep.Label, "latency", r.latency.Round(time.Millisecond))
		}
		m.recent = append(m.recent, outcome{at: now, ok: true})
		m.pruneRecent(now)
	} else {
		before := *e
		reason := classify(r.err)
		if e.fail(now, reason, m.settings) {
			slog.Info("Monitor: server down", "server", e.ep.Label, "error", reason)
		}
		m.recent = append(m.recent, outcome{at: now, key: r.key, before: before})
		trip = m.guardDue(now)
	}
	m.dirty = true
	m.updated = now
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	if trip {
		m.guard(ctx, now)
	}
}

// pruneRecent drops the checks that completed before GuardWindow. The caller
// holds mu.
func (m *Monitor) pruneRecent(now time.Time) {
	kept := m.recent[:0]
	for _, o := range m.recent {
		if now.Sub(o.at) < GuardWindow {
			kept = append(kept, o)
		}
	}
	m.recent = kept
}

// guardDue reports failures enough to ask whether the WAN is up: at least
// GuardMin checks - or every checkable endpoint, when there are fewer -
// completed within GuardWindow, all failed, and no control answered within
// that window. The caller holds mu.
func (m *Monitor) guardDue(now time.Time) bool {
	m.pruneRecent(now)
	checkable := 0
	for _, e := range m.entries {
		if e.checkable() {
			checkable++
		}
	}
	need := min(GuardMin, checkable)
	if need == 0 || len(m.recent) < need || now.Sub(m.controlOK) < GuardWindow {
		return false
	}
	for _, o := range m.recent {
		if o.ok {
			return false
		}
	}
	return true
}

// guard asks the controls. None answering, the failures that led here were the
// WAN's: they are undone, and checks pause until a control answers.
func (m *Monitor) guard(ctx context.Context, now time.Time) {
	if m.d.WANUp(ctx) {
		m.mu.Lock()
		m.controlOK = now
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	for i := len(m.recent) - 1; i >= 0; i-- {
		o := m.recent[i]
		if e := m.entries[o.key]; e != nil && !o.ok {
			e.st, e.pause = o.before.st, o.before.pause
			e.st.NextAt = now
		}
	}
	m.recent = nil
	m.nextControl = now.Add(ControlEvery)
	m.dirty = true
	m.mu.Unlock()
	m.setState(watchdapi.StateWANDown, "")
	slog.Warn("Monitor: the WAN is down; server statuses are kept until it is back")
}

// crashed handles a prober that exited on its own: each endpoint under check
// at that moment is checked alone in a prober of its own, and one that crashes
// that prober too is rejected. Then the prober starts again without the
// culprits, unless it keeps crashing.
func (m *Monitor) crashed(ctx context.Context) {
	now := m.d.Now()
	m.mu.Lock()
	sess := m.session
	var suspects []Endpoint
	for _, k := range m.order {
		if e := m.entries[k]; e != nil && e.inFlight {
			suspects = append(suspects, e.ep)
		}
	}
	for _, e := range m.entries {
		e.inFlight = false
	}
	m.session, m.setKey = nil, ""
	kept := m.crashes[:0]
	for _, t := range m.crashes {
		if now.Sub(t) < CrashWindow {
			kept = append(kept, t)
		}
	}
	m.crashes = append(kept, now)
	crashes := len(m.crashes)
	m.mu.Unlock()
	sess.Stop()
	slog.Warn("Monitor: the prober exited", "suspects", len(suspects))
	for _, ep := range suspects {
		if ctx.Err() != nil {
			return
		}
		if m.crashesAlone(ctx, ep) {
			m.rejectKey(now, ep.Key, "crashes Xray")
		}
	}
	if crashes >= CrashLimit {
		m.mu.Lock()
		m.crashes = nil
		m.mu.Unlock()
		m.startFailed(now, fmt.Errorf("the prober crashed %d times in %s", crashes, CrashWindow))
		return
	}
	m.ensureSession(ctx, now)
}

// crashesAlone checks ep in a prober holding it alone and reports whether that
// prober exited within crashGrace of the check.
func (m *Monitor) crashesAlone(ctx context.Context, ep Endpoint) bool {
	sess, err := m.d.Launcher.Start(ctx, []Endpoint{ep})
	if err != nil {
		return false
	}
	defer sess.Stop()
	_, _ = sess.Check(ctx, ep.Key)
	select {
	case <-sess.Exited():
		return true
	case <-time.After(crashGrace):
		return false
	case <-ctx.Done():
		return false
	}
}

// untilNext is how long the loop may sleep: until the next refresh, the next
// due check a free worker could take, the next control dial or the next save.
func (m *Monitor) untilNext(now, nextRefresh time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := nextRefresh
	if m.state == watchdapi.StateWANDown && m.nextControl.Before(next) {
		next = m.nextControl
	}
	if m.state == watchdapi.StateOK && m.session != nil {
		busy := 0
		for _, e := range m.entries {
			if e.inFlight {
				busy++
			}
		}
		if busy < m.settings.Concurrency {
			for _, e := range m.entries {
				if e.inFlight || !e.checkable() {
					continue
				}
				if e.urgent {
					return 0
				}
				if e.st.NextAt.Before(next) {
					next = e.st.NextAt
				}
			}
		}
	}
	if m.dirty && m.lastSave.Add(SaveEvery).Before(next) {
		next = m.lastSave.Add(SaveEvery)
	}
	return max(0, next.Sub(now))
}

// shutdown stops the prober and saves the state.
func (m *Monitor) shutdown() {
	m.stopSession()
	m.save(m.d.Now())
}

// warnOnce logs msg with err once per distinct error under key; an empty msg
// clears key, so the next failure is logged again.
func (m *Monitor) warnOnce(key, msg string, err error) {
	if msg == "" {
		delete(m.warned, key)
		return
	}
	text := err.Error()
	if m.warned[key] == text {
		return
	}
	m.warned[key] = text
	slog.Warn(msg, "error", err)
}
```

Create `server/internal/monitor/store.go`:

```go
package monitor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// savedEntry is an endpoint as the state file keeps it.
type savedEntry struct {
	State  watchdapi.EndpointState `json:"state"`
	Pause  time.Duration           `json:"pause"`
	Sticky bool                    `json:"sticky,omitempty"`
}

type savedState struct {
	SavedAt time.Time             `json:"saved_at"`
	Entries map[string]savedEntry `json:"entries"`
}

// restore reads the state an earlier run saved; the first refresh takes the
// entries of the keys still there. A missing or broken file is no state.
func (m *Monitor) restore() {
	if m.d.StatePath == "" {
		return
	}
	data, err := os.ReadFile(m.d.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var s savedState
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err != nil {
		slog.Warn("Monitor: cannot read the saved state; starting from nothing", "path", m.d.StatePath, "error", err)
		return
	}
	m.restored = s.Entries
}

// save writes the state through a temp file and a rename, mode 0600. A
// failure is logged, and the next save tries again in SaveEvery.
func (m *Monitor) save(now time.Time) {
	m.mu.Lock()
	s := savedState{SavedAt: now, Entries: make(map[string]savedEntry, len(m.entries))}
	for k, e := range m.entries {
		s.Entries[k] = savedEntry{State: e.st, Pause: e.pause, Sticky: e.sticky}
	}
	m.dirty = false
	m.lastSave = now
	m.mu.Unlock()
	if m.d.StatePath == "" {
		return
	}
	data, err := json.Marshal(s)
	if err == nil {
		err = writeAtomic(m.d.StatePath, data)
	}
	if err != nil {
		slog.Warn("Monitor: cannot save the state", "path", m.d.StatePath, "error", err)
	}
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
```

- [ ] **Step 7: Run the tests, with the race detector too**

Run: `cd server && gofmt -l internal/monitor && go vet ./internal/monitor/ && go test ./internal/monitor/ -count=1 && go test -race ./internal/monitor/ -count=3`
Expected: `gofmt -l` prints nothing; `ok` both times, no race report.

- [ ] **Step 8: Commit**

```bash
git add server/internal/monitor/entry.go server/internal/monitor/monitor.go server/internal/monitor/store.go \
  server/internal/monitor/entry_test.go server/internal/monitor/harness_test.go \
  server/internal/monitor/monitor_test.go server/internal/monitor/robust_test.go
git commit -m "feat(monitor): the engine

Every endpoint has its own schedule: alive once per interval within 10 % jitter, dead after twice the interval with the pause doubling to its cap, new at once, urgent ahead of the rest. A failure is retried once before it counts. Answers of a prober that was replaced or crashed count for nothing. Failures that all come at once while no control address answers are the WAN's: they are undone and checks pause until a control answers. An outbound Xray refuses is rejected, found by halves when Xray names none; a crash is pinned on the endpoint that crashes a prober of its own; a prober that cannot start backs off 1, 2, then 5 minutes. The state survives a restart."
```

### Task 8: The updater ships a third daemon and starts it the first time

The daemon table gains `vpn-director-watchd`, between the bot and the Web UI. Step 2 of a self-update then downloads its asset like any other daemon's; the contract in `selfupdate.go` allows adding one. The update script restarts only the daemons that ran before it, so a daemon a release adds would stay stopped after its first update: the script now notes, before it copies anything, every daemon whose binary does not exist yet, and counts those as running once the copy has succeeded. A failed update does not start them.

**Files:**
- Modify: `server/internal/updater/updater.go:77-89`
- Modify: `server/internal/updater/update_script.sh.tmpl` (steps 1 and 5)
- Test: `server/internal/updater/script_test.go`, `updater_test.go`, `github_test.go`, `selfupdate_test.go`, `downloader_test.go`
- Modify (regenerated): `server/internal/updater/testdata/update_script.golden.sh`

**Interfaces:**
- Produces: `updater.DaemonWatchd = "vpn-director-watchd"`, and the table entry `{Name: DaemonWatchd, Binary: "/opt/vpn-director/vpn-director-watchd", InitScript: "S98vpn-director-watchd"}` second of three.

- [ ] **Step 1: Write the failing test of the new rule**

Append to `server/internal/updater/script_test.go`:

```go
// A daemon a release adds is not running before its first update - nothing
// ran it - and restarting only the daemons that ran would leave it stopped.
// Step 1 notes every daemon whose binary is absent, and once the copy has
// succeeded those count as running: step 6 starts them before the bot. A
// daemon whose binary is there but stopped stays stopped.
func TestGenerateScript_StartsADaemonNewWithTheRelease(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}
	steps := afterOnExit(t, script)

	record := strings.Index(steps, `elif [ ! -e "$bin" ]; then`)
	copyFiles := strings.Index(steps, "# 4. Copy files")
	join := strings.Index(steps, `RUNNING_INITS="$RUNNING_INITS$NEW_INITS"`)
	start := strings.Index(steps, `if ! start_except "$NOTIFY_INIT"; then`)
	if record < 0 || copyFiles < 0 || join < 0 || start < 0 {
		t.Fatalf("missing a step: record %d, copy %d, join %d, start %d", record, copyFiles, join, start)
	}
	if !(record < copyFiles && copyFiles < join && join < start) {
		t.Errorf("the new daemons must be noted before the copy and join the running ones after it, before step 6")
	}
	if !strings.Contains(steps, `NEW_INITS="$NEW_INITS $init"`) {
		t.Error("step 1 does not note a daemon whose binary is absent")
	}
	if strings.Contains(onExitBody(t, script), "NEW_INITS") {
		t.Error("a failed update must not start a daemon it may not have installed")
	}
}
```

In `server/internal/updater/updater_test.go`, replace:

```go
	for _, name := range []string{DaemonBot, DaemonWebUI} {
```

with:

```go
	for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/updater/ -count=1 -run 'TestGenerateScript_StartsADaemonNewWithTheRelease|TestDaemonConstants'`
Expected: FAIL - `undefined: DaemonWatchd`.

- [ ] **Step 3: Add the daemon to the table**

In `server/internal/updater/updater.go`, replace:

```go
const (
	DaemonBot   = "telegram-bot"
	DaemonWebUI = "webui"
)
```

with:

```go
const (
	DaemonBot    = "telegram-bot"
	DaemonWatchd = "vpn-director-watchd"
	DaemonWebUI  = "webui"
)
```

and replace:

```go
var Daemons = []Daemon{
	{Name: DaemonBot, Binary: "/opt/vpn-director/telegram-bot", InitScript: "S98telegram-bot"},
	{Name: DaemonWebUI, Binary: "/opt/vpn-director/webui", InitScript: "S98vpn-director-webui"},
}
```

with:

```go
var Daemons = []Daemon{
	{Name: DaemonBot, Binary: "/opt/vpn-director/telegram-bot", InitScript: "S98telegram-bot"},
	{Name: DaemonWatchd, Binary: "/opt/vpn-director/vpn-director-watchd", InitScript: "S98vpn-director-watchd"},
	{Name: DaemonWebUI, Binary: "/opt/vpn-director/webui", InitScript: "S98vpn-director-webui"},
}
```

The Web UI stays last: `TestSelfUpdate_ChecksTheLockAgainBeforeTheScript` relies on its asset being step 2's last write.

- [ ] **Step 4: Start a new daemon after its first update**

In `server/internal/updater/update_script.sh.tmpl`, replace step 1:

```sh
# 1. Remember which daemons are running. Matching the full binary path keeps
#    pgrep off unrelated processes.
for entry in $DAEMONS; do
    name="${entry%%|*}"
    rest="${entry#*|}"
    bin="${rest%%|*}"
    init="${rest##*|}"
    if pgrep -f "$bin" >/dev/null 2>&1; then
        RUNNING_INITS="$RUNNING_INITS $init"
        log "$name is running"
    else
        log "$name is not running, it stays stopped after the update"
    fi
done
```

with:

```sh
# 1. Remember which daemons are running. Matching the full binary path keeps
#    pgrep off unrelated processes. A daemon whose binary is not there at all
#    is new with this release: nothing ran it, and nobody stopped it either,
#    so it starts once the copy has succeeded.
NEW_INITS=""
for entry in $DAEMONS; do
    name="${entry%%|*}"
    rest="${entry#*|}"
    bin="${rest%%|*}"
    init="${rest##*|}"
    if pgrep -f "$bin" >/dev/null 2>&1; then
        RUNNING_INITS="$RUNNING_INITS $init"
        log "$name is running"
    elif [ ! -e "$bin" ]; then
        NEW_INITS="$NEW_INITS $init"
        log "$name is new, it starts after the update"
    else
        log "$name is not running, it stays stopped after the update"
    fi
done
```

and after step 5, whose loop ends the permissions, insert the join - replace:

```sh
# 5. Set permissions of the daemon binaries (script files got theirs above)
for entry in $DAEMONS; do
    rest="${entry#*|}"
    chmod +x "$INIT_DIR/${entry##*|}"
    chmod +x "${rest%%|*}"
done
```

with:

```sh
# 5. Set permissions of the daemon binaries (script files got theirs above)
for entry in $DAEMONS; do
    rest="${entry#*|}"
    chmod +x "$INIT_DIR/${entry##*|}"
    chmod +x "${rest%%|*}"
done

# A daemon new with this release is installed now, so from here on it counts
# as one that was running: step 6 starts it with the others, before the bot.
RUNNING_INITS="$RUNNING_INITS$NEW_INITS"
```

- [ ] **Step 5: Give the fake releases the new asset**

The fixtures that serve a release by hand list its assets; step 2 now wants three.

In `server/internal/updater/github_test.go`, replace:

```go
var fakeAssets = map[string]string{"301": "telegram-bot-arm64", "302": "webui-arm64"}
```

with:

```go
var fakeAssets = map[string]string{"301": "telegram-bot-arm64", "302": "webui-arm64", "303": "vpn-director-watchd-arm64"}
```

and replace:

```go
			fmt.Fprintf(w, `{"tag_name": "v1.2.4", "body": "notes", "assets": [%s, %s]}`, f.asset("301"), f.asset("302"))
```

with:

```go
			fmt.Fprintf(w, `{"tag_name": "v1.2.4", "body": "notes", "assets": [%s, %s, %s]}`, f.asset("301"), f.asset("302"), f.asset("303"))
```

In `server/internal/updater/selfupdate_test.go`, replace:

```go
				{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/302", "id": 302, "name": "webui-arm64",
				 "browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/webui-arm64"}]}`, server.URL)
```

with:

```go
				{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/302", "id": 302, "name": "webui-arm64",
				 "browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/webui-arm64"},
				{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/303", "id": 303, "name": "vpn-director-watchd-arm64",
				 "browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/vpn-director-watchd-arm64"}]}`, server.URL)
```

In `server/internal/updater/downloader_test.go`, `TestDownloadBinaries_NeedsNoAssetForTheDaemonItIs` builds a release that carries every asset but the Web UI's; give it the new one - replace:

```go
// Step 2 is its own daemon's binary of the release it installs, so that asset
// is one the release need not carry at all: a release that ships only the
// other daemon's still installs.
```

with:

```go
// Step 2 is its own daemon's binary of the release it installs, so that asset
// is one the release need not carry at all: a release that ships only the
// other daemons' still installs.
```

and replace:

```go
	release := &Release{TagName: "v1.0.0", Assets: []Asset{
		{Name: DaemonBot + "-arm64", DownloadURL: server.URL + "/" + DaemonBot + "-arm64"},
	}}
```

with:

```go
	release := &Release{TagName: "v1.0.0", Assets: []Asset{
		{Name: DaemonBot + "-arm64", DownloadURL: server.URL + "/" + DaemonBot + "-arm64"},
		{Name: DaemonWatchd + "-arm64", DownloadURL: server.URL + "/" + DaemonWatchd + "-arm64"},
	}}
```

- [ ] **Step 6: Regenerate the golden script and review the diff**

Run: `cd server && UPDATE_GOLDEN=1 go test ./internal/updater -run TestGenerateScript_Golden -count=1 && git diff internal/updater/testdata/update_script.golden.sh`
Expected: `ok`, and a diff that is exactly this:

```diff
--- a/server/internal/updater/testdata/update_script.golden.sh
+++ b/server/internal/updater/testdata/update_script.golden.sh
@@ -21,7 +21,7 @@
 
 # Daemon table: "name|binary|init script" entries separated by spaces. The
 # loops below rely on word splitting, so no field may contain a space.
-DAEMONS="telegram-bot|/opt/vpn-director/telegram-bot|S98telegram-bot webui|/opt/vpn-director/webui|S98vpn-director-webui"
+DAEMONS="telegram-bot|/opt/vpn-director/telegram-bot|S98telegram-bot vpn-director-watchd|/opt/vpn-director/vpn-director-watchd|S98vpn-director-watchd webui|/opt/vpn-director/webui|S98vpn-director-webui"
 
 # File table: "src|dst|mode" entries separated by spaces, src relative to
 # FILES_DIR, mode "x" for executable or "-" for data. Word splitting again, so
@@ -178,7 +178,10 @@
 log "Starting update from $OLD_VERSION to $NEW_VERSION (initiator: $INITIATOR)"
 
 # 1. Remember which daemons are running. Matching the full binary path keeps
-#    pgrep off unrelated processes.
+#    pgrep off unrelated processes. A daemon whose binary is not there at all
+#    is new with this release: nothing ran it, and nobody stopped it either,
+#    so it starts once the copy has succeeded.
+NEW_INITS=""
 for entry in $DAEMONS; do
     name="${entry%%|*}"
     rest="${entry#*|}"
@@ -187,6 +190,9 @@
     if pgrep -f "$bin" >/dev/null 2>&1; then
         RUNNING_INITS="$RUNNING_INITS $init"
         log "$name is running"
+    elif [ ! -e "$bin" ]; then
+        NEW_INITS="$NEW_INITS $init"
+        log "$name is new, it starts after the update"
     else
         log "$name is not running, it stays stopped after the update"
     fi
@@ -265,6 +271,10 @@
     chmod +x "$INIT_DIR/${entry##*|}"
     chmod +x "${rest%%|*}"
 done
+
+# A daemon new with this release is installed now, so from here on it counts
+# as one that was running: step 6 starts it with the others, before the bot.
+RUNNING_INITS="$RUNNING_INITS$NEW_INITS"
 
 # Drop the payload copies. notify.json and update.log stay; the bot removes
 # the directory after a successful notify. A Web UI-only router has no bot
```

- [ ] **Step 7: Run the updater's tests**

Run: `cd server && gofmt -l internal/updater && go vet ./internal/updater/ && go test ./internal/updater/ -count=1`
Expected: `gofmt -l` prints nothing; `ok`.

- [ ] **Step 8: Commit**

```bash
git add server/internal/updater/updater.go server/internal/updater/update_script.sh.tmpl \
  server/internal/updater/script_test.go server/internal/updater/updater_test.go \
  server/internal/updater/github_test.go server/internal/updater/selfupdate_test.go \
  server/internal/updater/downloader_test.go server/internal/updater/testdata/update_script.golden.sh
git commit -m "feat(updater): ship vpn-director-watchd and start it after its first update

The daemon table gains vpn-director-watchd between the bot and the Web UI, so step 2 of a self-update downloads its asset like any other daemon's. The update script restarts only the daemons that ran before it, which would leave a daemon a release adds stopped: step 1 now notes every daemon whose binary does not exist yet, and once the copy has succeeded those start with the others, before the bot. A daemon the owner stopped stays stopped, and a failed update starts no new one."
```

### Task 9: The daemon

`cmd/watchd` wires the engine: the settings and the endpoints read from `vpn-director.json` and the subscription files, the real prober (a fake one in `--dev`), the stop marker, the WAN controls, the state file and the socket. It rereads its sources every minute, but rebuilds the endpoint set only when a file changed (`Stamp`: name, inode, size and modification time - a writer replaces a file by a rename, which gives a new inode, and JFFS2 keeps modification times to the second). Like the other daemons it answers `self-update` first (the updater contract requires every binary of the table to) and moves to `/` at startup.

**Files:**
- Modify: `server/internal/paths/paths.go`, `server/internal/paths/paths_test.go`
- Create: `server/internal/monitor/wan.go`, `server/internal/monitor/stamp.go`
- Test: `server/internal/monitor/wan_test.go`, `server/internal/monitor/stamp_test.go`
- Create: `server/cmd/watchd/main.go`
- Modify: `.gitignore` (the dev run's socket, state, log and prober config)

**Interfaces:**
- Consumes: everything of Tasks 2-8: `monitor.New`, `Deps`, `SettingsFrom`, `Build`, `XrayLauncher`, `FakeLauncher`, `KillLeftovers`, `watchdapi.Serve`, `service.OutboundJSON`, `endpoint.WANControls`, `updater.DaemonWatchd`, `updater.RunSelfUpdate`.
- Produces:
  - `paths.Paths` fields `WatchdLogPath`, `WatchdSocket`, `WatchdState`, `ProbeDir`, `ProbeBinary`, `StoppedMarker`; `RotatedLogs()` gains `WatchdLogPath`.
  - `monitor.WANUp(ctx context.Context, controls []vpnconfig.Server, timeout time.Duration) bool`.
  - `monitor.Stamp(dir string, files ...string) string`.
  - The binary `vpn-director-watchd`: `--config` (default `/opt/vpn-director/vpn-director.json`), `--dev`.

- [ ] **Step 1: Write the failing tests**

In `server/internal/paths/paths_test.go`, extend the three tables. In `TestDefault`, after `{"XrayLogPath", p.XrayLogPath, "/tmp/", "xray-error.log"},` add:

```go
		{"WatchdLogPath", p.WatchdLogPath, "/tmp/", "vpn-director-watchd.log"},
		{"WatchdSocket", p.WatchdSocket, "/tmp/vpn-director/", "watchd.sock"},
		{"WatchdState", p.WatchdState, "/tmp/vpn-director/", "watchd-state.json"},
		{"ProbeDir", p.ProbeDir, "/tmp/vpn-director/", "probe"},
		{"ProbeBinary", p.ProbeBinary, "/opt/vpn-director/", "vpn-director-probe"},
		{"StoppedMarker", p.StoppedMarker, "/tmp/vpn-director/", "stopped"},
```

In `TestDevPaths`, after `{"XrayLogPath", p.XrayLogPath, "testdata/dev/", "xray-error.log"},` add:

```go
		{"WatchdLogPath", p.WatchdLogPath, "testdata/dev/", "watchd.log"},
		{"WatchdSocket", p.WatchdSocket, "testdata/dev/", "watchd.sock"},
		{"WatchdState", p.WatchdState, "testdata/dev/", "watchd-state.json"},
		{"ProbeDir", p.ProbeDir, "testdata/dev/", "probe"},
		{"ProbeBinary", p.ProbeBinary, "testdata/dev/", "vpn-director-probe"},
		{"StoppedMarker", p.StoppedMarker, "testdata/dev/", "stopped"},
```

In `TestRotatedLogs`, replace `want := []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath}` with `want := []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath, p.WatchdLogPath}`.

Create `server/internal/monitor/wan_test.go`:

```go
package monitor

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestWANUp_AnyControlAcceptingIsUp(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	up := []vpnconfig.Server{{Address: "127.0.0.1", Port: closedPort}, {Address: "127.0.0.1", Port: port}}
	if !WANUp(context.Background(), up, time.Second) {
		t.Fatal("one control accepts, yet the WAN reads down")
	}
	down := []vpnconfig.Server{{Address: "127.0.0.1", Port: closedPort}}
	if WANUp(context.Background(), down, time.Second) {
		t.Fatal("no control accepts, yet the WAN reads up: " + strconv.Itoa(closedPort))
	}
}
```

Create `server/internal/monitor/stamp_test.go`:

```go
package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

// A writer replaces a file by a rename; the stamp sees it even at the same
// size and within the same second, as JFFS2 keeps modification times.
func TestStamp_SeesARenameAtTheSameSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "0a1b2c3d.json")
	cfg := filepath.Join(dir, "vpn-director.json")
	for _, p := range []string{sub, cfg} {
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := Stamp(dir, cfg)
	if Stamp(dir, cfg) != before {
		t.Fatal("two stamps of untouched files differ")
	}

	info, _ := os.Stat(sub)
	tmp := sub + ".tmp"
	if err := os.WriteFile(tmp, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, sub); err != nil {
		t.Fatal(err)
	}
	if Stamp(dir, cfg) == before {
		t.Fatal("the stamp missed a replaced file")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/paths/ ./internal/monitor/ -count=1`
Expected: FAIL - `p.WatchdLogPath undefined`, `undefined: WANUp`, `undefined: Stamp`.

- [ ] **Step 3: Add the paths**

In `server/internal/paths/paths.go`, replace the last field of `Paths`:

```go
	XrayLogPath    string // /tmp/xray-error.log (set by the log section of the Xray template)
}
```

with:

```go
	XrayLogPath    string // /tmp/xray-error.log (set by the log section of the Xray template)
	WatchdLogPath  string // /tmp/vpn-director-watchd.log
	WatchdSocket   string // /tmp/vpn-director/watchd.sock: the monitor's API
	WatchdState    string // /tmp/vpn-director/watchd-state.json: the monitor's state across restarts
	ProbeDir       string // /tmp/vpn-director/probe: the prober's config
	ProbeBinary    string // /opt/vpn-director/vpn-director-probe: a hard link to xray
	// StoppedMarker is the file vpn-director.sh stop writes and a full apply
	// removes; bot/path.go reads the same file for the subscription watch.
	StoppedMarker string // /tmp/vpn-director/stopped
}
```

In `Default()`, after `XrayLogPath:    "/tmp/xray-error.log",` add:

```go
		WatchdLogPath:  "/tmp/vpn-director-watchd.log",
		WatchdSocket:   "/tmp/vpn-director/watchd.sock",
		WatchdState:    "/tmp/vpn-director/watchd-state.json",
		ProbeDir:       "/tmp/vpn-director/probe",
		ProbeBinary:    "/opt/vpn-director/vpn-director-probe",
		StoppedMarker:  "/tmp/vpn-director/stopped",
```

In `DevPaths()`, after `XrayLogPath:    "testdata/dev/xray-error.log",` add:

```go
		WatchdLogPath:  "testdata/dev/watchd.log",
		WatchdSocket:   "testdata/dev/watchd.sock",
		WatchdState:    "testdata/dev/watchd-state.json",
		ProbeDir:       "testdata/dev/probe",
		ProbeBinary:    "testdata/dev/vpn-director-probe",
		StoppedMarker:  "testdata/dev/stopped",
```

Replace `RotatedLogs`:

```go
// RotatedLogs lists every log file the daemons truncate at
// logging.DefaultMaxSize. The bot and the Web UI rotate the same list;
// os.Truncate is idempotent, so two processes rotating at once are safe.
func (p Paths) RotatedLogs() []string {
	return []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath}
}
```

with:

```go
// RotatedLogs lists every log file the daemons truncate at
// logging.DefaultMaxSize. Every daemon rotates the same list; os.Truncate is
// idempotent, so two processes rotating at once are safe.
func (p Paths) RotatedLogs() []string {
	return []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath, p.WatchdLogPath}
}
```

- [ ] **Step 4: Write `WANUp` and `Stamp`**

Create `server/internal/monitor/wan.go`:

```go
package monitor

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// WANUp reports whether any of controls accepts a TCP connection within
// timeout; all are dialed at once.
func WANUp(ctx context.Context, controls []vpnconfig.Server, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	answers := make(chan bool, len(controls))
	for _, c := range controls {
		go func(addr string) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp4", addr)
			if err == nil {
				conn.Close()
			}
			answers <- err == nil
		}(net.JoinHostPort(c.Address, strconv.Itoa(c.Port)))
	}
	for range controls {
		if <-answers {
			return true
		}
	}
	return false
}
```

Create `server/internal/monitor/stamp.go`:

```go
package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Stamp names the state of the files the endpoint set is built from: every
// *.json in dir and each of files, by name, inode, size and modification
// time. Every writer replaces those files by a rename, which gives a new
// inode, so equal stamps mean nothing was written - JFFS2 keeps modification
// times to the second, and a size can repeat. A file that is missing stamps
// as missing.
func Stamp(dir string, files ...string) string {
	var parts []string
	add := func(path string) {
		info, err := os.Stat(path)
		if err != nil {
			parts = append(parts, path+":missing")
			return
		}
		var ino uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ino = uint64(st.Ino)
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%d:%d", path, ino, info.Size(), info.ModTime().UnixNano()))
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(names)
	for _, name := range names {
		add(name)
	}
	for _, f := range files {
		add(f)
	}
	return strings.Join(parts, "|")
}
```

Run: `cd server && gofmt -w internal/paths && gofmt -l internal/paths internal/monitor && go test ./internal/paths/ ./internal/monitor/ -count=1`
Expected: `gofmt -l` prints nothing; `ok` for both.

- [ ] **Step 5: Write the daemon**

Create `server/cmd/watchd/main.go`:

```go
// vpn-director-watchd checks, minute by minute, whether every server of every
// subscription carries traffic, and serves what it finds on a unix socket to
// the Web UI and the bot (internal/monitor, internal/watchdapi).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/logging"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updater"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func main() {
	// Step 2 of a self-update (internal/updater/selfupdate.go): every binary
	// of the daemon table implements it, and nothing a daemon does at startup
	// may run first.
	if len(os.Args) > 1 && os.Args[1] == updater.SelfUpdateCommand {
		os.Exit(updater.RunSelfUpdate(os.Args[2:], updater.DaemonWatchd, Version, os.Stdout, os.Stderr))
	}

	configPath := flag.String("config", "/opt/vpn-director/vpn-director.json", "path to vpn-director.json")
	devFlag := flag.Bool("dev", false, "run in development mode (testdata paths, a fake prober)")
	flag.Parse()

	var p paths.Paths
	var detachErr error
	if *devFlag {
		p = paths.DevPaths()
		if *configPath == "/opt/vpn-director/vpn-director.json" {
			*configPath = p.ScriptsDir + "/vpn-director.json"
		}
		if _, err := os.Stat(p.ScriptsDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: %s not found\n", p.ScriptsDir)
			fmt.Fprintf(os.Stderr, "Run from server/ directory: cd server && go run ./cmd/watchd --dev\n")
			os.Exit(1)
		}
	} else {
		p = paths.Default()
		// The update script starts this daemon from a directory the bot
		// deletes moments later (paths.DetachFromCallerDirectory).
		detachErr = paths.DetachFromCallerDirectory(configPath)
	}

	slogger, logger, err := logging.NewSlogLogger(p.WatchdLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()
	slog.SetDefault(slogger)
	slog.Info("starting vpn-director-watchd", "version", Version, "commit", Commit, "dev", *devFlag)
	if detachErr != nil {
		slog.Warn("could not leave the directory this process was started in", "error", detachErr)
	}

	scriptsDir := filepath.Dir(*configPath)
	configSvc := service.NewConfigService(scriptsDir, filepath.Join(scriptsDir, "data"), *configPath)

	var launcher monitor.Launcher
	if *devFlag {
		launcher = monitor.FakeLauncher{}
	} else {
		monitor.KillLeftovers(p.ProbeBinary)
		launcher = &monitor.XrayLauncher{ProbeBinary: p.ProbeBinary, ConfigDir: p.ProbeDir}
	}

	m := monitor.New(monitor.Deps{
		Settings:  settingsReader(configSvc),
		Endpoints: endpointsReader(configSvc),
		Launcher:  launcher,
		Stopped: func() bool {
			_, err := os.Stat(p.StoppedMarker)
			return err == nil
		},
		WANUp: func(ctx context.Context) bool {
			return monitor.WANUp(ctx, endpoint.WANControls, 3*time.Second)
		},
		StatePath:  p.WatchdState,
		OnSettings: levelSetter(logger),
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.StartRotation(ctx, p.RotatedLogs(), logging.DefaultMaxSize, time.Minute)

	go func() {
		if err := watchdapi.Serve(ctx, p.WatchdSocket, m); err != nil {
			slog.Error("the monitor's socket stopped", "path", p.WatchdSocket, "error", err)
		}
	}()
	m.Run(ctx)
	slog.Info("vpn-director-watchd stopped")
}

// settingsReader reads the monitor section at every refresh and warns once
// about each distinct set of values out of bounds.
func settingsReader(configSvc *service.ConfigService) func() (monitor.Settings, error) {
	var lastWarns string
	return func() (monitor.Settings, error) {
		cfg, err := configSvc.LoadVPNConfig()
		if err != nil {
			return monitor.Settings{}, err
		}
		s, warns := monitor.SettingsFrom(cfg.Monitor)
		if joined := strings.Join(warns, "\n"); joined != lastWarns {
			lastWarns = joined
			for _, w := range warns {
				slog.Warn(w)
			}
		}
		return s, nil
	}
}

// endpointsReader builds the endpoint set, again only when a subscription
// file or the config changed (monitor.Stamp): the build decodes every
// outbound.
func endpointsReader(configSvc *service.ConfigService) func() ([]monitor.Endpoint, map[string]string, error) {
	var lastStamp string
	var lastEps []monitor.Endpoint
	var lastRefused map[string]string
	return func() ([]monitor.Endpoint, map[string]string, error) {
		dir, err := configSvc.SubscriptionsDir()
		if err != nil {
			return nil, nil, err
		}
		stamp := monitor.Stamp(dir, configSvc.ConfigPath())
		if stamp == lastStamp {
			return lastEps, lastRefused, nil
		}
		subs, err := configSvc.LoadSubscriptions()
		if err != nil {
			return nil, nil, err
		}
		var active *vpnconfig.ActiveServer
		if cfg, err := configSvc.LoadVPNConfig(); err == nil {
			active = cfg.Xray.ActiveServer
		}
		eps, refused := monitor.Build(subs, active, func(s vpnconfig.Server) (json.RawMessage, error) {
			return service.OutboundJSON(s, "")
		})
		lastStamp, lastEps, lastRefused = stamp, eps, refused
		return eps, refused, nil
	}
}

// levelSetter follows monitor.log_level, and only when it changes: SetLevel
// warns about a level it does not know.
func levelSetter(logger *logging.Logger) func(monitor.Settings) {
	last := "\x00"
	return func(s monitor.Settings) {
		if s.LogLevel != last {
			last = s.LogLevel
			logger.SetLevel(s.LogLevel)
		}
	}
}
```

- [ ] **Step 6: Build it and run it in dev mode**

Run: `cd server && gofmt -l cmd/watchd && go vet ./cmd/watchd/ ./internal/paths/ ./internal/monitor/ && go build -o /tmp/vpn-director-watchd-dev ./cmd/watchd`
Expected: no output.

In `.gitignore`, after the `server/testdata/dev/data/` line, add:

```
# The server monitor's dev run: its socket, state, log and prober config
server/testdata/dev/watchd.sock
server/testdata/dev/watchd-state.json
server/testdata/dev/watchd.log
server/testdata/dev/probe/
```

Run the daemon for a few seconds on the dev config (from `server/`, where `testdata/dev` lives). `testdata/dev/vpn-director.json` and `testdata/dev/data/` are gitignored; the Web UI's `--dev` creates the config, and a minimal one does here when there is none:

```bash
cd server
[ -f testdata/dev/vpn-director.json ] || echo '{"data_dir":"data","xray":{},"tunnel_director":{"tunnels":{}}}' > testdata/dev/vpn-director.json
mkdir -p testdata/dev/data/subscriptions && cp ../testdata/substore/*.json testdata/dev/data/subscriptions/
/tmp/vpn-director-watchd-dev --dev & pid=$!
sleep 6
curl -s --unix-socket testdata/dev/watchd.sock http://watchd/v1/monitor | jq -c '{state, interval_seconds, n: (.endpoints | length), statuses: [.endpoints[] | .status]}'
curl -s -X POST --unix-socket testdata/dev/watchd.sock http://watchd/v1/monitor/check -d '{}'; echo
stat -c '%a' testdata/dev/watchd.sock
kill -TERM $pid; wait $pid; echo "exit=$?"
jq -c '{n: (.entries | length)}' testdata/dev/watchd-state.json
```

Expected: `{"state":"ok","interval_seconds":60,"n":4,"statuses":[...]}` with three `alive` and one `dead` in some order (the fake prober answers by key: `Gamma / Riga` fails); `{"queued":4}`; `600`; `exit=0`; `{"n":4}`. The log `testdata/dev/watchd.log` shows `Monitor: server alive` three times and `Monitor: server down` once.

Clean up: `rm -rf testdata/dev/data/subscriptions /tmp/vpn-director-watchd-dev` (the copied fixtures would otherwise show in the Web UI's dev mode). `git status` must show only the files of this task: the run's own files are ignored now.

- [ ] **Step 7: Commit**

```bash
git add server/internal/paths/paths.go server/internal/paths/paths_test.go \
  server/internal/monitor/wan.go server/internal/monitor/wan_test.go \
  server/internal/monitor/stamp.go server/internal/monitor/stamp_test.go server/cmd/watchd/main.go .gitignore
git commit -m "feat(watchd): the vpn-director-watchd daemon

cmd/watchd runs the monitor on the settings and subscriptions of vpn-director.json, rebuilding the endpoint set only when a file was written (Stamp), with the real prober or, in dev mode, the fake one; it serves the socket, saves its state and dies with its prober. Like every binary of the daemon table it answers self-update first, and it moves to / at startup."
```

### Task 10: Shipping - the init script, the manifest and the release build

The daemon is started at boot by an Entware init script modelled on the Web UI's, which the manifest installs and the updater copies; the release workflow and both Makefiles build it for the three router architectures.

**Files:**
- Create: `router/opt/etc/init.d/S98vpn-director-watchd` (mode 0755)
- Modify: `router/files.manifest`
- Modify: `server/Makefile`, `Makefile`
- Modify: `.github/workflows/telegram-bot.yml`

**Interfaces:**
- Consumes: `cmd/watchd` (Task 9), the table entry (Task 8).
- Produces: the release assets `vpn-director-watchd-arm64`, `vpn-director-watchd-arm`, `vpn-director-watchd-mipsle`; the make targets `build-watchd`, `build-watchd-arm64`, `build-watchd-arm`, `build-watchd-mipsle`.

- [ ] **Step 1: Write the init script**

Create `router/opt/etc/init.d/S98vpn-director-watchd` and make it executable (`chmod 755`). It is `S98vpn-director-webui` with the daemon's path, name, description and log tag: `pidof` and `killall` find the 19-character name through argv[0] and `/proc/PID/exe`, although the kernel keeps 15 characters of it in `comm`.

```sh
#!/bin/sh

# Note: We don't source rc.func because it calls `pidof $PROCS` with the full
# path stored in $PROCS, but BusyBox pidof matches by basename only. Running
# the script via rc.func would always report "failed" after ~10s even though
# the process started successfully. We also use `nohup` to detach the daemon
# from the controlling terminal so it survives an SSH logout when the script
# is invoked manually. (`setsid` is not provided by BusyBox 1.25 on Merlin.)
PATH=/opt/sbin:/opt/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENABLED=yes
DESC="VPN Director server monitor"
WATCHD_PATH="/opt/vpn-director/vpn-director-watchd"
WATCHD_NAME="vpn-director-watchd"
WATCHD_CONFIG="/opt/vpn-director/vpn-director.json"
LOGTAG="S98vpn-director-watchd"

start() {
    [ "$ENABLED" != "yes" ] && return 0
    if [ ! -x "$WATCHD_PATH" ]; then
        logger -t "$LOGTAG" "vpn-director-watchd not found at $WATCHD_PATH"
        return 0
    fi
    echo "Starting $DESC..."
    if pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "$DESC is already running"
        return 0
    fi
    nohup "$WATCHD_PATH" --config "$WATCHD_CONFIG" </dev/null >/dev/null 2>&1 &
    sleep 2
    if pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "$DESC started"
        logger -t "$LOGTAG" "Started $DESC"
    else
        echo "Failed to start $DESC"
        logger -t "$LOGTAG" "Failed to start $DESC"
        return 1
    fi
}

stop() {
    echo "Stopping $DESC..."
    if ! pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "$DESC is not running"
        return 0
    fi
    killall "$WATCHD_NAME" 2>/dev/null
    count=0
    while pidof "$WATCHD_NAME" >/dev/null 2>&1 && [ "$count" -lt 10 ]; do
        sleep 1
        count=$((count + 1))
    done
    if pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "Force killing $DESC..."
        killall -9 "$WATCHD_NAME" 2>/dev/null
        sleep 1
    fi
    if pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "Failed to stop $DESC"
        logger -t "$LOGTAG" "Failed to stop $DESC"
        return 1
    fi
    echo "$DESC stopped"
    return 0
}

check() {
    if pidof "$WATCHD_NAME" >/dev/null 2>&1; then
        echo "$DESC is running"
        return 0
    fi
    echo "$DESC is not running"
    return 1
}

case "$1" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; start ;;
    kill)    killall -9 "$WATCHD_NAME" 2>/dev/null ;;
    check)   check ;;
    *)       echo "Usage: $0 {start|stop|restart|kill|check}" ;;
esac
```

- [ ] **Step 2: Check it**

Run: `sh -n router/opt/etc/init.d/S98vpn-director-watchd && shellcheck router/opt/etc/init.d/S98vpn-director-watchd router/opt/etc/init.d/S98vpn-director-webui`
Expected: no output from `sh -n`; shellcheck reports for the new script exactly what it reports for `S98vpn-director-webui` (the two differ only in names).

- [ ] **Step 3: Install it through the manifest**

In `router/files.manifest`, replace:

```
common   router/opt/etc/init.d/S98vpn-director-webui
```

with:

```
common   router/opt/etc/init.d/S98vpn-director-webui
common   router/opt/etc/init.d/S98vpn-director-watchd
```

Run: `bats router/test/unit/install.bats`
Expected: every test passes (the manifest tests parse the real file).

- [ ] **Step 4: Build it**

In `server/Makefile`, replace:

```make
.PHONY: build build-bot build-arm64 build-arm build-mipsle build-webui build-webui-arm64 build-webui-arm build-webui-mipsle clean test
```

with:

```make
.PHONY: build build-bot build-arm64 build-arm build-mipsle build-webui build-webui-arm64 build-webui-arm build-webui-mipsle build-watchd build-watchd-arm64 build-watchd-arm build-watchd-mipsle clean test
```

replace `build: build-bot build-webui` with `build: build-bot build-webui build-watchd`, and replace:

```make
clean:
	rm -rf bin/
```

with:

```make
build-watchd:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o bin/vpn-director-watchd ./cmd/watchd

build-watchd-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath $(LDFLAGS) -o bin/vpn-director-watchd-arm64 ./cmd/watchd

build-watchd-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath $(LDFLAGS) -o bin/vpn-director-watchd-arm ./cmd/watchd

build-watchd-mipsle:
	CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath $(LDFLAGS) -o bin/vpn-director-watchd-mipsle ./cmd/watchd

clean:
	rm -rf bin/
```

In the root `Makefile`, replace:

```make
.PHONY: web web-embed build-webui build-webui-arm64 build-webui-arm build-webui-mipsle build-bot build-bot-arm64 build-bot-arm build-bot-mipsle build-all clean
```

with:

```make
.PHONY: web web-embed build-webui build-webui-arm64 build-webui-arm build-webui-mipsle build-bot build-bot-arm64 build-bot-arm build-bot-mipsle build-watchd build-watchd-arm64 build-watchd-arm build-watchd-mipsle build-all clean
```

and replace:

```make
# Build all
build-all: build-webui-arm64 build-webui-arm build-webui-mipsle build-bot-arm64 build-bot-arm build-bot-mipsle
```

with:

```make
# Build the server monitor (no SPA to embed)
build-watchd:
	make -C server build-watchd

build-watchd-arm64:
	make -C server build-watchd-arm64

build-watchd-arm:
	make -C server build-watchd-arm

build-watchd-mipsle:
	make -C server build-watchd-mipsle

# Build all
build-all: build-webui-arm64 build-webui-arm build-webui-mipsle build-bot-arm64 build-bot-arm build-bot-mipsle build-watchd-arm64 build-watchd-arm build-watchd-mipsle
```

In `.github/workflows/telegram-bot.yml`, replace:

```yaml
      - name: Build webui mipsle
        run: make -C server build-webui-mipsle
```

with:

```yaml
      - name: Build webui mipsle
        run: make -C server build-webui-mipsle

      - name: Build watchd arm64
        run: make -C server build-watchd-arm64

      - name: Build watchd arm
        run: make -C server build-watchd-arm

      - name: Build watchd mipsle
        run: make -C server build-watchd-mipsle
```

and replace:

```yaml
            bin/webui-arm64
            bin/webui-arm
            bin/webui-mipsle
```

with:

```yaml
            bin/webui-arm64
            bin/webui-arm
            bin/webui-mipsle
            bin/vpn-director-watchd-arm64
            bin/vpn-director-watchd-arm
            bin/vpn-director-watchd-mipsle
```

- [ ] **Step 5: Build the three router binaries**

Run: `make -C server build-watchd-arm64 build-watchd-arm build-watchd-mipsle && ls -l server/bin/vpn-director-watchd-*`
Expected: three binaries of roughly 9 MB each (the bot's are 9-10 MB). Delete them afterwards: `rm server/bin/vpn-director-watchd-*`.

- [ ] **Step 6: Commit**

```bash
git add router/opt/etc/init.d/S98vpn-director-watchd router/files.manifest server/Makefile Makefile .github/workflows/telegram-bot.yml
git commit -m "build(watchd): the init script, the manifest and the release assets

S98vpn-director-watchd starts the monitor at boot like the Web UI's script starts it, the manifest installs it, and the release workflow and both Makefiles build vpn-director-watchd for arm64, arm and mipsle."
```

### Task 11: `install.sh` installs and starts the monitor

The installer downloads the daemon as an optional component, the way it downloads the Web UI - a failed download does not stop the install - and starts it once the config exists.

**Files:**
- Modify: `install.sh` (two functions, two calls in `main`)
- Test: `router/test/unit/install.bats`

**Interfaces:**
- Consumes: the release asset `vpn-director-watchd-<arch>` (Task 10), `release_arch`, `RELEASE_ASSET_URL`, `VPD_DIR`, `INIT_DIR` (existing).
- Produces: `download_watchd`, `start_watchd`.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/install.bats`:

```bash
# fake_watchd_init writes a stand-in for the monitor's init script that records
# its arguments and exits with the given code.
fake_watchd_init() {
    local code="${1:-0}"
    cat > "$INIT_DIR/S98vpn-director-watchd" <<EOF2
#!/bin/sh
echo "\$1" >> "$BATS_TEST_TMPDIR/watchd.calls"
exit $code
EOF2
    chmod +x "$INIT_DIR/S98vpn-director-watchd"
}

@test "start_watchd: does nothing when the monitor binary was not installed" {
    load_installer
    fake_watchd_init 0

    run start_watchd

    assert_success
    [[ ! -f "$BATS_TEST_TMPDIR/watchd.calls" ]]
}

@test "start_watchd: starts the monitor" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"
    fake_watchd_init 0

    run start_watchd

    assert_success
    assert_output --partial "Server monitor started"
    run cat "$BATS_TEST_TMPDIR/watchd.calls"
    assert_output "start"
}

@test "start_watchd: a failed start is reported without aborting the installer" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"
    fake_watchd_init 1

    run start_watchd

    assert_success
    assert_output --partial "Failed to start the server monitor"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/install.bats`
Expected: the three `start_watchd` tests fail with `start_watchd: command not found`; every other test passes.

- [ ] **Step 3: Write the functions**

In `install.sh`, insert this section, followed by a blank line, right above the banner of `start_webui` (the three comment lines around `# Start Web UI`):

```bash
###############################################################################
# Download the server monitor binary (optional)
###############################################################################

download_watchd() {
    print_info "Downloading the server monitor binary..."

    local arch_suffix
    if ! arch_suffix="$(release_arch)"; then
        print_info "Architecture $(uname -m) not supported for the server monitor (optional component)"
        return 0
    fi
    local watchd_path="$VPD_DIR/vpn-director-watchd"
    local tmp_path="${watchd_path}.tmp"

    if ! curl -fsSL "$RELEASE_ASSET_URL/vpn-director-watchd-$arch_suffix" -o "$tmp_path"; then
        print_info "Warning: Failed to download the server monitor (optional component)"
        rm -f "$tmp_path" 2>/dev/null || true
        return 0
    fi

    # Stop a running monitor before its binary is replaced; start_watchd
    # starts it again.
    if pidof vpn-director-watchd >/dev/null 2>&1; then
        print_info "Stopping the running server monitor..."
        if [[ -x "$INIT_DIR/S98vpn-director-watchd" ]]; then
            "$INIT_DIR/S98vpn-director-watchd" stop >/dev/null 2>&1 || true
        else
            killall vpn-director-watchd 2>/dev/null || true
        fi
        sleep 1
    fi

    mv "$tmp_path" "$watchd_path"
    chmod +x "$watchd_path"
    print_success "Installed the server monitor"
}
```

and this one, followed by a blank line, right above the banner around `# Print next steps`:

```bash
###############################################################################
# Start the server monitor
###############################################################################

start_watchd() {
    local watchd_path="$VPD_DIR/vpn-director-watchd"
    local init_script="$INIT_DIR/S98vpn-director-watchd"

    # download_watchd skips unsupported architectures and tolerates a failed
    # download, so there is not always something to start.
    if [[ ! -x "$watchd_path" ]]; then
        return 0
    fi
    if [[ ! -x "$init_script" ]]; then
        print_info "Server monitor init script not found, skipping start"
        return 0
    fi
    # The init script's start is a no-op when the monitor is up.
    if ! "$init_script" start >/dev/null 2>&1; then
        print_error "Failed to start the server monitor - see /tmp/vpn-director-watchd.log"
        return 0
    fi
    print_success "Server monitor started"
}
```

In `main`, replace:

```bash
    download_telegram_bot
    download_webui
    generate_tls_cert
    setup_webui_config
    start_webui
    print_next_steps
```

with:

```bash
    download_telegram_bot
    download_webui
    download_watchd
    generate_tls_cert
    setup_webui_config
    start_webui
    start_watchd
    print_next_steps
```

- [ ] **Step 4: Run the tests**

Run: `bash -n install.sh && bats router/test/unit/install.bats`
Expected: every test passes.

- [ ] **Step 5: Commit**

```bash
git add install.sh router/test/unit/install.bats
git commit -m "feat(install): install and start the server monitor

install.sh downloads vpn-director-watchd as an optional component, stopping a running one before its binary is replaced, and starts it once the config exists; a failed download or start is reported without stopping the install."
```

### Task 12: The Web UI's routes

Two routes read the daemon: `GET /api/monitor` folds its endpoint states per server (`watchdapi.Health` over `endpoint.Keys`) and names each server by index and fingerprint; `POST /api/monitor/check` queues one server - every address of it - or all. `GET /api/servers` gains the same fingerprint, so the page can tell that its two lists still agree; the Select flow does not change. The fingerprint is the one `/xray` buttons have always carried, so it moves from the bot's handler to `vpnconfig`, where both daemons reach it. The Logs tab gains the monitor's log.

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (add `ServerFingerprint`), `server/internal/vpnconfig/vpnconfig_test.go`
- Modify: `server/internal/handler/xray.go` (call `vpnconfig.ServerFingerprint`), `server/internal/handler/xray_test.go`
- Create: `server/internal/webapi/handler_monitor.go`
- Test: `server/internal/webapi/handler_monitor_test.go`
- Modify: `server/internal/webapi/handler_servers.go`, `server/internal/webapi/router.go`
- Modify: `server/cmd/webui/main.go`

**Interfaces:**
- Consumes: `watchdapi.API`, `NewClient`, `Health`, `Snapshot`, `StateNotRunning`, `ErrNotActive` (Task 2); `endpoint.Keys` (Task 1); `paths.WatchdSocket`, `paths.WatchdLogPath` (Task 9).
- Produces:
  - `vpnconfig.ServerFingerprint(s vpnconfig.Server) string` - the first 8 hex digits of sha256(`"subscription|name|address|port"`); the bot's former `serverFingerprint`.
  - `webapi.Deps.Monitor watchdapi.API` (nil reads as not running).
  - `GET /api/monitor` → `{state, message?, interval_seconds, lag_seconds, subscriptions: [{id, alive, total, servers: [{index, fingerprint, status, latency_ms, checked_at, since, next_at, error?}]}]}`.
  - `POST /api/monitor/check` with `{subscription, index, fingerprint}` or `{}` → 200 `{queued}`; 409 `server list changed`; 409 while stopped or disabled; 503 when the daemon does not answer.
  - `GET /api/servers`: every server gains `fingerprint`.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/vpnconfig/vpnconfig_test.go`:

```go
// The Web UI and the bot name a server by it, so it must stay what /xray has
// always sent: the first 8 hex digits of sha256("subscription|name|address|port").
func TestServerFingerprint_IsTheButtonsOwn(t *testing.T) {
	s := Server{Subscription: "0a1b2c3d", Name: "Germany-1", Address: "de.example.com", Port: 443}
	if got := ServerFingerprint(s); got != "84130acd" {
		t.Fatalf("fingerprint %q", got)
	}
	twin := s
	twin.Subscription = "1b2c3d4e"
	if ServerFingerprint(s) == ServerFingerprint(twin) {
		t.Fatal("one fingerprint for two subscriptions")
	}
}
```

Create `server/internal/webapi/handler_monitor_test.go`:

```go
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeMonitor is vpn-director-watchd for the handler tests.
type fakeMonitor struct {
	snap     watchdapi.Snapshot
	err      error
	checkErr error
	checked  [][]string
}

func (f *fakeMonitor) Monitor(context.Context) (watchdapi.Snapshot, error) { return f.snap, f.err }

func (f *fakeMonitor) Check(_ context.Context, keys []string) (int, error) {
	f.checked = append(f.checked, keys)
	return len(keys), f.checkErr
}

func trojanServer(name, ip string) vpnconfig.Server {
	return vpnconfig.Server{Name: name, Address: name + ".example", Port: 443, IPs: []string{ip},
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + name + `.example","port":443,"password":"secret"}]}}`)}
}

func monitorDeps(t *testing.T, mon watchdapi.API) (*Deps, []vpnconfig.Server) {
	t.Helper()
	servers := []vpnconfig.Server{trojanServer("oslo", "192.0.2.1"), trojanServer("riga", "192.0.2.2")}
	deps := newTestDeps(t)
	deps.Config = &mockConfig{subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: servers}}}
	deps.Monitor = mon
	for i := range servers {
		servers[i].Subscription = "0a1b2c3d"
	}
	return deps, servers
}

func TestHandleMonitor_FoldsTheEndpointsOfEveryServer(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	mon.snap = watchdapi.Snapshot{State: watchdapi.StateOK, IntervalSeconds: 60, Endpoints: map[string]watchdapi.EndpointState{
		endpoint.Keys(servers[0])[0]: {Status: watchdapi.StatusAlive, LatencyMS: 142},
		endpoint.Keys(servers[1])[0]: {Status: watchdapi.StatusDead, Error: "timeout"},
	}}

	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))

	var resp monitorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.State != watchdapi.StateOK || resp.IntervalSeconds != 60 || len(resp.Subscriptions) != 1 {
		t.Fatalf("response %+v", resp)
	}
	sub := resp.Subscriptions[0]
	if sub.ID != "0a1b2c3d" || sub.Alive != 1 || sub.Total != 2 || len(sub.Servers) != 2 {
		t.Fatalf("subscription %+v", sub)
	}
	if s := sub.Servers[0]; s.Index != 0 || s.Fingerprint != vpnconfig.ServerFingerprint(servers[0]) || s.Status != watchdapi.StatusAlive || s.LatencyMS != 142 {
		t.Fatalf("server 0 %+v", s)
	}
	if s := sub.Servers[1]; s.Status != watchdapi.StatusDead || s.Error != "timeout" {
		t.Fatalf("server 1 %+v", s)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("secret")) {
		t.Fatalf("the response carries a credential: %s", rec.Body)
	}
}

// Without the daemon the page still gets every server, as not checked.
func TestHandleMonitor_NoDaemonIsNotRunning(t *testing.T) {
	deps, _ := monitorDeps(t, &fakeMonitor{err: errors.New("dial unix: no such file")})

	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))

	var resp monitorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if resp.State != watchdapi.StateNotRunning || resp.Subscriptions[0].Servers[0].Status != watchdapi.StatusUnknown {
		t.Fatalf("response %+v", resp)
	}
}

func post(t *testing.T, deps *Deps, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handleMonitorCheck(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/monitor/check", bytes.NewBufferString(body)))
	return rec
}

func TestHandleMonitorCheck_QueuesEveryAddressOfTheServer(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	body := `{"subscription":"0a1b2c3d","index":1,"fingerprint":"` + vpnconfig.ServerFingerprint(servers[1]) + `"}`

	if rec := post(t, deps, body); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(mon.checked) != 1 || !reflect.DeepEqual(mon.checked[0], endpoint.Keys(servers[1])) {
		t.Fatalf("checked %v", mon.checked)
	}
	if rec := post(t, deps, `{}`); rec.Code != http.StatusOK || len(mon.checked[1]) != 0 {
		t.Fatalf("check all: %d, keys %v", rec.Code, mon.checked)
	}
}

// Review focus: the list changed between the page load and the tap - the
// fingerprint at that index is another server's - and nothing is checked.
func TestHandleMonitorCheck_AChangedListIs409(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	for _, body := range []string{
		`{"subscription":"0a1b2c3d","index":0,"fingerprint":"` + vpnconfig.ServerFingerprint(servers[1]) + `"}`,
		`{"subscription":"0a1b2c3d","index":5,"fingerprint":"x"}`,
		`{"subscription":"ffffffff","index":0,"fingerprint":"x"}`,
	} {
		if rec := post(t, deps, body); rec.Code != http.StatusConflict {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if len(mon.checked) != 0 {
		t.Fatalf("checked %v", mon.checked)
	}
}

func TestHandleMonitorCheck_AStoppedMonitorIs409AndNoDaemon503(t *testing.T) {
	deps, _ := monitorDeps(t, &fakeMonitor{checkErr: watchdapi.ErrNotActive})
	if rec := post(t, deps, `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("stopped: %d %s", rec.Code, rec.Body)
	}
	deps, _ = monitorDeps(t, &fakeMonitor{checkErr: errors.New("dial unix: no such file")})
	if rec := post(t, deps, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no daemon: %d %s", rec.Code, rec.Body)
	}
	deps, _ = monitorDeps(t, nil)
	if rec := post(t, deps, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client: %d %s", rec.Code, rec.Body)
	}
}

// The Servers tab matches its rows with the monitor's by fingerprint.
func TestHandleListServers_CarriesTheFingerprint(t *testing.T) {
	deps, servers := monitorDeps(t, nil)

	rec := httptest.NewRecorder()
	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))

	var resp struct {
		Subscriptions []struct {
			Servers []struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"servers"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Subscriptions[0].Servers[1].Fingerprint; got != vpnconfig.ServerFingerprint(servers[1]) {
		t.Fatalf("fingerprint %q", got)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/vpnconfig/ ./internal/webapi/ -count=1`
Expected: FAIL - `undefined: ServerFingerprint`, `undefined: handleMonitor`, `deps.Monitor undefined`.

- [ ] **Step 3: Move the fingerprint to `vpnconfig`**

In `server/internal/vpnconfig/vpnconfig.go`, add `"crypto/sha256"`, `"encoding/hex"` and `"fmt"` to the imports and insert above the comment of `ServerIPs`:

```go
// ServerFingerprint names a server in a list a client holds - a button of
// /xray, a row of the Web UI: the first 8 hex digits of
// sha256("subscription|name|address|port"). The list can change between the
// moment it is shown and a tap on it - the subscription watch rotates
// endpoints, a refresh replaces a list - and the index alone then names
// another server.
func ServerFingerprint(s Server) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", s.Subscription, s.Name, s.Address, s.Port)))
	return hex.EncodeToString(sum[:4])
}
```

In `server/internal/handler/xray.go`, delete `serverFingerprint` with its comment, remove the imports `"crypto/sha256"` and `"encoding/hex"`, and replace its two calls - `serverFingerprint(s)` in `xrayServersPage` and `serverFingerprint(server)` in `selectServer` - with `vpnconfig.ServerFingerprint(s)` and `vpnconfig.ServerFingerprint(server)`. In `server/internal/handler/xray_test.go`, replace the three `serverFingerprint(` calls with `vpnconfig.ServerFingerprint(`.

- [ ] **Step 4: Write the routes**

Create `server/internal/webapi/handler_monitor.go`:

```go
package webapi

import (
	"errors"
	"net/http"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// monitorServer is one server's status as the Servers tab shows it, matched
// with its row of GET /api/servers by index and fingerprint.
type monitorServer struct {
	Index       int    `json:"index"`
	Fingerprint string `json:"fingerprint"`
	watchdapi.ServerHealth
}

// monitorSubscription is one subscription's servers with how many are alive.
type monitorSubscription struct {
	ID      string          `json:"id"`
	Alive   int             `json:"alive"`
	Total   int             `json:"total"`
	Servers []monitorServer `json:"servers"`
}

type monitorResponse struct {
	State           watchdapi.State       `json:"state"`
	Message         string                `json:"message,omitempty"`
	IntervalSeconds int                   `json:"interval_seconds"`
	LagSeconds      int                   `json:"lag_seconds"`
	Subscriptions   []monitorSubscription `json:"subscriptions"`
}

// handleMonitor answers the status of every server: the daemon's endpoint
// states folded per server (watchdapi.Health). A daemon that does not answer
// is state not_running, every server unknown.
func handleMonitor(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		snap := watchdapi.Snapshot{State: watchdapi.StateNotRunning}
		if deps.Monitor != nil {
			if s, err := deps.Monitor.Monitor(r.Context()); err == nil {
				snap = s
			}
		}
		resp := monitorResponse{
			State: snap.State, Message: snap.Message,
			IntervalSeconds: snap.IntervalSeconds, LagSeconds: snap.LagSeconds,
			Subscriptions: make([]monitorSubscription, 0, len(subs)),
		}
		for _, sub := range subs {
			ms := monitorSubscription{ID: sub.ID, Total: len(sub.Servers), Servers: make([]monitorServer, 0, len(sub.Servers))}
			for i, s := range sub.Servers {
				s.Subscription = sub.ID
				h := watchdapi.Health(endpoint.Keys(s), snap)
				if h.Status == watchdapi.StatusAlive {
					ms.Alive++
				}
				ms.Servers = append(ms.Servers, monitorServer{Index: i, Fingerprint: vpnconfig.ServerFingerprint(s), ServerHealth: h})
			}
			resp.Subscriptions = append(resp.Subscriptions, ms)
		}
		jsonOK(w, resp)
	}
}

// monitorCheckRequest names one server as the page showed it, or nothing for
// every server.
type monitorCheckRequest struct {
	Subscription string `json:"subscription"`
	Index        *int   `json:"index"`
	Fingerprint  string `json:"fingerprint"`
}

// handleMonitorCheck queues a check of one server - every address of it - or
// of every server. 409 when the list changed since the page loaded it, or the
// monitor is stopped or disabled; 503 when the daemon does not answer.
func handleMonitorCheck(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req monitorCheckRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var keys []string
		if req.Index != nil {
			subs, err := deps.Config.LoadSubscriptions()
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to load servers")
				return
			}
			si := vpnconfig.FindSubscription(subs, req.Subscription)
			if si < 0 || *req.Index < 0 || *req.Index >= len(subs[si].Servers) {
				jsonError(w, http.StatusConflict, "server list changed")
				return
			}
			s := subs[si].Servers[*req.Index]
			s.Subscription = subs[si].ID
			if vpnconfig.ServerFingerprint(s) != req.Fingerprint {
				jsonError(w, http.StatusConflict, "server list changed")
				return
			}
			keys = endpoint.Keys(s)
		}
		if deps.Monitor == nil {
			jsonError(w, http.StatusServiceUnavailable, "the server monitor is not running")
			return
		}
		n, err := deps.Monitor.Check(r.Context(), keys)
		switch {
		case errors.Is(err, watchdapi.ErrNotActive):
			jsonError(w, http.StatusConflict, "the server monitor is stopped or disabled")
		case err != nil:
			jsonError(w, http.StatusServiceUnavailable, "the server monitor is not running")
		default:
			jsonOK(w, map[string]int{"queued": n})
		}
	}
}
```

In `server/internal/webapi/handler_servers.go`, replace `serverView`:

```go
type serverView struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	Port     int      `json:"port"`
	IPs      []string `json:"ips"`
	Protocol string   `json:"protocol"`
}
```

with:

```go
type serverView struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	Port     int      `json:"port"`
	IPs      []string `json:"ips"`
	Protocol string   `json:"protocol"`
	// Fingerprint matches the server with its row of GET /api/monitor
	// (vpnconfig.ServerFingerprint).
	Fingerprint string `json:"fingerprint"`
}
```

and in `handleListServers` replace:

```go
				views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips, Protocol: s.Label()})
```

with:

```go
				s.Subscription = sub.ID
				views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips,
					Protocol: s.Label(), Fingerprint: vpnconfig.ServerFingerprint(s)})
```

In `server/internal/webapi/router.go`, add the import `"github.com/zinin/vpn-director/server/internal/watchdapi"`, add to `Deps` after `Update`:

```go
	Monitor      watchdapi.API     // vpn-director-watchd's server monitor; nil reads as not running
```

and register the routes after `mux.HandleFunc("DELETE /api/subscriptions", handleDeleteSubscription(deps))`:

```go

	// Server monitor (vpn-director-watchd)
	mux.HandleFunc("GET /api/monitor", handleMonitor(deps))
	mux.HandleFunc("POST /api/monitor/check", handleMonitorCheck(deps))
```

- [ ] **Step 5: Wire the client and the log into the daemon**

In `server/cmd/webui/main.go`, add the import `"github.com/zinin/vpn-director/server/internal/watchdapi"`, and replace:

```go
		LogPaths: map[string]string{
			"bot":   p.BotLogPath,
			"vpn":   p.VPNLogPath,
			"xray":  p.XrayLogPath,
			"webui": p.WebUILogPath,
		},
		Update:  updateFlow,
```

with:

```go
		LogPaths: map[string]string{
			"bot":    p.BotLogPath,
			"vpn":    p.VPNLogPath,
			"xray":   p.XrayLogPath,
			"webui":  p.WebUILogPath,
			"watchd": p.WatchdLogPath,
		},
		Update:  updateFlow,
		Monitor: watchdapi.NewClient(p.WatchdSocket),
```

- [ ] **Step 6: Run the tests**

Run: `cd server && gofmt -w internal/webapi cmd/webui internal/vpnconfig internal/handler && gofmt -l internal/webapi cmd/webui internal/vpnconfig internal/handler && go vet ./internal/webapi/ ./cmd/webui/ ./internal/vpnconfig/ ./internal/handler/ && go test ./internal/webapi/ ./cmd/webui/ ./internal/vpnconfig/ ./internal/handler/ -count=1`
Expected: `gofmt -l` prints nothing; `ok` for every package.

- [ ] **Step 7: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go \
  server/internal/handler/xray.go server/internal/handler/xray_test.go \
  server/internal/webapi/handler_monitor.go server/internal/webapi/handler_monitor_test.go \
  server/internal/webapi/handler_servers.go server/internal/webapi/router.go server/cmd/webui/main.go
git commit -m "feat(webapi): the server monitor's routes

GET /api/monitor folds the daemon's endpoint states per server and names each server by index and by the fingerprint /xray buttons carry, which moves to vpnconfig; GET /api/servers carries the same fingerprint, so the page can tell its two lists apart. POST /api/monitor/check queues one server - every address of it - or all, and answers 409 when the list changed since the page loaded it. A daemon that does not answer is not_running, not an error. The Logs tab gains the monitor's log."
```

### Task 13: The Servers tab shows each server's status

The page polls `GET /api/monitor` every 15 s while the Servers tab is open. It shows a Health column (`● 142 ms` green, `● down` red, `● —` grey, `rejected`), a tooltip with when and why, a `↻` per server, `45/62 alive` beside each subscription, and a line above the table with the monitor's state and **Check all now**. A row's status is taken only when the monitor's fingerprint at that index is the page's own; when the two lists disagree - a refresh moved the list - the page loads the list again. The UI stays in English.

There is no unit-test runner for the SPA: `npm run build` runs `vue-tsc` (the type check) and the Vite build, and the page is checked by hand in dev mode.

**Files:**
- Modify: `web/src/types.ts`, `web/src/api.ts`, `web/src/style.css`, `web/src/components/LogsTab.vue`
- Modify (whole file given): `web/src/components/ServersTab.vue`

**Interfaces:**
- Consumes: `GET /api/monitor`, `POST /api/monitor/check`, the `fingerprint` of `GET /api/servers` (Task 12); the log source `watchd` (Task 12).
- Produces: `api.getMonitor()`, `api.checkServer(subscription, index, fingerprint)`, `api.checkAllServers()`; the types `HealthStatus`, `ServerHealth`, `MonitorSubscription`, `MonitorState`, `MonitorResponse`; `Server.fingerprint`.

- [ ] **Step 1: Add the types**

In `web/src/types.ts`, replace:

```ts
/** A server as GET /api/servers shows it: no credentials. protocol is its
 *  label, e.g. "vless·reality", "trojan·tls", "ss", "hysteria2". */
export interface Server {
  name: string
  address: string
  port: number
  ips: string[]
  protocol: string
}
```

with:

```ts
/** A server as GET /api/servers shows it: no credentials. protocol is its
 *  label, e.g. "vless·reality", "trojan·tls", "ss", "hysteria2". fingerprint
 *  matches it with its row of GET /api/monitor. */
export interface Server {
  name: string
  address: string
  port: number
  ips: string[]
  protocol: string
  fingerprint: string
}

/** What the server monitor (vpn-director-watchd) knows of a server. */
export type HealthStatus = 'alive' | 'dead' | 'unknown' | 'rejected'

/** One server's status in GET /api/monitor; index and fingerprint match it
 *  with its row of GET /api/servers. Times are RFC 3339; Go writes a zero time
 *  as 0001-01-01T00:00:00Z. */
export interface ServerHealth {
  index: number
  fingerprint: string
  status: HealthStatus
  latency_ms: number
  checked_at: string
  since: string
  next_at: string
  error?: string
}

export interface MonitorSubscription {
  id: string
  alive: number
  total: number
  servers: ServerHealth[] | null
}

export type MonitorState =
  | 'ok'
  | 'stopped'
  | 'disabled'
  | 'no_xray'
  | 'wan_down'
  | 'prober_error'
  | 'not_running'

export interface MonitorResponse {
  state: MonitorState
  message?: string
  interval_seconds: number
  lag_seconds: number
  subscriptions: MonitorSubscription[] | null
}
```

- [ ] **Step 2: Add the calls**

In `web/src/api.ts`, add `MonitorResponse,` to the type imports right before `ServersResponse,`, and replace:

```ts
  deleteSubscription: (id: string) =>
    api.delete<DeleteSubscriptionResponse>('/api/subscriptions', { params: { id } }),
```

with:

```ts
  deleteSubscription: (id: string) =>
    api.delete<DeleteSubscriptionResponse>('/api/subscriptions', { params: { id } }),

  // Server monitor
  getMonitor: () =>
    api.get<MonitorResponse>('/api/monitor'),
  // One server as the page shows it - the router answers 409 when the list
  // changed since - or, with no server, every server.
  checkServer: (subscription: string, index: number, fingerprint: string) =>
    api.post<{ queued: number }>('/api/monitor/check', { subscription, index, fingerprint }),
  checkAllServers: () =>
    api.post<{ queued: number }>('/api/monitor/check', {}),
```

- [ ] **Step 3: Add the colours and the log source**

In `web/src/style.css`, insert after the `.badge-grey` rule:

```css
/* Server monitor */
.health-alive {
  color: #51cf66;
}

.health-dead {
  color: #ff6b6b;
}

.health-unknown {
  color: #999;
}

.health-rejected {
  color: #f0a020;
}
```

In `web/src/components/LogsTab.vue`, replace `const logSources = ['vpn', 'xray', 'webui', 'bot'] as const` with `const logSources = ['vpn', 'xray', 'webui', 'bot', 'watchd'] as const`.

- [ ] **Step 4: Rewrite the Servers tab**

Replace the whole of `web/src/components/ServersTab.vue` with the version below. Against the current file it adds: the imports of `computed`, `onUnmounted` and the monitor types; the refs `monitor` and `checking` and the `poll` timer; the functions `until`, `loadMonitor`, `listChanged`, `healthOf`, `healthText`, `healthTitle`, `aliveText`, `sendCheck`, `checkServer`, `checkAll` and the computed `monitorLine`, `canCheck`; the polling in `onMounted`/`onUnmounted`; and in the Servers card the monitor line, the `alive` count in each summary and the Health column. Everything else stays as it is.

```vue
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import api from '../api'
import type {
  ActiveServer,
  MonitorResponse,
  Server,
  ServerHealth,
  Subscription,
  SubscriptionServers,
} from '../types'

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
// The server monitor's answer, polled while the tab is open.
const monitor = ref<MonitorResponse | null>(null)
// The check being sent: 'all' or '<subscription>:<index>'.
const checking = ref('')
let poll: ReturnType<typeof setInterval> | undefined

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

// "2 h ago" for a time the router wrote in UTC, and "never" for one before
// 2000: a file made by hand can lack the time, and Go answers its zero time,
// 0001-01-01, for it.
function ago(iso: string): string {
  const t = Date.parse(iso)
  if (isNaN(t)) return ''
  if (t < Date.UTC(2000, 0, 1)) return 'never'
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

// "in 40 s", "in 5 min" or "now" for a time the router wrote; nothing for
// Go's zero time.
function until(iso: string): string {
  const t = Date.parse(iso)
  if (isNaN(t) || t < Date.UTC(2000, 0, 1)) return ''
  const s = Math.round((t - Date.now()) / 1000)
  if (s <= 0) return 'now'
  if (s < 60) return `in ${s} s`
  return `in ${Math.round(s / 60)} min`
}

async function loadMonitor() {
  try {
    const resp = await api.getMonitor()
    monitor.value = resp.data
    if (listChanged()) await load()
  } catch {
    // A missed poll waits for the next; the page's own load reports errors.
  }
}

// The monitor's rows go by index; a count or a fingerprint that differs from
// the page's means a refresh moved the list since the page loaded it.
function listChanged(): boolean {
  const subs = monitor.value?.subscriptions ?? []
  if (subs.length !== groups.value.length) return true
  for (const group of groups.value) {
    const rows = subs.find((s) => s.id === group.id)?.servers ?? []
    const servers = group.servers ?? []
    if (rows.length !== servers.length) return true
    if (servers.some((server, i) => rows[i].fingerprint !== server.fingerprint)) return true
  }
  return false
}

// The status of a server as the page shows it, never another server's.
function healthOf(group: SubscriptionServers, idx: number, server: Server): ServerHealth | null {
  const row = monitor.value?.subscriptions?.find((s) => s.id === group.id)?.servers?.[idx]
  return row && row.fingerprint === server.fingerprint ? row : null
}

function healthText(h: ServerHealth | null): string {
  switch (h?.status) {
    case 'alive':
      return `● ${h.latency_ms} ms`
    case 'dead':
      return '● down'
    case 'rejected':
      return 'rejected'
    default:
      return '● —'
  }
}

function healthTitle(h: ServerHealth | null): string {
  if (!h || h.status === 'unknown') return 'Not checked yet'
  if (h.status === 'rejected') return h.error || 'Xray refused this server'
  const lines = [`Checked ${ago(h.checked_at)}`, `${h.status === 'alive' ? 'Alive' : 'Down'} since ${ago(h.since)}`]
  if (h.error) lines.push(`Error: ${h.error}`)
  const next = until(h.next_at)
  if (next) lines.push(`Next check ${next}`)
  return lines.join('\n')
}

function aliveText(groupId: string): string {
  const m = monitor.value
  if (!m || m.state === 'not_running') return ''
  const sub = m.subscriptions?.find((s) => s.id === groupId)
  return sub ? `${sub.alive}/${sub.total} alive` : ''
}

const monitorLine = computed(() => {
  const m = monitor.value
  if (!m) return ''
  switch (m.state) {
    case 'ok': {
      if (m.lag_seconds > m.interval_seconds) return 'Monitoring: checks are falling behind'
      const s = m.interval_seconds
      return `Monitoring every ${s % 60 === 0 ? `${s / 60} min` : `${s} s`}`
    }
    case 'stopped':
      return 'Monitoring: stopped with VPN Director'
    case 'disabled':
      return 'Monitoring: disabled in settings'
    case 'no_xray':
      return 'Monitoring: xray not found'
    case 'wan_down':
      return 'Monitoring: WAN down, statuses kept'
    case 'prober_error':
      return `Monitoring: the prober does not start: ${m.message ?? ''}`
    default:
      return 'Monitoring: not running'
  }
})

const canCheck = computed(() => monitor.value?.state === 'ok' || monitor.value?.state === 'wan_down')

// A check answers within seconds: the page looks again before the next poll.
async function sendCheck(what: string, fn: () => Promise<unknown>) {
  checking.value = what
  try {
    await fn()
    setTimeout(loadMonitor, 3000)
  } catch (e: any) {
    if (e.response?.status === 409 && e.response?.data?.error === 'server list changed') {
      await load()
    } else {
      alert('Error: ' + errorText(e))
    }
  } finally {
    checking.value = ''
  }
}

function checkServer(group: SubscriptionServers, idx: number, server: Server) {
  return sendCheck(`${group.id}:${idx}`, () => api.checkServer(group.id, idx, server.fingerprint))
}

function checkAll() {
  return sendCheck('all', () => api.checkAllServers())
}

async function run(what: string, fn: () => Promise<void>) {
  busy.value = what
  try {
    await fn()
  } catch (e: any) {
    alert('Error: ' + errorText(e))
    // A failure can still have changed the list: "saved, but" and "deleted,
    // but" answers, or a 404 for a subscription deleted meanwhile.
    await load()
  } finally {
    busy.value = ''
  }
}

function addSubscription() {
  if (!addUrl.value.trim()) return
  return run('add', async () => {
    summaries.value = []
    const resp = await api.addSubscription(addUrl.value.trim(), addName.value.trim())
    summaries.value = resp.data.existed
      ? [resp.data.summary, 'The link was saved already; its list was refreshed.']
      : [resp.data.summary]
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
  // Enter reaches here while the ✓ button is disabled.
  if (busy.value) return
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

onMounted(() => {
  load()
  loadMonitor()
  poll = setInterval(loadMonitor, 15000)
})

onUnmounted(() => clearInterval(poll))
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
    <p v-else-if="!loading && !error" style="color: #999; font-size: 0.875rem;">
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
    <p v-if="monitorLine" style="font-size: 0.875rem; margin: 0 0 0.75rem;">
      {{ monitorLine }}
      <button
        v-if="canCheck"
        class="btn btn-blue"
        style="margin-left: 0.5rem;"
        :disabled="checking !== ''"
        @click="checkAll"
      >
        {{ checking === 'all' ? '...' : 'Check all now' }}
      </button>
    </p>
    <details
      v-for="group in groups"
      :key="group.id"
      :open="runsFrom(group.id) || groups.length === 1"
      style="margin-bottom: 0.75rem;"
    >
      <summary style="cursor: pointer; font-weight: 600;">
        {{ group.name }} — {{ (group.servers ?? []).length }} servers<template v-if="aliveText(group.id)">, {{ aliveText(group.id) }}</template>
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
            <th>Health</th>
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
            <td
              :class="'health-' + (healthOf(group, idx, server)?.status ?? 'unknown')"
              :title="healthTitle(healthOf(group, idx, server))"
              style="white-space: nowrap;"
            >
              {{ healthText(healthOf(group, idx, server)) }}
              <button
                v-if="canCheck"
                class="btn btn-blue"
                style="padding: 0 0.4rem; margin-left: 0.4rem;"
                :disabled="checking !== ''"
                title="Check now"
                @click="checkServer(group, idx, server)"
              >
                {{ checking === `${group.id}:${idx}` ? '...' : '↻' }}
              </button>
            </td>
            <td>
              <button class="btn btn-green" :disabled="!!busy" @click="selectServer(group, idx)">
                {{ busy === `select:${group.id}:${idx}` ? '...' : 'Select' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </details>
    <p v-if="groups.length === 0 && !loading && !error" style="color: #999; font-size: 0.875rem;">
      No servers found. Add a subscription to get started.
    </p>
  </div>
</template>
```

- [ ] **Step 5: Type-check and build**

Run: `cd web && npm run build`
Expected: `vue-tsc` reports nothing and Vite prints `✓ built`.

- [ ] **Step 6: Look at it in dev mode**

Build the embedded SPA and run the three pieces from `server/` (the monitor from Task 9 with the fixture subscriptions, as in its Step 6):

```bash
make web-embed
cd server
[ -f testdata/dev/vpn-director.json ] || echo '{"data_dir":"data","xray":{},"tunnel_director":{"tunnels":{}}}' > testdata/dev/vpn-director.json
mkdir -p testdata/dev/data/subscriptions && cp ../testdata/substore/*.json testdata/dev/data/subscriptions/
go run ./cmd/watchd --dev &
go run ./cmd/webui --dev
```

Open `http://localhost:8444`, log in as admin/admin, open **Servers**. Expected: the line `Monitoring every 1 min [Check all now]`; `Alpha — 2 servers, 2/2 alive`, `Beta — 1 servers, 1/1 alive`, `Gamma — 1 servers, 0/1 alive`; green `● <n> ms` for Oslo and both Germany-1, red `● down` for Riga, with a tooltip `Checked …`, `Down since …`, `Error: connection closed`, `Next check in …`; a `↻` that turns to `...` and back. Stop the monitor (`kill %1`): within 15 s the line reads `Monitoring: not running` and every server shows `● —`. Afterwards: `rm -rf testdata/dev/data/subscriptions`.

- [ ] **Step 7: Commit**

```bash
git add web/src/types.ts web/src/api.ts web/src/style.css web/src/components/LogsTab.vue web/src/components/ServersTab.vue
git commit -m "feat(web): each server's status on the Servers tab

The Servers tab polls the server monitor every 15 s: a Health column with the latency of a live server, a tooltip with when it was checked and why it is down, a check button per server, the live count of each subscription and the monitor's state with Check all now. A status is shown only on the server whose fingerprint it carries; when the two lists disagree the page loads the list again. The Logs tab gains the monitor's log."
```

### Task 14: The bot marks `/servers` and `/xray`

With the daemon answering, `/servers` starts each line with 🟢 (and the latency), 🔴, ⚪ or ⛔ and heads each subscription with `<alive>/<total> живы`; `/xray` labels a subscription `Alpha (45/62)` and a server `🟢 3. Oslo · 142 ms`. A daemon that does not answer - the client's bound is 2 s - leaves the marks out and the page says «мониторинг не запущен»; one that answers but does not check says why under the title. `/logs` gains the source `watchd`, and `/logs` alone reads it too.

**Files:**
- Create: `server/internal/handler/health.go`
- Test: `server/internal/handler/health_test.go`
- Modify: `server/internal/handler/handler.go` (`Deps.Monitor`)
- Modify: `server/internal/handler/servers.go`, `server/internal/handler/xray.go`, `server/internal/handler/misc.go`
- Test: `server/internal/handler/servers_test.go`, `xray_test.go`, `misc_test.go`
- Modify: `server/internal/bot/bot.go` (the client)

**Interfaces:**
- Consumes: `watchdapi.API`, `Health`, `NewClient`, the states (Task 2); `endpoint.Keys` (Task 1); `paths.WatchdSocket`, `paths.WatchdLogPath` (Task 9).
- Produces:
  - `handler.Deps.Monitor watchdapi.API` (nil: no marks).
  - In package `handler`: `type health`, `monitorHealth(api watchdapi.API, subs []vpnconfig.Subscription) health`, `(health) of(sub string, index int) (watchdapi.ServerHealth, bool)`, `(health) header() string`, `mark(watchdapi.ServerHealth) string`, `latency(watchdapi.ServerHealth) string`.
  - New signatures: `buildServersPage(lines []serverLine, subCount, page int, h health)`, `xraySubscriptions(subs, active, h health)`, `xrayServersPage(sub, page, back, active, h health)`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/handler/health_test.go`:

```go
// internal/handler/health_test.go
package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeMonitor is vpn-director-watchd for the handler tests.
type fakeMonitor struct {
	snap watchdapi.Snapshot
	err  error
}

func (f *fakeMonitor) Monitor(context.Context) (watchdapi.Snapshot, error) { return f.snap, f.err }
func (f *fakeMonitor) Check(context.Context, []string) (int, error)        { return 0, nil }

// monitored is a subscription of two servers, S1 alive at 142 ms and S2 dead,
// and the monitor that says so.
func monitored() ([]vpnconfig.Subscription, *fakeMonitor) {
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(2)}
	keys := func(i int) string {
		s := sub.Servers[i]
		s.Subscription = sub.ID
		return endpoint.Keys(s)[0]
	}
	return []vpnconfig.Subscription{sub}, &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		keys(0): {Status: watchdapi.StatusAlive, LatencyMS: 142},
		keys(1): {Status: watchdapi.StatusDead, Error: "timeout"},
	}}}
}

func TestBuildServersPage_MarksEachServerWithItsStatus(t *testing.T) {
	subs, mon := monitored()

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(mon, subs))

	for _, want := range []string{"*Alpha* — 1/2 живы", "🟢 1\\. S1", "· 142 ms", "🔴 2\\. S2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("page lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "мониторинг") {
		t.Fatalf("a monitor that checks got a note:\n%s", text)
	}
}

func TestBuildServersPage_ASilentDaemonLeavesTheMarksOut(t *testing.T) {
	subs, _ := monitored()

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(&fakeMonitor{err: errors.New("no socket")}, subs))

	if !strings.Contains(text, "мониторинг не запущен") || strings.Contains(text, "🟢") || strings.Contains(text, "живы") {
		t.Fatalf("page:\n%s", text)
	}
}

// A monitor that is not checking says why: its statuses may be old.
func TestBuildServersPage_AStoppedMonitorSaysSo(t *testing.T) {
	subs, mon := monitored()
	mon.snap.State = watchdapi.StateStopped

	text, _ := buildServersPage(serverLines(subs), len(subs), 0, monitorHealth(mon, subs))

	if !strings.Contains(text, "мониторинг приостановлен") {
		t.Fatalf("page:\n%s", text)
	}
}

func TestXray_ButtonsCarryTheStatus(t *testing.T) {
	subs, mon := monitored()
	h := monitorHealth(mon, subs)

	_, kb := xraySubscriptions(append(subs, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta", Servers: servers(1)}), nil, h)
	if got := kb.InlineKeyboard[0][0].Text; got != "Alpha (1/2)" {
		t.Fatalf("subscription button %q", got)
	}
	_, kb = xrayServersPage(subs[0], 0, false, nil, h)
	if a, b := kb.InlineKeyboard[0][0].Text, kb.InlineKeyboard[0][1].Text; a != "🟢 1. S1 · 142 ms" || b != "🔴 2. S2" {
		t.Fatalf("server buttons %q, %q", a, b)
	}
}
```

The existing tests call the old signatures. In `server/internal/handler/servers_test.go`, replace `buildServersPage(serverLines(subs), len(subs), 0)` with `buildServersPage(serverLines(subs), len(subs), 0, health{})` and `buildServersPage(serverLines(subs), len(subs), 1)` with `buildServersPage(serverLines(subs), len(subs), 1, health{})`. In `server/internal/handler/xray_test.go`, add a last argument `health{}` to the four `xrayServersPage(` calls: `xrayServersPage(sub, 0, true, nil, health{})`, `xrayServersPage(sub, 2, false, nil, health{})`, `xrayServersPage(subs[0], 0, true, tc.active, health{})`, `xrayServersPage(subs[1], 0, true, tc.active, health{})`.

In `server/internal/handler/misc_test.go`, `/logs` alone now reads five logs. In `TestMiscHandler_HandleLogs_DefaultArgs` replace its `testPaths`:

```go
	testPaths := paths.Paths{
		BotLogPath:   "/tmp/bot.log",
		VPNLogPath:   "/tmp/vpn.log",
		XrayLogPath:  "/tmp/xray-error.log",
		WebUILogPath: "/tmp/webui.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs",
```

with:

```go
	testPaths := paths.Paths{
		BotLogPath:    "/tmp/bot.log",
		VPNLogPath:    "/tmp/vpn.log",
		XrayLogPath:   "/tmp/xray-error.log",
		WebUILogPath:  "/tmp/webui.log",
		WatchdLogPath: "/tmp/vpn-director-watchd.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs",
```

replace:

```go
	// Default is "all" which reads bot, vpn, xray and webui logs
	if len(logReader.calls) != 4 {
		t.Fatalf("expected 4 log read calls, got %d", len(logReader.calls))
	}
```

with:

```go
	// Default is "all" which reads bot, vpn, xray, webui and watchd logs
	if len(logReader.calls) != 5 {
		t.Fatalf("expected 5 log read calls, got %d", len(logReader.calls))
	}
```

and after its check of the fourth call (`/tmp/webui.log`) add:

```go
	// Check fifth call (the server monitor's log)
	if logReader.calls[4].path != "/tmp/vpn-director-watchd.log" {
		t.Errorf("expected watchd log path, got %q", logReader.calls[4].path)
	}
```

In `TestMiscHandler_HandleLogs_LinesOnly` replace:

```go
	// When only a number is given, source defaults to "all"
	if len(logReader.calls) != 4 {
		t.Fatalf("expected 4 log read calls, got %d", len(logReader.calls))
	}
```

with:

```go
	// When only a number is given, source defaults to "all"
	if len(logReader.calls) != 5 {
		t.Fatalf("expected 5 log read calls, got %d", len(logReader.calls))
	}
```

Append to `server/internal/handler/misc_test.go`:

```go
func TestMiscHandler_HandleLogs_SourceWatchd(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: paths.Paths{WatchdLogPath: "/tmp/vpn-director-watchd.log"}}
	h := NewMiscHandler(deps)

	h.HandleLogs(&tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 100},
		Text:     "/logs watchd",
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}},
	})

	if len(logReader.calls) != 1 || logReader.calls[0].path != "/tmp/vpn-director-watchd.log" {
		t.Fatalf("log reads %+v, want the monitor's log", logReader.calls)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/handler/ -count=1`
Expected: FAIL - `undefined: monitorHealth`, `undefined: health`, `too many arguments in call to buildServersPage`, and more.

- [ ] **Step 3: Write the health helper and the field**

Create `server/internal/handler/health.go`:

```go
// internal/handler/health.go
package handler

import (
	"context"
	"fmt"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// health is what vpn-director-watchd says of every server, by subscription
// id and index. ok is false when it said nothing: no marks then.
type health struct {
	ok      bool
	note    string // the monitor's state when it is not ok, for the header
	servers map[string][]watchdapi.ServerHealth
	alive   map[string]int
}

// monitorHealth asks the daemon about subs; one that does not answer gives no
// marks.
func monitorHealth(api watchdapi.API, subs []vpnconfig.Subscription) health {
	if api == nil {
		return health{}
	}
	snap, err := api.Monitor(context.Background())
	if err != nil {
		return health{}
	}
	h := health{ok: true, note: stateNote(snap.State), servers: map[string][]watchdapi.ServerHealth{}, alive: map[string]int{}}
	for _, sub := range subs {
		list := make([]watchdapi.ServerHealth, len(sub.Servers))
		for i, s := range sub.Servers {
			s.Subscription = sub.ID
			list[i] = watchdapi.Health(endpoint.Keys(s), snap)
			if list[i].Status == watchdapi.StatusAlive {
				h.alive[sub.ID]++
			}
		}
		h.servers[sub.ID] = list
	}
	return h
}

// of is the status of the server at index in subscription sub.
func (h health) of(sub string, index int) (watchdapi.ServerHealth, bool) {
	list := h.servers[sub]
	if !h.ok || index < 0 || index >= len(list) {
		return watchdapi.ServerHealth{}, false
	}
	return list[index], true
}

// header is the line under a page's title: that the monitor is not running,
// or what it is doing when that is not checking.
func (h health) header() string {
	if !h.ok {
		return "мониторинг не запущен"
	}
	return h.note
}

func stateNote(s watchdapi.State) string {
	switch s {
	case watchdapi.StateStopped:
		return "мониторинг приостановлен: VPN Director остановлен"
	case watchdapi.StateDisabled:
		return "мониторинг выключен в настройках"
	case watchdapi.StateNoXray:
		return "мониторинг: xray не найден"
	case watchdapi.StateWANDown:
		return "мониторинг: WAN недоступен, статусы сохранены"
	case watchdapi.StateProberError:
		return "мониторинг: пробный Xray не запускается"
	}
	return ""
}

// mark is a server's status as a line or a button starts with it.
func mark(s watchdapi.ServerHealth) string {
	switch s.Status {
	case watchdapi.StatusAlive:
		return "🟢"
	case watchdapi.StatusDead:
		return "🔴"
	case watchdapi.StatusRejected:
		return "⛔"
	default:
		return "⚪"
	}
}

// latency is " · 142 ms" for a live server, nothing otherwise.
func latency(s watchdapi.ServerHealth) string {
	if s.Status != watchdapi.StatusAlive {
		return ""
	}
	return fmt.Sprintf(" · %d ms", s.LatencyMS)
}
```

In `server/internal/handler/handler.go`, add the import `"github.com/zinin/vpn-director/server/internal/watchdapi"` and, after the `TelegramPath` field of `Deps`, add:

```go
	// Monitor is vpn-director-watchd's server monitor; nil means no marks.
	Monitor watchdapi.API
```

- [ ] **Step 4: Mark `/servers`**

In `server/internal/handler/servers.go`, replace `serverLine` and `serverLines`:

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
```

with:

```go
// serverLine is one server on a /servers page: its subscription's name, id
// and size, and its number within that subscription.
type serverLine struct {
	sub    string
	subID  string
	total  int
	number int
	server vpnconfig.Server
}

// serverLines is every server of subs, in subscription order.
func serverLines(subs []vpnconfig.Subscription) []serverLine {
	var lines []serverLine
	for _, sub := range subs {
		for i, s := range sub.Servers {
			lines = append(lines, serverLine{sub: sub.Name, subID: sub.ID, total: len(sub.Servers), number: i + 1, server: s})
		}
	}
	return lines
}
```

In `HandleServers`, replace `text, keyboard := buildServersPage(lines, len(subs), 0)` with `text, keyboard := buildServersPage(lines, len(subs), 0, monitorHealth(h.deps.Monitor, subs))`; in `HandleCallback`, replace `text, keyboard := buildServersPage(lines, len(subs), page)` with `text, keyboard := buildServersPage(lines, len(subs), page, monitorHealth(h.deps.Monitor, subs))`.

Replace the comment and the signature of `buildServersPage`:

```go
// buildServersPage builds one page of the list, a header wherever a
// subscription starts and at the top of the page, with the navigation keyboard.
func buildServersPage(lines []serverLine, subCount, page int) (string, tgbotapi.InlineKeyboardMarkup) {
```

with:

```go
// buildServersPage builds one page of the list, a header wherever a
// subscription starts and at the top of the page, with the navigation keyboard.
// With the monitor's health each line starts with the server's status and each
// subscription's header counts its live servers.
func buildServersPage(lines []serverLine, subCount, page int, h health) (string, tgbotapi.InlineKeyboardMarkup) {
```

and replace its body's text part:

```go
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
```

with:

```go
	sb.WriteString(fmt.Sprintf("🖥 *Servers* \\(%d\\) in %d subscriptions, page %d/%d:\n",
		len(lines), subCount, page+1, totalPages))
	if note := h.header(); note != "" {
		sb.WriteString(telegram.EscapeMarkdownV2(note) + "\n")
	}

	for i := start; i < end; i++ {
		l := lines[i]
		if i == start || l.sub != lines[i-1].sub {
			sb.WriteString("\n*" + telegram.EscapeMarkdownV2(l.sub) + "*")
			if h.ok {
				sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf(" — %d/%d живы", h.alive[l.subID], l.total)))
			}
			sb.WriteString("\n")
		}
		s := l.server
		prefix, suffix := "", ""
		if hs, ok := h.of(l.subID, l.number-1); ok {
			prefix, suffix = mark(hs)+" ", telegram.EscapeMarkdownV2(latency(hs))
		}
		sb.WriteString(fmt.Sprintf("%s%d\\. %s — %s \\(%s\\) · %s%s\n",
			prefix,
			l.number,
			telegram.EscapeMarkdownV2(s.Name),
			telegram.EscapeMarkdownV2(s.Address),
			telegram.EscapeMarkdownV2(strings.Join(s.IPs, ", ")),
			telegram.EscapeMarkdownV2(s.Label()),
			suffix))
	}
```

- [ ] **Step 5: Mark `/xray`**

In `server/internal/handler/xray.go`, replace in `firstStep`:

```go
	active := h.active()
	if len(subs) == 1 {
		text, kb := xrayServersPage(subs[0], 0, false, active)
		return text, kb, nil
	}
	text, kb := xraySubscriptions(subs, active)
	return text, kb, nil
```

with:

```go
	active := h.active()
	health := monitorHealth(h.deps.Monitor, subs)
	if len(subs) == 1 {
		text, kb := xrayServersPage(subs[0], 0, false, active, health)
		return text, kb, nil
	}
	text, kb := xraySubscriptions(subs, active, health)
	return text, kb, nil
```

replace the head of `xraySubscriptions`:

```go
// xraySubscriptions is the first step: a button per subscription with its
// server count, the running server's subscription checked.
func xraySubscriptions(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		if len(s.Servers) == 0 {
			continue
		}
		label := fmt.Sprintf("%s (%d)", s.Name, len(s.Servers))
```

with:

```go
// xraySubscriptions is the first step: a button per subscription with its
// server count - its live servers of all, with the monitor's health - the
// running server's subscription checked.
func xraySubscriptions(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer, h health) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		if len(s.Servers) == 0 {
			continue
		}
		label := fmt.Sprintf("%s (%d)", s.Name, len(s.Servers))
		if h.ok {
			label = fmt.Sprintf("%s (%d/%d)", s.Name, h.alive[s.ID], len(s.Servers))
		}
```

replace the comment and signature of `xrayServersPage`:

```go
// xrayServersPage is one page of a subscription's servers, two a row, with ◀ ▶
// between pages and « Back to the subscriptions when back is set.
func xrayServersPage(sub vpnconfig.Subscription, page int, back bool, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
```

with:

```go
// xrayServersPage is one page of a subscription's servers, two a row, with ◀ ▶
// between pages and « Back to the subscriptions when back is set. With the
// monitor's health each button starts with the server's status.
func xrayServersPage(sub vpnconfig.Subscription, page int, back bool, active *vpnconfig.ActiveServer, h health) (string, tgbotapi.InlineKeyboardMarkup) {
```

and inside it replace:

```go
		label := fmt.Sprintf("%d. %s", i+1, s.Name)
		if active != nil
```

with:

```go
		label := fmt.Sprintf("%d. %s", i+1, s.Name)
		if hs, ok := h.of(sub.ID, i); ok {
			label = mark(hs) + " " + label + latency(hs)
		}
		if active != nil
```

In `HandleCallback`, replace `text, kb := xrayServersPage(subs[i], page, len(subs) > 1, h.active())` with `text, kb := xrayServersPage(subs[i], page, len(subs) > 1, h.active(), monitorHealth(h.deps.Monitor, subs))`.

- [ ] **Step 6: Add the log source**

In `server/internal/handler/misc.go`, replace `case "bot", "vpn", "xray", "webui", "all":` with `case "bot", "vpn", "xray", "webui", "watchd", "all":`, replace the usage text ``"Usage: `/logs [bot|vpn|xray|webui|all] [lines]`"`` with ``"Usage: `/logs [bot|vpn|xray|webui|watchd|all] [lines]`"``, and after the `webui` block add:

```go

	if source == "watchd" || source == "all" {
		h.sendLogFile(msg.Chat.ID, h.deps.Paths.WatchdLogPath, "Server monitor", lines)
	}
```

- [ ] **Step 7: Give the handlers the client**

In `server/internal/bot/bot.go`, add the import `"github.com/zinin/vpn-director/server/internal/watchdapi"` and, in the `handler.Deps` literal of `b.wire`, after `DevMode:     b.devMode,` add:

```go
			Monitor:     watchdapi.NewClient(p.WatchdSocket),
```

- [ ] **Step 8: Run the tests**

Run: `cd server && gofmt -l internal/handler internal/bot && go vet ./internal/handler/ ./internal/bot/ && go test ./internal/handler/ ./internal/bot/ -count=1 && go test -race ./internal/handler/ -count=1`
Expected: `gofmt -l` prints nothing; `ok` for every run.

- [ ] **Step 9: Commit**

```bash
git add server/internal/handler/health.go server/internal/handler/health_test.go server/internal/handler/handler.go \
  server/internal/handler/servers.go server/internal/handler/servers_test.go \
  server/internal/handler/xray.go server/internal/handler/xray_test.go \
  server/internal/handler/misc.go server/internal/handler/misc_test.go server/internal/bot/bot.go
git commit -m "feat(bot): the server monitor's marks in /servers and /xray

With vpn-director-watchd answering, /servers starts each line with its status and a live server's latency and heads each subscription with its live count, and /xray labels subscriptions and servers the same way. A daemon that does not answer within 2 s leaves the marks out and the page says so; one that does not check says why. /logs gains the monitor's log."
```

### Task 15: Documentation and the final check

**Files:**
- Create: `.claude/rules/watchd.md`
- Modify: `CLAUDE.md`, `.claude/rules/webui.md`, `.claude/rules/telegram-bot.md`, `README.md`, `README.ru.md`

- [ ] **Step 1: The rules file of the new daemon**

Create `.claude/rules/watchd.md`:

````markdown
---
paths: "server/cmd/watchd/**/*, server/internal/monitor/**/*, server/internal/watchdapi/**/*, server/internal/endpoint/**/*"
---

# Server monitor (vpn-director-watchd)

The daemon checks, minute by minute, whether every server of every subscription carries
traffic, and serves what it finds on a unix socket to the Web UI and the bot. Stage 1 of three:
stage 2 moves the subscription watch (`subwatch`) into this daemon, stage 3 makes its failover
use the monitor's data (`docs/superpowers/specs/2026-09-28-server-monitor-design.md`).

## Layout

```
server/cmd/watchd/main.go          # flags, logging, the readers of settings and endpoints, DI
server/internal/endpoint/          # PerAddress, ServerForDial, DialKey, Key, Keys, WANControls
server/internal/monitor/
├── settings.go                    # the monitor section, resolved (SettingsFrom)
├── endpoints.go                   # Build: the endpoint set, the generator's refusals
├── probeconfig.go                 # the prober's Xray config, accounts
├── check.go                       # one check through SOCKS5 (probeGet), classify
├── process*.go                    # the prober process: Pdeathsig, the tail of its stderr
├── xray.go                        # Launcher, Session, XrayLauncher, EnsureProbeBinary, KillLeftovers
├── fake.go                        # FakeLauncher for --dev
├── entry.go                       # one endpoint's record: succeed, fail, reject
├── monitor.go                     # the engine: refresh, dispatch, apply, the WAN guard, crashes
├── store.go                       # the state file
├── wan.go, stamp.go               # WANUp; Stamp, which spares a rebuild when no file changed
server/internal/watchdapi/         # the socket contract: types, Health, Serve, Client
```

## Endpoints

An endpoint is one address of one server as the walk dials it: `endpoint.PerAddress`, then
`endpoint.ServerForDial` (the IP in the outbound's address slot). Its key is the hex SHA-256 of
`endpoint.DialKey`; a record without an outbound hashes its flat VLESS fields. Names that share
an endpoint - one provider lists 62 names on 9 endpoints - share one key and one check. The Web
UI and the bot compute keys with `endpoint.Keys` and fold them with `watchdapi.Health`: alive
when any address is alive (the best latency), otherwise unknown when any is unchecked or unknown
to the daemon, otherwise dead, otherwise rejected. A key never carries a credential.

`monitor.Build` refuses what `Generate` refuses (`service.OutboundJSON`, which wraps
`serverOutbound`): such a server is `rejected` with the generator's reason and stays out of the
prober. The active server's endpoints come first at a rebuild.

## The prober

A second Xray process, run as `/opt/vpn-director/vpn-director-probe` - a hard link to the `xray`
on PATH (`EnsureProbeBinary`; a copy with xray's modification time where a hard link cannot
cross filesystems). Never under the name `xray` and never through a symbolic link: Entware's
`S24xray` goes through `rc.func`, whose `start` answers "already running" when `pidof xray` finds
any process, and BusyBox `pidof` also compares the resolved `/proc/PID/exe`. The prober's
command line contains no "xray" either, so the README's monit rule `matching "xray"` ignores it.

Its config (`/tmp/vpn-director/probe/config.json`, 0600): a `blackhole` first - Xray sends what
matches no rule to its first outbound, so nothing leaves the router directly - then one outbound
per endpoint, `m<i>`; one SOCKS inbound on 127.0.0.1 at a port free at start, with an account
`e<i>` per endpoint and a random password per start; a rule `user e<i> → m<i>`. Measured on Xray
26.3.27: 31 MB with one outbound, 34 MB with 600.

It runs while the monitor is active, restarts when the endpoint set changes (checks cut short
count for nothing) and dies with the daemon (`Pdeathsig` from a locked OS thread; at startup
`KillLeftovers`). Xray stops at the first outbound it cannot build and names only its tag: that
endpoint is rejected and the prober starts again, at most 20 times in a row; an error without a
tag is bisected with `xray run -test`. The kept reason drops the last segment of Xray's error
chain, which quotes the offending value. A prober that exits during checks is a crash: each
endpoint then under check is checked alone, and one that crashes its own prober is rejected
("crashes Xray") until its outbound changes; 5 crashes in 10 minutes back off like a failed
start (1, 2, then 5 minutes).

## A check

`http://www.gstatic.com/generate_204` through the endpoint's account: 204 within 10 s is alive,
the latency the time to the response headers. Plain HTTP where the watch probes HTTPS: a TLS
handshake with gstatic is 6.8 KB of a check's ~12 KB. A failure is retried once after 2 s.
Reasons: `timeout`, `connection closed`, `HTTP <code>`.

## The schedule

Every endpoint has its own `NextAt`: alive `interval` later ±10 %; dead `2 × interval` later, the
pause doubling up to `dead_interval_max`; new at once; rejected never. `concurrency` workers take
due endpoints, urgent first (`Request`), then the longest due. `lag_seconds` reports how long the
next due check waits; over an interval it is logged once. The daemon rereads its settings, the
subscriptions and `xray.active_server` every minute; `Stamp` (name, inode, size, mtime of every
file) spares the rebuild when nothing was written.

The WAN guard: at least 5 checks - or every checkable endpoint when there are fewer - completed
within 30 s, all failed, and no control answered within 30 s: the daemon dials
`endpoint.WANControls`. None accepting, the failures in the window are undone, the state is
`wan_down` and checks pause; controls are dialed every 15 s, and when one accepts every endpoint
is due.

States: `ok`, `stopped` (`/tmp/vpn-director/stopped`), `disabled` (`monitor.enabled`),
`no_xray`, `wan_down`, `prober_error`. The first four stop the prober; statuses stay as they were.

## The socket

`/tmp/vpn-director/watchd.sock`, 0600, HTTP: `GET /v1/monitor` (the whole state, endpoints by
key) and `POST /v1/monitor/check` (`{"keys": [...]}` or `{}`: 202 with `queued`; 409 while
stopped or disabled). `watchdapi.Client` bounds every request by 2 s. In-process, `Monitor.Check`
waits for answers from after the call - stage 3 uses it.

The state survives a restart: `/tmp/vpn-director/watchd-state.json`, saved every minute when it
changed and at shutdown, read at startup; the first refresh takes the entries of keys still in
the set.

## Dev mode

`cd server && go run ./cmd/watchd --dev`: `testdata/dev` paths (socket `testdata/dev/watchd.sock`)
and `FakeLauncher`, whose answer follows from the first byte of each key: mostly alive, some
dead, a few refused. The Web UI and the bot in `--dev` read that socket. Subscriptions for it go
in `testdata/dev/data/subscriptions/` (the files of `testdata/substore/` will do).
````

- [ ] **Step 2: `CLAUDE.md`**

Replace:

```bash
# Web UI in development: plain HTTP, testdata/dev paths, mock shell, admin/admin
cd server && go run ./cmd/webui --dev
```

with:

```bash
# Web UI in development: plain HTTP, testdata/dev paths, mock shell, admin/admin
cd server && go run ./cmd/webui --dev

# Server monitor in development: testdata/dev paths, a fake prober, socket testdata/dev/watchd.sock
cd server && go run ./cmd/watchd --dev
```

After the table row of `server/cmd/webui/main.go` add:

```markdown
| `server/cmd/watchd/main.go` | Server monitor daemon (`vpn-director-watchd`): checks every subscription server, serves the result on a unix socket |
| `server/internal/monitor/` | The monitor's engine: endpoint set, prober (a second Xray run as `vpn-director-probe`), schedule, WAN guard, state file |
| `server/internal/watchdapi/` | The socket contract between `vpn-director-watchd` and the other daemons: types, `Health`, server, client |
| `server/internal/endpoint/` | One address of one server as the walk dials it: `PerAddress`, `ServerForDial`, `DialKey`, the monitor's `Key` |
| `router/opt/etc/init.d/S98vpn-director-watchd` | Entware init.d script of the server monitor |
```

After the `**Web UI settings**` paragraph add:

```markdown

**Server monitor settings**: the `monitor` section of `vpn-director.json` — `enabled` (true), `interval` (`1m`, a live server's check), `dead_interval_max` (`30m`, the longest pause of a dead one), `concurrency` (8), `log_level`. The daemon rereads it every minute.
```

After the line ``- `webui.md` — Web UI architecture, API table, authentication, dev mode, update flow`` add:

```markdown
- `watchd.md` — server monitor: endpoints, the prober and its name, the schedule, the WAN guard, the socket
```

- [ ] **Step 3: `.claude/rules/webui.md`**

In the API table, replace the `GET /api/servers` row:

```markdown
| GET | `/api/servers` | The servers grouped by subscription — `subscriptions`, each with `id`, `name` and its `servers`: name, address, port, IPs and the protocol label (`vless·reality`, `ss`, `hysteria2`), no credentials — plus `active`, the recorded server with its `subscription` |
```

with:

```markdown
| GET | `/api/servers` | The servers grouped by subscription — `subscriptions`, each with `id`, `name` and its `servers`: name, address, port, IPs, the protocol label (`vless·reality`, `ss`, `hysteria2`) and the `fingerprint` that matches the server with its row of `/api/monitor`, no credentials — plus `active`, the recorded server with its `subscription` |
```

and replace the `GET /api/logs` row:

```markdown
| GET | `/api/logs` | One source (`?source=`) or every source at once |
```

with:

```markdown
| GET | `/api/monitor` | The server monitor (`watchd.md`): `state` (`not_running` when `vpn-director-watchd` does not answer), `message`, `interval_seconds`, `lag_seconds`, and per subscription `id`, `alive`, `total` and its servers in list order: `index`, `fingerprint`, `status`, `latency_ms`, `checked_at`, `since`, `next_at`, `error` |
| POST | `/api/monitor/check` | Check one server now - `{subscription, index, fingerprint}`, every address of it - or `{}` for every server; 409 "server list changed" when the fingerprint at that index is another server's, 409 while the monitor is stopped or disabled, 503 when the daemon does not answer |
| GET | `/api/logs` | One source (`?source=`: `bot`, `vpn`, `xray`, `webui`, `watchd`) or every source at once |
```

- [ ] **Step 4: `.claude/rules/telegram-bot.md`**

In the architecture tree, after `│   │   ├── misc.go           # /start, /version, /ip, /logs` add:

```
│   │   ├── health.go         # The server monitor's marks for /servers and /xray
```

In the commands table, replace ``| `/logs [bot\|vpn\|xray\|webui\|all] [N]` |`` with ``| `/logs [bot\|vpn\|xray\|webui\|watchd\|all] [N]` |``.

In the long paragraph of the subscription watch, replace:
- ``(`reachControls`: `1.1.1.1:443`, `8.8.8.8:443`)`` with ``(`reachControls`, which is `endpoint.WANControls`: `1.1.1.1:443`, `8.8.8.8:443`)``;
- ``(`perAddress`: a ban takes an address, not the name)`` with ``(`endpoint.PerAddress`: a ban takes an address, not the name)``;
- ``(`dialKey`: one provider puts 62 names on 9 endpoints)`` with ``(`endpoint.DialKey`: one provider puts 62 names on 9 endpoints)``;
- ``(`ServerForDial`: the IPv4 goes into`` with ``(`endpoint.ServerForDial`: the IPv4 goes into``.

Replace the first paragraph of `## Self-Update (`/update`)`:

```markdown
Both daemons — `telegram-bot` and `webui` — are updated together, from the bot or from the Web UI. The orchestration lives in `internal/updateflow`; the bot command and the Web UI handlers are adapters over it.
```

with:

```markdown
Every daemon of the table — `telegram-bot`, `vpn-director-watchd` and `webui` — is updated together, from the bot or from the Web UI; the monitor starts no update of its own. The orchestration lives in `internal/updateflow`; the bot command and the Web UI handlers are adapters over it. The update script restarts the daemons that ran before it, and starts as well every daemon whose binary did not exist before it — one new with this release, which nothing ran — once the copy has succeeded.
```

Insert above `## Self-Update (`/update`)`:

```markdown
## Server monitor marks (`/servers`, `/xray`)

With `vpn-director-watchd` answering (`watchd.md`), `/servers` starts each line with 🟢 (with
its latency, `· 142 ms`), 🔴, ⚪ (not yet checked) or ⛔ (rejected) and heads each subscription
with `<alive>/<total> живы`; `/xray` labels a subscription `Alpha (45/62)` and a server
`🟢 3. Oslo · 142 ms`. A daemon that does not answer - the socket is asked with a 2 s bound -
leaves the marks out, and the page says «мониторинг не запущен»; one that answers but does not
check says why under the title (stopped with VPN Director, disabled, no xray, WAN down, the
prober does not start). The marks come from `handler/health.go` (`monitorHealth`), which folds
each server's endpoints with `watchdapi.Health`.
```

- [ ] **Step 5: `README.md` and `README.ru.md`**

In `README.md`, replace in **Updates**:
- `«Update to vX» downloads the release and updates both the Web UI and the Telegram bot, restarting the ones that were running;` with `«Update to vX» downloads the release and updates every daemon — the Web UI, the Telegram bot and the server monitor — restarting the ones that were running and starting one the release adds;`
- `both paths update both daemons.` with `both paths update every daemon.`

Insert above `## How It Works`:

````markdown
## Server Monitor

`vpn-director-watchd` checks every server of every subscription, once a minute, through a second Xray process of its own — the live Xray and its clients never notice. A check fetches `http://www.gstatic.com/generate_204` through the server; 204 within 10 s is alive. A dead server is checked less and less often: after 2 minutes, then 4, 8, 16, and every 30 minutes at most. When every server fails at once, the monitor asks `1.1.1.1` and `8.8.8.8` whether the WAN is up, and keeps the statuses while it is down.

The Web UI's **Servers** tab shows each server's status and latency, how many of each subscription are alive, and a `↻` to check a server now; the bot marks `/servers` and `/xray` with 🟢 🔴 ⚪ ⛔.

Cost: about 5 KB of traffic per check, so 100 live servers checked every minute take about 0.7 GB a day, 21 GB a month, spread over the subscriptions they belong to. The daemon takes 10–15 MB of memory, its Xray about 35 MB. On a router with little memory, or subscriptions with a traffic cap, raise `interval` or set `enabled` to false:

```json
{
  "monitor": {
    "enabled": true,
    "interval": "1m",
    "dead_interval_max": "30m",
    "concurrency": 8,
    "log_level": "info"
  }
}
```

The daemon rereads the section every minute; it logs to `/tmp/vpn-director-watchd.log`.

```bash
/opt/etc/init.d/S98vpn-director-watchd start
/opt/etc/init.d/S98vpn-director-watchd stop
/opt/etc/init.d/S98vpn-director-watchd restart
```
````

In **Process Monitoring**, after the `webui` monit block (the fenced block ending with `if does not exist then restart` under `**webui:**`) add:

````markdown
   **vpn-director-watchd:**
   ```
   check process vpn-director-watchd matching "vpn-director-watchd"
       start program = "/opt/etc/init.d/S98vpn-director-watchd start"
       stop program = "/opt/etc/init.d/S98vpn-director-watchd stop"
       if does not exist then restart
   ```

   The monitor's prober runs as `vpn-director-probe`, so the `xray` rule above never takes it for Xray.
````

In `README.ru.md`, replace in **Обновления**:
- `Кнопка «Update to vX» скачивает релиз и обновляет и Web UI, и Telegram-бота, перезапуская те, что работали;` with `Кнопка «Update to vX» скачивает релиз и обновляет все демоны — Web UI, Telegram-бота и монитор серверов, — перезапуская те, что работали, и запуская тот, что появился в релизе;`
- `оба пути обновляют оба демона.` with `оба пути обновляют все демоны.`

Insert above `## Как это работает`:

````markdown
## Мониторинг серверов

`vpn-director-watchd` раз в минуту проверяет каждый сервер каждой подписки через собственный, второй процесс Xray — рабочий Xray и его клиенты этого не замечают. Проверка — запрос `http://www.gstatic.com/generate_204` через сервер; ответ 204 за 10 с — сервер жив. Мёртвый сервер проверяется всё реже: через 2 минуты, потом через 4, 8, 16 и не реже раза в 30 минут. Когда разом не отвечает ни один сервер, монитор спрашивает `1.1.1.1` и `8.8.8.8`, жив ли WAN, и, пока он лежит, сохраняет статусы.

Вкладка **Servers** веб-интерфейса показывает статус и задержку каждого сервера, сколько серверов каждой подписки живы, и кнопку `↻`, чтобы проверить сервер сейчас; бот отмечает `/servers` и `/xray` значками 🟢 🔴 ⚪ ⛔.

Цена: около 5 КБ трафика на проверку, то есть 100 живых серверов с проверкой раз в минуту — около 0,7 ГБ в сутки, 21 ГБ в месяц, поделённые между подписками этих серверов. Демон занимает 10–15 МБ памяти, его Xray — около 35 МБ. На роутере с малой памятью или при подписках с лимитом трафика увеличьте `interval` или выставьте `enabled` в false:

```json
{
  "monitor": {
    "enabled": true,
    "interval": "1m",
    "dead_interval_max": "30m",
    "concurrency": 8,
    "log_level": "info"
  }
}
```

Демон перечитывает секцию раз в минуту; его лог — `/tmp/vpn-director-watchd.log`.

```bash
/opt/etc/init.d/S98vpn-director-watchd start
/opt/etc/init.d/S98vpn-director-watchd stop
/opt/etc/init.d/S98vpn-director-watchd restart
```
````

In **Мониторинг процессов**, after the `webui` monit block add:

````markdown
   **vpn-director-watchd:**
   ```
   check process vpn-director-watchd matching "vpn-director-watchd"
       start program = "/opt/etc/init.d/S98vpn-director-watchd start"
       stop program = "/opt/etc/init.d/S98vpn-director-watchd stop"
       if does not exist then restart
   ```

   Пробный Xray монитора работает под именем `vpn-director-probe`, поэтому правило для `xray` выше никогда не примет его за Xray.
````

- [ ] **Step 6: The final check**

Run from the repository root:

```bash
make web-embed
(cd server && gofmt -l . && go vet ./... && go test ./... -count=1)
(cd web && npm run build)
bats router/test/*.bats router/test/unit router/test/integration > /tmp/vpd-bats.log 2>&1; tail -3 /tmp/vpd-bats.log; grep -c '^not ok' /tmp/vpd-bats.log
```

Expected: `gofmt -l` lists only `internal/ssrf/ssrf_test.go` and `internal/wizard/handler.go`; `go vet` prints nothing; every Go package `ok` (`cmd/watchd` has no test files); the SPA builds; the bats log ends with `ok 754 …` and `grep -c '^not ok'` prints `0`.

- [ ] **Step 7: Commit**

```bash
git add .claude/rules/watchd.md CLAUDE.md .claude/rules/webui.md .claude/rules/telegram-bot.md README.md README.ru.md
git commit -m "docs: the server monitor

A rules file for vpn-director-watchd - its endpoints, its prober and why nothing of it is named after Xray, its schedule, its WAN guard and its socket - and the daemon in CLAUDE.md, the Web UI's API table, the bot's marks and commands, and both READMEs: what the monitor does and costs, its settings, its init script and a monit rule."
```

## Verification on routers (with the owner, after Task 15)

The owner runs these on the RT-AX86U (Merlin, BusyBox 1.25) and on a Keenetic, with a build of the branch installed (`make build-all`, the three `vpn-director-watchd-*` binaries copied to `/opt/vpn-director/vpn-director-watchd`, the init script to `/opt/etc/init.d/`). Each answer goes into the PR description.

1. Memory and CPU with the real subscriptions: `ps w | grep -E 'vpn-director-(watchd|probe)'` and `top` for a few minutes; the prober should stay near 35 MB whatever the number of servers.
2. SOCKS user routing on the router's xray: `GET /v1/monitor` (`curl --unix-socket /tmp/vpn-director/watchd.sock http://x/v1/monitor | jq '.state, [.endpoints[].status] | group_by(.) | map({(.[0]): length})'`) shows `alive` servers; a router whose xray does not route SOCKS users would show every server dead with the WAN up.
3. The 19-character name: `pidof vpn-director-watchd`, `/opt/etc/init.d/S98vpn-director-watchd stop`, `start`, `check`.
4. `S24xray` beside the prober: with the prober running, `/opt/etc/init.d/S24xray stop` leaves `vpn-director-probe` alive (`pidof vpn-director-probe`), `start` starts the live Xray (`pidof xray` names one process), `restart` works.
5. monit: with the README's rules installed, `monit status` shows `xray` bound to the live Xray's PID, not the prober's.
6. The bytes of one check against a real server: `iptables -I OUTPUT -d <server IP> -j ACCEPT` and read its counters after one check (`POST /v1/monitor/check` with that key), then delete the rule; the spec estimates 3-5 KB for the server's handshake.
7. The first update: from the previous release, «Update» in the Web UI of a router without the daemon - `vpn-director-watchd` is running afterwards (`/tmp/vpn-director-update/update.log` says `vpn-director-watchd is new, it starts after the update`).

## Finishing

- Stage 1 changes no failover behaviour: `subwatch` still probes, walks and returns as before (Task 1 moved its helpers only). Stage 2 moves it into `vpn-director-watchd`, stage 3 lets it use `Monitor.Check` - each with its own spec, plan and PR.
- Before the PR, as the owner's workflow asks: `git rm` every file under `docs/superpowers/` that this branch added (the spec and this plan) and commit, so they stay in the branch history and out of the PR diff.
- The PR description lists the router checks above with their answers.
