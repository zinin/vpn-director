---
paths: "server/**/*"
---

# Telegram Bot

Go-based Telegram bot for remote VPN Director management.

## Architecture

```
server/
├── cmd/bot/main.go           # Bot entry point, signal handling, DI setup
├── cmd/webui/main.go         # Web UI entry point — see webui.md
├── internal/
│   ├── auth/                 # /etc/shadow verification and JWT — see webui.md
│   ├── bot/                  # Core bot orchestration
│   │   ├── bot.go            # Bot struct, Run(), message dispatch
│   │   ├── router.go         # Command and callback routing
│   │   ├── auth.go           # Username-based authorization
│   │   ├── path.go           # Telegram API path identity and candidates
│   │   ├── pathmanager.go    # Probe cycle and current-path selection
│   │   ├── probe.go          # One getMe over a given path; any HTTP status counts as live
│   │   ├── transport.go      # DialPath, NewPathClient, SO_BINDTODEVICE
│   │   ├── transport_linux.go # SO_BINDTODEVICE + SO_MARK socket control
│   │   ├── transport_other.go # Non-Linux stub that fails the dial
│   │   ├── outbox.go         # The watch's notifications, held until a path to Telegram carries them
│   │   ├── reach.go          # tcp4 reachability check for the subscription watch
│   │   └── subfetch.go       # Subscription fetch: WAN then DialPath tunnel
│   ├── chatstore/            # Chat ID persistence
│   │   └── store.go          # Thread-safe chat storage for notifications
│   ├── config/               # Configuration
│   │   └── config.go         # Bot config (token, users, log_level, update_check_interval)
│   ├── devmode/              # Development mode
│   │   └── executor.go       # Mock executor for safe dev testing
│   ├── handler/              # Command handlers
│   │   ├── handler.go        # Deps struct, handler registration
│   │   ├── misc.go           # /start, /version, /ip, /logs
│   │   ├── status.go         # /status, /restart, /stop
│   │   ├── servers.go        # /servers
│   │   ├── import.go         # /import
│   │   ├── subs.go           # /subs: refresh, rename, delete a subscription
│   │   ├── update.go         # /update (self-update from GitHub)
│   │   ├── xray.go           # /xray (quick server switch)
│   │   └── wizard_*.go       # /configure wizard handlers
│   ├── logging/              # Logging
│   │   ├── logger.go         # slog setup (stdout + file)
│   │   └── rotation.go       # Log file rotation (200KB)
│   ├── paths/                # Path constants
│   │   └── paths.go          # BotLogPath, VPNLogPath, etc.
│   ├── service/              # Business logic interfaces
│   │   ├── interfaces.go     # ShellExecutor, Network, etc.
│   │   └── subscriptions.go  # One download path; add, refresh, rename, delete for both daemons
│   ├── shell/                # Shell command execution
│   │   └── shell.go          # Real command executor
│   ├── ssrf/                 # Dial guard: refuse private and reserved addresses
│   ├── startup/              # Startup notifications
│   │   └── notify.go         # Post-update notification
│   ├── subwatch/             # Xray outbound watch (SOCKS probe, failover, restore)
│   │   ├── order.go          # The hybrid walk order and the dedupe key
│   │   ├── probe.go          # HTTPS 204 through Xray SOCKS
│   │   ├── reach.go          # TCP look at a server's addresses; the unreachable streak
│   │   ├── return.go         # Return to the preferred server
│   │   └── watch.go          # Tick: arming, failover, import, restore
│   ├── telegram/             # Telegram API helpers
│   │   └── sender.go         # Message sending, escaping
│   ├── updatechecker/        # Automatic update notifications
│   │   └── checker.go        # Background goroutine, per-user tracking
│   ├── updateflow/           # Update orchestration shared by the bot and the Web UI
│   │   ├── flow.go           # Check with a 30-minute cache, typed errors
│   │   └── start.go          # Pre-flight, lock, background handover
│   ├── updater/              # Self-update logic
│   │   ├── updater.go        # Daemon table (asset names, binaries, init scripts), GitHub API, lock file
│   │   ├── github.go         # GitHub release fetching
│   │   ├── handover.go       # Step 1: run the new release's binary as the installer
│   │   ├── selfupdate.go     # Step 2 and the contract between the steps
│   │   ├── downloader.go     # Asset downloading
│   │   ├── manifest.go       # router/files.manifest parsing, filtered by platform tag
│   │   ├── script.go         # Update script generation
│   │   ├── version.go        # Semantic version comparison and validation
│   │   └── update_script.sh.tmpl # The script rendered from the daemon table
│   ├── subscription/         # Subscription decoder (twin of lib/subscription.sh)
│   │   ├── subscription.go   # Package doc, Result, Skip, skip reasons
│   │   ├── decode.go         # Decode: format detection, dispatch
│   │   ├── text.go           # Base64/JSON helpers, the sanitizer
│   │   ├── links.go          # Share-link parsing
│   │   ├── vless.go          # VLESS converter
│   │   ├── vmess.go          # VMess converter
│   │   ├── trojan.go         # Trojan converter
│   │   ├── shadowsocks.go    # Shadowsocks converter
│   │   ├── hysteria2.go      # Hysteria2 converter
│   │   ├── xrayjson.go       # Xray JSON subscriptions
│   │   ├── names.go          # cleanName
│   │   ├── resolve.go        # Resolution
│   │   └── summary.go        # Import summaries and details
│   ├── vpnconfig/            # VPN Director config
│   │   ├── vpnconfig.go      # vpn-director.json, Server, the active and preferred server records
│   │   ├── substore.go       # Subscription files: order, ids, names (twin of lib/substore.sh)
│   │   └── subops.go         # Add, refresh, rename, delete a subscription inside a config-lock update
│   ├── webapi/               # Web UI HTTP API — see webui.md
│   └── wizard/               # Configuration wizard
│       ├── state.go          # Thread-safe state storage
│       └── wizard.go         # Wizard manager
└── Makefile                  # Build targets
```

## Commands

| Command | Handler | Description |
|---------|---------|-------------|
| `/start` | `MiscHandler.HandleStart` | Show help |
| `/status` | `StatusHandler.HandleStatus` | VPN Director status |
| `/xray` | `XrayHandler.HandleXray` | Quick server switch (see below) |
| `/servers` | `ServersHandler.HandleServers` | Server list (paginated) |
| `/import [url] [name]` | `ImportHandler.HandleImport` | Add a subscription, or refresh the one saved with that link; alone, refresh every subscription (a body over 1 MiB is refused); reports what it skipped and why |
| `/subs` | `SubsHandler.HandleSubs` | Subscriptions with refresh, rename and delete buttons; a rename takes the chat's next message, ahead of the wizard and the other prompts, and any command, any button outside `/subs` or 5 minutes end it |
| `/cancel` | `SubsHandler.HandleCancel` | Ends a rename that waits for its name |
| `/configure` | `WizardHandler.HandleConfigure` | Configuration wizard |
| `/restart` | `StatusHandler.HandleRestart` | Restart VPN Director |
| `/stop` | `StatusHandler.HandleStop` | Stop VPN Director |
| `/logs [bot\|vpn\|xray\|webui\|all] [N]` | `MiscHandler.HandleLogs` | Recent logs (default: all, 20 lines) |
| `/ip` | `MiscHandler.HandleIP` | External IP |
| `/update` | `UpdateHandler.HandleUpdate` | Self-update to latest GitHub release |
| `/version` | `MiscHandler.HandleVersion` | Bot version |

## Configuration Wizard

4-step inline keyboard wizard:

1. **Server Selection** — a subscription, then one of its servers (the first step is skipped with one subscription), 30 a page; a button whose subscription or server has gone, one from before subscriptions among them, starts the step again
2. **Exclusions** — select country sets to exclude (user-configurable)
3. **Clients** — add LAN clients with a route (xray or a tunnel the platform lists: `wgcN`/`ovpncN` on Merlin, `OpenVPNN`/`WireguardN` on Keenetic)
4. **Confirm** — review and apply

On apply:
- Updates vpn-director.json (clients, exclusions, rules)
- Generates /opt/etc/xray/config.json from template, for the server step 1
  picked: the state records its subscription, name, address and port, and
  step 4 and the apply look for it again by all four in the lists as they are
  then — a refresh during the wizard can move it. One a refresh dropped, or
  whose subscription was deleted, is named as gone in step 4, and the apply
  leaves the running server alone and says so
- Records the chosen server in `xray.active_server`, and only once the
  generation above succeeded — `/xray` does the same. See `webui.md`
- Runs `vpn-director.sh update`
- Restarts Xray

## Server switch (`/xray`)

Two steps. First the subscriptions, a button each (`xray:sub:<id>:0`) with its
server count and a ✓ on the running server's subscription; one without servers
is left out, and with one subscription the step is skipped. Then that
subscription's servers, 30 a page, with ◀ ▶ (`xray:sub:<id>:<page>`) and
« Back (`xray:subs`); a « Back that finds no server left clears the keyboard.
Each server button carries `xray:select:<id>:<index>:<fingerprint>`, the index
counted within the subscription and the fingerprint being the first 8 hex
digits of sha256 of `subscription|name|address|port`. The list can change
between `/xray` and the tap — the subscription watch rotates endpoints, a
refresh replaces a list, a subscription is deleted — and the index alone then
names another server: a tap whose subscription is gone, or whose server is no
longer at that index, replaces the keyboard with "The server list has changed
since these buttons were sent; run /xray again" instead of switching. So does
every button of a keyboard sent before subscriptions
(`xray:select:<index>[:<fingerprint>]`): it indexes a list that no longer
exists.

## Self-Update (`/update`)

Both daemons — `telegram-bot` and `webui` — are updated together, from the bot or from the Web UI. The orchestration lives in `internal/updateflow`; the bot command and the Web UI handlers are adapters over it.

1. `Flow.Check` asks the GitHub API for the latest release (result cached for 30 minutes; a forced check pierces the cache at most once a minute)
2. `Flow.Start` creates the lock file (`/tmp/vpn-director-update/lock`)
3. **Handover, step 1.** `Flow.Start`'s goroutine calls `updater.Handover` in the daemon the user pressed. It downloads that daemon's binary of the new release (`<daemon>-<arch>`, another daemon's when the release lacks it) to `/tmp/vpn-director-update/installer` and runs `installer self-update --from <current> --to <tag> --initiator <bot|webui> --chat-id <id>` from `/`. Every stdout line of the installer is a progress line: at most 10 reach the user, each cut to 300 characters. Exit 0 means the update script has started; otherwise the last stderr line is the reason. A step 2 that dies of a Go runtime failure is reported by its first `panic:` or `fatal error:` line instead: the last line of a crash is a stack frame. Step 1 waits at most 15 minutes, then asks step 2 to stop with `SIGTERM` and kills it 5 seconds later. It deletes `installer` whatever the outcome; after a failure it also removes `files/` and the lock, but only while the lock still names step 1's own process — a lock naming another process means the update script has taken over, and a lock that is gone is left alone the same way. The new release therefore installs itself: items 4 and 5 run in the new binary, with its code and its template, so a fix to the update procedure takes effect in the release that ships it.
4. **Step 2** (`updater.RunSelfUpdate`, which both `main` functions run first when their first argument is `self-update`) refuses unless `--to` is its own version and the lock names its parent process, fetches the release by tag and downloads it. Downloads go to `/tmp/vpn-director-update/files/`: first `router/files.manifest` of the release, then every file it tags `common` or with this platform's tag, in manifest order, taken from the repository at the release tag, plus one binary per daemon (`telegram-bot-<arch>`, `webui-<arch>`) from the release assets — except its own daemon's binary, which step 2 already is: it hard-links itself into `files/`, or copies itself when a link is not possible. The manifest read is the one of the release being installed, not the one this build shipped with, so a release may add or move files this binary knows nothing about; tags it does not know are skipped. A release with no `files.manifest`, a manifest that lists no file for this platform, or missing another daemon's binary is a download error. The lock check is repeated before every file step 2 writes, and once more before the script starts: the daemon running step 1 can die mid-download — out of memory on a 256 MB router is the plausible way — and a step 2 that kept downloading would truncate the files of the retry that took the directory over. "Before every file it writes" is literal: each download is staged beside its target and the claim is checked again before the rename that publishes it, because the whole HTTP round trip sits between the check at the top of the loop and the first byte written.
5. `update.sh` is generated from the daemon table and the payload's own `files/files.manifest` (a payload without one cannot be installed), and run detached, from `/` rather than from the update directory — the bot deletes that directory the moment it reports success, and a working directory that no longer exists is inherited by every daemon the script starts: monit then refuses to run at all, and every shell those daemons spawn prints `getcwd` errors into the Web UI's status output. For the same reason the script's `log` helper survives the loss of its own file, or `set -e` would end the script mid-way through its last steps. Both daemons also move to `/` themselves at startup, outside dev mode, resolving their path flags first — the directory they are started in is not theirs to hold, and no startup check can catch this deletion, which comes after they are up
6. The script remembers which daemons were running, stops them, copies everything, writes `notify.json` and starts back exactly those daemons
7. On failure an `EXIT` trap restarts the daemons that were running and writes `notify.json` with `"status": "failed"`

The contract between the two steps is the doc comment at the top of `internal/updater/selfupdate.go`: the asset names, the invocation, what stdout, stderr and the exit status mean, the update directory, the version check and the limits step 1 imposes. Later releases may add to it but never remove or rename anything in it, because a router cannot update the step 1 it runs. `internal/updater/testdata/selfupdate_argv.txt` keeps every invocation a released step 1 builds, and step 2 must parse them all. Step 2 never logs: its stderr carries the reason step 1 shows.

Both steps fetch release binaries — step 1 its installer, step 2 the daemons it does not carry — from each asset's own address on the API (`url` in the release document, `downloadAsset`), asking for `application/octet-stream`, which the API answers with a redirect to GitHub's CDN. Not from github.com's `browser_download_url`: on 2026-09-25 the RT-AX86U's provider dropped the addresses github.com resolved to while the API answered, and the bot learned of v0.17.1 and then failed to download it three times. An update needs the API anyway — the check and step 2's release document come from it — so this takes one host out of what an update depends on. The API answers any other request for that address with the asset's description in JSON, and an answer in JSON is refused rather than installed as a daemon that would never start. Each asset fetched costs a request of the API's allowance without a token (60 an hour per IP). A router on an older release still fetches its installer from github.com, since step 1 runs the old code; `install.sh` resolves the tag and fetches the binaries through github.com too (no API quota, no JSON parsing).

`notify.json`:

```json
{"chat_id": 0, "old_version": "v1.2.0", "new_version": "v1.3.0",
 "status": "ok", "initiator": "webui"}
```

`chat_id` 0 marks an update started from the Web UI: on its next start the bot notifies every active chat. A successful update clears `/tmp/vpn-director-update`; a failed one keeps `update.log`, because the message points at it, and drops the downloaded `files/` — 16 MB of tmpfs that a retry re-downloads anyway. Unless the directory is spoken for: the notifier claims it through `updater.CreateLockAt`, the same atomic claim `updateflow.Start` makes before it downloads a byte, and a refused claim can mean `files/` belongs to a live attempt. Claiming rather than checking is the whole of it — a check leaves the window between itself and the removal, and an attempt starting inside that window takes the lock and loses the payload it has just downloaded. The claim can equally be refused by the failed script's own lock — that script starts the daemons back up before dropping it — and skipping then costs only the reclaimed space, since the next attempt wipes `files/` before writing anyway. Wrong in the harmless direction either way. It is held for the removal and no longer: a claim left behind names a process that is alive, and every later update would be refused as one already in progress.

The bot only reads `notify.json` at startup, so a failure can wait there for a long time. A failure is treated as overtaken — logged, cleaned up, not announced — only when the running build is **strictly newer** than the `new_version` the attempt was installing: `install.sh` was re-run, or a later update landed, and the daemons it describes are gone. Not "neither end of the attempt": the two daemons need not share a version, because `install.sh` calls the Web UI optional and carries on when its download fails, and `old_version` is the version of whichever daemon *started* the update. A bot on v0.11.3 reading a failed v0.11.2 → v0.11.4 attempt of the Web UI's matches neither end while that failure is the freshest thing on the router — and the Web UI is down, so nothing else reports it. Running `new_version` is not overtaking either: the script's step 4 copies both binaries before anything is started, so a daemon that fails to start in the script's step 6 leaves the bot on the new build with the other daemon down — and that step rewrites `notify.json` before starting the bot precisely so that failure gets reported. A version that does not parse dates nothing and decides nothing: a tag this build cannot read, or a bot built outside a release, leaves the failure reported. A successful notification is always delivered, however late: it names what that update did.

**Dev mode**: `/update` is disabled with `--dev` and for a `dev` build.

## Config File

`/opt/vpn-director/telegram-bot.json`:
```json
{
  "bot_token": "123456:ABC...",
  "allowed_users": ["username1", "username2"],
  "log_level": "info",
  "update_check_interval": "1h"
}
```

**Fields:**
- `bot_token` — Telegram Bot API token (required)
- `allowed_users` — Array of Telegram usernames (required)
- `log_level` — `debug`, `info`, `warn`, `error` (default: `info`)
- `update_check_interval` — Go duration (`1h`, `30m`, `24h`). If omitted or `"0"`, automatic update checking is disabled

Setup: `./setup_telegram_bot.sh`

## Telegram API transport

The bot reaches `api.telegram.org` without a `proxy` setting. A selection cycle runs at startup, every 30s, and after a path failure. Every cycle probes **direct**, and probes the current path as well when that is not `direct`; the backups — Xray SOCKS on `advanced.xray.socks_port` (default 12346) when that port listens, then each `tunnel_director.tunnels` key except `main` that has clients and a connected platform iface, sorted by id — are probed only when a replacement is needed (direct dead and the current path dead or unset), stopping at the first live one. A tunnel socket gets `SO_BINDTODEVICE` **plus** the Tunnel Director `SO_MARK` derived from `/tmp/tunnel_director/tun_dir_tables`, so a tunnel path needs an applied Tunnel Director config; tunnel DNS queries `8.8.8.8` then `1.1.1.1` over the bound device instead of `resolv.conf`. When no path answers, a WARN lists every candidate with its reason. `--dev` is direct only. Leftover `proxy` / `proxy_fallback_direct` keys in old JSON are ignored.

## Subscription watch

Armed when at least one subscription exists — a static list included — and there are effective Xray clients (after subtracting `paused_clients`). Subscriptions that cannot be read count as one (`subscriptionCount`): a watch with Xray clients still fails them over when Xray dies, and the wave and the walk then find nothing to read. A `xray.failover` record arms it with or without a subscription, and so does a restore whose last apply has not succeeded: without a subscription the watch still restores the clients, follows the fallback tunnel and says so, but refreshes and walks nothing, and it starts no failover of its own. Nothing of the single subscription of earlier releases is read: until a subscription is added the watch stays unarmed, the first write of a subscription deletes `servers.json`, and the next config write of a daemon, like every add, refresh, rename and delete of the shell, drops `xray.subscription_url`. `--dev` does not start it. The watch starts with the bot process, before Telegram `getMe` succeeds: a down Xray outbound must not block recovery. There is still no watch without the bot daemon. Its notifications go through the outbox (`bot/outbox.go`): each active chat's messages wait there, in order, until Telegram is connected and the path manager has a path to it, and the bot tries them again every 10 seconds (`outboxRetryEvery`). Where the WAN does not reach Telegram, Xray's SOCKS port is often the only path, so the message that the outbound died is sent exactly when nothing can carry it; sent once, it used to be lost, and so was the one about the server the walk picked seconds later. A send that failed on the way — no path, a timeout, Telegram's 429 or 5xx — stops that chat's queue until the next try; any other answer from Telegram (the user blocked the bot, a bad request) drops the message, or it would hold the queue forever. A message delivered a minute or more late starts with when it happened — `(10:46, delayed) …`, with the day when that was another day. A chat keeps its latest 20 messages, none older than 12 hours; the outbox lives in memory, so a bot restart loses what waits in it. Failover stages TUN_DIR while the clients remain in `xray.clients`, then drops them from Xray only after the fallback tunnel is in `TUN_DIR_TABLES` and `failover_ready` (route, ip rule and PREROUTING jumps installed, every failover client's MARK rule in `TUN_DIR` — an apply that finds one gone rebuilds the chain first). `tunnel_apply` returns 0 with a WARN when that tunnel is not carrying traffic, so boot and hooks do not fail. An already committed failover still refreshes the subscriptions if that tunnel cannot be reapplied. `tunnel_apply` returns 1 when the failover tunnel's route or ip rule is not installed, so the watch does not drop Xray membership; it still records the config hash so the next apply does not `tunnel_stop` (which would take TUN_DIR down for every client). `tunnel_apply` does not fail the failover readiness check when the fallback tunnel has no effective clients (all paused or deleted). A restore whose fallback tunnel key is gone does not put those addresses back on Xray. Restore puts snapshot addresses back on Xray and removes from the tunnel only those that were appended at stage time (`failover.added`); addresses that were already on that tunnel stay there.

Every 30s the bot probes `https://www.gstatic.com/generate_204` through Xray SOCKS (`127.0.0.1:<socks_port>`, default 12346). Success is HTTP 204. A failed probe also dials the active server: every IPv4 address its subscription's entry lists (`chosenIndex`: its subscription, then its name, as the walk finds it), `tcp4`, 3 seconds, all at once. When none accepts, the look also dials two control addresses (`reachControls`: `1.1.1.1:443`, `8.8.8.8:443`), one of which a WAN that works reaches. When every such look since the first miss found none of the server's addresses accepting and a control accepting — two looks at least — the outbound is dead after 1 minute (`FastDeadAfter`) instead of 3, and the log says `reason=unreachable` rather than `reason=probe`. A look without an answer — no record, no entry, no IPv4 address, the subscriptions unreadable, an address the bot does not dial, a server no TCP dial can see, no control accepting either — keeps the 3 minutes for that run of misses; whatever starts the 3 minutes over starts the streak over too. A local failure (Xray restarting, a broken config) leaves the server accepting TCP and still waits the full time, and so does a WAN outage, which leaves no control accepting: a tunnel over the same WAN would carry nothing, and an outage shorter than 3 minutes passes without a failover. A server no TCP dial can see is never dialed at all (`tcpChecked`): Hysteria2, the Hysteria transport under any protocol, and xhttp over TLS whose `alpn` is exactly `h3`, speak QUIC, and mKCP runs over UDP, so its port accepts no TCP connection, and a port that does accept one — a masquerade site beside it — says nothing about the proxy behind it; every look at one is a look without an answer. Every other stored outbound is dialed, and so are a legacy record and an outbound that cannot be read. Once the outbound is declared dead it moves those clients onto the first Tunnel Director exit (same filter as PathManager: not `main`, has clients, platform lists it connected with an iface; no Telegram probe). `tunnel.sh` emits MARK rules for `xray.failover.clients` first so overlapping earlier rules (including `main`) do not send those snapshot addresses to WAN; other tunnels keep JSON order. It then runs a wave: every subscription with a link downloads at once, each within `FetchTimeout` (SSRF WAN first, then `DialPath` through the tunnel the clients are on — `xray.failover.tunnel` while it is still an exit, otherwise the first exit (`vpnconfig.FailoverTDExit`); server hostnames resolve over the same path that fetched the body, IPv4 only and bound to the tick's context (`subscription.LookupIPv4`, the default for `/import`, `/subs` and the Web UI's subscription routes too): an AF_UNSPEC lookup of every host in a subscription waits out the unanswered AAAA half, five seconds each, and a stop could not end it. A body that arrives over the WAN resolves each host with the WAN resolver and, for a host it does not answer, with the tunnel's, which asks 8.8.8.8 over the interface; one none of whose hosts answered on either falls through to the tunnel's own download; the watch walk dials those IPv4 addresses, every address of a server in turn before the next server (`perAddress`: a ban takes an address, not the name), while Web UI and `/xray` keep the hostname in vnext so CDN/DDNS still resolves). The walk tries the server the user chose first (`preferred_server`, else `active_server`) — its subscription's entry with its name, address and port, or else that subscription's first entry with its name, since a subscription that rotates endpoints gives a name a new address every day; the same name in another subscription is never it — then the next servers of its subscription until `OwnFirst` (3) are placed, then one server of each subscription in turn, starting after the chosen server's subscription (`walkOrder`): with A (32 servers) and B (62) and A5 chosen, the order starts A5, A1, A2, B1, A3, B2, A4, B3. Without that subscription — nothing chosen, a record from before subscriptions, its subscription deleted — the turns start at the first subscription, and with one subscription the order is the chosen server, then the rest of the list. A copy whose outbound, with its IPv4 in place, was already tried in the wave is skipped (`dialKey`: one provider puts 62 names on 9 endpoints); only a copy `Generate` generated counts as tried, so one the guard refused or that did not generate leaves its twins their turn. The walk's own records keep that choice in `xray.preferred_server` for as long as `active_server` names another server (`vpnconfig.RecordWalkedServer`, from the first record that leaves its subscription and name until one comes back to them), so a walk cut short by a bot restart or a stop starts the next wave from the user's server and returns to it; a selection in the Web UI, `/xray` or either wizard ends it. A live SOCKS probe restores the addresses that were taken off Xray, including those that already sat on the fallback tunnel — while failed over the health probe runs every tick, so a recovered outbound or a Web UI Select does not wait on the subscription hosts. Restore stages onto Xray only snapshot addresses still on the fallback tunnel, and neither stages nor drops tunnel membership until `tproxy_apply` has written `/tmp/xray_tproxy/ready` (SOCKS 204 does not prove LAN TPROXY; a soft-fail apply must not strip TUN_DIR). Waiting before the stage is the point: the PREROUTING jumps are installed even when the platform's own rules are not, and Xray wins over TUN_DIR, so a client put back into `xray.clients` too early is intercepted by a TPROXY that cannot carry it while the tunnel it is still on carries nothing. While the marker is missing the clients stay on the fallback tunnel and only the apply is retried, on the import cadence. The marker is checked once more after the apply that drops the tunnel membership, because that apply is the one that has to keep TPROXY up: if it soft-fails, the restored addresses go back onto the tunnel as a committed failover (`vpnconfig.ApplyFailoverSnapshot`, only what the restore moved) and the restore is retried once the marker returns, rather than leaving them with neither the proxy nor the tunnel and nothing to try again. The marker also needs the platform's own rules: on Keenetic a missing mangle INPUT accept drops proxied HTTPS in `_NDM_HTTP_INPUT_TLS_`, which the SOCKS probe cannot see. A full `vpn-director.sh stop` (bot `/stop`, `POST /api/stop`) writes `/tmp/vpn-director/stopped` before it tears anything down; a full `apply`, `restart` and `update` remove it — the firewall hooks and the daily update included, `apply --dry-run` not, and neither does an apply or restart of one component (`restart xray` after a Web UI server switch turns Xray back on, not the rest). The watch does nothing while that file exists. Its own applies and Xray restarts pass `--unless-stopped`, which the script checks right after taking the lock, so a stop that finishes mid-tick, or takes the lock ahead of a queued watch apply, stays in force; the watch also re-checks the marker after every wait, a restart the script skipped included, and ends the tick without writes or messages. The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between. While a tick waits it looks for the marker every second (`stopPoll`) and cancels what it is waiting on, so a stop does not sit out a download, the resolution behind it or a probe, and a wave a stop cut short leaves its window to the next. One download and the resolution of every host in it share a 3-minute deadline (`FetchTimeout`), and a fetch whose context ended part-way through the resolution returns an error rather than the servers resolved so far — the daemons' own "resolving the servers took longer than the deadline" when its deadline ended it: a list cut short is never published. The wait for the config lock is covered the same way for every config write of the watch: the guard the walk hands `Generate`, and `Watch.update` for the stage, commit, restore, retarget and publication writes, check the marker first, under that lock, so a stop that finishes while a write is queued refuses it instead of leaving it for the next manual apply. A deleted or wizard-moved address is not put back. If TPROXY cannot be installed, apply retries on the import cadence, not every 30s. Each list that arrives is published with `xray.servers` in one config-lock update, and only while its subscription still exists with the link downloaded (`vpnconfig.RefreshSubscription`, which the Web UI and the bot refresh through too): a subscription deleted while its download ran is not brought back. `import_server_list.sh` writes the files under the same lock, with the same guard on a refresh. A download that fails records why in the subscription's `error` (`vpnconfig.RecordSubscriptionError`) and keeps its list — unless the file's `refreshed` has moved since the wave read it: a Web UI or bot refresh that succeeded meanwhile is not marked failed by an older download. The walk runs unless some subscription failed and none was published — a download that failed and a publication that failed are both failures, and a list dropped because its subscription was deleted while it downloaded is neither — and a subscription whose download failed is walked from its last list: a provider's panel can be down while its servers work. So when every download failed there is no walk — a WAN outage fails them all, and a walk would cost an Xray restart per server for nothing — and Telegram gets "Subscription refresh failed: Alpha, Beta", as it does for every wave without a walk; a wave that published a list sends nothing: a download that failed goes to the log and stays in `error`, which the Web UI and `/subs` show. The walk's guard refuses a server whose subscription is gone or has another link than the one the walk read (`vpnconfig.ErrSubscriptionGone`): the walk skips the rest of that subscription and goes on with the others, and the return to the preferred server after an all-dead walk writes nothing back when the preferred server's subscription is gone. The look before a restore checks only for a newer selection (`walkOwnsNow`): the restore after a live probe does not look at the subscription — the server runs and answers, and the deletion of its subscription does not keep the clients off it. The restore's own writes — the stage and the drop of tunnel membership — carry the walk's guard, without the subscription check, under the config lock, so a Web UI or `/xray` selection that commits after that look, while a write waits for the lock or between the two, ends the walk there too: clients the stage handed back to Xray leave it again if they had left it before (`unstageRestore`), and the next tick probes the server that runs now. The tick's own restore carries a guard for the server its probe tested (`probedServer`) for the same reason. A walk that is still fetching or probing abandons if `xray.active_server` changes to a record it did not just write (Web UI / `/xray` Select). The record carries a write counter (`seq`) that every record moves on, so re-selecting the server already running counts too; `GenerateAndRecordWalkedServer` returns the counter it wrote inside the locked transaction, and the walk compares against that — reading it back after the lock was released would adopt a selection committed in between as the walk's own. Before each write that check runs as the guard of `GenerateAndRecordWalkedServer`, under the config lock the write takes, so a selection committed just before it is refused rather than overwritten; the return to the preferred server after an all-dead walk carries the same guard. The import walk still runs when SOCKS is down. If the fallback cannot be applied while the move is still staged, the watch keeps probing and refreshing the subscriptions, and a healthy probe rolls the stage back. Watch `Generate` writes config.json from a tunnel-resolved IPv4 (`ServerForDial`: the IPv4 goes into the stored outbound's own address slot, `vpnconfig.OutboundTarget`, and the hostname into an empty TLS server name or, for a stream without security, an empty ws/httpupgrade Host, the Host of the `xhttpSettings` or `splithttpSettings` the record has (Xray reads the former over the latter), or an empty gRPC `authority`; an xhttp extra's `downloadSettings.address` keeps its name — the record's IPs are the main address's, and Xray resolves the download host itself — so with the WAN resolver silent such a server reads as dead although its main address resolved; a REALITY entry without a server name keeps none and is refused, as the Web UI refuses it) and records the subscription hostname in `xray.active_server`, with the server's subscription, so the Web UI Active badge still matches the entry in its subscription's list. Web UI and `/xray` keep the hostname in vnext. A failed restore-Apply writes that snapshot back; it does not move unrelated Xray clients. Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored, back on the preferred server), and names a server `<subscription> / <server>`: "LAN clients back on Xray; server Beta / Germany-1". A walk that finds nothing live says "No live server in any subscription"; the variants that append "; still on tunnel:X" keep it. A wave that finds no live server backs the next one off to 10, 20, then 30 minutes after the walk ends; a wave in which some subscription failed and none was published retries every 5 minutes (and resets that backoff), and a restore resets the interval.

A failover moves only the Xray clients Tunnel Director can carry (`vpnconfig.TDCarries`: an IPv4 address or CIDR that iptables and ipset read as written — no leading zeros — inside RFC1918, the test `is_ipv4_net` and `is_lan_ip` make in the shell). The others stay on Xray, where a dead outbound takes them nowhere rather than out through the WAN; with none it can carry, the death is announced as one with no fallback. Xray clients added, re-added or resumed while a failover lasts join it the way the snapshot did (`ExtendXrayFailover`: onto the tunnel, then off Xray once TUN_DIR has them). The record carries `committed`, set by `CommitXrayFailover` and kept by the restore stage — a record from before the field counts as committed while its snapshot is off Xray. A restore says "back on Xray" only for a failover whose clients had left Xray, and one that does not hold goes back to the state it started from: committed, or a stage that never was. A restore stage whose apply loses the TPROXY marker takes the clients off Xray again, and the retry of a failed last restore apply checks the marker as the first try does. While Xray stays down, a committed failover asks the platform about its tunnel once a minute (`FallbackCheck`) and looks for `failover_ready`. A tunnel the platform still lists but Tunnel Director no longer sends the clients into — the last apply withheld the marker, and an apply of the watch's own did not bring it back — counts as gone, unless the failover has nobody left to carry (`vpnconfig.FailoverCarries`: every snapshot address paused or off the tunnel). Gone for a minute (`FallbackDownAfter`) — an empty tunnel list or a failed lookup is no answer — the clients move to the next exit, or with none left back to Xray, announced as a death with no fallback. An unready fallback is retried every 5 minutes and replaced by the next exit this round has not tried; after a round in which none became ready the next one waits 10, 20, then 30 minutes. What one episode learned about its fallbacks ends with it. The three minutes before a death count from the first miss of a working outbound: a pick without a fallback and a `/stop` start them again, and a failover keeps them counting. The Web UI and the bot's `/clients` take an address they add or delete out of the failover record, and the wizard keeps in it only what it puts on Xray, so an assignment a user makes during a failover is not undone by the restore.

While Xray works and a walk has left another server running (`xray.preferred_server` names the user's choice), the watch looks every 5 minutes (`ReturnCheck`) whether the preferred server's entry in its subscription accepts TCP; a completed restore holds the first look for 5 minutes, since that server has just failed. A preferred server no dial can see — Hysteria2, mKCP, xhttp over HTTP/3 (`tcpChecked`) — is switched to on that same schedule without a look: the attempt is then the only check there is. When an address accepts, or the schedule comes round for such a server, the watch switches Xray to it the way the walk does — `Generate` with the walk's guard, `restart xray-process`, 3 seconds, a SOCKS probe. A live probe ends it: `RecordWalkedServer` clears `preferred_server` and Telegram gets "Xray back on the preferred server <subscription> / <server>". A dead one switches back to the server that ran before — the address the walk picked first, when the process remembers it, even when a list refreshed since no longer has that address, then each other address of that entry in turn — says nothing, and waits 10, 20, then 30 minutes (`ReturnRetry`, `ReturnRetryMax`) before the next attempt. A death starts that interval over, unless it starts within 30 minutes (`ReturnHold`) of a return: the preferred server has failed again, the death counts as a failed return, and the restore after it does not shorten that wait. A return that has held for 30 minutes ends the backoff. After the fourth failed return in a row (`ReturnFailsMax`; a death within `ReturnHold` of a return counts) the watch stops trying: an endpoint that accepts TCP and keeps refusing the proxy would otherwise cost every Xray client ten to twenty dead seconds every 30 minutes. A new death, a selection or a bot restart starts the returns over. An attempt the watch could not undo is not made: while no subscription lists the server that runs and the process remembers no copy of it, each look is put off to the next. Every switch to the preferred server carries the walk's guard with the preferred server's subscription: a subscription deleted mid-attempt refuses the write, and the attempt tries no further address of that server, backs off as a failed one does and, when it has already left the server that ran, rolls back to it (`switcher.to` answers live, ended or gone) — the way back is not held to the subscription, since it is the server that ran. A `preferred_server` from before subscriptions has no `subscription`, matches no server, and is quietly nothing to return to. A Web UI, `/xray` or wizard selection clears `preferred_server` and so ends the returns, and so does a delete of the preferred server's subscription; one that commits during an attempt refuses the watch's next write and stands. A successful return costs one `restart xray-process`; a failed one costs one for every preferred address it tries and one for every address of the previous server it rolls back to — two when each has a single address.

## Automatic Update Notifications

When `update_check_interval` is set, the bot periodically checks GitHub for new releases and notifies users.

**How it works:**
1. Bot checks GitHub API at configured interval
2. If new version found, sends notification to all active users
3. Notification includes changelog and "🔄 Обновить" button
4. Each user is notified only once per version

**Data storage:**
- `/opt/vpn-director/data/chats.json` — stores chat IDs and notification history

**User tracking:**
- Chat ID recorded on first message from authorized user
- Users marked inactive if bot is blocked
- Reactivated automatically when user messages bot again

**Disabled in dev mode:** Update checker does not run when `--dev` flag is used or version is "dev".

## Build

```bash
cd server

# Native build
make build

# Cross-compile for router
make build-arm64   # ARM64 routers (AX86U, GT-AX6000, etc.)
make build-arm     # ARMv7 routers (older models)

# Tests
make test

# Run tests with coverage
go test ./... -cover
```

Binary: `bin/telegram-bot-{arch}`

```bash
# Web UI (from the repository root: needs the SPA embedded first)
make build-webui
make build-webui-arm64
make build-webui-arm
```

## Test Commands

```bash
# Run all Go tests
cd server && go test ./...

# Run with verbose output
go test -v ./internal/bot/...

# Run specific test
go test -run TestHandleStatus ./internal/bot/
```

## Deployment

1. Build for target architecture
2. Copy binary to `/opt/vpn-director/telegram-bot`
3. Run `./setup_telegram_bot.sh` to create config
4. Bot auto-starts if config exists and token is set

## Key Patterns

**Dependency Injection**: All handlers receive `*handler.Deps` struct with services

**Authorization**: Username whitelist in config, checked in `bot.isAuthorized()`

**Shell execution**: All router commands via `service.ShellExecutor` interface (real or dev mock)

**State management**: Thread-safe wizard state with mutex, per-chat storage

**Graceful shutdown**: Context cancellation on SIGINT/SIGTERM

**Dev mode**: `--dev` enables mock executor (safe commands only, blocks destructive ops)

## Dependencies

- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram API client
- `golang.org/x/net` — SOCKS5 proxy support

## Logging

Uses Go's `log/slog` package:

- Output: stdout + `/tmp/telegram-bot.log` (via `io.MultiWriter`)
- Format: `time=2026-01-30T15:04:05.000+03:00 level=INFO source=main.go:42 msg="Bot started"`
- Levels: `DEBUG`, `INFO`, `WARN`, `ERROR` (configurable via `log_level` in config)
- Rotation: Log file truncated at 200KB (checked every minute)
- Redaction: everything written passes `logging.redactingWriter`, which replaces the
  secret half of a bot token with `REDACTED` and keeps the public bot id. The token
  travels in the Telegram request path, and `net/http` puts the whole URL into every
  `*url.Error`, so a bot that cannot reach Telegram used to log its own credentials on
  each retry into a mode 0644 file the Web UI displays. The writer sits under both slog
  and the standard log package, so the Telegram client's own debug output is covered too

**Runtime level change**: Call `logger.SetLevel("debug")` to adjust without restart
