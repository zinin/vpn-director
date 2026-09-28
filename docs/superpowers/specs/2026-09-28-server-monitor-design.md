# Server monitor, stage 1: `vpn-director-watchd` checks every subscription server: design

Date: 2026-09-28
Branch: `feature/server-monitor`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Know, minute by minute, which Xray servers of every subscription carry traffic, so that a dead
active server can be replaced at once by one that worked a minute ago. Stage 1 builds the monitor
and shows its results. Stages 2 and 3 (section 9) move the subscription watch into the same daemon
and make its failover use them.

Scale: several subscriptions of 50–100 servers each. "Carries traffic" means that a request
through the server gets its answer. In the owner's experience servers fail completely rather than
slow down, so the monitor measures latency and leaves throughput alone.

## 2. Where it stands

The subscription watch (`internal/subwatch`, inside the bot daemon) probes only the active server:
every 30 s it fetches `https://www.gstatic.com/generate_204` through the live Xray's SOCKS port.
After 1–3 minutes of failures (`FastDeadAfter`, `DeadAfter`) it declares the server dead, moves the
Xray clients onto a Tunnel Director exit when there is one, downloads every subscription and walks
the servers one by one: a `config.json` per server, `restart xray-process`, 3 s to settle, a probe
of up to 10 s. The walk does not know which servers work. A blocked provider or protocol costs it
5–15 s per dead server, and a walk over a few hundred servers can take many minutes.

## 3. Decisions

| Question | Decision |
|---|---|
| What the results serve | Stage 1: the Web UI and the bot show them. Stage 3: fast failover, early switching, Telegram notifications. |
| What a check proves | A request through the server gets its answer. No throughput, UDP or per-site checks. |
| Where the monitor runs | A new daemon, `vpn-director-watchd`. Stage 2 moves the subscription watch into it, so failover stops depending on the Telegram bot. |
| Engine | A separate Xray process, the prober, holds every server; the daemon sends its own checks through it. The live Xray is never touched. |
| Rejected engines | Xray's `burstObservatory`: no check on demand, no failure reason, a probe other than the watch's. A balancer in the live Xray: it replaces the single-active-server model that failover, the returns, the Web UI and `/xray` rest on. xray-core linked into the daemon: 20–30 MB more per binary, and a second Xray beside the Entware one whose version the user controls. A ready-made checker such as xray-checker: its own core and subscription parser, no tie to our store or the walk. |
| Unit of checking | An endpoint: one address of one server, with the outbound as the walk dials it. Copies with one `dialKey` are one endpoint. |
| Frequency | Every live endpoint once per `interval` (1 min by default). Dead ones back off from `2 × interval` to `dead_interval_max` (30 min by default). |
| Probe | `http://www.gstatic.com/generate_204`: HTTP 204 within 10 s, one retry after 2 s. |
| Name | `vpn-director-watchd`; nothing in it is named after Xray (section 4.1). |
| Staging | Three stages, each with its own spec, plan and PR (section 9). |

## 4. Design

### 4.1 The daemon

`server/cmd/watchd/main.go` builds `/opt/vpn-director/vpn-director-watchd`, shipped as the release
assets `vpn-director-watchd-{arm64,arm,mipsle}` and started by the init script
`S98vpn-director-watchd`, modelled on `S98vpn-director-webui`. It logs to
`/tmp/vpn-director-watchd.log`, rotated at 200 KB like the other daemons' logs. It starts at boot,
moves to `/` at startup outside dev mode as the other daemons do, and keeps running while the
monitor is disabled or VPN Director is stopped: stage 2 puts the watch into it.

The name contains no "xray". The monit rule the README recommends,
`check process xray matching "xray"`, matches that substring anywhere in a command line: a daemon
named after Xray would pass for a live Xray forever, and `vpn-director.sh status`
(`pgrep -la xray`) would list it. The name also extends no existing binary path, because the
update script finds running daemons with `pgrep -f <binary path>`, which matches a prefix. The
kernel cuts `comm` to 15 characters (`vpn-director-wa`), but BusyBox `pidof` and `killall` also
compare argv[0] and `/proc/PID/exe`, so the init script finds the daemon (checked on BusyBox 1.37;
to be checked on the router's 1.25, section 7).

Cost: about 9 MB more per update download (the bot and Web UI binaries are 9–10 MB each),
10–15 MB of memory for the daemon, about 35 MB for the prober while it runs (section 4.3).
`monitor.enabled: false` turns the checks off on routers with little memory.

Code layout, names for the plan to settle:

- `internal/endpoint`: `perAddress`, `ServerForDial` and `dialKey`, moved out of `subwatch`, which
  calls them from there; plus `Key` (the hex SHA-256 of a `dialKey`) and `Keys(server)`. The
  monitor, the Web UI and the bot derive endpoints through this one package.
- `internal/monitor`: the endpoint set, the prober, the checks, the schedule, the state.
- `internal/watchdapi`: the socket protocol (server and client) and `Health`, which folds the
  states of a server's endpoints into the server's status.

### 4.2 Endpoints

On every refresh (section 4.5) the daemon reads every subscription with
`vpnconfig.LoadSubscriptions`, which skips a broken file with a warning as every reader does. It
lists each server once per address (`perAddress`), puts the address into the outbound
(`ServerForDial`) and deduplicates by `dialKey`. One provider puts 62 names on 9 endpoints; the
monitor checks 9.

A server that `Generate` would refuse gets the status `rejected` and stays out of the prober:
`serverOutbound` in `service/xray.go` refuses the xhttp outbounds that make Xray panic on their
first dial. The monitor calls the same code, so the two cannot drift apart.

An endpoint's key is the hex SHA-256 of its `dialKey`. Results carry keys only, because a
`dialKey` holds UUIDs and REALITY keys. A refresh that changes a server's outbound (a rotated UUID)
changes its key, so the server shows as not yet checked instead of inheriting the old outbound's
status.

The prober's connections leave the router the way the live Xray's do. Traffic the router itself
originates never enters `XRAY_TPROXY` or `TUN_DIR`, which hang off PREROUTING, so a check sees the
path the live Xray would take.

### 4.3 The prober

**Binary.** The prober runs as `/opt/vpn-director/vpn-director-probe`, a hard link to the xray that
`LookPath("xray")` finds, and never under the name `xray`. Entware's `S24xray` goes through
`rc.func`, whose `start` answers "already running" when `pidof xray` finds any process: a prober
named `xray` would keep a crashed live Xray from being started, and `S24xray stop` would kill the
prober as well. A symbolic link does not help, because BusyBox `pidof` also compares the resolved
`/proc/PID/exe`: on BusyBox 1.37 it found an xray started through a symlink and missed one started
through a hard link. The prober's command line
(`/opt/vpn-director/vpn-director-probe run -c /tmp/vpn-director/probe/config.json`) contains no
"xray", so the monit rule of section 4.1 ignores it too.

The daemon re-links when the link no longer points at the file `LookPath` finds (`os.SameFile`
fails after Entware upgraded xray), through a temporary name and a rename. Where a hard link
fails with `EXDEV` (another filesystem), it copies the binary instead, about 30 MB of disk. Where
the copy fails, the state is `prober_error`.

**Config.** `/tmp/vpn-director/probe/config.json`, mode 0600, rewritten at every start:

- The first outbound is a `blackhole`. Xray sends whatever matches no rule to its first outbound,
  so nothing leaves the router directly.
- One outbound per endpoint follows, tagged `m<i>`, as `ServerForDial` built it.
- One SOCKS inbound listens on 127.0.0.1 at a port free at start. It requires a password and holds
  one account per endpoint, `e<i>`, with a random password per start, so no local process can use
  the prober as a proxy to every server.
- One routing rule per endpoint sends `user: ["e<i>"]` to `m<i>`.
- Xray logs at `warning` to stderr, which the daemon reads. The live Xray's `/tmp/xray-error.log`
  stays untouched.

Measured on Xray 26.3.27 (x86_64, 2026-09-28): one SOCKS inbound routed by user reaches the right
outbound, and the process holds 31 MB with one outbound, 33 MB with 300 and 34 MB with 600. The
number of servers barely matters. If the router's Xray does not route SOCKS users, the fallback is
one inbound per endpoint: 35 MB and 305 descriptors at 300 endpoints, measured the same day.

**Lifecycle.** The prober runs while the monitor is active and restarts when the endpoint set
changes. A check cut short by a restart counts as neither success nor failure. The daemon starts
the prober with `Pdeathsig: SIGKILL` from an OS thread locked for the prober's lifetime, since the
signal follows the thread that forked, so the prober dies with the daemon. At startup the daemon
also kills any process whose `/proc/PID/exe` is the probe binary. While the monitor is stopped or
disabled the prober does not run, which frees its memory.

**A config Xray refuses.** Xray stops at the first outbound it cannot build and names only its tag:
`failed to build outbound config with tag m37`. The daemon marks that endpoint `rejected`, drops it
and starts the prober again, at most 20 times in a row. When the output names no tag, it bisects
the set with `xray run -test`. The reason it keeps is Xray's error chain without the last segment,
because that segment quotes the offending value, which can be a key
(`invalid "password": not-a-key`). The last segment is neither stored nor logged.

**A crash.** When the prober exits during checks, the endpoints under check at that moment become
suspects. The daemon checks each alone in a one-endpoint prober, one after another. A suspect that
crashes its own prober becomes `rejected` ("crashes Xray") until its outbound changes. The main
prober then starts again without the culprits. Other endpoints wait during the isolation, which
takes at most `concurrency` single checks.

### 4.4 The check

The daemon fetches `http://www.gstatic.com/generate_204` through the prober's SOCKS port with the
endpoint's account; HTTP 204 within 10 s is a success. The hostname travels to the proxy as a SOCKS5
domain address, so the router resolves nothing. Latency is the time to the response headers: the
full cost of a new connection, the server's handshake and the request both. A failed attempt is
retried once after 2 s, and only a second failure fails the check, so one lost packet does not
flip a status.

The probe uses plain HTTP, where the watch's probe uses HTTPS. The TLS handshake with
www.gstatic.com costs 6.8 KB (5.2 KB read, 1.6 KB written, measured) and the 204 exchange about
0.2 KB; the server's own handshake adds an estimated 3–5 KB. A check therefore costs about 5 KB
instead of about 12 KB. At one check a minute that saving outweighs what separates the two probes:
they disagree only on a server that blocks outbound port 80, and in stage 3 the watch's own probe
still confirms every switch.

Stored failure reasons: `timeout`; `connection closed` (Xray drops the SOCKS connection when its
outbound fails); `HTTP <code>`; and the `rejected` reasons of section 4.3. Nothing of a response
body is kept.

### 4.5 The schedule

Every endpoint has its own next check:

- `alive`: one `interval` after its last check, with up to 10 % jitter so that checks spread over
  the minute instead of arriving in waves;
- `dead`: after `2 × interval`, then after twice the previous pause, up to `dead_interval_max`; a
  success resets the pause;
- `unknown` (new): at once;
- `rejected`: never, since a changed outbound is a new key.

An endpoint becomes `dead` when a check fails after its retry. `since` records when the status
last changed, and `fails` counts failed checks in a row.

`concurrency` workers (8 by default) take the due endpoints, earliest first. At about a second per
check that makes some 480 checks a minute, enough for about 400 endpoints at a 1-minute interval.
When due checks wait longer than one interval, the daemon logs a WARN once and reports the delay
as `lag_seconds`, and the Web UI says that the checks are falling behind.

The engine's `Check(ctx, keys)` makes the named endpoints (or all) due ahead of the rest and waits
for their results. The socket's `check` (section 4.6) calls it without waiting; stage 3 calls it
in-process and waits.

Every minute the daemon refreshes the endpoint set and rereads its config; a subscription file
that has not changed is not parsed again. At a rebuild the endpoints of the active server
(`xray.active_server`) come first.

**The WAN guard.** When at least five checks, or every endpoint when there are fewer, completed in
the last 30 s and all of them failed, the daemon dials the watch's control addresses
(`reachControls`: 1.1.1.1:443 and 8.8.8.8:443, TCP, 3 s). If none accepts, the WAN is down: the
state becomes `wan_down`, statuses freeze, failures are not counted, dead pauses do not grow,
endpoint checks pause and the controls are dialed every 15 s. When one accepts, every endpoint
becomes due. Without the guard an outage would mark every server dead.

### 4.6 State and the API

Per endpoint the daemon keeps `status` (`alive`, `dead`, `unknown`, `rejected`), `latency_ms` of
the last success, `checked_at`, `next_at`, `since`, `fails` and `error`. For itself it keeps
`state`, `message`, `interval_seconds`, `lag_seconds` and `updated_at`. The states:

- `ok`;
- `stopped`: `/tmp/vpn-director/stopped` exists;
- `disabled`: `monitor.enabled` is false;
- `no_xray`: no xray on PATH, looked up again every minute;
- `wan_down`: section 4.5;
- `prober_error`: the prober cannot start, with the reason in `message`; the daemon tries again
  after 1, 2, then 5 minutes.

The API is HTTP over the unix socket `/tmp/vpn-director/watchd.sock`, mode 0600; the daemon opens
no TCP port:

- `GET /v1/monitor` returns the whole state, endpoints keyed by their key.
- `POST /v1/monitor/check` takes `{"keys": [...]}`, or `{}` for every endpoint, and answers 202
  with the number queued. It ignores unknown keys and answers 409 with the state while the monitor
  is stopped or disabled.

The bot and the Web UI are clients of this API; no file is a contract between the daemons. The
client in `watchdapi` gives each request 2 s, so a hung daemon cannot stall a page or a command.

`Health` folds a server's endpoints into one status: `alive` when any address is alive, with the
best latency; otherwise `unknown` when any address is not yet checked; otherwise `dead` when any
is dead; otherwise `rejected`.

The state survives a restart of the daemon. The daemon saves it to
`/tmp/vpn-director/watchd-state.json` every minute when it changed, and at shutdown, and reads it at
startup; keys no longer in the set leave at the first refresh. The file lives on tmpfs, so after a
reboot the monitor starts from nothing.

### 4.7 Web UI

Two new routes; `/api/servers` and the Select flow stay as they are.

- `GET /api/monitor` returns `state`, `message`, `interval_seconds`, `lag_seconds` and, per
  subscription, `id`, `alive`, `total` and its servers in list order, each with `index`,
  `fingerprint`, `status`, `latency_ms`, `checked_at`, `since`, `next_at` and `error`. The handler
  reads the subscriptions, derives the keys through `internal/endpoint`, asks the daemon and folds
  with `Health`. The fingerprint is the bot's: the first 8 hex digits of the SHA-256 of
  `subscription|name|address|port`, so the page never pins a status on the wrong server when the
  list changed between its two requests. A daemon that does not answer gives
  `state: "not_running"` with 200.
- `POST /api/monitor/check` takes `{subscription, index, fingerprint}` for one server, or `{}` for
  all. It answers 409 when the server at that index has another fingerprint, as Select does, and
  503 when the daemon does not answer.

The Servers card:

- a Health column: `● 142 ms` in green, `● down` in red, `● —` in grey for a server not yet
  checked, `rejected` with its reason. The tooltip tells when the server was checked, since when
  it holds the status, the error and when the next check comes;
- a `↻` button per server that checks it now; a dead server's next check can be 30 minutes away;
- `45/62 alive` in each subscription's header;
- a line above the table: `Monitoring every 1 min · [Check all now]`, or `stopped`, `WAN down`,
  `checks are falling behind`, `not running`;
- while the tab is open, the page polls `/api/monitor` every 15 s.

The UI stays in English, as it is now.

### 4.8 Bot

- `/servers`: each line starts with 🟢, 🔴, ⚪ (not yet checked) or ⛔ (rejected), and a live
  server shows its latency. Each subscription header gets `45/62 живы`.
- `/xray`: a subscription button reads `Alpha (45/62)`, a server button `🟢 3. Oslo · 142 ms` or
  `🔴 4. Riga`.
- When the daemon does not answer, the marks are left out and one line says «мониторинг не
  запущен».

The bot gets no check button: with checks every minute it is seldom needed, and the Web UI has
one.

### 4.9 Configuration

A `monitor` section joins `vpn-director.json`. It goes into `vpn-director.json.template`, so that
`configure.sh`, which merges the template under the existing config, brings the defaults with an
update. It also goes into `VPNDirectorConfig`, because a daemon's write drops every key that struct
does not know.

```json
"monitor": {
  "enabled": true,
  "interval": "1m",
  "dead_interval_max": "30m",
  "concurrency": 8,
  "log_level": "info"
}
```

A missing key takes its default. `interval` is at least 10 s, `dead_interval_max` at least
`2 × interval`, `concurrency` between 1 and 32; a value out of bounds takes its default with a
WARN. The daemon applies a change at its next minute refresh, without a restart.

Traffic, for the documentation: about 5 KB per check, so 100 live endpoints checked every minute
come to about 0.7 GB a day, 21 GB a month. Each subscription carries its endpoints' share.

### 4.10 Installation and update

- `updater.Daemons` gains
  `{vpn-director-watchd, /opt/vpn-director/vpn-director-watchd, S98vpn-director-watchd}`. Step 2
  of a self-update then downloads the new asset like any other daemon's; the contract in
  `selfupdate.go` allows adding an asset. The download grows by about 9 MB within the 15-minute
  budget of step 2.
- The update script restarts only the daemons that ran before the update, so the first update
  that brings the new daemon would leave it stopped. Before it copies anything, the script records
  which daemon binaries do not exist yet; after a successful copy it starts those as well, with the
  other daemons and before the bot. A failed update does not start them.
- `install.sh` downloads and starts the daemon as an optional component, as it does the Web UI: a
  failed download does not stop the install.
- `files.manifest` gains the init script. The release workflow and both Makefiles build
  `vpn-director-watchd` for arm64, arm and mipsle.
- The README's monit section gains a rule for the new daemon.

### 4.11 Dev mode

`go run ./cmd/watchd --dev` works in `testdata/dev` (the socket `testdata/dev/watchd.sock`, the
state and the log beside it) with a fake prober whose results follow from each key: mostly alive
with varied latency, some dead, a few rejected. The Web UI and the bot in `--dev` talk to that
socket, so the pages can be built without xray.

## 5. Tests

- `internal/endpoint`: the keys match the walk's `dialKey` on the `testdata/substore` fixtures,
  and the `subwatch` tests pass unchanged after the move.
- `internal/monitor`, with a fake clock and a fake prober:
  - the prober config: golden JSON (the `blackhole` first, one SOCKS inbound, a rule per account),
    mode 0600;
  - the schedule: live endpoints every `interval` within the jitter bound, the dead pause doubling
    to its cap and resetting on success, new endpoints at once, rejected ones never, `Check` ahead
    of the queue, `lag_seconds`;
  - the retry: a failure followed by a success is alive, two failures are dead;
  - the WAN guard: with every check failing and no control accepting, statuses freeze; with a
    control accepting, they turn dead;
  - a refused config: the named tag rejected and dropped, an error without a tag bisected with a
    fake `xray run -test`, the bound of 20;
  - a crash: the culprit isolated and rejected, the other suspects checked again;
  - the state saved and restored, keys that left the set dropped;
  - error text: the last segment of Xray's chain never stored.
- An integration test with a real xray, skipped when none is on PATH: accounts routed to `freedom`
  and `blackhole`, and a local HTTP server that answers 204.
- `internal/watchdapi`: the socket handlers, the client's 2 s bound, `Health`.
- Web UI: `/api/monitor` mapped by fingerprint, `not_running`, and the 409 and 503 of
  `POST /api/monitor/check`.
- Bot: golden `/servers` and `/xray` output with statuses and with the daemon silent.
- Updater: the rendered script starts a daemon whose binary was absent before the update, and only
  after a successful copy.

## 6. Documentation

- `CLAUDE.md`: the daemon and its packages in the architecture table, the `monitor` section.
- `.claude/rules/watchd.md` (new): the daemon, the prober and why nothing is named after Xray, the
  schedule, the API. `webui.md` and `telegram-bot.md` point to it from the new routes and marks.
- README, English and Russian: what the monitor does and what it costs in traffic, the `monitor`
  settings, the monit rule for the daemon.

## 7. Verification on routers

On the RT-AX86U (Merlin, BusyBox 1.25) and a Keenetic:

- memory and CPU of the daemon and the prober with the real subscriptions;
- SOCKS user routing on the router's xray version;
- `pidof`, `killall` and the init script with the 19-character name;
- `S24xray start`, `stop` and `restart` with the prober running: the prober survives, and the live
  Xray starts and stops as before;
- the monit rule for xray ignores the daemon and the prober;
- the bytes of one check against a real server (the server's handshake is an estimate here);
- a first update from the previous release starts the new daemon.

## 8. Out of scope

- Failover and the walk (stage 3); moving the watch (stage 2).
- Notifications (stage 3).
- Throughput, UDP and per-site checks.
- Sorting servers by latency, history and uptime graphs, a check button in the bot.

## 9. Roadmap

Each stage gets its own spec, plan and PR.

- **Stage 2: the watch moves in.** `subwatch` moves into `vpn-director-watchd` together with what
  it takes from the bot: subscription downloads over the tunnels (`fetchSub`, `DialPath`) and the
  reach and readiness checks. The bot keeps Telegram. The watch's notifications cross to the bot
  through a queue that keeps the outbox's rules: per chat, in order, retried until a path to
  Telegram exists. The first update to that release stops the watch in the bot and starts it in
  the daemon. Failover then works without the bot; its behavior does not change.
- **Stage 3: failover on the monitor's data.** The walk tries verified servers first. When the
  live probe fails, the watch calls `Check` for the active server and the best candidates: the
  active server dead while a control answers confirms the death without waiting out the three
  minutes. The watch switches straight to a verified server, with no detour through the fallback
  tunnel, and takes today's path when no candidate works. Telegram hears when a subscription loses
  its last live server and when one comes back.
