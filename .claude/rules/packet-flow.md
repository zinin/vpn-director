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
│              ├─ all ──► MARK 0x200 (direct), PPE (Keenetic) │
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
│              ├─ src matched? ──► PPE (Keenetic)             │
│              ├─ src + dst excluded? ──► MARK 0x200, RETURN  │
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
│  pref 16639+: same marks ──► prohibit (no route in table)   │
│  pref 16894: fwmark 0x200/0x200 ──► table main (direct)     │
│  pref 16895-16896: Tunnel Director guards, from <client>    │
│    iif br0: to an address/CIDR exclusion ──► table main,    │
│    else ──► its tunnel's table, then ──► prohibit           │
│  pref 16897-16898: Xray guards, from <client> iif br0:      │
│    to private ranges, exclude_ips ──► table main,           │
│    else ──► prohibit                                        │
│  ...                                                         │
│  pref 32766: from all ──► table main (WAN)                  │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

A packet a chain marked - for Xray, for a tunnel, or direct - meets its fwmark rule before any
guard, so in normal operation only an unmarked packet of a protected client reaches one. The guards
exist for the moment a firmware rebuild of the firewall has taken the chains away ("Left open"
below): ip rules by source and LAN interface, which the rebuild leaves alone, one per interface
(`iif` takes one name). The local table at preference 0 answers traffic to the router before any
of them. Tunnel Director's come first, so a client on Xray and on a tunnel goes through its tunnel
meanwhile. The direct rule and the guards follow the block range and move with `pref_base`
(`lib/guard.sh`); an owner's list sits at one of its two preferences while a changed one goes in at
the other (`guard_sync`). The rules in full: `tunnel-director.md`, "The kill switch", and
`xray-tproxy.md`, "Fail-Safe".

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
that is no object, a tunnel name that is empty or holds whitespace or a control character, block
rules that would reach main's rule at 32766, a direct mark or guard preferences
`guard_layout_check` refuses, no LAN interface, no PREROUTING position) ends the run
before step 3; `tproxy_apply` refuses such a direct mark or such guard preferences first, before
step 1 touches anything. One that
costs some clients returns 0 - a refused rule, a swap that did not take over every interface, a
tunnel the platform does not list, a slot whose route, lookup rule or block rule is not in place, a client outside
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

- A firewall rebuild of the firmware (Merlin's `iptables -t mangle -F` on a firewall start, an NDM
  rebuild on KeeneticOS, which every renewal of the WAN's DHCP lease brings: `keenetic.md`) takes
  our chains away until the hook puts them back, and the guards route the protected clients
  meanwhile ("Packet Processing Order"). A Tunnel Director client stays in its tunnel, or is
  refused while the tunnel has no route, and its connections survive. An Xray client's new
  connections are refused and its proxied ones stall, but for the private ranges and
  `xray.exclude_ips`, which go direct. A country exclusion is no ip rule, so a tunnel client's
  excluded-country traffic goes through its tunnel, and an Xray client's is refused. On KeeneticOS
  a flow opened then keeps that route for its whole life: `PPE` went with the chain ("The firmware
  fast path"). The hook shortens the window, which the apply alone held open for 1 to 4 s on the
  KN-4521 (from the DHCP ACK to a client's MARK rule): before it queues the apply, it restores the
  chains from the snapshot the last apply left, in one `iptables-restore`
  (`lib/fwsnap-restore.sh`; `tunnel-director.md`, "State Tracking").
- A tunnel that is down refuses its clients' traffic (the block rule after its lookup rule);
  excluded destinations still go direct. One that comes from Xray while the tunnel's route cannot
  be installed stays proxied instead (step 3).
- Xray that cannot intercept at all - no `xt_TPROXY`, so `tproxy_apply` sets up no rule and
  withholds the ready marker - carries no client: one moved from a tunnel to Xray leaves through
  the WAN once `tunnel_apply` lets it go, and nothing asks first. It is the router's state, not a
  window of the apply: while `XRAY_TPROXY` holds TPROXY targets the kernel keeps the module
  loaded, and an exclusion set that cannot be built ends the run before `tproxy_apply`
  (`_ensure_ipsets`).
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
31       24 23               16 15             8 7             0
├──────────┼───────────────────┼────────────────┼───────────────┤
│          │ Tunnel Director   │ Direct + Xray  │ VPN firmware  │
│          │ (8 bits: 0-255)   │                │               │
├──────────┼───────────────────┼────────────────┼───────────────┤
│          │ 0x00ff0000        │ 0x200 = direct │ 0x01 = VPN    │
│          │ mark = slot << 16 │ 0x100 = Xray   │               │
└──────────┴───────────────────┴────────────────┴───────────────┘

Examples:
  0x00000100 = Xray TPROXY (bit 8)
  0x00000200 = direct (bit 9): what a chain lets go direct; the rule at 16894 sends it to main
  0x00010000 = TD rule 0 (slot 1 << 16)
  0x00020000 = TD rule 1 (slot 2 << 16)
  0x00010100 = Both Xray and TD rule 0 (theoretically, but Xray wins)
  0x00010200 = TD rule 0 and direct: what Xray lets through of a client on a tunnel too;
               the lookup rule at 16384 comes before the direct rule
```

The direct mark is `advanced.tunnel_director.direct_mark`; `guard_layout_check` refuses a value
that is not one bit or lies in `mark_mask` or in Xray's `fwmark_mask`. NDM's policy marks on
KeeneticOS have bit 9 set as well, and NDM's own rules at prefs 100-105 route them first
(`keenetic.md`); Merlin's marks are unverified.

## Xray TPROXY Details

### Chain Structure (tproxy.sh)

```
XRAY_TPROXY chain:
  1. ! --match-set XRAY_CLIENTS src → RETURN
  2. → MARK 0x200/0x200 (direct; every packet past rule 1)
  3. → PPE (Keenetic only)
  4. --match-set TPROXY_BYPASS dst → RETURN
  5. -d 127.0.0.0/8 → RETURN (loopback)
  6. -d 10.0.0.0/8 → RETURN (RFC1918)
  7. -d 172.16.0.0/12 → RETURN (RFC1918)
  8. -d 192.168.0.0/16 → RETURN (RFC1918)
  9. -d 169.254.0.0/16 → RETURN (link-local)
  10. -d 224.0.0.0/4 → RETURN (multicast)
  11. -d 255.255.255.255/32 → RETURN (broadcast)
  12. --match-set {exclude_set} dst → RETURN (for each exclude_set)
  13. -p tcp → TPROXY --on-port PORT --tproxy-mark 0x100/0x100
  14. -p udp → TPROXY --on-port PORT --tproxy-mark 0x100/0x100
```

What the RETURNs let go, and what falls off the end (a protocol TPROXY does not take), leaves
with the direct mark, which the direct rule (`fwmark 0x200/0x200 lookup main`, `lib/guard.sh`)
sends to main; a packet TPROXY takes meets pref 200 first (`xray-tproxy.md`, "Chain Structure").

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
  -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 → PPE                    (Keenetic only)
  -s 192.168.50.0/24 -m set --match-set <country> dst → MARK 0x200/0x200  (direct)
  -s 192.168.50.0/24 -m set --match-set <country> dst → RETURN
  -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 → MARK 0x10000

  # Client 2 (ovpnc1)
  -s 192.168.1.5 -m mark --mark 0x0/0xff0000 → PPE                        (Keenetic only)
  -s 192.168.1.5 -m set --match-set <country> dst → MARK 0x200/0x200      (direct)
  -s 192.168.1.5 -m set --match-set <country> dst → RETURN
  -s 192.168.1.5 -d 203.0.113.7 → MARK 0x200/0x200                        (an address in exclude)
  -s 192.168.1.5 -d 203.0.113.7 → RETURN
  -s 192.168.1.5 -m mark --mark 0x0/0xff0000 → MARK 0x20000
```

Each exclusion sets the direct mark and returns, both with its own match; the direct rule
(`fwmark 0x200/0x200 lookup main`, `lib/guard.sh`) sends what carries the mark to main
(`tunnel-director.md`, "Chain Architecture").

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

The fast path routes a flow it took over by the ip rules alone, with the flow's source,
destination and incoming interface - never by a mark. Measured on the KN-4521 (`keenetic.md`,
"Fast path"): with a stand-in for the direct mark, its rule and the guards behind them but no
`PPE`, a download from an excluded address timed out with 0 bytes - the six packets of its
handshake went direct, then the fast path took the flow and the guard sent it into the tunnel;
with `PPE` it ran at full rate. That is why the guards hold even the flows the fast path took
while the chains were gone, and why every flow of a protected client is kept off the fast path.

Placement carries the meaning: the opt-out opens each client's block with the `MARK` rule's
match, ahead of the exclusions, so no flow of a Tunnel Director client is accelerated - one to an
excluded destination included, which its guard would take into the tunnel. Every other device
keeps its acceleration.

A flow TPROXY takes needs none of this — it terminates in a local socket, so no forwarded flow is
left to accelerate. That is why Xray worked on KeeneticOS while Tunnel Director did not. What
`XRAY_TPROXY` lets through is forwarded, though: the opt-out follows its direct mark, right after
rule 1, so no flow of an Xray client is accelerated either.

A flow already bound to the fast path stays bound until its conntrack entry expires; new flows
are correct immediately. A firewall rebuild takes `PPE` away with the chains: a flow opened before
they are back can be bound, and keeps the route its guard gave it ("Left open"). A router
upgraded with its configuration unchanged gets the rules of a new placement on the first apply
after the upgrade: the hash input carries the format of the rules (`_tunnel_format`).
`restart tunnel` forces a rebuild at any time.

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
# For each TD rule; the table is the one vpd_tunnel_table names for the tunnel id
# (an interface tunnel: 2000+idx; otherwise platform_tunnel_table's: Merlin the id from
# rt_tables, Keenetic 2000+idx), its route re-installed by vpd_tunnel_route_ensure:
ip rule add pref {16384 + idx} fwmark {mark}/{mask} table {wgc1|ovpnc1|main|2000}
```

```bash
# Right after the lookup range, for every slot but main's (the kill switch):
ip rule add pref {16384 + 255 + idx} fwmark {mark}/{mask} prohibit
```

```bash
# Right after the block range (lib/guard.sh): the direct rule, then the guards of each client of
# every slot but main's, per LAN interface, at 16895 or 16896 (tunnel-director.md, "The kill
# switch"); Xray's follow at 16897 or 16898 (xray-tproxy.md, "Fail-Safe"):
ip rule add pref {16384 + 2 × 255} fwmark 0x200/0x200 lookup main
ip rule add pref {16895|16896} from {client} to {address or CIDR exclusion} iif br0 lookup main
ip rule add pref {16895|16896} from {client} iif br0 lookup {table}
ip rule add pref {16895|16896} from {client} iif br0 prohibit
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
2. dst in excluded country? Yes → **RETURN** with the direct mark 0x200
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
2. TUN_DIR: src=192.168.1.5 + dst in excluded country? Yes → **RETURN** with the direct mark 0x200
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
| `ip rule show` | Fwmark rules, the direct rule and the guards |
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
| `lib/tunnel.sh` | ip rule creation | Fwmark → `vpd_tunnel_table` |
| `lib/tunnel.sh` | `_tunnel_ensure_routes()` | Re-install tunnel routes on an apply with no rebuild |
| `lib/tunnel.sh` | `_tunnel_marks_present()` | Rebuild when a client's MARK rule is gone (Merlin's firewall start flushes every mangle chain) |
| `lib/platform/keenetic.sh` | `platform_tunnel_offload_target()` | `PPE` — takes every flow of a protected client out of the firmware fast path |
| `lib/tunnel.sh` | `_tunnel_block_ensure()` | The slot's `prohibit` rule after its lookup rule |
| `lib/guard.sh` | `guard_direct_ensure()`, `guard_sync()` | The direct rule; an owner's guards, the new list in before the old one goes |
| `lib/fwsnap.sh` | `fwsnap_save()` | The snapshot of our rules each apply leaves |
| `lib/fwsnap-restore.sh` | `restore_mangle()`, `restore_nat()` | The firmware hooks put the snapshot back before they queue the apply |
| `lib/iftunnel.sh` | `iftunnel_route_ensure()`, `vpd_*` | Interface tunnels; the layer the core calls for any tunnel |
