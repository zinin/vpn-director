---
paths: "server/cmd/watchd/**/*, server/internal/monitor/**/*, server/internal/watchdapi/**/*, server/internal/endpoint/**/*"
---

# Server monitor (vpn-director-watchd)

The daemon checks, minute by minute, whether every server of every subscription carries
traffic, and serves what it finds on a unix socket to the Web UI and the bot. Stage 1 of three:
stage 2 moves the subscription watch (`subwatch`) into this daemon, stage 3 makes its failover
use the monitor's data. Stage 1 changes no failover behavior: `subwatch` still probes, walks
and returns in the bot.

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
