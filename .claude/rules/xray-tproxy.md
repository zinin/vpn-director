---
paths: "router/opt/vpn-director/**"
---

# Xray TPROXY

Transparent proxy routing for LAN clients via Xray.

Module location: `lib/tproxy.sh`

## Usage via CLI

```bash
vpn-director.sh status xray       # Show Xray TPROXY status
vpn-director.sh restart xray      # Restart Xray TPROXY
vpn-director.sh apply             # Apply all (including TPROXY)
```

## How It Works

1. Selected LAN clients → mangle PREROUTING chain
2. Exclude: servers, private IPs, specified countries
3. Remaining traffic → TPROXY to Xray port
4. Xray dokodemo-door inbound → VLESS outbound

## Outbound Generation (REALITY/TLS)

`config.json.template` is valid JSON with an empty `outbounds: []`. The proxy-out outbound
is generated per selected server from `servers.json` stream params and injected as `outbounds[0]`:

- Shell: `lib/xrayconf.sh` (`xrayconf_generate`, jq) — used by `configure.sh`.
- Go: `service/xray.go` (`buildOutbound` + `encoding/json`) — used by bot wizard and `/xray`.

Per-server params (parsed from the VLESS URI): `security` (`reality`|`tls`), `network`, `flow`,
`sni`, `fingerprint` (fp), `public_key` (pbk), `short_id` (sid), `alpn`.

- `security=reality` -> `realitySettings { serverName, fingerprint, publicKey, shortId }`, user `flow`.
- `security=tls` -> `tlsSettings { serverName (sni||address), fingerprint?, alpn? }`.
- empty `security` -> legacy `tlsSettings { alpn:["h2"], serverName:address }`, no flow.

**Migration after upgrade:** the previously generated `/opt/etc/xray/config.json` stays on disk as
plain TLS until regenerated, so Xray — and the Telegram bot, which proxies its API connection
through it — cannot connect to a REALITY server until the user acts:

1. Re-run `/import` (or, over SSH if the bot is unreachable through the broken proxy:
   `/opt/vpn-director/import_server_list.sh`) so params land in `servers.json`.
2. Re-select the server (`/configure` wizard or `/xray`, or over SSH `/opt/vpn-director/configure.sh`)
   to regenerate `config.json`.

## Configuration

In `vpn-director.json`:

| JSON Path | Default | Purpose |
|-----------|---------|---------|
| `xray.clients` | `[]` | LAN IPs/CIDRs to proxy (JSON array) |
| `xray.servers` | `[]` | Xray server IPs to exclude (avoid loops) |
| `xray.exclude_sets` | `[]` | Country codes/ipsets to skip |
| `advanced.xray.tproxy_port` | `12345` | Xray dokodemo-door port |
| `advanced.xray.route_table` | `100` | ip route table number |
| `advanced.xray.rule_pref` | `200` | ip rule priority |
| `advanced.xray.fwmark` | `0x100` | Routing fwmark (bit 8) |
| `advanced.xray.fwmark_mask` | `0x100` | Fwmark mask |
| `advanced.xray.chain` | `XRAY_TPROXY` | mangle chain name |
| `advanced.xray.clients_ipset` | `XRAY_CLIENTS` | Source clients ipset |
| `advanced.xray.bypass_ipset` | `TPROXY_BYPASS` | Bypass ipset (servers + user excludes + OpenVPN endpoints) |

`advanced.xray.tproxy_port` and `socks_port` are not just read by the firewall
rules: both generators write them into `config.json` — `xrayconf_generate` takes
them as its second and third arguments, `XrayService.GenerateConfig` as
`InboundPorts` — so the dokodemo-door inbound listens where the TPROXY rules
send traffic. Regenerating `config.json` from the template alone would silently
put the inbound back on 12345.

## IPSets Created

| Name | Type | Purpose |
|------|------|---------|
| `XRAY_CLIENTS_IPSET` | `hash:net` | Source clients |
| `TPROXY_BYPASS` | `hash:net` | Bypass set (servers + user excludes + OpenVPN endpoints) |

## Chain Structure

```
PREROUTING (pos 1)
    └─→ XRAY_TPROXY
          ├─ ! src clients → RETURN
          ├─ dst servers → RETURN
          ├─ dst 127.0.0.0/8 → RETURN
          ├─ dst 10.0.0.0/8 → RETURN
          ├─ dst 172.16.0.0/12 → RETURN
          ├─ dst 192.168.0.0/16 → RETURN
          ├─ dst 169.254.0.0/16 → RETURN
          ├─ dst 224.0.0.0/4 → RETURN
          ├─ dst 255.255.255.255 → RETURN
          ├─ dst {exclude_sets} → RETURN
          ├─ tcp → TPROXY :port mark
          └─ udp → TPROXY :port mark

mangle INPUT (Keenetic, pos 1):
    -m mark --mark <fwmark> → ACCEPT
    (so TPROXY-marked HTTPS never reaches _NDM_HTTP_INPUT_TLS_)
```

## Routing Setup

```bash
# Route table (for TPROXY to work)
ip route add local default dev lo table $XRAY_ROUTE_TABLE

# ip rule (fwmark-based lookup)
ip rule add pref $XRAY_RULE_PREF fwmark $XRAY_FWMARK/$XRAY_FWMARK_MASK table $XRAY_ROUTE_TABLE
```

**Fwmark bit allocation**: `0x100` (bit 8) is free from firmware VPN marking (bit 0) and Tunnel Director (bits 16-23).

The TPROXY ip rule uses pref 200. On KeeneticOS 5.1.5 that preference was free of
NDM connection policies (a user policy landed at prefs 102/103, table 4097; no
tables in the 40s). `_tproxy_setup_routing` still only removes rules carrying our
mark or our table, because other firmware versions may still park policies at 200.

## Requirements

- `xt_TPROXY` kernel module
- Xray running with dokodemo-door inbound (tproxy mode)
- ipsets from `lib/ipset.sh` (for country exclusions)

## Fail-Safe

Script exits without changes if:
- Required exclusion ipsets not found
- xt_TPROXY module unavailable

> An unknown country code in `xray.exclude_sets` is **not** one of those
> reasons: `_tproxy_exclude_sets` drops it with a WARN before the check, so a
> typo cannot abort apply.

## Extended Exclusion Sets

Prefers `{set}_ext` variant if exists:
```bash
resolve_exclude_set "<country_code>"  # Returns: <country_code>_ext if exists, else <country_code>
```

## Key Functions

**Public API**:

| Function | Purpose |
|----------|---------|
| `tproxy_status()` | Show XRAY_TPROXY chain, routing, xray process |
| `tproxy_apply()` | Apply TPROXY rules (idempotent), soft-fail if unavailable |
| `tproxy_stop()` | Remove chain and routing |
| `tproxy_restart_process()` | Restart Xray process via Entware init script |
| `tproxy_get_required_ipsets()` | Return list of valid exclude ipsets (unknown codes dropped with a WARN) |

**Internal functions** (for testing):

| Function | Purpose |
|----------|---------|
| `_tproxy_init()` | Initialize module state |
| `_tproxy_check_module()` | Verify/load xt_TPROXY kernel module |
| `_tproxy_check_required_ipsets()` | Fail-safe: exit if exclusion ipsets missing |
| `_tproxy_exclude_sets([-q])` | Validated, lower-cased `XRAY_EXCLUDE_SETS` on one line; `-q` suppresses the WARN per dropped code |
| `_tproxy_resolve_exclude_set(key)` | Try `{set}_ext` first, fall back to `{set}` |
| `_tproxy_setup_routing()` | Create route table + ip rule |
| `_tproxy_teardown_routing()` | Remove route table + ip rule |
| `_tproxy_setup_clients_ipset()` | Create and populate client ipset |
| `_tproxy_setup_bypass_ipset()` | Create and populate bypass ipset |
| `_tproxy_validate_ipv4_cidr()` | Validate IPv4 CIDR format |
| `_tproxy_setup_iptables()` | Build XRAY_TPROXY chain with exclusions |
| `_tproxy_teardown_iptables()` | Remove chain and ipsets |

**Soft-fail behavior**: `tproxy_apply()` returns 0 even if xt_TPROXY unavailable or ipsets missing, allowing caller scripts to continue.

**Ready marker**: `tproxy_apply()` writes `/tmp/xray_tproxy/ready` (`XRAY_TPROXY_READY`) only when
every rule went in, the platform's own included, and removes it on any soft-fail. The Telegram
bot's subscription watch waits for it before Xray clients leave the fallback tunnel. A failed
`platform_tproxy_extra_rules apply` (Keenetic's mangle INPUT accept) fails `_tproxy_setup_iptables`
only after the PREROUTING jumps are in place, so interception stays as it was and only the marker
is withheld. The chain flush or a RETURN rule that does not go in (clients, bypass, the private
ranges, an exclusion) ends it before the TPROXY targets instead, on a flushed chain that intercepts
nothing: without those rules the targets would take every LAN client or LAN-to-LAN traffic.
`XRAY_CLIENTS` has to be complete too: nothing validates `xray.clients`, and an address the set
does not take is RETURNed by rule 1 and left unproxied, so `_tproxy_setup_clients_ipset` reports it
and the marker is withheld although the chain itself is in place. The adds pass `-exist`, so a
repeated address is not a failure.
