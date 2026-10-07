# VPN Director

Traffic routing system for home routers: Xray TPROXY, Tunnel Director, IPSet Builder.

Firmware-specific facts belong behind the platform contract in `lib/platform.sh`, implemented
per platform under `lib/platform/`: Asuswrt-Merlin (`merlin.sh`) and KeeneticOS (`keenetic.sh`).

## Commands

```bash
# Install
curl -fsSL https://raw.githubusercontent.com/zinin/vpn-director/master/install.sh | bash

# VPN Director CLI
/opt/vpn-director/vpn-director.sh status              # Show all status
/opt/vpn-director/vpn-director.sh apply               # Apply configuration
/opt/vpn-director/vpn-director.sh stop                # Stop Xray/routing; fence automation, leave daemons running
/opt/vpn-director/vpn-director.sh restart             # Restart Xray, rebuild all in place without a stop
/opt/vpn-director/vpn-director.sh update              # Update ipsets + reapply
/opt/vpn-director/vpn-director.sh platform            # Platform facts as JSON (tunnels, WAN, password file)
/opt/vpn-director/vpn-director.sh cron install        # Schedule the daily update (S99 does this)
/opt/vpn-director/vpn-director.sh --wait apply        # Queue for a running instance (120 s) instead of skipping
/opt/vpn-director/vpn-director.sh --unless-stopped apply  # Skip once "stop" has run (watchd's subscription watch)

# Component-specific commands
/opt/vpn-director/vpn-director.sh status tunnel       # Tunnel Director status only
/opt/vpn-director/vpn-director.sh restart xray        # Restart the Xray process, TPROXY applied again in place
/opt/vpn-director/vpn-director.sh restart xray-process  # Restart the Xray process, TPROXY rules kept

# Independent watchd lifecycle (does not stop main Xray or remove routes)
/opt/etc/init.d/S98vpn-director-watchd check          # Process check, not automation readiness
/opt/etc/init.d/S98vpn-director-watchd restart        # Restart monitoring and subscription automation
/opt/vpn-director/telegram-bot --watchd-capabilities   # Read-only ownership contract; no token/config needed

# Subscriptions: add, refresh, rename, delete (a menu)
/opt/vpn-director/import_server_list.sh
```

```bash
# Build (from the repository root)
make build-webui         # Vue SPA -> go:embed -> webui binary
make build-all           # all three daemons for arm64, arm and mipsle
make -C server test      # Go tests

# Web UI in development: plain HTTP, testdata/dev paths, mock shell, admin/admin
cd server && go run ./cmd/webui --dev

# Watchd in development: testdata/dev paths, fake prober and mock shell; not router acceptance
cd server && go run ./cmd/watchd --dev
```

## Architecture

| Path | Purpose |
|------|---------|
| `router/opt/vpn-director/vpn-director.sh` | Unified CLI entry point |
| `router/opt/vpn-director/lib/common.sh` | Core utilities: log, tmp_file, download_file, resolve_ip |
| `router/opt/vpn-director/lib/platform.sh` | Platform detection (`VPD_PLATFORM`) and loader of the platform contract |
| `router/opt/vpn-director/lib/platform/merlin.sh` | Asuswrt-Merlin implementation: nvram, rt_tables, cru |
| `router/opt/vpn-director/lib/platform/keenetic.sh` | KeeneticOS implementation: RCI, ip-full, insmod, cron.d |
| `router/opt/etc/ndm/*/50-vpn-director.sh` | KeeneticOS hooks: firewall rebuilds, WAN, tunnel interfaces |
| `server/internal/platform/` | Platform name and password file for the daemons |
| `router/files.manifest` | Shipped files with platform tags; read by `install.sh` and the updater |
| `router/opt/vpn-director/lib/firewall.sh` | Firewall helpers: chain, rule, block/allow host |
| `router/opt/vpn-director/lib/config.sh` | JSON config loader (vpn-director.json → shell vars) |
| `router/opt/vpn-director/lib/ipset.sh` | IPSet module: ensure, update, status |
| `router/opt/vpn-director/lib/tunnel.sh` | Tunnel Director module: apply, stop, status |
| `router/opt/vpn-director/lib/tproxy.sh` | Xray TPROXY module: apply, stop, status |
| `router/opt/vpn-director/lib/xrayconf.sh` | Xray config.json from a server's stored outbound (legacy VLESS records built), `xray run -test` before it replaces the live one |
| `router/opt/vpn-director/lib/subscription.sh` | Subscription decoder: share links (vless, vmess, trojan, ss, hysteria2), base64 or plain, and Xray JSON, into servers with a ready outbound |
| `testdata/subscription/` | Cases both subscription decoders (shell and Go) must decode alike |
| `router/opt/vpn-director/lib/substore.sh` | Subscription files (`<data_dir>/subscriptions/<id>.json`): order, ids, names, the write under the config lock; the twin of `vpnconfig/substore.go` |
| `testdata/substore/` | Synthetic subscription files both stores (shell and Go) must list alike |
| `router/opt/etc/init.d/S99vpn-director` | Entware init.d script for startup |
| `router/jffs/scripts/firewall-start` | Asuswrt-Merlin hook for firewall reload |
| `router/jffs/scripts/wan-event` | Asuswrt-Merlin hook for WAN events |
| `router/test/` | Bats tests (unit/, integration/) |
| `router/opt/vpn-director/vpn-director.json.template` | Unified config template |
| `router/opt/etc/xray/config.json.template` | Xray server config template |
| `install.sh` | Interactive installer |
| `server/cmd/bot/main.go` | Telegram management and watchd notification delivery; no subscription watcher |
| `server/cmd/webui/main.go` | Web UI daemon: HTTPS server, DI, dev mode |
| `server/cmd/watchd/` | `vpn-director-watchd`: monitoring, subscription automation and queue, one socket-owned runtime |
| `server/internal/monitor/` | Endpoint checks, `vpn-director-probe`, schedule, WAN guard and fresh failover evidence |
| `server/internal/subwatch/` | Watchd's SOCKS probe, subscription walk, failover, preferred return and pending restore |
| `server/internal/watchcompat/` | Read-only capability gate for installed and running bot executables |
| `server/internal/notifications/` | Watchd's durable events, per-chat delivery progress and subscription-health transitions |
| `server/internal/watchdapi/` | Unix IPC: independent monitor/watch status, recipient sync, pending events and ack |
| `server/internal/endpoint/` | One address of one server as the walk dials it: `PerAddress`, `ServerForDial`, `DialKey`, the monitor's `Key` |
| `router/opt/etc/init.d/S98vpn-director-watchd` | Entware init.d script of the server monitor |
| `server/internal/webapi/` | HTTP API: router, JWT middleware, handlers, response deadlines |
| `server/internal/subscription/` | Go subscription decoder, the twin of `lib/subscription.sh`; resolution and import summaries |
| `server/internal/auth/` | Password check against the platform password file, JWT issue and validation |
| `web/` | Vue 3 SPA, embedded into the webui binary with `go:embed` |
| `router/opt/vpn-director/setup_telegram_bot.sh` | Bot configuration script |

## Key Concepts

**Automation ownership**: `vpn-director-watchd` owns monitoring and automatic failover;
Telegram is a management/delivery client, not a prerequisite. Monitor and watch states are
independent (`GET /api/monitor`, `GET /api/watch`, bot `/status`). Old installed or running bot
bytes keep automation `incompatible` without disabling monitoring. Disabling `monitor.enabled`
stops endpoint monitoring/prober, not watchd's legacy failover. See `watchd.md` for upgrade,
shutdown, durable notification and recovery contracts.

**Tunnel Director**: Routes LAN client traffic through VPN tunnels with exclusion-based logic.
```json
{
  "tunnel_director": {
    "tunnels": {
      "wgc1": { "clients": ["192.168.50.0/24"], "exclude": ["<country_code>"] },
      "OpenVPN0": { "clients": ["192.168.1.5"], "exclude": ["<country_code>"], "gateway": "10.73.149.1" }
    }
  }
}
```
- All traffic from `clients` goes through tunnel
- Traffic to destinations in `exclude` bypasses VPN (direct)
- A tunnel key is any id `platform_tunnels` lists (Merlin: `wgcN` from
  `/etc/iproute2/rt_tables`, and `ovpncN` while that OpenVPN client is in "VPN Director
  (policy rules)" mode, plus `main`; Keenetic: `OpenVPNN` and `WireguardN` from RCI, plus
  `main`); a key the platform does not list is skipped with a warning, which on Merlin
  names the OpenVPN client's mode
- Optional `gateway` is the OpenVPN next hop; ignored on WireGuard. On Merlin it
  is also what fills ovpncN when the server does not push redirect-gateway.

**IPSet Types**: Country sets — 2-letter ISO codes from multi-source download

**IPSet Sources** (priority order):
1. GeoLite2 via GitHub (firehol/blocklist-ipsets)
2. IPDeny via GitHub (firehol mirror)
3. IPDeny direct (ipdeny.com)
4. Manual fallback (interactive)

## Config Files (after install)

| Path | Purpose |
|------|---------|
| `/opt/vpn-director/vpn-director.json` | Unified config (Xray + Tunnel Director) |
| `/opt/etc/xray/config.json` | Xray server configuration |
| `/opt/vpn-director/telegram-bot.json` | Telegram bot config (token, allowed users) |
| `/opt/vpn-director/certs/server.{crt,key}` | Self-signed TLS certificate for the Web UI |

**Data storage**: `data_dir` in vpn-director.json (default: `/opt/vpn-director/data`) — `subscriptions/<id>.json`, ipset dumps and `watchd-notifications.json` (watchd is its sole writer; bot uses IPC).

**Web UI settings**: the `webui` section of `vpn-director.json` — `port` (8444), `cert_file`, `key_file`, `jwt_secret` (auto-generated when empty), `log_level` (`debug|info|warn|error`).

**Server monitor settings**: the `monitor` section of `vpn-director.json` — `enabled` (true), `interval` (`1m`, a live server's check), `dead_interval_max` (`30m`, the longest pause of a dead one), `concurrency` (8), `log_level`. The daemon rereads it every minute.

## Shell Conventions

- Shebang: sourced libraries keep `#!/usr/bin/env bash`; a script a router executes
  (`vpn-director.sh`, `configure.sh`, `import_server_list.sh`, `setup_telegram_bot.sh`,
  `lib/send-email.sh`, `install.sh`) starts `#!/bin/sh` and hands over to bash by absolute path — KeeneticOS
  has no `/usr/bin/env`. Both forms then `set -euo pipefail`
- Debug: `DEBUG=1 ./script.sh` enables tracing with informative PS4
- Conditionals: Use `[[ ]]` instead of `[ ]`
- Logging: `log -l ERROR|WARN|INFO|DEBUG|TRACE "message"`
- Platform facts (WAN, tunnels, cron, kernel modules) belong behind the `platform_*` functions;
  core modules never test the platform name. Tests set `VPD_PLATFORM=merlin` through
  `test_helper.bash`; Keenetic tests export `VPD_PLATFORM=keenetic`

## Modular Docs

See `.claude/rules/` for detailed docs:
- `packet-flow.md` — packet processing order, Xray vs TD priority, fwmark bit layout
- `keenetic.md` — KeeneticOS facts: NDM, RCI, hooks, marks, fast path, cron
- `tunnel-director.md` — rule format, chain architecture, fwmark layout
- `ipset-builder.md` — IPdeny sources, dump/restore, combo sets
- `xray-tproxy.md` — TPROXY chain, exclusions, fail-safe
- `shell-conventions.md` — utilities from lib/common.sh, lib/firewall.sh, **known pitfalls**
- `testing.md` — Bats framework, mocks, fixtures
- `telegram-bot.md` — management, notification receiver/ack, commands, wizard and self-update
- `webui.md` — HTTP API, separate monitoring/automation status, authentication, dev and update flow
- `watchd.md` — automation ownership, compatibility, upgrades, failover/restore, durable queue and monitor/prober
- `entware-init.md` — Entware init system (rc.unslung, rc.func, S* scripts)
