# Protection that survives the firmware's firewall rebuilds: design

Date: 2026-10-26
Branch: to be created
Status: brainstormed with the owner, awaiting review

## 1. The problem

Everything that keeps a protected client off the WAN lives in two mangle chains and their PREROUTING
jumps: `TUN_DIR` marks a Tunnel Director client's packets for its tunnel's table, and the kill switch
(`fwmark <mark>/<mask> prohibit`, `tunnel-director.md`, "The kill switch") refuses a marked packet
whose table has no route; `XRAY_TPROXY` hands an Xray client's packets to Xray. The firmware deletes
both chains on its own firewall rebuilds:

- KeeneticOS: NDM rebuilds `filter`, `mangle` and `nat` from scratch and deletes foreign chains
  (`keenetic.md`). Measured on the owner's KN-4521 (kernel 4.9-ndm-5) from 2026-10-25 15:45 to
  2026-10-26 12:15: every renewal of the WAN's DHCP lease rebuilt them - 40 of 40, one every 30
  minutes with the upstream's 3600 s lease - and so did each OpenVPN client reconnect (2, inactivity
  timeout) and the LTE modem's ping-check state change (1).
- Asuswrt-Merlin: every firewall start runs `iptables -t mangle -F` (`shell-conventions.md`).

`ip rule`, routing tables and ipsets survive both. Until the hook's `--wait apply` puts the chains
back - 1 to 4 s from the DHCP ACK to the client's MARK rule on the KN-4521 (second-resolution logs) -
a protected client's packets carry no mark. The kill switch never sees them: they reach `main` and
leave through the WAN with the router's own address. `packet-flow.md` lists this as the first point
of "Left open".

What that cost, measured:

| Effect | Evidence |
|--------|----------|
| New connections in the window leave through the WAN | Three CLIProxyAPI incidents (2026-10-18, -21, -25): OpenAI answered with its geo-block 403 naming the home address. On 25.10 the DHCP ACK came at 18:45:17, `TUN_DIR` was back at 18:45:19, the 403 at 18:45:20 |
| On KeeneticOS such a connection stays on the WAN for its whole life | Real renewal 26.10 12:15:27: a keep-alive connection opened at 12:15:29.8 made 30 of 30 requests over 29 s through the home address. `PPE` lives in `TUN_DIR`, so nothing kept the flow off the fast path, which took it after its first packets; it never met `TUN_DIR` again |
| Connections opened before the window are reset | Same renewal: a download through the tunnel got `Connection reset by peer`, keep-alive connections broke and reconnected inside the window - through the WAN. Their packets leave the WAN carrying the tunnel's NAT address, the upstream masquerades them, the server answers with RST, and the RST comes back to the client. CLIProxyAPI's EOF/408 before each 403 is this |

## 2. Goal

1. While a firmware rebuild has taken the chains away, no packet of a protected client leaves through
   the WAN with the router's address - except to the address and CIDR exclusions its configuration
   names, which go direct by design.
2. A Tunnel Director client keeps going through its tunnel during the window: its connections are not
   reset, and its new connections go through the tunnel.
3. An Xray client's new connections are refused during the window (Xray cannot be reached without the
   TPROXY rule); its proxied connections stall and resume once the chain is back.
4. The window itself gets as short as the hook can make it.
5. Normal operation - chains in place - routes every client and destination exactly as today (5.1).

## 3. Decisions

The owner made each of these in brainstorming.

| # | Decision | Why |
|---|----------|-----|
| 1 | The protection moves into what the firmware does not rebuild: per-client `ip rule`s by source address and LAN interface ("guards"), behind the existing fwmark rules. No setting turns them off | ip rules survive NDM's rebuilds and `mangle -F`; marks do not |
| 2 | In the window, a Tunnel Director client goes through its tunnel's table, and is refused when the table has no route | The owner's priority is no leak and no reset for tunnel clients |
| 3 | In the window, an Xray client is refused, except to private ranges and `xray.exclude_ips`, which go direct | Xray is unreachable without TPROXY; refusing is the only way not to leak |
| 4 | Country exclusions cannot be expressed in ip rules (no ipset match). In the window a Tunnel Director client's traffic to an excluded country goes through the tunnel, an Xray client's is refused. Address and CIDR exclusions stay direct (guards with `to <cidr>`) | The owner accepted that a Russian site sees the VPN's address for these seconds. Keeping countries direct would need their prefixes as routes - about 13 000 per tunnel for ru+by+kz, copies of the tunnel tables on Merlin - and was rejected as too many moving parts |
| 5 | On KeeneticOS every flow of a protected client is kept off the fast path (`PPE`): the ones to its tunnel and the ones to its exclusions alike, for Xray clients the ones Xray does not take. Those flows lose acceleration; every other device keeps it | Measured (section 4): the fast path routes by ip rules and does not see marks. An accelerated flow to an excluded destination follows the guard into the tunnel and dies |
| 6 | The window is shortened in the same project: on a rebuild the firmware hook restores the chains at once from a snapshot the last successful apply saved, then queues the full apply as today | The window is safe with guards, but still costs: Xray clients are refused, excluded traffic goes through the tunnel |
| 7 | Both platforms | The project is open source; Merlin has the same window |

## 4. What the measurements established

The design rests on two experiments on the owner's KN-4521 (2026-10-26). Each touched only the traffic
of one LAN client (`192.168.1.10`, on `OpenVPN1`) to one or two test addresses, and removed everything
afterwards. A packet to a test address captured on the upstream router's LAN port (`tcpdump` on
`mitino`, the device the Keenetic's WAN plugs into) went around the tunnel.

**Spike 1 - guards keep tunnel clients in the tunnel, the fast path included.** A `RETURN` ahead of
the `TUN_DIR` jump for two Cloudflare addresses imitated the window for 10 s. Probes from the client:
a new connection every 0.25 s, a keep-alive connection started every second (20 requests at 1/s), and
a slow download running through the window.

| | Without guards (control) | With `from <client> to <addr> iif br0 lookup 2000`, then `prohibit` |
|---|---|---|
| New connections in the window | 25 left through the home address | 73 of 73 through the VPN |
| Keep-alive connections opened in the window | all 10 through the home address, 10-20 s past the window | all through the VPN, none broke |
| Keep-alive connections opened before it | 4 of 4 broke, reconnected through the WAN | none broke |
| Slow download | stalled: 457 KB in 50 s | full rate: 2 MB in 50 s |
| Packets around the tunnel on the upstream | 3179, 1591 of them after the window | 0 |

**Spike 2 - the fast path ignores marks.** A slow download from `213.180.204.183` (`mirror.yandex.ru`,
in the `ru` set, so excluded) with a stand-in for the direct mark: `MARK 0x200` ahead of `TUN_DIR`,
`from <client> to <addr> fwmark 0x200/0x200 lookup main` at 16898, and the guard pair at 16899/16901.

| Round | Download | Direct packets out on the upstream |
|---|---|---|
| Nothing added | 100 KB/s, 2 MB in 20 s | 311 |
| Direct mark + guards, no `PPE` | 0 bytes, timed out | 6 - the handshake |
| The same with `PPE` | 100 KB/s, 4 MB in 40 s | 656 |

So: a flow the fast path takes over is routed by `ip rule` with its source, destination and incoming
interface, never by a mark - the fwmark rule stopped applying after the first six packets. That is why
guards work for the window's flows (spike 1), why the old bug sent accelerated tunnel flows to the WAN
(`packet-flow.md`, "The firmware fast path"), and why decision 5 is needed.

## 5. Design

### 5.1 Outcomes

Normal operation (chains in place) must not change:

| Client | Destination | Route |
|--------|-------------|-------|
| Tunnel Director, on a tunnel | not excluded | the tunnel; refused when its table has no route |
| Tunnel Director, on a tunnel | excluded: country, address, CIDR | direct |
| Tunnel Director, on `main` | anything | direct; first match in JSON order still carves it out of a later tunnel's network |
| Xray | TCP and UDP Xray does not exclude | Xray |
| Xray | what Xray excludes (bypass set, private ranges, `exclude_sets`), and protocols TPROXY does not take (ICMP, ...) | on to `TUN_DIR` as today: a Tunnel Director client goes where `TUN_DIR` sends it, any other client direct |
| Any other LAN device | anything | direct, accelerated |

In the window (our chains gone; ip rules, tables and ipsets in place):

| Client | Destination | Route |
|--------|-------------|-------|
| Tunnel Director, on a tunnel | anything but its address/CIDR exclusions | its tunnel; refused when the table has no route |
| Tunnel Director, on a tunnel | its address/CIDR exclusions | direct |
| Xray only | private ranges, `xray.exclude_ips` | direct |
| Xray only | anything else | refused |
| Xray and a tunnel | anything | as a Tunnel Director client: Tunnel Director's guards come first |
| Tunnel Director, on `main` | anything | direct, unless a guarded entry of another client covers it (then that entry's route: tunnel or refused) |

A flow opened in the window is not kept off the fast path (`PPE` is gone with the chain), so on
KeeneticOS it keeps the window's route for its whole life: an excluded-country flow opened then stays
in the tunnel until it closes. The owner's acceptance of decision 4 covers that.

### 5.2 Guards

A guard is an `ip rule` with `from <client>` and `iif <lan>` - one rule per LAN interface
(`platform_lan_ifaces`), since `iif` takes one name. `iif` keeps the router's own traffic out: a client
CIDR may hold the router's LAN address. The local table (pref 0) answers before any guard, so traffic
to the router itself is untouched.

Tunnel Director guards, for exactly the clients `TUN_DIR` emits rules for (RFC1918, valid IPv4), in
`TUN_DIR`'s order (failover clients first, then JSON order), so the first match is the same one:

```
from <C> to <address or CIDR exclusion> iif br0 lookup main     # one per such exclusion of C's tunnel
from <C> iif br0 lookup <C's tunnel table>                       # vpd_tunnel_table; none for a tunnel with no table (_tunnel_routeless)
from <C> iif br0 prohibit
```

A client of `main` gets no guard: it goes direct anyway, and a guard sending it to `main` would let it
past the guard of an Xray client entry that covers it (my call; the cost is that in the window a `main`
carve-out from a tunnel's network goes through that tunnel - safe, not a leak).

Xray guards, for what `XRAY_CLIENTS` holds after the prune - the effective clients, plus the ones the
prune keeps (`tproxy_prune`):

```
from <X> to 10.0.0.0/8 iif br0 lookup main        # likewise 172.16.0.0/12, 192.168.0.0/16,
                                                  # 169.254.0.0/16, 224.0.0.0/4 (my call)
from <X> to <xray.exclude_ips entry> iif br0 lookup main
from <X> iif br0 prohibit
```

The Xray servers and the firmware's VPN endpoints (`TPROXY_BYPASS`'s other sources) get no guard: an
Xray client talking to them directly is rare, and refusing it for the window costs little (my call).

Ordering of the whole rule list: the fwmark rules (Xray at 200, Tunnel Director's lookup and block
ranges) and the direct rule (5.3) first, then every Tunnel Director guard, then every Xray guard, all
below `main`'s 32766. A packet that a chain marked never reaches a guard; in normal operation only an
unmarked packet of a protected client does - one whose MARK or `PPE` rule the kernel refused, which a
guard now sends to its tunnel instead of `main`.

Every routed packet of every device walks the rules until one matches, so the guard count matters:
it grows with clients times address exclusions, not with countries.

### 5.3 The direct mark

Today an exclusion is a bare `RETURN`: the packet leaves the chain unmarked and reaches `main`. With
guards an unmarked packet of a protected client goes to its guard, so "direct" needs a mark of its
own: one bit outside Tunnel Director's field and Xray's bit (suggested default `0x200`), and one rule
`fwmark <direct>/<direct> lookup main` after Tunnel Director's block range and before every guard.

- `XRAY_TPROXY` sets the bit on every packet of an Xray client, right after rule 1 (`! XRAY_CLIENTS
  src -> RETURN`). A packet TPROXY takes also gets `0x100` and meets pref 200 first; one Xray excludes
  returns with the bit; one of a protocol TPROXY does not take falls off the end of the chain with it
  and goes direct, as today.
- `TUN_DIR`'s tunnel MARK still tests only its own field, so a Tunnel Director client's packet that Xray
  excluded is marked for the tunnel as today, and the tunnel's lookup rule comes before the direct
  rule.
- Each exclusion of a Tunnel Director client sets the bit and returns. A rule that returned whenever
  the bit is set would also return what Xray excluded and keep it from the tunnel, so each exclusion
  sets and returns with its own match (or an equivalent the builder prefers).

The bit must not collide with marks the platforms use. On KeeneticOS, NDM's policy marks
(`0xffffaaa`, ...) have bit 9 set, but their own rules at prefs 100-105 route or blackhole them first.
Merlin's marks are unverified.

A packet carrying the bit no longer meets NDM's DNS-based routing chain (`-m mark --mark 0x0 -j
_NDM_DNSRT_PREROUTING_MANGLE`), which a bare `RETURN` reached: a protected client's excluded traffic
no longer follows Keenetic's domain routes. Marked tunnel traffic never did. On the KN-4521 that
jump is the only rule in mangle that tests for a zero mark (`iptables-save -t mangle`, 2026-10-26);
other firmware versions may have more, which acceptance looks for.

### 5.4 Chain changes

- `TUN_DIR`, per client: the offload opt-out (`platform_tunnel_offload_target`) moves to the head of
  the client's block, ahead of its exclusions, with the same `-s <C> -m mark --mark 0x0/<mask>` match,
  so every flow of the client - `main`'s clients included - stays off the fast path. Then the
  exclusions (direct bit, return), then the MARK.
- `XRAY_TPROXY`: after rule 1, the direct bit (5.3) and, where the platform names one, the offload
  target. A proxied flow terminates in a local socket and is never accelerated; the opt-out is for the
  flows Xray lets through.
- The contract text of `platform_tunnel_offload_target` (`lib/platform.sh`: "placed immediately before
  that rule, so a destination the exclusions return on keeps the platform's acceleration") changes with
  it.

### 5.5 Lifecycle

Make before break holds for guards as for everything else (`packet-flow.md`, "The apply: make before
break"): a protected client never loses its guard while it is protected.

- `tunnel_apply`: a rebuild installs the guards of the new layout in its routing step, before the
  chain swap, and removes the guards of clients the layout dropped after it, with the slots it releases.
  The up-to-date branch puts back a guard that is missing and leaves one that is there alone, as
  `_tunnel_rule_ensure` does for lookup rules.
- `tproxy_apply` adds the guards of every effective client and removes none; `tproxy_prune` removes the
  guards of clients the set lost, after the swap.
- The direct rule exists while either module has guards. `tunnel_stop` and `tproxy_stop` remove their
  own guards, and the direct rule goes once neither has any.
- A guard that the kernel refuses is logged as an ERROR. A rebuild with one missing records no hash, so
  the next apply retries it. It does not count as uncarried (`TUNNEL_UNCARRIED`): in normal operation
  the client is carried.
- `failover_ready` additionally needs the guards of the failover clients on the failover tunnel, and the
  TPROXY ready marker the guards of every effective Xray client: the watch moves clients on those
  markers, and a client moved without its guard leaks in the next window.
- The first apply after the upgrade rebuilds `TUN_DIR` in place although the configuration hash is
  the same: the old chain's bare `RETURN`s would send excluded traffic to the guards - into the tunnel -
  in normal operation, and the up-to-date path looks for MARK rules only. A format version in the hash
  input does it (my call).
- Guards are reconciled by their full selector - preference, `from`, `to`, `iif`, action - read from
  the whole `ip rule show` listing (`_tunnel_ip_rule_listed`; the pipefail pitfall in
  `shell-conventions.md`).

### 5.6 Shortening the window

- Every successful apply (full, component, `update`, `restart`) writes a snapshot of our rules as they
  stand after it, per table, as `iptables-restore --noflush` input: in mangle the `XRAY_TPROXY` and `TUN_DIR` chains with
  their rules and PREROUTING jumps, Keenetic's mangle INPUT accept (`platform_tproxy_extra_rules`) and
  the interface tunnels' MSS rules in mangle FORWARD; in nat the interface tunnels' `MASQUERADE`. It
  lives in `/tmp` and goes with `stop`. An apply that fails part-way leaves the previous snapshot.
- The hooks restore before they queue the apply: KeeneticOS's `netfilter.d` hook for the `$table` it was
  called for, Merlin's `firewall-start` for mangle and `nat-start` for nat. A restore happens only when
  a snapshot exists and the table lacks our rules - a hook for a table the rebuild did not empty must
  not duplicate a jump.
- The restore is fast and small: no `common.sh`, no `jq`, no config. NDM runs hooks one after another
  under a 24-second timeout, and every millisecond is part of the window. The full `--wait apply`
  follows, detached as today, and reconciles everything the snapshot may have had stale.
- iptables-restore with `--noflush` flushes and refills a user chain it declares that already exists in
  the same commit, which is what Merlin's emptied chains need. PREROUTING jumps go in as inserts at
  their positions: on Merlin `TUN_DIR`'s position depends on the firmware's iface-mark rules
  (`platform_prerouting_base_pos`) and is computed at restore time.
- A restore drops Tunnel Director's hash (`tun_dir_rules.sha256`), so the apply that follows is a
  rebuild: the up-to-date path looks for MARK rules only, and anything stale the snapshot brought back
  would otherwise stay (my call). `tproxy_apply` rebuilds its chain on every apply anyway.
- A restore racing a running apply must converge after the queued apply, and must never leave two
  jumps to one chain behind for good.
- Entware's iptables 1.4.21 on the KN-4521 ships `iptables-restore` with `--noflush` and `--table`
  (checked 2026-10-26); Merlin's firmware has its own.
- The guards and ipsets survive the rebuild, so a restored chain works at once.

### 5.7 Configuration

- One new setting for the direct bit (my call: `advanced.tunnel_director.direct_mark`, default
  `0x200`), validated against Xray's mark and Tunnel Director's mask. The template gets it; a
  configuration without it takes the default, since existing installs have no such key.
- The guard ranges are derived from existing settings or get their own; either way they are validated
  like the block range today (`tunnel_apply` refuses a layout whose block rules would reach 32766): no
  overlap with the lookup and block ranges, Tunnel Director's guards before Xray's, everything below
  32766, an ERROR before touching a rule otherwise.
- No setting turns the guards or the opt-out off.

### 5.8 Status

`status tunnel` and `status xray` list the guards and the direct rule beside the fwmark rules
(`tunnel_status` greps `fwmark.*(lookup|prohibit)` today).

## 6. Non-goals

- Preventing the firmware's rebuilds. The owner's own router got a 24-hour lease on 2026-10-26
  (`mitino-server`), which cut its rebuilds from 48 a day to 2.
- Keeping country exclusions direct in the window, or keeping the fast path for protected clients'
  excluded traffic (decision 4).
- IPv6: neither module routes it.
- A change to the Go daemons: the files they read (`tun_dir_tables`, `failover_ready`, the TPROXY ready
  marker) keep their meaning.
- `S99vpn-director restart` (stop, then start) and `vpn-director.sh stop`: a stop still takes the
  protection away, as the owner asks for it.

## 7. Anti-goals

- Any change to normal operation's routes (5.1).
- A moment in which a protected client leaves through the WAN with the router's address - in a
  firmware rebuild, an apply, a guard update, a restore - other than to its address and CIDR
  exclusions.
- Another LAN device losing its acceleration.
- A hook that blocks NDM or runs into its 24-second timeout.
- Traffic to the router itself (Web UI, SSH, DNS) caught by a guard.

## 8. Constraints

- Shell conventions of the project (`shell-conventions.md`): bash 4 on the router via the shebang
  hand-over, BusyBox `sh` with no `command` builtin in Merlin's hooks, iptables 1.4.21 and iproute2 4.4
  (Entware `ip-full`, which refuses `ip rule show pref N`) on KeeneticOS, no jq regex functions.
- Core modules never test the platform name; whatever differs goes behind `platform_*`.
- Tests set `VPD_PLATFORM=merlin` through `test_helper.bash`; Keenetic tests export
  `VPD_PLATFORM=keenetic`.

## 9. Left to the builder

- The exact rule encoding of the direct bit in both chains, within 5.3.
- The preference ranges of the guards and their bookkeeping (one preference per rule, two halves for
  make before break, or another scheme), within 5.5 and 5.7.
- Where the direct rule's ownership lives, since both modules need it.
- The snapshot format and file layout, how the restore finds its positions, and whether it takes the
  lock (`flock -n` and skip, or not), within 5.6.
- A safe way to trigger a real firewall rebuild on each platform for acceptance (KeeneticOS: a WAN
  lease renewal or another NDM change that rebuilds netfilter; Merlin: `service restart_firewall`).

## 10. Tests and acceptance

- Bats (`testing.md`): the `ip` mock learns `from`, `to` and `iif` selectors. Guards created, ordered,
  ensured on the up-to-date path, released with a slot, removed by `stop`; their make before break;
  `PPE` at the head of every client's block; the direct bit in both chains; the readiness markers; the
  snapshot written and restored by each hook, on both platforms; no restore when the table still holds
  our rules, and none after `stop`.
- Acceptance on a real KeeneticOS router, repeating section 4 with the implementation and a real
  rebuild: no packet of the protected client to the test addresses around the tunnel; keep-alive
  connections and a download through the window survive; an Xray client is refused in the window, not
  leaked; a flow to an excluded address goes direct at full rate in normal operation; the window's
  length with the restore, measured.
- The same on Asuswrt-Merlin if a router is at hand: whether its acceleration (Broadcom flow cache,
  Runner) routes by ip rules as KeeneticOS's fast path does is unknown, and so is the direct bit there.

## 11. Documentation to update

`packet-flow.md` (rule list, the first "Left open" point, the mark layout, the fast path), 
`tunnel-director.md` (the kill switch, the chain, state tracking), `xray-tproxy.md` (chain structure,
fail-safe, ready marker), `keenetic.md` (what triggers NDM's rebuilds, how the fast path routes, the
hooks), `lib/platform.sh`'s contract text, `CLAUDE.md`, and the user-facing READMEs: on KeeneticOS a
protected client's excluded traffic is not accelerated; in a rebuild an Xray client is refused for a
moment and a tunnel client's excluded-country traffic goes through the tunnel.
