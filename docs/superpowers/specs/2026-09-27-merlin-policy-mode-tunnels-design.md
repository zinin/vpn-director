# Merlin: Tunnel Director only through OpenVPN clients in VPN Director mode: design

Date: 2026-09-27
Branch: `feature/merlin-policy-mode-tunnels`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

On Asuswrt-Merlin, Tunnel Director must neither offer nor route through an OpenVPN client
whose "Redirect Internet traffic through tunnel" is anything but "VPN Director (policy
rules)". In the other two modes the firmware sends every packet of the router through the
client's routing table, so the default route Tunnel Director installs there takes the whole
network into the tunnel: the direct clients, the router itself (DNS for everyone), Xray's
upstream connections and the other tunnels' clients. With a server that routes nothing to
the Internet, the whole network goes offline.

The v0.18.0 device check found the hazard on the RT-AX86U (Merlin 388.11); v0.17.2 has it
too. KeeneticOS is not affected: there Tunnel Director routes through tables of its own
(`2000+idx`), which no firmware rule reads.

## 2. The hazard

### 2.1 What the firmware does

The mode is nvram `vpn_clientN_rgw`: `0` "No", `1` "Yes (all)", `2` "VPN Director (policy
rules)". The client page offers mode 2 only for a TUN client
(`Advanced_OpenVPNClient_Content.asp`). The sources cited below are RMerl/asuswrt-merlin.ng,
branch `3004.388`; the router's `libovpn.so` holds the same command strings.

| Mode | ip rules (`amvpn_set_routing_rules`, `amvpn_routing.c:219-236`) | Table `ovpncN` when the client comes up (`openvpn_control.c:283-363`) |
|---|---|---|
| `0`, or empty (`nvram_pf_get_int` reads it as 0) | `from all lookup ovpncN` at priority 10000+N, while the client runs | flushed, `main` copied in, the pushed routes added, then the default deleted (`ip route del default table ovpncN`) |
| `1` | the same `from all` rule | flushed, `main` copied in, the pushed routes, `default via $route_vpn_gateway dev tunX` |
| `2` | the VPN Director rules that name this client | as in mode 1 |

A client that stops loses its rules and its table (`openvpn_control.c:614`).

WireGuard clients have no mode: `amvpn_set_routing_rules` sets `rgw = OVPN_RGW_POLICY` for
every WireGuard unit (`amvpn_routing.c:210`), nvram holds no `wgcN_rgw`, and no firmware
rule sends every packet through a `wgcN` table.

### 2.2 What Tunnel Director does

On every apply, for every tunnel with clients, `platform_tunnel_route_ensure`
(`merlin.sh:159`) runs `ip route replace default via <gateway, or the first host of tun1N's
subnet> dev tun1N table ovpncN`. It assumes that only Tunnel Director's own rule,
`fwmark <mark> lookup ovpncN` at 16384+idx, reads the table: "the default we installed is
inert without it" (`merlin.sh:168-170`). That holds in mode 2 alone. In modes 0 and 1 the
firmware's rule at 10000+N, ahead of every rule but `local`, finds the default for every
packet:

- in mode 0 the table held no default, and now all traffic goes into the tunnel;
- in mode 1 all traffic went into the tunnel already, so Tunnel Director's exclusions
  cannot work, and its default replaces the firmware's.

`platform_tunnels` lists every `ovpncN` in `/etc/iproute2/rt_tables`, all five on 388,
configured or not. The Web UI, the bot and `configure.sh` offer all five, and the failover
takes any of them that carries clients and is up as an exit (`TDExits`).

Moving the client away does not undo it: the release is a no-op on Merlin
(`merlin.sh:171`), and the default stays until the OpenVPN client restarts or the router
reboots.

A second path puts the default back with the config unchanged. The apply's hash covers the
tunnels JSON only (`tunnel.sh:949-953`). Switch a client that carries Tunnel Director
clients from mode 2 to mode 0: the firmware's restart empties the table, the next apply
takes the up-to-date branch, and `_tunnel_ensure_routes` installs the default again for
every tunnel on record. A filter in `platform_tunnels` alone leaves this path open.

### 2.3 The owner's router

Read-only, 2026-09-27, RT-AX86U on 388.11: `vpn_client{1..5}_rgw` are 0, 2, 2, 2, 0;
`ip rule` holds `10001: from all lookup ovpnc1`; `ovpnc1` has no default, `ovpnc2` and
`ovpnc3` have one; `wgc1`-`wgc5` are off, their tables empty. Tunnel Director has never
carried a client through `ovpnc1`.

## 3. Decisions

| Question | Decision |
|---|---|
| Which OpenVPN clients | Mode 2 only. Modes 0 and 1 and an empty value are refused. |
| WireGuard (`wgcN`) | Unchanged: the firmware has no mode for it and no `from all` rule. |
| Offer or flag | The platform does not list a refused tunnel; everything downstream already follows the list. |
| Saying why | In the apply log only: a new contract function names the reason, and Tunnel Director's WARNs print it. |
| A default already in a mode-0 table | Removed whenever Tunnel Director touches the tunnel: in the route ensure it refuses and in the release. The firmware keeps no default in that mode. A mode-1 default is the firmware's and stays. |
| KeeneticOS | Unchanged. |

Rejected:

- **Listing the tunnel with a flag** (`routable: false` and a reason in the platform JSON, a
  disabled entry in the UI). Every consumer would have to learn the flag (`tunnel.sh`,
  `configure.sh`, `checkClientRoute`, `TDExits`, the bot, the wizard), and one that did not
  would offer the tunnel again: the fix would fail open.
- **The reason in the Web UI and the bot too**, from a separate list of refused tunnels in
  the platform JSON: Go, Vue and their tests for what the README and the log explain.
- **The guard in the core**: `_tunnel_ensure_routes` skipping, and releasing, every
  recorded tunnel the platform does not list. While KeeneticOS's RCI does not answer, the
  list holds only `main`: the up-to-date branch would stop installing routes and flush every
  table `2000+idx`, and every Tunnel Director client would leave through the WAN until RCI
  answered.
- **A rebuild when a recorded tunnel leaves the list**: the same failure on KeeneticOS, and a
  rebuild on every RCI silence.
- **Documenting the cleanup only**: a router the hazard already hit would keep sending
  everything into the tunnel after the update, until the OpenVPN client restarted.
- **Tunnel Director's own tables on Merlin** (`2000+idx`, as on KeeneticOS): option (b) of
  the device check. The owner chose (a).

## 4. Design

### 4.1 Merlin (`lib/platform/merlin.sh`)

- `_merlin_tunnel_routable <id>` holds the rule, and nothing else repeats it. `ovpncN`: 0
  when `nvram get vpn_clientN_rgw` prints `2`, 1 otherwise. `wgcN` and `main`: 0. It reads
  nvram on every call: the mode can change at any time.
- `platform_tunnels` prints the `wgcN` as today, then the `ovpncN` that
  `_merlin_tunnel_routable` accepts, then `main`. A TAP client drops out as well: its page
  offers no mode 2, and Tunnel Director, which looks for `tun1N`, could not route it anyway.
  A call costs at most five `nvram get`.
- `platform_tunnel_route_ensure` installs nothing for a refused tunnel and returns 1, after
  removing the default of an `ovpncN` in mode 0 (below). The rest is unchanged.
- `platform_tunnel_table_release` removes the default of an `ovpncN` in mode 0 and stays a
  no-op for every other tunnel.
- Removing the default: `ip route del default dev tun1N table ovpncN`, its failure ignored
  ("no such route" is the usual answer). The firmware runs the same delete in that mode;
  the `dev tun1N` selector narrows it to a default through the tunnel, the only kind Tunnel
  Director installs. Mode 0 means `0` or an empty value.
- `platform_tunnel_unlisted_reason <id>` prints one line for an `ovpncN` that `rt_tables`
  names and `_merlin_tunnel_routable` refuses:

  ```
  OpenVPN client 1 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No")
  ```

  The setting reads "No" for `0` or an empty value, "Yes (all)" for `1` and the raw value
  otherwise. For a tunnel the platform lists, for `wgcN` and `main`, and for an id
  `rt_tables` does not name (a typo), it prints nothing and returns 1.

### 4.2 The contract (`lib/platform.sh`) and KeeneticOS

- A new function, `platform_tunnel_unlisted_reason <id>`: why a tunnel the firmware has is
  not one `platform_tunnels` lists; nothing and rc 1 when there is no such reason.
- `platform_tunnels` lists the tunnels Tunnel Director may route through.
- Two notes under "What the core relies on": `platform_tunnel_route_ensure` may refuse a
  tunnel `platform_tunnels` does not list (on Merlin the firmware routes the whole router
  through its table), and the core reads the reason for its messages only, never to decide
  anything.
- `keenetic.sh`: `platform_tunnel_unlisted_reason` returns 1. Tunnel Director may route
  through every OpenVPN and WireGuard interface RCI lists, and the tables are its own.

### 4.3 Tunnel Director (`lib/tunnel.sh`)

- `_tunnel_collect_applied`, for a tunnel the platform does not list: with a reason,
  `WARN "Tunnel 'ovpnc1' is skipped: <reason>"`; without one, today's text ("is not a
  tunnel this platform knows").
- `_tunnel_ensure_routes`, for a route ensure that fails: with a reason,
  `WARN "Tunnel 'ovpnc2': route not installed: <reason>; Tunnel Director does not route
  through it"`; without one, today's text ("interface down or not mapped?").
- Comments and texts: the comment above `_tunnel_ensure_routes` (`:250`, "A no-op on
  Merlin") is corrected. The comment at `:1158-1169` ("On Merlin the only case is a typo")
  and the WARN at `:1173` ("RCI down, or a typo in the id") name no platform and keep "not
  recorded as up-to-date". The file header lists the new function.

The mechanics stay as they are:

- A config that names a refused tunnel records no hash, as one with a typo does: every
  apply rebuilds `TUN_DIR` in place and repeats the WARN, and the first apply after the
  switch to VPN Director carries the tunnel's clients.
- A tunnel on record whose mode changes while the config stays keeps its slot on the
  up-to-date branch. Its route ensure refuses on every apply (in mode 0 it removes the
  default first), the WARN repeats, and its clients are reported as not carried, so
  `tproxy_prune` keeps the ones `XRAY_CLIENTS` still holds. Switched back to VPN Director,
  the tunnel carries them again without a rebuild.
- A refused failover tunnel gets no `failover_ready`, so the watch keeps Xray membership.
  `TDExits` no longer offers it as an exit.

### 4.4 Daemons, Web UI, bot

No change: all of them read the list from `vpn-director.sh platform`. The Web UI's route
options and Add form, the bot's route keyboard and wizard, `TDExits` (the failover exits and
the bot's own Telegram path) and `configure.sh` stop offering a refused tunnel. A client
already on one keeps its row's route. The Web UI asks before a move or an add to a tunnel
the router does not list (commit `526744f`); the bot marks such a tunnel "(unknown)" and
asks before a move. `checkClientRoute` accepts it only because the config already holds it,
as it accepts any unlisted tunnel, and the shell skips it.

## 5. Tests

TDD: every new test fails before the change it covers.

- `router/test/mocks/nvram`: `vpn_client1_rgw` and `vpn_client2_rgw` print `2`. The fixture
  `rt_tables` names `ovpnc1` and `ovpnc2`, so every existing test sees what it sees today.
- `platform_merlin.bats`:
  - `platform_tunnels` lists an `ovpncN` in mode 2 only; mode 0, mode 1 and an empty value
    drop it; the `wgcN` do not depend on nvram;
  - `platform_tunnel_route_ensure` returns 1 in modes 0 and 1 and with an empty value, and
    the `ip` log holds no `route replace`;
  - in mode 0 and with an empty value, `route_ensure` and `table_release` each run
    `ip route del default dev tun11 table ovpnc1`; in mode 1 neither runs a `route del`;
    in mode 2 the release still touches nothing;
  - `platform_tunnel_unlisted_reason` prints the exact line for mode 0 ("No"), mode 1
    ("Yes (all)") and an empty value ("No"), and returns 1 without output for mode 2,
    `wgc1`, `main` and `ovpnc9` (not in `rt_tables`).
- `platform_keenetic.bats`: `platform_tunnel_unlisted_reason` returns 1 without output.
- `tunnel.bats`, through the real Merlin implementation, with client 1 in mode 0:
  - a rebuild with `ovpnc1` in the config: the WARN carries the reason, the `ip` log holds
    no `route replace` into `ovpnc1`, and no hash is recorded;
  - the up-to-date branch (`ovpnc1` on record, the hash matching, the mode switched): no
    `route replace`, a `route del default dev tun11 table ovpnc1`, the WARN with the reason,
    and the client in `tunnel_uncarried`;
  - without a reason the texts stay as they are, which the `wgc9` tests already pin.
- `vpn_director.bats`: `platform` does not list an OpenVPN client in mode 0
  (`[.tunnels[].id] == ["wgc1","wgc2","ovpnc2"]`).

## 6. Documentation

- `CLAUDE.md`, Key Concepts: a Merlin tunnel key is a `wgcN` from `rt_tables`, an `ovpncN`
  whose client is in "VPN Director (policy rules)" mode, or `main`.
- `.claude/rules/tunnel-director.md`: the tunnel key row, Requirements, the platform
  functions under Dependencies, and a paragraph on why Merlin lists no client in "No" or
  "Yes (all)" and why its route ensure refuses such a client and cleans up in mode 0, with
  the firmware references of section 2.
- `lib/platform.sh`, `merlin.sh`, `tunnel.sh`: the comments of section 4.
- `README.md` and `README.ru.md`: under Requirements for Asuswrt-Merlin, an OpenVPN client
  in "VPN Director (policy rules)" mode; under Tunnel Director, a sentence or two on why the
  other modes are not offered and what an apply does with a config that names such a
  client.
- The v0.18.1 release notes, if the owner releases one: for a tunnel Tunnel Director no
  longer has on record, restart that OpenVPN client or run
  `ip route del default table ovpncN`.

## 7. Verification

- The whole bats suite (the four places of `testing.md`), in the background, the counts read
  from its log.
- shellcheck: no more findings than `master` (53).
- No Go or web change, so no builds.
- After a release, read-only on the RT-AX86U: `vpn-director.sh platform` lists neither
  `ovpnc1` nor `ovpnc5`.

## 8. Out of scope

- A Merlin router with no `wgcN` table and no OpenVPN client in mode 2 now gets
  `tunnels: []` from `vpn-director.sh platform`. The daemons read an empty list as a
  platform that cannot answer (KeeneticOS's RCI silence), so a route outside the config gets
  503 instead of 400. Such a router offers no tunnel either way.
- A default left in the table of a tunnel Tunnel Director no longer has on record (its
  clients moved away before this fix): the release notes cover it.
- The `gateway` option and the route itself in mode 2: unchanged.
