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

✅ Done — see commit(s): `7ea7120`, `13ffe23`

---

### Task 2: Publish a periodic refresh only when its list changed

✅ Done — see commit(s): `a2e3ab3`

---

### Task 3: A manual refresh carries the records over a rename

✅ Done — see commit(s): `addb3bb`

---

### Task 4: The fetcher lists every server, resolved or not

✅ Done — see commit(s): `fb8b7e4`

---

### Task 5: The `monitor.subscription_refresh` setting

✅ Done — see commit(s): `5baab38`

---

### Task 6: The periodic refresh loop

✅ Done — see commit(s): `a64ae9c`, `8357ceb`

---

### Task 7: Start the loop in watchd

✅ Done — see commit(s): `933f324`, `1c78c14`

---

### Task 8: The bot wizard finds a pick a refresh renamed

✅ Done — see commit(s): `2135d5a`

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
