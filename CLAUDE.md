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
/opt/vpn-director/vpn-director.sh stop                # Stop all components
/opt/vpn-director/vpn-director.sh restart             # Restart all
/opt/vpn-director/vpn-director.sh update              # Update ipsets + reapply
/opt/vpn-director/vpn-director.sh platform            # Platform facts as JSON (tunnels, WAN, password file)
/opt/vpn-director/vpn-director.sh cron install        # Schedule the daily update (S99 does this)
/opt/vpn-director/vpn-director.sh --wait apply        # Queue for a running instance (120 s) instead of skipping
/opt/vpn-director/vpn-director.sh --unless-stopped apply  # Skip once "stop" has run (the bot's subscription watch)

# Component-specific commands
/opt/vpn-director/vpn-director.sh status tunnel       # Tunnel Director status only
/opt/vpn-director/vpn-director.sh restart xray        # Restart Xray TPROXY only
/opt/vpn-director/vpn-director.sh restart xray-process  # Restart the Xray process, TPROXY rules kept

# Import servers
/opt/vpn-director/import_server_list.sh
```

```bash
# Build (from the repository root)
make build-webui         # Vue SPA -> go:embed -> webui binary
make build-all           # both daemons for arm64 and arm
make -C server test      # Go tests

# Web UI in development: plain HTTP, testdata/dev paths, mock shell, admin/admin
cd server && go run ./cmd/webui --dev
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
| `router/opt/vpn-director/lib/xrayconf.sh` | Build Xray outbound (REALITY/TLS) + config.json from a server |
| `router/opt/etc/init.d/S99vpn-director` | Entware init.d script for startup |
| `router/jffs/scripts/firewall-start` | Asuswrt-Merlin hook for firewall reload |
| `router/jffs/scripts/wan-event` | Asuswrt-Merlin hook for WAN events |
| `router/test/` | Bats tests (unit/, integration/) |
| `router/opt/vpn-director/vpn-director.json.template` | Unified config template |
| `router/opt/etc/xray/config.json.template` | Xray server config template |
| `install.sh` | Interactive installer |
| `server/cmd/bot/main.go` | Telegram bot daemon: DI, signal handling |
| `server/cmd/webui/main.go` | Web UI daemon: HTTPS server, DI, dev mode |
| `server/internal/webapi/` | HTTP API: router, JWT middleware, handlers, response deadlines |
| `server/internal/auth/` | Password check against the platform password file, JWT issue and validation |
| `web/` | Vue 3 SPA, embedded into the webui binary with `go:embed` |
| `router/opt/vpn-director/setup_telegram_bot.sh` | Bot configuration script |

## Key Concepts

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
- A tunnel key is any id `platform_tunnels` lists (Merlin: `wgcN` and `ovpncN` from
  `/etc/iproute2/rt_tables`, plus `main`; Keenetic: `OpenVPNN` and `WireguardN` from
  RCI, plus `main`); a key the platform does not list is skipped with a warning
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

**Data storage**: `data_dir` in vpn-director.json (default: `/opt/vpn-director/data`) — servers.json, ipset dumps

**Web UI settings**: the `webui` section of `vpn-director.json` — `port` (8444), `cert_file`, `key_file`, `jwt_secret` (auto-generated when empty), `log_level` (`debug|info|warn|error`).

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
- `telegram-bot.md` — Go bot architecture, commands, wizard flow
- `webui.md` — Web UI architecture, API table, authentication, dev mode, update flow
- `entware-init.md` — Entware init system (rc.unslung, rc.func, S* scripts)
