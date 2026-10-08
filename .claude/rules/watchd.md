---
paths: "server/cmd/watchd/**/*, server/internal/monitor/**/*, server/internal/watchdapi/**/*, server/internal/endpoint/**/*, server/internal/subwatch/**/*, server/internal/watchcompat/**/*, server/internal/notifications/**/*, router/opt/etc/init.d/S98vpn-director-watchd, install.sh"
---

# Monitoring and automation (vpn-director-watchd)

`vpn-director-watchd` owns endpoint monitoring, the subscription watch (`subwatch`) and the
durable notification store. The watch probes the active Xray, refreshes subscriptions, walks
servers, fails over LAN clients and restores them. The bot manages the router and delivers
watchd events; neither its process, token nor Telegram connectivity is required for automation.

Monitor and watch are independent workers. `monitor.enabled=false` stops endpoint checks and
the prober, but watchd keeps the legacy SOCKS/TCP failover path. `Watch: active` means the worker
is available, not that it is armed or that monitoring is enabled. A missing watchd disables both
monitoring and automatic failover; starting the new bot alone never creates a replacement watch.

## Layout

```
server/cmd/watchd/main.go          # flags, logging, readers, DI; self-update runs first
server/cmd/watchd/runtime.go       # socket ownership, monitor/watch/queue start and drain
server/cmd/watchd/watch.go         # shared netpath readiness, generation, compatibility gate
server/cmd/watchd/health.go        # subscription-health event producer
server/internal/subwatch/         # fast/legacy failover, walk, preferred return, pending restore
server/internal/watchcompat/      # read-only installed/running bot capability checks
server/internal/notifications/    # sole-writer durable events and per-chat progress
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

## Compatibility and lifecycle

Before any watch mutation, `watchcompat.Gate` checks the installed
`/opt/vpn-director/telegram-bot` **and every running bot executable** through `/proc/PID/exe`.
Only processes running as root count as running bot copies; same-named processes of other
users are neither executed nor considered.
It recognizes an old executable that was replaced or deleted; checking only the new installed
path cannot attest the old running process. PID/starttime and executable identity changes
invalidate cached results. Unreadable or uncertain identities fail closed for automation.

A compatible bot answers `--watchd-capabilities` with
`{"protocol_version":1,"watch_owner":"watchd"}`. The bot handles this read-only flag before
platform/config/logging/Telegram initialization, even without a token; early `self-update`
remains the first startup contract. The gate runs the executable from `/` with a 2-second bound
and a 4096-byte output limit. Old, malformed or incompatible replies leave `Watch: incompatible`:
no apply, refresh publication, config mutation or automatic Xray restart, while monitoring and
IPC remain available. An absent bot with no running copy, or a compatible stopped/no-token bot,
allows watchd. Do not start the bot or add a token merely to open the gate.

Watchd acquires lifetime socket ownership before building its workers or touching the prober.
A second instance cannot replace the first socket or start another watch/prober. Shutdown
cancels and drains monitor/watch/API workers, stops only `vpn-director-probe`, flushes the queue
and releases the socket. It cancels read-only commands, downloads and validation, while an
automatic apply or Xray process restart already running finishes (bounded by its timeout), so
main Xray and current routes are not left half-changed. It preserves main Xray, ready/stopped
markers and current routes. `S98vpn-director-watchd stop` therefore waits up to 320 seconds
before SIGKILL: a watchd killed sooner leaves that command running on its own, holding the
VPN Director lock, while stop reports watchd stopped.
Stopping the bot stops delivery/management, not watchd automation. These daemon stops differ
from `vpn-director.sh stop` or bot `/stop`, which fence automation and tear down routing.

## Installation and mixed-version recovery

`install.sh` installs watchd as an optional download and starts it after the unified config
exists, independently of bot installation/token. Success names both monitoring and automatic
failover; it does not promise that the compatibility gate is open. Missing/non-executable binary
or init, failed download/move/chmod, or failed start names the failed path and a recovery action.
Optional failure still permits Web UI startup. Inspect `/tmp/vpn-director-watchd.log` for startup
errors; restore a missing binary/init by rerunning the installer, then start watchd through
`/opt/etc/init.d/S98vpn-director-watchd`. `check` checks the process, not readiness of automation.

The unified updater keeps its daemon table order: bot, watchd, Web UI (Web UI last). Successful
startup starts non-bot daemons first, writes the terminal update status, then starts bot last.
A first introduction starts new watchd after its copy succeeds; an existing daemon stopped by
the owner stays stopped. Old installed/running/deleted bot bytes may keep the new watch
`incompatible` until the compatible bot is installed and old processes have exited.

Recovery is a restart net, not a rollback of copied files. Partial copy/start/notify/late failure
restores originally running daemons without starting originally stopped ones. An attempted new
daemon is stopped and its owned first-copy binary cleaned up; a retry can introduce it again.
The installed tree may contain mixed versions after failure. Keep failed update logs and repair
that installation; do not disable the capability gate or weaken notification/ownership contracts.
See `telegram-bot.md` for the unchanged early self-update/handover protocol.

## Subscription watch and failover

The watch is armed by subscriptions plus effective Xray clients, by an existing failover, or by
pending recovery intent. A static subscription counts; an unreadable subscription list does not
silently disarm clients. With no subscriptions, existing failover/restore intent is followed,
but the watch starts no new subscription-driven failover. Paused clients are excluded.

Every 30 seconds the watch checks HTTPS 204 through the main Xray SOCKS port. After a miss, the
fast path checks every active endpoint and at most three other distinct, previously live
connections in hybrid walk order. Fresh checks share one 30-second deadline and the monitor's
worker pool. Switching requires fresh active-dead/candidate-alive evidence and a working WAN;
evidence from an old endpoint set, prober/session, activity epoch or completion is not proof.
The config-lock guard revalidates ownership, subscription identity, inbound ports and evidence.
Incomplete/invalid monitor evidence falls back to the legacy path, not an invented healthy route.

The legacy path confirms death after 1 minute of proven TCP unreachability with a working WAN,
otherwise 3 minutes. It remains available with monitoring disabled/unavailable. UDP/QUIC-only
outbounds do not gain false TCP proof. The walk keeps `OwnFirst=3`, then round-robins subscriptions;
`endpoint.PerAddress` and `DialKey` deduplicate connections rather than provider labels. A refresh
that fails keeps the previous list; deleted/relinked subscriptions cannot be republished by an
old download. Preferred return waits 5 minutes, retries at 10–30 minutes, holds success for
30 minutes and stops after four failed returns. Details of the shared watch algorithms are in
`telegram-bot.md` and the `subwatch` modules; the process owner is watchd.

Only RFC1918 IPv4 clients/CIDRs that `vpnconfig.TDCarries` accepts can enter Tunnel Director
failover. Other Xray clients remain on Xray, not direct WAN. Staging retains Xray membership
until `failover_ready` proves the tunnel carries the clients. Restore waits for TPROXY readiness
and removes only tunnel memberships added by that failover, preserving manual assignments.
`xray.pending_restore` persists unfinished restore intent across watchd restart; startup
reconciles it against current selection, readiness and assignments before completing it.
A readiness/apply failure retains or restores the safe fallback instead of declaring recovery.

`/stop` and `/tmp/vpn-director/stopped` fence every mutation. Guards recheck after waits and under
the config lock; automatic apply/restart use `--unless-stopped`, with restart limited to
`restart xray-process`. Manual server selection increments `active_server.seq` even when the
same server is selected again, ending stale walks/returns. Subscription deletion/refresh and
client changes also invalidate stale writes. Recovery must preserve make-before-break ordering.

## Durable notifications

Watchd is the sole writer of `<data_dir>/watchd-notifications.json`, resolved from the selected
config (`0600`). Bot never opens/writes this file: it uses recipient, pending and ack IPC.
Events have a store epoch plus monotonic sequence ID, timestamp and text. Per-chat pending and
recent history each retain at most 20 events; TTL is 12 hours. Recipient synchronization uses
`chat_id` and `first_seen`: a new chat receives no event from before its first authorized use.
A known chat returning while recent history survives can recover still-eligible events; durable
closed progress prevents acked events from being re-added by recipient sync.

Writes stage a `0600` file, sync it, rename and sync the directory. Invalid storage is preserved
as `watchd-notifications.json.corrupt-*` before replacement. Write/sync/rename failure is visible
as `notifications.storage_error`; dirty RAM state retries saving every 10 seconds and at shutdown.
Monitoring and failover continue with the available queue. RAM events after failed persistence
can be lost on crash: a failed write or ack is not a durable-delivery guarantee.

Ack is idempotent and succeeds only after durable progress is flushed; storage failures return
503. Delivery is at-least-once, not exactly-once Telegram: a send followed by an unconfirmed ack
can be delivered again after a bot crash. The live bot remembers receipt IDs to retry ack without
resending. Transport/no-path/429/5xx failures retain ordered messages for retry; permanent refusal
closes that delivery, and a blocked chat becomes inactive. See `telegram-bot.md` for the receiver.

Subscription health publishes transitions using the same endpoint folding as status. Unknown
or paused monitoring is not evidence that a subscription has no live servers. State and events
are saved together; persisted all-dead state does not publish the same event on every restart.
Queue health is independent of watch state: `active` with `storage_error` is a degraded queue,
not evidence that failover is stopped.

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
26.3.27 on x86_64: 31 MB with one outbound, 34 MB with 600; router costs remain estimates.

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
handshake with gstatic is 6.8 KB of the watch's ~12 KB check. A failure is retried once after 2 s.
Reasons: `timeout`, `connection closed`, `HTTP <code>`.

## The schedule

Every endpoint has its own `NextAt`: alive `interval` later ±10 %; dead `2 × interval` later, the
pause doubling up to `dead_interval_max`; new at once; rejected never. `concurrency` workers take
due endpoints, urgent first (`Request`), then the longest due. `lag_seconds` reports how long the
next due check waits; over an interval it is logged once. The daemon rereads its settings, the
subscriptions and `xray.active_server` every minute; `Stamp` (name, inode, size, mtime of every
file) spares the rebuild when nothing was written.

Unchanged subscription files are reused by `vpnconfig.SubscriptionCache`, not parsed again.
`interval` is at least 10 s, `dead_interval_max` at least twice the interval, and `concurrency`
is 1 to 32; invalid values take their defaults with a WARN.

The WAN guard: at least 5 checks - or every checkable endpoint when there are fewer - completed
within 30 s, all failed, and no control answered within 30 s: the daemon dials
`endpoint.WANControls`. None accepting, the failures in the window are undone, the state is
`wan_down` and checks pause; controls are dialed every 15 s, and when one accepts every endpoint
is due.

States: `ok`, `stopped` (`/tmp/vpn-director/stopped`), `disabled` (`monitor.enabled`),
`no_xray`, `wan_down`, `prober_error`.
`stopped`, `disabled` and `no_xray` stop the prober; `ok` runs it. Statuses stay as they were
while checks are paused.

## The socket

`/tmp/vpn-director/watchd.sock`, `0600`, HTTP over unix IPC:

| Route | Contract |
|-------|----------|
| `GET /v1/monitor` | Independent monitor state and endpoints by key |
| `POST /v1/monitor/check` | `{"keys": [...]}` or `{}`; 202 with `queued`, 409 while stopped/disabled |
| `GET /v1/watch` | Watch state/action, `committed_failover`, `pending_restore`, notification pending/storage error |
| `POST /v1/notifications/recipients` | Replace authorized recipients with `chat_id`/`first_seen` |
| `GET /v1/notifications/pending?cursor=...` | Ordered per-chat events and `next_cursor` |
| `POST /v1/notifications/ack` | Idempotent durable progress for `chat_id`/`event_id` |

Each client request is bounded by 2 seconds. Notification POST bodies are capped at 1 MiB
(413 on oversize); responses are bounded by 16 MiB, pending pages by 100 messages, cursors by
256 characters. Invalid cursor/event ID is 400; storage failure is 503, not false success.
In-process fast failover uses `CheckEvidence`/`ValidateEvidence`, not a stale public snapshot.

Watch states: `starting`, `active`, `stopped`, `incompatible`, `error`, `not_running`. They do not
reuse monitor states. The Web UI exposes them through `GET /api/watch`, alongside
`GET /api/monitor`; bot `/status` prints separate Monitor/Watch, committed failover, pending
restore and queue-health fields. `/api/watch` is a read-only authenticated status route, not an
ack endpoint. An old daemon without watch IPC is `not_running` for automation even when its
monitor answers. Logs are `/tmp/vpn-director-watchd.log` (bot `/logs watchd`, Web UI log source
`watchd`); the init process check alone cannot tell whether automation is incompatible.

The state survives a restart: `/tmp/vpn-director/watchd-state.json`, saved at once when an
endpoint's status changes, other monitor state at most once a minute, and at shutdown; it is
read at startup, and the first refresh takes the entries of keys still in the set.

## Dev mode

`cd server && go run ./cmd/watchd --dev` uses `testdata/dev` paths (socket
`testdata/dev/watchd.sock`), `FakeLauncher` and a mock shell executor. The fake answer follows
from the first byte of each key: mostly alive, some dead, a few refused. Router bot attestation
is bypassed in dev; the watch and durable queue still run and write dev config/data. The Web UI
and bot in `--dev` read that socket. Subscriptions go in `testdata/dev/data/subscriptions/`;
`testdata/substore/` supplies synthetic examples.

Use synthetic provider endpoints/credentials and RFC1918 examples for LAN failover clients.
Documentation-range addresses are suitable remote examples, not eligible Tunnel Director LAN
clients. Dev mode is not an OS/network sandbox and fake health is not evidence of working Xray,
Telegram, firmware hooks, routing or router resources. Native tests/dev observations do not
replace real-router acceptance or grant permission to change a router.
