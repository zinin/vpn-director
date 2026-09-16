---
paths: "router/opt/vpn-director/**"
---

# Tunnel Director

Policy-based outbound routing through VPN tunnels (WireGuard/OpenVPN) using exclusion-based logic.

Module location: `lib/tunnel.sh`

## Overview

Routes LAN client traffic through VPN tunnels. All traffic goes through the tunnel except destinations in exclude lists (country ipsets).

## Configuration Format

```json
{
  "tunnel_director": {
    "tunnels": {
      "wgc1": {
        "clients": ["192.168.50.0/24", "192.168.50.100"],
        "exclude": ["<country_code>"]
      },
      "ovpnc1": {
        "clients": ["192.168.1.5"],
        "exclude": ["<country_code>"]
      },
      "OpenVPN0": {
        "clients": ["192.168.1.10"],
        "exclude": ["<country_code>"],
        "gateway": "10.73.149.1"
      }
    }
  }
}
```

| Field | Description |
|-------|-------------|
| Tunnel key | A tunnel id `platform_tunnels` lists (Merlin: `wgcN`, `ovpncN` from `/etc/iproute2/rt_tables`, plus `main`; Keenetic: `OpenVPNN`, `WireguardN` from RCI, plus `main`) |
| `clients` | Array of LAN IPs/CIDRs (RFC1918 only) |
| `exclude` | Array of country codes for direct routing |
| `gateway` | Optional. OpenVPN next hop; ignored on WireGuard. On Merlin it fills ovpncN when the server does not push redirect-gateway |

## Behavior

- All traffic from `clients` routes through the tunnel
- Traffic to destinations in `exclude` ipsets bypasses VPN (goes direct)
- Order of tunnels in JSON determines fwmark assignment

## Chain Architecture

Single chain in mangle table:

```
PREROUTING
    └─> TUN_DIR
          ├─ src 192.168.50.0/24 + dst <country> -> RETURN
          ├─ src 192.168.50.0/24 -> PPE                  (Keenetic only)
          ├─ src 192.168.50.0/24 -> MARK 0x10000 (wgc1)
          ├─ src 192.168.1.5 + dst <country> -> RETURN
          ├─ src 192.168.1.5 -> PPE                      (Keenetic only)
          └─ src 192.168.1.5 -> MARK 0x20000 (ovpnc1)
```

The routing table for each tunnel comes from `platform_tunnel_table` (Merlin: the
id from `rt_tables`; Keenetic: `KEENETIC_TABLE_BASE + idx`). `platform_tunnel_offload_target`
prints `PPE` on Keenetic (nothing, rc 1 on Merlin); `tunnel.sh` places that target
inside `TUN_DIR` with the identical match immediately before `MARK`.

## Advanced Configuration

| JSON Path | Default | Purpose |
|-----------|---------|---------|
| `advanced.tunnel_director.chain` | `TUN_DIR` | Chain name |
| `advanced.tunnel_director.pref_base` | `16384` | ip rule priority base |
| `advanced.tunnel_director.mark_mask` | `0x00ff0000` | Fwmark mask |
| `advanced.tunnel_director.mark_shift` | `16` | Bit position |

## Fwmark Layout

Default: bits 16-23 (8 bits = 255 tunnels max)

## State Tracking

| File | Purpose |
|------|---------|
| `/tmp/tunnel_director/tun_dir_rules.sha256` | Hash of last applied config (`TUN_DIR_HASH`) |
| `/tmp/tunnel_director/tun_dir_tables` | Applied tunnels, `<idx> <id>` per line (`TUN_DIR_TABLES`) |

`tunnel_apply` writes both; `tunnel_stop` walks `TUN_DIR_TABLES` to release each tunnel's table
through `platform_tunnel_table_release`, then removes both files.

The two can be out of step in one direction: an apply that skipped a tunnel the platform does not
list writes `TUN_DIR_TABLES` but deliberately no hash, so the next apply retries instead of
reporting itself up-to-date. The cleanup that precedes a rebuild therefore keys off **either**
file — keying it off the hash alone left those tables allocated while the next apply handed their
indices to other tunnels, and on Keenetic table `2000+idx` then still held the previous tunnel's
route.

A failover tunnel whose route or ip rule cannot be installed still writes the hash (when every
configured tunnel was applied) and returns 1, so the watch does not drop Xray membership. Deleting
the hash there forced every later apply through `tunnel_stop`, which takes TUN_DIR down for every
client while the fallback interface is still coming up.

The rebuild loop also releases each tunnel's table right before it ensures the route
(`platform_tunnel_table_release`, then `platform_tunnel_route_ensure`): an apply that dies after
ensuring a route but before writing `TUN_DIR_TABLES` leaves that route with no record at all, and
the next apply, handing the index to a tunnel whose route cannot be installed, would otherwise
send its clients through the previous owner's tunnel.

**Rebuild triggers**:
- Config hash changed
- Chain does not exist
- `TUN_DIR_TABLES` is missing

## Key Functions

**Public API**:

| Function | Purpose |
|----------|---------|
| `tunnel_status()` | Show chain, ip rules, configured tunnels |
| `tunnel_apply()` | Apply rules from config (idempotent) |
| `tunnel_stop()` | Remove chain, ip rules and the tunnel tables this module owns |
| `tunnel_get_required_ipsets()` | Return list of exclude ipsets needed |

**Internal functions** (for testing):

| Function | Purpose |
|----------|---------|
| `_tunnel_init()` | Initialize module state (valid tables from `platform_tunnels`, fwmark helpers) |
| `_tunnel_table_allowed(id)` | Check the tunnel id is one `platform_tunnels` lists |
| `_tunnel_ensure_routes()` | Re-install the routes of the applied tunnels (used when nothing needs rebuilding) |

**Module state variables**:
- `_tunnel_valid_tables` - space-separated tunnel ids from `platform_tunnels`
- `_tunnel_mark_field_max` - max tunnels that fit in fwmark field (default: 255)
- `_tunnel_mark_mask_hex` - hex string of `TUN_DIR_MARK_MASK`

## Dependencies

From `lib/common.sh`: `log`, `tmp_file`, `compute_hash`, `is_lan_ip`

From `lib/firewall.sh`: `create_fw_chain`, `delete_fw_chain`, `ensure_fw_rule`, `sync_fw_rule`, `purge_fw_rules`, `fw_chain_exists`

From `lib/ipset.sh`: `_ipset_exists`, `parse_exclude_sets_from_json`, `TUN_DIR_HASH`, `TUN_DIR_TABLES`

From the platform contract (`lib/platform.sh`, sourced by `common.sh`): `platform_tunnels`,
`platform_tunnel_table`, `platform_tunnel_route_ensure`, `platform_tunnel_table_release`,
`platform_tunnel_offload_target`, `platform_prerouting_base_pos`, `platform_lan_ifaces`

## Requirements

- Requires ipsets from `lib/ipset.sh` (country codes in exclude lists)
- VPN client must be active with NAT enabled
