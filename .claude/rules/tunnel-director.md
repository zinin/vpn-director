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
- Order of tunnels in JSON decides which rule a client meets first; a tunnel's fwmark comes from its slot, which it keeps while it has clients (see "Stable slots and the rebuild in place")

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

## Stable slots and the rebuild in place

A tunnel's slot `idx` gives its mark (`(idx + 1) << mark_shift`), its ip rule preference
(`pref_base + idx`) and, on Keenetic, its table (`2000 + idx`). `_tunnel_collect_applied` reads the
previous layout from `TUN_DIR_TABLES`: a tunnel in it keeps its idx, and a tunnel new to the layout
takes the lowest idx that neither the previous nor the new layout holds, so an apply never reuses a
slot it frees. The chain keeps the JSON order (the failover clients first), so the first match still
wins between overlapping CIDRs. The slots used to follow the JSON order, and a tunnel that gained
its first client or lost its last shifted the mark, preference and table of every tunnel after it.

A rebuild no longer starts with `tunnel_stop`:

1. Routing first, for every tunnel of the new layout. A new slot's table is released, its route
   ensured and its ip rule replaced; a kept slot's route and rule are only ensured
   (`_tunnel_rule_ensure` leaves a correct rule alone), and its table is never released.
2. `TUN_DIR_NEW` is built (`_tunnel_build_chain`) and swapped in by `swap_fw_chain`.
3. Every slot the new layout dropped loses its ip rule (only when it is ours) and its table
   (`_tunnel_slot_release`) — after the swap, when no packet carries its mark.
4. The hash and `failover_ready` are recorded. `TUN_DIR_TABLES` holds the union of both layouts
   from step 1 to step 3, and the new layout after.

The hash is removed when a rebuild starts and written back only by one that completes: a rebuild
that dies part-way leaves the next apply a rebuild, which finishes the interrupted swap
(`swap_fw_chain` does that before anything else; see "An apply never flushes a live chain or set"
in `shell-conventions.md`) and releases the slots left on record. A rebuild whose swap did not take
over every interface (`swap_fw_chain` returned 2 or 3) records no hash and releases nothing.

Both writes of `TUN_DIR_TABLES` go into `tun_dir_tables.new` beside it, which a rename then puts in
its place (`_tunnel_tables_write`). Written in place, the record was emptied before the new lines
went in: an apply killed in that instant, or a full `/tmp` failing the write, left it empty or
partial after the hash was already gone, and the next rebuild handed out slots the live chain still
marked with as free ones.

## What tunnel_apply reports as not carried

`tunnel_apply` returns 0 on a failure that costs some clients - a refused rule, a swap that did not
take over every interface, a tunnel the platform does not list - so its status cannot tell a full
apply whether the clients that left Xray are carried now. It lists in `TUNNEL_UNCARRIED`, an array
reset at its start, the clients the live `TUN_DIR` may not mark or route when it returns: each
spelled as its tunnel's client list spells it (paused clients are not in those lists), each once.
`tunnel_uncarried` prints them.

| Path | Reported |
|------|----------|
| Rebuild | The clients of a tunnel `_tunnel_collect_applied` gave no slot: one the platform does not list, one the mark field has no room for |
| Rebuild | The clients of a slot whose route or ip rule is not in place after the routing step, new or kept: its mark reaches `main` |
| Rebuild | A client `_tunnel_emit_client` skipped as outside RFC1918, or whose MARK rule was refused - or whose offload opt-out was: the fast path then takes its flows past mangle after their first packets |
| Rebuild | Every client, when `swap_fw_chain` did not take over every interface (rc 2 or 3): an interface still on the old chain marks none of the clients new to it |
| Up to date | The clients of a slot whose route or rule did not go back in (`_tunnel_ensure_routes`), every client when a PREROUTING jump did not, and every client outside RFC1918: the rebuild that skipped it recorded the hash, and no path marks it |
| No tunnels (`{}`) | Nothing: no client is on a tunnel |

The failover clients are clients of their tunnel and are reported with it. `vpn-director.sh` hands
the list to `tproxy_prune`, which keeps each client `XRAY_CLIENTS` still holds: a client moving from
Xray to a tunnel stays proxied until an apply in which Tunnel Director carries it, instead of
leaving through the WAN. A client that left Xray for direct is not in the list, and a client that
was not proxied - one moving between two tunnels - is not added to Xray (`packet-flow.md`, "The
apply: make before break"). `apply xray` and `restart xray` run no `tunnel_apply`: they hand the
prune every client the config puts on a tunnel (`tunnel_clients`).

## State Tracking

| File | Purpose |
|------|---------|
| `/tmp/tunnel_director/tun_dir_rules.sha256` | Hash of last applied config (`TUN_DIR_HASH`) |
| `/tmp/tunnel_director/tun_dir_tables` | Applied tunnels, `<idx> <id>` per line (`TUN_DIR_TABLES`) |

`tunnel_apply` writes both; `tunnel_stop` walks `TUN_DIR_TABLES` to release each tunnel's table
through `platform_tunnel_table_release`, then removes both files.

The two can be out of step in one direction: an apply that skipped a tunnel the platform does not
list writes `TUN_DIR_TABLES` but deliberately no hash, so the next apply retries instead of
reporting itself up-to-date. A rebuild reads its previous layout from `TUN_DIR_TABLES` whatever the
hash says, so the tunnels on record keep their slots and the ones gone from the layout are released.

A failover tunnel whose route or ip rule cannot be installed still writes the hash (when every
configured tunnel was applied) and returns 0 with a WARN, so S99 start, hooks and Web UI Apply do
not fail while the fallback interface is still coming up. The watch does not Commit Xray membership
until `/tmp/tunnel_director/failover_ready` names that tunnel (route and ip rule installed);
`TUN_DIR_TABLES` alone is written even when those failed. The up-to-date path always re-installs
recorded routes **and ip rules** (`_tunnel_rule_ensure`, one per row of `TUN_DIR_TABLES`), then
re-checks the failover tunnel's row and its rule (`pref TUN_DIR_PREF_BASE+idx`) and rewrites or
removes `failover_ready` without a rebuild. Re-installing the rules is what makes a failed
`ip rule add` recoverable at all: the hash is recorded regardless, so every later apply lands in
this branch, and a rule that is only checked here would stay missing until the configuration
changed — the clients of that tunnel falling through to `main`, and the watch waiting on a
`failover_ready` nothing would write. A rule that is in place is left alone; deleting and
re-adding it is a window in which marked packets reach `main`. Whether a rule is there is read from
the whole `ip rule show` listing (`_tunnel_rule_listed`), never piped into `grep -q` — see the
pipefail pitfall in `shell-conventions.md`. "There" means this module's rule, not a rule on the
preference: `from all fwmark <mark>/<mask> lookup <table>`, the table as the kernel prints it
(`rt_table_label`) and every part a whole word, since iproute2 4.4 ends each rule with a space. A
look at the preference alone took another owner's rule for ours, so ours was never put back and
`failover_ready` went out with the failover marks routed by that rule or by `main`; such a rule now
makes way, as it does on a rebuild.

A client entry that is no IPv4 address or CIDR (`is_ipv4_net`: `192.168.1.1000`, an octet with a
leading zero, IPv6) or lies outside RFC1918 is skipped with a WARN, and the rest applies: `is_lan_ip`
looks at the prefix only, and the failover copies `xray.clients` into a tunnel. A chain rule the
kernel refuses costs that client only — `_tunnel_emit_client` checks every rule itself and runs
under `||`, because under errexit one refused MARK used to end the apply half-way. Such a rebuild
is not recorded as up-to-date, so the next apply rebuilds and retries it: the up-to-date branch
looks for the MARK rules only, not for an exclusion or offload rule.

The PREROUTING jumps: a rebuild whose new jumps did not all go in records no hash, and the next
apply finishes the swap. A jump that goes missing after a recorded rebuild is put back by the
up-to-date branch (`_tunnel_jumps_ensure`), which leaves one that is there where it is.
`failover_ready` needs the jumps, and every failover client still on the failover tunnel marked —
one outside RFC1918 is TPROXY's to take but never TUN_DIR's, and dropped from Xray it would leave
through the WAN.

A chain can also lose rules a recorded rebuild put in. Merlin's firewall start runs
`iptables -t mangle -F`, which empties `TUN_DIR` without deleting it (the pitfall in
`shell-conventions.md`), and `firewall-start` then applies again with the hash, the chain and
`TUN_DIR_TABLES` all reading as applied. The up-to-date path used to put the jumps back into the
empty chain and write `failover_ready`; every client, the failover clients the watch then took off
Xray among them, left through the WAN until the configuration changed. It now asks `iptables -C`
for the MARK rule of every client of every row of `TUN_DIR_TABLES` (`_tunnel_marks_present`) and
rebuilds when one is gone. The failover clients are clients of their tunnel under its mark, so they
are among them. A client the rebuild skips (no IPv4 address, outside RFC1918) is skipped there
too: looking for its rule would rebuild the chain on every apply.

A rebuild releases the table of a new slot right before it ensures the route
(`platform_tunnel_table_release`, then `platform_tunnel_route_ensure`): an apply that died after
ensuring a route but before recording its slot — an earlier version recorded the slots last — left
that route with no record at all, and the next apply, handing the index to a tunnel whose route
cannot be installed, would otherwise send its clients through the previous owner's tunnel. A kept
slot's table is never released: it is carrying traffic.

**Rebuild triggers**:
- Config hash changed
- Chain does not exist
- `TUN_DIR_TABLES` is missing
- A client's MARK rule is missing from the chain (`_tunnel_marks_present`)
- `TUN_DIR_FORCE_REBUILD=1` (`restart`, `restart tunnel`)

## Key Functions

**Public API**:

| Function | Purpose |
|----------|---------|
| `tunnel_status()` | Show chain, ip rules, configured tunnels |
| `tunnel_apply()` | Apply rules from config (idempotent; a rebuild happens in place); reports the clients it does not carry in `TUNNEL_UNCARRIED` |
| `tunnel_uncarried()` | Print `TUNNEL_UNCARRIED`, one client per line |
| `tunnel_clients()` | Print every client the config puts on a tunnel, `main` included, paused ones left out |
| `tunnel_stop()` | Remove chain, ip rules and the tunnel tables this module owns |
| `tunnel_get_required_ipsets()` | Return list of exclude ipsets needed |

**Internal functions** (for testing):

| Function | Purpose |
|----------|---------|
| `_tunnel_init()` | Initialize module state (valid tables from `platform_tunnels`, fwmark helpers) |
| `_tunnel_table_allowed(id)` | Check the tunnel id is one `platform_tunnels` lists |
| `_tunnel_ensure_routes()` | Re-install the routes of the applied tunnels (used when nothing needs rebuilding) |
| `_tunnel_marks_present()` | Does `TUN_DIR` still hold every client's MARK rule; one that is gone makes the apply a rebuild |
| `_tunnel_collect_applied(prev)` | The slot of every tunnel of the new layout: kept from `prev`, or the lowest idx neither layout holds |
| `_tunnel_build_chain(chain)` | `swap_fw_chain`'s build_fn: every client's rules, the failover clients first |
| `_tunnel_slot_release(idx, tunnel)` | Drop the ip rule (ours only) and the table of a slot the layout dropped |
| `_tunnel_tables_write(file...)` | Replace `TUN_DIR_TABLES` with the lines of the files, each once, through a file beside it and a rename |
| `_tunnel_clients_of(tunnel)` | The clients of one tunnel, one per line; none for an entry that is no object or clients that are no array |
| `_tunnel_not_carried(clients...)` | Add clients to `TUNNEL_UNCARRIED`, each once |
| `_tunnel_not_carried_non_rfc1918()` | Add every configured client outside RFC1918 (the up-to-date path) |

**Module state variables**:
- `_tunnel_valid_tables` - space-separated tunnel ids from `platform_tunnels`
- `_tunnel_mark_field_max` - max tunnels that fit in fwmark field (default: 255)
- `_tunnel_mark_mask_hex` - hex string of `TUN_DIR_MARK_MASK`

## Dependencies

From `lib/common.sh`: `log`, `tmp_file`, `compute_hash`, `is_lan_ip`

From `lib/firewall.sh`: `swap_fw_chain`, `delete_fw_chain`, `ensure_fw_rule`, `sync_fw_rule`, `purge_fw_rules`, `find_fw_rules`, `fw_chain_exists`

From `lib/ipset.sh`: `_ipset_exists`, `parse_exclude_sets_from_json`, `TUN_DIR_HASH`, `TUN_DIR_TABLES`

From the platform contract (`lib/platform.sh`, sourced by `common.sh`): `platform_tunnels`,
`platform_tunnel_table`, `platform_tunnel_route_ensure`, `platform_tunnel_table_release`,
`platform_tunnel_offload_target`, `platform_prerouting_base_pos`, `platform_lan_ifaces`

## Requirements

- Requires ipsets from `lib/ipset.sh` (country codes in exclude lists)
- VPN client must be active with NAT enabled
