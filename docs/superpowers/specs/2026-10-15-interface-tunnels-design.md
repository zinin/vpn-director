# Interface tunnels and the tunnel kill switch: design

Date: 2026-10-15
Branch: `feature/interface-tunnels`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Tunnel Director routes LAN clients only through the tunnels the firmware has: `wgcN` and `ovpncN`
on Asuswrt-Merlin, `OpenVPNN` and `WireguardN` on KeeneticOS. This work lets it route through any
Linux interface the router has, starting with the AmneziaWG interface `awg1` on the owner's
RT-AX86U, and closes the leak a dead tunnel opens today.

1. The configuration names the interfaces Tunnel Director may route through. Such an interface is a
   tunnel like any other: it gets a slot, a table, NAT and MSS clamping, and the Web UI, the bot and
   watchd list it.
2. A client on a tunnel never leaves through the WAN because the tunnel carries nothing - not while
   the tunnel is down, not while its interface is re-created, not while the platform does not know
   the tunnel. Its traffic to the tunnel is refused instead.
3. A tunnel's `exclude` takes IPv4 addresses and CIDRs beside country codes.

Why `awg1`: measured from the router on 2026-10-15 (median of three rounds,
`proof.ovh.net/files/100Mb.dat`), `awg1` carried 109 Mbit/s, `tun12` (OpenVPN through the nginx
relay on zinin) 35 Mbit/s, the bare WAN 112 Mbit/s (`wireguard-network`, `docs/nodes/asus.md`,
«Замеры»).

## 2. Decisions

The owner made each of these in brainstorming.

| # | Decision | Why |
|---|----------|-----|
| 1 | Every Tunnel Director tunnel refuses its clients' traffic when its table has no route. No setting | The router may have no OpenVPN to fall back on, and a tunnel that falls through to `main` leaks |
| 2 | A configured tunnel the platform does not know refuses its clients' traffic too | The same rule. It also closes the case where a silent RCI empties KeeneticOS's whole tunnel list |
| 3 | The configuration lists the interfaces: `tunnel_director.interfaces` | Works for any interface on both platforms, and a listed interface stays a tunnel while it is re-created |
| 4 | vpn-director owns NAT and MSS for the listed interfaces | A tunnel without NAT carries nothing, and vpn-director already re-applies after the firmware's rebuilds |
| 5 | A cron job puts back, within a minute, the route an interface's re-creation took. The interface's init script may call vpn-director at once | The guarantee must not depend on another project's script; the call shortens the minute to seconds |
| 6 | Address and CIDR exclusions go into the existing `exclude` | No schema change in Go, and the ipset builder already ignores entries that are no country code |
| 7 | A module of its own, `lib/iftunnel.sh`, beside the platform contract, and a `vpd_*` layer the core calls | One implementation for both platforms; the platform contract keeps describing the firmware only |

## 3. Configuration and the tunnel list

```json
"tunnel_director": {
  "interfaces": ["awg1"],
  "tunnels": {
    "awg1": { "clients": ["192.168.1.3", "192.168.1.30"], "exclude": ["ru", "by", "kz", "203.0.113.10"] }
  }
}
```

- `tunnel_director.interfaces` lists Linux interface names. A tunnel key equal to a listed name is an
  interface tunnel. An absent list is an empty one.
- `config.sh` loads the list into `TUN_DIR_INTERFACES` as written. `lib/iftunnel.sh` validates it
  once per run and logs one WARN for each name it rejects:
  - a name outside 1-15 characters of `A-Za-z0-9_.-`, or `.` or `..`;
  - `main`, or an id `platform_tunnels` lists;
  - a LAN interface (`platform_lan_ifaces`), or the WAN (`platform_wan_if`; this check is skipped
    when the platform cannot answer).

  A rejected name is no tunnel: a tunnel key that names it is unknown, and its clients are refused
  (section 5). A name listed twice counts once.
- The Tunnel Director hash covers the list as written when the list is not empty, so a change of
  the list rebuilds; the validated list would change with the platform's answers. An empty list
  leaves the hash as it is today: the upgrade rebuilds nothing on a router without interfaces.
- `vpn-director.sh platform` lists each valid interface after the firmware's tunnels:
  `{"id": "awg1", "iface": "awg1", "type": "interface", "connected": <bool>, "description": ""}`.
  `connected` is true while the interface exists and is UP. A missing interface keeps its entry,
  with `connected: false`.
- Go: `TunnelDirectorConfig` gains `Interfaces []string` (`json:"interfaces,omitempty"`).
  `SaveVPNDirectorConfig` marshals the struct, so without the field the first save by a daemon - a
  client moved in the Web UI, a failover commit - would drop the list. The comment on
  `PlatformTunnel.Type` names the new value `interface`.
- Nothing else changes for the daemons. The Web UI, the bot and both wizards offer `awg1` as a route,
  "(down)" while it is not UP. watchd treats it as an exit (`vpnconfig.TDExits`) and dials through
  it (`netpath`: `SO_BINDTODEVICE` plus the slot's mark). A router socket bound to `awg1` takes the
  interface's address and needs no NAT.

## 4. `lib/iftunnel.sh` and the `vpd_*` layer

The module handles interface tunnels, the same way on both platforms:

| Function | Does |
|----------|------|
| `iftunnel_list` | The valid names from `TUN_DIR_INTERFACES`, one per line |
| `iftunnel_table <idx>` | `IFTUNNEL_TABLE_BASE + idx`; the base is 2000 |
| `iftunnel_route_ensure <id> <idx>` | The NAT and MSS rules (section 6); then, while the interface exists and is UP, `ip route replace default dev <id> table <T>`. Returns 1 when the interface is missing or down, or when the route did not go in |
| `iftunnel_table_release <id> <idx>` | `ip route flush table <T>`, and the NAT and MSS rules removed |
| `iftunnel_iface <id>` | The id |
| `iftunnel_info <id>` | Three lines: `interface`, connected (`1` while UP, else `0`), an empty description |

- Tables 2000 and up are free on Merlin, whose firmware uses 100, 111-120 and 200. KeeneticOS's own
  Tunnel Director tables are 2000+idx as well (`KEENETIC_TABLE_BASE`); a slot belongs to one tunnel,
  so no two tunnels share a table.
- The UP test reads `ip -o link show <id>` whole, then matches `<([^>]*,)?UP[,>]`, as Merlin's
  `platform_tunnel_info` does. No `ip ... | grep -q`: the pipefail pitfall in `shell-conventions.md`.

The layer the core calls lives in the same file:

| Function | Does |
|----------|------|
| `vpd_tunnels` | `platform_tunnels` without `main`, then the valid interfaces, then `main`. Returns 1 when `platform_tunnels` does |
| `vpd_tunnel_table`, `vpd_tunnel_route_ensure`, `vpd_tunnel_table_release`, `vpd_tunnel_iface`, `vpd_tunnel_info`, `vpd_tunnel_unlisted_reason` | The signatures of the `platform_*` functions. A valid interface goes to `iftunnel_*`, any other id to `platform_*` |

- A release also goes to `iftunnel_table_release` for an id the last apply recorded as an interface
  tunnel (`iftunnels`, section 7) and the list no longer names.
- `vpd_tunnel_unlisted_reason` says why the module rejected a listed name, and asks the platform for
  any other id.
- `common.sh` sources the module right after `lib/platform.sh`. The module reads
  `TUN_DIR_INTERFACES` when it is called, so the order against `config.sh` does not matter.
- These callers move to the layer: every `platform_tunnels` and `platform_tunnel_*` call in
  `lib/tunnel.sh`, `cmd_platform` in `vpn-director.sh`, and the wizard in `configure.sh`.
  `configure.sh` sources `platform.sh` without `common.sh`; it sources the module as well and sets
  `TUN_DIR_INTERFACES` from the config it reads. The module logs only when `log` exists, as
  `config.sh` does.
- `lib/platform.sh`, `merlin.sh`, `keenetic.sh` and their tests stay as they are.

## 5. The kill switch

### Rules

Every slot except `main`'s gets two ip rules:

| Rule | Preference | Selector | Action |
|------|------------|----------|--------|
| Lookup (as today) | `pref_base + idx` | `fwmark <mark>/<mask>` | `lookup <table>` |
| Block (new) | `pref_base + F + idx` | `fwmark <mark>/<mask>` | `prohibit` |

`F` is the size of the mark field, `_tunnel_mark_field_max` (255 by default). With the defaults the
lookups take 16384-16638 and the blocks 16639-16893, all ahead of `main` at 32766. When the block
range would reach 32766 (`pref_base + 2F > 32766`), `tunnel_apply` logs an ERROR and returns 1
before it touches a rule.

When the tunnel's table has no route for a packet - the interface is missing or down, the route never
went in, the firmware flushed the table - the lookup falls through to the block, never to `main`.
`prohibit` answers the client with ICMP "administratively prohibited", so a connection fails at once
instead of hanging; Merlin's own killswitch uses the same action. The exclusions return ahead of the
mark, so an excluded destination still goes direct.

### Order and lifecycle

- Rebuild, routing step: for each slot the block rule goes in first. Then a new slot gets its table
  released, its route and its lookup rule; a kept slot gets its route and lookup rule ensured. At no
  moment does a marked packet meet neither rule.
- Up-to-date path: `_tunnel_ensure_routes` ensures each row's block rule beside its lookup rule, so
  a router gets the kill switch on its first apply after the upgrade, without a rebuild.
- `_tunnel_block_ensure` reads the whole `ip rule show` listing, as `_tunnel_rule_listed` does, and
  looks for `from all fwmark <mark>/<mask> prohibit` on the preference. Any other rule there makes
  way: the range is this module's.
- Slot release, after the swap: the lookup rule (ours only), the table, the block rule (ours only).
- `tunnel_stop` deletes the preferences `pref_base` to `pref_base + 2F - 1`. After `stop` the clients
  go direct, as today.

### Unknown tunnels

A tunnel the platform does not list (`_tunnel_table_allowed` fails) and that has clients is no
longer skipped. It gets a slot (kept from `TUN_DIR_TABLES`, or the lowest free), its clients' MARK
rules with their exclusions, and the block rule. Then:

- When `vpd_tunnel_table` names its table, its route and lookup rule go in as for a listed tunnel,
  and the platform decides about the route. KeeneticOS, while RCI is silent, still routes `WireguardN`
  by its naming convention, so the clients keep flowing. Merlin refuses `ovpncN` in "No" mode and
  removes our default, so the clients are refused.
- When it names none - a typo, an interface taken off the list - the slot keeps the block rule only.
  A lookup rule of ours still on its preference goes, and the table is released through the layer,
  which recognises a former interface tunnel by its `iftunnels` record.
- The hash stays unrecorded, as today, so the next apply retries. The slot is kept, so a tunnel that
  comes back keeps its mark and table. Its clients stay in `TUNNEL_UNCARRIED`.
- The WARN no longer says "is skipped": it says the tunnel is not on the platform's list, why, and
  that its clients are refused.

### Unchanged

- `TUNNEL_UNCARRIED` names the same clients, and `tproxy_prune` keeps those that came from Xray on
  the proxy.
- `failover_ready` still needs the route and the lookup rule. A committed Xray failover onto a tunnel
  that goes down is refused rather than leaked until the watch moves it on.
- `main` has no block rule and is never unknown.
- The firmware's own killswitch (`vpn_clientN_enforce`, `wgcN_enforce`) no longer matters for
  Tunnel Director clients.

### Texts

The Web UI (`ClientsTab.vue`: `confirmDown`, `confirmUnlisted`) and the bot (`moveQuestion` in
`handler/clients.go`) promise the WAN today. They say instead that the client's traffic to the
tunnel is refused until the tunnel is up, or until the router lists it, and that excluded
destinations still go direct. README ("What remains", both languages) and `packet-flow.md` ("Left
open") change to match.

## 6. NAT and MSS

For every interface tunnel with a slot, `iftunnel_route_ensure` keeps three rules at the head of
their chains:

1. `nat POSTROUTING -o <iface> -j MASQUERADE`.
2. `mangle FORWARD -o <iface> -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu`. An
   outgoing SYN takes the tunnel's MTU: 1280 - 40 = 1240 on `awg1`.
3. `mangle FORWARD -i <iface> -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss <MTU - 40>`, for
   the SYN-ACK coming back. `--clamp-mss-to-pmtu` would read the LAN's 1500 there. The MTU comes
   from the interface; `sync_fw_rule` replaces the rule when the MTU changes, and the rule waits
   while the interface is missing.

Rules 1 and 2 need no interface: iptables matches the name, so they go in whether the interface
exists or not, and they survive its re-creation. At the head of their chains no firmware rule takes
the packet first, and they match the tunnel's traffic only. `iftunnel_table_release` removes all
three, for a dropped slot and on `stop`.

The firmware's rebuilds:

- Merlin, firewall restart: `iptables -t mangle -F` takes the MSS rules and the marks.
  `firewall-start` applies; the apply rebuilds `TUN_DIR` and ensures everything.
- Merlin, `nat` rebuilt (for example on WAN up, or after a port forward changed): the firmware runs
  `/jffs/scripts/nat-start`, a name its `rc` knows. A new hook ships there, like `firewall-start`:
  `vpn-director.sh --wait apply`. The apply takes the up-to-date path and puts `MASQUERADE` back.
- KeeneticOS: the netfilter hook already applies after a rebuild of `nat` or `mangle`.

Between a rebuild of `nat` and the hook's apply, packets enter the tunnel without NAT and the far end
drops them: a stall of seconds, no leak. A connection opened in that window keeps a conntrack entry
without NAT until the entry expires; new connections work.

`status tunnel` shows, for each interface tunnel, its table, its route and whether the three rules
are in place.

## 7. Route recovery

Deleting an interface takes its routes along. `S51awg1`'s watchdog restarts `awg1` with
`ip link del`, which leaves the tunnel's table empty, and the clients stay refused until something
puts the route back. The ip rules, `MASQUERADE` and `TCPMSS` survive.

- **State.** Each apply writes `/tmp/tunnel_director/iftunnels`: one `<iface> <table>` line per
  interface tunnel with a slot, through a file beside it and a rename. `tunnel_stop` removes it.
- **`vpn-director.sh ensure-routes`.** It exits quietly once `stop` has run
  (`/tmp/vpn-director/stopped`), and when the state file is missing or empty. Otherwise it reads, for
  each line, whether the interface exists and is UP and whether its table holds a default, each
  output read whole. An interface that is UP without a default makes it log one INFO line and run
  `apply tunnel`, which takes the up-to-date path and puts the route back, with the MSS rule for the
  current MTU. It takes the lock without waiting: an apply that holds the lock does the same work. A
  minute with nothing missing costs a bash start, one file and two `ip` calls - no jq, no config.
- **Cron.** The job `vpn_director_routes` runs `ensure-routes` every minute (`* * * * *`). The CLI
  manages the job after every `tunnel_apply`:
  - With interface tunnels and no marker `/tmp/tunnel_director/routes_cron`, it adds the job
    (`platform_cron_add`) and writes the marker.
  - Without them and with the marker, it removes both.
  - A job the platform's cron refuses logs a WARN with `platform_cron_requirements`' text and leaves
    no marker, so the next apply tries again; the apply does not fail.

  So the job arrives with the first apply after an upgrade or after a change of the list, and a
  router without interfaces never gets it. `cron remove` (S99 stop) removes the job and the marker
  together with the daily `update`.
- **Fast path.** It belongs to the interface's init script (section 12): once the interface is up,
  the script runs `vpn-director.sh --unless-stopped --wait apply tunnel` in the background. The route
  is back within seconds with it, within a minute without it.
- **Boot.** `S51awg1` starts ahead of `S99vpn-director`, so the apply at boot usually finds `awg1`
  up. When it does not, the clients stay refused until the cron job or the fast path puts the route
  in.

## 8. Address exclusions

A tunnel's `exclude` takes IPv4 addresses and CIDRs beside country codes. `_tunnel_emit_client`
tells them apart with `is_ipv4_net` (no leading zeros, prefix 0-32):

- An address or CIDR becomes `-s <client> -d <cidr> -j RETURN` ahead of the mark, beside the
  country RETURNs.
- Any other entry is a set, lower-cased, as today: "Exclude ipset '<x>' not found" when the set is
  missing.
- An IPv6 entry is skipped with a WARN: Tunnel Director routes IPv4 only.

`parse_exclude_sets_from_json` passes known country codes only, so an address never starts a
download. `exclude` belongs to `TUN_DIR_TUNNELS_JSON`, so a change rebuilds, and a refused RETURN
leaves the hash unrecorded so the next apply retries, as a refused country RETURN does.
`TunnelConfig.Exclude` is a free list of strings, so the daemons keep the entries. Neither the Web
UI nor the bot edits a tunnel's exclusions, and a tunnel the Web UI creates still copies
`xray.exclude_sets` only.

For the owner: `203.0.113.10` in the `exclude` of `awg1` sends channels 10 and 13 (home2 and the
notebook to yop:8443) direct, as the overlay intends, instead of AWG inside AWG with an MTU of 1360
inside 1280.

## 9. Testing

Shell (bats):

- `router/test/unit/iftunnel.bats`, new:
  - name validation: length, characters, `main`, a firmware id, the LAN, the WAN, a repeat; one WARN
    per rejected name;
  - `iftunnel_table`;
  - route ensure and release with the interface missing, down and UP;
  - the NAT and MSS rules on the stateful iptables mock: inserted first, `--set-mss` replaced on an
    MTU change, waiting without the interface, removed on release;
  - `iftunnel_info`;
  - the layer: the order of `vpd_tunnels` and its rc 1, the dispatch, the release of an id that
    `iftunnels` records and the list no longer names.
- `router/test/unit/tunnel.bats`:
  - the block rule for a new and a kept slot, ahead of the lookup rule; put back on the up-to-date
    path; removed with a released slot and by `tunnel_stop` (both ranges);
  - the refusal when the range reaches 32766;
  - an unknown tunnel: slot, marks and block; a lookup rule only when its table is named; a lookup
    rule left from a known past removed; no hash;
  - an interface tunnel end to end: table 2000+idx, route, the three rules, `iftunnels`;
  - `RETURN` for an address and a CIDR, a WARN for IPv6;
  - the hash unchanged by an empty list and changed by a non-empty one;
  - `live_flushes` stays empty.
- `router/test/config.bats`: `TUN_DIR_INTERFACES`.
- `router/test/integration/vpn_director.bats`:
  - `platform` lists the interfaces, connected and not;
  - `ensure-routes`: quiet with nothing missing; `apply tunnel` for an interface UP without a default;
    nothing once stopped or without the state file; an exit while the lock is held;
  - the cron job added with its marker and removed without interfaces; `cron remove` removes it.
- `router/test/unit/entrypoints.bats`, `router/test/unit/install.bats`: the `nat-start` hook in
  `files.manifest` with the tag `merlin`, its shebang, its `--wait apply`.

Go:

- `vpnconfig`: `Interfaces` survives a load and a save; an empty list is not written.
- `PlatformInfo` parses `type: interface`; `TDExits` takes an interface tunnel like any other.
- `handler`: the new `moveQuestion` texts.

No web test covers `ClientsTab.vue` (`npm test` runs `servers-monitor.cjs` and `watch-status.cjs`);
its two texts get a manual check.

## 10. Acceptance on the RT-AX86U

Acceptance starts after the owner's check of 2026-10-16 (`awg1` comes back by itself after a reboot
and holds for a day), and each step waits for the owner's yes. home2 (192.168.1.3) runs the agent
and reaches the Claude API through the tunnel: moving it, or stopping its tunnel, is announced
first.

1. Install the branch build and apply. `ip rule` shows the block range from 16639; the clients of
   `ovpnc2` work as before.
2. Stop OpenVPN client 2 (`service stop_vpnclient2`) with the notebook (192.168.1.30) on `ovpnc2`:
   its traffic is refused, not sent through the WAN. Start the client: the traffic is back. home2
   loses its exit for that time as well.
3. Add `awg1` to `interfaces` and move the notebook onto `awg1` in the Web UI. Its external address
   is yop's, 203.0.113.10. `status tunnel` shows table 2000+idx, the route and the three rules;
   the cron job is there.
4. `S51awg1 restart`: the notebook is refused during the restart, and the route is back within a
   minute, logged by `ensure-routes`.
5. `service restart_firewall`: the marks, NAT and MSS are back.
6. A rebuild of `nat` by the firmware: `nat-start` ran, `MASQUERADE` is back.
7. With `203.0.113.10` in `exclude`: conntrack shows the notebook's flow to yop:8443 through
   `eth0`, and channel 13 is alive.
8. Move home2 onto `awg1`, announced; channel 10 is alive. The owner then decides about `ovpnc2`.

## 11. Documentation

- `.claude/rules/tunnel-director.md`: interface tunnels, the kill switch and its preference layout,
  unknown tunnels, address exclusions, `ensure-routes` and its cron job, the `iftunnels` file.
- `.claude/rules/packet-flow.md`: the ip rule picture with the block range; "Left open" - a dead
  tunnel no longer reaches `main`, the firmware's windows remain.
- `README.md`, `README.ru.md`: `tunnel_director.interfaces`, addresses in `exclude`, "What remains".
- `CLAUDE.md`: the config notes, the `ensure-routes` command, `lib/iftunnel.sh` and
  `router/jffs/scripts/nat-start` in the architecture table.
- `.claude/rules/webui.md`, `.claude/rules/telegram-bot.md`: the confirmation texts.
- `.claude/rules/testing.md`: `iftunnel.bats`.

## 12. To do in wireguard-network («Сделать в wireguard-network»)

The owner does these in `/opt/gitlab/ai/wireguard-network`; this branch changes none of its files.

1. `scripts/remote/S51awg1`: after every successful `do_start` (start, restart, the watchdog's
   restart), the background call

   ```sh
   [ -x /opt/vpn-director/vpn-director.sh ] &&
     ( /opt/vpn-director/vpn-director.sh --unless-stopped --wait apply tunnel 9>&- >/dev/null 2>&1 & )
   ```

   In the background and with FD 9 closed, so that the watchdog does not wait up to 120 s for
   vpn-director's lock and vpn-director does not carry `S51awg1`'s lock away. `--unless-stopped`,
   so that a vpn-director the owner stopped stays stopped. A test in `tests/S51awg1.bats`.
2. `S51awg1` still sets no routes, rules or NAT: vpn-director owns them for the LAN.
3. `docs/nodes/asus.md`: vpn-director routes LAN clients into `awg1` and owns NAT and MSS; a restart
   by the watchdog refuses them for seconds with the fast path, up to a minute without it.
4. `docs/backlog.md`: once acceptance passes, close «Выход vpn-director через `awg1` роутера Asus»
   and «Каналы 10 и 13 и Tunnel Director роутера».

## 13. Out of scope

- The health of a tunnel. An interface that is UP while its flow is dead swallows the traffic until
  its own watchdog restarts it; nothing leaks.
- A fallback to another tunnel.
- The windows with no vpn-director rules at all: boot before `S99vpn-director`, the firmware's
  firewall rebuild before its hook, `stop` (`S99vpn-director restart` included).
- The routes the firmware copies from `main` into `ovpncN` (`8.8.8.8` and `8.8.4.4` via `eth0` on the
  owner's router), which send those destinations past `tun12`.
- IPv6, which neither module routes.
- Editing `tunnel_director.interfaces` or a tunnel's exclusions in the Web UI or the bot.
- Acceptance on KeeneticOS hardware: the code is the same, the check is Merlin's.

## 14. Facts this design rests on

Read on the RT-AX86U (Merlin 388.12_2), read-only, on 2026-10-15:

- `vpn_client2_enforce=0`, `vpn_client2_rgw=2`. `ip rule`: 0, 200 (fwmark 0x100/0x100 → `wan0`),
  10001 (from all → `ovpnc1`), 16384 (fwmark 0x10000/0xff0000 → `ovpnc2`), 32766, 32767.
- Table `ovpnc2`: `default via 10.74.150.1 dev tun12`, plus routes copied from `main` (`8.8.8.8` and
  `8.8.4.4` via `eth0`, the LAN, the WAN subnet).
- Client 2's config holds `persist-tun` and `ping-restart 60`: a dropped connection keeps `tun12`
  and its route. A stopped client gets its table flushed (`openvpn_control.c`, read on 2026-09-27).
- The OpenVPN and WireGuard client pages carry "Killswitch - Block routed clients if tunnel goes
  down" (`vpn_clientN_enforce`, `wgcN_enforce`); `libovpn.so` holds the string `prohibit`.
- `/sbin/rc` knows `nat-start`, `firewall-start`, `wan-event`, `services-start`, `wgclient-start` and
  `wgclient-stop`. `/jffs/scripts` holds `firewall-start`, `post-mount`, `services-stop` and
  `wan-event`.

From `wireguard-network` (`scripts/remote/S51awg1`, `docs/nodes/asus.md`):

- `stop` runs `ip link del awg1`; `restart` and the watchdog run `stop`, then `start`. The watchdog
  runs every minute from cron and restarts the interface once it has been up for 180 s and three
  pings through it fail.
- `awg1` is 10.77.0.3/32 with MTU 1280, endpoint 203.0.113.10:8444; the router's peer on yop has
  `AllowedIPs = 10.77.0.3/32`.

In the code:

- `SaveVPNDirectorConfig` marshals the struct: a key under `tunnel_director` that the struct lacks
  does not survive a daemon's save.
- `parse_exclude_sets_from_json` keeps known country codes only.
- KeeneticOS's netfilter hook applies after a rebuild of `filter`, `nat` or `mangle`.
- `TDExits` sorts the exit ids; `netpath` reads the first two fields of `TUN_DIR_TABLES`.
- `_tunnel_collect_applied` gives an unlisted tunnel no slot today. `platform_tunnels` on KeeneticOS
  prints only `main`, with rc 0, when RCI does not answer.
