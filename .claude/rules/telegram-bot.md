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
| `/xray` | `XrayHandler.HandleXray` | Quick server switch |
| `/servers` | `ServersHandler.HandleServers` | Server list (paginated) |
| `/import [url]` | `ImportHandler.HandleImport` | Import VLESS subscription (saved URL if omitted) |
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
- Generates /opt/etc/xray/config.json from template
- Records the chosen server in `xray.active_server`, and only once the
  generation above succeeded — `/xray` does the same. See `webui.md`
- Runs `vpn-director.sh update`
- Restarts Xray

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

Armed when `xray.subscription_url` is saved and there are effective Xray clients (after subtracting `paused_clients`) or a `xray.failover` record. `--dev` does not start it.

Every 30s the bot probes `https://www.gstatic.com/generate_204` through Xray SOCKS (`127.0.0.1:<socks_port>`, default 12346). Success is HTTP 204. After 3 minutes of consecutive failures it moves those clients onto the first Tunnel Director exit (same filter as PathManager: not `main`, has clients, platform lists it connected with an iface; no Telegram probe), refreshes the saved subscription (SSRF WAN first, then `DialPath` through that tunnel), and tries the same `active_server.name` then the rest. A live SOCKS probe restores only the addresses that were moved. Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored). A failed import keeps `servers.json`. A refresh that finds no live server backs the next one off to 10, 20, then 30 minutes after the walk ends; a failed download retries every 5 minutes, and a restore resets the interval.

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
