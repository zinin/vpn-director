# Xray Subscription Watch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist the VLESS subscription URL and, in the Telegram bot, watch the Xray outbound through SOCKS; after three minutes of consecutive failures move Xray LAN clients onto a Tunnel Director exit, refresh the subscription, pick the same name or the first working server, then restore the clients.

**Architecture:** Config mutations (`subscription_url`, `failover`, TD exit filter, move/restore) live in `vpnconfig` as pure functions. SOCKS probe and the watch loop live in `internal/subwatch` with injected I/O. The bot wires the loop next to PathManager (not in `--dev`), fetches the subscription WAN-first then `DialPath` through a TD tunnel, and notifies active chats. Web UI and `/import` only save and reuse the URL; they do not run the watch.

**Tech Stack:** Go 1.x (existing module), `golang.org/x/net/proxy` SOCKS5, Vue 3 Servers tab, `vpn-director.json` via `ConfigStore.UpdateVPNConfig`.

**Spec:** `docs/superpowers/specs/2026-09-16-xray-subscription-watch-design.md`

## Global Constraints

- Watch loop runs only inside `telegram-bot`. No watch in `webui`. No new daemon.
- `--dev` does not start SubscriptionWatch.
- Probe URL is `https://www.gstatic.com/generate_204`; success is HTTP 204 through SOCKS on `127.0.0.1:<socks_port>` (default 12346). Missing SOCKS listener is a failed probe.
- Probe interval 30 s. Dead after 3 minutes of consecutive failed probes (six ticks). Import retry while failed over: 5 minutes. Settle 3 s after Xray restart.
- Timing values are constants, not `advanced.*` knobs.
- TD fallback uses the same candidate filter as PathManager, **not** a Telegram probe: key in `tunnel_director.tunnels`, not `main`, `clients` length ≥ 1, platform `connected` with non-empty `iface`. Sort ids, take the first.
- Xray clients must leave `xray.clients` during failover or TPROXY wins.
- `xray.subscription_url` is a secret. `GET /api/config` blanks it. Do not put a real subscription token in tests, logs, or commits.
- `xray.failover` may appear in `/api/config`.
- `configure.sh` is not failover-aware. Do not change it in this plan.
- PathManager stickiness is unchanged.
- IPv6 is out of scope; SOCKS and fetch dial `tcp4`.
- English for code, comments, commits, docs, Telegram watch text. Do not commit `docs/superpowers/` in a later PR (git-rm before the PR). Stage by name, never `git add -A`.
- Tests: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./<pkg> -count=1`.

## File map

| File | Role |
|------|------|
| `server/internal/vpnconfig/vpnconfig.go` | `SubscriptionURL`, `Failover *XrayFailover` on `XrayConfig` |
| Create `server/internal/vpnconfig/failover.go` | `XrayFailover`, `TDExits`, `FirstTDExit`, `EffectiveXrayClients`, `Armed`, `MoveXrayClientsToTunnel`, `RestoreXrayClientsFromFailover` |
| `server/internal/bot/path.go` | `candidates` uses `TDExits` so the filter cannot drift |
| `server/internal/webapi/handler_servers.go` | `subscription_saved`; resolve empty import URL; persist URL on import |
| `server/internal/webapi/handler_logs.go` | Blank `subscription_url` on `GET /api/config` |
| `server/internal/handler/import.go` | `/import` with no args uses saved URL; with args saves it |
| `web/src/types.ts`, `web/src/api.ts`, `web/src/components/ServersTab.vue` | Re-import from saved URL |
| Create `server/internal/subwatch/` | Probe + `Watch.Tick` |
| Create `server/internal/bot/subfetch.go` | WAN then `DialPath` download |
| `server/internal/bot/bot.go` | Start/stop the watch with PathManager |
| `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md` | Document the behaviour |

---

### Task 1: Config types and failover mutations
✅ Done — see commit(s): `620af51`

### Task 2: Web API — save, redact, re-import URL
✅ Done — see commit(s): `82e341e`

### Task 3: Bot `/import` saves and reuses the URL
✅ Done — see commit(s): `27ab989`

### Task 4: Servers tab re-import
✅ Done — see commit(s): `d8683ff`

### Task 5: SOCKS probe
✅ Done — see commit(s): `08b0124`

### Task 6: Watch — arming, death timer, failover, notify once
✅ Done — see commit(s): `436e23d`

### Task 7: Watch — import, pick server, restore
✅ Done — see commit(s): `418d18d`

### Task 8: Wire the bot, DialPath fetch, docs
✅ Done — see commit(s): `c8e9ddb`

Post-plan fix wave (final review): `3b85ab7` — keep failover until Apply succeeds; sync `xray.servers` on watch import; WAN fetch once.

---

## Self-review

**Spec coverage:** Tasks 1–8 map to spec §§4–15 as originally written. Residual after final review: `lastImport` cleared on failed restore-Apply (`watch.go:314`) — see continuation prompt.

**Note for executors:** Spec §13 says "six consecutive failed probes"; §5 says 3 minutes. Code implements `>= DeadAfter` (3 minutes).
