# Leak-free route changes: design

Date: 2026-09-25
Branch: `feature/leak-free-route-change`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Moving a LAN client from Xray to a tunnel, or back, takes two actions today:
delete the client, then add it with the new route. Between the two, its traffic
leaves through the WAN. Traffic that belongs in a VPN reaches the WAN for
shorter stretches without any move as well: every apply, every Tunnel Director
change and every server switch opens a window (section 2).

This change:

- moves a client to another route in one action, from the Web UI and from the
  Telegram bot;
- makes every apply, restart and server switch make-before-break: at every
  moment a client's packets take its old route, its new route or no route at
  all, and never the WAN.

A **leak** here is a packet from a client configured for Xray or a tunnel that
leaves through the WAN to a destination its route does not exclude. A dropped
packet is no leak: TPROXY drops what no socket takes, so a restarting Xray costs
packets, not privacy. Traffic meant to go direct stays direct: the exclusions
(country sets, `xray.exclude_ips`, private ranges), paused clients and the
`main` route.

## 2. Where traffic leaks today

| # | Window | Who leaks | For how long |
|---|---|---|---|
| 1 | A move is a delete, then an add | the moved client | until the second action |
| 2 | Every apply: `_tproxy_setup_iptables` flushes `XRAY_TPROXY` (`create_fw_chain -f`) and `_tproxy_setup_clients_ipset` flushes `XRAY_CLIENTS`, then both refill one entry at a time | every Xray client | a fraction of a second, on every apply, the hooks and the daily update included |
| 3 | Every Tunnel Director rebuild: `tunnel_apply` runs `tunnel_stop` (the jumps, the chain, 255 `ip rule del`, the tables), then builds everything again | every TD client | seconds |
| 4 | `cmd_apply` runs `tproxy_apply` before `tunnel_apply` | a client moving from Xray to a tunnel | until the TD part ends |
| 5 | A server switch (the Web UI, `/xray`, the wizard) runs `restart xray`: the process restart, then `stop xray`, then `apply xray` | every Xray client | from the stop to the end of the apply |

The subscription watch already avoids window 5: it restarts Xray with
`restart xray-process`, which leaves the TPROXY rules in place.

The firmware opens windows of its own, and this change leaves them open
(section 11).

## 3. Decisions

| Question | Decision |
|---|---|
| Scope | Windows 1–5, the ones our code opens. The firmware's windows and a tunnel that is down stay as they are, documented. |
| Mechanism | Plain iptables and ipset, make-before-break: a chain is built beside the live one and swapped in; a set is swapped or added to, never flushed. |
| Unit of change | A full apply: Xray adds, Tunnel Director applies, Xray prunes. |
| Tunnel Director slots | Stable: a tunnel keeps its idx, and with it its mark, preference and table, for as long as it has clients. |
| `restart` | `restart` and its component forms stop nothing: they rebuild in place, and restart the Xray process where they did before. Only `stop` tears down. |
| Web UI | A select in the Route column moves the client at once; a tunnel that is down asks first. |
| Telegram bot | A 🔀 button per client in `/clients` opens a route keyboard. |
| A paused client | Moves and stays paused. |
| A failover | A move takes the address out of the failover record, as Add and Delete do. |

Rejected:

- **`iptables-restore --noflush`.** One atomic commit per chain and the
  simplest code, but a new dependency to verify on both firmwares (Merlin ships
  several iptables versions; Keenetic runs Entware's 1.4.21, whose restore takes
  no `-w`), and all or nothing: one refused rule rejects the whole chain, so a
  chain the firmware has just flushed would stay empty until the next apply.
  Today every other client still gets its rules.
- **Two applies per move, in Go**, the way the watch stages a failover. The
  smallest change, but windows 2 and 3 stay: both applies of the move leak for
  every client.
- **A kill switch for a tunnel that is down**: an `unreachable` default beneath
  the tunnel's route in Keenetic's tables. It changes what a down tunnel means
  for every client, and nobody asked for it.
- **Fail-closed routing**: source-based ip rules, which survive the firmware's
  firewall rebuilds, so that a missing iptables rule sends a client into its
  tunnel or nowhere. It is the only way to close the firmware's windows, but it
  redesigns both modules and changes behaviour (ICMP, exclusions during a
  window). A project of its own.

## 4. Moving a client

### 4.1 The config change

`vpnconfig.MoveClient(cfg, addr, route)` changes the config in one locked
update:

- It removes every spelling of the address (`1.2.3.4`, `1.2.3.4/32`) from
  `xray.clients` and from the clients of every tunnel.
- It appends the normalized address to the target route. A tunnel new to the
  config inherits `xray.exclude_sets`, as Add does: an empty `exclude` would
  carry the client's local-country traffic through the tunnel too.
- It repoints `paused_clients` at the new spelling (`RepointPausedClients`).
  The shell subtracts paused clients by exact string, so a paused client would
  otherwise resume on its own.
- It takes the address out of `xray.failover` (`DetachFailoverClient`), so a
  restore cannot undo the user's choice. A client moved to Xray while Xray is
  down joins the failover afresh, as an added one does.
- A tunnel whose last client leaves stays in the config with an empty list, as
  after a Delete.

A move changes nothing when the target route is the only route that holds the
address.

### 4.2 API

`POST /api/clients/route` with `{"ip": "…", "route": "…"}`:

- The address must normalize (`NormalizeClientAddr`), as for Add; 400
  otherwise. The lookup matches every stored spelling of it.
- The route is checked as Add checks it: `xray`, a tunnel in the config, or a
  tunnel `/api/platform` lists. 503 when the platform cannot answer for a tunnel
  outside the config, 400 for a route it does not list.
- 404 when no route holds the address.
- A move that changes nothing answers 200 and neither writes nor applies.
- Otherwise `updateAndApply`: the change under the config lock, then one
  `vpn-director.sh apply`. The answers are those of the other client routes,
  `saved: true` included when only the apply failed.

### 4.3 Web UI

- The Route cell of every row becomes a `<select>`. Its options are the Add
  form's (xray, the platform's tunnels, and the routes in use when the platform
  cannot be asked), plus the row's own route when they lack it.
- Picking a tunnel whose `connected` is false asks first: "Wireguard1 is down:
  its traffic goes out through the WAN until it is up. Move anyway?" Cancel puts
  the select back. The Add form asks the same before it adds to such a tunnel.
- While the request runs, the row's controls are disabled. The list reloads when
  it ends, after an error too (`reportError`).
- Rows are keyed by `ip|route`: during a staged failover one address sits in two
  routes, and two rows shared the key `ip`.

### 4.4 Telegram bot

- `/clients` gives every client a third button, `🔀 <ip>` (`clients:move:<ip>`).
- The button replaces the list with a route keyboard: xray, the platform's
  tunnels with their description and "(down)", and the config's tunnels the
  platform does not list, marked "(unknown)" as in the Add flow. The current
  route carries ✓, and « Back returns to the list.
- A route button (`clients:to:<route>:<ip>`) moves the client; the prefix, a
  route id and an address stay well within Telegram's 64 bytes. A tunnel that
  is down asks first, with a second button that confirms.
- The move runs `MoveClient` and `Apply`, and the message shows the list again.
  A button whose client is gone, or whose route is no longer offered, redraws
  the list, as the other `/clients` buttons do.

## 5. The apply

### 5.1 A full apply

A full apply is the unit of change, and it runs make-before-break:

1. **Country sets**: `_ensure_ipsets`, as today.
2. **Xray make** (`tproxy_apply`): the TPROXY routing (the ip rule at pref 200,
   table 100); `TPROXY_BYPASS` by a swap; every effective client *added* to
   `XRAY_CLIENTS`, none removed; and `XRAY_TPROXY` rebuilt beside the live chain
   and swapped in (section 6), with its jumps and the platform's own rules. The
   ready marker is decided here.
3. **Tunnel Director** (`tunnel_apply`): the up-to-date path, or a
   make-before-break rebuild (section 8).
4. **Xray prune** (`tproxy_prune`, new): `XRAY_CLIENTS` becomes exactly the
   effective clients, by a swap. A client that left Xray is let go only now,
   when Tunnel Director already carries it.

Both directions hold. A client moving from Xray to a tunnel is marked by
`TUN_DIR` from step 3 on, but `XRAY_TPROXY` runs first and keeps taking it until
step 4 lets it go. A client moving from a tunnel to Xray is taken by TPROXY at
step 2 and let go by `TUN_DIR` at step 3.

A Tunnel Director failure ends the run under errexit, and step 4 does not run.
The clients that left Xray stay proxied rather than leak, and the next apply
that succeeds completes the move. The Web UI and the bot report an apply that
failed with the change saved, as they do today.

### 5.2 Commands

| Command | Today | Now |
|---|---|---|
| `apply`, `update` | Xray, then TD | make → TD → prune |
| `restart` | process restart, `stop`, `apply` | process restart, then make → TD (rebuilt even when up to date) → prune |
| `restart xray` | process restart, `stop xray`, `apply xray` | process restart, then make → prune |
| `restart tunnel` | `stop tunnel`, `apply tunnel` | TD rebuilt in place, even when up to date |
| `apply xray` | apply xray | make → prune |
| `apply tunnel`, `restart xray-process`, `stop` | — | unchanged |

- The stopped marker behaves as today: `restart` removes it, as `apply` does,
  and a component command keeps it.
- `restart xray` keeps the TPROXY rules in place while the process restarts, so
  a server switch (the Web UI, `/xray`, the wizard) drops packets for that
  moment instead of leaking them. `TPROXY_BYPASS` is swapped, as in every apply.
  The Go callers do not change.
- `configure.sh` ends with `restart`, so the shell wizard switches without a
  window too.
- A component command does not coordinate with the other module, so only a full
  apply can move a client. The daemons move clients with full applies alone, and
  a server switch moves none. The help text says so.
- The subscription watch keeps its two-step failover and restore; each of its
  applies is leak-free now as well.

## 6. Swapping a chain

A new helper in `lib/firewall.sh` replaces the contents of a chain that the
PREROUTING jumps lead to, `XRAY_TPROXY` and `TUN_DIR` alike:

0. **Recover.** A `<chain>_NEW` left by an interrupted swap is complete when a
   jump leads to it, since the jumps go in only after the build; the helper
   finishes that swap (steps 3–5). Otherwise it deletes the chain.
1. **Create** an empty `<chain>_NEW`.
2. **Build.** The caller fills it one rule at a time, with today's error
   handling. When the caller reports a rule that bounds the chain as failed (for
   Xray, rule 1, `! XRAY_CLIENTS → RETURN`, and the private-range RETURNs), the
   helper deletes `<chain>_NEW` and fails, and the live chain keeps working.
   Today the same failure leaves the live chain flushed, intercepting nothing.
3. **Insert** a jump to `<chain>_NEW` for every LAN interface, where the chain's
   jump belongs: Xray from position 1; Tunnel Director at the platform's base
   position (`platform_prerouting_base_pos`), never ahead of an Xray jump. Both
   positions are read from the current listing, the old jump included.
4. **Delete** the old jumps of the interfaces whose new jump went in.
5. **Retire.** Once no jump leads to `<chain>`: `-F`, `-X`, then
   `-E <chain>_NEW <chain>`. When a new jump did not go in, both chains stay,
   that interface keeps the old one, and the helper fails; the next apply
   finishes the swap through step 0.

Every step is atomic for a packet, and every packet crosses a whole chain, old
or new. While both jumps stand, the new chain comes first, and the old one can
only take up what the new one returned, which is harmless.

A chain name from `advanced.*.chain` must leave room for `_NEW`: 24 characters
at most, where iptables allows 28. A set name from `advanced.xray.*_ipset` gets
the same suffix: 27 characters at most, where ipset allows 31. The helpers
refuse a longer name with an ERROR.

## 7. Xray (`tproxy.sh`)

- `XRAY_TPROXY` is rebuilt and swapped on every apply, as today it is refilled
  on every apply. That still repairs a chain Merlin's `mangle -F` emptied, now
  without a window. A narrowing RETURN that fails (bypass, loopback, link-local,
  multicast, broadcast, an exclusion) still costs only the ready marker: the
  targets and the swap go on, as today.
- `TPROXY_BYPASS` is built as `TPROXY_BYPASS_NEW`, then `ipset swap` and
  `destroy`.
- `XRAY_CLIENTS` in `tproxy_apply`: created when missing, then
  `ipset add -exist` for every effective client. No flush, no delete.
- `XRAY_CLIENTS` in `tproxy_prune`: the exact set is built as
  `XRAY_CLIENTS_NEW`, then swap and destroy. A prune that fails leaves extra
  clients proxied, with a WARN, and leaves the ready marker alone: every
  effective client is still in the set.
- The ready marker means what it means today: the chain swapped in whole, the
  jumps and the platform's rules in place, every effective client in the set.
- `tproxy_stop` tears down, as today.

## 8. Tunnel Director (`tunnel.sh`)

### 8.1 Stable slots

Today the slots follow the order of the tunnels in the JSON, so a tunnel that
gains its first client or loses its last one shifts the mark, the preference
and, on Keenetic, the table (`2000 + idx`) of every tunnel after it. A
make-before-break swap cannot follow such a shift: the chain and the ip rules
would change in two separate steps, with one tunnel's clients routed through
another tunnel's table in between.

Now the previous layout comes from `TUN_DIR_TABLES`. A tunnel in it keeps its
idx; a tunnel new to the layout takes the lowest idx that neither layout uses,
so an apply never reuses a slot it frees. The chain keeps today's order (the
failover clients first, then the tunnels in JSON order), so the first match
still wins between overlapping CIDRs. `TUN_DIR_TABLES` keeps its format
(`<idx> <id>`): the first apply after the update adopts the layout the old
version wrote, without renumbering, and the bot still derives its `SO_MARK` from
the file.

### 8.2 The rebuild

The triggers stay (a new hash, no chain, no `TUN_DIR_TABLES`, a missing MARK
rule), and `restart` adds one: a rebuild even when up to date.

1. **Routing, before any packet carries a new mark.** For every tunnel of the
   new layout:
   - a new slot: its table is released first (Keenetic: `ip route flush`,
     clearing what an apply that died left behind), then its route is ensured,
     then its ip rule is replaced at its preference, as today;
   - a kept slot: its route is ensured (`ip route replace`, idempotent;
     releasing a live table would open a window), and so is its ip rule
     (`_tunnel_rule_ensure`, which leaves a correct rule alone).
2. **The chain.** `TUN_DIR_NEW` is built (the exclusion RETURNs, the offload
   target on Keenetic, the MARK rules with the stable marks) and swapped in
   (section 6).
3. **Release.** Every slot of the previous layout that the new one lacks loses
   its ip rule, only when the rule is ours (`from all fwmark M/mask lookup T`,
   as `_tunnel_rule_listed` reads it), and its table is released. After the
   swap no packet carries its mark.
4. **Record** the hash and `failover_ready` by today's rules. From step 1 to
   step 3, `TUN_DIR_TABLES` holds the union of both layouts, so an apply that
   dies in between leaves every slot in use on record, and the next apply
   releases the extras. After step 3 it holds the new layout.

### 8.3 What stays

- `tunnel_stop` runs only for `stop` and for the branch with no tunnels at all
  (`{}`). By then Xray make has taken every client that moved to Xray.
- The up-to-date path: routes, ip rules and jumps re-ensured, and the failover
  checks.
- A tunnel the platform does not list (a typo, or RCI silent during an NDM
  rebuild) loses its slot, as today: its clients go through the WAN, no hash is
  recorded, and the next apply retries.
- A rebuild runs no 255 `ip rule del` and releases no live table, so it is
  faster, the rebuilds after NDM deleted our chain (the ip rules and routes
  survive it) and after Merlin's `mangle -F` included. That shortens the
  firmware's window without closing it.

## 9. Failures

A failure leaves the previous routing or a proxied client, never a leak.

| Failure | Result |
|---|---|
| The run dies between make and prune | The clients that left Xray stay proxied; the next successful apply lets them go |
| A bounding Xray rule does not go in | No swap: the live chain keeps working; no ready marker |
| A new jump does not go in | Both chains stay, that interface on the old one; no ready marker or no `failover_ready`; the next apply finishes the swap |
| The rename fails | `<chain>_NEW` stays live; the next apply renames it |
| A MARK rule is refused | That client only, as today: no hash, and the next apply rebuilds |
| The firmware flushes our chains mid-apply | The hook's queued `--wait apply` rebuilds them, as today |

## 10. Testing

Bats:

- `firewall.bats`, the swap helper: the order of the calls (build in `_NEW`,
  insert the new jump, delete the old one, `-F`, `-X`, `-E`); no flush of the
  live chain during an apply; recovery from an interrupted swap, with and
  without a jump on `_NEW`; a failed jump leaving both chains. These tests need
  a stateful iptables mock (rules per chain in files, a listing, `-C`,
  positional `-I`) beside today's stateless one.
- `tproxy.bats`: no `ipset flush` of either set; make only adds; prune swaps in
  the exact set; the bypass set is swapped; a failed bounding rule keeps the
  live chain.
- `tunnel.bats`: stable slots (a kept idx, the lowest free idx for a new tunnel,
  no reuse of a slot freed in the same apply); no `tunnel_stop` in a rebuild; a
  new slot's ip rule before the swap and a released slot's rule after it; no
  release of a kept slot's table. The tests that assert today's teardown are
  rewritten.
- `vpn_director.bats`: a full apply calls make, then TD, then prune; `restart`,
  `restart xray` and `restart tunnel` stop nothing; the stopped marker behaves
  as before.

Go:

- `vpnconfig.MoveClient`: every spelling, the paused entry, the failover record,
  a new tunnel's exclusions, a move that changes nothing.
- `POST /api/clients/route`: 400, 404, 503, a move that changes nothing, saved
  but not applied.
- The bot: the move keyboard, stale buttons, the confirmation for a tunnel that
  is down.

Web: `vue-tsc` and the build.

## 11. Limits

The READMEs and the rules document these:

- **The firmware's windows.** Merlin runs `iptables -t mangle -F` on every
  firewall start, and NDM rebuilds the tables on any configuration change. Until
  the hook's apply, clients go through the WAN. The window is shorter now but
  still open; fail-closed routing (section 3) would close it.
- **A tunnel that is down** sends its Tunnel Director clients to `main`, the
  WAN. On Merlin the killswitch of the firmware's VPN client prevents that. The
  Web UI and the bot warn before a move to such a tunnel.
- **Open connections break on a move.** On Keenetic, a flow the fast path
  already holds keeps its old path until its conntrack entry expires, and a UDP
  flow moved from Xray to a tunnel can stall until its entry expires. Nothing
  flushes conntrack: Keenetic has no conntrack-tools.
- **Component commands** (`apply xray`, `apply tunnel`, `restart xray`) cannot
  move a client.
- **`S99vpn-director restart`** stops the service and starts it again.
- **IPv6** is routed by neither module, as before.

## 12. Device check

On the RT-AX86U (Merlin) and the KN-4521 (Keenetic):

1. `iptables -E` renames a chain that a jump leads to, and the jump follows it.
   It is the one iptables operation this change adds; `ipset swap` on a set a
   rule uses already runs in `ipset.sh`.
2. A counter without a target in `FORWARD`: `-s <client> -d 1.1.1.1 -o <wan>`
   (added again after a firmware rebuild of the table). The client loops
   `curl https://1.1.1.1` and a DNS query to `1.1.1.1`: TCP and UDP, since ICMP
   from Xray clients goes direct by design.
3. Moves from xray to a tunnel, from a tunnel to xray and between tunnels, a
   server switch in the Web UI, `restart`, `restart xray` and an apply that
   changes nothing: the counter stays at 0. The same run on the current release
   makes it grow, which proves the check catches a leak.
4. After each step: `iptables -t mangle -S PREROUTING` lists the Xray jumps,
   then the `TUN_DIR` jumps (behind the firmware's iface-mark rules on Merlin);
   no `_NEW` chain or set is left; `tun_dir_tables` keeps the idx of every
   tunnel that kept clients, and `ip rule` shows their rules untouched.

## 13. Documentation

- `.claude/rules/packet-flow.md`, `tunnel-director.md`, `xray-tproxy.md`: the
  apply order, the stable slots, the swap, the new `restart`.
- `.claude/rules/shell-conventions.md`: the swap helper, and the rule that an
  apply never flushes a live chain or a live set.
- `.claude/rules/testing.md`: the stateful iptables mock.
- `.claude/rules/webui.md`: `POST /api/clients/route` in the API table.
- `.claude/rules/telegram-bot.md`: `/clients` in the command table, where it is
  missing, and its move button.
- `README.md`, `README.ru.md`: moving a client, leak-free transitions, the
  limits.
- `CLAUDE.md`: the `restart` lines of the command list.

## 14. Out of scope

- A kill switch for a tunnel that is down.
- Closing the firmware's windows (fail-closed routing).
- Flushing the conntrack entries of a moved client.
- IPv6.
- Moving several clients at once.
