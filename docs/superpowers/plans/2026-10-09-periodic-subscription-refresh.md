# Periodic Subscription Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `vpn-director-watchd` downloads every subscription with a link every 5 minutes by default and writes only what changed, so the monitor and the failover walk see the addresses a provider serves now, and a server that did not change keeps its monitor status.

**Architecture:** A new loop in `subwatch` (`(*Watch).StartRefresh`) downloads outside the watch's tick and publishes between ticks through a new `vpnconfig.PublishRefresh`, which merges the download with the file by server identity: the stored outbound without the REALITY fields panels pick at random. A paired server stays the stored record under the fresh name, so its endpoint key and monitor status survive; `active_server`, `preferred_server` and `pending_restore.active` follow a renamed server without moving `seq`. A manual refresh carries the records over renames the same way; the wave stays as it is.

**Tech Stack:** Go 1.25 (`server/`), Vue 3 SPA (`web/`), Markdown docs.

**Spec:** `docs/superpowers/specs/2026-10-09-periodic-subscription-refresh-design.md`

## Global Constraints

- `monitor.subscription_refresh`: a Go duration, `5m` when absent, `0` off; a value that does not parse, is negative or is below `1m` takes `5m` with a WARN. It does not depend on `monitor.enabled`.
- The first round starts `RefreshFirst` = 1 minute after watchd starts; each later round one interval after the previous round ended; a round that stands down is tried again `RefreshRetry` = 1 minute later.
- Each download is bounded by `subwatch.FetchTimeout` (3 minutes); a stop or a closed compatibility gate ends the downloads within `stopPoll` (1 s).
- A round stands down while `/tmp/vpn-director/stopped` exists, the gate refuses mutations, the watch's probe is failing (`failSince`), `pendingApply` is set, or `xray.failover` / `xray.pending_restore` exists.
- Publication happens only while holding `tickMu` (between ticks), one config-lock update per subscription.
- Server identity drops only the REALITY `serverName`, `shortId` and `spiderX` of every `realitySettings` object (an xhttp `downloadSettings` included), keys matched with `strings.EqualFold`; a plain TLS `serverName` stays part of the identity.
- A merged list equal to the file's (server by server, field by field, same order) with no recorded error writes nothing: neither the subscription file nor `vpn-director.json`.
- A rename moves `active_server`, `preferred_server` and `pending_restore.active` together; `seq` never moves.
- Unchanged: the wave (`subwatch` keeps calling `vpnconfig.RefreshSubscription`), `import_server_list.sh`, `configure.sh`, every `lib/*.sh`. The shell changes only in `router/opt/vpn-director/vpn-director.json.template`.
- The watchd socket contract does not change. The API field `refreshed` keeps its name.
- No subscription link in any log line. The periodic refresh sends no Telegram message.
- Comments follow the repository's style: full sentences that say why. No TODO, no placeholder.
- Go tests: `cd server && go test ./...` (the Makefile's `make -C server test` runs `go test -v ./...`). Concurrency code also runs under `-race`. SPA tests: `cd web && npm test`.

## Review Focus

1. A manual refresh writes the file between a round's download and its publication: the merge compares with the file as it is then, and a download that differs from it only in REALITY picks writes nothing. — Task 2, `TestPublishRefresh_MergesWithTheFileAsItIsThen`.
2. An operator rotates a REALITY key and keeps the name: the server comes in as a new one, and a record that named the old one is neither renamed nor cleared. — Task 2, `TestPublishRefresh_ARotatedKeyIsANewServer`.
3. The panel answers 200 with an HTML page or with no supported server: the round records the error once and keeps the list, and the next good round clears it. — Task 6, `TestRefreshRound_AnUnreadableAnswerIsRecordedAndTheNextRoundClearsIt`.
4. watchd shuts down in the middle of a round: nothing more is written and no error is recorded for the downloads it cut short. — Task 6, `TestRefreshRound_AShutdownWritesNothingMore`.
5. The compatibility gate closes during the downloads: the round drops them and records nothing. — Task 6, `TestRefreshRound_AGateThatClosesDropsTheDownloads`.

---

## File Structure

| File | Responsibility |
|---|---|
| `server/internal/vpnconfig/identity.go` (new) | `ServerIdentity`, `pairServers`: what a server is apart from REALITY picks, and pairing two lists by it |
| `server/internal/vpnconfig/refresh.go` (new) | `MergeRefresh`, `PublishRefresh`, `followRenames`, `RefreshResult`, `RecordRename`, `ErrNoServerResolved` |
| `server/internal/vpnconfig/subops.go` | `AddSubscription` of a saved link and the new `RefreshSubscriptionFollowingRenames` carry the records over renames |
| `server/internal/vpnconfig/vpnconfig.go` | `MonitorConfig.SubscriptionRefresh` |
| `server/internal/monitor/settings.go` | `SubscriptionRefreshFrom`, `DefaultSubscriptionRefresh`, `MinSubscriptionRefresh`; `SettingsFrom` warns about the key |
| `server/internal/subscription/resolve.go` | `Import.Listed`: every decoded server, with or without addresses |
| `server/internal/service/subfetch.go` | `fetchImport`, `SubscriptionFetcher.FetchList` |
| `server/internal/service/subscriptions.go` | a manual refresh goes through `RefreshSubscriptionFollowingRenames` |
| `server/internal/subwatch/refresh.go` (new) | the periodic refresh loop |
| `server/internal/subwatch/watch.go` | `Watch.FetchList`, `Watch.RefreshInterval`, `Watch.after` |
| `server/cmd/watchd/watch.go`, `runtime.go` | wiring and start of the loop |
| `server/internal/wizard/state.go` | the wizard finds a pick a refresh renamed |
| `server/internal/handler/subs.go`, `web/src/components/ServersTab.vue` | "changed 2 h ago", the Changed column |
| `router/opt/vpn-director/vpn-director.json.template`, `README.md`, `README.ru.md` | the new key |
| `.claude/rules/watchd.md`, `telegram-bot.md`, `webui.md`, `CLAUDE.md` | documentation |

---

### Task 1: Server identity and the merge

**Files:**
- Create: `server/internal/vpnconfig/identity.go`
- Create: `server/internal/vpnconfig/refresh.go`
- Test: `server/internal/vpnconfig/identity_test.go` (new), `server/internal/vpnconfig/refresh_test.go` (new), `server/internal/monitor/endpoints_test.go`

**Interfaces:**
- Consumes: `vpnconfig.Server` (`server/internal/vpnconfig/vpnconfig.go`).
- Produces:
  - `func ServerIdentity(s Server) string` — canonical JSON of the stored outbound without the REALITY picks; `""` for a record without an outbound or with one that does not parse.
  - `func pairServers(stored, fresh []Server) []int` — `pairs[i]` is the index in `stored` paired with `fresh[i]`, or `-1`.
  - `type RefreshMerge struct { Servers []Server; Added, Removed, Renamed, Readdressed int; pairs []int }`
  - `func MergeRefresh(stored, listed []Server) RefreshMerge`
  - Test helpers in package `vpnconfig`, used by Tasks 2 and 3: `realityOutbound(address, sni, sid string) json.RawMessage`, `reality(name, sni, sid string, ips ...string) Server` (a server on `de.example`), `trojanTo(address string) json.RawMessage`.

- [ ] **Step 1: Write the failing identity tests**

Create `server/internal/vpnconfig/identity_test.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// realityOutbound is a VLESS REALITY outbound on address, with the server
// name and the short id a panel picked for one download.
func realityOutbound(address, sni, sid string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":%q,"fingerprint":"chrome","publicKey":"pk-1","shortId":%q,"spiderX":"/"}}}`, address, sni, sid))
}

// reality is a server on de.example as an import stores it.
func reality(name, sni, sid string, ips ...string) Server {
	return Server{Name: name, Address: "de.example", Port: 443, IPs: ips, Outbound: realityOutbound("de.example", sni, sid)}
}

// trojanTo is a Trojan outbound on address.
func trojanTo(address string) json.RawMessage {
	return json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + address + `","port":443,"password":"p"}]}}`)
}

// 3x-ui and Marzban pick the REALITY server name and short id at random for
// every download: two downloads of one server are one server.
func TestServerIdentity_WhatAPanelPicksAtRandomDoesNotCount(t *testing.T) {
	a := ServerIdentity(reality("DE", "www.example.com", "aa11"))
	b := ServerIdentity(reality("DE 9GB", "example.com", "bb22"))
	if a == "" || a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

func TestServerIdentity_EverythingElseCounts(t *testing.T) {
	base := ServerIdentity(reality("DE", "www.example.com", "aa11"))
	key := reality("DE", "www.example.com", "aa11")
	key.Outbound = json.RawMessage(strings.Replace(string(key.Outbound), "pk-1", "pk-2", 1))
	moved := Server{Name: "DE", Outbound: realityOutbound("fr.example", "www.example.com", "aa11")}
	for what, got := range map[string]string{"public key": ServerIdentity(key), "address": ServerIdentity(moved)} {
		if got == base {
			t.Errorf("a server with another %s has the same identity", what)
		}
	}
	// On a CDN the TLS server name picks the backend: two names are two servers.
	tls := func(sni string) Server {
		return Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"cdn.example","port":443,"password":"p"}]},"streamSettings":{"security":"tls","tlsSettings":{"serverName":"` + sni + `"}}}`)}
	}
	if ServerIdentity(tls("a.example")) == ServerIdentity(tls("b.example")) {
		t.Error("two TLS server names have one identity")
	}
}

// Xray reads its config with encoding/json, which folds case: a pick spelled
// another way is still the pick, and the identity drops it all the same.
func TestServerIdentity_MatchesTheKeysAsXrayDoes(t *testing.T) {
	canonical := Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"realitySettings":{"publicKey":"pk-1","shortId":"aa11","serverName":"a.example","spiderX":"/a"}}}`)}
	folded := Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"realitySettings":{"publicKey":"pk-1","ShortID":"bb22","SERVERNAME":"b.example","Spiderx":"/b"}}}`)}
	if a, b := ServerIdentity(canonical), ServerIdentity(folded); a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

// An xhttp extra carries a whole download stream, REALITY picks and all.
func TestServerIdentity_LooksIntoTheDownloadStream(t *testing.T) {
	xhttp := func(sni, sid string) Server {
		return Server{Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"vless","streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"publicKey":"pk-1","serverName":"a.example","shortId":"aa11"},"xhttpSettings":{"extra":{"downloadSettings":{"address":"dl.example","port":443,"network":"xhttp","security":"reality","realitySettings":{"publicKey":"pk-2","serverName":%q,"shortId":%q}}}}}}`, sni, sid))}
	}
	if a, b := ServerIdentity(xhttp("x.example", "cc33")), ServerIdentity(xhttp("y.example", "dd44")); a == "" || a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

func TestServerIdentity_KeyOrderDoesNotCount(t *testing.T) {
	a := Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"fr.example","port":443,"password":"p"}]}}`)}
	b := Server{Outbound: json.RawMessage(`{ "settings": {"servers": [{"password": "p", "port": 443, "address": "fr.example"}]}, "protocol": "trojan" }`)}
	if ServerIdentity(a) != ServerIdentity(b) {
		t.Fatal("key order changed the identity")
	}
}

func TestServerIdentity_ARecordWithoutAnOutboundHasNone(t *testing.T) {
	legacy := Server{Name: "DE", Address: "de.example", Port: 443, UUID: "u-1", Security: "reality", ShortID: "aa11"}
	if id := ServerIdentity(legacy); id != "" {
		t.Fatalf("legacy record identity %q", id)
	}
	if id := ServerIdentity(Server{Outbound: json.RawMessage(`not json`)}); id != "" {
		t.Fatalf("unreadable outbound identity %q", id)
	}
}

func TestPairServers_EachTakesTheFirstFreeStoredTwin(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, Outbound: trojanTo("fr.example")}
	stored := []Server{reality("DE-1", "a.example", "aa11"), reality("DE-2", "a.example", "bb22"), fr}
	fresh := []Server{
		reality("DE-1", "b.example", "cc33"),
		reality("DE-2", "b.example", "dd44"),
		{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")},
		{Name: "Legacy", Address: "l.example", Port: 443},
	}
	if got := pairServers(stored, fresh); !reflect.DeepEqual(got, []int{0, 1, -1, -1}) {
		t.Fatalf("pairs %v, want [0 1 -1 -1]", got)
	}
}
```

- [ ] **Step 2: Run the identity tests to verify they fail**

Run: `cd server && go test ./internal/vpnconfig/ -run 'TestServerIdentity|TestPairServers' -v`
Expected: FAIL to compile with `undefined: ServerIdentity` and `undefined: pairServers`.

- [ ] **Step 3: Write the identity**

Create `server/internal/vpnconfig/identity.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"strings"
)

// realityPicks are the realitySettings keys a panel may fill at random on
// every request. 3x-ui and Marzban pick the server name from serverNames and
// the short id from shortIds anew for each download, and spiderX is the
// client's own path on the borrowed site. Any value the server lists works,
// so none of them tells one server from another.
var realityPicks = []string{"serverName", "shortId", "spiderX"}

// ServerIdentity is what s is apart from what a panel picks at random: its
// stored outbound without the realityPicks of any realitySettings in it, an
// xhttp downloadSettings included, as canonical JSON. Keys are matched as Xray
// matches them, folding case as strings.EqualFold does, so no spelling of a
// pick survives into the identity and the decoders' guarded keys need no new
// name. Everything else stays: address, port, protocol, credentials,
// transport, and the serverName of plain TLS, which picks the backend on a
// CDN. A record without an outbound, or with one that does not parse, has no
// identity: "" pairs with nothing.
func ServerIdentity(s Server) string {
	if len(s.Outbound) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(s.Outbound, &v); err != nil {
		return ""
	}
	dropRealityPicks(v)
	// Marshalling a map sorts its keys: key order and whitespace do not count.
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// dropRealityPicks removes the realityPicks from every realitySettings object
// in v, however deep.
func dropRealityPicks(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if strings.EqualFold(k, "realitySettings") {
				if rs, ok := child.(map[string]interface{}); ok {
					for rk := range rs {
						if isRealityPick(rk) {
							delete(rs, rk)
						}
					}
				}
			}
			dropRealityPicks(child)
		}
	case []interface{}:
		for _, child := range t {
			dropRealityPicks(child)
		}
	}
}

func isRealityPick(k string) bool {
	for _, name := range realityPicks {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// pairServers pairs each fresh server, in order, with the first stored server
// of the same ServerIdentity not yet paired: pairs[i] is the index in stored
// of fresh[i]'s pair, or -1. A server without an identity pairs with nothing.
func pairServers(stored, fresh []Server) []int {
	free := map[string][]int{}
	for i, s := range stored {
		if id := ServerIdentity(s); id != "" {
			free[id] = append(free[id], i)
		}
	}
	pairs := make([]int, len(fresh))
	for i, f := range fresh {
		pairs[i] = -1
		id := ServerIdentity(f)
		if q := free[id]; id != "" && len(q) > 0 {
			pairs[i], free[id] = q[0], q[1:]
		}
	}
	return pairs
}
```

- [ ] **Step 4: Run the identity tests to verify they pass**

Run: `cd server && go test ./internal/vpnconfig/ -run 'TestServerIdentity|TestPairServers' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing merge tests**

Create `server/internal/vpnconfig/refresh_test.go`:

```go
package vpnconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMergeRefresh_WhatAPanelPicksAtRandomKeepsTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want the stored list as it is", m)
	}
}

// 3x-ui's default remark puts the traffic left into every name.
func TestMergeRefresh_TheFreshNameOnTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")})

	if len(m.Servers) != 1 || m.Servers[0].Name != "DE 9GB" || !bytes.Equal(m.Servers[0].Outbound, stored[0].Outbound) || m.Renamed != 1 {
		t.Fatalf("merge %+v", m)
	}
}

func TestMergeRefresh_Addresses(t *testing.T) {
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10", "203.0.113.11")}
	for _, tc := range []struct {
		name  string
		fresh []string
		want  []string
		moved int
	}{
		{"the same set in another order", []string{"203.0.113.11", "203.0.113.10"}, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a host that did not resolve this time", nil, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a new set", []string{"203.0.113.12"}, []string{"203.0.113.12"}, 1},
	} {
		m := MergeRefresh(stored, []Server{reality("DE", "t.example", "bb22", tc.fresh...)})
		if len(m.Servers) != 1 || !reflect.DeepEqual(m.Servers[0].IPs, tc.want) || m.Readdressed != tc.moved {
			t.Errorf("%s: merge %+v", tc.name, m)
		}
	}
}

func TestMergeRefresh_NewChangedAndGoneServers(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10"), fr}
	rotated := reality("DE", "s.example", "bb22", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))
	nl := Server{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")} // did not resolve

	m := MergeRefresh(stored, []Server{rotated, nl})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, rotated.Outbound) {
		t.Fatalf("servers %+v, want the rotated server alone", m.Servers)
	}
	if m.Added != 1 || m.Removed != 2 {
		t.Fatalf("added %d, removed %d, want 1 and 2", m.Added, m.Removed)
	}
}

func TestMergeRefresh_TwinsPairInOrder(t *testing.T) {
	stored := []Server{reality("DE-1", "a.example", "aa11", "203.0.113.10"), reality("DE-2", "a.example", "bb22", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE-1", "b.example", "cc33", "203.0.113.10"), reality("DE-2", "b.example", "dd44", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) {
		t.Fatalf("merge %+v, want each twin on its own stored copy", m.Servers)
	}
}

func TestMergeRefresh_TheFreshOrder(t *testing.T) {
	de := reality("DE", "a.example", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}

	m := MergeRefresh([]Server{de, fr}, []Server{fr, reality("DE", "b.example", "bb22", "203.0.113.10")})

	if len(m.Servers) != 2 || m.Servers[0].Name != "FR" || !bytes.Equal(m.Servers[1].Outbound, de.Outbound) {
		t.Fatalf("merge %+v, want FR, then the stored DE", m.Servers)
	}
}

// A record stored before outbounds has no identity; the first refresh puts a
// fresh copy in its place.
func TestMergeRefresh_ALegacyRecordMakesWayForAFreshCopy(t *testing.T) {
	legacy := Server{Name: "DE", Address: "de.example", Port: 443, UUID: "u-1", Security: "reality", IPs: []string{"203.0.113.10"}}
	fresh := reality("DE", "a.example", "aa11", "203.0.113.10")

	m := MergeRefresh([]Server{legacy}, []Server{fresh})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, fresh.Outbound) || m.Added != 1 || m.Removed != 1 {
		t.Fatalf("merge %+v", m)
	}
}
```

Add to `server/internal/monitor/endpoints_test.go`, after `TestBuild_OneEndpointPerKey`:

```go
// vlessReality is a VLESS REALITY outbound on address with the short id a
// panel picked for one download.
func vlessReality(address, sid string) json.RawMessage {
	return json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"` + address + `","port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"www.example.com","fingerprint":"chrome","publicKey":"pk-1","shortId":"` + sid + `"}}}`)
}

// A periodic refresh that finds a server renamed and its short id picked anew
// keeps the stored outbound: every endpoint keeps its key and the outbound the
// prober holds, so the monitor keeps its statuses and does not restart it.
func TestBuild_APeriodicRefreshKeepsEveryEndpoint(t *testing.T) {
	stored := []vpnconfig.Server{
		{Name: "DE 10GB", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1", "192.0.2.2"}, Outbound: vlessReality("de.example", "aa11")},
		{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"192.0.2.3"}, Outbound: trojan("fr.example")},
	}
	listed := []vpnconfig.Server{
		{Name: "DE 9GB", Address: "de.example", Port: 443, IPs: []string{"192.0.2.2", "192.0.2.1"}, Outbound: vlessReality("de.example", "bb22")},
		{Name: "FR", Address: "fr.example", Port: 443, Outbound: trojan("fr.example")}, // its host did not answer this time
	}
	merged := vpnconfig.MergeRefresh(stored, listed).Servers

	before, _ := Build([]vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: stored}}, nil, outboundAsIs)
	after, _ := Build([]vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: merged}}, nil, outboundAsIs)

	if keysOf(after) != keysOf(before) {
		t.Fatalf("endpoints %+v, want the keys and outbounds of %+v", after, before)
	}
}
```

- [ ] **Step 6: Run the merge tests to verify they fail**

Run: `cd server && go test ./internal/vpnconfig/ -run TestMergeRefresh -v && go test ./internal/monitor/ -run TestBuild_APeriodicRefreshKeepsEveryEndpoint -v`
Expected: FAIL to compile with `undefined: MergeRefresh`.

- [ ] **Step 7: Write the merge**

Create `server/internal/vpnconfig/refresh.go`:

```go
package vpnconfig

import (
	"slices"
	"sort"
)

// RefreshMerge is a downloaded list merged with the stored one (MergeRefresh).
type RefreshMerge struct {
	Servers []Server
	// Added counts the listed servers without a pair, Removed the stored
	// servers without one, Renamed the pairs whose name changed and
	// Readdressed the pairs whose addresses did.
	Added, Removed, Renamed, Readdressed int
	// pairs[i] is the index in the stored list of listed server i's pair, or -1.
	pairs []int
}

// MergeRefresh merges listed, every server a periodic refresh downloaded in
// subscription order - without addresses where its host did not resolve -
// with stored, the list in the file. Each listed server pairs with the first
// stored server of the same ServerIdentity not yet paired and is then that
// stored record - its outbound, and so the stored REALITY picks - under the
// listed name: its endpoint keys, and so its monitor statuses, stay. Its
// addresses are the stored ones when the listed ones are the same set or
// there are none, the listed ones otherwise. A listed server without a pair
// comes in as it is, unless it has no address; a stored one without a pair
// leaves. The servers take the listed order.
func MergeRefresh(stored, listed []Server) RefreshMerge {
	m := RefreshMerge{pairs: pairServers(stored, listed)}
	paired := make([]bool, len(stored))
	for i, l := range listed {
		si := m.pairs[i]
		if si < 0 {
			if len(l.IPs) > 0 {
				m.Servers = append(m.Servers, l)
				m.Added++
			}
			continue
		}
		paired[si] = true
		s := stored[si]
		if l.Name != s.Name {
			s.Name = l.Name
			m.Renamed++
		}
		if len(l.IPs) > 0 && !sameIPSet(l.IPs, s.IPs) {
			s.IPs = l.IPs
			m.Readdressed++
		}
		m.Servers = append(m.Servers, s)
	}
	for _, p := range paired {
		if !p {
			m.Removed++
		}
	}
	return m
}

// sameIPSet reports whether a and b hold the same addresses, whatever their
// order: a resolver can answer the same set in another order every time.
func sameIPSet(a, b []string) bool {
	set := func(ips []string) []string {
		out := append([]string(nil), ips...)
		sort.Strings(out)
		return slices.Compact(out)
	}
	return slices.Equal(set(a), set(b))
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `cd server && go test ./internal/vpnconfig/ ./internal/monitor/ -v -run 'TestServerIdentity|TestPairServers|TestMergeRefresh|TestBuild'`
Expected: PASS.

- [ ] **Step 9: Run the package suites and gofmt**

Run: `cd server && gofmt -l ./internal/vpnconfig ./internal/monitor && go test ./internal/vpnconfig/ ./internal/monitor/`
Expected: `gofmt -l` prints nothing; both packages `ok`.

- [ ] **Step 10: Commit**

```bash
git add server/internal/vpnconfig/identity.go server/internal/vpnconfig/identity_test.go server/internal/vpnconfig/refresh.go server/internal/vpnconfig/refresh_test.go server/internal/monitor/endpoints_test.go
git commit -m "feat(vpnconfig): pair refreshed servers by identity, not by REALITY picks"
```

---

### Task 2: Publish a periodic refresh only when its list changed

**Files:**
- Modify: `server/internal/vpnconfig/refresh.go`
- Test: `server/internal/vpnconfig/refresh_test.go`

**Interfaces:**
- Consumes: `MergeRefresh`, `pairServers`, `RefreshMerge.pairs` (Task 1); `ConfigUpdate`, `SubscriptionFiles`, `FindSubscription`, `SubscriptionIPs`, `ServersSaved`, `stamp`, `errNothingToWrite`, `ErrSubscriptionGone`, `ErrSubscriptionStatic`, `ErrSaveSubscription` (`subops.go`, `substore.go`); test helpers `memStore`, `t0` (`subops_test.go`), `reality`, `trojanTo` (Task 1).
- Produces:
  - `var ErrNoServerResolved = errors.New("could not resolve IP for any server")`
  - `type RecordRename struct { Record, From, To string }` — `Record` is `"active_server"`, `"preferred_server"` or `"pending_restore.active"`.
  - `type RefreshResult struct { Wrote bool; Count, Added, Removed, Renamed, Readdressed int; Followed []RecordRename }`
  - `func PublishRefresh(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, listed []Server, now time.Time) (RefreshResult, error)`
  - `func followRenames(cfg *VPNDirectorConfig, id string, stored, fresh []Server, pairs []int) []RecordRename` (used again by Task 3)
  - Test helpers `alphaLink` (`"https://sub.example.com/s/t"`) and `alphaWith(servers ...Server) Subscription` (id `0a1b2c3d`, name `Alpha`, `Added` and `Refreshed` `t0`), used by Task 3.

- [ ] **Step 1: Write the failing publication tests**

Append to `server/internal/vpnconfig/refresh_test.go` (add `"errors"` and `"time"` to its imports):

```go
const alphaLink = "https://sub.example.com/s/t"

func alphaWith(servers ...Server) Subscription {
	return Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: alphaLink, Added: t0, Refreshed: t0, Servers: servers}
}

// A download that differs from the file only in what the panel picked writes
// neither the file nor the config: saveErr and configErr would fail any write.
func TestPublishRefresh_NothingChangedWritesNothing(t *testing.T) {
	stored := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(stored)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || res.Wrote {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{stored}) || !m.subs[0].Refreshed.Equal(t0) {
		t.Fatalf("file %+v", m.subs[0])
	}
}

func TestPublishRefresh_AChangeIsWrittenOnce(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	m := &memStore{subs: []Subscription{alphaWith(de)}}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10"), fr}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || res.Added != 1 || res.Count != 2 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de, fr}) || !m.subs[0].Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("file %+v", m.subs[0])
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"203.0.113.10", "203.0.113.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
	if m.writesOutside != 0 {
		t.Fatal("a file was written outside the config lock")
	}
}

func TestPublishRefresh_ClearsARecordedError(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	sub := alphaWith(de)
	sub.Error = "download failed: HTTP 403"
	m := &memStore{subs: []Subscription{sub}}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || m.subs[0].Error != "" || !m.subs[0].Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de}) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
}

func TestPublishRefresh_ADeletedOrRelinkedSubscriptionIsNotPublished(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "www.example.com", "aa11", "203.0.113.10"))}}
	listed := []Server{{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}}

	if _, err := PublishRefresh(m.update, m.files(), "1b2c3d4e", alphaLink, listed, t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/other", listed, t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("relinked: %v", err)
	}
	if m.subs[0].Servers[0].Name != "DE" || m.cfg.Xray.Servers != nil {
		t.Fatalf("wrote %+v, %v", m.subs, m.cfg.Xray.Servers)
	}
}

func TestPublishRefresh_AnEmptyMergeKeepsTheList(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(de)}}
	nl := Server{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")} // did not resolve

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{nl}, t0); !errors.Is(err, ErrNoServerResolved) {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de}) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
}

func TestPublishRefresh_TheRecordsFollowARename(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	named := ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443}
	active, preferred := named, named
	active.Seq = 7
	pending := active
	m.cfg.Xray.ActiveServer, m.cfg.Xray.PreferredServer = &active, &preferred
	m.cfg.Xray.PendingRestore = &XrayPendingRestore{Active: &pending}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || len(res.Followed) != 3 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	for record, a := range map[string]*ActiveServer{
		"active_server":          m.cfg.Xray.ActiveServer,
		"preferred_server":       m.cfg.Xray.PreferredServer,
		"pending_restore.active": m.cfg.Xray.PendingRestore.Active,
	} {
		if a.Name != "DE 9GB" || a.Address != "de.example" || a.Port != 443 {
			t.Errorf("%s %+v", record, a)
		}
	}
	// A rename is no selection: the write counter stays.
	if m.cfg.Xray.ActiveServer.Seq != 7 || m.cfg.Xray.PendingRestore.Active.Seq != 7 {
		t.Fatalf("seq %d and %d, want 7", m.cfg.Xray.ActiveServer.Seq, m.cfg.Xray.PendingRestore.Active.Seq)
	}
}

func TestPublishRefresh_OnlyItsOwnSubscriptionsRecordsFollow(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "1b2c3d4e", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 3}

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Xray.ActiveServer.Name != "DE 10GB" {
		t.Fatalf("another subscription's record followed: %+v", m.cfg.Xray.ActiveServer)
	}
}

func TestPublishRefresh_ARecordWhoseServerLeftStays(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"), fr)}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 3}
	renamed := fr
	renamed.Name = "FR 2"

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{renamed}, t0); err != nil {
		t.Fatal(err)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE 10GB" || a.Seq != 3 {
		t.Fatalf("active %+v, want it left as it was", a)
	}
}

// A manual refresh can write between a round's download and its publication.
// The merge reads the file as it is then: the manual refresh's copies are the
// stored ones, and a download that differs from them only in what the panel
// picked writes nothing.
func TestPublishRefresh_MergesWithTheFileAsItIsThen(t *testing.T) {
	manual := reality("DE", "b.example", "cc33", "203.0.113.10") // the round itself read aa11
	m := &memStore{subs: []Subscription{alphaWith(manual)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "a.example", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || res.Wrote || !bytes.Equal(m.subs[0].Servers[0].Outbound, manual.Outbound) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
}

// An operator that rotates a REALITY key and keeps the name makes another
// server: it comes in fresh, and the record that named the old one is left
// alone - neither renamed nor cleared.
func TestPublishRefresh_ARotatedKeyIsANewServer(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "s.example", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE", Address: "de.example", Port: 443, Seq: 3}
	rotated := reality("DE 2", "s.example", "aa11", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{rotated}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || res.Added != 1 || res.Removed != 1 || len(res.Followed) != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !bytes.Equal(m.subs[0].Servers[0].Outbound, rotated.Outbound) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE" || a.Seq != 3 {
		t.Fatalf("active %+v, want it left as it was", a)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/vpnconfig/ -run TestPublishRefresh -v`
Expected: FAIL to compile with `undefined: PublishRefresh` and `undefined: ErrNoServerResolved`.

- [ ] **Step 3: Write the publication**

In `server/internal/vpnconfig/refresh.go`, replace the import block with:

```go
import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"
)
```

and append:

```go
// ErrNoServerResolved is a periodic refresh whose merge came out empty: the
// download held servers, and none of them resolved or paired with one the
// file has. The list stays.
var ErrNoServerResolved = errors.New("could not resolve IP for any server")

// RecordRename is a server record that followed its renamed server.
type RecordRename struct {
	Record   string // active_server, preferred_server or pending_restore.active
	From, To string
}

// RefreshResult is what one PublishRefresh came to, for the log.
type RefreshResult struct {
	// Wrote says the subscription file was written: its list changed, or its
	// error was cleared.
	Wrote bool
	// Count is the servers of the merged list; the rest are MergeRefresh's.
	Count, Added, Removed, Renamed, Readdressed int
	// Followed is every record that followed a renamed server.
	Followed []RecordRename
}

// PublishRefresh publishes a periodic refresh of subscription id, downloaded
// from rawURL, under update's lock. listed is every server the download
// lists, in its order, without addresses where its host did not resolve. The
// merge (MergeRefresh) runs against the file as it is under the lock, not
// against what the caller read before downloading: a manual refresh may have
// written in between. A merged list equal to the file's, with no error
// recorded, writes nothing at all - neither the file nor the config. Anything
// else writes the merged list, refreshed, a cleared error and xray.servers in
// one update, and the records that named a renamed server follow it
// (followRenames). ErrSubscriptionGone when the subscription was deleted or
// relinked meanwhile, ErrNoServerResolved when the merge comes out empty;
// neither writes. A failure after the file was written carries
// ErrServersSaved.
func PublishRefresh(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, listed []Server, now time.Time) (RefreshResult, error) {
	if rawURL == "" {
		return RefreshResult{}, ErrSubscriptionStatic
	}
	var res RefreshResult
	saved := false
	err := update(func(cfg *VPNDirectorConfig) error {
		res = RefreshResult{}
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		stored := subs[i].Servers
		m := MergeRefresh(stored, listed)
		res.Count, res.Added, res.Removed, res.Renamed, res.Readdressed = len(m.Servers), m.Added, m.Removed, m.Renamed, m.Readdressed
		if len(m.Servers) == 0 {
			return ErrNoServerResolved
		}
		if subs[i].Error == "" && reflect.DeepEqual(m.Servers, stored) {
			return errNothingToWrite
		}
		sub := subs[i]
		sub.Servers = m.Servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
			res.Followed = followRenames(cfg, id, stored, listed, m.pairs)
		}
		return nil
	})
	switch {
	case err == nil:
		res.Wrote = true
		return res, nil
	case errors.Is(err, errNothingToWrite):
		return res, nil
	case saved:
		res.Wrote, res.Followed = true, nil
		return res, ServersSaved(err)
	default:
		res.Followed = nil
		return res, err
	}
}

// followRenames points every record that names a server of subscription id
// the refresh renamed at its new name: active_server, preferred_server and
// pending_restore.active, together - a restore compares the first and the
// last whole, and found them different, would discard its intent as
// superseded by a new selection. A record names the first stored server with
// its name, address and port; pairs pairs fresh with stored (pairServers).
// Address and port belong to the identity, so only the name moves, and seq
// stays: a rename is no selection, and the watch must not take it for one. A
// record whose server left the list stays as it is.
func followRenames(cfg *VPNDirectorConfig, id string, stored, fresh []Server, pairs []int) []RecordRename {
	renamed := map[int]string{}
	for fi, si := range pairs {
		if si >= 0 && fresh[fi].Name != stored[si].Name {
			renamed[si] = fresh[fi].Name
		}
	}
	if len(renamed) == 0 {
		return nil
	}
	var out []RecordRename
	follow := func(record string, a *ActiveServer) {
		if a == nil || a.Subscription != id {
			return
		}
		for si, s := range stored {
			if s.Name != a.Name || s.Address != a.Address || s.Port != a.Port {
				continue
			}
			if name, ok := renamed[si]; ok {
				out = append(out, RecordRename{Record: record, From: a.Name, To: name})
				a.Name = name
			}
			return
		}
	}
	follow("active_server", cfg.Xray.ActiveServer)
	follow("preferred_server", cfg.Xray.PreferredServer)
	if p := cfg.Xray.PendingRestore; p != nil {
		follow("pending_restore.active", p.Active)
	}
	return out
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd server && go test ./internal/vpnconfig/ -run 'TestPublishRefresh|TestMergeRefresh' -v`
Expected: PASS.

- [ ] **Step 5: Run the package suite and gofmt**

Run: `cd server && gofmt -l ./internal/vpnconfig && go test ./internal/vpnconfig/`
Expected: no gofmt output; `ok`.

- [ ] **Step 6: Commit**

```bash
git add server/internal/vpnconfig/refresh.go server/internal/vpnconfig/refresh_test.go
git commit -m "feat(vpnconfig): publish a periodic refresh only when its list changed"
```

---

### Task 3: A manual refresh carries the records over a rename

**Files:**
- Modify: `server/internal/vpnconfig/subops.go` (`AddSubscription`, `RefreshSubscription`)
- Modify: `server/internal/service/subscriptions.go:234`
- Test: `server/internal/vpnconfig/subops_test.go`, `server/internal/service/subscriptions_test.go`

**Interfaces:**
- Consumes: `followRenames`, `pairServers` (Tasks 1–2); test helpers `memStore`, `t0`, `reality`, `alphaWith`, `alphaLink` (package `vpnconfig`); `memConfigStore`, `newMemConfigStore`, `subscriptionHost`, `serve`, `publicLink` (package `service`, `subscriptions_test.go`).
- Produces: `func RefreshSubscriptionFollowingRenames(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error)`. `RefreshSubscription` keeps its signature and behaviour (the wave uses it). `AddSubscription` of a saved link now carries the records too.

- [ ] **Step 1: Write the failing vpnconfig tests**

Append to `server/internal/vpnconfig/subops_test.go`:

```go
// A refresh a user asks for takes the fresh copies and carries the records
// over a server the panel renamed - 3x-ui puts the traffic left into every
// name - without moving the write counter.
func TestRefreshSubscriptionFollowingRenames_TheRecordsFollowTheFreshCopy(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
	fresh := reality("DE 9GB", "example.com", "bb22", "203.0.113.10")

	if _, err := RefreshSubscriptionFollowingRenames(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{fresh}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(m.subs[0].Servers, []Server{fresh}) {
		t.Fatalf("servers %+v, want the fresh copy", m.subs[0].Servers)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE 9GB" || a.Seq != 7 {
		t.Fatalf("active %+v", a)
	}
}

// The wave's walk compares the records within its tick: its refresh leaves
// them alone.
func TestRefreshSubscription_TheWaveLeavesTheRecordsAlone(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}

	if _, err := RefreshSubscription(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if m.cfg.Xray.ActiveServer.Name != "DE 10GB" {
		t.Fatalf("active %+v, want it left alone", m.cfg.Xray.ActiveServer)
	}
}

func TestAddSubscription_ASavedLinkCarriesTheRecords(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.PreferredServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443}

	if _, existed, err := AddSubscription(m.update, m.files(), alphaLink, "", []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour)); err != nil || !existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}

	if m.cfg.Xray.PreferredServer.Name != "DE 9GB" {
		t.Fatalf("preferred %+v", m.cfg.Xray.PreferredServer)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/vpnconfig/ -run 'FollowingRenames|TheWaveLeavesTheRecordsAlone|ASavedLinkCarriesTheRecords' -v`
Expected: FAIL to compile with `undefined: RefreshSubscriptionFollowingRenames`.

- [ ] **Step 3: Carry the records in AddSubscription**

In `server/internal/vpnconfig/subops.go`, inside `AddSubscription`, replace:

```go
			sub = Subscription{ID: id, Name: n, URL: rawURL, Added: stamp(now)}
			i = len(subs)
			subs = append(subs, sub)
		}
		sub.Servers = servers
```

with:

```go
			sub = Subscription{ID: id, Name: n, URL: rawURL, Added: stamp(now)}
			i = len(subs)
			subs = append(subs, sub)
		}
		stored := sub.Servers
		sub.Servers = servers
```

and, in the same function, replace:

```go
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
```

with:

```go
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
			if existed {
				// A saved link is a refresh a user asked for: the records that
				// named a server it renamed follow it.
				followRenames(cfg, sub.ID, stored, servers, pairServers(stored, servers))
			}
		}
		return nil
	})
	switch {
	case err == nil:
		return sub, existed, nil
```

- [ ] **Step 4: Split RefreshSubscription**

In `server/internal/vpnconfig/subops.go`, replace the whole `RefreshSubscription` function (its doc comment included) with:

```go
// RefreshSubscription replaces the servers of subscription id with servers,
// downloaded from rawURL, under update's lock, and clears its error - only
// while that subscription still exists with that link: ErrSubscriptionGone
// otherwise, and nothing is written. xray.servers is recomputed in the same
// update. The watch's wave refreshes here: its walk compares the server
// records within one tick, and they must not change under it.
func RefreshSubscription(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error) {
	return refreshSubscription(update, files, id, rawURL, servers, now, false)
}

// RefreshSubscriptionFollowingRenames is RefreshSubscription for a refresh a
// user asked for - the Web UI's and the bot's. It takes the fresh copies as
// well, and the records that named a server the fresh list renamed follow it
// (followRenames): a panel that puts the traffic left into every name would
// otherwise lose the Active mark, and the return to the preferred server, at
// every refresh.
func RefreshSubscriptionFollowingRenames(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error) {
	return refreshSubscription(update, files, id, rawURL, servers, now, true)
}

func refreshSubscription(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time, follow bool) (Subscription, error) {
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
		stored := sub.Servers
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
			if follow {
				followRenames(cfg, id, stored, servers, pairServers(stored, servers))
			}
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
```

- [ ] **Step 5: Run the vpnconfig tests to verify they pass**

Run: `cd server && go test ./internal/vpnconfig/ -v -run 'Refresh|AddSubscription'`
Expected: PASS, the existing `TestRefreshSubscription_*` and `TestAddSubscription_*` included.

- [ ] **Step 6: Write the failing service test**

Append to `server/internal/service/subscriptions_test.go`:

```go
// A refresh from the Web UI or the bot carries the running server's record
// over the name the panel gave it this time - 3x-ui puts the traffic left
// into it - keeps its write counter, and stores the fresh copy.
func TestRefreshSubscription_TheRunningServerFollowsARename(t *testing.T) {
	link := func(name, sid string) string {
		return base64.StdEncoding.EncodeToString([]byte("vless://11111111-2222-3333-4444-555555555555@203.0.113.10:443?type=tcp&security=reality&pbk=pk-1&sni=www.example.com&fp=chrome&sid=" + sid + "#" + name))
	}
	store := newMemConfigStore()
	add := AddSubscription(context.Background(), store, subscriptionHost(t, serve(link("DE10GB", "aa11"))), publicLink, "Alpha")
	if add.Err != nil {
		t.Fatal(add.Err)
	}
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: add.ID, Name: "DE10GB", Address: "203.0.113.10", Port: 443, Seq: 3}

	res := RefreshSubscription(context.Background(), store, subscriptionHost(t, serve(link("DE9GB", "bb22"))), add.ID)

	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if a := store.cfg.Xray.ActiveServer; a.Name != "DE9GB" || a.Seq != 3 {
		t.Fatalf("active %+v, want DE9GB with seq 3", a)
	}
	if !strings.Contains(string(store.subs[0].Servers[0].Outbound), "bb22") {
		t.Fatalf("outbound %s, want the fresh copy", store.subs[0].Servers[0].Outbound)
	}
}
```

- [ ] **Step 7: Run it to verify it fails**

Run: `cd server && go test ./internal/service/ -run TestRefreshSubscription_TheRunningServerFollowsARename -v`
Expected: FAIL: `active {Name:DE10GB ...}, want DE9GB with seq 3`.

- [ ] **Step 8: Refresh through the new function**

In `server/internal/service/subscriptions.go`, function `refresh`, replace:

```go
	_, res.Err = vpnconfig.RefreshSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, imp.Servers, time.Now())
```

with:

```go
	// A refresh a user asked for: the records follow a server it renamed.
	_, res.Err = vpnconfig.RefreshSubscriptionFollowingRenames(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, imp.Servers, time.Now())
```

- [ ] **Step 9: Run the service and vpnconfig suites**

Run: `cd server && gofmt -l ./internal/vpnconfig ./internal/service && go test ./internal/vpnconfig/ ./internal/service/`
Expected: no gofmt output; both `ok`.

- [ ] **Step 10: Commit**

```bash
git add server/internal/vpnconfig/subops.go server/internal/vpnconfig/subops_test.go server/internal/service/subscriptions.go server/internal/service/subscriptions_test.go
git commit -m "feat(service): carry the server records over a rename on a manual refresh"
```

---

### Task 4: The fetcher lists every server, resolved or not

**Files:**
- Modify: `server/internal/subscription/resolve.go` (`Import`, `DecodeAndResolveLookup`)
- Modify: `server/internal/service/subfetch.go` (`SubscriptionFetcher`, `fetchServers`, `serversFromSubscriptionLookup`)
- Test: `server/internal/subscription/subscription_test.go`, `server/internal/service/subfetch_test.go`

**Interfaces:**
- Consumes: `subscription.DecodeAndResolveLookup`, `lazyTunnel`, `getSubscription`, `eitherLookup`, `cutShort`, `errNoResolved` (`subfetch.go`).
- Produces:
  - `subscription.Import.Listed []vpnconfig.Server` — every decoded server in order, with `IPs` where its host resolved and none where it did not.
  - `func (f SubscriptionFetcher) FetchList(ctx context.Context, rawURL string) ([]vpnconfig.Server, error)` — `Import.Listed`; a body none of whose hosts resolved is no error here.
  - `func fetchImport(ctx context.Context, rawURL string, wan *http.Client, tunnel func() *http.Client, wanLookup, tunnelLookup func(host string) ([]net.IP, error)) (subscription.Import, error)`; `fetchServers` keeps its signature and behaviour.

- [ ] **Step 1: Write the failing decoder test**

Append to `server/internal/subscription/subscription_test.go`:

```go
// watchd's periodic refresh keeps the addresses it has for a server whose host
// does not answer this time, so the import lists every server it decoded -
// those that resolved with their addresses, the others without - in order.
func TestDecodeAndResolveLookup_ListsEveryServer(t *testing.T) {
	body := "vless://uuid-1@oslo.example.invalid:443#Oslo\nvless://uuid-2@riga.example.invalid:443#Riga\nvless://uuid-3@bergen.example.invalid:443#Bergen"
	imp, err := DecodeAndResolveLookup(body, func(host string) ([]net.IP, error) {
		if host == "riga.example.invalid" {
			return nil, errors.New("no answer")
		}
		return []net.IP{net.ParseIP("203.0.113.50")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range imp.Listed {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"Oslo", "Riga", "Bergen"}) || len(imp.Listed[0].IPs) != 1 || len(imp.Listed[1].IPs) != 0 {
		t.Fatalf("listed %+v", imp.Listed)
	}
	if len(imp.Servers) != 2 || imp.ResolveErrors != 1 {
		t.Fatalf("servers %+v, resolve errors %d", imp.Servers, imp.ResolveErrors)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd server && go test ./internal/subscription/ -run TestDecodeAndResolveLookup_ListsEveryServer -v`
Expected: FAIL to compile with `imp.Listed undefined`.

- [ ] **Step 3: Keep the listed servers**

In `server/internal/subscription/resolve.go`, replace the `Import` type with:

```go
// Import is a decoded subscription after resolution. Servers keeps
// subscription order and holds only the servers whose address resolved;
// Parsed counts the servers Decode returned.
type Import struct {
	Servers []vpnconfig.Server
	// Listed is every server Decode returned, in subscription order, with its
	// addresses where they resolved and none where they did not: watchd's
	// periodic refresh keeps the addresses it has for a server whose host
	// does not answer this time.
	Listed        []vpnconfig.Server
	Total         int
	Parsed        int
	Skipped       []Skip
	ResolveErrors int
}
```

and, in `DecodeAndResolveLookup`, replace the loop:

```go
	for _, s := range decoded.Servers {
		ips, err := resolveIPv4(lookup, s.Address)
		if err != nil {
			imp.ResolveErrors++
			continue
		}
		s.IPs = ips
		imp.Servers = append(imp.Servers, s)
	}
```

with:

```go
	for _, s := range decoded.Servers {
		ips, err := resolveIPv4(lookup, s.Address)
		if err != nil {
			imp.ResolveErrors++
			imp.Listed = append(imp.Listed, s)
			continue
		}
		s.IPs = ips
		imp.Servers = append(imp.Servers, s)
		imp.Listed = append(imp.Listed, s)
	}
```

- [ ] **Step 4: Run the subscription suite**

Run: `cd server && go test ./internal/subscription/`
Expected: `ok`.

- [ ] **Step 5: Write the failing fetcher tests**

In `server/internal/service/subfetch_test.go`, replace `TestServersFromSubscription` with:

```go
func TestImportFromBody(t *testing.T) {
	if _, err := importFromBody([]byte("not base64 !!!"), nil); err == nil || err.Error() != "unrecognized subscription format" {
		t.Fatalf("err %v, want unrecognized subscription format", err)
	}
	// A readable subscription none of whose entries Xray can run.
	if _, err := importFromBody([]byte("tuic://uuid:pw@203.0.113.10:443#TUIC"), nil); err == nil || err.Error() != "no supported servers" {
		t.Fatalf("err %v, want no supported servers", err)
	}

	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443#Oslo"))
	imp, err := importFromBody([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Servers) != 1 || imp.Servers[0].Name != "Oslo" || !reflect.DeepEqual(imp.Servers[0].IPs, []string{"203.0.113.10"}) {
		t.Fatalf("servers %+v, want Oslo on 203.0.113.10", imp.Servers)
	}
}
```

and append:

```go
// The periodic refresh keeps the addresses it has for a server whose host does
// not answer this time: the fetch lists it, without addresses, beside the
// servers that resolved.
func TestFetchImport_ListsEveryServerWhetherItResolvedOrNot(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@oslo.example.invalid:443#Oslo\nvless://uuid-2@riga.example.invalid:443#Riga"))
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(wan.Close)

	imp, err := fetchImport(context.Background(), wan.URL, hostClient(wan), knownTunnel(nil),
		func(host string) ([]net.IP, error) {
			if host == "oslo.example.invalid" {
				return []net.IP{net.ParseIP("203.0.113.50")}, nil
			}
			return nil, errors.New("no answer")
		}, nil)

	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Listed) != 2 || imp.Listed[0].Name != "Oslo" || imp.Listed[1].Name != "Riga" || len(imp.Listed[1].IPs) != 0 {
		t.Fatalf("listed %+v", imp.Listed)
	}
	if len(imp.Servers) != 1 || imp.Servers[0].Name != "Oslo" {
		t.Fatalf("servers %+v", imp.Servers)
	}
}

// A body none of whose hosts resolved is a list to the periodic refresh, and
// still the error it always was to the wave.
func TestFetchImport_NothingResolvedIsAListToo(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@oslo.example.invalid:443#Oslo"))
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(wan.Close)
	noAnswer := func(string) ([]net.IP, error) { return nil, errors.New("no answer") }

	imp, err := fetchImport(context.Background(), wan.URL, hostClient(wan), knownTunnel(nil), noAnswer, nil)
	if err != nil || len(imp.Servers) != 0 || len(imp.Listed) != 1 {
		t.Fatalf("import %+v, err %v", imp, err)
	}
	if _, err := fetchServers(context.Background(), wan.URL, hostClient(wan), knownTunnel(nil), noAnswer, nil); !errors.Is(err, errNoResolved) {
		t.Fatalf("the wave's fetch: %v, want errNoResolved", err)
	}
}

func TestSubscriptionFetcher_FetchListRefusesALinkThatIsNotHTTPS(t *testing.T) {
	if _, err := (SubscriptionFetcher{}).FetchList(context.Background(), "http://sub.example.com/s/t"); !errors.Is(err, ErrSubscriptionURL) {
		t.Fatalf("err %v, want ErrSubscriptionURL", err)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `cd server && go test ./internal/service/ -run 'TestImportFromBody|TestFetchImport|FetchListRefuses' -v`
Expected: FAIL to compile with `undefined: importFromBody`, `undefined: fetchImport` and `FetchList undefined`.

- [ ] **Step 7: Split the fetch**

In `server/internal/service/subfetch.go`, replace the block from `// SubscriptionFetcher downloads and resolves subscriptions over WAN with a tunnel fallback.` through the end of `func (f SubscriptionFetcher) Fetch(...)` with:

```go
// SubscriptionFetcher downloads and resolves subscriptions over WAN with a tunnel fallback.
type SubscriptionFetcher struct {
	Store      ConfigStore
	VPN        VPNDirector
	TablesPath string
}

// Fetch downloads an HTTPS subscription and resolves its servers within ctx.
func (f SubscriptionFetcher) Fetch(ctx context.Context, rawURL string) ([]vpnconfig.Server, error) {
	return resolvedServers(f.fetch(ctx, rawURL))
}

// FetchList downloads an HTTPS subscription as Fetch does and returns every
// server it lists, in its order: those whose host resolved with their
// addresses, the others - over the tunnel too - without. A body none of whose
// hosts resolved is a list here, not an error: watchd's periodic refresh keeps
// the addresses it has for them (vpnconfig.MergeRefresh).
func (f SubscriptionFetcher) FetchList(ctx context.Context, rawURL string) ([]vpnconfig.Server, error) {
	imp, err := f.fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return imp.Listed, nil
}

func (f SubscriptionFetcher) fetch(ctx context.Context, rawURL string) (subscription.Import, error) {
	if u, err := url.Parse(rawURL); err != nil || u.Scheme != "https" {
		return subscription.Import{}, fmt.Errorf("%w: use an https:// link", ErrSubscriptionURL)
	}
	wan := ssrf.NewClient(10 * time.Second)
	// IPv4 only and bound to ctx: an AF_UNSPEC lookup of every hostname in the
	// subscription can hold a watch tick for minutes on this router, and a stop
	// has to be able to end it.
	wanLookup := subscription.LookupIPv4(ctx)
	tunnel, tunnelLookup := lazyTunnel(ctx, f.Store, f.VPN, f.TablesPath)
	return fetchImport(ctx, rawURL, wan, tunnel, wanLookup, tunnelLookup)
}

// resolvedServers is what the wave takes from an import: the servers whose
// hosts resolved, and errNoResolved when none did.
func resolvedServers(imp subscription.Import, err error) ([]vpnconfig.Server, error) {
	if err != nil {
		return nil, err
	}
	if len(imp.Servers) == 0 {
		return nil, errNoResolved
	}
	return imp.Servers, nil
}
```

Replace the whole `fetchServers` function (its doc comment included) with:

```go
// fetchServers is fetchImport for the wave: the servers whose hosts resolved.
func fetchServers(ctx context.Context, rawURL string, wan *http.Client, tunnel func() *http.Client, wanLookup, tunnelLookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
	return resolvedServers(fetchImport(ctx, rawURL, wan, tunnel, wanLookup, tunnelLookup))
}

// fetchImport GETs via wan, then tunnel. A body the tunnel fetched resolves
// over the tunnel. One the WAN fetched resolves each host with the WAN lookup,
// and with the tunnel's for a host the WAN resolver does not answer: one WAN
// answer used to make the whole list count as resolved, and the servers only
// the tunnel's resolver knew were dropped from it. A context that ends during
// the resolution fails every lookup after it at once, and what resolved before
// that is not the subscription: the fetch returns the daemons' "resolving the
// servers took longer than the deadline" when its deadline ended it, and the
// context's error when it was cancelled - never a list cut short. A download
// that failed reads as the daemons' own (DownloadError): the watch records it
// in the subscription's error, which the Web UI and /subs show. tunnel is
// asked for the tunnel's client only once the WAN falls short, and answers nil
// when there is none. A body none of whose hosts answered, on the WAN resolver
// or the tunnel's, gets the tunnel's own download as its last try, and comes
// back without an error if that resolves nothing either: Servers is empty,
// and Listed still has every server.
func fetchImport(ctx context.Context, rawURL string, wan *http.Client, tunnel func() *http.Client, wanLookup, tunnelLookup func(host string) ([]net.IP, error)) (subscription.Import, error) {
	body, err := getSubscription(ctx, wan, rawURL)
	if err == nil {
		imp, rerr := importFromBody(body, eitherLookup(ctx, wanLookup, tunnelLookup))
		if cerr := ctx.Err(); cerr != nil {
			return subscription.Import{}, cutShort(cerr)
		}
		if rerr != nil {
			return subscription.Import{}, rerr
		}
		// A body none of whose hostnames answered, on the WAN resolver or the
		// tunnel's, is the resolvers' failure rather than the subscription's:
		// the tunnel's own download gets the last try.
		if len(imp.Servers) > 0 || tunnel() == nil {
			return imp, nil
		}
		slog.Debug("Subscription hostnames did not resolve over the WAN, trying the tunnel")
	} else {
		err = NewDownloadError(err)
		// An ended context leaves the tunnel nothing to try, and finding the tunnel runs vpn-director.sh platform.
		if ctx.Err() != nil || tunnel() == nil {
			return subscription.Import{}, err
		}
		slog.Debug("Subscription fetch over WAN failed, trying the tunnel", "error", err)
	}
	body, err = getSubscription(ctx, tunnel(), rawURL)
	if err != nil {
		return subscription.Import{}, NewDownloadError(err)
	}
	imp, err := importFromBody(body, tunnelLookup)
	if cerr := ctx.Err(); cerr != nil {
		return subscription.Import{}, cutShort(cerr)
	}
	return imp, err
}
```

Replace the whole `serversFromSubscriptionLookup` function (its doc comment included) with:

```go
// importFromBody decodes a fetched body and resolves its servers through
// lookup, the default resolver when it is nil. A body none of whose hosts
// resolved is no error here: its Servers is empty, and Listed has them all.
func importFromBody(body []byte, lookup func(host string) ([]net.IP, error)) (subscription.Import, error) {
	imp, err := subscription.DecodeAndResolveLookup(string(body), lookup)
	if err != nil {
		return subscription.Import{}, err
	}
	if imp.Parsed == 0 {
		return subscription.Import{}, errors.New("no supported servers")
	}
	return imp, nil
}
```

- [ ] **Step 8: Run the service suite**

Run: `cd server && gofmt -l ./internal/service ./internal/subscription && go test ./internal/service/ ./internal/subscription/ && go vet ./internal/service/`
Expected: no gofmt output; both `ok`; `go vet` silent. The existing `TestFetchServers_*` and `TestSubscriptionFetcher_SharedDeadlinePublishesNoPartialList` pass unchanged.

- [ ] **Step 9: Commit**

```bash
git add server/internal/subscription/resolve.go server/internal/subscription/subscription_test.go server/internal/service/subfetch.go server/internal/service/subfetch_test.go
git commit -m "feat(service): list every subscription server for the periodic refresh"
```

---

### Task 5: The `monitor.subscription_refresh` setting

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`MonitorConfig`)
- Modify: `server/internal/monitor/settings.go`
- Modify: `router/opt/vpn-director/vpn-director.json.template`, `README.md`, `README.ru.md`
- Test: `server/internal/monitor/settings_test.go`, `server/internal/vpnconfig/vpnconfig_test.go`

**Interfaces:**
- Consumes: `vpnconfig.MonitorConfig`, `monitor.SettingsFrom`.
- Produces:
  - `MonitorConfig.SubscriptionRefresh string` (`json:"subscription_refresh,omitempty"`)
  - `const monitor.DefaultSubscriptionRefresh = 5 * time.Minute`, `const monitor.MinSubscriptionRefresh = time.Minute`
  - `func monitor.SubscriptionRefreshFrom(c *vpnconfig.MonitorConfig) (time.Duration, string)` — the period (`0` off) and a warning, empty when the value is good.
  - `monitor.SettingsFrom` returns that warning among its own; `monitor.Settings` does not change.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/monitor/settings_test.go`:

```go
func TestSubscriptionRefreshFrom(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		warns bool
	}{
		{"", 5 * time.Minute, false},
		{"7m", 7 * time.Minute, false},
		{"1m", time.Minute, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"30s", 5 * time.Minute, true},
		{"-5m", 5 * time.Minute, true},
		{"soon", 5 * time.Minute, true},
	} {
		got, warn := SubscriptionRefreshFrom(&vpnconfig.MonitorConfig{SubscriptionRefresh: tc.value})
		if got != tc.want || (warn != "") != tc.warns {
			t.Errorf("%q: %s, warning %q", tc.value, got, warn)
		}
	}
	if got, warn := SubscriptionRefreshFrom(nil); got != DefaultSubscriptionRefresh || warn != "" {
		t.Fatalf("no section: %s, warning %q", got, warn)
	}
}

// The monitor's settings reader logs each distinct set of warnings once; the
// refresh period's warning travels with them.
func TestSettingsFrom_WarnsAboutASubscriptionRefreshOutOfBounds(t *testing.T) {
	_, warns := SettingsFrom(&vpnconfig.MonitorConfig{SubscriptionRefresh: "30s"})
	if len(warns) != 1 || !strings.Contains(warns[0], "monitor.subscription_refresh") || !strings.Contains(warns[0], "using 5m0s") {
		t.Fatalf("warnings %v", warns)
	}
}
```

Append to `server/internal/vpnconfig/vpnconfig_test.go`:

```go
// A daemon's config write must not drop the key: the monitor section is
// rewritten whole.
func TestMonitorConfig_KeepsTheSubscriptionRefresh(t *testing.T) {
	var cfg VPNDirectorConfig
	if err := json.Unmarshal([]byte(`{"monitor":{"subscription_refresh":"7m"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor == nil || cfg.Monitor.SubscriptionRefresh != "7m" {
		t.Fatalf("monitor %+v", cfg.Monitor)
	}
	out, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(out), `"subscription_refresh":"7m"`) {
		t.Fatalf("written %s, err %v", out, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/monitor/ -run 'SubscriptionRefresh' -v; go test ./internal/vpnconfig/ -run TestMonitorConfig_KeepsTheSubscriptionRefresh -v`
Expected: FAIL to compile with `unknown field SubscriptionRefresh` and `undefined: SubscriptionRefreshFrom`.

- [ ] **Step 3: Add the key**

In `server/internal/vpnconfig/vpnconfig.go`, in `MonitorConfig`, replace:

```go
	Concurrency     int    `json:"concurrency,omitempty"`
	LogLevel        string `json:"log_level,omitempty"`
```

with:

```go
	Concurrency     int    `json:"concurrency,omitempty"`
	LogLevel        string `json:"log_level,omitempty"`
	// SubscriptionRefresh is how often watchd downloads every subscription
	// with a link; "0" turns it off (monitor.SubscriptionRefreshFrom).
	SubscriptionRefresh string `json:"subscription_refresh,omitempty"`
```

In `server/internal/monitor/settings.go`, replace:

```go
	// MaxInterval leaves room for the first dead pause.
	MaxInterval = time.Duration(1<<63-1) / 2
)
```

with:

```go
	// MaxInterval leaves room for the first dead pause.
	MaxInterval = time.Duration(1<<63-1) / 2
	// DefaultSubscriptionRefresh is how often watchd downloads every
	// subscription with a link when monitor.subscription_refresh is absent,
	// and MinSubscriptionRefresh the shortest period it takes: the monitor
	// reads the files once a minute anyway.
	DefaultSubscriptionRefresh = 5 * time.Minute
	MinSubscriptionRefresh     = time.Minute
)

// SubscriptionRefreshFrom resolves monitor.subscription_refresh, the period of
// watchd's periodic subscription refresh; 0 turns it off. A missing key is the
// default. A value that does not parse, is negative or is below
// MinSubscriptionRefresh is the default too, and the warning says so.
func SubscriptionRefreshFrom(c *vpnconfig.MonitorConfig) (time.Duration, string) {
	if c == nil || c.SubscriptionRefresh == "" {
		return DefaultSubscriptionRefresh, ""
	}
	d, err := time.ParseDuration(c.SubscriptionRefresh)
	if err == nil && (d == 0 || d >= MinSubscriptionRefresh) {
		return d, ""
	}
	return DefaultSubscriptionRefresh, fmt.Sprintf("monitor.subscription_refresh %q is neither 0 nor at least %s; using %s", c.SubscriptionRefresh, MinSubscriptionRefresh, DefaultSubscriptionRefresh)
}
```

and in `SettingsFrom`, replace its final:

```go
	return s, warns
}
```

with:

```go
	// The refresh period is watchd's, not the engine's, but it lives in this
	// section: its warning is logged with the others.
	if _, warn := SubscriptionRefreshFrom(c); warn != "" {
		warns = append(warns, warn)
	}
	return s, warns
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd server && gofmt -l ./internal/monitor ./internal/vpnconfig && go test ./internal/monitor/ ./internal/vpnconfig/`
Expected: no gofmt output; both `ok` (the existing `TestSettingsFrom_*` pass: their sections have no `subscription_refresh`).

- [ ] **Step 5: Document the key in the template and the READMEs**

In `router/opt/vpn-director/vpn-director.json.template`, replace:

```json
    "concurrency": 8,
    "log_level": "info"
```

with:

```json
    "concurrency": 8,
    "subscription_refresh": "5m",
    "log_level": "info"
```

In `README.md` and in `README.ru.md`, inside the `monitor` JSON example, make the same replacement (`"concurrency": 8,` / `"subscription_refresh": "5m",` / `"log_level": "info"`).

In `README.md`, after the line `The daemon rereads the section every minute; it logs to `/tmp/vpn-director-watchd.log`.`, insert a blank line and this paragraph:

```markdown
Every `subscription_refresh` (5 minutes by default; `0` turns it off) the daemon downloads every subscription with a link and writes only what changed, so the monitor checks the addresses a provider serves now. A server that did not change keeps its status, and so does one that differs only in the REALITY `sni`, `sid` or `spx` a panel picks at random for each download; a renamed server keeps its Active mark. The refresh waits while the Xray watch handles a failure, and the Changed column of the subscription list shows when a list last changed.
```

In `README.ru.md`, after the line `Демон перечитывает секцию раз в минуту; его лог — `/tmp/vpn-director-watchd.log`.`, insert a blank line and this paragraph:

```markdown
Раз в `subscription_refresh` (по умолчанию 5 минут; `0` выключает) демон скачивает все подписки со ссылкой и записывает только изменения, так что монитор проверяет адреса, которые провайдер раздаёт сейчас. Сервер, который не изменился, сохраняет статус; так же и сервер, у которого отличаются только REALITY `sni`, `sid` или `spx`, выбранные панелью случайно при этом скачивании; переименованный сервер сохраняет отметку Active. Пока watch Xray обрабатывает отказ, обновление ждёт; колонка Changed списка подписок показывает, когда список менялся последний раз.
```

- [ ] **Step 6: Check the template still parses**

Run: `jq -e '.monitor.subscription_refresh == "5m"' router/opt/vpn-director/vpn-director.json.template`
Expected: `true`.

- [ ] **Step 7: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go server/internal/monitor/settings.go server/internal/monitor/settings_test.go router/opt/vpn-director/vpn-director.json.template README.md README.ru.md
git commit -m "feat(monitor): add monitor.subscription_refresh"
```

---

### Task 6: The periodic refresh loop

**Files:**
- Create: `server/internal/subwatch/refresh.go`
- Modify: `server/internal/subwatch/watch.go` (`Watch` fields)
- Test: `server/internal/subwatch/refresh_test.go` (new)

**Interfaces:**
- Consumes: `vpnconfig.PublishRefresh`, `vpnconfig.RefreshResult`, `vpnconfig.ErrNoServerResolved`, `vpnconfig.RecordSubscriptionError`, `vpnconfig.ErrSubscriptionGone` (Tasks 1–2); from the watch: `tickMu`, `mu`, `failSince`, `pendingApply`, `applyDefaults`, `mutationRefused`, `updateFor`, `files`, `loadSubscriptions`, `stopPoll`, `errStopped`, `watchErrorAttr`, `FetchTimeout`; test helpers `fake`, `baseCfg`, `failedOverCfg`, `runningWatch`, `cloneSubs` (`watch_test.go`).
- Produces:
  - `Watch.FetchList func(ctx context.Context, url string) ([]vpnconfig.Server, error)` — nil leaves the refresh off.
  - `Watch.RefreshInterval func() time.Duration` — nil or 0 leaves the refresh off.
  - `func (w *Watch) StartRefresh(ctx context.Context)`
  - `const RefreshFirst = time.Minute`, `const RefreshRetry = time.Minute`

- [ ] **Step 1: Write the failing tests**

Create `server/internal/subwatch/refresh_test.go`:

```go
package subwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

// realityAt is a VLESS REALITY server on address as an import stores it, with
// the short id a panel picked for one download.
func realityAt(name, address, sid string, ips ...string) vpnconfig.Server {
	ob := fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"www.example.com","fingerprint":"chrome","publicKey":"pk-1","shortId":%q}}}`, address, sid)
	return vpnconfig.Server{Name: name, Address: address, Port: 443, IPs: ips, Outbound: json.RawMessage(ob)}
}

const alphaURL = "https://a.example/s/token"

func alphaOf(servers ...vpnconfig.Server) vpnconfig.Subscription {
	return vpnconfig.Subscription{ID: "aaaaaaaa", Name: "Alpha", URL: alphaURL, Servers: servers}
}

// refreshRig is a healthy watch over subs with the periodic refresh on, and
// the count of the subscription files it writes.
func refreshRig(subs ...vpnconfig.Subscription) (*fake, *Watch, *atomic.Int32) {
	f := &fake{cfg: baseCfg(), now: time.Unix(1_700_000_000, 0), subs: subs}
	w := runningWatch(f.watch())
	saves := new(atomic.Int32)
	save := w.SaveSubscription
	w.SaveSubscription = func(s vpnconfig.Subscription) error {
		saves.Add(1)
		return save(s)
	}
	w.RefreshInterval = func() time.Duration { return 5 * time.Minute }
	return f, w, saves
}

// serveList answers every download with servers.
func serveList(servers ...vpnconfig.Server) func(context.Context, string) ([]vpnconfig.Server, error) {
	return func(context.Context, string) ([]vpnconfig.Server, error) { return servers, nil }
}

func shortPoll(t *testing.T) {
	t.Helper()
	old := stopPoll
	stopPoll = time.Millisecond
	t.Cleanup(func() { stopPoll = old })
}

// A download that differs from the file only in what the panel picked at
// random writes nothing: the monitor keeps every status, and the prober runs on.
func TestRefreshRound_AnUnchangedListWritesNothing(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22", "203.0.113.10"))

	if !w.refreshRound(context.Background()) {
		t.Fatal("the round stood down")
	}

	if saves.Load() != 0 || !reflect.DeepEqual(f.subs[0].Servers, []vpnconfig.Server{stored}) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
	if f.cfg.Xray.Servers != nil {
		t.Fatalf("the config was written: xray.servers %v", f.cfg.Xray.Servers)
	}
}

func TestRefreshRound_ARenamedServerTakesItsRecords(t *testing.T) {
	stored := realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 7}
	f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Subscription: "aaaaaaaa", Name: "DE 10GB", Address: "de.example", Port: 443}
	w.FetchList = serveList(realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10"))

	w.refreshRound(context.Background())

	if saves.Load() != 1 || f.subs[0].Servers[0].Name != "DE 9GB" || !bytes.Equal(f.subs[0].Servers[0].Outbound, stored.Outbound) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
	if a := f.cfg.Xray.ActiveServer; a.Name != "DE 9GB" || a.Seq != 7 {
		t.Fatalf("active %+v", a)
	}
	if p := f.cfg.Xray.PreferredServer; p.Name != "DE 9GB" {
		t.Fatalf("preferred %+v", p)
	}
}

// While the watch handles an Xray failure the wave refreshes: the periodic
// refresh downloads nothing and writes nothing, and so while VPN Director is
// stopped or the gate is closed.
func TestRefreshRound_StandsDownWhileTheWatchMayNotOrIsBusy(t *testing.T) {
	for name, set := range map[string]func(*fake, *Watch){
		"a failover":      func(f *fake, _ *Watch) { f.cfg = failedOverCfg() },
		"a restore":       func(f *fake, _ *Watch) { f.cfg.Xray.PendingRestore = &vpnconfig.XrayPendingRestore{} },
		"a failing probe": func(f *fake, w *Watch) { w.failSince = f.now },
		"a pending apply": func(_ *fake, w *Watch) { w.pendingApply = true },
		"a stop":          func(_ *fake, w *Watch) { w.Stopped = func() bool { return true } },
		"a closed gate":   func(_ *fake, w *Watch) { w.CanMutate = func() error { return watchcompat.ErrIncompatible } },
	} {
		f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
		set(f, w)
		fetched := 0
		w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
			fetched++
			return nil, nil
		}

		if w.refreshRound(context.Background()) || fetched != 0 || saves.Load() != 0 {
			t.Errorf("%s: the round ran, downloads %d, writes %d", name, fetched, saves.Load())
		}
	}
}

// A failover that commits while the downloads run makes them the wave's.
func TestRefreshRound_AnEpisodeThatBeginsDuringTheDownloadsDropsThem(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		f.cfg.Xray.Failover = &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
		return []vpnconfig.Server{realityAt("FR", "fr.example", "cc33", "203.0.113.20")}, nil
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
}

// The publication waits for a running tick: a walk or a return must never see
// a record renamed under it.
func TestRefreshRound_PublishesOnlyBetweenTicks(t *testing.T) {
	_, w, saves := refreshRig(alphaOf(realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")))
	started, release := make(chan struct{}), make(chan struct{})
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		close(started)
		<-release
		return []vpnconfig.Server{realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10")}, nil
	}
	done := make(chan bool)
	go func() { done <- w.refreshRound(context.Background()) }()

	<-started
	w.tickMu.Lock() // a tick begins while the download runs
	close(release)
	time.Sleep(50 * time.Millisecond)
	if n := saves.Load(); n != 0 {
		w.tickMu.Unlock()
		t.Fatalf("published %d times inside a tick", n)
	}
	w.tickMu.Unlock()

	if !<-done || saves.Load() != 1 {
		t.Fatalf("writes %d after the tick, want 1", saves.Load())
	}
}

func TestRefreshRound_AStopEndsTheDownloadsAndRecordsNothing(t *testing.T) {
	shortPoll(t)
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	var stopped atomic.Bool
	w.Stopped = stopped.Load
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		stopped.Store(true)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

func TestRefreshRound_AFailedDownloadIsRecordedOnce(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		return nil, errors.New("download failed: HTTP 403")
	}

	w.refreshRound(context.Background())
	w.refreshRound(context.Background())

	if f.subs[0].Error != "download failed: HTTP 403" || saves.Load() != 1 || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("error %q, writes %d, servers %+v", f.subs[0].Error, saves.Load(), f.subs[0].Servers)
	}
}

// A resolver that answers nothing this time takes no server off the list.
func TestRefreshRound_NothingResolvedKeepsTheStoredAddresses(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, saves := refreshRig(alphaOf(stored))
	w.FetchList = serveList(realityAt("DE", "de.example", "bb22"))

	w.refreshRound(context.Background())

	if saves.Load() != 0 || !reflect.DeepEqual(f.subs[0].Servers, []vpnconfig.Server{stored}) {
		t.Fatalf("writes %d, servers %+v", saves.Load(), f.subs[0].Servers)
	}
}

func TestRefreshRound_AnEmptyMergeRecordsWhy(t *testing.T) {
	f, w, _ := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	w.FetchList = serveList(realityAt("FR", "fr.example", "cc33")) // another server, unresolved

	w.refreshRound(context.Background())

	if f.subs[0].Error != "could not resolve IP for any server" || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
}

func TestRefreshRound_LeavesStaticListsOut(t *testing.T) {
	beta := vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{realityAt("NL", "nl.example", "dd44", "203.0.113.30")}}
	f, w, _ := refreshRig(beta, alphaOf(realityAt("DE 10GB", "de.example", "aa11", "203.0.113.10")))
	var asked []string
	w.FetchList = func(_ context.Context, url string) ([]vpnconfig.Server, error) {
		asked = append(asked, url)
		return []vpnconfig.Server{realityAt("DE 9GB", "de.example", "bb22", "203.0.113.10")}, nil
	}

	w.refreshRound(context.Background())

	if !reflect.DeepEqual(asked, []string{alphaURL}) || f.subs[1].Servers[0].Name != "DE 9GB" || f.subs[0].Servers[0].Name != "NL" {
		t.Fatalf("asked %v, subscriptions %+v", asked, f.subs)
	}
}

func TestRefreshRound_NoLinkStandsDown(t *testing.T) {
	_, w, _ := refreshRig(vpnconfig.Subscription{ID: "bbbbbbbb", Name: "Beta", Servers: []vpnconfig.Server{realityAt("NL", "nl.example", "dd44", "203.0.113.30")}})
	fetched := 0
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetched++
		return nil, nil
	}

	if w.refreshRound(context.Background()) || fetched != 0 {
		t.Fatalf("the round ran over a static list alone, downloads %d", fetched)
	}
}

// A panel that answers with an HTML page, or with no server Xray can run,
// costs the subscription nothing but its error, and the next good answer
// clears it.
func TestRefreshRound_AnUnreadableAnswerIsRecordedAndTheNextRoundClearsIt(t *testing.T) {
	stored := realityAt("DE", "de.example", "aa11", "203.0.113.10")
	f, w, _ := refreshRig(alphaOf(stored))
	answers := 0
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		answers++
		if answers == 1 {
			return nil, errors.New("unrecognized subscription format")
		}
		return []vpnconfig.Server{realityAt("DE", "de.example", "bb22", "203.0.113.10")}, nil
	}

	w.refreshRound(context.Background())
	if f.subs[0].Error != "unrecognized subscription format" || f.subs[0].Servers[0].Name != "DE" {
		t.Fatalf("after the HTML page: error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
	w.refreshRound(context.Background())
	if f.subs[0].Error != "" || !bytes.Equal(f.subs[0].Servers[0].Outbound, stored.Outbound) {
		t.Fatalf("after the good answer: error %q, servers %+v", f.subs[0].Error, f.subs[0].Servers)
	}
}

// watchd shutting down in the middle of a round writes nothing more, and a
// download it cut short says nothing about the subscription.
func TestRefreshRound_AShutdownWritesNothingMore(t *testing.T) {
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	ctx, cancel := context.WithCancel(context.Background())
	w.FetchList = func(fctx context.Context, _ string) ([]vpnconfig.Server, error) {
		cancel()
		<-fctx.Done()
		return nil, fctx.Err()
	}

	if w.refreshRound(ctx) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

func TestRefreshRound_AGateThatClosesDropsTheDownloads(t *testing.T) {
	shortPoll(t)
	f, w, saves := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	var closed atomic.Bool
	w.CanMutate = func() error {
		if closed.Load() {
			return watchcompat.ErrIncompatible
		}
		return nil
	}
	w.FetchList = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		closed.Store(true)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}

	if w.refreshRound(context.Background()) || saves.Load() != 0 || f.subs[0].Error != "" {
		t.Fatalf("writes %d, error %q", saves.Load(), f.subs[0].Error)
	}
}

// The first round a minute after the start, the next one an interval after a
// round ran, a minute after one that stood down, and a refresh turned off
// looks again every minute without downloading.
func TestStartRefresh_Schedule(t *testing.T) {
	_, w, _ := refreshRig(alphaOf(realityAt("DE", "de.example", "aa11", "203.0.113.10")))
	waits := make(chan time.Duration, 10)
	fire := make(chan time.Time)
	w.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	var interval atomic.Int64
	interval.Store(int64(7 * time.Minute))
	w.RefreshInterval = func() time.Duration { return time.Duration(interval.Load()) }
	var stopped atomic.Bool
	w.Stopped = stopped.Load
	var fetched atomic.Int32
	w.FetchList = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetched.Add(1)
		return []vpnconfig.Server{realityAt("DE", "de.example", "bb22", "203.0.113.10")}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.StartRefresh(ctx); close(done) }()

	expect := func(want time.Duration) {
		t.Helper()
		select {
		case d := <-waits:
			if d != want {
				t.Fatalf("wait %s, want %s", d, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no wait of %s", want)
		}
	}
	expect(RefreshFirst)
	fire <- time.Now() // a round that runs
	expect(7 * time.Minute)
	stopped.Store(true)
	fire <- time.Now() // a round that stands down
	expect(RefreshRetry)
	stopped.Store(false)
	interval.Store(0)
	fire <- time.Now() // the refresh is off
	expect(RefreshRetry)
	cancel()
	<-done

	if n := fetched.Load(); n != 1 {
		t.Fatalf("downloads %d, want the first round's alone", n)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/subwatch/ -run 'TestRefreshRound|TestStartRefresh' -v`
Expected: FAIL to compile with `w.refreshRound undefined`, `unknown field FetchList` (or `w.FetchList undefined`), `w.after undefined`.

- [ ] **Step 3: Add the watch's fields**

In `server/internal/subwatch/watch.go`, in the `Watch` struct, replace:

```go
	Fetch             func(ctx context.Context, url string) ([]vpnconfig.Server, error)
```

with:

```go
	Fetch             func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	// FetchList downloads a subscription for the periodic refresh: every
	// server it lists, in its order, without addresses where the host did not
	// resolve. nil leaves the periodic refresh off.
	FetchList func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	// RefreshInterval is the period of the periodic refresh, read before every
	// round; 0 turns it off, and so does a nil RefreshInterval.
	RefreshInterval func() time.Duration
```

and replace:

```go
	lastPicked        *vpnconfig.Server // the copy the walk picked or a return proved, with the address it ran on
```

with:

```go
	lastPicked        *vpnconfig.Server // the copy the walk picked or a return proved, with the address it ran on
	after             func(time.Duration) <-chan time.Time // StartRefresh's waits; nil is time.After
```

- [ ] **Step 4: Write the loop**

Create `server/internal/subwatch/refresh.go`:

```go
package subwatch

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

const (
	// RefreshFirst is how long after watchd starts its first periodic refresh
	// begins: at boot the WAN may not be up yet.
	RefreshFirst = time.Minute
	// RefreshRetry is how soon a round that stood down looks again, and how
	// often a refresh turned off looks whether it still is.
	RefreshRetry = time.Minute
)

var (
	// errEpisode stands the periodic refresh down while the watch handles an
	// Xray failure: the wave refreshes the subscriptions then.
	errEpisode = errors.New("the watch is handling an Xray failure")
	// errNoLinks stands it down when no subscription has a link.
	errNoLinks = errors.New("no subscription has a link")
	// errNoRefresh is a watch built without what the refresh needs.
	errNoRefresh = errors.New("the periodic refresh is not configured")
)

// listed is one subscription's download in a round of the periodic refresh.
type listed struct {
	servers []vpnconfig.Server
	err     error
}

// StartRefresh downloads every subscription with a link, one round every
// RefreshInterval, until ctx ends, and publishes what changed. It downloads
// outside the watch's tick, publishes between ticks, and stands down while
// VPN Director is stopped, the gate is closed or the watch handles an Xray
// failure. The first round starts RefreshFirst after it does; each next one
// RefreshInterval after the last one ended, or RefreshRetry after one that
// stood down.
func (w *Watch) StartRefresh(ctx context.Context) {
	wait := RefreshFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.refreshAfter(wait):
		}
		wait = RefreshRetry
		var interval time.Duration
		if w.RefreshInterval != nil {
			interval = w.RefreshInterval()
		}
		if interval > 0 && w.refreshRound(ctx) {
			wait = interval
		}
	}
}

func (w *Watch) refreshAfter(d time.Duration) <-chan time.Time {
	if w.after != nil {
		return w.after(d)
	}
	return time.After(d)
}

// refreshRound runs one round and reports whether it published; one that
// stood down, or that a shutdown cut short, returns false.
func (w *Watch) refreshRound(ctx context.Context) bool {
	subs, err := w.refreshTargets()
	if err != nil {
		slog.Debug("Periodic subscription refresh stood down", "reason", err.Error())
		return false
	}
	results := w.downloadLists(ctx, subs)
	if ctx.Err() != nil {
		return false
	}
	// Publish between ticks: a walk or a return compares the server records
	// whole, and a rename under it would read as a new selection.
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.refreshStandsDown(); err != nil {
		slog.Info("Periodic subscription refresh dropped its downloads", "reason", err.Error())
		return false
	}
	for i, s := range subs {
		if ctx.Err() != nil {
			return false
		}
		w.publishList(ctx, s, results[i])
	}
	return true
}

// refreshTargets is every subscription with a link, read while no tick runs,
// or why the round stands down.
func (w *Watch) refreshTargets() ([]vpnconfig.Subscription, error) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.refreshStandsDown(); err != nil {
		return nil, err
	}
	all, err := w.loadSubscriptions()
	if err != nil {
		return nil, err
	}
	var linked []vpnconfig.Subscription
	for _, s := range all {
		if !s.Static() {
			linked = append(linked, s)
		}
	}
	if len(linked) == 0 {
		return nil, errNoLinks
	}
	return linked, nil
}

// refreshStandsDown is why the periodic refresh may not download or publish
// now, or nil. Its caller holds tickMu and mu, so no tick runs meanwhile.
func (w *Watch) refreshStandsDown() error {
	w.applyDefaults()
	if w.FetchList == nil || w.LoadVPN == nil || w.UpdateVPN == nil {
		return errNoRefresh
	}
	if err := w.mutationRefused(); err != nil {
		return err
	}
	if !w.failSince.IsZero() || w.pendingApply {
		return errEpisode
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return err
	}
	if cfg != nil && (cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil) {
		return errEpisode
	}
	return nil
}

// downloadLists downloads every subscription of subs at once, each within
// FetchTimeout. A stop or a closed gate ends the downloads within stopPoll,
// as cancelOnStop ends a tick's waits.
func (w *Watch) downloadLists(ctx context.Context, subs []vpnconfig.Subscription) []listed {
	ctx, end := w.refreshScope(ctx)
	defer end()
	results := make([]listed, len(subs))
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
			defer cancel()
			servers, err := w.FetchList(fetchCtx, s.URL)
			results[i] = listed{servers, err}
		}()
	}
	wg.Wait()
	return results
}

// refreshScope is ctx, ended once a stop or the gate forbids mutation; end
// joins its poller.
func (w *Watch) refreshScope(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	if w.Stopped == nil && w.CanMutate == nil {
		return ctx, func() { cancel(nil) }
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(stopPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.mutationRefused(); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel(nil)
		<-ended
	}
}

// publishList publishes one subscription's download, or records why it did
// not arrive (vpnconfig.RecordSubscriptionError writes an error once). Its
// caller holds tickMu and mu.
func (w *Watch) publishList(ctx context.Context, s vpnconfig.Subscription, l listed) {
	update, files := w.updateFor(ctx), w.files(ctx)
	err := l.err
	if err == nil {
		res, perr := vpnconfig.PublishRefresh(update, files, s.ID, s.URL, l.servers, w.Now())
		switch {
		case perr == nil:
			logPublished(s, res)
			return
		case errors.Is(perr, vpnconfig.ErrNoServerResolved):
			err = perr
		case errors.Is(perr, vpnconfig.ErrSubscriptionGone):
			slog.Info("Periodic refresh dropped; the subscription was deleted or relinked while it downloaded", "subscription", s.Name)
			return
		case errors.Is(perr, errStopped), errors.Is(perr, watchcompat.ErrIncompatible), ctx.Err() != nil:
			return
		default:
			slog.Warn("Failed to publish the periodically refreshed subscription", "subscription", s.Name, watchErrorAttr(perr))
			return
		}
	}
	// A *url.Error carries the whole subscription URL, token included.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if msg != s.Error {
		slog.Warn("Periodic subscription refresh failed", "subscription", s.Name, watchErrorAttr(err))
	}
	rerr := vpnconfig.RecordSubscriptionError(update, files, s.ID, s.URL, s.Refreshed, msg)
	if rerr != nil && !errors.Is(rerr, errStopped) && !errors.Is(rerr, watchcompat.ErrIncompatible) && !errors.Is(rerr, vpnconfig.ErrSubscriptionGone) {
		slog.Warn("Failed to record why the subscription did not refresh", "subscription", s.Name, watchErrorAttr(rerr))
	}
}

// logPublished says what a published round did to subscription s. No link:
// names and counts only.
func logPublished(s vpnconfig.Subscription, res vpnconfig.RefreshResult) {
	if !res.Wrote {
		slog.Debug("Periodic refresh found the subscription unchanged", "subscription", s.Name, "servers", res.Count)
		return
	}
	if s.Error != "" {
		slog.Info("Subscription downloads again", "subscription", s.Name)
	}
	slog.Info("Subscription list published", "subscription", s.Name, "servers", res.Count,
		"added", res.Added, "removed", res.Removed, "renamed", res.Renamed, "readdressed", res.Readdressed)
	for _, f := range res.Followed {
		slog.Info("Server record follows its renamed server", "record", f.Record, "from", f.From, "to", f.To)
	}
}
```

- [ ] **Step 5: Run the loop's tests, with the race detector**

Run: `cd server && gofmt -l ./internal/subwatch && go test -race ./internal/subwatch/ -run 'TestRefreshRound|TestStartRefresh' -v`
Expected: no gofmt output (if `gofmt -l` lists `watch.go`, run `gofmt -w internal/subwatch/watch.go`: the struct's field alignment); every test PASS; no `DATA RACE`.

- [ ] **Step 6: Run the whole subwatch suite**

Run: `cd server && go test ./internal/subwatch/`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add server/internal/subwatch/refresh.go server/internal/subwatch/refresh_test.go server/internal/subwatch/watch.go
git commit -m "feat(subwatch): refresh every subscription periodically, between ticks"
```

---

### Task 7: Start the loop in watchd

**Files:**
- Modify: `server/cmd/watchd/watch.go` (`newWatch`)
- Modify: `server/cmd/watchd/runtime.go` (`runRuntime`)
- Test: `server/cmd/watchd/runtime_test.go`

**Interfaces:**
- Consumes: `service.SubscriptionFetcher.FetchList` (Task 4); `monitor.SubscriptionRefreshFrom`, `monitor.DefaultSubscriptionRefresh` (Task 5); `Watch.FetchList`, `Watch.RefreshInterval`, `(*Watch).StartRefresh` (Task 6); test helpers `runtimePaths`, `runtimeConfig`, `runtimeQueue`, `runtimeWatch`, `runtimeExecutor` (`runtime_test.go`).
- Produces: a watchd that runs the periodic refresh beside the watch.

- [ ] **Step 1: Write the failing test**

Append to `server/cmd/watchd/runtime_test.go`:

```go
// The periodic refresh reads monitor.subscription_refresh before every round:
// absent is 5 minutes, 0 is off, a value below a minute is the default.
func TestNewWatch_RefreshFollowsTheMonitorSection(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`{"data_dir":"data"}`, 5 * time.Minute},
		{`{"data_dir":"data","monitor":{"subscription_refresh":"7m"}}`, 7 * time.Minute},
		{`{"data_dir":"data","monitor":{"subscription_refresh":"0"}}`, 0},
		{`{"data_dir":"data","monitor":{"subscription_refresh":"30s"}}`, 5 * time.Minute},
	} {
		p := runtimePaths(t)
		cfg := runtimeConfig(t, p, tc.raw)
		q := runtimeQueue(t, filepath.Join(t.TempDir(), "watchd-notifications.json"))
		w := runtimeWatch(t, context.Background(), p, cfg, q, runtimeExecutor(func(context.Context, string, ...string) (*shell.Result, error) {
			return &shell.Result{}, nil
		}))
		if w.FetchList == nil || w.RefreshInterval == nil {
			t.Fatal("newWatch left the periodic refresh unwired")
		}
		if got := w.RefreshInterval(); got != tc.want {
			t.Errorf("%s: interval %s, want %s", tc.raw, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd server && go test ./cmd/watchd/ -run TestNewWatch_RefreshFollowsTheMonitorSection -v`
Expected: FAIL: `newWatch left the periodic refresh unwired`.

- [ ] **Step 3: Wire the refresh**

In `server/cmd/watchd/watch.go`, add `"time"` and `"github.com/zinin/vpn-director/server/internal/monitor"` to the imports, and, right after the `w.Fetch = func(...)` block, add:

```go
	w.FetchList = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		fetcher := service.SubscriptionFetcher{
			Store:      cfg,
			VPN:        vpn.ForContext(ctx),
			TablesPath: p.TunnelTables,
		}
		return fetcher.FetchList(ctx, url)
	}
	// Read before every round, so a change takes effect without a restart. A
	// config that does not load stands the round down anyway.
	w.RefreshInterval = func() time.Duration {
		config, err := cfg.LoadVPNConfig()
		if err != nil {
			return monitor.DefaultSubscriptionRefresh
		}
		d, _ := monitor.SubscriptionRefreshFrom(config.Monitor)
		return d
	}
```

In `server/cmd/watchd/runtime.go`, replace:

```go
	start(deps.Watch.Start)
```

with:

```go
	start(deps.Watch.Start)
	start(deps.Watch.StartRefresh)
```

- [ ] **Step 4: Run the watchd suite**

Run: `cd server && gofmt -l ./cmd/watchd && go test ./cmd/watchd/`
Expected: no gofmt output; `ok` (the runtime tests drain within their 3 s: the loop's first wait of a minute ends with the context).

- [ ] **Step 5: Commit**

```bash
git add server/cmd/watchd/watch.go server/cmd/watchd/runtime.go server/cmd/watchd/runtime_test.go
git commit -m "feat(watchd): start the periodic subscription refresh"
```

---

### Task 8: The bot wizard finds a pick a refresh renamed

**Files:**
- Modify: `server/internal/wizard/state.go` (`State`, `PickServer`, `PickedIndex`)
- Test: `server/internal/wizard/state_test.go`

**Interfaces:**
- Consumes: `vpnconfig.ServerIdentity` (Task 1).
- Produces: `State.PickedIndex` falls back to the first server of the picked subscription with the picked identity.

- [ ] **Step 1: Write the failing test**

Add `"encoding/json"` and `"strings"` to the imports of `server/internal/wizard/state_test.go`, and append:

```go
// A periodic refresh can rename the pick during the minutes the steps take -
// 3x-ui puts the traffic left into every name - and the wizard still finds
// it, in its own subscription only.
func TestState_PickedIndexFindsAPickARefreshRenamed(t *testing.T) {
	ob := `{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u-1"}]}]},"streamSettings":{"security":"reality","realitySettings":{"publicKey":"pk-1","shortId":"aa11","serverName":"www.example.com"}}}`
	picked := vpnconfig.Server{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Outbound: json.RawMessage(ob)}
	other := vpnconfig.Server{Subscription: "0a1b2c3d", Name: "FR", Address: "fr.example", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"fr.example","port":443,"password":"p"}]}}`)}
	s := NewManager().Start(123)
	s.PickServer(0, picked)
	renamed := picked
	renamed.Name = "DE 9GB"
	renamed.Outbound = json.RawMessage(strings.Replace(ob, "aa11", "bb22", 1))

	if got := s.PickedIndex([]vpnconfig.Server{other, renamed}); got != 1 {
		t.Fatalf("PickedIndex %d, want 1", got)
	}
	twin := renamed
	twin.Subscription = "1b2c3d4e"
	if got := s.PickedIndex([]vpnconfig.Server{other, twin}); got != -1 {
		t.Fatalf("PickedIndex %d, want -1: another subscription's server is not the pick", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd server && go test ./internal/wizard/ -run TestState_PickedIndexFindsAPickARefreshRenamed -v`
Expected: FAIL: `PickedIndex -1, want 1`.

- [ ] **Step 3: Keep the pick's identity**

In `server/internal/wizard/state.go`, in the `State` struct, replace:

```go
	Picked      *vpnconfig.ActiveServer // the server step 1 picked, as the list read then
```

with:

```go
	Picked      *vpnconfig.ActiveServer // the server step 1 picked, as the list read then
	// pickedIdentity is the pick's vpnconfig.ServerIdentity: a refresh can
	// rename it while the steps run.
	pickedIdentity string
```

replace `PickServer` with:

```go
// PickServer records the server step 1 picked and where the list had it. Steps
// 2 to 4 take minutes, and meanwhile a refresh - the subscription watch,
// watchd's periodic refresh, another importer - can move it, rename it or drop
// it: its index then names a server the user never chose.
func (s *State) PickServer(idx int, srv vpnconfig.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ServerIndex = idx
	s.Picked = vpnconfig.NewActiveServer(srv)
	s.pickedIdentity = vpnconfig.ServerIdentity(srv)
}
```

and, in `PickedIndex`, replace its end:

```go
	for i, srv := range servers {
		if s.picks(srv) {
			return i
		}
	}
	return -1
}
```

with:

```go
	for i, srv := range servers {
		if s.picks(srv) {
			return i
		}
	}
	// A refresh renamed it - a panel that puts the traffic left into every
	// name renames them all: the server of the picked subscription with the
	// picked identity is the pick.
	if s.pickedIdentity != "" {
		for i, srv := range servers {
			if srv.Subscription == s.Picked.Subscription && vpnconfig.ServerIdentity(srv) == s.pickedIdentity {
				return i
			}
		}
	}
	return -1
}
```

Also update the doc comment of `PickedIndex` to:

```go
// PickedIndex is where servers has the server step 1 picked - by subscription,
// name, address and port, or, renamed by a refresh, by subscription and
// identity - or -1 when servers no longer has it. A state with no pick
// recorded has only its index to go by.
```

- [ ] **Step 4: Run the wizard suite**

Run: `cd server && gofmt -l ./internal/wizard && go test -race ./internal/wizard/`
Expected: no gofmt output; `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/wizard/state.go server/internal/wizard/state_test.go
git commit -m "fix(wizard): find a pick a refresh renamed"
```

---

### Task 9: Say when a list last changed

**Files:**
- Modify: `server/internal/handler/subs.go` (`subLine`)
- Modify: `web/src/components/ServersTab.vue:410`
- Test: `server/internal/handler/subs_test.go`, `web/test/servers-monitor.cjs`

**Interfaces:**
- Consumes: `timeAgo` (`subs.go`).
- Produces: `func changedAgo(now, t time.Time) string` — `"changed 2 h ago"`, `"never changed"` for a zero time.

- [ ] **Step 1: Write the failing tests**

In `server/internal/handler/subs_test.go`, in `TestSubs_ListsEachSubscriptionWithItsButtons`, replace:

```go
	if !strings.Contains(got, `Alpha — sub\.example\.com — 32 servers — 2 h ago — OK`) || strings.Contains(got, "token") {
		t.Fatalf("list %q", got)
	}
	if !strings.Contains(got, "Beta — static list — 5 servers — 2 h ago — download failed: HTTP 403") {
		t.Fatalf("list %q", got)
	}
```

with:

```go
	if !strings.Contains(got, `Alpha — sub\.example\.com — 32 servers — changed 2 h ago — OK`) || strings.Contains(got, "token") {
		t.Fatalf("list %q", got)
	}
	if !strings.Contains(got, "Beta — static list — 5 servers — changed 2 h ago — download failed: HTTP 403") {
		t.Fatalf("list %q", got)
	}
```

and append:

```go
// watchd's periodic refresh writes a list only when it changed: the time a
// line shows is that of the last change, and the line says so.
func TestChangedAgo(t *testing.T) {
	if got := changedAgo(subsNow, subsNow.Add(-2*time.Hour)); got != "changed 2 h ago" {
		t.Fatalf("got %q", got)
	}
	if got := changedAgo(subsNow, time.Time{}); got != "never changed" {
		t.Fatalf("got %q", got)
	}
}
```

In `web/test/servers-monitor.cjs`, right after the line `const component = fs.readFileSync(path.join(sourceRoot, 'components/ServersTab.vue'), 'utf8')`, add:

```js
// watchd's periodic refresh writes a list only when it changed: the column
// shows when the list last changed, not when it was last checked.
assert.match(component, /<th>Changed<\/th>/)
assert.doesNotMatch(component, /<th>Refreshed<\/th>/)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd server && go test ./internal/handler/ -run 'TestSubs_ListsEachSubscriptionWithItsButtons|TestChangedAgo' -v; cd ../web && npm test`
Expected: Go FAIL (`undefined: changedAgo`); `npm test` fails on `The input did not match the regular expression /<th>Changed<\/th>/`.

- [ ] **Step 3: Change the wording**

In `server/internal/handler/subs.go`, replace `subLine` (its doc comment included) with:

```go
// subLine is one subscription on the list:
// "Alpha — sub.example.com — 32 servers — changed 2 h ago — OK".
func subLine(s vpnconfig.Subscription, now time.Time) string {
	where := s.Host()
	if s.Static() {
		where = "static list"
	}
	status := "OK"
	if s.Error != "" {
		status = s.Error
	}
	return fmt.Sprintf("%s — %s — %d servers — %s — %s", s.Name, where, len(s.Servers), changedAgo(now, s.Refreshed), status)
}

// changedAgo is when a subscription's list was last written, the way a list
// says it: "changed 2 h ago". watchd's periodic refresh writes a list only
// when it changed, so this is the last change, manual refresh or cleared
// error - not the last check; the status after it says how that went.
func changedAgo(now, t time.Time) string {
	if t.IsZero() {
		return "never changed"
	}
	return "changed " + timeAgo(now, t)
}
```

In `web/src/components/ServersTab.vue`, replace:

```html
            <th>Refreshed</th>
```

with:

```html
            <th>Changed</th>
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd server && gofmt -l ./internal/handler && go test ./internal/handler/; cd ../web && npm test`
Expected: no gofmt output; `ok`; `npm test` exits 0.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/subs.go server/internal/handler/subs_test.go web/src/components/ServersTab.vue web/test/servers-monitor.cjs
git commit -m "feat(ui): say when a subscription list last changed"
```

---

### Task 10: Documentation

**Files:**
- Modify: `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: the names Tasks 1–9 introduced: `ServerIdentity`, `MergeRefresh`, `PublishRefresh`, `RefreshSubscriptionFollowingRenames`, `SubscriptionFetcher.FetchList`, `StartRefresh`, `monitor.subscription_refresh`.
- Produces: rules that describe the shipped behaviour.

- [ ] **Step 1: watchd.md**

In `.claude/rules/watchd.md`, in the Layout block, replace:

```
server/internal/subwatch/         # fast/legacy failover, walk, preferred return, pending restore
```

with:

```
server/internal/subwatch/         # fast/legacy failover, walk, preferred return, pending restore, periodic refresh
```

and insert this section right before `## Durable notifications`:

```markdown
## Periodic subscription refresh

`(*Watch).StartRefresh` (`subwatch/refresh.go`) downloads every subscription with a link every
`monitor.subscription_refresh` (`5m` by default, `0` off, a value below `1m` the default with a
WARN), whatever `monitor.enabled` says: the failover walk wants current lists too. Its first round
starts a minute after watchd, each next one an interval after the previous one ended, and a round
that stands down looks again a minute later. A round stands down while VPN Director is stopped, the
compatibility gate is closed, or the watch handles an Xray failure - its probe failing
(`failSince`), an apply of its own pending, a `xray.failover` or a `xray.pending_restore`: the wave
refreshes then.

It downloads outside the tick through the wave's fetcher (`SubscriptionFetcher.FetchList`: the WAN,
then the tunnel; every listed server, without addresses where the host did not resolve), each
subscription within `FetchTimeout`; a stop or a closed gate ends the downloads within `stopPoll`. It
publishes between ticks, holding `tickMu`, after checking the stand-down conditions again, one
config-lock update per subscription (`vpnconfig.PublishRefresh`). It applies and restarts nothing.

`vpnconfig.MergeRefresh` merges the download with the file as it is under the lock. A server's
identity (`vpnconfig.ServerIdentity`) is its stored outbound without the REALITY `serverName`,
`shortId` and `spiderX` that 3x-ui and Marzban pick at random for every download. A fresh server
pairs with the first stored server of its identity and stays that stored record under the fresh
name - so its endpoint key, and its monitor status, survive; its addresses stay too when the fresh
ones are the same set or did not resolve this time. A merge equal to the file, with no error
recorded, writes nothing at all, `refreshed` included; anything else writes the list, `refreshed`,
a cleared error and `xray.servers` once. `active_server`, `preferred_server` and
`pending_restore.active` follow a renamed server in that write, `seq` unchanged: a rename is no
selection, and `sameRestoreActive` compares the records whole. A download that fails, or a merge
that comes out empty, records its error once (`RecordSubscriptionError`) and keeps the list.
Nothing goes to Telegram; the log gives the counts and every record that followed a rename, never a
link.

A successful probe remembers the active server's identity (`probeOKActive`): Xray dying within
about 30 s of a rename, before the next successful probe, leaves that episode to the legacy
confirmation. A stored REALITY pick that the server's admin removes leaves the stored copy dead until
a manual refresh or the wave, which take fresh copies.
```

- [ ] **Step 2: telegram-bot.md**

In `.claude/rules/telegram-bot.md`:

Replace:

```
│   │   ├── reach.go          # TCP look at a server's addresses; the unreachable streak
```

with:

```
│   │   ├── reach.go          # TCP look at a server's addresses; the unreachable streak
│   │   ├── refresh.go        # Periodic refresh of every subscription with a link, between ticks
```

Replace:

```
| `/subs` | `SubsHandler.HandleSubs` | Subscriptions with refresh, rename and delete buttons;
```

with:

```
| `/subs` | `SubsHandler.HandleSubs` | Subscriptions with refresh, rename and delete buttons, each line saying when its list last changed (`changed 2 h ago`);
```

Replace:

```
(`vpnconfig.RefreshSubscription`, which the Web UI and the bot refresh through too)
```

with:

```
(`vpnconfig.RefreshSubscription`; the Web UI and the bot refresh through `vpnconfig.RefreshSubscriptionFollowingRenames`, which also carries `active_server`, `preferred_server` and `pending_restore.active` over a server the list renamed, and watchd's periodic refresh through `vpnconfig.PublishRefresh`, which keeps a stored copy that differs only in the REALITY picks: see `watchd.md`)
```

- [ ] **Step 3: webui.md**

In `.claude/rules/webui.md`:

Replace:

```
| GET | `/api/subscriptions` | Every subscription: `id`, `name`, `host`, `static`, `servers` (the count), `added`, `refreshed`, `error`. No link |
```

with:

```
| GET | `/api/subscriptions` | Every subscription: `id`, `name`, `host`, `static`, `servers` (the count), `added`, `refreshed` (when its list was last written: the Servers tab's Changed column), `error`. No link |
```

Replace these two lines (the sentence wraps):

```
records it in its `error`, unless a refresh that succeeded meanwhile has moved
its `refreshed`. Every write of a subscription also deletes the `servers.json`
```

with:

```
records it in its `error`, unless a refresh that succeeded meanwhile has moved
its `refreshed`. A refresh, and an add of a saved link, carry `active_server`,
`preferred_server` and `pending_restore.active` over a server the fresh list
renamed - the same server by `vpnconfig.ServerIdentity`, its outbound without
the REALITY picks - without moving `seq`
(`vpnconfig.RefreshSubscriptionFollowingRenames`): a panel that puts the
traffic left into every name would otherwise lose the Active mark at each
refresh. watchd's periodic refresh writes a list only when it changed
(`watchd.md`), so `refreshed` is when the list last changed. Every write of a
subscription also deletes the `servers.json`
```

- [ ] **Step 4: CLAUDE.md**

In `CLAUDE.md`:

Replace:

```
| `server/internal/subwatch/` | Watchd's SOCKS probe, subscription walk, failover, preferred return and pending restore |
```

with:

```
| `server/internal/subwatch/` | Watchd's SOCKS probe, subscription walk, failover, preferred return, pending restore and periodic subscription refresh |
```

Replace:

```
`concurrency` (8), `log_level`. The daemon rereads it every minute.
```

with:

```
`concurrency` (8), `log_level`, `subscription_refresh` (`5m`, how often watchd downloads every subscription with a link and writes what changed; `0` off, whatever `enabled` says). The daemon rereads it every minute.
```

- [ ] **Step 5: Check every replacement landed**

Run: `grep -c "Periodic subscription refresh" .claude/rules/watchd.md; grep -c "refresh.go" .claude/rules/telegram-bot.md; grep -c "RefreshSubscriptionFollowingRenames" .claude/rules/telegram-bot.md .claude/rules/webui.md; grep -c "subscription_refresh" CLAUDE.md`
Expected: `1`; `1`; `1` for each file; `1`.

- [ ] **Step 6: Commit**

```bash
git add .claude/rules/watchd.md .claude/rules/telegram-bot.md .claude/rules/webui.md CLAUDE.md
git commit -m "docs: describe the periodic subscription refresh"
```

---

### Task 11: Whole-branch verification

**Files:** none changed unless a check fails.

**Interfaces:**
- Consumes: everything above.
- Produces: evidence that the branch builds and its suites pass.

- [ ] **Step 1: Format and vet**

Run: `cd server && gofmt -l . && go vet ./...`
Expected: no output from either.

- [ ] **Step 2: The Go suite**

Run: `make -C server test`
Expected: every package `ok`, no `FAIL`.

- [ ] **Step 3: The concurrent packages under the race detector**

Run: `cd server && go test -race ./internal/subwatch/ ./internal/vpnconfig/ ./internal/service/ ./internal/monitor/ ./internal/wizard/ ./cmd/watchd/`
Expected: every package `ok`, no `DATA RACE`.

- [ ] **Step 4: The SPA**

Run: `cd web && npm test`
Expected: exit 0.

- [ ] **Step 5: The daemons build for the routers**

Run: `cd server && GOOS=linux GOARCH=arm64 go build ./cmd/watchd ./cmd/bot && GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build ./cmd/watchd`
Expected: no output, exit 0. Delete the produced binaries afterwards (`rm -f watchd bot`).

- [ ] **Step 6: Nothing outside the plan changed**

Run: `git diff --stat master...HEAD -- router/ ':!router/opt/vpn-director/vpn-director.json.template'`
Expected: empty: the shell, the init scripts and the hooks are untouched.

If any step fails, fix it in the task that owns the code, rerun that task's tests, commit the fix with a message naming what it fixes, and run this task again.
