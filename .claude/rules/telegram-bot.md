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
│   │   ├── update.go         # /update (self-update from GitHub)
│   │   ├── xray.go           # /xray (quick server switch)
│   │   └── wizard_*.go       # /configure wizard handlers
│   ├── logging/              # Logging
│   │   ├── logger.go         # slog setup (stdout + file)
│   │   └── rotation.go       # Log file rotation (200KB)
│   ├── paths/                # Path constants
│   │   └── paths.go          # BotLogPath, VPNLogPath, etc.
│   ├── service/              # Business logic interfaces
│   │   └── interfaces.go     # ShellExecutor, Network, etc.
│   ├── shell/                # Shell command execution
│   │   └── shell.go          # Real command executor
│   ├── ssrf/                 # Dial guard: refuse private and reserved addresses
│   ├── startup/              # Startup notifications
│   │   └── notify.go         # Post-update notification
│   ├── subwatch/             # Xray outbound watch (SOCKS probe, failover, restore)
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
│   ├── vless/                # VLESS protocol
│   │   └── parser.go         # VLESS URL parser, subscription decoder
│   ├── vpnconfig/            # VPN Director config
│   │   └── vpnconfig.go      # vpn-director.json, servers.json
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
| `/import [url]` | `ImportHandler.HandleImport` | Import VLESS subscription (saved URL if omitted; a body over 1 MiB is refused) |
| `/configure` | `WizardHandler.HandleConfigure` | Configuration wizard |
| `/restart` | `StatusHandler.HandleRestart` | Restart VPN Director |
| `/stop` | `StatusHandler.HandleStop` | Stop VPN Director |
| `/logs [bot\|vpn\|xray\|webui\|all] [N]` | `MiscHandler.HandleLogs` | Recent logs (default: all, 20 lines) |
| `/ip` | `MiscHandler.HandleIP` | External IP |
| `/update` | `UpdateHandler.HandleUpdate` | Self-update to latest GitHub release |
| `/version` | `MiscHandler.HandleVersion` | Bot version |

## Configuration Wizard

4-step inline keyboard wizard:

1. **Server Selection** — choose Xray server from servers.json
2. **Exclusions** — select country sets to exclude (user-configurable)
3. **Clients** — add LAN clients with a route (xray or a tunnel the platform lists: `wgcN`/`ovpncN` on Merlin, `OpenVPNN`/`WireguardN` on Keenetic)
4. **Confirm** — review and apply

On apply:
- Updates vpn-director.json (clients, exclusions, rules)
- Generates /opt/etc/xray/config.json from template, for the server step 1
  picked: the state records its name, address and port, and step 4 and the
  apply look for it again in the list as it is then — a refresh during the
  wizard can move it. One a refresh dropped is named as gone in step 4, and the
  apply leaves the running server alone and says so
- Records the chosen server in `xray.active_server`, and only once the
  generation above succeeded — `/xray` does the same. See `webui.md`
- Runs `vpn-director.sh update`
- Restarts Xray

## Server switch (`/xray`)

Each button carries `xray:select:<index>:<fingerprint>`, the fingerprint being
the first 8 hex digits of sha256 of `name|address|port`. The list can change
between `/xray` and the tap — the subscription watch rotates endpoints, an import
replaces it — and the index alone then names another server: a tap whose server
is no longer at that index replaces the keyboard with "server list changed"
instead of switching. A keyboard sent before buttons carried the fingerprint
still works by index.

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

Armed when `xray.subscription_url` is saved and there are effective Xray clients (after subtracting `paused_clients`). A `xray.failover` record arms it with or without a saved link — `import_server_list.sh` clears the link for a list from a file or a plain-http link, typically while Xray is down — and so does a restore whose last apply has not succeeded: without a link the watch still restores the clients, follows the fallback tunnel and says so, but refreshes nothing, and it starts no failover of its own. `--dev` does not start it. The watch starts with the bot process, before Telegram `getMe` succeeds: a down Xray outbound must not block recovery. There is still no watch without the bot daemon. Notifications that fire before `getMe` are queued and flushed when the sender is published. Failover stages TUN_DIR while the clients remain in `xray.clients`, then drops them from Xray only after the fallback tunnel is in `TUN_DIR_TABLES` and `failover_ready` (route, ip rule and PREROUTING jumps installed, every failover client's MARK rule in `TUN_DIR` — an apply that finds one gone rebuilds the chain first). `tunnel_apply` returns 0 with a WARN when that tunnel is not carrying traffic, so boot and hooks do not fail. An already committed failover still refreshes the subscription if that tunnel cannot be reapplied. `tunnel_apply` returns 1 when the failover tunnel's route or ip rule is not installed, so the watch does not drop Xray membership; it still records the config hash so the next apply does not `tunnel_stop` (which would take TUN_DIR down for every client). `tunnel_apply` does not fail the failover readiness check when the fallback tunnel has no effective clients (all paused or deleted). A restore whose fallback tunnel key is gone does not put those addresses back on Xray. Restore puts snapshot addresses back on Xray and removes from the tunnel only those that were appended at stage time (`failover.added`); addresses that were already on that tunnel stay there.

Every 30s the bot probes `https://www.gstatic.com/generate_204` through Xray SOCKS (`127.0.0.1:<socks_port>`, default 12346). Success is HTTP 204. A failed probe also dials the active server: every IPv4 address its `servers.json` entry lists (`chosenIndex`, as the walk finds it), `tcp4`, 3 seconds, all at once. When every such look since the first miss found none accepting — two looks at least — the outbound is dead after 1 minute (`FastDeadAfter`) instead of 3, and the log says `reason=unreachable` rather than `reason=probe`. A look without an answer — no record, no entry, no IPv4 address, `servers.json` unreadable, an address the bot does not dial — keeps the 3 minutes for that run of misses; whatever starts the 3 minutes over starts the streak over too. A local failure (Xray restarting, a broken config) leaves the server accepting TCP and still waits the full time. A WAN outage fails every look too, so one that lasts longer than a minute now ends in a failover the 3 minutes used to ride out: the tunnel over the same WAN carries nothing meanwhile, and the restore follows once Xray answers again. Once the outbound is declared dead it moves those clients onto the first Tunnel Director exit (same filter as PathManager: not `main`, has clients, platform lists it connected with an iface; no Telegram probe). `tunnel.sh` emits MARK rules for `xray.failover.clients` first so overlapping earlier rules (including `main`) do not send those snapshot addresses to WAN; other tunnels keep JSON order. It then refreshes the saved subscription (SSRF WAN first, then `DialPath` through the tunnel the clients are on — `xray.failover.tunnel` while it is still an exit, otherwise the first exit (`vpnconfig.FailoverTDExit`); VLESS hostnames resolve over the same path that fetched the body, IPv4 only and bound to the tick's context (`vless.LookupIPv4`, the default for `/import` and the Web UI import too): an AF_UNSPEC lookup of every host in a subscription waits out the unanswered AAAA half, five seconds each, and a stop could not end it. A body that arrives over the WAN resolves each host with the WAN resolver and, for a host it does not answer, with the tunnel's, which asks 8.8.8.8 over the interface; one none of whose hosts answered on either falls through to the tunnel's own download; the watch walk dials those IPv4 addresses, every address of a server in turn before the next server (`perAddress`: a ban takes an address, not the name), while Web UI and `/xray` keep the hostname in vnext so CDN/DDNS still resolves), and tries the server the user chose first — the entry with its name, address and port, or else the first entry with its name, since a subscription that rotates endpoints gives a name a new address every day — then the rest in list order. The walk's own records keep that choice in `xray.preferred_server` for as long as `active_server` names another server (`vpnconfig.RecordWalkedServer`, from the first record that leaves its name until one comes back to it), so a walk cut short by a bot restart or a stop starts the next wave from the user's server and returns to it; a selection in the Web UI, `/xray` or either wizard ends it. A live SOCKS probe restores the addresses that were taken off Xray, including those that already sat on the fallback tunnel — while failed over the health probe runs every tick, so a recovered outbound or a Web UI Select does not wait on the subscription host. Restore stages onto Xray only snapshot addresses still on the fallback tunnel, and neither stages nor drops tunnel membership until `tproxy_apply` has written `/tmp/xray_tproxy/ready` (SOCKS 204 does not prove LAN TPROXY; a soft-fail apply must not strip TUN_DIR). Waiting before the stage is the point: the PREROUTING jumps are installed even when the platform's own rules are not, and Xray wins over TUN_DIR, so a client put back into `xray.clients` too early is intercepted by a TPROXY that cannot carry it while the tunnel it is still on carries nothing. While the marker is missing the clients stay on the fallback tunnel and only the apply is retried, on the import cadence. The marker is checked once more after the apply that drops the tunnel membership, because that apply is the one that has to keep TPROXY up: if it soft-fails, the restored addresses go back onto the tunnel as a committed failover (`vpnconfig.ApplyFailoverSnapshot`, only what the restore moved) and the restore is retried once the marker returns, rather than leaving them with neither the proxy nor the tunnel and nothing to try again. The marker also needs the platform's own rules: on Keenetic a missing mangle INPUT accept drops proxied HTTPS in `_NDM_HTTP_INPUT_TLS_`, which the SOCKS probe cannot see. A full `vpn-director.sh stop` (bot `/stop`, `POST /api/stop`) writes `/tmp/vpn-director/stopped` before it tears anything down; a full `apply`, `restart` and `update` remove it — the firewall hooks and the daily update included, `apply --dry-run` not, and neither does an apply or restart of one component (`restart xray` after a Web UI server switch turns Xray back on, not the rest). The watch does nothing while that file exists. Its own applies and Xray restarts pass `--unless-stopped`, which the script checks right after taking the lock, so a stop that finishes mid-tick, or takes the lock ahead of a queued watch apply, stays in force; the watch also re-checks the marker after every wait, a restart the script skipped included, and ends the tick without writes or messages. The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between. While a tick waits it looks for the marker every second (`stopPoll`) and cancels what it is waiting on, so a stop does not sit out a download, the resolution behind it or a probe. One download and the resolution of every host in it share a 3-minute deadline (`FetchTimeout`), and a fetch whose context ended part-way through the resolution returns the context's error rather than the servers resolved so far: a list cut short is never published. The wait for the config lock is covered the same way for every config write of the watch: the guard the walk hands `Generate`, and `Watch.update` for the stage, commit, restore, retarget and publication writes, check the marker first, under that lock, so a stop that finishes while a write is queued refuses it instead of leaving it for the next manual apply. A deleted or wizard-moved address is not put back. If TPROXY cannot be installed, apply retries on the import cadence, not every 30s. It publishes `servers.json` and `xray.servers` in one config-lock update (`vpnconfig.PublishServers`), as the Web UI and `/import` do, so two importers cannot leave one file from each. `import_server_list.sh` publishes under the same lock: it resolves the list into a temp file, writes nothing until it holds the lock, then replaces `servers.json`, `xray.servers` and the saved link together. An https link is saved, and a file or a plain-http link — neither of which the watch or the Web UI fetches — clears the saved one, so the next refresh does not bring the old link's list back. An import that cannot get the lock within 30 seconds, or whose list has no usable server, leaves the previous one whole. That update also checks, under the same lock, that `xray.subscription_url` is still the link this wave downloaded: a subscription saved while the download was in flight abandons the wave instead of publishing a list the saved link did not produce, and the next tick fetches the new one without waiting out the import window. The walk's guard makes the same check before every server it writes, the return to the preferred server included, and so does the look before a restore: a link saved while the walk runs ends it where it is, with no return and no message, and the new link gets the next tick. The restore's own writes — the stage and the drop of tunnel membership — carry the same guard under the config lock, so a Web UI or `/xray` selection that commits after that look, while a write waits for the lock or between the two, ends the walk there too: clients the stage handed back to Xray leave it again if they had left it before (`unstageRestore`), and the next tick probes the server that runs now. The tick's own restore carries a guard for the server its probe tested (`probedServer`) for the same reason. A walk that is still fetching or probing abandons if `xray.active_server` changes to a record it did not just write (Web UI / `/xray` Select). The record carries a write counter (`seq`) that every record moves on, so re-selecting the server already running counts too; `GenerateAndRecordWalkedServer` returns the counter it wrote inside the locked transaction, and the walk compares against that — reading it back after the lock was released would adopt a selection committed in between as the walk's own. Before each write that check runs as the guard of `GenerateAndRecordWalkedServer`, under the config lock the write takes, so a selection committed just before it is refused rather than overwritten; the return to the preferred server after an all-dead walk carries the same guard. The import walk still runs when SOCKS is down. If the fallback cannot be applied while the move is still staged, the watch keeps probing and refreshing the subscription, and a healthy probe rolls the stage back. Watch `Generate` writes config.json from a tunnel-resolved IPv4 (`ServerForDial`, which fills an empty TLS server name with the hostname; a REALITY entry without one keeps none and is refused, as the Web UI refuses it) and records the subscription hostname in `xray.active_server` so the Web UI Active badge still matches `servers.json`. Web UI and `/xray` keep the hostname in vnext. A failed restore-Apply writes that snapshot back; it does not move unrelated Xray clients. Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored, back on the preferred server). A failed import keeps `servers.json`. A refresh that finds no live server backs the next one off to 10, 20, then 30 minutes after the walk ends; a failed download retries every 5 minutes (and resets that backoff), and a restore resets the interval.

A failover moves only the Xray clients Tunnel Director can carry (`vpnconfig.TDCarries`: an IPv4 address or CIDR that iptables and ipset read as written — no leading zeros — inside RFC1918, the test `is_ipv4_net` and `is_lan_ip` make in the shell). The others stay on Xray, where a dead outbound takes them nowhere rather than out through the WAN; with none it can carry, the death is announced as one with no fallback. Xray clients added, re-added or resumed while a failover lasts join it the way the snapshot did (`ExtendXrayFailover`: onto the tunnel, then off Xray once TUN_DIR has them). The record carries `committed`, set by `CommitXrayFailover` and kept by the restore stage — a record from before the field counts as committed while its snapshot is off Xray. A restore says "back on Xray" only for a failover whose clients had left Xray, and one that does not hold goes back to the state it started from: committed, or a stage that never was. A restore stage whose apply loses the TPROXY marker takes the clients off Xray again, and the retry of a failed last restore apply checks the marker as the first try does. While Xray stays down, a committed failover asks the platform about its tunnel once a minute (`FallbackCheck`) and looks for `failover_ready`. A tunnel the platform still lists but Tunnel Director no longer sends the clients into — the last apply withheld the marker, and an apply of the watch's own did not bring it back — counts as gone, unless the failover has nobody left to carry (`vpnconfig.FailoverCarries`: every snapshot address paused or off the tunnel). Gone for a minute (`FallbackDownAfter`) — an empty tunnel list or a failed lookup is no answer — the clients move to the next exit, or with none left back to Xray, announced as a death with no fallback. An unready fallback is retried every 5 minutes and replaced by the next exit this round has not tried; after a round in which none became ready the next one waits 10, 20, then 30 minutes. What one episode learned about its fallbacks ends with it. The three minutes before a death count from the first miss of a working outbound: a pick without a fallback and a `/stop` start them again, and a failover keeps them counting. The Web UI and the bot's `/clients` take an address they add or delete out of the failover record, and the wizard keeps in it only what it puts on Xray, so an assignment a user makes during a failover is not undone by the restore.

While Xray works and a walk has left another server running (`xray.preferred_server` names the user's choice), the watch looks every 5 minutes (`ReturnCheck`) whether the preferred server's `servers.json` entry accepts TCP; a completed restore holds the first look for 5 minutes, since that server has just failed. When an address accepts, the watch switches Xray to it the way the walk does — `Generate` with the walk's guard, `restart xray-process`, 3 seconds, a SOCKS probe. A live probe ends it: `RecordWalkedServer` clears `preferred_server` and Telegram gets "Xray back on the preferred server <name>". A dead one switches back to the server that ran before — the address the walk picked first, when the process remembers it, even when a list imported since no longer has that address, then each other address of that entry in turn — says nothing, and waits 10, 20, then 30 minutes (`ReturnRetry`, `ReturnRetryMax`) before the next attempt. A death starts that interval over, unless it starts within 30 minutes (`ReturnHold`) of a return: the preferred server has failed again, the death counts as a failed return, and the restore after it does not shorten that wait. A return that has held for 30 minutes ends the backoff. An attempt the watch could not undo is not made: while servers.json no longer lists the server that runs and the process remembers no copy of it, each look is put off to the next. A Web UI, `/xray` or wizard selection clears `preferred_server` and so ends the returns; one that commits during an attempt refuses the watch's next write and stands. A successful return costs one `restart xray-process`; a failed one costs one for every preferred address it tries and one for every address of the previous server it rolls back to — two when each has a single address.

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
