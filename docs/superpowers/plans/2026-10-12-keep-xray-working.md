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

✅ Done — see commit(s): `48b7dde`, `aad8abf`

---

### Task 2: Is the selected server current, changed, moved or gone?

✅ Done — see commit(s): `e546ec1`, `6974626`

---

### Task 3: Order candidates by group, subscription and latency; the walk uses it

✅ Done — see commit(s): `636db79`

---

### Task 4: The fast path tries the user's group first

✅ Done — see commit(s): `ae95a63`

---

### Task 5: Keep Xray on a working server, with or without clients

✅ Done — see commit(s): `3afdb59`, `4e4b009`, `e4d65dc`; after the final review `805be71`, `b29852a`, `a5a721a`, `588729d`

---

### Task 6: Return after five minutes alive, and back to the user's group

✅ Done — see commit(s): `6ff4a87`, `2aa5d2d`; after the final review `123f764`

---

### Task 7: Documentation

✅ Done — see commit(s): `ee8cc74`; after the final review `9a5a2d7`, `bc4446d`

---

### Task 8: Whole-branch verification

✅ Done — verification only, no commits; last full run green at `588729d`
