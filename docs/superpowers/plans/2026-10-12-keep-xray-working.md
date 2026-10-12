# Keep Xray on a Working Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `vpn-director-watchd` keeps Xray on a working server with or without LAN clients on it. It follows a selected server that moved or changed and replaces one that left its subscription or died with the best working server: same group (protocol and security) first, the user's subscription first, by latency. It returns to the user's choice once that has been alive for 5 minutes.

**Architecture:**
- The selection record (`xray.active_server`, `xray.preferred_server`) gains `identity` and `group`. Every Go writer fills them in through `vpnconfig.RecordActiveServer`.
- `subwatch` gains three units:
  - `classifyActive`: is the record current, changed in place, moved, or gone?
  - `orderBest`: the order of candidates.
  - a keeper engine that switches through the existing `switcher.to`.
- `Tick` runs the keeper mode when the watch is not armed. Between episodes, in both modes, it runs the actuality check. The fast path, the walk and the returns order their candidates with `orderBest`.

**Tech Stack:** Go 1.25 (`server/`), Markdown docs. No new dependency.

**Spec:** `docs/superpowers/specs/2026-10-12-keep-xray-working-design.md`

## Global Constraints

- **Group:** `<protocol>+<security>`, e.g. `vless+reality`, `trojan+tls`. The bare protocol when the security is empty or `none`, e.g. `shadowsocks`. `""` for a record without an outbound, an outbound that does not parse, or one that names no protocol. The transport is no part of it.
- **`identity`:** the first 16 hex digits of the SHA-256 of `looseIdentity(s)`; `""` when that is `""`.
- **JSON:** `ActiveServer` gains `identity` and `group`, both `omitempty`. A record without them is checked by name, address and port.
- **Return timing:** `ReturnAlive` = 5 minutes. `ReturnCheck`, `ReturnRetry`, `ReturnRetryMax`, `ReturnHold` and `ReturnFailsMax` stay as they are.
- **Keeper backoff:** `KeeperRetry` = 10 minutes, doubling up to `KeeperRetryMax` = 30 minutes, after a run of switches none of which worked.
- **Messages, exact:**
  - `Xray server %s moved; Xray follows it`
  - `Xray server %s changed; Xray follows it`
  - `Xray server %s left its subscription; Xray switched to %s`
  - `Xray server %s is not answering; Xray switched to %s`
  - `No working Xray server found; Xray stays on %s`
  - `Xray back on a %s server %s`

  `%s` names a server `<subscription> / <server>` (`label`). The fast path keeps `Xray switched to server %s`, and the return keeps `Xray back on the preferred server %s`.
- **Keeper mode runs only when all of these hold:**
  - the watch is not armed (`vpnconfig.Armed` false, no `pendingApply`);
  - `active_server.subscription` is not empty;
  - there is at least one subscription;
  - `Generate` and `Probe` are set;
  - the monitor's state is not `no_xray`.
- **Every switch:**
  - happens inside the tick under `tickMu`;
  - goes through `switcher.to`: the walk's guard, `restart xray-process` only, the SOCKS probe;
  - never runs `restart xray`.
- **Unchanged:**
  - the watchd socket contract and the config keys;
  - every shell script, `configure.sh` included.
- **Logs and comments:**
  - no log line carries a subscription link;
  - comments are full sentences that say why, in the surrounding style, with no TODO.
- **Tests:**
  - `cd server && go test ./...`;
  - the concurrent packages also under `-race`;
  - `make -C server test` runs `go test -v ./...`.

## Review Focus

1. A Web UI or `/xray` selection commits while the keeper switches. The keeper's write is refused, the user's selection stands, and no message is sent. Owner: Task 5, `TestKeeper_ASelectionDuringTheSwitchStands`.
2. A `/stop` lands while the keeper switches. Nothing more is written or restarted, and no message is sent. Owner: Task 5, `TestKeeper_AStopEndsTheSwitchSilently`.
3. The monitor is disabled or not OK. The replacement order keeps the walk's order (`OwnFirst`, then one server of each subscription in turn), the user's group first. It must not try every server of the user's subscription first. Owner: Task 3, `TestOrderBest_WithoutEvidenceKeepsTheWalkOrderGroupFirst`.
4. The entry a moved server is followed to does not answer either. The keeper replaces it as dead, leaving it out, instead of trying it again every tick. Owner: Task 5, `TestKeeper_AFollowedEntryThatDoesNotAnswerIsReplaced`.
5. No candidate works. The keeper sends "No working Xray server found" once, and the next attempt waits `KeeperRetry`: Xray must not restart every 30 seconds. Owner: Task 5, `TestKeeper_NothingWorksSaysOnceAndBacksOff`.

---

## File Structure

| File | Responsibility |
|---|---|
| `server/internal/vpnconfig/group.go` (new) | `Server.Group`, `IdentityMark` |
| `server/internal/vpnconfig/vpnconfig.go` | `ActiveServer.Identity`, `ActiveServer.Group`; `NewActiveServer` fills them |
| `server/internal/subwatch/actuality.go` (new) | `classifyActive` |
| `server/internal/subwatch/best.go` (new; replaces `health_order.go`) | `bestRef`, `refFor`, `orderBest`, `walkEvidence`, `bestWalkOrder` |
| `server/internal/subwatch/keeper.go` (new) | keeper mode: `keeps`, `keep`, `followActive`, `replaceActive`, `replaceDead`, `switchAmong`, `keeperBackOff`, `say`, the messages |
| `server/internal/subwatch/watch.go` | `Tick` runs the keeper mode and the actuality check; the walk orders with `bestWalkOrder`; three keeper fields |
| `server/internal/subwatch/fast.go` | the fast path orders its candidates with `orderBest` |
| `server/internal/subwatch/return.go` | `ReturnAlive`, `aliveFor`, the evidence look, the return to the group |
| `.claude/rules/watchd.md`, `telegram-bot.md`, `webui.md`, `README.md`, `README.ru.md`, `CLAUDE.md` | documentation |

---

### Task 1: The selection record carries its server's identity and group

**Files:**
- Create: `server/internal/vpnconfig/group.go`
- Create: `server/internal/vpnconfig/group_test.go`
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`ActiveServer`, `NewActiveServer`)
- Modify: tests that compare whole `ActiveServer` records (see Step 6)

**Interfaces:**
- Consumes: `looseIdentity(s Server) string` (`identity.go`), `DecodeOutbound` (`outbound.go`).
- Produces: `func (s Server) Group() string`; `func IdentityMark(s Server) string`; `ActiveServer.Identity string` (`json:"identity,omitempty"`), `ActiveServer.Group string` (`json:"group,omitempty"`); `NewActiveServer(s)` sets both, so `RecordActiveServer` and `RecordWalkedServer` carry them.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/vpnconfig/group_test.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A server's group is its protocol and its security: the kind a replacement is
// picked from first. The transport is no part of it.
func TestServerGroup_ProtocolAndSecurity(t *testing.T) {
	cases := []struct {
		name string
		s    Server
		want string
	}{
		{"reality", reality("DE", "www.example.com", "aa11"), "vless+reality"},
		{"websocket over tls", webSocket("DE", "cdn.example", "/de"), "vless+tls"},
		{"trojan", Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"t.example","port":443,"password":"p"}]},"streamSettings":{"network":"tcp","security":"tls"}}`)}, "trojan+tls"},
		{"shadowsocks", Server{Outbound: json.RawMessage(`{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss.example","port":8388,"method":"aes-256-gcm","password":"p"}]}}`)}, "shadowsocks"},
		{"security none", Server{Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":80,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"ws","security":"none"}}`)}, "vless"},
		{"legacy record", Server{Name: "Old", Address: "old.example", Port: 443}, ""},
		{"unreadable", Server{Outbound: json.RawMessage(`{"protocol":`)}, ""},
		{"null", Server{Outbound: json.RawMessage(`null`)}, ""},
		{"no protocol", Server{Outbound: json.RawMessage(`{"settings":{}}`)}, ""},
	}
	for _, tc := range cases {
		if got := tc.s.Group(); got != tc.want {
			t.Errorf("%s: group %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The mark follows the server, not what a panel picks at random for each
// download or the name it gives the server.
func TestIdentityMark_FollowsTheServerNotThePanelsPicks(t *testing.T) {
	a := IdentityMark(reality("DE", "www.example.com", "aa11"))
	if len(a) != 16 {
		t.Fatalf("mark %q, want 16 hex digits", a)
	}
	if b := IdentityMark(reality("DE 9GB", "www.other.com", "bb22")); b != a {
		t.Fatalf("the REALITY picks and the name moved the mark: %q, %q", a, b)
	}
	if IdentityMark(webSocket("DE", "a.example", "/x1")) != IdentityMark(webSocket("DE", "b.example", "/y22")) {
		t.Fatal("a generated WebSocket Host and path moved the mark")
	}
	moved := reality("DE", "www.example.com", "aa11")
	moved.Outbound = realityOutbound("nl.example", "www.example.com", "aa11")
	if IdentityMark(moved) == a {
		t.Fatal("a new address kept the mark")
	}
	rekeyed := reality("DE", "www.example.com", "aa11")
	rekeyed.Outbound = json.RawMessage(strings.Replace(string(rekeyed.Outbound), `"u-1"`, `"u-2"`, 1))
	if IdentityMark(rekeyed) == a {
		t.Fatal("a new UUID kept the mark")
	}
	if m := IdentityMark(Server{Name: "Old", Address: "old.example", Port: 443}); m != "" {
		t.Fatalf("a legacy record has mark %q, want none", m)
	}
}

// Every writer of the selection goes through NewActiveServer: the record keeps
// the server's mark and group, and the walk's copy into preferred_server keeps
// them too. A record without them writes neither key.
func TestNewActiveServer_RecordsIdentityAndGroup(t *testing.T) {
	s := reality("DE", "www.example.com", "aa11")
	s.Subscription = "0a1b2c3d"
	a := NewActiveServer(s)
	if a.Identity != IdentityMark(s) || a.Group != "vless+reality" {
		t.Fatalf("record %+v", a)
	}
	r := RecordActiveServer(&ActiveServer{Seq: 4}, s)
	if r.Seq != 5 || r.Identity != a.Identity || r.Group != a.Group {
		t.Fatalf("record %+v", r)
	}
	cfg := &VPNDirectorConfig{}
	cfg.Xray.ActiveServer = a
	other := reality("NL", "www.example.com", "aa11")
	other.Subscription = "0a1b2c3d"
	other.Outbound = realityOutbound("nl.example", "www.example.com", "aa11")
	RecordWalkedServer(cfg, other)
	if p := cfg.Xray.PreferredServer; p == nil || p.Identity != a.Identity || p.Group != a.Group {
		t.Fatalf("preferred %+v, want the record of DE", p)
	}
	data, err := json.Marshal(&ActiveServer{Name: "DE", Address: "de.example", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "identity") || strings.Contains(string(data), "group") {
		t.Fatalf("a record without them writes them: %s", data)
	}
}

// A rename follows the server, whose outbound - and so its mark and group -
// did not change.
func TestPublishRefresh_ARenameKeepsTheRecordsMarkAndGroup(t *testing.T) {
	stored := reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(stored)}}
	stored.Subscription = "0a1b2c3d"
	m.cfg.Xray.ActiveServer = RecordActiveServer(nil, stored)
	want := *m.cfg.Xray.ActiveServer
	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE 9GB", "www.example.com", "aa11", "203.0.113.10")}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := m.cfg.Xray.ActiveServer
	if got == nil || got.Name != "DE 9GB" || got.Identity != want.Identity || got.Group != want.Group || got.Seq != want.Seq {
		t.Fatalf("record %+v, want %+v under the new name", got, want)
	}
}
```

`reality`, `realityOutbound` and `webSocket` are in `identity_test.go`. `memStore`, `alphaWith`, `alphaLink` and `t0` are in `refresh_test.go`.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/vpnconfig/ -run 'TestServerGroup|TestIdentityMark|TestNewActiveServer_RecordsIdentityAndGroup|TestPublishRefresh_ARenameKeepsTheRecordsMarkAndGroup'`
Expected: build failure: `s.Group undefined`, `undefined: IdentityMark`, `a.Identity undefined`.

- [ ] **Step 3: Implement**

Create `server/internal/vpnconfig/group.go`:

```go
package vpnconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Group is the kind of server s is when a replacement is picked: its
// outbound's protocol and its stream's security, "vless+reality",
// "trojan+tls", "shadowsocks". Servers of one group fare alike on a censored
// path - a panel's Shadowsocks servers can pass a check and carry nothing
// while its REALITY ones work - so the watch tries the group of the user's
// choice first. The transport is no part of it: tcp, xhttp, ws and grpc of one
// protocol and security would split a provider's servers too finely. A legacy
// record without an outbound, and an outbound that does not parse or names no
// protocol, have no group.
func (s Server) Group() string {
	if len(s.Outbound) == 0 {
		return ""
	}
	if _, err := DecodeOutbound(s.Outbound); err != nil {
		return ""
	}
	var ob struct {
		Protocol       string `json:"protocol"`
		StreamSettings struct {
			Security string `json:"security"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(s.Outbound, &ob); err != nil || ob.Protocol == "" {
		return ""
	}
	if ob.StreamSettings.Security == "" || ob.StreamSettings.Security == "none" {
		return ob.Protocol
	}
	return ob.Protocol + "+" + ob.StreamSettings.Security
}

// IdentityMark is a short fingerprint of s's loose identity - its outbound
// without what a panel picks at random for each download - for the selection
// record: the first 16 hex digits of its SHA-256. A record that keeps it tells
// an entry that changed in place - new keys, a new UUID - from one a refresh
// only renamed. "" for a server without an identity.
func IdentityMark(s Server) string {
	id := looseIdentity(s)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}
```

In `server/internal/vpnconfig/vpnconfig.go`, add to `ActiveServer` after `Seq`:

```go
	// Identity is IdentityMark of the server the record was written from, and
	// Group its Group. watchd tells by them a server that changed in place or
	// moved from one that is current, and picks a replacement of the user's
	// group. configure.sh and older daemons write records without them; such a
	// record is checked by name, address and port.
	Identity string `json:"identity,omitempty"`
	Group    string `json:"group,omitempty"`
```

Replace `NewActiveServer`:

```go
// NewActiveServer records the fields of s that identify it to a reader. The
// rest - UUID, REALITY keys - would put credentials in a file the Web UI hands
// out over /api/config; the mark is a hash that names none of them.
func NewActiveServer(s Server) *ActiveServer {
	return &ActiveServer{Name: s.Name, Address: s.Address, Port: s.Port, Subscription: s.Subscription,
		Identity: IdentityMark(s), Group: s.Group()}
}
```

- [ ] **Step 4: Run the new tests to verify they pass**

Run: `cd server && go test ./internal/vpnconfig/ -run 'TestServerGroup|TestIdentityMark|TestNewActiveServer_RecordsIdentityAndGroup|TestPublishRefresh_ARenameKeepsTheRecordsMarkAndGroup' -v`
Expected: PASS.

- [ ] **Step 5: Run every package**

Run: `cd server && go test ./... 2>&1 | grep -E '^(ok|FAIL|---)' | head -60`
Expected: some tests fail because they compare a whole `ActiveServer` (`reflect.DeepEqual`, `==`, `!=`) with a record that now carries `Identity` and `Group`, e.g. `wantActive := &vpnconfig.ActiveServer{...}` in `subwatch/return_test.go`.

- [ ] **Step 6: Update those expectations, not the behaviour**

For every such failure, build the expected record with the server's mark and group. Do not drop the fields from the production record. Use `vpnconfig.IdentityMark(server)` and `server.Group()`, or derive the record with `vpnconfig.NewActiveServer(server)` and set `Seq`. A test that fails for any other reason is a real regression: stop and report it. Re-run until `cd server && go test ./...` passes. List every test you changed in your report.

- [ ] **Step 7: Commit**

```bash
git add server/internal/vpnconfig/group.go server/internal/vpnconfig/group_test.go server/internal/vpnconfig/vpnconfig.go <each test file you changed in Step 6>
git commit -m "feat(vpnconfig): a selection record carries its server's identity and group"
```

---

### Task 2: Is the selected server current, changed, moved or gone?

**Files:**
- Create: `server/internal/subwatch/actuality.go`
- Create: `server/internal/subwatch/actuality_test.go`

**Interfaces:**
- Consumes: `vpnconfig.IdentityMark` (Task 1), `vpnconfig.FindSubscription`.
- Produces: `type actuality uint8` with `actualUnknown`, `actualCurrent`, `actualChanged`, `actualMoved`, `actualGone`; `func classifyActive(subs []vpnconfig.Subscription, a *vpnconfig.ActiveServer) (actuality, vpnconfig.Server)`. The server returned for `actualChanged` and `actualMoved` carries its subscription's id.

- [ ] **Step 1: Write the failing test**

Create `server/internal/subwatch/actuality_test.go`:

```go
package subwatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestClassifyActive(t *testing.T) {
	oslo := fastServer("Oslo", "oslo.example", "203.0.113.10")
	record := vpnconfig.RecordActiveServer(nil, oslo)
	moved := fastServer("Oslo", "oslo-2.example", "203.0.113.20")
	rekeyed := oslo
	rekeyed.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), "000000000010", "000000000011", 1))
	bergen := fastServer("Bergen", "bergen.example", "203.0.113.30")
	alpha := func(servers ...vpnconfig.Server) []vpnconfig.Subscription {
		return []vpnconfig.Subscription{{ID: "alpha", Name: "Alpha", URL: "https://alpha.example/list", Servers: servers}}
	}
	cases := []struct {
		name    string
		subs    []vpnconfig.Subscription
		a       *vpnconfig.ActiveServer
		want    actuality
		address string // of the entry returned
	}{
		{"current", alpha(bergen, oslo), record, actualCurrent, "oslo.example"},
		{"changed in place", alpha(rekeyed), record, actualChanged, "oslo.example"},
		{"moved", alpha(bergen, moved), record, actualMoved, "oslo-2.example"},
		{"gone: no entry of the name", alpha(bergen), record, actualGone, ""},
		{"gone: the name twice", alpha(moved, fastServer("Oslo", "oslo-3.example", "203.0.113.40")), record, actualGone, ""},
		{"gone: the subscription deleted", nil, record, actualGone, ""},
		{"a record without a mark is checked by name, address and port", alpha(rekeyed), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}, actualCurrent, "oslo.example"},
		{"a record without address and port is checked by name", alpha(moved), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo"}, actualCurrent, "oslo-2.example"},
		{"a record without a subscription", alpha(oslo), &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}, actualUnknown, ""},
		{"no record", alpha(oslo), nil, actualUnknown, ""},
	}
	for _, tc := range cases {
		got, entry := classifyActive(tc.subs, tc.a)
		if got != tc.want || entry.Address != tc.address {
			t.Errorf("%s: %d with entry %q, want %d with %q", tc.name, got, entry.Address, tc.want, tc.address)
		}
		if tc.address != "" && entry.Subscription != "alpha" {
			t.Errorf("%s: entry carries subscription %q", tc.name, entry.Subscription)
		}
	}
}
```

`fastServer(name, host, ip)` (`fast_test.go`) is a VLESS TLS server of subscription `alpha` on port 443.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd server && go test ./internal/subwatch/ -run TestClassifyActive`
Expected: build failure, `undefined: classifyActive`.

- [ ] **Step 3: Implement**

Create `server/internal/subwatch/actuality.go`:

```go
package subwatch

import "github.com/zinin/vpn-director/server/internal/vpnconfig"

// actuality is how the server active_server names stands against its
// subscription's list as it is now (spec 4.2).
type actuality uint8

const (
	// actualUnknown is no record, or one without a subscription: nothing to keep.
	actualUnknown actuality = iota
	actualCurrent
	// actualChanged is the entry of the record's name, address and port with
	// another mark: new keys or a new UUID behind the same name and address.
	actualChanged
	// actualMoved is no such entry, and exactly one entry of the name.
	actualMoved
	// actualGone is neither: the subscription deleted, no entry of the name,
	// or several - the name no longer tells which server the user chose.
	actualGone
)

// classifyActive compares a with its subscription's list in subs, and returns
// the entry a is to follow when the server changed in place or moved. A record
// without a mark - configure.sh's, an older daemon's - is current while the
// entry of its name, address and port is listed; one without an address and a
// port while an entry of its name is.
func classifyActive(subs []vpnconfig.Subscription, a *vpnconfig.ActiveServer) (actuality, vpnconfig.Server) {
	if a == nil || a.Subscription == "" || a.Name == "" {
		return actualUnknown, vpnconfig.Server{}
	}
	i := vpnconfig.FindSubscription(subs, a.Subscription)
	if i < 0 {
		return actualGone, vpnconfig.Server{}
	}
	var named []vpnconfig.Server
	for _, s := range subs[i].Servers {
		if s.Name != a.Name {
			continue
		}
		s.Subscription = subs[i].ID
		if a.Address == "" && a.Port == 0 {
			return actualCurrent, s
		}
		if s.Address == a.Address && s.Port == a.Port {
			if a.Identity == "" || vpnconfig.IdentityMark(s) == a.Identity {
				return actualCurrent, s
			}
			return actualChanged, s
		}
		named = append(named, s)
	}
	if len(named) == 1 {
		return actualMoved, named[0]
	}
	return actualGone, vpnconfig.Server{}
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd server && gofmt -l ./internal/subwatch && go test ./internal/subwatch/ -run TestClassifyActive -v`
Expected: no gofmt output; PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/subwatch/actuality.go server/internal/subwatch/actuality_test.go
git commit -m "feat(subwatch): tell whether the selected server is current, changed, moved or gone"
```

---

### Task 3: Order candidates by group, subscription and latency; the walk uses it

**Files:**
- Create: `server/internal/subwatch/best.go`
- Create: `server/internal/subwatch/best_test.go`
- Delete: `server/internal/subwatch/health_order.go`
- Delete: `server/internal/subwatch/health_order_test.go`. Move its helpers `orderTestEvidence` and `healthLabels` into `best_test.go` unchanged: `return_test.go` and `wave_test.go` use them.
- Modify: `server/internal/subwatch/watch.go` (`walk`: the order of `remaining`)

**Interfaces:**
- Consumes: `walkOrder`, `chosenIndex` (existing), `vpnconfig.Server.Group` (Task 1).
- Produces:
  - `type bestRef struct{ Subscription, Group string }`;
  - `func refFor(cfg *vpnconfig.VPNDirectorConfig, servers []vpnconfig.Server) bestRef`;
  - `func orderBest(copies []vpnconfig.Server, ref bestRef, ev monitor.Evidence, ok bool) []vpnconfig.Server`;
  - `func (w *Watch) walkEvidence(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) (monitor.Evidence, bool)`;
  - `func (w *Watch) bestWalkOrder(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig, ref bestRef) []vpnconfig.Server`;
  - test helper `ssServer(name, ip string) vpnconfig.Server` in `best_test.go`, for Tasks 4–6.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/subwatch/best_test.go`. Move into it, unchanged, `orderTestEvidence` and `healthLabels` from `health_order_test.go`. Port the three subtests of `TestHealthOrder_StableGroupsAndCurrentRejections` to `orderBest(copies, bestRef{Subscription: "aaaaaaaa"}, e, true)` with these expectations:

- **"alive first without latency sorting…"** becomes "alive first by latency". Its want is now `[]string{"alive2", "alive1", "unknown1", "dead1"}`: alive2 has 1 ms, alive1 900 ms. The rejected one stays out.
- **"stable after OwnFirst round robin and PerAddress":** same `want` as before. Every alive copy has 17 ms, and the Alpha copies, the reference subscription, come first among the alive ones as before.
- **"no matching keys retains every candidate":** same `want`.

Then add:

```go
// ssServer is a Shadowsocks server of subscription alpha on ip: another group
// than fastServer's VLESS over TLS.
func ssServer(name, ip string) vpnconfig.Server {
	return vpnconfig.Server{
		Subscription: "alpha", Name: name, Address: ip, Port: 8388, IPs: []string{ip},
		Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"shadowsocks","settings":{"servers":[{"address":%q,"port":8388,"method":"aes-256-gcm","password":"p"}]}}`, ip)),
	}
}

func bestNames(servers []vpnconfig.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name
	}
	return out
}

func bestEvidence(latency map[vpnconfig.Server]int64, states map[string]watchdapi.Status, servers []vpnconfig.Server) monitor.Evidence {
	e := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	for _, s := range servers {
		st, ok := states[s.Name]
		if !ok {
			continue
		}
		e.Endpoints[endpoint.Key(s)] = watchdapi.EndpointState{Status: st, LatencyMS: latency[s]}
	}
	return e
}

// The alive servers go first in tiers - the user's group in the user's
// subscription, the group elsewhere, other groups in the subscription, other
// groups elsewhere - each by latency; then the rest, the group first, each
// part in the order given; rejected ones not at all.
func TestOrderBest_Tiers(t *testing.T) {
	beta := func(s vpnconfig.Server) vpnconfig.Server { s.Subscription = "beta"; return s }
	aV1 := fastServer("aV1", "a1.example", "203.0.113.1")
	aSS := ssServer("aSS", "203.0.113.2")
	aV2 := fastServer("aV2", "a2.example", "203.0.113.3")
	bV1 := beta(fastServer("bV1", "b1.example", "198.51.100.1"))
	bSS := beta(ssServer("bSS", "198.51.100.2"))
	aV3 := fastServer("aV3", "a3.example", "203.0.113.4")
	bV2 := beta(fastServer("bV2", "b2.example", "198.51.100.3"))
	aV4 := fastServer("aV4", "a4.example", "203.0.113.5")
	aSS2 := ssServer("aSS2", "203.0.113.6")
	copies := []vpnconfig.Server{aV1, aSS, aV2, bV1, bSS, aV3, bV2, aV4, aSS2}
	e := bestEvidence(
		map[vpnconfig.Server]int64{aV1: 30, aSS: 5, aV2: 10, bV1: 1, bSS: 2},
		map[string]watchdapi.Status{
			"aV1": watchdapi.StatusAlive, "aSS": watchdapi.StatusAlive, "aV2": watchdapi.StatusAlive,
			"bV1": watchdapi.StatusAlive, "bSS": watchdapi.StatusAlive, "bV2": watchdapi.StatusDead,
			"aV4": watchdapi.StatusRejected,
		}, copies)
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha", Group: "vless+tls"}, e, true))
	want := []string{"aV2", "aV1", "bV1", "aSS", "bSS", "aV3", "bV2", "aSS2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Without evidence nothing counts as alive: the group goes first and each part
// keeps the order given - the walk's OwnFirst and round robin - rather than
// every server of the user's subscription before another provider's first.
func TestOrderBest_WithoutEvidenceKeepsTheWalkOrderGroupFirst(t *testing.T) {
	beta := func(s vpnconfig.Server) vpnconfig.Server { s.Subscription = "beta"; return s }
	copies := []vpnconfig.Server{
		ssServer("aSS", "203.0.113.2"), fastServer("aV1", "a1.example", "203.0.113.1"),
		beta(fastServer("bV1", "b1.example", "198.51.100.1")), fastServer("aV2", "a2.example", "203.0.113.3"),
	}
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha", Group: "vless+tls"}, monitor.Evidence{}, false))
	if want := []string{"aV1", "bV1", "aV2", "aSS"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// With no group to lean on, groups do not order anything.
func TestOrderBest_NoReferenceGroup(t *testing.T) {
	aSS, aV1 := ssServer("aSS", "203.0.113.2"), fastServer("aV1", "a1.example", "203.0.113.1")
	copies := []vpnconfig.Server{aV1, aSS}
	e := bestEvidence(map[vpnconfig.Server]int64{aV1: 30, aSS: 5},
		map[string]watchdapi.Status{"aV1": watchdapi.StatusAlive, "aSS": watchdapi.StatusAlive}, copies)
	got := bestNames(orderBest(copies, bestRef{Subscription: "alpha"}, e, true))
	if want := []string{"aSS", "aV1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// The reference is the user's choice: preferred_server, else active_server;
// its group from the record, else from the entry it names.
func TestRefFor(t *testing.T) {
	oslo, shadow := fastServer("Oslo", "oslo.example", "203.0.113.10"), ssServer("Shadow", "203.0.113.20")
	servers := []vpnconfig.Server{oslo, shadow}
	cfg := &vpnconfig.VPNDirectorConfig{}
	if got := refFor(cfg, servers); got != (bestRef{}) {
		t.Fatalf("no record: %+v", got)
	}
	cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(nil, shadow)
	if got := refFor(cfg, servers); got != (bestRef{Subscription: "alpha", Group: "shadowsocks"}) {
		t.Fatalf("active: %+v", got)
	}
	cfg.Xray.PreferredServer = vpnconfig.NewActiveServer(oslo)
	if got := refFor(cfg, servers); got != (bestRef{Subscription: "alpha", Group: "vless+tls"}) {
		t.Fatalf("preferred: %+v", got)
	}
	cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}
	if got := refFor(cfg, servers); got.Group != "vless+tls" {
		t.Fatalf("a record without a group takes its entry's: %+v", got)
	}
}
```

The imports of `best_test.go` are `encoding/json`, `fmt`, `reflect`, `testing`, `time`, `endpoint`, `monitor`, `vpnconfig` and `watchdapi`. `subOf` and `chosenOf` stay where they are.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/subwatch/ -run 'TestOrderBest|TestRefFor|TestHealthOrder'`
Expected: build failure, `undefined: orderBest`, `undefined: bestRef`, `undefined: refFor`.

- [ ] **Step 3: Implement**

Delete `server/internal/subwatch/health_order.go` and `server/internal/subwatch/health_order_test.go`. The test file's helpers now live in `best_test.go`. Create `server/internal/subwatch/best.go`:

```go
package subwatch

import (
	"sort"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// bestRef is what the choice of a replacement leans on (spec 4.5): the
// subscription and the group of the user's choice.
type bestRef struct {
	Subscription string
	Group        string
}

// refFor is the reference for cfg: preferred_server when one is kept - a
// choice that left its list included, whose group is still the one the user
// wants - and active_server otherwise. The group comes from the record, or
// from the entry it names in servers when the record has none.
func refFor(cfg *vpnconfig.VPNDirectorConfig, servers []vpnconfig.Server) bestRef {
	if cfg == nil {
		return bestRef{}
	}
	a := cfg.Xray.PreferredServer
	if a == nil {
		a = cfg.Xray.ActiveServer
	}
	if a == nil {
		return bestRef{}
	}
	ref := bestRef{Subscription: a.Subscription, Group: a.Group}
	if ref.Group == "" {
		if i := chosenIndex(servers, a); i >= 0 {
			ref.Group = servers[i].Group()
		}
	}
	return ref
}

// orderBest orders copies - endpoint.PerAddress copies in the walk's order -
// for a switch. With ok, the copies the monitor holds alive come first, in
// tiers: the reference group in the reference subscription, the group
// elsewhere, other groups in the subscription, other groups elsewhere; each
// tier by latency, ties in the order given. Every other copy follows, the
// reference group first, each part in the order given: a provider that fails
// as a whole then costs the walk's OwnFirst tries, not one per server. Copies
// the monitor rejected are left out. Without ok nothing counts as alive. With
// no reference group, groups order nothing.
func orderBest(copies []vpnconfig.Server, ref bestRef, ev monitor.Evidence, ok bool) []vpnconfig.Server {
	type ranked struct {
		s       vpnconfig.Server
		tier    int
		latency int64
	}
	out := make([]ranked, 0, len(copies))
	for _, c := range copies {
		st, known := ev.Endpoints[endpoint.Key(c)]
		if ok && known && st.Status == watchdapi.StatusRejected {
			continue
		}
		group := ref.Group == "" || c.Group() == ref.Group
		own := c.Subscription == ref.Subscription
		alive := ok && known && st.Status == watchdapi.StatusAlive
		r := ranked{s: c}
		switch {
		case alive && group && own:
			r.tier = 0
		case alive && group:
			r.tier = 1
		case alive && own:
			r.tier = 2
		case alive:
			r.tier = 3
		case group:
			r.tier = 4
		default:
			r.tier = 5
		}
		if alive {
			r.latency = st.LatencyMS
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		return out[i].tier < 4 && out[i].latency < out[j].latency
	})
	servers := make([]vpnconfig.Server, len(out))
	for i, r := range out {
		servers[i] = r.s
	}
	return servers
}

// walkEvidence is the monitor's evidence for ordering servers, and whether it
// may: monitoring on, its state OK and every key of servers it knows still
// valid. Missing keys stay unknown; an empty validation request would mean the
// whole monitor set.
func (w *Watch) walkEvidence(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) (monitor.Evidence, bool) {
	if len(servers) == 0 || w.Health == nil ||
		(cfg != nil && cfg.Monitor != nil && cfg.Monitor.Enabled != nil && !*cfg.Monitor.Enabled) {
		return monitor.Evidence{}, false
	}
	evidence := w.Health.Evidence()
	if evidence.State != watchdapi.StateOK {
		return monitor.Evidence{}, false
	}
	keys := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		key := endpoint.Key(s)
		if _, ok := evidence.Endpoints[key]; ok && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	if len(keys) == 0 || w.Health.ValidateEvidence(evidence, keys) != nil {
		return monitor.Evidence{}, false
	}
	return evidence, true
}

// bestWalkOrder is orderBest on the monitor's evidence for servers, when it may
// order them.
func (w *Watch) bestWalkOrder(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig, ref bestRef) []vpnconfig.Server {
	ev, ok := w.walkEvidence(servers, cfg)
	return orderBest(servers, ref, ev, ok)
}
```

In `server/internal/subwatch/watch.go`, `walk`: after `order, chosenFirst := walkOrder(subs, chosen)` add

```go
	ref := refFor(cfg, vpnconfig.AllServers(subs))
```

and replace

```go
		// Rejected entries stay pending so later evidence can admit them again.
		ordered := w.healthWalkOrder(remaining, cfg)
```

with

```go
		// Rejected entries stay pending so later evidence can admit them again;
		// the user's group goes first, the alive by latency (orderBest).
		ordered := w.bestWalkOrder(remaining, cfg, ref)
```

- [ ] **Step 4: Run the new tests**

Run: `cd server && gofmt -l ./internal/subwatch && go test ./internal/subwatch/ -run 'TestOrderBest|TestRefFor|TestHealthOrder' -v`
Expected: no gofmt output; PASS.

- [ ] **Step 5: Run the package**

Run: `cd server && go test ./internal/subwatch/`
Expected: `ok`. A walk test that pinned the old order may fail. Two causes are possible:
- a server alive at a higher latency used to go first;
- a server of another group sat before the user's group.

Update such a test's expectation to the order of spec 4.5, and say in the report why. A failure for any other reason is a regression: stop and report it.

- [ ] **Step 6: Commit**

```bash
git rm server/internal/subwatch/health_order.go server/internal/subwatch/health_order_test.go
git add server/internal/subwatch/best.go server/internal/subwatch/best_test.go server/internal/subwatch/watch.go <each test file you changed in Step 5>
git commit -m "feat(subwatch): order candidates by group, subscription and latency"
```

---

### Task 4: The fast path tries the user's group first

**Files:**
- Modify: `server/internal/subwatch/fast.go` (`fastFailover`, its candidate loop)
- Test: `server/internal/subwatch/fast_test.go`

**Interfaces:**
- Consumes: `orderBest`, `refFor` (Task 3); `ssServer` (Task 3 test helper).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `server/internal/subwatch/fast_test.go`:

```go
// A Shadowsocks server listed before the VLESS backup, and faster, is still
// tried after it: the fast path takes the user's group first.
func TestFastFailover_TriesTheUsersGroupFirst(t *testing.T) {
	s := newFastFixture(t)
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	s.f.subs[0].Servers = []vpnconfig.Server{oslo, ssServer("Shadow", "198.51.100.40"), backup}
	s.setHealth()
	for _, key := range endpoint.Keys(ssServer("Shadow", "198.51.100.40")) {
		for _, endpoints := range []map[string]watchdapi.EndpointState{s.h.cached.Endpoints, s.h.fresh.Endpoints} {
			st := endpoints[key]
			st.LatencyMS = 5
			endpoints[key] = st
		}
	}
	s.passThenMiss(time.Second)
	s.w.Tick(context.Background())
	if len(s.written) == 0 || s.written[0].Name != "Backup" {
		t.Fatalf("written %v, want Backup first", healthLabels(s.written))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd server && go test ./internal/subwatch/ -run TestFastFailover_TriesTheUsersGroupFirst`
Expected: FAIL. Shadow is written first: the old order is the walk's list order.

- [ ] **Step 3: Implement**

In `server/internal/subwatch/fast.go`, `fastFailover`, replace

```go
	for _, s := range endpoint.PerAddress(order) {
```

with

```go
	// The alive copies in the user's group and subscription first, by latency
	// (orderBest): a server of another group can answer a check and carry
	// nothing.
	ref := refFor(cfg, vpnconfig.AllServers(subs))
	for _, s := range orderBest(endpoint.PerAddress(order), ref, cached, true) {
```

`cached` is already known to be in `StateOK`. The loop still validates each candidate's key and takes only alive ones.

- [ ] **Step 4: Run the fast-path tests**

Run: `cd server && go test ./internal/subwatch/ -run 'TestFast' -v 2>&1 | tail -20`
Expected: PASS. A test that pinned the old candidate order fails if it relied on list order across groups or latencies. Update it to spec 4.5 and note it in the report. Stop on any other failure.

- [ ] **Step 5: Run the package, then commit**

Run: `cd server && go test ./internal/subwatch/`
Expected: `ok`.

```bash
git add server/internal/subwatch/fast.go server/internal/subwatch/fast_test.go <each test file you changed>
git commit -m "feat(subwatch): the fast path tries the user's group first"
```

---

### Task 5: Keep Xray on a working server, with or without clients

**Files:**
- Create: `server/internal/subwatch/keeper.go`
- Create: `server/internal/subwatch/keeper_test.go`
- Modify: `server/internal/subwatch/watch.go` (`Watch` fields, `Tick`)

**Interfaces:**
- Consumes:
  - `classifyActive` (Task 2);
  - `orderBest`, `refFor`, `bestWalkOrder` (Task 3);
  - existing: `switcher` and `switcher.to`, `walkOrder`, `exactFastServer`, `fastFailover`, `checkReach`, `deadReason`, `returnAfterDeath`, `settled`, `label`, `subscriptionNames`, `serverID`, `activeID`, `endpoint.PerAddress`, `endpoint.DialKey`.
- Produces:
  - `func (w *Watch) keeps(cfg, subs int) bool`;
  - `func (w *Watch) keep(ctx, cfg)`;
  - `func (w *Watch) followActive(ctx, cfg) bool`;
  - `func (w *Watch) replaceActive(ctx, cfg, subs, dead *vpnconfig.Server, msg, was string)`;
  - `func (w *Watch) replaceDead(ctx, cfg, reason string)`;
  - `func (w *Watch) switchAmong(ctx, cfg, subs, candidates, message func(vpnconfig.Server) string) (switched, ended bool)`;
  - `func (w *Watch) keeperBackOff()`;
  - `func (w *Watch) say(msg string)`;
  - constants `KeeperRetry`, `KeeperRetryMax`;
  - message constants `msgFollowedMoved`, `msgFollowedChanged`, `msgReplacedGone`, `msgReplacedDead`, `msgNoWorking`, `msgBackToGroup` (Task 6 uses the last);
  - test helper `newKeeperFixture(t) *fastFixture`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/subwatch/keeper_test.go`:

```go
package subwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// newKeeperFixture is the fast fixture with nobody on Xray: the watch is not
// armed, and the keeper looks after the server active_server names - Oslo, which
// the monitor holds dead, while Backup is alive.
func newKeeperFixture(t *testing.T) *fastFixture {
	t.Helper()
	s := newFastFixture(t)
	s.f.cfg.Xray.Clients = nil
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.setHealth()
	return s
}

func (s *fastFixture) setLatency(name string, ms int64) {
	for _, sub := range cloneSubs(s.f.subs) {
		for _, server := range sub.Servers {
			if server.Name != name {
				continue
			}
			for _, key := range endpoint.Keys(server) {
				for _, endpoints := range []map[string]watchdapi.EndpointState{s.h.cached.Endpoints, s.h.fresh.Endpoints} {
					st := endpoints[key]
					st.LatencyMS = ms
					endpoints[key] = st
				}
			}
		}
	}
}

func writtenNames(servers []vpnconfig.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name + "@" + s.Address
	}
	return out
}

// The provider moved Oslo to a new address: the keeper follows it at once.
func TestKeeper_FollowsAMovedServer(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example"}) {
		t.Fatalf("written %v", got)
	}
	if a := s.f.cfg.Xray.ActiveServer; a == nil || a.Address != "oslo-2.example" || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("active %+v, preferred %+v", a, s.f.cfg.Xray.PreferredServer)
	}
	if want := []string{"Xray server Alpha / Oslo moved; Xray follows it"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// New keys behind the same name and address: the keeper writes them.
func TestKeeper_FollowsAServerChangedInPlace(t *testing.T) {
	s := newKeeperFixture(t)
	oslo := s.f.subs[0].Servers[0]
	oslo.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), "000000000010", "000000000011", 1))
	s.f.subs[0].Servers[0] = oslo
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; len(s.written) != 1 || a == nil || a.Identity != vpnconfig.IdentityMark(oslo) {
		t.Fatalf("written %v, active %+v", writtenNames(s.written), a)
	}
	if want := []string{"Xray server Alpha / Oslo changed; Xray follows it"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Oslo left its subscription: the best working server of its group replaces
// it - Backup, though the Shadowsocks server answers faster - and the vanished
// choice stays in preferred_server, its group with it.
func TestKeeper_ReplacesAServerThatLeftItsSubscription(t *testing.T) {
	s := newKeeperFixture(t)
	backup := s.f.subs[0].Servers[1]
	s.f.subs[0].Servers = []vpnconfig.Server{ssServer("Shadow", "198.51.100.40"), backup}
	s.setHealth()
	s.setLatency("Shadow", 5)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" || p.Group != "vless+tls" {
		t.Fatalf("preferred %+v, want the vanished Oslo", p)
	}
	if want := []string{"Xray server Alpha / Oslo left its subscription; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Without the monitor the keeper confirms the death the legacy way, then
// replaces the server.
func TestKeeper_ReplacesADeadServer(t *testing.T) {
	s := newKeeperFixture(t)
	s.w.Health = nil
	tickUntilDead(s.w, s.f)
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept for the return", p)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	if s.f.applies != 0 || s.f.cfg.Xray.Failover != nil {
		t.Fatalf("applies %d, failover %+v: nobody to move", s.f.applies, s.f.cfg.Xray.Failover)
	}
}

// With fresh evidence the fast path replaces the server within a tick, with
// its own message.
func TestKeeper_FastEvidenceSwitchesAtOnce(t *testing.T) {
	s := newKeeperFixture(t)
	s.passThenMiss(time.Second)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if want := []string{"Xray switched to server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// The entry Oslo moved to does not answer either: it is replaced as dead, and
// not tried again on the next tick.
func TestKeeper_AFollowedEntryThatDoesNotAnswerIsReplaced(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if len(s.written) > 0 && s.written[len(s.written)-1].Name == "Backup" {
			return nil
		}
		return errProbe
	}
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example", "Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	s.f.now = s.f.now.Add(ProbeInterval)
	s.w.Tick(context.Background())
	if len(s.written) != 2 {
		t.Fatalf("written %v; the next tick switched again", writtenNames(s.written))
	}
}

// Nothing works: one message, and the next attempt waits KeeperRetry instead
// of restarting Xray every 30 seconds.
func TestKeeper_NothingWorksSaysOnceAndBacksOff(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers = s.f.subs[0].Servers[1:] // Oslo left; Backup alone
	s.setHealth()
	s.w.Probe = func(context.Context, int) error { s.probes++; return errProbe }
	s.w.Tick(context.Background())
	if want := []string{"No working Xray server found; Xray stays on Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	tried := s.generateCalls
	tickFor(s.w, s.f, KeeperRetry-time.Minute)
	if s.generateCalls != tried {
		t.Fatalf("generate %d within the backoff, want %d", s.generateCalls, tried)
	}
	tickFor(s.w, s.f, 2*time.Minute)
	if len(s.f.notes) != 1 {
		t.Fatalf("notes %v; the run of failures is said once", s.f.notes)
	}
}

// A /stop while the keeper switches ends it: nothing restarts, nothing is said.
func TestKeeper_AStopEndsTheSwitchSilently(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	stopped := false
	s.w.Stopped = func() bool { return stopped }
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		stopped = true
		return generate(server, guard)
	}
	s.w.Tick(context.Background())
	if s.restarts != 0 || len(s.f.notes) != 0 || len(s.written) != 0 {
		t.Fatalf("restarts %d, notes %v, written %v after a stop", s.restarts, s.f.notes, writtenNames(s.written))
	}
}

// A selection the user commits while the keeper switches stands.
func TestKeeper_ASelectionDuringTheSwitchStands(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	backup := s.f.subs[0].Servers[1]
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, backup)
		return generate(server, guard)
	}
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Backup" || len(s.written) != 0 || len(s.f.notes) != 0 {
		t.Fatalf("active %+v, written %v, notes %v; the user's selection must stand", a, writtenNames(s.written), s.f.notes)
	}
}

// A record without a subscription is nothing the keeper can look after.
func TestKeeper_ARecordWithoutASubscriptionIsLeftAlone(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.cfg.Xray.ActiveServer.Subscription = ""
	s.w.Health = nil
	tickUntilDead(s.w, s.f)
	if s.generateCalls != 0 || len(s.f.notes) != 0 {
		t.Fatalf("generate %d, notes %v", s.generateCalls, s.f.notes)
	}
}

// Without xray installed the keeper does nothing.
func TestKeeper_NoXrayDoesNothing(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.h.cached.State = watchdapi.StateNoXray
	s.w.Tick(context.Background())
	if s.generateCalls != 0 {
		t.Fatalf("generate %d with no xray", s.generateCalls)
	}
}

// With clients on Xray the same actuality check runs before the probe.
func TestTick_WithClientsFollowsAMovedServer(t *testing.T) {
	s := newFastFixture(t)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example"}) || s.f.cfg.Xray.Failover != nil {
		t.Fatalf("written %v, failover %+v", got, s.f.cfg.Xray.Failover)
	}
	if want := []string{fmt.Sprintf(msgFollowedMoved, "Alpha / Oslo")}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}
```

`errProbe`, `tickUntilDead` and `tickFor` exist in the subwatch tests. If `errProbe` is defined elsewhere under another name, use that one.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/subwatch/ -run 'TestKeeper|TestTick_WithClientsFollowsAMovedServer'`
Expected: build failure, `undefined: msgFollowedMoved`, `undefined: KeeperRetry`.

- [ ] **Step 3: Implement the keeper**

Create `server/internal/subwatch/keeper.go`:

```go
package subwatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	msgFollowedMoved   = "Xray server %s moved; Xray follows it"
	msgFollowedChanged = "Xray server %s changed; Xray follows it"
	msgReplacedGone    = "Xray server %s left its subscription; Xray switched to %s"
	msgReplacedDead    = "Xray server %s is not answering; Xray switched to %s"
	msgNoWorking       = "No working Xray server found; Xray stays on %s"
	msgBackToGroup     = "Xray back on a %s server %s"
)

const (
	// KeeperRetry is the wait after a run of switches none of which worked,
	// doubling up to KeeperRetryMax: every candidate costs an Xray restart, and
	// back-to-back runs would restart Xray every tick.
	KeeperRetry    = 10 * time.Minute
	KeeperRetryMax = 30 * time.Minute
)

// keeps reports whether a tick of a watch that is not armed keeps Xray on a
// working server (spec 4.1): active_server names a server of a subscription,
// the watch can switch, and xray is installed. Nobody need be on Xray: a
// client back from a pause, or the bot's SOCKS path to Telegram, finds it
// working.
func (w *Watch) keeps(cfg *vpnconfig.VPNDirectorConfig, subs int) bool {
	if subs == 0 || cfg == nil || cfg.Xray.ActiveServer == nil || cfg.Xray.ActiveServer.Subscription == "" ||
		w.Generate == nil || w.Probe == nil {
		return false
	}
	return w.Health == nil || w.Health.Evidence().State != watchdapi.StateNoXray
}

// keep is a tick in keeper mode. There is nobody to move into a tunnel and no
// wave to run - the periodic refresh keeps the lists current - so a server
// that moved, left its subscription or died is replaced at once. A death is
// confirmed by the armed tick's rules (spec 4.4): the fast path's evidence,
// the unreachable streak, or DeadAfter.
func (w *Watch) keep(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.failSince.IsZero() && w.followActive(ctx, cfg) {
		return
	}
	w.setStatusAction("probe")
	err := w.Probe(ctx, w.socksPort(cfg))
	if w.mutationEnded(ctx) {
		return
	}
	if err == nil {
		w.probeOKAt = w.Now()
		w.probeOKActive = activeID(cfg.Xray.ActiveServer)
		w.resetFail()
		w.keeperFailNoted = false
		w.lastRouteKind, w.lastImportKind = noteNone, noteNone
		w.maybeReturn(ctx, cfg)
		return
	}
	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
		w.failSeq = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
	}
	if now.Before(w.keeperNotBefore) {
		return
	}
	attempt := w.fastFailover(ctx, cfg)
	switch attempt.Outcome {
	case fastSwitched:
		return
	case fastCanceled:
		w.resetFail()
		return
	case fastFallback:
		w.replaceDead(ctx, attempt.Config, "monitor")
		return
	}
	cfg = attempt.Config
	if now.Sub(w.failSince) < DeadAfter {
		w.checkReach(ctx, cfg)
	}
	if reason := w.deadReason(now); reason != "" {
		w.replaceDead(ctx, cfg, reason)
	}
}

// followActive is the actuality check (spec 4.2), made between episodes in
// both modes. A server that changed in place or moved is followed; one whose
// new entry does not answer either, and one that left its subscription, are
// replaced. It reports whether it acted - switched, or tried and backed off -
// and the tick then ends.
func (w *Watch) followActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if cfg == nil || cfg.Xray.ActiveServer == nil || w.Generate == nil || w.Probe == nil ||
		w.Now().Before(w.keeperNotBefore) || w.mutationEnded(ctx) {
		return false
	}
	subs, err := w.loadSubscriptions()
	if err != nil || len(subs) == 0 || w.mutationEnded(ctx) {
		return false
	}
	state, entry := classifyActive(subs, cfg.Xray.ActiveServer)
	if state == actualUnknown || state == actualCurrent {
		return false
	}
	a := cfg.Xray.ActiveServer
	was := label(subscriptionNames(subs), a.Subscription, a.Name)
	if state == actualGone {
		slog.Info("The Xray server left its subscription; replacing it", "server", was)
		w.replaceActive(ctx, cfg, subs, nil, msgReplacedGone, was)
		return true
	}
	msg := msgFollowedMoved
	if state == actualChanged {
		msg = msgFollowedChanged
	}
	slog.Info("The Xray server moved or changed; following it", "server", was, "changed", state == actualChanged)
	switched, ended := w.switchAmong(ctx, cfg, subs, endpoint.PerAddress([]vpnconfig.Server{entry}), func(vpnconfig.Server) string {
		return fmt.Sprintf(msg, was)
	})
	if switched || ended {
		return true
	}
	// The entry it moved to does not answer either: as far as Xray goes, the
	// server died.
	w.replaceActive(ctx, cfg, subs, &entry, msgReplacedDead, was)
	return true
}

// replaceDead replaces the server active_server names once its death is
// confirmed: nobody to move, so no tunnel and no wave.
func (w *Watch) replaceDead(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, reason string) {
	if cfg == nil || cfg.Xray.ActiveServer == nil {
		return
	}
	subs, err := w.loadSubscriptions()
	if err != nil || len(subs) == 0 || w.mutationEnded(ctx) {
		return
	}
	// The returns settle once per death, as failOutbound settles them.
	if !w.returnDeath.Equal(w.failSince) {
		w.returnDeath = w.failSince
		w.returnAfterDeath()
	}
	a := cfg.Xray.ActiveServer
	was := label(subscriptionNames(subs), a.Subscription, a.Name)
	slog.Info("Xray outbound declared dead; replacing the server", "server", was, "reason", reason)
	var dead *vpnconfig.Server
	if s, ok := exactFastServer(subs, a); ok {
		dead = &s
	}
	w.replaceActive(ctx, cfg, subs, dead, msgReplacedDead, was)
}

// replaceActive switches Xray to the best working server (spec 4.3). It reads
// the config again first: a follow that failed has written a record since. It
// leaves out dead - every copy of it, and every copy that dials the same
// connection (endpoint.DialKey), which dies with it. It sends msg with was,
// the server replaced, and the server found. With none working it says so once
// a run and backs the next attempt off. RecordWalkedServer keeps the user's
// choice in preferred_server for the return.
func (w *Watch) replaceActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, dead *vpnconfig.Server, msg, was string) {
	if w.LoadVPN != nil {
		if latest, err := w.LoadVPN(); err == nil && latest != nil {
			cfg = latest
		}
	}
	if w.mutationEnded(ctx) || cfg.Xray.ActiveServer == nil {
		return
	}
	chosen := cfg.Xray.PreferredServer
	if chosen == nil {
		chosen = cfg.Xray.ActiveServer
	}
	order, _ := walkOrder(subs, chosen)
	deadKeys := map[string]bool{}
	if dead != nil {
		for _, c := range endpoint.PerAddress([]vpnconfig.Server{*dead}) {
			if key := endpoint.DialKey(c); key != "" {
				deadKeys[key] = true
			}
		}
	}
	var copies []vpnconfig.Server
	for _, c := range endpoint.PerAddress(order) {
		if dead != nil && serverID(c) == serverID(*dead) {
			continue
		}
		if key := endpoint.DialKey(c); key != "" && deadKeys[key] {
			continue
		}
		copies = append(copies, c)
	}
	names := subscriptionNames(subs)
	ref := refFor(cfg, vpnconfig.AllServers(subs))
	switched, ended := w.switchAmong(ctx, cfg, subs, w.bestWalkOrder(copies, cfg, ref), func(c vpnconfig.Server) string {
		return fmt.Sprintf(msg, was, label(names, c.Subscription, c.Name))
	})
	if switched || ended {
		return
	}
	w.keeperBackOff()
	if w.keeperFailNoted {
		return
	}
	w.keeperFailNoted = true
	stays := was
	if w.LoadVPN != nil {
		if latest, err := w.LoadVPN(); err == nil && latest != nil && latest.Xray.ActiveServer != nil {
			stays = label(names, latest.Xray.ActiveServer.Subscription, latest.Xray.ActiveServer.Name)
		}
	}
	slog.Warn("No working Xray server found", "stays", stays)
	w.say(fmt.Sprintf(msgNoWorking, stays))
}

// switchAmong switches Xray to the first of candidates the SOCKS probe finds
// working, through the walk's guarded write (switcher.to), and sends message
// for it. It skips a copy whose endpoint.DialKey it tried already, and the
// copies of a subscription deleted meanwhile. ended is a stop, a closed gate
// or a selection the user made meanwhile: nothing more may be written.
func (w *Watch) switchAmong(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, candidates []vpnconfig.Server, message func(vpnconfig.Server) string) (switched, ended bool) {
	links := make(map[string]string, len(subs))
	for _, s := range subs {
		links[s.ID] = s.URL
	}
	before := cfg.Xray.ActiveServer
	sw := &switcher{w: w, links: links, started: activeID(before), seq: vpnconfig.ActiveSeq(before), socks: w.socksPort(cfg)}
	seen, gone := map[string]bool{}, map[string]bool{}
	w.setStatusAction("switch")
	for _, c := range candidates {
		if gone[c.Subscription] {
			continue
		}
		if key := endpoint.DialKey(c); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		live, stop, isGone := sw.to(ctx, c, true)
		if stop {
			return false, true
		}
		if isGone {
			gone[c.Subscription] = true
			continue
		}
		if !live {
			continue
		}
		// A stop or a selection can land while the probe waits; the walk makes
		// the same look before it announces.
		if w.mutationEnded(ctx) || w.endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq)) {
			return false, true
		}
		picked := c
		w.lastPicked = &picked
		w.settled()
		w.lastRouteKind, w.lastImportKind = noteNone, noteNone
		w.keeperRetry, w.keeperNotBefore, w.keeperFailNoted = 0, time.Time{}, false
		slog.Info("Xray switched to a working server", "server", c.Name, "ips", c.IPs)
		w.say(message(c))
		return true, false
	}
	return false, w.mutationEnded(ctx)
}

// keeperBackOff holds the next attempt back after a run of switches none of
// which worked: KeeperRetry, then twice the last wait, KeeperRetryMax at most.
func (w *Watch) keeperBackOff() {
	if w.keeperRetry == 0 {
		w.keeperRetry = KeeperRetry
	} else {
		w.keeperRetry = min(2*w.keeperRetry, KeeperRetryMax)
	}
	w.keeperNotBefore = w.Now().Add(w.keeperRetry)
	slog.Info("Next attempt to find a working Xray server backed off", "after", w.keeperRetry)
}

// say sends msg as it stands. Each keeper switch is news: notify's
// one-per-kind bookkeeping would swallow the second of two switches alike.
// Like notify it lets go of mu for the send.
func (w *Watch) say(msg string) {
	if w.mutationAllowed() != nil || w.outboundAllowedNow() != nil || w.Notify == nil {
		return
	}
	n := w.Notify
	w.mu.Unlock()
	n(msg)
	w.mu.Lock()
}
```

- [ ] **Step 4: Wire it into `Tick` and the `Watch` type**

In `server/internal/subwatch/watch.go`:

Add to `Watch`, after `lastPicked`:

```go
	keeperNotBefore   time.Time                            // no keeper switch before this: the backoff after a run that found nothing working
	keeperRetry       time.Duration                        // the current keeper backoff; zero before any
	keeperFailNoted   bool                                 // "No working Xray server found" sent for this run of failures
```

In `Tick`, replace

```go
	if !vpnconfig.Armed(cfg, subs) && !w.pendingApply {
		w.resetFail()
		return
	}
```

with

```go
	// A watch with nobody on Xray still keeps Xray on a working server
	// (keeper mode, spec 4.1); one without a server to keep does nothing.
	armed := vpnconfig.Armed(cfg, subs) || w.pendingApply
	keeper := !armed && w.keeps(cfg, subs)
	if !armed && !keeper {
		w.resetFail()
		return
	}
```

After the `defer` that follows `ctx, endWatch := w.cancelOnStop(ctx)`, and before `if !w.reconciled {`, add:

```go
	if keeper {
		w.keep(ctx, cfg)
		return
	}
```

After

```go
	// Pending intent finishes an existing restore, not a failover of its own.
	if subs == 0 || len(vpnconfig.EffectiveXrayClients(cfg)) == 0 {
		w.resetFail()
		return
	}
```

add:

```go
	// Between episodes the selected server is checked against its list: one
	// that moved, changed or left it is followed or replaced before Xray dies.
	if w.failSince.IsZero() && w.followActive(ctx, cfg) {
		return
	}
```

In the armed probe-success block, after `w.lastImportKind = noteNone`, add `w.keeperFailNoted = false`.

- [ ] **Step 5: Run the new tests**

Run: `cd server && gofmt -l ./internal/subwatch && go test ./internal/subwatch/ -run 'TestKeeper|TestTick_WithClientsFollowsAMovedServer' -v 2>&1 | tail -30`
Expected: no gofmt output; PASS.

- [ ] **Step 6: Run the package, under -race too**

Run: `cd server && go test ./internal/subwatch/ && go test -race -count=1 ./internal/subwatch/`
Expected: `ok` twice. An existing test may now fail because the keeper acts: an unarmed config whose `active_server` names a subscription, or an armed one whose record no longer matches its list. Decide by the test's subject:
- If the test is about something else, give its fixture what keeps the old premise: effective clients, a record that matches the list, or a record without a subscription.
- If the test asserts that nothing happens where the spec now says the keeper acts, update it to the spec.

List every such test and the reason in the report. Stop on any other failure.

- [ ] **Step 7: Commit**

```bash
git add server/internal/subwatch/keeper.go server/internal/subwatch/keeper_test.go server/internal/subwatch/watch.go <each test file you changed in Step 6>
git commit -m "feat(subwatch): keep Xray on a working server with or without clients"
```

---

### Task 6: Return after five minutes alive, and back to the user's group

**Files:**
- Modify: `server/internal/subwatch/return.go` (`ReturnAlive`, `aliveFor`, `maybeReturn`, `tryReturn`)
- Test: `server/internal/subwatch/keeper_test.go` (append)

**Interfaces:**
- Consumes:
  - `walkEvidence`, `orderBest`, `bestRef` (Task 3);
  - `msgBackToGroup`, `newKeeperFixture` (Task 5);
  - `vpnconfig.Server.Group` (Task 1).
- Produces:
  - `const ReturnAlive = 5 * time.Minute`;
  - `func aliveFor(copies []vpnconfig.Server, ev monitor.Evidence, now time.Time, d time.Duration) []vpnconfig.Server`;
  - `tryReturn` gains a last parameter `group string`: "" for the return to the preferred server, the group's name for the return to the group.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/subwatch/keeper_test.go`:

```go
// setStates replaces the monitor's view: every endpoint of each named server
// takes the state given.
func (s *fastFixture) setStates(states map[string]watchdapi.EndpointState) {
	e := monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{}}
	for _, sub := range cloneSubs(s.f.subs) {
		for _, server := range sub.Servers {
			if st, ok := states[server.Name]; ok {
				for _, key := range endpoint.Keys(server) {
					e.Endpoints[key] = st
				}
			}
		}
	}
	s.h = &fastHealth{cached: e, fresh: copyFastEvidence(e, nil), invalid: map[string]bool{}}
	s.w.Health = s.h
}

func aliveSince(since time.Time, ms int64) watchdapi.EndpointState {
	return watchdapi.EndpointState{Status: watchdapi.StatusAlive, LatencyMS: ms, Since: since, CheckedAt: since}
}

// newReturnKeeper is the keeper fixture running Backup after Oslo, the user's
// choice, died: Xray works, and the return looks every ReturnCheck.
func newReturnKeeper(t *testing.T) *fastFixture {
	t.Helper()
	s := newKeeperFixture(t)
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	s.f.cfg.Xray.PreferredServer = vpnconfig.NewActiveServer(oslo)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, backup)
	s.w.Probe = func(context.Context, int) error { s.probes++; return nil }
	return s
}

// The monitor has held Oslo alive for six minutes: Xray goes back to it.
func TestMaybeReturn_AfterFiveMinutesAliveReturns(t *testing.T) {
	s := newReturnKeeper(t)
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo": aliveSince(s.f.now.Add(-6*time.Minute), 40), "Backup": aliveSince(s.f.now.Add(-time.Hour), 17),
	})
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo.example"}) || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("written %v, preferred %+v", got, s.f.cfg.Xray.PreferredServer)
	}
	if want := []string{"Xray back on the preferred server Alpha / Oslo"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Alive for two minutes is not yet a server to go back to.
func TestMaybeReturn_TwoMinutesAliveIsNotEnough(t *testing.T) {
	s := newReturnKeeper(t)
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo": aliveSince(s.f.now.Add(-2*time.Minute), 40), "Backup": aliveSince(s.f.now.Add(-time.Hour), 17),
	})
	s.w.Tick(context.Background())
	if len(s.written) != 0 {
		t.Fatalf("written %v", writtenNames(s.written))
	}
}

// Xray runs a Shadowsocks server while Oslo, the user's vless+tls choice, is
// dead: once Backup, of Oslo's group, has been alive five minutes, Xray goes
// there, and Oslo stays the choice to return to.
func TestMaybeReturn_BackToTheUsersGroup(t *testing.T) {
	s := newReturnKeeper(t)
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	shadow := ssServer("Shadow", "198.51.100.40")
	s.f.subs[0].Servers = []vpnconfig.Server{oslo, shadow, backup}
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, shadow)
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo":   {Status: watchdapi.StatusDead, Since: s.f.now.Add(-time.Hour)},
		"Shadow": aliveSince(s.f.now.Add(-time.Hour), 5),
		"Backup": aliveSince(s.f.now.Add(-6*time.Minute), 17),
	})
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept", p)
	}
	if want := []string{"Xray back on a vless+tls server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}
```

Add `github.com/zinin/vpn-director/server/internal/monitor` to the imports of `keeper_test.go`.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/subwatch/ -run 'TestMaybeReturn_AfterFiveMinutesAliveReturns|TestMaybeReturn_TwoMinutesAliveIsNotEnough|TestMaybeReturn_BackToTheUsersGroup'`
Expected:
- `TestMaybeReturn_AfterFiveMinutesAliveReturns` and `TestMaybeReturn_BackToTheUsersGroup` FAIL. The fixture has no `Reachable`, so today's return never looks, and there is no return to the group.
- `TestMaybeReturn_TwoMinutesAliveIsNotEnough` passes already, for the wrong reason. Keep it: it pins the threshold once the look exists.

- [ ] **Step 3: Implement**

In `server/internal/subwatch/return.go`, add to the constants:

```go
	// ReturnAlive is how long the monitor has to hold a server alive before
	// Xray goes back to it - to the preferred server, or to the user's group:
	// a server that has just come back is often gone again within minutes.
	ReturnAlive = 5 * time.Minute
```

Add (imports: `sort`, `monitor`, `watchdapi`):

```go
// aliveFor is the copies the monitor has held alive for at least d at now,
// fastest first, ties in the order given.
func aliveFor(copies []vpnconfig.Server, ev monitor.Evidence, now time.Time, d time.Duration) []vpnconfig.Server {
	var out []vpnconfig.Server
	for _, c := range copies {
		st, ok := ev.Endpoints[endpoint.Key(c)]
		if ok && st.Status == watchdapi.StatusAlive && !st.Since.After(now.Add(-d)) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return ev.Endpoints[endpoint.Key(out[i])].LatencyMS < ev.Endpoints[endpoint.Key(out[j])].LatencyMS
	})
	return out
}
```

In `maybeReturn`:

- Replace `if w.LoadSubscriptions == nil || w.Reachable == nil || w.Generate == nil {` with `if w.LoadSubscriptions == nil || w.Generate == nil {`.
- Replace everything from `servers := vpnconfig.AllServers(subs)` to the end of the function with:

```go
	servers := vpnconfig.AllServers(subs)
	// With fresh evidence the monitor's look is the look: alive for
	// ReturnAlive. Without it, the TCP look as before - a server no TCP dial
	// can see is switched to on its turn, the attempt the only check there is.
	ev, fresh := w.walkEvidence(servers, cfg)
	var candidates []vpnconfig.Server
	if i := chosenIndex(servers, preferred); i >= 0 {
		copies := endpoint.PerAddress(servers[i : i+1])
		switch {
		case fresh:
			candidates = aliveFor(copies, ev, now, ReturnAlive)
		case w.Reachable == nil:
		case tcpChecked(servers[i]):
			candidates = w.reachable(ctx, dialable(copies))
		default:
			candidates = dialable(copies)
		}
	}
	if ctx.Err() != nil || w.mutationEnded(ctx) {
		return
	}
	if len(candidates) > 0 {
		w.tryReturn(ctx, cfg, subs, servers, candidates, "")
		return
	}
	// The preferred server is not back, but Xray may run another group than
	// the user's: a server of that group alive for ReturnAlive is the nearer
	// choice. preferred_server stays, and the return to it still comes.
	activeGroup := active.Group
	if activeGroup == "" {
		if j := chosenIndex(servers, active); j >= 0 {
			activeGroup = servers[j].Group()
		}
	}
	if !fresh || preferred.Group == "" || activeGroup == "" || activeGroup == preferred.Group {
		return
	}
	ref := bestRef{Subscription: preferred.Subscription, Group: preferred.Group}
	var group []vpnconfig.Server
	for _, c := range aliveFor(endpoint.PerAddress(servers), ev, now, ReturnAlive) {
		if c.Group() == ref.Group {
			group = append(group, c)
		}
	}
	if len(group) > 0 {
		w.tryReturn(ctx, cfg, subs, servers, orderBest(group, ref, ev, true), ref.Group)
	}
```

The function must still read `now := w.Now()` before `w.returnNotBefore = now.Add(ReturnCheck)`, as today; keep that.

Change `tryReturn`'s signature to

```go
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, servers, candidates []vpnconfig.Server, group string) {
```

and add to its doc comment: "With group set, the candidates are servers of the user's group, not the preferred server: the messages say so, and preferred_server stays." Inside it:

- Replace `slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)` with

```go
	if group == "" {
		slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)
	} else {
		slog.Info("Returning to a server of the preferred group", "group", group, "server", candidates[0].Name, "from", before.Name)
	}
```

- Replace the success announcement

```go
			slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			w.lastReturn = w.Now()
			w.returnSeq = sw.seq
			w.notify(noteReturned, fmt.Sprintf(msgReturned, label(names, c.Subscription, c.Name)))
			return
```

with

```go
			w.lastPicked = &c
			w.lastReturn = w.Now()
			w.returnSeq = sw.seq
			if group == "" {
				slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
				w.notify(noteReturned, fmt.Sprintf(msgReturned, label(names, c.Subscription, c.Name)))
			} else {
				slog.Info("Xray returned to a server of the preferred group", "group", group, "server", c.Name, "ips", c.IPs)
				w.say(fmt.Sprintf(msgBackToGroup, group, label(names, c.Subscription, c.Name)))
			}
			return
```

`maybeReturn` is `tryReturn`'s only caller. Check with `grep -n "tryReturn(" server/internal/subwatch/*.go`.

- [ ] **Step 4: Run the new tests**

Run: `cd server && gofmt -l ./internal/subwatch && go test ./internal/subwatch/ -run 'TestMaybeReturn_' -v 2>&1 | tail -20`
Expected: no gofmt output; PASS.

- [ ] **Step 5: Run the package, under -race too**

Run: `cd server && go test ./internal/subwatch/ && go test -race -count=1 ./internal/subwatch/`
Expected: `ok` twice. A return test may fail when it sets `w.Health` to OK evidence and expects the old TCP-based return, e.g. through `orderTestEvidence`, whose `Since` is "now". With fresh evidence the look is now "alive for `ReturnAlive`". Update such a test to that rule: give the preferred server's endpoints an older `Since`, or drop `Health` when the test is about the TCP look. Note each change in the report. Stop on any other failure.

- [ ] **Step 6: Commit**

```bash
git add server/internal/subwatch/return.go server/internal/subwatch/keeper_test.go <each test file you changed in Step 5>
git commit -m "feat(subwatch): return after five minutes alive, and back to the user's group"
```

---

### Task 7: Documentation

**Files:**
- Modify: `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`, `README.md`, `README.ru.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: the names of Tasks 1–6.
- Produces: docs that describe the shipped behaviour.

- [ ] **Step 1: watchd.md**

In `.claude/rules/watchd.md`, insert this section right before `## Durable notifications`:

```markdown
## Keeping Xray on a working server

The watch keeps Xray on a working server whether or not anybody is on Xray. Armed - effective
clients - it works as described above. Not armed, but with `xray.active_server` naming a server of
a subscription, its tick runs in keeper mode (`subwatch/keeper.go`): it probes Xray through SOCKS
every 30 s and confirms a death by the same rules - the fast path's evidence, the unreachable
streak, `DeadAfter` - but moves no client and runs no wave, since the periodic refresh keeps the
lists current. Nothing happens while VPN Director is stopped, the gate is closed, no server is
selected, or the monitor reports `no_xray`.

Between episodes, in both modes, the tick checks the selected server against its list
(`classifyActive`). `active_server` carries `identity` (`vpnconfig.IdentityMark`: the first 16 hex
digits of a SHA-256 of the loose identity) and `group` (`Server.Group`: protocol and security, such
as `vless+reality`). Every Go writer fills them in through `vpnconfig.RecordActiveServer`. An entry
of the record's name, address and port with the same mark is current, one with another mark changed
in place, a single other entry of the name means the server moved, anything else that it left its
list. A record without a mark - `configure.sh`'s, an older daemon's - is checked by name, address
and port. A changed or moved server is followed; one that left, or whose new entry does not answer
either, is replaced; so is one confirmed dead.

A replacement tries the candidates in `orderBest` order (`subwatch/best.go`), the same order the
fast path and the walk use. With valid monitor evidence the alive servers go first in tiers: the
group of the user's choice (`preferred_server`, else `active_server`) in its subscription, the
group in the other subscriptions, other groups in the subscription, other groups elsewhere, each
by latency. The rest follow, the group first, each part in the walk's order (`OwnFirst`, then one
server of each subscription in turn). Rejected endpoints are skipped, and so is every copy that
dials the dead server's connection. Each candidate goes through the walk's guarded write
(`switcher.to`), `restart xray-process`, 3 s and the SOCKS probe; a stop or a selection made
meanwhile ends the attempt. A run in which none works sends "No working Xray server found" once
and waits `KeeperRetry` (10 minutes, doubling to `KeeperRetryMax`, 30) before the next. The watch
never switches between alive servers for latency alone.

`RecordWalkedServer` keeps the user's choice in `preferred_server`, a vanished one included, so its
group stays the reference across a restart. The return looks every `ReturnCheck`: with valid
evidence it goes back once the monitor has held the preferred server alive for `ReturnAlive`
(5 minutes); without evidence the TCP look applies. While Xray runs another group and the
preferred server is not back, a server of the user's group alive for `ReturnAlive` is switched to
the same way, and `preferred_server` stays. Every switch sends one message, past `notify`'s
one-per-kind deduplication: `Xray server <s> moved; Xray follows it`, `… changed; Xray follows
it`, `… left its subscription; Xray switched to <s>`, `… is not answering; Xray switched to <s>`,
`Xray back on a <group> server <s>`; the fast path and the return to the preferred server keep
their messages. No line names a link.
```

In the same file, section "Subscription watch and failover", replace the sentence that starts `The walk keeps \`OwnFirst=3\`, then round-robins subscriptions;` and ends `those Xray rejected in the current generation are skipped;` with:

```
The walk and the fast path order their candidates with `orderBest` (see "Keeping Xray on a working
server"): with valid monitor evidence the alive servers of the user's group and subscription first,
by latency; otherwise the walk's order, `OwnFirst=3` and then one server of each subscription in
turn, the user's group first; endpoints Xray rejected in the current generation are skipped;
```

In the Layout block, replace

```
server/internal/subwatch/         # fast/legacy failover, walk, preferred return, pending restore, periodic refresh
```

with

```
server/internal/subwatch/         # fast/legacy failover, walk, preferred return, pending restore, periodic refresh, keeper
```

- [ ] **Step 2: telegram-bot.md**

In `.claude/rules/telegram-bot.md`, in the subwatch tree, after the line for `reach.go`, add:

```
│   │   ├── keeper.go         # Keeper mode and the actuality check: follow, replace, back off
│   │   ├── best.go           # orderBest: candidates by group, subscription and latency
```

In the long paragraph of "Subscription watch (owned by watchd)", right after the sentence that ends `…the order starts A5, A1, A2, B1, A3, B2, A4, B3.`, insert:

```
With valid monitor evidence the alive servers go first, the user's group and subscription first and
each tier by latency, and the rest keep this order with the user's group first (`orderBest`,
`watchd.md`).
```

In the return paragraph, replace `the watch looks every 5 minutes (\`ReturnCheck\`) whether the preferred server's entry in its subscription accepts TCP;` with:

```
the watch looks every 5 minutes (`ReturnCheck`) whether the preferred server's entry in its subscription accepts TCP - with valid monitor evidence, whether the monitor has held it alive for 5 minutes (`ReturnAlive`);
```

In the sentence `Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored, back on the preferred server)`, add after the closing parenthesis: `, and each switch of the keeper one more (\`watchd.md\`)`.

- [ ] **Step 3: webui.md**

In `.claude/rules/webui.md`, find the paragraph that describes `xray.active_server` as written by a Select (`grep -n "active_server" .claude/rules/webui.md`). Add this sentence to it:

```
The record also carries `identity` (a hash of the server's outbound without the picks a panel makes
at random) and `group` (its protocol and security): watchd follows a server that moved by them and
picks a replacement of the user's group (`watchd.md`).
```

- [ ] **Step 4: READMEs and CLAUDE.md**

In `README.md`, right after the paragraph that starts `Every \`subscription_refresh\``, add:

```
If the selected server moves, changes or leaves its subscription, or stops working, the daemon switches Xray: to the same server at its new address, or else to the best working server - the same kind (protocol and security) first, its own subscription first, the fastest first - and back to your choice once it has worked for 5 minutes. It does so with or without clients on Xray, and says so in Telegram.
```

In `README.ru.md`, after the paragraph that starts `Раз в \`subscription_refresh\``, add:

```
Если выбранный сервер переехал, изменился, пропал из подписки или перестал работать, демон переключает Xray: на тот же сервер по новому адресу, а иначе — на лучший рабочий сервер (сначала того же типа — протокол и security, — сначала из той же подписки, сначала самый быстрый) и возвращается к вашему выбору, когда тот проработает 5 минут. Это происходит и с клиентами на Xray, и без них, и сообщается в Telegram.
```

In `CLAUDE.md`, replace

```
| `server/internal/subwatch/` | Watchd's SOCKS probe, subscription walk, failover, preferred return, pending restore and periodic subscription refresh |
```

with

```
| `server/internal/subwatch/` | Watchd's SOCKS probe, subscription walk, failover, preferred return, pending restore, periodic subscription refresh, and the keeper that holds Xray on a working server |
```

- [ ] **Step 5: Check**

Run: `grep -c "Keeping Xray on a working server" .claude/rules/watchd.md; grep -c "keeper.go" .claude/rules/telegram-bot.md; grep -c "ReturnAlive" .claude/rules/watchd.md .claude/rules/telegram-bot.md; grep -c '"group"\|`group`' .claude/rules/webui.md; git diff --check`
Expected: `1` or more for each; no `git diff --check` output. Confirm that every identifier the new text names exists in `server/` (`grep -rn`).

- [ ] **Step 6: Commit**

```bash
git add .claude/rules/watchd.md .claude/rules/telegram-bot.md .claude/rules/webui.md README.md README.ru.md CLAUDE.md
git commit -m "docs: keeping Xray on a working server"
```

---

### Task 8: Whole-branch verification

**Files:** none, unless a check fails.

**Interfaces:**
- Consumes: everything above.
- Produces: evidence that the branch builds and its suites pass.

- [ ] **Step 1: Format and vet**

Run: `cd server && gofmt -l . && go vet ./...`
Expected: no output.

- [ ] **Step 2: The Go suite**

Run: `L="$(mktemp -d)/go-test.log"; make -C server test > "$L" 2>&1; echo exit=$?; grep -E '^(ok|FAIL)' "$L"`
Expected: `exit=0`, every package `ok`, no `FAIL`.

- [ ] **Step 3: Race detector**

Run: `cd server && go test -race -count=1 ./internal/subwatch/ ./internal/vpnconfig/ ./internal/service/ ./cmd/watchd/`
Expected: every package `ok`, no `DATA RACE`.

- [ ] **Step 4: The SPA**

Run: `cd web && npm test`
Expected: exit 0.

- [ ] **Step 5: Router builds**

Run: `cd server && D="$(mktemp -d)" && GOOS=linux GOARCH=arm64 go build ./cmd/watchd ./cmd/bot && GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -o "$D/watchd-mipsle" ./cmd/watchd && rm -rf "$D"`
Expected: exit 0, no output. Never delete anything under `server/`: `server/bot` and `server/webui` are the owner's own git-ignored binaries.

- [ ] **Step 6: Nothing outside the plan changed**

Run: `git diff --stat master...HEAD -- router/`
Expected: empty.

If a step fails, report it with the output: the fix goes to the task that owns the code and is reviewed like any change.
