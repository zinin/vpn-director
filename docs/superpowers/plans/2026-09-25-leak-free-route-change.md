# Leak-Free Route Changes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move a LAN client between Xray and a tunnel in one action from the Web UI and the bot, and make every apply, restart and server switch make-before-break, so that no packet that belongs in a VPN leaves through the WAN while routing changes.

**Architecture:** The shell stops tearing routing down to rebuild it. A new helper, `swap_fw_chain`, builds a chain beside the live one and moves the PREROUTING jumps over; sets are swapped instead of flushed; Tunnel Director keeps each tunnel's slot (mark, preference, table) across applies, so a rebuild installs routing first, swaps the chain and releases the dropped slots last. A full apply runs Xray make (add clients), Tunnel Director, then Xray prune (let go of the clients that left). On top of that, `vpnconfig.MoveClient` changes a client's route in one config update, reached through `POST /api/clients/route`, a select in the Web UI's Route column and a 🔀 button in the bot's `/clients`.

**Tech Stack:** bash with iptables and ipset (BusyBox, Entware jq without regex builtins), bats; Go 1.25 (standard library and the existing telegram-bot-api); Vue 3 + TypeScript (Vite, vue-tsc).

**Spec:** `docs/superpowers/specs/2026-09-25-leak-free-route-change-design.md`. Read it before any task; "spec 8.2" below means its section 8.2.

## Global Constraints

- An apply never flushes a chain or a set that a rule currently uses: no `iptables -F` of a live chain, no `ipset flush` of a live set. Only `tproxy_stop` and `tunnel_stop` tear down, and they run for `stop` and for Tunnel Director's no-tunnels branch alone.
- The shadow of a chain or a set is its name plus `_NEW`: `XRAY_TPROXY_NEW`, `TUN_DIR_NEW`, `XRAY_CLIENTS_NEW`, `TPROXY_BYPASS_NEW`. A chain name may have 24 characters at most (iptables allows 28), a set name 27 (ipset allows 31).
- `swap_fw_chain` returns 0 (swapped, every rule in), 1 (swapped, `build_fn` reported a rule missing), 2 (the live chain untouched), 3 (the cutover unfinished: an interface still on the old chain, or the rename failed).
- A full apply (`apply`, `update`, `restart`) runs `tproxy_apply` → `tunnel_apply` → `tproxy_prune`. `apply xray` and `restart xray` run `tproxy_apply` → `tproxy_prune`; `apply tunnel` runs `tunnel_apply` alone. `TUN_DIR_FORCE_REBUILD=1` makes `tunnel_apply` rebuild even when it reads up to date (`restart`, `restart tunnel`).
- `TUN_DIR_TABLES` keeps its format, one `<idx> <id>` line per tunnel; `mark = (idx + 1) << TUN_DIR_MARK_SHIFT`, `pref = TUN_DIR_PREF_BASE + idx`, Keenetic table `2000 + idx`.
- `POST /api/clients/route` takes `{"ip": "...", "route": "..."}` and answers as the other client routes do.
- Bot callbacks: `clients:move:<ip>`, `clients:to:<route>:<ip>`, `clients:toyes:<route>:<ip>`; « Back and Cancel reuse `clients:rm_no`. `/clients` answers in English.
- Copy: the Web UI asks "`<route>` is down: until it is up, this client's traffic goes out through the WAN. Move anyway?" (the Add form: "… Add anyway?"); the bot asks "`<route>` is down: until it is up, `<ip>`'s traffic goes out through the WAN. Move anyway?".
- Shell: a sourced library starts with `#!/usr/bin/env bash`; use `[[ ]]` and `log -l`; no jq regex builtins (`test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub`). A function that runs under `||` or `if !` checks its commands itself: errexit is off there. The callbacks of `swap_fw_chain` run in the caller's dynamic scope; its own locals carry a `_sw_` prefix.
- No new Go or npm dependency.
- The owner's working rules: run Go and npm build and test commands through the `claude-forge:build-runner` agent; run bats and shellcheck directly; the full bats suite takes minutes — run it in the background and read the counts from its log. Never write to the local memory directory. Commit each task on `feature/leak-free-route-change`; do not push without asking.

## Review Focus

1. **Merlin's firewall start empties every mangle chain** (`iptables -t mangle -F`), the PREROUTING jumps with them, and the hook applies again. Both chains must come back whole, with their jumps, and no flush may count as a live one. Tests: Task 2 (`tproxy_apply` after `mangle -F`), Task 3 (`tunnel_apply` after `mangle -F`).
2. **An NDM rebuild deletes our chains while the ip rules, the routes and `TUN_DIR_TABLES` survive.** The rebuild must keep every slot and touch no ip rule of a kept tunnel. Test: Task 3.
3. **Tunnel Director fails after Xray's make half.** The prune must not run: a client that left Xray stays proxied instead of leaking. Test: Task 2 (the CLI).
4. **A tunnel loses its last client** (moved away, paused). Its slot is released after the swap, and every other tunnel keeps its mark, its rule and its table untouched. Test: Task 3.
5. **A paused client stored as `/32`, moved while a committed failover holds it.** One entry lands on the new route, it stays paused, and the failover record lets it go. Test: Task 5.

## File Structure

| Path | Responsibility |
|---|---|
| `router/opt/vpn-director/lib/firewall.sh` | `swap_fw_chain`, `_fw_chain_cutover`: rebuild a chain beside the live one and move the jumps over |
| `router/opt/vpn-director/lib/tproxy.sh` | `_tproxy_build_chain` and the swapped `XRAY_TPROXY`; `TPROXY_BYPASS` by swap; `XRAY_CLIENTS` add-only; `tproxy_prune`; shadows removed on stop |
| `router/opt/vpn-director/lib/tunnel.sh` | Stable slots (`_tunnel_collect_applied`), `_tunnel_build_chain`, `_tunnel_slot_release`, the rebuild in place; shadow removed on stop |
| `router/opt/vpn-director/vpn-director.sh` | The apply order, `restart` without a stop, the help text |
| `router/test/mocks/stateful/iptables` (new) | An iptables mock that remembers chains and rules and records a flush of a live chain |
| `router/test/test_helper.bash` | `use_stateful_iptables` |
| `router/test/firewall.bats`, `router/test/unit/tproxy.bats`, `router/test/unit/tunnel.bats`, `router/test/integration/vpn_director.bats` | Tests |
| `server/internal/vpnconfig/clients.go` (new) | `ClientRoutes`, `MoveClient` |
| `server/internal/webapi/handler_clients.go`, `router.go` | `checkClientRoute`, `handleMoveClient`, the route |
| `server/internal/handler/clients.go` | The 🔀 button, the route keyboard, the move |
| `server/internal/service/vpndirector.go` | The doc comments of the restart calls |
| `web/src/api.ts`, `web/src/components/ClientsTab.vue` | `moveClient`, the Route select, the confirmation for a tunnel that is down |
| `.claude/rules/*.md`, `README.md`, `README.ru.md`, `CLAUDE.md` | Documentation |

Every commit keeps the suites green. Tasks 1–4 change the shell: the helper first, then Xray together with the new apply order, then Tunnel Director, then `restart`. Tasks 5–8 add the move on top of it, and Task 9 documents it.

---

### Task 1: `swap_fw_chain` and a stateful iptables mock

✅ Done — see commit(s): `720751b`, `2681984`

---

### Task 2: Xray — the chain and the sets swapped, clients pruned after Tunnel Director

✅ Done — see commit(s): `6fc1b00`

---

### Task 3: Tunnel Director — stable slots and the rebuild in place

✅ Done — see commit(s): `ea6daed`, `6242428`

---

### Task 4: `restart` rebuilds in place and stops nothing

✅ Done — see commit(s): `ab485cf`

---

### Task 5: `vpnconfig.MoveClient`

✅ Done — see commit(s): `3a64c0a`

---

### Task 6: `POST /api/clients/route`

✅ Done — see commit(s): `6004f70`

---

### Task 7: Web UI — the Route column moves a client

✅ Done — see commit(s): `9e5d42c`

---

### Task 8: Bot — `/clients` moves a client

✅ Done — see commit(s): `ae5d0d7`

---

### Task 9: Documentation

**Files:**
- Modify: `.claude/rules/packet-flow.md`, `.claude/rules/tunnel-director.md`, `.claude/rules/xray-tproxy.md`, `.claude/rules/shell-conventions.md`, `.claude/rules/testing.md`, `.claude/rules/webui.md`, `.claude/rules/telegram-bot.md`
- Modify: `README.md`, `README.ru.md`, `CLAUDE.md`

**Interfaces:** none.

- [ ] **Step 1: `packet-flow.md`**

Insert before `## Fwmark Bit Layout`:

```markdown
## The apply: make before break

A full apply (`apply`, `update`, `restart`) moves every client make-before-break:

1. `tproxy_apply` adds every client `xray.clients` names to `XRAY_CLIENTS` and removes none.
2. `tunnel_apply` puts Tunnel Director's rules in place.
3. `tproxy_prune` swaps in the exact `XRAY_CLIENTS`.

Xray wins over TUN_DIR (above). A client moving from Xray to a tunnel stays proxied until step 3
lets it go, when TUN_DIR already marks it; a client moving the other way is proxied from step 1,
before TUN_DIR lets it go in step 2. A Tunnel Director failure ends the run before step 3: the
clients that left Xray stay proxied rather than leak.

No step flushes a chain or a set a packet is crossing. `XRAY_TPROXY` and a rebuilt `TUN_DIR` are
built as `<chain>_NEW` and swapped in by `swap_fw_chain` (`lib/firewall.sh`): the new jump goes in
ahead of the old one, the old jump and chain go, the new chain takes the name. The sets are built
as `<set>_NEW` and swapped in (`ipset swap`). A Tunnel Director rebuild keeps every tunnel's slot
and installs the routing of a new slot before the swap and releases a dropped one after it
(`tunnel-director.md`). `restart` and `restart xray` stop nothing; while the Xray process
restarts, TPROXY drops what no socket takes.

Left open: the firmware flushing our chains (Merlin's `iptables -t mangle -F` on a firewall start,
an NDM rebuild on KeeneticOS) leaves the clients on the WAN until the hook's apply, and a tunnel
that is down sends its clients to `main`.
```

In `### Position Calculation`, append to the paragraph that starts `` `tunnel_apply` raises `base_pos` ``:

```markdown
A rebuild inserts its jumps to `TUN_DIR_NEW` at those positions, ahead of the old jumps, which go
after (`swap_fw_chain`).
```

In `### Temporary state`, replace the `tun_dir_tables` row with:

```markdown
| `/tmp/tunnel_director/tun_dir_tables` | Applied tunnels, `<idx> <id>` per line (`TUN_DIR_TABLES`); a tunnel keeps its idx while it has clients; during a rebuild, the union of both layouts |
```

- [ ] **Step 2: `tunnel-director.md`**

Replace the bullet `- Order of tunnels in JSON determines fwmark assignment` with:

```markdown
- Order of tunnels in JSON decides which rule a client meets first; a tunnel's fwmark comes from its slot, which it keeps while it has clients (see "Stable slots and the rebuild in place")
```

Insert before `## State Tracking`:

```markdown
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
that dies part-way leaves the next apply a rebuild, which finishes the interrupted swap (step 0 of
`swap_fw_chain`) and releases the slots left on record. A rebuild whose swap did not take over every
interface (`swap_fw_chain` returned 2 or 3) records no hash and releases nothing.
```

In `## State Tracking`:

- In the paragraph that begins `The two can be out of step in one direction`, replace its last two sentences (from `The cleanup that precedes a rebuild` to the end of the paragraph) with:

  ```markdown
  A rebuild reads its previous layout from `TUN_DIR_TABLES` whatever the hash says, so the tunnels on
  record keep their slots and the ones gone from the layout are released.
  ```

- Delete the sentence `Deleting the hash forced the next apply through `tunnel_stop`.`
- Replace `because under errexit one refused MARK used to end the apply after `tunnel_stop` had purged the jumps and the ip rules of every tunnel.` with `because under errexit one refused MARK used to end the apply half-way.`
- Replace the sentence `The PREROUTING jumps are different: a rebuild whose jump did not go in keeps its hash, and the up-to-date branch puts a missing jump back (`_tunnel_jumps_ensure`), leaving one that is there where it is.` with:

  ```markdown
  The PREROUTING jumps: a rebuild whose new jumps did not all go in records no hash, and the next
  apply finishes the swap. A jump that goes missing after a recorded rebuild is put back by the
  up-to-date branch (`_tunnel_jumps_ensure`), which leaves one that is there where it is.
  ```

- Replace the paragraph that begins `The rebuild loop also releases each tunnel's table` with:

  ```markdown
  A rebuild releases the table of a new slot right before it ensures the route
  (`platform_tunnel_table_release`, then `platform_tunnel_route_ensure`): an apply that dies after
  ensuring a route but before recording its slot leaves that route with no record at all, and the
  next apply, handing the index to a tunnel whose route cannot be installed, would otherwise send its
  clients through the previous owner's tunnel. A kept slot's table is never released: it is carrying
  traffic.
  ```

- Add to **Rebuild triggers**: `- `TUN_DIR_FORCE_REBUILD=1` (`restart`, `restart tunnel`)`.

In `## Key Functions`, replace the `tunnel_apply()` row with `| `tunnel_apply()` | Apply rules from config (idempotent; a rebuild happens in place) |` and add to the internal functions table:

```markdown
| `_tunnel_collect_applied(prev)` | The slot of every tunnel of the new layout: kept from `prev`, or the lowest idx neither layout holds |
| `_tunnel_build_chain(chain)` | `swap_fw_chain`'s build_fn: every client's rules, the failover clients first |
| `_tunnel_slot_release(idx, tunnel)` | Drop the ip rule (ours only) and the table of a slot the layout dropped |
```

In `## Dependencies`, replace the `lib/firewall.sh` line with:

```markdown
From `lib/firewall.sh`: `swap_fw_chain`, `delete_fw_chain`, `ensure_fw_rule`, `sync_fw_rule`, `purge_fw_rules`, `find_fw_rules`, `fw_chain_exists`
```

- [ ] **Step 3: `xray-tproxy.md`**

Replace the `## Usage via CLI` block and the paragraph under it with:

````markdown
## Usage via CLI

```bash
vpn-director.sh status xray       # Show Xray TPROXY status
vpn-director.sh restart xray      # Restart the Xray process and apply TPROXY again in place
vpn-director.sh restart xray-process  # Restart the Xray process only, TPROXY rules kept
vpn-director.sh apply             # Apply all (including TPROXY)
```

`restart xray` restarts the process, then applies the TPROXY rules in place (`tproxy_apply`,
`tproxy_prune`): nothing is stopped, so a client meets a restarting Xray and waits — TPROXY drops
what no socket takes. The Web UI server switch, `/xray` and the wizard use it; its apply also swaps
in `TPROXY_BYPASS`, whose `xray.servers` a switch may have recomputed. `restart xray-process`
restarts the process alone, for a caller whose only change is `config.json` — the subscription
watch trying one server after another.
````

Insert before `## Outbound Generation`:

```markdown
## Applying without a window

`tproxy_apply` flushes nothing a packet is crossing:

- `XRAY_TPROXY` is built as `XRAY_TPROXY_NEW` on every apply (`_tproxy_build_chain`) and swapped in
  by `swap_fw_chain`. Flushing the live chain and refilling it one rule per call left every Xray
  client unproxied — out through the WAN — for the length of the refill, on every apply.
- `TPROXY_BYPASS` is built as `TPROXY_BYPASS_NEW` and swapped in (`ipset swap`).
- `XRAY_CLIENTS` is only added to. `tproxy_prune`, which a full apply runs after `tunnel_apply`,
  swaps in the exact set: a client moving to a tunnel stays proxied until Tunnel Director carries
  it (`packet-flow.md`, "The apply: make before break").
- A name from `advanced.xray.chain` leaves room for `_NEW` (24 characters at most), and so does
  one from `advanced.xray.clients_ipset` or `bypass_ipset` (27).
```

In the Public API table, replace the `tproxy_apply()` and `tproxy_stop()` rows with:

```markdown
| `tproxy_apply()` | The make half of an apply: routing, sets, chain swapped in; clients added, never removed; soft-fail if unavailable |
| `tproxy_prune()` | The break half: `XRAY_CLIENTS` swapped to exactly `xray.clients`; always returns 0 |
| `tproxy_stop()` | Remove chain and routing (an interrupted swap's shadows included) |
```

In the internal functions table, replace the rows of `_tproxy_setup_clients_ipset()`, `_tproxy_setup_bypass_ipset()` and `_tproxy_setup_iptables()` with:

```markdown
| `_tproxy_setup_clients_ipset()` | Create the client ipset and add every effective client (never removes) |
| `_tproxy_shadow_set(name)` | An empty `<name>_NEW` to fill and swap in |
| `_tproxy_swap_set(name)` | `ipset swap` of `<name>_NEW` into `<name>`, then destroy the shadow |
| `_tproxy_setup_bypass_ipset()` | Build `TPROXY_BYPASS_NEW` from the three sources and swap it in |
| `_tproxy_build_chain(chain)` | Fill a fresh chain with the TPROXY rules: 0, 1 (a narrowing RETURN missing), 2 (a bounding RETURN or a target missing) |
| `_tproxy_jump_pos()` | Position of the first jump: 1 |
| `_tproxy_setup_iptables()` | Platform rules, then `XRAY_TPROXY` swapped in with its jumps |
```

In the **Ready marker** paragraph, replace the sentence that begins `The chain flush, rule 1 (`! XRAY_CLIENTS`) or a private-range RETURN that does not go in ends it before the TPROXY targets instead` (through `the rest of the LAN.`) with:

```markdown
Rule 1 (`! XRAY_CLIENTS`), a private-range RETURN or a TPROXY target that does not go in keeps the
rebuilt chain out (`_tproxy_build_chain` returns 2): the chain already in place keeps working and
the marker is withheld. Without those rules the targets would take every LAN client, or traffic to
the router and the rest of the LAN.
```

and replace `A PREROUTING jump that does not go in withholds the marker as well: `sync_fw_rule` returns 1 for an insert the kernel refused.` with:

```markdown
A PREROUTING jump that does not go in withholds the marker as well: `swap_fw_chain` returns 3, and
that interface stays on the old chain until the next apply finishes the swap.
```

- [ ] **Step 4: `shell-conventions.md` and `testing.md`**

In `shell-conventions.md`, add to the `## Firewall Utilities (firewall.sh)` table, after the `sync_fw_rule` row:

```markdown
| `swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...` | Rebuild a chain PREROUTING jumps to as `<chain>_NEW` and swap it in; 0 done, 1 done with a rule missing, 2 live chain untouched, 3 cutover unfinished |
```

In the Merlin firewall-start pitfall, replace `refill the chain on every apply (`_tproxy_setup_iptables` flushes and rebuilds `XRAY_TPROXY`)` with `rebuild the chain on every apply (`_tproxy_setup_iptables` builds `XRAY_TPROXY_NEW` and swaps it in)`. Then add a pitfall after that section:

```markdown
### An apply never flushes a live chain or set

**Problem**: `create_fw_chain -f` on a chain the PREROUTING jumps lead to, `ipset flush` on a set a
rule matches, and `tunnel_stop` ahead of a rebuild each opened a window in which packets crossed an
empty chain or set. `XRAY_TPROXY` returned every Xray client to the WAN for the length of its
refill, on every apply; a Tunnel Director rebuild sent every tunnel client there for seconds.

**Solution**: build beside the live object and swap it in. `swap_fw_chain` (`lib/firewall.sh`)
fills `<chain>_NEW` through a callback, inserts its jumps ahead of the old ones, deletes the old
jumps and chain and renames the new chain; `ipset swap` replaces a set's contents in one step. Order
does the rest: add before remove (`tproxy_apply` only adds clients, `tproxy_prune` removes them
after `tunnel_apply`). Only `tproxy_stop` and `tunnel_stop` tear down.

The stateful iptables mock (`use_stateful_iptables`) appends every flush of a chain a rule still
jumps to to `$BATS_IPT_DIR/live_flushes`; a test of an apply asserts that file stays empty.
```

In `testing.md`, add to the Test Helpers table, after the `load_tproxy_module` row:

```markdown
| `use_stateful_iptables` | For the rest of the test, `mocks/stateful/iptables` ahead of the stateless mock: chains and rules kept under `$BATS_IPT_DIR`, a flush of a live chain appended to `$BATS_IPT_DIR/live_flushes`; call it after the `load_*` helper |
```

and to the mocks list, after `- **iptables/ip6tables**: Tracks rule operations`:

```markdown
- **stateful/iptables**: Remembers chains and rules per table under `$BATS_IPT_DIR` (`-S`, `-N`, `-F`, `-X`, `-E`, `-A`, `-I`, `-D`, `-C`); records a flush of a chain a rule still jumps to. Turned on per test by `use_stateful_iptables`
```

- [ ] **Step 5: `webui.md` and `telegram-bot.md`**

In `webui.md`, add to the API table, after the `/api/clients/pause`, `/api/clients/resume` row:

```markdown
| POST | `/api/clients/route` | Move a client: `{ip, route}`, the route checked as for an add; one config update (`vpnconfig.MoveClient`) and one apply; 404 when no route holds the address, 200 with no write and no apply when the route already holds it alone |
```

and append to the paragraph that begins `Client and exclusion mutations go through `updateAndApply``:

```markdown
A move is one such change: its apply takes the client off its old route only once the new one
carries it (`packet-flow.md`), where a delete and an add left it on the WAN in between.
```

In `telegram-bot.md`, add to the Commands table, after the `/cancel` row:

```markdown
| `/clients` | `ClientsHandler.HandleClients` | LAN clients: pause, resume, move (🔀) and remove, and add |
```

Insert before `## Server switch (`/xray`)`:

```markdown
## Clients (`/clients`)

Each client gets ⏸/▶, 🔀 (`clients:move:<ip>`) and 🗑; an address no route can take (an IPv6 entry
an older build saved) gets no 🔀. 🔀 replaces the list with the routes (`routeChoices`, which the
Add flow's keyboard uses too): xray, the platform's tunnels with their description and "(down)",
the config's tunnels the platform does not list ("(unknown)" when the platform answered); ✓ on the
current route; « Back (`clients:rm_no`). A route button (`clients:to:<route>:<ip>`) runs
`vpnconfig.MoveClient` under the config lock and one `Apply`, and the message shows the list again.
A tunnel that is down asks first — "Move anyway" (`clients:toyes:<route>:<ip>`) or Cancel. A button
whose client is gone, or whose route the router no longer has, redraws the list.
```

In the watch paragraph, replace `it still records the config hash so the next apply does not `tunnel_stop` (which would take TUN_DIR down for every client).` with `it still records the config hash, so the next apply does not rebuild for nothing.` and replace `The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between.` with `The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries and changes nothing else, and `restart xray` would apply the TPROXY rules again each time for nothing.`

- [ ] **Step 6: `README.md`, `README.ru.md`, `CLAUDE.md`**

`README.md`:
- In `## Commands`: `restart             # Restart all` → `restart             # Rebuild all in place (nothing stopped)`; `restart xray        # Restart Xray TPROXY only` → `restart xray        # Restart Xray, TPROXY applied again in place`.
- In the Web UI features table, the Clients row: `| **Clients** | LAN client routing: add, change the route in place, pause/resume, delete |`.
- In the bot commands table: `| `/clients` | Manage VPN clients: move between routes, pause, remove |`.
- Insert before `### Country IPSets`:

  ```markdown
  ### Changing a client's route

  Pick another route for a client in the Web UI (the Route column) or in the bot (`/clients`, 🔀).
  It is one change and one apply, and the apply moves the client make-before-break: the client stays
  on its old route until the new one carries it, so none of its traffic leaves through the WAN in
  between. Every apply works that way — chains and sets are built beside the live ones and swapped
  in — and so does a server switch, which restarts Xray with its rules in place.

  What remains: the firmware's own firewall rebuilds (a firewall restart on Merlin, an NDM rebuild on
  KeeneticOS) empty the chains until the hook applies them again, and a tunnel that is down sends its
  clients through the WAN (on Merlin, the VPN client's killswitch prevents that). The Web UI and the
  bot ask before they move a client to a tunnel that is down.
  ```

`README.ru.md`:
- `restart             # Перезапустить всё` → `restart             # Пересобрать всё на месте, без остановки`; `restart xray        # Перезапустить только Xray TPROXY` → `restart xray        # Перезапустить Xray, правила TPROXY применяются на месте`.
- The Clients row: `| **Clients** | Маршруты LAN-клиентов: добавление, смена маршрута на месте, пауза/возобновление, удаление |`.
- `/clients`: `| `/clients` | Управление VPN-клиентами: смена маршрута, пауза, удаление |`.
- Insert before `### Country IPSets`:

  ```markdown
  ### Смена маршрута клиента

  Маршрут клиента меняется в Web UI (колонка Route) или в боте (`/clients`, кнопка 🔀). Это одно
  изменение и один apply, и apply переносит клиента по принципу make-before-break: клиент остаётся на
  старом маршруте, пока его не подхватит новый, поэтому в промежутке ни один его пакет не уходит в WAN.
  Так работает любой apply — цепочки и наборы собираются рядом с действующими и подменяются, — и так же
  переключается сервер: Xray перезапускается, а его правила остаются на месте.

  Что остаётся: собственные пересборки firewall прошивки (рестарт firewall на Merlin, пересборка NDM
  на KeeneticOS) очищают цепочки, пока хук не применит их заново, а упавший туннель отправляет своих
  клиентов в WAN (на Merlin этого не будет, если у VPN-клиента прошивки включён killswitch). Web UI и
  бот спрашивают подтверждение, прежде чем перенести клиента на упавший туннель.
  ```

`CLAUDE.md`, in `## Commands`: `restart             # Restart all` → `restart             # Rebuild all in place, nothing stopped`; `restart xray        # Restart Xray TPROXY only` → `restart xray        # Restart the Xray process, TPROXY applied again in place`.

- [ ] **Step 7: Check the docs**

Run: `grep -rn "Order of tunnels in JSON determines\|flushes and rebuilds .XRAY_TPROXY\|Restart (stop + apply)" .claude/rules/ README.md README.ru.md CLAUDE.md router/opt/`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add .claude/rules/ README.md README.ru.md CLAUDE.md
git commit -m "docs: route changes and applies without a window"
```

---

### Task 10: Final verification and the device check

**Files:** none new; `docs/superpowers/` leaves the branch in Step 6.

- [ ] **Step 1: The Go suite, vet and format**

Run (through `claude-forge:build-runner`): `cd server && go vet ./... && go test ./... -count=1 && go test -race ./internal/webapi/ ./internal/handler/ ./internal/vpnconfig/ -count=1 && gofmt -l .`
Expected: PASS; `gofmt -l` lists no file this branch touched.

- [ ] **Step 2: Leftovers**

Run:

```bash
grep -rn "_tunnel_sync_jumps\|create_fw_chain -q -f mangle \"\$TUN_DIR_CHAIN\"\|ipset flush \"\$XRAY_CLIENTS_IPSET\"" router/opt/
grep -rn "create_fw_chain -f mangle \"\$XRAY_CHAIN\"" router/opt/
```

Expected: no output.

- [ ] **Step 3: The shell suites and shellcheck**

Run in the background and read the counts from the log: `cd router/test && bats -r . > "$TMPDIR/bats.log" 2>&1; tail -5 "$TMPDIR/bats.log"` (with `TMPDIR` the session's scratchpad directory).
Expected: `0 failures`.
Run: `shellcheck router/opt/vpn-director/*.sh router/opt/vpn-director/lib/*.sh`
Expected: nothing new against `master`.

- [ ] **Step 4: Builds**

Run (through `claude-forge:build-runner`): `cd web && npm run build`, then from the repository root `make build-webui && make build-all`.
Expected: both daemons build for every target; the SPA is embedded.

- [ ] **Step 5: The Web UI in dev mode**

Repeat Task 7 Step 4 against the embedded build (`cd server && go run ./cmd/webui --dev`). Stop the server.

- [ ] **Step 6: Remove the plan documents from the branch**

The owner's rule: `docs/superpowers/` must not be in the pull request's diff; the documents stay in the branch history.

```bash
git rm -r docs/superpowers/specs/2026-09-25-leak-free-route-change-design.md \
          docs/superpowers/plans/2026-09-25-leak-free-route-change.md
git commit -m "chore: remove superpowers docs from feature branch"
```

(Only these two files are tracked under `docs/superpowers/` on this branch; the other files there are the owner's untracked notes and stay untouched.)

- [ ] **Step 7: The device check — only with the owner's explicit permission**

Ask the owner first. With a yes, on the RT-AX86U (Merlin) and on the KN-4521 (Keenetic), spec 12:
1. Install the build.
2. Check the one new iptables operation on the firmware's iptables: `iptables -t mangle -N VPD_T; iptables -t mangle -I PREROUTING 1 -i br0 -p icmp -j VPD_T; iptables -t mangle -E VPD_T VPD_U; iptables -t mangle -S PREROUTING | grep VPD_U` prints the jump under the new name; then remove it: `iptables -t mangle -D PREROUTING -i br0 -p icmp -j VPD_U; iptables -t mangle -X VPD_U`.
3. Pick a LAN test client `<client>` and put a counter without a target at the top of `FORWARD`: `iptables -I FORWARD 1 -s <client> -d 1.1.1.1 -o <wan>` (`<wan>` from `vpn-director.sh platform`; add it again after a firmware rebuild of the table). On the client, loop `curl -m 2 https://1.1.1.1` and a DNS query to `1.1.1.1` every 100 ms — TCP and UDP, since ICMP from Xray clients goes direct by design.
4. Move the client from xray to a tunnel, back to xray, and between two tunnels; switch the server in the Web UI; run `restart`, `restart xray` and an `apply` that changes nothing. After each: `iptables -L FORWARD 1 -v -n` shows 0 packets on the counter; `iptables -t mangle -S PREROUTING` lists the Xray jumps, then the `TUN_DIR` jumps (behind the firmware's iface-mark rules on Merlin); `iptables -t mangle -S | grep _NEW` and `ipset list -n | grep _NEW` print nothing; `cat /tmp/tunnel_director/tun_dir_tables` keeps the idx of every tunnel that kept clients, and `ip rule` shows their rules untouched.
5. On a router still running the release before this change, the same moves and switch make the counter grow — the check catches a leak.
6. Remove the counter: `iptables -D FORWARD -s <client> -d 1.1.1.1 -o <wan>`.

- [ ] **Step 8: Ask before pushing**

Report the results and ask the owner whether to push `feature/leak-free-route-change` and open the pull request.
