# Xray subscription watch: design

Date: 2026-09-16
Branch: `feature/xray-subscription-watch`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

When the active VLESS server dies (the provider bans the endpoint), LAN
clients on Xray TPROXY recover without a person pasting a subscription URL.

The bot watches the Xray outbound through the local SOCKS inbound. After
several minutes of consecutive failures it moves those Xray clients onto a
Tunnel Director tunnel, refreshes the saved subscription, and tries a new
server: the same name first, then the first server whose outbound actually
works. When a probe succeeds, the clients return to Xray.

This is the live failure on the author's RT-AX86U: a BlancVPN-style
subscription rotates endpoints daily, the Web UI cannot re-import without
pasting the URL (the link is never stored), and until someone imports again
the Xray clients have no internet. `ovpnc2` is already a working TD exit.

## 2. Why the current import cannot do this

`POST /api/servers/import` and the bot's `/import <url>` download the URL,
write `servers.json`, and forget the link. The Servers tab's "Refresh"
reloads the list from disk. There is no health check of the active outbound.
`xray.active_server` records name, address and port, but nothing notices
when that outbound stops carrying HTTPS.

Xray wins over Tunnel Director: a client in `xray.clients` never reaches
`TUN_DIR`. Failover therefore removes those addresses from `xray.clients`
for the duration, or TPROXY keeps eating the packets.

## 3. Architecture

Everything that loops runs inside `telegram-bot`, next to PathManager, not
instead of it. No new daemon. If the bot is not installed, behaviour stays
as today: manual import. A watchdog in `webui` is out of scope.

| Piece | Responsibility |
|-------|----------------|
| PathManager | Path to `api.telegram.org` for the bot itself. Unchanged. |
| SubscriptionWatch | Health of the VLESS outbound for LAN Xray clients; failover; import; server pick; restore. |
| Web UI / `/import` | Persist the subscription URL; re-import from the saved URL. No monitor loop. |

Shared code: persist and read the URL; download and decode a subscription
(the existing importer, plus writing the URL). Telegram text and the watch
loop live only in the bot.

While VLESS is dead, LAN Xray clients are off TPROXY and on the first TD
candidate (same filter as the bot, no Telegram probe). Probes of
replacement VLESS servers use `127.0.0.1` SOCKS, so household machines
stay on the tunnel until a probe succeeds.

`--dev` does not start SubscriptionWatch (same reason as PathManager: no
platform tunnels, no real Xray).

### 3.1 Alternatives rejected

- **Watch loop in both daemons with a lock.** The author runs both, so the
  extra process only adds double-failover risk. Revisit if a webui-only
  install needs this.
- **Cron + `vpn-director.sh`.** SOCKS probes, `GenerateAndRecordActiveServer`
  and Telegram already live in Go.
- **TCP to the VLESS `address:port` as the health signal.** The user sees
  "nothing opens through this server"; a listening banned endpoint would
  look healthy. The probe is HTTPS through SOCKS.
- **Leave clients in `xray.clients` during failover.** TPROXY has absolute
  priority; TUN_DIR would never see them.

## 4. Arming

The watch is armed when both are true:

1. `xray.subscription_url` is non-empty.
2. There is someone to protect: `xray.clients` is non-empty after subtracting
   `paused_clients`, or a `xray.failover` record is present (bot restarted
   mid-failover).

Otherwise the loop is idle: no probes, no messages.

## 5. Timing (constants, not config)

| Constant | Value |
|----------|--------|
| Probe interval | 30 s |
| Dead after | 3 minutes of consecutive failed probes |
| Import retry while failed over | 5 minutes |
| Probe URL | `https://www.gstatic.com/generate_204` |
| Success | HTTP 204 through SOCKS (no body required) |
| Settle after Xray restart | 3 s before the next SOCKS probe |

No `advanced.*` knobs in this change.

## 6. Probe

Each cycle dials the probe URL through Xray SOCKS at
`127.0.0.1:<advanced.xray.socks_port>` (default 12346). A missing listener
is a failed probe, same as a dead outbound.

Success resets the consecutive-failure timer. The watch does not declare
the outbound dead on a single miss.

## 7. Failover

When the outbound is dead:

1. Snapshot the effective Xray clients (in `xray.clients`, not in
   `paused_clients`). Addresses already in the target tunnel's `clients`
   are not duplicated.
2. Pick a Tunnel Director tunnel with the **same candidate filter as
   PathManager**, not a Telegram probe: key in `tunnel_director.tunnels`,
   not `main`, `clients` length ≥ 1 (pause leaves the address in
   `clients`), platform lists the id `connected` with a non-empty `iface`.
   Sort those ids, take the first. OpenVPN and WireGuard are equal.
   Firmware tunnels that TD does not use (on the RT-AX86U: `ovpnc1`,
   `ovpnc3`) are not candidates. A tunnel that can carry LAN internet but
   not `api.telegram.org` is still a valid failover.
3. If a tunnel exists: under the config lock, **append** the snapshot to
   that tunnel's `clients`, **remove** those addresses from `xray.clients`,
   write `xray.failover` `{tunnel, clients}` in the same write. Then
   `vpn-director.sh apply`.
4. If no tunnel exists: do not move clients. Still import and walk servers.
   Telegram says Xray is dead and there is no fallback tunnel.

`xray.failover` is the restart source of truth. After `bot.New`, a present
record means "we are failed over"; a live SOCKS does not by itself restore
clients.

A Web UI Select during failover changes only the outbound. LAN stays on
the tunnel until step 9 restores them.

`configure.sh` overwrites `xray.clients` from the wizard. A wizard run
during failover can disagree with `xray.failover`. Out of scope: do not
run the wizard while failed over.

## 8. Import and server pick

Download the saved URL with the existing SSRF client (HTTPS only). Prefer
direct WAN. If that fails, dial through a live TD tunnel with the same
`DialPath` the bot already has (`SO_BINDTODEVICE` + mark). The subscription
host is usually a CDN and reachable on WAN; the tunnel is the backup.

An empty body, HTTP error, or decode failure **does not** replace
`servers.json`. Stay failed over (if we already are) and retry in 5
minutes.

On a successful decode: save `servers.json` as today, keep
`xray.subscription_url`.

Pick a server:

1. If `xray.active_server.name` matches an imported name, try that entry
   once (`GenerateAndRecordActiveServer`, restart Xray, settle, SOCKS
   probe). Same name, new address/port is the usual rotation.
2. If that probe fails, or no name matches, walk the imported list in
   order. Skip an entry already tried in (1). Stop at the first successful
   SOCKS probe.

LAN clients stay off `xray.clients` for the whole walk.

## 9. Restore

The first successful probe:

1. Under the config lock, put `failover.clients` back into `xray.clients`
   (no duplicates), remove **only those addresses** from
   `tunnel_director.tunnels[failover.tunnel].clients`, delete
   `xray.failover`.
2. `vpn-director.sh apply`.
3. Telegram: back on Xray, server name.

Original tunnel clients (for example `192.168.1.3` on `ovpnc2`) stay.

PathManager is unchanged. The bot may remain on `tunnel:ovpnc2` because of
path stickiness. LAN clients are on Xray again. That split is accepted.

## 10. Persistence and the Web UI

`vpn-director.json`:

```json
"xray": {
  "subscription_url": "https://…",
  "failover": {
    "tunnel": "ovpnc2",
    "clients": ["192.168.1.8"]
  }
}
```

`subscription_url` is a secret (token in the path). `GET /api/config`
blanks it the same way it blanks `jwt_secret`. `failover` may appear in
that response (LAN addresses and a tunnel id are not credentials).

The Servers API reports whether a URL is saved (`subscription_saved: true`)
without echoing it. Import with a URL writes `subscription_url`. Import
with an empty URL uses the saved one; if none is saved, the handler returns
an explicit error (today: "url is required"). The bot's `/import` without
arguments does the same; `/import <url>` saves the new link.

The Servers tab grows a control that re-imports from the saved URL when
`subscription_saved` is true. Pasting a URL still replaces the saved link.

`configure.sh` already merges the existing file over the template and only
replaces `xray.clients`, `exclude_sets`, `servers`, `active_server`. The
new keys survive a wizard save. The template need not list them.

## 11. Notifications

Telegram, one message per **state change**, not per probe:

1. Xray dead, clients moved to `tunnel:<id>` (or: dead, no fallback tunnel).
2. Subscription refreshed, selected server `<name>` (or: refresh failed /
   no live server, still on the tunnel).
3. Clients back on Xray, server `<name>`.

Repeating the same state does not send again. A later import wave that
still finds no live server does not re-send (2) until something else
changed (new count, restore, or a successful pick).

## 12. Errors

| Case | Behaviour |
|------|-----------|
| SOCKS down | Failed probe. |
| No TD candidate | No client move; import and walk still run. |
| Import fails | Keep `servers.json`; retry in 5 min; one message for that wave. |
| Every server dead | Stay failed over; import again in 5 min. |
| `apply` or Xray restart fails | Do not drop `failover` to guess. Retry. If the client move never landed, do not write `failover`. |
| `--dev` | Watch does not start. |
| Paused addresses | Not in the snapshot. |
| Overlap with UI | Config lock / `OpMutex` as today. The watch does not overlap itself. |
| URL replaced | Next import uses the new URL. The server list is not rolled back. |
| Bot stopped | No automatic recovery. |

## 13. Testing

Go tests with fakes (same style as PathManager). No live Telegram, no real
SOCKS in CI.

- Import with a URL writes `xray.subscription_url`; `GET /api/config`
  returns an empty string; list servers includes `subscription_saved`.
- Re-import with no URL, and `/import` with no arguments, use the saved
  URL. Missing URL is an error, not a download of `""`.
- After six consecutive failed probes (3 minutes at 30 s, fake clock),
  config has `failover`, snapshot addresses left `xray.clients` and joined
  the tunnel; `paused_clients` are absent from the snapshot; pre-existing
  tunnel clients remain.
- Restore removes only `failover.clients` from the tunnel, puts them back
  on Xray, and clears `failover`.
- `active_server.name` is tried first; a dead match continues down the list.
- A failed import does not write `servers.json`.
- No TD candidate: clients stay in `xray.clients`; import is still called.
- `--dev` does not start the watch.
- Telegram is not sent twice for the same state.

No new bats unless `setup_telegram_bot.sh` changes (it should not).

## 14. Device check (owner's permission only)

RT-AX86U: VLESS outbound dead, `ovpnc2` connected, subscription reachable
on WAN. Expect: after ~3 minutes, Xray clients on `ovpnc2`; a refresh;
either the same name or another live server; clients back on Xray;
Telegram for each transition. KN-4521 is not required for this feature.

## 15. Out of scope

- Watch loop in `webui`
- Changing PathManager stickiness so the bot leaves the tunnel when SOCKS
  revives
- IPv6
- Making probe URL or timings configurable
- Signing or pinning the subscription
- Creating a Tunnel Director tunnel when none exists
- Making `configure.sh` failover-aware
