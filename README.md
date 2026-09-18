🇬🇧 English | [🇷🇺 Русский](README.ru.md)

# VPN Director for Asuswrt-Merlin and KeeneticOS

Selective traffic routing through Xray TPROXY and OpenVPN/WireGuard tunnels.

## Features

- **Xray TPROXY**: Transparent proxy for selected LAN clients via VLESS
- **Tunnel Director**: Route traffic through OpenVPN/WireGuard by destination
- **Country-based routing**: Route traffic directly or through VPN based on destination geography
- **Web UI**: HTTPS web interface for managing VPN Director from the browser
- **Telegram Bot**: Remote management via Telegram (status, config, restart)
- **Easy installation**: One-command setup with interactive configuration

## Quick Install

```bash
curl -fsSL \
  -H "Cache-Control: no-cache" \
  -H "Pragma: no-cache" \
  -H "If-Modified-Since: Thu, 01 Jan 1970 00:00:00 GMT" \
  "https://raw.githubusercontent.com/zinin/vpn-director/master/install.sh?v=$(date +%s)" \
| /opt/bin/bash
```

After installation:

1. Import VLESS servers (optional):
   ```bash
   /opt/vpn-director/import_server_list.sh
   ```

2. Run the configuration wizard:
   ```bash
   /opt/vpn-director/configure.sh
   ```

3. Access Web UI (installed automatically):
   ```
   https://<router-ip>:8444
   ```
   Merlin: the router admin password. KeeneticOS: user `root` with the Entware password.

4. Setup Telegram bot (optional):
   ```bash
   /opt/vpn-director/setup_telegram_bot.sh
   ```

## Requirements

### Asuswrt-Merlin

- Asuswrt-Merlin firmware
- Entware installed
- Required packages:
  ```bash
  opkg install curl coreutils-base64 coreutils-sha256sum gawk jq xray-core procps-ng-pgrep procps-ng-pkill procps-ng-ps
  ```
- OpenVPN client configured in router UI (for Tunnel Director)

### KeeneticOS

- KeeneticOS 5.x (verified on 5.1.5) with Entware installed on USB storage
- Firmware component "Kernel modules for Netfilter" (the router reboots once when it is added)
- Packages `install.sh` installs on request (bash itself must be installed first: `opkg install bash`):
  ```bash
  opkg install bash curl jq iptables ipset ip-full flock coreutils-nohup coreutils-base64 coreutils-sha256sum gawk procps-ng-pgrep procps-ng-pkill procps-ng-ps openssl-util cron xray
  ```
- An OpenVPN or WireGuard client interface configured in the router UI (for Tunnel Director)

MIPS (`mipsle`) builds ship untested.

Known limitation: the tunnel list is built from every OpenVPN and WireGuard interface the
router has, an OpenVPN or WireGuard **server** included. Do not assign clients to a server
interface: Tunnel Director would route them into the router's own server tunnel, where their
traffic is dropped. Telling servers apart needs a router that has one to check the RCI fields
against; that check is still open.

### Optional

- `opkg install wget-ssl` — faster and more reliable downloads for country zone files (recommended)
- `opkg install openssl-util` — for email notifications
- `opkg install monit` — for automatic Xray restart on crash (see [Process Monitoring](#process-monitoring))
- `opkg install coreutils-tr` — fixes buggy `tr` command (stock busybox `tr` corrupts characters with certain locales)

## Manual Configuration

After installation, configs are located at:

- `/opt/vpn-director/vpn-director.json` - Unified config (Xray + Tunnel Director)
- `/opt/etc/xray/config.json` - Xray server configuration

## Commands

```bash
# VPN Director CLI
/opt/vpn-director/vpn-director.sh status              # Show all status
/opt/vpn-director/vpn-director.sh apply               # Apply configuration
/opt/vpn-director/vpn-director.sh stop                # Stop all components
/opt/vpn-director/vpn-director.sh restart             # Restart all
/opt/vpn-director/vpn-director.sh update              # Update ipsets + reapply

# Component-specific
/opt/vpn-director/vpn-director.sh status tunnel       # Tunnel Director status only
/opt/vpn-director/vpn-director.sh status ipset        # IPSet status only
/opt/vpn-director/vpn-director.sh restart xray        # Restart Xray TPROXY only

# Options (can be used with any command)
/opt/vpn-director/vpn-director.sh -v status           # Verbose output
/opt/vpn-director/vpn-director.sh -f apply            # Force reapply
/opt/vpn-director/vpn-director.sh --dry-run apply     # Show what would be done
/opt/vpn-director/vpn-director.sh --wait apply        # Wait up to 120 s for a running instance instead of skipping
/opt/vpn-director/vpn-director.sh --unless-stopped apply  # Skip if "stop" has run since (the bot's subscription watch)

# Import servers
/opt/vpn-director/import_server_list.sh
```

## Web UI

HTTPS web interface for managing VPN Director from the browser.

### Access

Open `https://<router-ip>:8444`.

- **Asuswrt-Merlin**: log in with the router admin password (authenticated via `/etc/shadow`).
- **KeeneticOS**: log in as user `root` with the Entware password (`/opt/etc/passwd`, set with `passwd` over SSH).

A self-signed TLS certificate is generated automatically during installation. Your browser will show a security warning — this is expected.

### Features

| Tab | Description |
|-----|-------------|
| **Status** | VPN Director operational overview |
| **Servers** | Xray server management, switch active server |
| **Clients** | LAN client routing assignment (pause/resume/delete) |
| **Exclusions** | Country and IP/CIDR exclusion lists |
| **Logs** | Log viewer (bot, vpn, xray, webui) |
| **Settings** | Version, self-update, configuration |

### Configuration

Web UI settings are in `/opt/vpn-director/vpn-director.json` under the `webui` section:

```json
{
  "webui": {
    "port": 8444,
    "cert_file": "/opt/vpn-director/certs/server.crt",
    "key_file": "/opt/vpn-director/certs/server.key",
    "jwt_secret": "",
    "log_level": "info"
  }
}
```

`jwt_secret` is auto-generated on first start if left empty. `log_level` accepts `debug`, `info`, `warn`, `error` (default `info`). The Web UI logs to `/tmp/vpn-director-webui.log`; all logs are truncated at 200 KB.

### Service Management

```bash
/opt/etc/init.d/S98vpn-director-webui start
/opt/etc/init.d/S98vpn-director-webui stop
/opt/etc/init.d/S98vpn-director-webui restart
```

### Updates

The **Settings** tab shows the running version, the latest GitHub release and its changelog. «Update to vX» downloads the release and updates both the Web UI and the Telegram bot, restarting the ones that were running; the page polls for the new version and reloads itself when it comes up. The login session survives the update.

An update started from the Web UI is announced in Telegram to every active chat. `/update` in the bot does the same thing from the other side — both paths update both daemons.

Updates are authenticated by TLS to github.com and nothing else — there is no signature and no checksum on the binaries or the scripts, and they are installed and run as root. This is the same trust model as the `curl … | bash` install command above; anyone who can publish a release to this repository can run code on your router.

> Upgrading **to** the first release with the unified updater is still done by the old bot-only updater, which does not know about the Web UI. Re-run the [Quick Install](#quick-install) command once after that upgrade; every later update handles both.

> **Upgrading from v0.11.x or earlier.** The updater built into those releases downloads a fixed file list without `lib/platform.sh`, so pressing «Update» installs this release incompletely: the shell CLI, the firewall hooks and the daily update stop working (the Xray TPROXY and Tunnel Director rules are no longer re-applied after a firewall restart) until you re-run the [Quick Install](#quick-install) command once. The Web UI and the bot themselves keep running. Every later update reads the release manifest and needs no such step.

## Telegram Bot

Remote management via Telegram with username-based authorization.

### Setup

1. Create a bot via [@BotFather](https://t.me/BotFather) and get the token
2. Run setup script:
   ```bash
   /opt/vpn-director/setup_telegram_bot.sh
   ```
3. Enter bot token and allowed usernames (without @)

### Bot Commands

| Command | Description |
|---------|-------------|
| `/status` | VPN Director status |
| `/xray` | Switch Xray server |
| `/servers` | Server list |
| `/import <url>` | Import VLESS subscription (auto-syncs xray.servers) |
| `/exclude` | Manage excluded IPs/CIDRs |
| `/clients` | Manage VPN clients |
| `/configure` | Configuration wizard |
| `/restart` | Restart VPN Director |
| `/stop` | Stop VPN Director |
| `/logs [bot\|vpn\|xray\|webui\|all] [N]` | Recent logs (default: all, 20 lines) |
| `/ip` | External IP |
| `/update` | Update to latest release |
| `/version` | Bot version |

### Configuration Wizard

The `/configure` command starts a 4-step wizard:
1. Select Xray server
2. Exclude from proxy (country codes, IPs/CIDRs)
3. Configure LAN clients with routing (Xray/OpenVPN/WireGuard)
4. Review and apply

## How It Works

### Xray TPROXY

Traffic from specified LAN clients is transparently redirected through Xray using TPROXY. The proxy uses VLESS protocol over TLS to connect to your VPN server.

### Tunnel Director

Routes traffic from specified LAN clients through OpenVPN/WireGuard tunnels based on destination. Configurable exclusions allow direct access to specified countries for optimal performance. A tunnel key is an id the platform lists (`wgc1` / `ovpnc1` on Merlin, `OpenVPN0` / `Wireguard1` on KeeneticOS). An OpenVPN tunnel may set optional `gateway` (the next hop; otherwise the subnet's first host). WireGuard ignores it.

```json
{
  "tunnel_director": {
    "tunnels": {
      "wgc1": { "clients": ["192.168.50.0/24"], "exclude": ["<country_code>"] },
      "OpenVPN0": {
        "clients": ["192.168.1.5"],
        "exclude": ["<country_code>"],
        "gateway": "10.73.149.1"
      }
    }
  }
}
```

### Country IPSets

Country IP lists are downloaded automatically from multiple sources with fallback:
1. GeoLite2 via GitHub (firehol/blocklist-ipsets) — most accurate
2. IPDeny via GitHub mirror — not blocked in most regions
3. IPDeny direct — may be blocked in some regions
4. Manual fallback — interactive prompt if all sources fail

## Startup Scripts

This project uses Entware init.d for automatic startup:

| Script | When Called | Purpose |
|--------|-------------|---------|
| `/opt/etc/init.d/S99vpn-director` | After Entware initialized | Runs `vpn-director.sh apply` to initialize all components |
| `/jffs/scripts/firewall-start` | After firewall rules applied (Merlin) | Reapplies configuration after firewall reload |
| `/jffs/scripts/wan-event` | On WAN connected (Merlin) | Runs `vpn-director.sh apply` on WAN connection |
| `/opt/etc/ndm/netfilter.d`, `wan.d`, `iflayerchanged.d` (`50-vpn-director.sh`) | NDM firewall rebuild / WAN start / VPN client IPv4 layer (KeeneticOS) | Detached `vpn-director.sh --wait apply` |

**Note:** The init.d script ensures Entware bash is available before running vpn-director scripts.

On Merlin, to enable user scripts: Administration -> System -> Enable JFFS custom scripts and configs -> Yes

## Process Monitoring

Xray, Telegram bot, and Web UI may occasionally crash. Use monit for automatic restart.

### Setup

1. Install monit:
   ```bash
   opkg install monit
   ```

2. Create configs in `/opt/etc/monit.d/`:

   **xray:**
   ```
   check process xray matching "xray"
       start program = "/opt/etc/init.d/S24xray start"
       stop program = "/opt/etc/init.d/S24xray stop"
       if does not exist then restart
   ```

   **telegram-bot:**
   ```
   check process telegram-bot matching "telegram-bot"
       start program = "/opt/etc/init.d/S98telegram-bot start"
       stop program = "/opt/etc/init.d/S98telegram-bot stop"
       if does not exist then restart
   ```

   **webui:**
   ```
   check process webui matching "webui"
       start program = "/opt/etc/init.d/S98vpn-director-webui start"
       stop program = "/opt/etc/init.d/S98vpn-director-webui stop"
       if does not exist then restart
   ```

3. Enable config directory in `/opt/etc/monitrc`:
   ```
   include /opt/etc/monit.d/*
   ```

4. Edit `/opt/etc/monitrc`, set check interval:
   ```
   set daemon 30    # check every 30 seconds
   ```

5. Restart monit:
   ```bash
   /opt/etc/init.d/S99monit restart
   ```

6. Verify:
   ```bash
   monit status
   ```

## License

Copyright (C) 2026 Alexander Zinin <mail@zinin.ru>

Licensed under the GNU Affero General Public License v3.0 or later
(AGPL-3.0-or-later). See `LICENSE`.
