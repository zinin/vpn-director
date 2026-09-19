# Xray watch: fast death, return to the preferred server, bounded shell DNS

Date: 2026-09-19
Branch: `feature/xray-watch-fast-death-and-return`
Status: approved in brainstorming (three parts), awaiting implementation plan

## 1. Why

The first night of v0.14.0 on the RT-AX86U brought four real failovers (23:22, 01:23, 09:18,
10:56). Each time the provider's six foreign endpoints (`143.20.x.x`, `87.85.x.x`) stopped
answering at the IP level for about ten minutes: no ping, no TCP on 443, while the WAN reached
Google and Cloudflare and the provider's Russian "Whitelist" relays answered. The subscription
itself did not change (62 servers on the same 9 endpoints).

The watch handled every episode as designed. Three costs stood out:

1. **~3.5 minutes without internet.** `DeadAfter` waits 3 minutes after the first failed probe,
   and the move takes another 26–41 s.
2. **~20 s inside roughly every second TPROXY apply.** `TPROXY_BYPASS` resolves the five OpenVPN
   client hostnames through BusyBox 1.25 `nslookup`, which asks glibc 2.26 for A and AAAA at
   once, straight at `8.8.8.8`/`8.8.4.4` with the default 5 s timeout, two attempts and two
   servers. Measured: `mil-it.fromblancwithlove.com` 5.1 s per lookup, `yop.ru` 0.25–6.2 s. The
   failover's move and the restore each contain such an apply.
3. **No way back.** After an episode the clients stay on the server the walk picked (Madrid,
   once Vilnius) while `preferred_server` keeps the user's choice (Oslo) until the next death.

## 2. Goals and non-goals

Goals:

- Declare the outbound dead after about a minute when its server does not accept TCP.
- Return to the preferred server once it works again.
- Bound the shell resolver's wait.

Non-goals, considered and not chosen:

- **Reachable-first walk order.** It would cut the ~6 minutes on the tunnel to seconds, but in
  these outages the only reachable servers were the Russian relays, and the clients would stay
  there. The tunnel carries the clients meanwhile; the owner did not pick this.
- **A second, probing Xray process** for the return: no interruption for failed attempts, at the
  price of another process, its lifecycle and a separate config. Rejected as too much code.
- Changing `DeadAfter` for failures other than an unreachable server; configurable timings;
  IPv6; resolving through dnsmasq (`127.0.0.1`, cached) — the bounded timeout is enough.

## 3. Fast death

Where: the healthy branch of `Tick`, after a failed SOCKS probe, while no failover exists.

**The check.** The watch looks up the active server's entry in `servers.json` with
`chosenIndex(servers, active_server)` — name, address and port, else the first entry with that
name, as the walk does. It dials TCP (`tcp4`, `ReachTimeout` = 3 s) to every IPv4 address of
that entry on the entry's port, all at once. The addresses are the entry's `ips`; an entry
without any uses its `address` when that is an IPv4 literal. The server is **up** when one
address accepts, **down** when none does.

The check has no answer — and the fast rule stays out of it — when there is no
`active_server`, no matching entry, no IPv4 address to dial, `LoadServers` fails, or the
watch has no `Reachable`. `Reachable` reports an address it refuses to dial (private or
reserved, `ssrf.IsPrivateOrReserved`) as up, so the rule never acts on it either.

**The rule.** The outbound is dead when either holds:

- the probe has failed for `DeadAfter` (3 minutes), as today;
- the probe has failed for `FastDeadAfter` (60 s), and every check since the first failed probe
  found the server down, with at least two checks.

With the 30 s tick that is misses at 0, 30 and 60 s. The streak belongs to the current
`failSince`: whatever resets `failSince` (a successful probe, a stop, an unarmed tick, a pick
without a fallback) resets it too, and a check that finds the server up or has no answer ends it
for this `failSince`.

Everything after "declared dead" stays as it is: stage, commit, refresh, walk, restore.

**Logging.** `Xray outbound declared dead` gains `reason=unreachable` or `reason=probe`.
Telegram text does not change.

**Why this is safe.** A local failure — Xray restarting, a broken config — leaves the server
accepting TCP, so the 3-minute rule applies. A dead WAN fails both checks; the failover then
comes two minutes sooner, and the tunnel over the same WAN carries nothing either way. Stale
`ips` in `servers.json` for a server whose name resolves elsewhere can only matter while the
probe already fails.

## 4. Return to the preferred server

**Preconditions**, all on the same tick:

- the health probe succeeded;
- no `xray.failover`, no pending apply (`pendingApply`), no pending restore (`pendingRestore`);
- `preferred_server` is set and `active_server` names another server;
- the time is past `returnNotBefore`;
- `LoadServers`, `Reachable` and `Generate` are wired.

**Cadence.** `ReturnCheck` (5 minutes) between checks. The first check comes on the first
healthy tick after the process starts. Every completed restore — the tick's own, the walk's
pick, a retried restore apply — sets `returnNotBefore` to now plus `ReturnCheck`: the preferred
server failed minutes ago.

**The check.** Find the preferred entry with `chosenIndex(servers, preferred_server)`. Its
`perAddress` copies carry one address each, chosen as in §3 (an `ips` entry, or an IPv4-literal
`address`). No entry, or no copy with an IPv4 address: wait `ReturnCheck`. Otherwise dial every
copy at once; the candidates are the copies whose address accepted, in list order. None: wait
`ReturnCheck`. Nothing is written and Xray is not restarted.

**The attempt**, candidate by candidate (usually one):

1. `Generate(c, guard)` with the walk's guard (`walkGuard`: stop under the config lock, saved
   link unchanged, no newer selection by identity and `seq`). The bot's `Generate` writes
   `config.json` for `ServerForDial(c)` and records the entry through
   `GenerateAndRecordWalkedServer`; `RecordWalkedServer` clears `preferred_server` because the
   name comes back. A refusal ends the attempt with nothing more written.
2. `restart xray-process --unless-stopped`; a skipped restart ends the attempt.
3. Wait `SettleAfterRestart`, probe through SOCKS.
4. Success: log `Xray returned to the preferred server`, send `msgReturned`
   ("Xray back on the preferred server %s", note kind `noteReturned`), reset the retry interval,
   remember the candidate as `lastPicked`.

**Rollback**, when no candidate probes live:

- The previous server is the entry `active_server` named when the attempt began. Its candidates
  are `lastPicked` first when that copy belongs to this entry, then the entry's other
  `perAddress` copies. `lastPicked` is the copy the walk picked or a return proved; a restart of
  the bot forgets it, and the rollback then tries the entry's addresses in order.
- Each candidate: `Generate` with the guard carried on from the attempt's last record, restart,
  settle, probe. The first live one ends the rollback. `RecordWalkedServer` puts the preferred
  server back into `preferred_server`, since the active name leaves it again.
- No live candidate: nothing more; the next tick's health probe fails and the death path takes
  over.
- The next attempt waits `ReturnRetry` (10 minutes), doubling up to `ReturnRetryMax`
  (30 minutes). Log: `Return to the preferred server failed; back on the previous server` and
  `Next return attempt backed off`. No Telegram message.

**The retry interval** resets on a successful return, on a death, and on any tick without a
`preferred_server`.

**Concurrency.** Every write goes through `Generate` with a guard, under the config lock. A Web
UI or `/xray` selection during the attempt refuses the watch's next write, and the selection
stands; it also clears `preferred_server`, which ends the returns. A `/stop` ends the attempt the
way it ends the walk. A bot restart in the middle leaves the preferred server active and
`preferred_server` cleared; if it does not work, the death path handles it. The walk runs only
while failed over and the return only while not, both inside `Tick` under `w.mu`.

**Cost.** A successful return restarts Xray once: 1–2 s of pause for the Xray clients. A failed
attempt restarts it twice; the TCP check filters out almost all of them.

## 5. Bounded shell DNS

`_resolve_ip_impl` in `lib/common.sh` calls `nslookup` on two paths (first match and `-a`). Both
go through one helper that runs `RES_OPTIONS="timeout:1 attempts:2" nslookup "$@"`.

- glibc reads `RES_OPTIONS` (measured on the RT-AX86U: a slow lookup took 1.1–2.1 s with
  `timeout:1` instead of 5 s). A stuck name costs at most about 4 s instead of about 20 s;
  answers do not change.
- Every shell resolution benefits: `TPROXY_BYPASS`, `import_server_list.sh`,
  `block/allow_wan_for_host`, `resolve_lan_ip`.
- KeeneticOS already resolves through its local proxy with `timeout:1 attempts:1`; the variable
  raises attempts to two there, which is harmless.

## 6. Components

| File | Change |
|------|--------|
| `server/internal/subwatch/watch.go` | Fields `LoadServers`, `Reachable`; state for the reach streak, `returnNotBefore`, `returnRetry`, `lastPicked`; constants `FastDeadAfter`, `ReachTimeout`, `ReturnCheck`, `ReturnRetry`, `ReturnRetryMax`; the death rule; the return hook after a healthy probe; `lastPicked` set at the walk's pick; `returnNotBefore` set at every completed restore |
| `server/internal/subwatch/reach.go` (new) | The reachability check of an entry's addresses |
| `server/internal/subwatch/return.go` (new) | Attempt, rollback, backoff, `msgReturned` |
| `server/internal/bot/reach.go` (new) | `Reachable`: `net.Dialer` with `tcp4` and `ReachTimeout`; private and reserved addresses are reported up without a dial; the dialer is injectable for tests |
| `server/internal/bot/bot.go` | Wire `LoadServers: configSvc.LoadServers` and `Reachable` |
| `router/opt/vpn-director/lib/common.sh` | The `nslookup` helper with `RES_OPTIONS` |
| `.claude/rules/telegram-bot.md` | Fast death and the return in "Subscription watch" |
| `.claude/rules/shell-conventions.md` | The DNS pitfall: the shell resolver bounds its wait |

Constants:

| Constant | Value |
|----------|-------|
| `FastDeadAfter` | 60 s |
| `ReachTimeout` | 3 s |
| `ReturnCheck` | 5 minutes |
| `ReturnRetry` | 10 minutes, doubling |
| `ReturnRetryMax` | 30 minutes |

## 7. Testing

Go, `subwatch` (fakes and the fake clock; `-race`):

- Fast death: every check down → dead at 60 s and not before; the server up → dead at 3 minutes;
  one up check inside the streak → 3 minutes; no entry, no IPv4, `LoadServers` error → 3 minutes;
  a successful probe resets the streak; the log carries the reason.
- Reach: all addresses down → down; one up → up; an entry without `ips` and an IPv4 literal
  address → that address; a hostname without `ips` → no answer.
- Return: success → one `Generate` of the preferred copy, `preferred_server` cleared through
  `RecordWalkedServer`, one message, retry reset; failure → rollback to `lastPicked`'s address,
  `preferred_server` back, then 10, 20, 30 minutes; nothing reachable → no `Generate`, next check
  after 5 minutes; a restore → the first check 5 minutes later; no return while failed over,
  while `pendingApply` or `pendingRestore`; a Web UI selection or `/stop` during the attempt →
  no further writes; after a bot restart the rollback walks the previous entry's addresses.

Go, `bot`: `Reachable` against a local listener through the injected dialer; a closed port →
down; a private address → up without a dial.

Bats, `router/test/common.bats`: the `nslookup` mock records `RES_OPTIONS`, and both
`resolve_ip` and `resolve_ip -a` pass `timeout:1 attempts:2`.

Verification: `go build`, `go vet`, `go test ./... -count=1` (24 packages); `-race` on
subwatch, vpnconfig, service, bot, webapi, handler, wizard; gofmt against the baseline; bats;
shellcheck against the baseline.

## 8. Device check (owner's permission)

RT-AX86U, after deploying the build:

- **Right away.** `active_server` is Madrid and `preferred_server` Oslo. The first healthy tick
  dials `87.85.136.209:443`; when it answers, Xray switches to Oslo and Telegram says so. The
  config then has no `preferred_server`.
- **At the next natural outage.** `Xray outbound declared dead` with `reason=unreachable` about a
  minute after the first miss. The clients land on `ovpnc2` within about 10 s, with no ~20 s in
  the apply. The walk and the restore run as before. Once Oslo answers TCP again, the watch
  returns to it within 5 minutes.

## 9. Risks

- Three TCP dials lost in a row to a working server would declare a false death; the clients then
  sit on the tunnel until a probe succeeds. Rare, and no worse than a failover.
- A preferred server that accepts TCP but is blocked at the protocol level costs two short Xray
  pauses every 30 minutes. Acceptable; a cap can come later.
- A subscription entry pointing at a private address never gets the fast rule and returns
  without the TCP gate.
