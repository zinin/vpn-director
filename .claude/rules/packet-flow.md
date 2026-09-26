# Packet Flow Architecture

Detailed description of how traffic routing works in VPN Director.

## Overview

VPN Director uses two independent routing mechanisms:

| Mechanism | Purpose | Priority | Fwmark bits |
|-----------|---------|----------|-------------|
| **Xray TPROXY** | Transparent proxy via Xray | Higher (pos 1) | bit 8 (0x100) |
| **Tunnel Director** | VPN tunnel routing (WG/OVPN) | Lower (pos 2+) | bits 16-23 (0x00ff0000) |

Both work in `mangle` table's `PREROUTING` chain but use separate fwmark bit fields, so they don't interfere with each other's marks.

## Packet Processing Order

```
Incoming packet from LAN (LAN interface)
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│                    mangle PREROUTING                         │
│                                                              │
│  pos 1: ──► XRAY_TPROXY chain                               │
│              │                                               │
│              ├─ src NOT in XRAY_CLIENTS? ──► RETURN         │
│              ├─ dst in TPROXY_BYPASS? ──► RETURN (bypass)   │
│              ├─ dst is private/local? ──► RETURN            │
│              ├─ dst in exclude_sets? ──► RETURN             │
│              │                                               │
│              └─ TPROXY redirect to Xray port                │
│                 + set mark 0x100                             │
│                 ══► Packet goes to Xray, exits PREROUTING   │
│                                                              │
│  pos N: ──► TUN_DIR chain (if mark field 0x00ff0000 == 0)   │
│              │                                               │
│              ├─ src + dst in exclude? ──► RETURN            │
│              ├─ src matched? ──► MARK 0xN0000               │
│              └─ first client match wins (linear order)      │
│                                                              │
└─────────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│                    ip rule lookup                            │
│                                                              │
│  pref 200:   fwmark 0x100/0x100 ──► table 100 (Xray local)  │
│  pref 16384: fwmark 0x10000/0xff0000 ──► table wgc1 / 2000  │
│  pref 16385: fwmark 0x20000/0xff0000 ──► table ovpnc1 / 2001│
│  ...                                                         │
│  pref 32767: default ──► table main (WAN)                   │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

On KeeneticOS 5.1.5 pref 200 was free of NDM policies (no tables in the 40s).
Built-in LTE backup stays at prefs 100/101, fwmark `0xffffaaa`, table 4096.
A user connection policy measured at prefs 102/103, table 4097, fwmark `0xffffaab`.
`_tproxy_setup_routing` still reconciles only rules carrying our mark or our
table, because other firmware versions may still park policies at 200.

## Priority: Xray vs Tunnel Director

**Xray has absolute priority** because:

1. XRAY_TPROXY jumps are inserted from position 1 in PREROUTING, one per LAN interface
   (`platform_lan_ifaces`; Merlin and Keenetic: `br0`)
2. TPROXY target **redirects** the packet to local Xray socket
3. Packet never continues to the TUN_DIR chain

TUN_DIR's insert position is `platform_prerouting_base_pos`: Merlin after the
firmware's iface-mark rules (or 1 when it has none); Keenetic **2**, ahead of
every `_NDM_*` jump. `tunnel_apply` then counts the `XRAY_TPROXY` jumps already
in PREROUTING and goes behind them when the base position would land among
them, so the order is `[XRAY_TPROXY, TUN_DIR]` on both platforms.

**Implication**: If a client IP is in both `xray.clients` and a TD rule, traffic goes through Xray only. TD rule is ignored for that client.

## The apply: make before break

A full apply (`apply`, `update`, `restart`) moves every client make-before-break:

1. `tproxy_apply` adds every effective Xray client to `XRAY_CLIENTS` - `xray.clients` less
   `paused_clients`, an entry that is no IPv4 address or CIDR skipped - and removes none.
2. `tunnel_apply` puts Tunnel Director's rules in place and reports the clients the live `TUN_DIR`
   does not carry when it returns (`TUNNEL_UNCARRIED`) - never a client of `main`, which goes direct
   unmarked as well.
3. `tproxy_prune` swaps in the effective Xray clients, plus every reported client `XRAY_CLIENTS`
   still holds.

Xray wins over TUN_DIR (above). A client moving from Xray to a tunnel stays proxied until step 3
lets it go, when TUN_DIR already marks it; a client moving the other way is proxied from step 1,
before TUN_DIR lets it go in step 2. A Tunnel Director failure that stops the apply (a tunnels value
that is no object, no LAN interface, no PREROUTING position) ends the run before step 3. One that
costs some clients returns 0 - a refused rule, a swap that did not take over every interface, a
tunnel the platform does not list, a slot whose route or ip rule is not in place, a client outside
RFC1918 - and step 2 reports the clients it cost. Step 3 keeps each of them that `XRAY_CLIENTS`
still holds: a client that came from Xray stays proxied rather than leak, until an apply in which
Tunnel Director carries it. A client that left Xray for direct - paused, deleted - is let go, and
one that was not proxied is not made so (`tunnel-director.md`, "What tunnel_apply reports as not
carried").

No step flushes a chain or a set a packet is crossing. `XRAY_TPROXY` and a rebuilt `TUN_DIR` are
built as `<chain>_NEW` and swapped in by `swap_fw_chain` (`lib/firewall.sh`): the new jump goes in
ahead of the old one, the old jump and chain go, the new chain takes the name. `TPROXY_BYPASS`
and, in the prune, `XRAY_CLIENTS` are built as `<set>_NEW` and swapped in (`ipset swap`). A Tunnel
Director rebuild keeps every tunnel's slot and installs the routing of a new slot before the swap
and releases a dropped one after it (`tunnel-director.md`). `restart` and `restart xray` stop
nothing; while the Xray process restarts, TPROXY drops what no socket takes.

Left open:

- The firmware flushing our chains (Merlin's `iptables -t mangle -F` on a firewall start, an NDM
  rebuild on KeeneticOS) leaves the clients on the WAN until the hook's apply.
- A tunnel that is down sends its clients to `main`. One that comes from Xray while the tunnel's
  route cannot be installed stays proxied instead (step 3).
- A client the kernel refuses to add to `XRAY_CLIENTS` (a WARN names it, and the ready marker is
  withheld) is not proxied until an add succeeds - the prune of the same apply tries again. One
  moving from a tunnel to Xray leaves through the WAN in between, once `tunnel_apply` has let it
  go: the cost a refused MARK rule has for a Tunnel Director client that did not come from Xray.
- A move changes the route of new connections only. An open connection breaks, or, on KeeneticOS,
  one the fast path already holds keeps its old path until its conntrack entry expires ("The
  firmware fast path" below); a UDP flow moved from Xray to a tunnel can stall until its entry
  expires. Nothing flushes conntrack: KeeneticOS has no conntrack-tools.
- A component command does not move a client between Xray and a tunnel. `apply xray` and
  `restart xray` run no Tunnel Director, so their prune keeps every client the config puts on a
  tunnel - `main`'s left out - that `XRAY_CLIENTS` still holds: a client moved from Xray to a tunnel
  by hand stays proxied until a full apply moves it, and only a client that left Xray for direct is
  let go.
  `apply tunnel` and `restart tunnel` move a client between tunnels in place, but add nothing to
  `XRAY_CLIENTS`: one moved from a tunnel to Xray leaves through the WAN until the next full apply.
  The daemons move clients with full applies; a server switch, which changes no client's route,
  runs `restart xray`.
- `S99vpn-director restart` is `stop`, then `start` (`vpn-director.sh stop`, then `apply`), with no
  routing in between: not the in-place `restart`.
- IPv6 is routed by neither module.

## Fwmark Bit Layout

```
31                        16 15         8 7              0
├─────────────────────────┼─────────────┼───────────────┤
│   Tunnel Director       │   Reserved  │  Xray + VPN   │
│   (8 bits: 0-255)       │             │  firmware     │
├─────────────────────────┼─────────────┼───────────────┤
│   0x00ff0000            │             │  0x100 = Xray │
│   mark = slot << 16     │             │  0x01 = VPN   │
└─────────────────────────┴─────────────┴───────────────┘

Examples:
  0x00000100 = Xray TPROXY (bit 8)
  0x00010000 = TD rule 0 (slot 1 << 16)
  0x00020000 = TD rule 1 (slot 2 << 16)
  0x00010100 = Both Xray and TD rule 0 (theoretically, but Xray wins)
```

## Xray TPROXY Details

### Chain Structure (tproxy.sh)

```
XRAY_TPROXY chain:
  1. ! --match-set XRAY_CLIENTS src → RETURN
  2. --match-set TPROXY_BYPASS dst → RETURN
  3. -d 127.0.0.0/8 → RETURN (loopback)
  4. -d 10.0.0.0/8 → RETURN (RFC1918)
  5. -d 172.16.0.0/12 → RETURN (RFC1918)
  6. -d 192.168.0.0/16 → RETURN (RFC1918)
  7. -d 169.254.0.0/16 → RETURN (link-local)
  8. -d 224.0.0.0/4 → RETURN (multicast)
  9. -d 255.255.255.255/32 → RETURN (broadcast)
  10. --match-set {exclude_set} dst → RETURN (for each exclude_set)
  11. -p tcp → TPROXY --on-port PORT --tproxy-mark 0x100/0x100
  12. -p udp → TPROXY --on-port PORT --tproxy-mark 0x100/0x100
```

### IPSets

| IPSet | Type | Purpose |
|-------|------|---------|
| `XRAY_CLIENTS` | hash:net | Source IPs to proxy |
| `TPROXY_BYPASS` | hash:net | Bypass set (servers + user excludes + OpenVPN endpoints) |
| `{country}` or `{country}_ext` | hash:net | Country exclusions |

### Routing

```bash
# Route table for TPROXY (local delivery)
ip route add local default dev lo table 100

# IP rule: marked packets use table 100
ip rule add pref 200 fwmark 0x100/0x100 table 100
```

On Keenetic, `platform_tproxy_extra_rules apply` also puts `-m mark --mark <fwmark> -j ACCEPT`
at position 1 of `mangle INPUT`, so TPROXY-marked HTTPS never reaches
`_NDM_HTTP_INPUT_TLS_`.

## Tunnel Director Details

### Chain Structure (tunnel.sh)

Single chain with rules for all clients:

```
TUN_DIR chain:
  # Client 1 (wgc1)
  -s 192.168.50.0/24 -m set --match-set <country> dst → RETURN
  -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 → PPE       (Keenetic only)
  -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 → MARK 0x10000

  # Client 2 (ovpnc1)
  -s 192.168.1.5 -m set --match-set <country> dst → RETURN
  -s 192.168.1.5 -m mark --mark 0x0/0xff0000 → PPE           (Keenetic only)
  -s 192.168.1.5 -m mark --mark 0x0/0xff0000 → MARK 0x20000
```

PREROUTING jump, one per LAN interface (`platform_lan_ifaces`; Merlin: `br0`):

```
-i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
```

The `--mark 0x0/0xff0000` condition ensures **first-match-wins**: once a packet is marked, subsequent rules skip it.

### The firmware fast path (`platform_tunnel_offload_target`)

KeeneticOS binds an established **forwarded** flow to a NAT/route fast path that runs before
`mangle` and never returns to it — the conntrack hook sits at priority -200, `mangle` at -150.
Once a flow is bound, its packets never reach `TUN_DIR`, so the `MARK` is applied to the first
few packets only and the rest miss `ip rule 16384` and leave through the WAN carrying the
tunnel's source address. Measured on a KN-4521: conntrack counted 24 packets of one flow while
`TUN_DIR` counted 6, and a 1 MB download stalled at exactly one TCP window (20610 bytes).

`platform_tunnel_offload_target` names the mangle target that opts a flow out of it — `PPE` on
Keenetic, which sets `ct->fast_ext` (a condition of both the `fastnat` and the `fastroute` entry
test) and `FOE_ALG_SKIP`, closing the hardware path too. Merlin has no such path and prints
nothing, so the rule does not exist there at all.

Placement carries the meaning: the exclusion `RETURN`s run first, so an excluded destination
keeps its acceleration, and the opt-out sits immediately before `MARK` with the identical match,
where the `--mark 0x0/0xff0000` test still holds.

Xray needs none of this — TPROXY terminates the connection in a local socket, so no forwarded
flow is left to accelerate. That is why Xray worked on KeeneticOS while Tunnel Director did not.

A flow already bound to the fast path stays bound until its conntrack entry expires; new flows
are correct immediately. And because `tunnel_apply` hashes the *configuration*, a router that
already has Tunnel Director applied with an unchanged config gets the rule on its next rebuild —
seconds away on Keenetic, where any NDM firewall rebuild wipes our chains and the `netfilter.d`
hook re-applies. `restart tunnel` forces it.

### Position Calculation

```bash
platform_prerouting_base_pos()  # Platform contract
                                # Merlin: the position after the firmware's iface-mark
                                #   rules, or 1 when it has none
                                # Keenetic: 2, ahead of every _NDM_* jump
```

Each LAN interface gets its own jump, at `base_pos`, `base_pos + 1`, … — one shared position would
make every interface displace the one before it, and each rewrite is a window with no jump.

`tunnel_apply` raises `base_pos` to one past the `XRAY_TPROXY` jumps already in PREROUTING when
it would otherwise land among them. On Merlin without firmware iface-mark rules the base
position is 1, the slot XRAY_TPROXY holds, and a rebuild used to put TUN_DIR ahead of it; the
next apply then found the Xray jump off its position and purged and re-inserted it — a window
with no TPROXY jump on every apply. A rebuild inserts its jumps to `TUN_DIR_NEW` at those
positions, ahead of the old jumps, which go after (`swap_fw_chain`).

Typically (one LAN interface):
- Position 1: XRAY_TPROXY
- Merlin: firmware iface-mark rules (if any), then TUN_DIR
- Keenetic: Position 2: TUN_DIR (ahead of every `_NDM_*` jump)

### IP Rules

```bash
# For each TD rule; the table is the one platform_tunnel_table names for the tunnel id
# (Merlin: the id from rt_tables; Keenetic: 2000+idx, route re-installed by
# platform_tunnel_route_ensure):
ip rule add pref {16384 + idx} fwmark {mark}/{mask} table {wgc1|ovpnc1|main|2000}
```

## Traffic Flow Examples

### Example 1: Client in Xray only

Config:
```json
{
  "xray": { "clients": ["192.168.50.10"], "exclude_sets": ["<country_code>"] }
}
```

Packet from 192.168.50.10 to 8.8.8.8 (foreign):
1. XRAY_TPROXY: src in XRAY_CLIENTS? Yes
2. dst in TPROXY_BYPASS? No
3. dst private? No
4. dst in excluded country? No
5. **TPROXY to Xray port, mark 0x100**
6. Packet delivered to Xray process

Packet from 192.168.50.10 to 203.0.113.1 (excluded country):
1. XRAY_TPROXY: src in XRAY_CLIENTS? Yes
2. dst in excluded country? Yes → **RETURN**
3. TUN_DIR: no rules for this client
4. **Goes to main table → WAN (direct)**

### Example 2: Client in Tunnel Director only

Config:
```json
{
  "tunnel_director": {
    "tunnels": {
      "ovpnc3": {
        "clients": ["192.168.1.5"],
        "exclude": ["<country_code>"]
      }
    }
  }
}
```

Packet from 192.168.1.5 to 8.8.8.8 (foreign):
1. XRAY_TPROXY: src in XRAY_CLIENTS? No → RETURN
2. TUN_DIR: src=192.168.1.5 + dst in excluded country? No
3. TUN_DIR: src=192.168.1.5? Yes → **MARK 0x10000**
4. ip rule pref 16384: fwmark 0x10000 → table ovpnc3
5. **Goes through OpenVPN tunnel**

Packet from 192.168.1.5 to 203.0.113.1 (excluded country):
1. XRAY_TPROXY: src in XRAY_CLIENTS? No → RETURN
2. TUN_DIR: src=192.168.1.5 + dst in excluded country? Yes → **RETURN** (no mark)
3. **Goes to main table → WAN (direct)**

### Example 3: Client in both Xray and TD

Config:
```json
{
  "xray": { "clients": ["192.168.50.0/24"], "exclude_sets": ["<country_code>"] },
  "tunnel_director": {
    "tunnels": {
      "wgc1": {
        "clients": ["192.168.50.10"],
        "exclude": ["<country_code>"]
      }
    }
  }
}
```

Packet from 192.168.50.10 to 8.8.8.8 (foreign):
1. XRAY_TPROXY: src in XRAY_CLIENTS (192.168.50.0/24)? Yes
2. dst in excluded country? No
3. **TPROXY to Xray** — TUN_DIR chain never evaluated

**Xray always wins** for overlapping clients.

## Where Routing Info is Stored

### Configuration (desired state)

| Location | Content |
|----------|---------|
| `vpn-director.json` | Rules, clients, exclusions |

### Kernel (applied state)

| Command | Shows |
|---------|-------|
| `ipset list -n` | All loaded ipsets |
| `ipset list {name}` | Entries in specific ipset |
| `iptables -t mangle -S XRAY_TPROXY` | Xray chain rules |
| `iptables -t mangle -S TUN_DIR` | TD chain rules |
| `iptables -t mangle -S PREROUTING` | Jump rules and positions |
| `ip rule show` | Fwmark-based routing rules |
| `ip route show table {N\|name}` | Routes in specific table |

### Temporary state

| File | Purpose |
|------|---------|
| `/tmp/tunnel_director/tun_dir_rules.sha256` | Hash of applied TD rules |
| `/tmp/tunnel_director/tun_dir_tables` | Applied tunnels, `<idx> <id>` per line (`TUN_DIR_TABLES`); a tunnel keeps its idx while it has clients; during a rebuild, the union of both layouts |

## Key Code Locations

| File | Function | Purpose |
|------|----------|---------|
| `lib/tproxy.sh` | `_tproxy_setup_iptables()` | Chain rules and PREROUTING jumps from pos 1 |
| `lib/platform/merlin.sh` | `platform_prerouting_base_pos()` | TD position after firmware iface-mark rules |
| `lib/platform/keenetic.sh` | `platform_prerouting_base_pos()` | prints `2`, ahead of every `_NDM_*` jump |
| `lib/platform/keenetic.sh` | `platform_tunnel_route_ensure()` | `ip route replace` into `2000+idx` |
| `lib/platform/keenetic.sh` | `platform_tproxy_extra_rules()` | `ACCEPT` at mangle INPUT pos 1 |
| `lib/tunnel.sh` | PREROUTING jump with mark check | First-match-wins |
| `lib/tunnel.sh` | ip rule creation | Fwmark → `platform_tunnel_table` |
| `lib/tunnel.sh` | `_tunnel_ensure_routes()` | Re-install tunnel routes on an apply with no rebuild |
| `lib/tunnel.sh` | `_tunnel_marks_present()` | Rebuild when a client's MARK rule is gone (Merlin's firewall start flushes every mangle chain) |
| `lib/platform/keenetic.sh` | `platform_tunnel_offload_target()` | `PPE` — takes a marked flow out of the firmware fast path |
