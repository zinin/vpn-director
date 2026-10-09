🇬🇧 English | [🇷🇺 Русский](README.ru.md)

# VPN Director for Asuswrt-Merlin and KeeneticOS

Selective traffic routing through Xray TPROXY and OpenVPN/WireGuard tunnels.

## Features

- **Xray TPROXY**: Transparent proxy for selected LAN clients via VLESS, VMess, Trojan, Shadowsocks or Hysteria2
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

1. Add the servers of your subscriptions (optional). The script is a menu: add, refresh, rename and delete subscriptions — up to ten, each a link or a file:
   ```bash
   /opt/vpn-director/import_server_list.sh
   ```
   A new subscription is named after the link's host, or after the file's name without its extension, unless you give it a name. A file or a plain-http link is kept as a static list, which nothing refreshes.

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
- OpenVPN client configured in router UI, with "Redirect Internet traffic through tunnel" set to "VPN Director (policy rules)" (for Tunnel Director)

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
/opt/vpn-director/vpn-director.sh restart             # Restart Xray, rebuild all in place without a stop
/opt/vpn-director/vpn-director.sh update              # Update ipsets + reapply

# Component-specific
/opt/vpn-director/vpn-director.sh status tunnel       # Tunnel Director status only
/opt/vpn-director/vpn-director.sh status ipset        # IPSet status only
/opt/vpn-director/vpn-director.sh restart xray        # Restart Xray, TPROXY applied again in place

# Options (can be used with any command)
/opt/vpn-director/vpn-director.sh -v status           # Verbose output
/opt/vpn-director/vpn-director.sh -f apply            # Force reapply
/opt/vpn-director/vpn-director.sh --dry-run apply     # Show what would be done
/opt/vpn-director/vpn-director.sh --wait apply        # Wait up to 120 s for a running instance instead of skipping
/opt/vpn-director/vpn-director.sh --unless-stopped apply  # Skip if "stop" has run since (the bot's subscription watch)

# Subscriptions: add, refresh, rename, delete
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
| **Servers** | Subscriptions (add, refresh, rename, delete) and their Xray servers, switch active server |
| **Clients** | LAN client routing: add, change the route in place, pause/resume, delete |
| **Exclusions** | Country and IP/CIDR exclusion lists |
| **Logs** | Log viewer (bot, vpn, xray, webui, watchd) |
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

The **Settings** tab shows the running version, the latest GitHub release and its changelog. «Update to vX» downloads the release and updates every daemon — the Web UI, the Telegram bot and the server monitor — restarting the ones that were running and starting one the release adds; the page polls for the new version and reloads itself when it comes up. The login session survives the update.

An update started from the Web UI is announced in Telegram to every active chat. `/update` in the bot does the same thing from the other side — both paths update every daemon.

Updates are authenticated by TLS to github.com and nothing else — there is no signature and no checksum on the binaries or the scripts, and they are installed and run as root. This is the same trust model as the `curl … | bash` install command above; anyone who can publish a release to this repository can run code on your router.

> Upgrading **to** the first release with the unified updater is still done by the old bot-only updater, which does not know about the Web UI. Re-run the [Quick Install](#quick-install) command once after that upgrade; every later update handles every daemon.

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
| `/import <url> [name]` | Add a subscription, or refresh the one saved with that link; /import alone refreshes them all |
| `/subs` | Subscriptions: refresh, rename, delete |
| `/exclude` | Manage excluded IPs/CIDRs |
| `/clients` | Manage VPN clients: move between routes, pause, remove |
| `/configure` | Configuration wizard |
| `/restart` | Restart VPN Director |
| `/stop` | Stop VPN Director |
| `/logs [bot\|vpn\|xray\|webui\|watchd\|all] [N]` | Recent logs (default: all, 20 lines) |
| `/ip` | External IP |
| `/update` | Update to latest release |
| `/version` | Bot version |

### Configuration Wizard

The `/configure` command starts a 4-step wizard:
1. Select Xray server
2. Exclude from proxy (country codes, IPs/CIDRs)
3. Configure LAN clients with routing (Xray/OpenVPN/WireGuard)
4. Review and apply

## Server Monitor

`vpn-director-watchd` checks every server of every subscription, once a minute, through a second Xray process of its own — the live Xray and its clients never notice. A check fetches `http://www.gstatic.com/generate_204` through the server; 204 within 10 s is alive. A dead server is checked less and less often: after 2 minutes, then 4, 8, 16, and every 30 minutes at most. When every server fails at once, the monitor asks `1.1.1.1` and `8.8.8.8` whether the WAN is up, and keeps the statuses while it is down.

The Web UI's **Servers** tab shows each server's status and latency, how many of each subscription are alive, and a `↻` to check a server now; the bot marks `/servers` and `/xray` with 🟢 🔴 ⚪ ⛔.

Estimated cost: about 5 KB of traffic per check, so 100 live servers checked every minute take about 0.7 GB a day, 21 GB a month, spread over the subscriptions they belong to. The daemon is estimated to take 10–15 MB of memory, its Xray about 35 MB. On a router with little memory, or subscriptions with a traffic cap, raise `interval` or set `enabled` to false:

```json
{
  "monitor": {
    "enabled": true,
    "interval": "1m",
    "dead_interval_max": "30m",
    "concurrency": 8,
    "subscription_refresh": "5m",
    "log_level": "info"
  }
}
```

The daemon rereads the section every minute; it logs to `/tmp/vpn-director-watchd.log`.

Every `subscription_refresh` (5 minutes by default; `0` turns it off) the daemon downloads every subscription with a link and writes only what changed, so the monitor checks the addresses a provider serves now. A server that did not change keeps its status, and so does one that differs only in the REALITY `sni`, `sid` or `spx` a panel picks at random for each download; a renamed server keeps its Active mark. The refresh waits while the Xray watch handles a failure, and the Changed column of the subscription list shows when a list last changed.

```bash
/opt/etc/init.d/S98vpn-director-watchd start
/opt/etc/init.d/S98vpn-director-watchd stop
/opt/etc/init.d/S98vpn-director-watchd restart
```

These are estimates, not router measurements; traffic depends on the protocol and retries. Names sharing the same address and outbound share one check, so the example assumes 100 distinct live endpoints. `vpn-director-watchd` owns monitoring and automatic failover; the Telegram bot provides management and notification delivery. Stopping the bot leaves watchd automation running; `monitor.enabled=false` disables endpoint monitoring and the prober while legacy failover continues.

Router validation is still pending on the RT-AX86U (Merlin, BusyBox 1.25) and Keenetic: memory/CPU with real subscriptions, SOCKS user routing, the long daemon name with `pidof`/`killall` and init start/stop/check, live-Xray isolation through `S24xray` and monit, bytes per check, and the first update that introduces the daemon. If a target cannot route SOCKS users, release compatibility requires the per-endpoint-inbound fallback and fresh validation.

## How It Works

### Xray TPROXY

Traffic from specified LAN clients is transparently redirected through Xray using TPROXY. Xray reaches the server you select with the protocol your subscription gives it.

Subscriptions it reads: share links (`vless://`, `vmess://`, `trojan://`, `ss://`, `hysteria2://` / `hy2://`), base64-encoded or plain, and Xray JSON - the array of Xray configs panels such as Remnawave and Marzban give Xray clients. An entry Xray cannot run - TUIC, SSR, a balancer, a chained config - is skipped, and the import says why.

Up to ten subscriptions live side by side; you pick the running server from any of them. When it fails, watchd first tries a direct server switch based on fresh active-dead/candidate-alive monitor evidence and a working WAN, keeping client assignments and TPROXY routing in place. Without that proof or when the direct attempt fails, its legacy path confirms the failure, moves eligible LAN clients onto a Tunnel Director tunnel, refreshes every linked subscription at once and walks their servers — the chosen one and two more of its subscription, then one server of each subscription in turn; with valid monitor evidence, the endpoints the monitor last saw alive go first and those Xray rejected in the current generation are skipped — until one answers, and restores the moved clients once Xray TPROXY is ready.

### Tunnel Director

Routes traffic from specified LAN clients through OpenVPN/WireGuard tunnels based on destination. Configurable exclusions allow direct access to specified countries for optimal performance. A tunnel key is an id the platform lists (`wgc1` / `ovpnc1` on Merlin, `OpenVPN0` / `Wireguard1` on KeeneticOS). On Merlin an OpenVPN client is listed only in "VPN Director (policy rules)" mode: in "No" and "Yes (all)" the firmware sends all of the router's traffic through the client's routing table, so a route Tunnel Director put there would take every device into the tunnel. An apply skips such a client with a warning that names its mode. If Tunnel Director still has the tunnel on record, an apply also takes out a default route an earlier version left in its table in "No" mode. An OpenVPN tunnel may set optional `gateway` (the next hop; otherwise the subnet's first host). WireGuard ignores it.

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

### Changing a client's route

Pick another route for a client in the Web UI (the Route column) or in the bot (`/clients`, 🔀). It is one change and one apply, and the apply moves the client make-before-break: the client stays on its old route until the new one carries it, so none of its traffic leaves through the WAN in between. Every apply works that way — chains and sets are built beside the live ones and swapped in — and so does a server switch, which restarts Xray with its rules in place.

What remains:

- The firmware's own firewall rebuilds (a firewall restart on Merlin, an NDM rebuild on KeeneticOS) empty the chains until the hook applies them again.
- A tunnel that is down sends its clients through the WAN (on Merlin, the VPN client's killswitch, when enabled, prevents that). The Web UI and the bot ask before they move a client to a tunnel that is down or that the router does not list.
- A move changes the route of new connections. An open connection may break (reconnect it) or, on KeeneticOS, keep its old route until its conntrack entry expires. A UDP flow moved from Xray to a tunnel can stall until its conntrack entry expires.
- Only a full `apply`, `restart` or `update` moves a client between Xray and a tunnel. The Web UI and the bot move clients with a full apply (a server switch, which changes no client's route, runs `restart xray`). After moving a client by hand in `vpn-director.json`, run a full `vpn-director.sh apply`: `apply xray` and `restart xray` keep a client moved from Xray to a tunnel proxied until then, but `apply tunnel` and `restart tunnel`, which move a client between tunnels in place, add nothing to Xray, so a client moved from a tunnel to Xray goes out through the WAN until the full apply.
- `/opt/etc/init.d/S99vpn-director restart` stops the service and starts it again, and the clients go out through the WAN in between; `vpn-director.sh restart` rebuilds in place.
- IPv6 is routed by neither module: where the LAN has IPv6, a client's IPv6 traffic bypasses Xray and Tunnel Director.

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

Xray, Telegram bot, Web UI, and the server monitor may occasionally crash. Use monit for automatic restart.

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

   **vpn-director-watchd:**
   ```
   check process vpn-director-watchd matching "vpn-director-watchd"
       start program = "/opt/etc/init.d/S98vpn-director-watchd start"
       stop program = "/opt/etc/init.d/S98vpn-director-watchd stop" with timeout 330 seconds
       if does not exist then restart
   ```

   The monitor's prober runs as `vpn-director-probe`, so the `xray` rule above never takes it for Xray.

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
