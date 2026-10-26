# Firewall Rebuild Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep every protected client off the WAN while the firmware's firewall rebuild has deleted
`TUN_DIR` and `XRAY_TPROXY`, and shorten that window with a snapshot the hooks restore.

**Architecture:** A new shared layer (`lib/guard.sh`) owns per-client `ip rule` guards (by source
address and LAN interface) after the fwmark rules, and a "direct" fwmark rule for exclusions. Both
chains set a direct-mark bit on what they let go direct and, on KeeneticOS, keep every protected flow
off the fast path. Every successful apply snapshots our rules (`lib/fwsnap.sh`); the firmware hooks
restore the snapshot synchronously (`lib/fwsnap-restore.sh`, POSIX sh) before queuing the full apply.

**Tech Stack:** bash 4 (sourced libraries), POSIX sh (hooks, restore script), iptables 1.4.21,
iproute2 4.4 (`ip-full`), ipset, Bats with bats-assert.

**Spec:** `docs/superpowers/specs/2026-10-26-firewall-rebuild-guard-design.md`

## Global Constraints

- Normal operation (chains in place) routes every client and destination exactly as today (spec 5.1).
- Rule preferences, with `field_max = TUN_DIR_MARK_MASK >> TUN_DIR_MARK_SHIFT` (255 by default):
  direct rule at `TUN_DIR_PREF_BASE + 2*field_max` (16894), Tunnel Director guards at the next two
  (16895, 16896), Xray guards at the two after (16897, 16898); all below 32766.
- Direct mark: `advanced.tunnel_director.direct_mark`, default `0x200`; exactly one bit, outside
  `TUN_DIR_MARK_MASK` and outside `XRAY_FWMARK_MASK`. Direct rule: `fwmark 0x200/0x200 lookup main`.
- A guard carries `from <client>` and `iif <lan>` for each of `platform_lan_ifaces`; `/32` is written
  without the suffix, as iproute2 prints it.
- Xray guard private ranges, in this order: `10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16
  224.0.0.0/4`.
- Where `platform_tunnel_offload_target` names a target (KeeneticOS: `PPE`), it applies to every flow of
  every Tunnel Director client (`main`'s included) and every Xray client's packet that `XRAY_TPROXY` lets
  through.
- Make before break: no moment without a guard for a protected client; no flush of a live chain
  (`$BATS_IPT_DIR/live_flushes` stays empty in apply tests).
- Shell conventions (`.claude/rules/shell-conventions.md`): libraries start `#!/usr/bin/env bash` and
  `set -euo pipefail`; `[[ ]]`; `log -l LEVEL`; no jq regex builtins; `ip rule show` read whole, never
  piped into `grep -q`, never `ip rule show pref N`; hooks and `lib/fwsnap-restore.sh` are POSIX sh with
  no `command` builtin (Merlin's BusyBox).
- Core modules never test the platform name.
- Tests run under `VPD_PLATFORM=merlin` by default; Keenetic tests export `VPD_PLATFORM=keenetic`.
- Commits: conventional English subjects as in `git log` (`feat(tunnel): ...`, `test: ...`,
  `docs: ...`), no trailer.
- Full suite: `bats router/test/*.bats router/test/unit router/test/integration`.

## Review Focus

- A client CIDR that holds the router's own LAN address (`192.168.1.0/24` with the router at
  `192.168.1.1`): the router's own traffic and traffic to the router must never meet a guard. Pinned in
  Task 3 ("every guard names a LAN interface").
- `exclude` entries that are IPv6 or that iptables would misread (`2001:db8::1`, `203.0.113.010`): no
  guard for them, and the apply still succeeds. Pinned in Task 3.
- Paused clients (`paused_clients`) get no guard of either kind. Pinned in Tasks 3 and 5.
- NDM calls the `netfilter.d` hook for `mangle` several times in one rebuild (6 in one interface cycle
  on 5.1.5): the restore must leave exactly one jump per chain and interface. Pinned in Task 7.
- A router without `iptables-restore`, or one whose restore fails: the hook still queues the apply and
  exits 0. Pinned in Task 7.

---

### Task 1: The guard layer and the direct-mark setting

**Files:**
- Create: `router/opt/vpn-director/lib/guard.sh`
- Create: `router/test/unit/guard.bats`
- Modify: `router/opt/vpn-director/lib/config.sh` (section 7 and the readonly list)
- Modify: `router/opt/vpn-director/vpn-director.json.template` (`advanced.tunnel_director.direct_mark`)
- Modify: `router/opt/vpn-director/vpn-director.sh` (`_load_modules`: source `lib/guard.sh` right after
  `lib/config.sh`)
- Modify: `router/files.manifest` (`common   router/opt/vpn-director/lib/guard.sh` after `firewall.sh`)
- Modify: `router/test/mocks/ip` (`from`, `to`, `iif` selectors)
- Modify: `router/test/test_helper.bash` (`load_tunnel_module`, `load_tproxy_module` source
  `$LIB_DIR/guard.sh` after `load_config`)
- Modify: `router/test/unit/tunnel.bats` (`load_tunnel_module_with` sources `$LIB_DIR/guard.sh` after
  `firewall.sh`), `router/test/unit/tproxy.bats` (`load_tproxy_module_with_clients` sources it after
  `config.sh`)
- Modify: `router/test/config.bats`
- Modify: `.claude/rules/tunnel-director.md` (Advanced Configuration table: `direct_mark`)

**Interfaces:**
- Produces:
  - `TUN_DIR_DIRECT_MARK` (config.sh, readonly; `0x200` when the key is missing). `guard.sh` uses
    `${TUN_DIR_DIRECT_MARK:-0x200}` so tests that skip config.sh work.
  - `guard_direct_pref` → prints the direct rule's preference.
  - `guard_owner_prefs tunnel|xray` → prints the owner's two preferences on one line, `16895 16896` /
    `16897 16898` with the defaults; rc 1 for another owner.
  - `guard_direct_mark_hex` → prints the mark as lowercase hex (`0x200`).
  - `guard_layout_check` → rc 0, or rc 1 with an ERROR naming `direct_mark` or the preference. The Xray
    mask is checked only when `XRAY_FWMARK_MASK` is set (Tunnel Director tests run without it).
  - `guard_direct_ensure` → rc 0 once `<direct>: from all fwmark 0x200/0x200 lookup main` is listed
    (added only when missing); rc 1 with an ERROR when the add fails.
  - `guard_direct_release` → removes the direct rule only when neither `$TUN_DIR_CHAIN` nor `$XRAY_CHAIN`
    exists in mangle (`fw_chain_exists`) and neither owner's preferences hold a rule; rc 0.
  - `guard_sync <owner> <lines>` → `<lines>`: newline-separated rule selectors in match order, e.g.
    `from 192.168.1.5 iif br0 lookup wgc1`. Afterwards the owner's preferences hold exactly these, in
    order; rc 0, or rc 1 with an ERROR per refused rule.
  - `guard_clear <owner>` → removes every rule at the owner's two preferences; rc 0.
  - `guard_list <owner>` → prints the `ip rule show` lines at the owner's preferences.

- [ ] **Step 1: Extend the `ip` mock.** `_parse_rule_args` also reads `from <addr>`, `to <addr>`,
  `iif <dev>`. `_rule_line` renders in iproute2's order: `from <src|all>`, `to <dst>`, `fwmark <m>`,
  `iif <dev>`, then `lookup <table>` or the action; an address ending in `/32` loses the suffix. `del`
  with any selector matches the whole rendered line, as it does now. Existing output without these
  selectors must not change (every current `tunnel.bats` assertion keeps passing).

- [ ] **Step 2: Write the failing tests** in `router/test/unit/guard.bats` (`load '../test_helper'`,
  `load_tunnel_module` in each):

```bash
@test "guard_owner_prefs: the tunnel's two follow the direct rule, Xray's follow those" {
    assert_equal "$(guard_direct_pref)" 16894
    assert_equal "$(guard_owner_prefs tunnel)" "16895 16896"
    assert_equal "$(guard_owner_prefs xray)" "16897 16898"
}
@test "guard_layout_check: refuses a direct mark inside Tunnel Director's field"   # TUN_DIR_DIRECT_MARK=0x10000 → failure, output has "direct_mark"
@test "guard_layout_check: refuses a direct mark that is Xray's bit"               # XRAY_FWMARK_MASK=0x100, mark 0x100 → failure
@test "guard_layout_check: refuses a direct mark of more than one bit"             # 0x300 → failure
@test "guard_layout_check: refuses guards that would reach main's rule"            # TUN_DIR_PREF_BASE=32000 → failure
@test "guard_direct_ensure: puts the direct rule after the block range, once" {
    guard_direct_ensure; guard_direct_ensure
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(grep -c '^16894:' <<< "$output")" 1
}
@test "guard_sync: installs the lines in order at the owner's first preference" {
    guard_sync tunnel $'from 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main\nfrom 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    run ip rule show
    assert_line --index 1 $'16895:\tfrom 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main'
    assert_line --index 2 $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line --index 3 $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    refute_line --partial '16896:'
}
@test "guard_sync: an unchanged list adds and deletes nothing"                     # second sync: no "ip rule add|del" in /tmp/bats_ip_calls.log after the first
@test "guard_sync: a changed list goes in at the other preference before the old one goes"
    # order log via an ip() wrapper: every "rule add pref 16896" precedes the first "rule del pref 16895";
    # afterwards only 16896 lines, the new ones
@test "guard_sync: a refused rule keeps the old list and returns 1"                # ip() refuses adds naming 192.168.1.6 → rc 1, old lines still listed, ERROR in $LOG_FILE
@test "guard_sync: an empty list clears both preferences"
@test "guard_sync: finishes a list an interrupted sync left half-written"          # 16895: full list A; 16896: first line of B → sync B → B whole at one preference, nothing at the other
@test "guard_sync: a client spelled x/32 is not rewritten on every apply"          # sync "from 192.168.1.5/32 iif br0 prohibit" twice → the second adds nothing
@test "guard_clear: removes the owner's rules and no other"                        # direct rule and a 16639 block rule survive guard_clear tunnel
@test "guard_direct_release: keeps the direct rule while a chain or a guard is there"
    # use_stateful_iptables; iptables -t mangle -N TUN_DIR → rule stays; chain gone, guards there → stays; both gone → removed
@test "guard_list: prints the owner's rules as ip rule show lists them"
```

  In `router/test/config.bats`: `"config.sh: TUN_DIR_DIRECT_MARK defaults to 0x200"` (the fixture has no
  key) and `"config.sh: loads advanced.tunnel_director.direct_mark"` (a temp config with `"0x400"`).

- [ ] **Step 3: Run them to verify they fail**

  Run: `bats router/test/unit/guard.bats router/test/config.bats`
  Expected: FAIL (`guard_direct_pref: command not found`, `TUN_DIR_DIRECT_MARK` unset).

- [ ] **Step 4: Implement `lib/guard.sh` and the setting.** `guard_sync` decides like this (read the whole
  listing once; compare normalized lines - tabs and runs of spaces collapsed, ends trimmed, `/32` dropped
  after `from`/`to`, a `lookup` table as `rt_table_label` prints it; drop duplicate desired lines keeping
  the first):

```
P0, P1 = guard_owner_prefs owner; L0, L1 = rules listed at P0, P1; D = desired lines
if L0 == D: clear P1; return 0
if L1 == D: clear P0; return 0
T, O = (P0, P1) if L0 is empty else (P1, P0)
clear T; add every line of D at T in order (ip rule add pref T <line>), ERROR per refused line
if every line went in: clear O; return 0     # else keep O: the union still guards; return 1
```

  "clear P" deletes rule by rule with its full selector (`ip rule del pref P <selectors>`) as listed, so
  a refusal never takes another rule along. `guard_layout_check` covers the four refusals of Step 2.
  Template: `"direct_mark": "0x200"` after `"mark_shift": 16`.

- [ ] **Step 5: Run the tests to verify they pass**

  Run: `bats router/test/unit/guard.bats router/test/config.bats router/test/unit/tunnel.bats router/test/unit/tproxy.bats`
  Expected: PASS (tunnel and tproxy unchanged by the mock and the loaders).

- [ ] **Step 6: Commit**

```bash
git add router/opt/vpn-director/lib/guard.sh router/opt/vpn-director/lib/config.sh \
  router/opt/vpn-director/vpn-director.json.template router/opt/vpn-director/vpn-director.sh \
  router/files.manifest router/test/mocks/ip router/test/test_helper.bash router/test/unit/guard.bats \
  router/test/unit/tunnel.bats router/test/unit/tproxy.bats router/test/config.bats \
  .claude/rules/tunnel-director.md
git commit -m "feat(guard): ip rule guards and the direct mark, shared by both modules"
```

### Task 2: `TUN_DIR` opts every flow out first and marks exclusions direct

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (`_tunnel_emit_client`, `tunnel_apply`)
- Modify: `router/opt/vpn-director/lib/platform.sh` (contract note on `platform_tunnel_offload_target`)
- Modify: `router/test/unit/tunnel.bats` (the assertions at about lines 1115-1145 and 3180-3190, and new
  tests)
- Modify: `.claude/rules/tunnel-director.md` (Chain Architecture), `.claude/rules/packet-flow.md` (the
  `TUN_DIR chain:` block)

**Interfaces:**
- Consumes: `guard_direct_mark_hex`, `guard_direct_ensure`, `guard_layout_check` (Task 1).
- Produces: per client, in this order (`<D>` the direct mark, `<M>` the mask, `<S>` the slot mark):

```
-s C -m mark --mark 0x0/<M> -j <offload target>            # only when the platform names one
-s C -m set --match-set <set> dst -j MARK --set-xmark <D>/<D>   # per exclusion, in config order;
-s C -m set --match-set <set> dst -j RETURN                     #   an address/CIDR uses -d <excl>
-s C -m mark --mark 0x0/<M> -j MARK --set-xmark <S>/<M>
```

  `_tunnel_emit_client` keeps its return codes (0, 1, 2, 3, 4). A refused rule of an exclusion pair sets
  `warnings` and `incomplete`.
- Produces: the hash input ends with `format: 2` (a module constant `_tunnel_format=2`), so the first
  apply after the upgrade rebuilds in place.

- [ ] **Step 1: Write the failing tests**

```bash
@test "tunnel_apply: the platform's offload target opens the client's block, ahead of its exclusions" {
    load_tunnel_module
    platform_tunnel_offload_target() { printf 'PPE\n'; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    run grep -- '-A TUN_DIR_NEW' /tmp/bats_iptables_calls.log
    assert_line --index 0 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j PPE'
    assert_line --index 1 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m set --match-set ru dst -j MARK --set-xmark 0x200/0x200'
    assert_line --index 2 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN'
    assert_line --index 3 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
}
```

  Replace "the platform's offload target goes immediately before MARK, same match" with it. Update "no
  offload rule when the platform names no target" to the three lines without PPE (count 3) and the
  `192.168.1.30` assertions near line 3185 to the pairs (`-j MARK --set-xmark 0x200/0x200`, then `-j
  RETURN`, for `ru`, `203.0.113.7`, `198.51.100.0/24`). New:
  - `"tunnel_apply: a refused direct mark on an exclusion records no hash"`: an `iptables()` wrapper
    returns 1 for `--set-xmark 0x200/0x200`; `[ ! -f "$TUN_DIR_HASH" ]`, output has "a client rule did not
    go in".
  - `"tunnel_apply: the first apply after the upgrade rebuilds in place although the configuration is the
    same"`: write the old hash (`printf '%s' "$TUN_DIR_TUNNELS_JSON" | compute_hash > "$TUN_DIR_HASH"`),
    a `TUN_DIR_TABLES` row, `fw_chain_exists() { return 0; }`, `marks_in_place`; output has "rebuilding in
    place".
  - `"tunnel_apply: puts the direct rule in before the chain"`: one order log fed by `ip()` and
    `iptables()` wrappers; `ip rule add pref 16894 fwmark 0x200/0x200 lookup main` precedes `-N
    TUN_DIR_NEW`.
  - `"tunnel_apply: refuses a direct mark inside its own field, before touching a rule"`:
    `TUN_DIR_DIRECT_MARK=0x10000` → failure, no `ip rule add` in the call log.

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/tunnel.bats`
  Expected: the new and updated tests FAIL on rule order and the missing direct rule.

- [ ] **Step 3: Implement.** In `tunnel_apply`: `guard_layout_check || return 1` right after the existing
  block-range check; the format line in the hash input; `guard_direct_ensure` before step 1 of a rebuild
  and at the head of the up-to-date path (failure: `warnings=1`, `incomplete=1`, and failover not ready).
  In `_tunnel_emit_client`: the offload rule moves to the head; each exclusion becomes the pair. Rewrite
  the platform.sh contract sentence: the target goes at the head of the client's block, so no flow of a
  Tunnel Director client - excluded destinations included - is accelerated (spec decision 5).

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/unit/tunnel.bats router/test/unit/iftunnel.bats router/test/integration`
  Expected: PASS. An existing up-to-date test that builds its state by hand and asserts that no `ip rule
  add` ran now also sees the direct rule go in once: seed it (`guard_direct_ensure`) in that test rather
  than weaken the assertion.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/opt/vpn-director/lib/platform.sh \
  router/test/unit/tunnel.bats .claude/rules/tunnel-director.md .claude/rules/packet-flow.md
git commit -m "feat(tunnel): every flow off the fast path, exclusions marked direct"
```

### Task 3: Tunnel Director guards

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (`_tunnel_guard_lines`, `tunnel_apply`, `tunnel_stop`,
  `tunnel_status`)
- Modify: `router/test/unit/tunnel.bats`
- Modify: `.claude/rules/tunnel-director.md` (The kill switch; Key Functions)

**Interfaces:**
- Consumes: `guard_sync tunnel`, `guard_clear tunnel`, `guard_direct_release`, `guard_list tunnel`
  (Task 1).
- Produces: `_tunnel_guard_lines <slots_file>` → prints the desired guard lines (`<slots_file>`:
  `<idx> <id>` rows, as `slots_tmp` and `TUN_DIR_TABLES`). Order: the failover clients still on the
  failover tunnel first, under it, then each row's clients in file order, skipping those failover
  clients - the order `_tunnel_build_chain` uses. Skipped: tunnel `main`; a client `is_ipv4_net` or
  `is_lan_ip` rejects. Per client, per LAN interface `L`:

```
from <C> to <X> iif L lookup main          # each exclusion X of the tunnel that is_ipv4_net accepts
from <C> iif L lookup <table>              # vpd_tunnel_table <id> <idx>; omitted when _tunnel_routeless
from <C> iif L prohibit
```

- [ ] **Step 1: Write the failing tests** (`load_tunnel_module_with '<json>'`, `run tunnel_apply`,
  `run ip rule show`):

```bash
@test "tunnel_apply: a client of a tunnel gets its guard pair after the fwmark rules" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
}
```

  And, each with its assertion:
  - `"an address exclusion goes direct ahead of the client's guard"`: exclude
    `["ru","192.168.100.0/24"]` → the three lines in order, none for `ru`.
  - `"a client of main gets no guard"`: `{"main":{"clients":["192.168.1.6"]},"wgc1":{"clients":["192.168.1.0/24"]}}`.
  - `"guards follow the chain's order, the failover clients first"`: `XRAY_FAILOVER_TUNNEL=wgc1`,
    `XRAY_FAILOVER_CLIENTS=192.168.1.7`, `{"wgc2":{"clients":["192.168.1.0/24"]},"wgc1":{"clients":["192.168.1.7"]}}`
    → the `192.168.1.7` lines precede the `/24` lines.
  - `"a tunnel with no table to route through gets the refusing guard alone"`: `wgc9`.
  - `"a client outside RFC1918 gets no guard"`: `["100.64.0.8","192.168.1.5"]`.
  - `"an IPv6 or misread exclusion gets no guard, and the apply succeeds"`: exclude
    `["2001:db8::1","203.0.113.010"]`.
  - `"a paused client gets no guard"`: `load_tunnel_module` with a temp config whose `paused_clients`
    holds the fixture's `192.168.50.0/24`.
  - `"every guard names a LAN interface"`: every guard line matches ` iif br0 `.
  - `"a guard per LAN interface"`: `platform_lan_ifaces() { printf 'br0\nbr1\n'; }` → `iif br0` and
    `iif br1` lines.
  - `"a client the layout dropped loses its guard after the swap"`: two clients, then one; in an order log
    `-E TUN_DIR_NEW TUN_DIR` precedes the `rule del` of the dropped client's guard.
  - `"the up-to-date path puts a missing guard back"`: delete
    `pref 16895 from 192.168.1.5 iif br0 prohibit`, `fw_chain_exists() { return 0; }`, `marks_in_place` →
    "up-to-date", line back.
  - `"a refused guard records no hash and withholds failover_ready"`: `ip()` refuses adds with
    `iif br0`; failover config as in the block-rule test near line 2543.
  - `"tunnel_stop: removes the guards and, with no chain left, the direct rule"`: nothing at 16894-16896.
  - `"tunnel_status: lists the guards"`.

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/tunnel.bats`
  Expected: the new tests FAIL (no `16895:` lines).

- [ ] **Step 3: Implement.** Rebuild: when `jumps_ok` is 1, `guard_sync tunnel "$(_tunnel_guard_lines
  "$slots_tmp")"` right after the swap, before step 3; failure sets `warnings`, `incomplete` and keeps
  `failover_ready` out. Up-to-date path: the same sync with `TUN_DIR_TABLES` after `_tunnel_ensure_routes`;
  failure keeps `failover_ready` out. `tunnel_stop`: `guard_clear tunnel` after the range loop and
  `guard_direct_release` after the chain is deleted. `tunnel_status`: a section `--- Guards (ip rules by
  source) ---` with the direct rule and `guard_list tunnel`. A refused guard is not added to
  `TUNNEL_UNCARRIED`.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/unit/tunnel.bats router/test/unit/iftunnel.bats router/test/integration`
  Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats .claude/rules/tunnel-director.md
git commit -m "feat(tunnel): guards keep tunnel clients in their tunnel when the firmware deletes TUN_DIR"
```

### Task 4: `XRAY_TPROXY` marks what it lets through direct and keeps it off the fast path

**Files:**
- Modify: `router/opt/vpn-director/lib/tproxy.sh` (`_tproxy_build_chain`, `tproxy_apply`)
- Modify: `router/test/unit/tproxy.bats`
- Modify: `.claude/rules/xray-tproxy.md` (Chain Structure), `.claude/rules/packet-flow.md` (the
  `XRAY_TPROXY chain:` block)

**Interfaces:**
- Consumes: `guard_direct_mark_hex`, `guard_direct_ensure`, `guard_layout_check` (Task 1);
  `platform_tunnel_offload_target`.
- Produces: right after rule 1 of `XRAY_TPROXY`: `-j MARK --set-xmark <D>/<D>`, then `-j <offload
  target>` when the platform names one. A refused direct mark returns 2 (the live chain stays); a refused
  offload rule sets `narrow_rc=1`. `tproxy_apply` returns 1 with an ERROR, touching nothing, when
  `guard_layout_check` fails, and calls `guard_direct_ensure` before `_tproxy_setup_iptables` (failure
  withholds the ready marker).

- [ ] **Step 1: Write the failing tests** (`load_tproxy_module`, `use_stateful_iptables`):

```bash
@test "tproxy_apply: every Xray client's packet carries the direct mark right after rule 1" {
    load_tproxy_module
    use_stateful_iptables
    run tproxy_apply
    assert_success
    run iptables -t mangle -S XRAY_TPROXY
    assert_line --index 1 '-A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN'
    assert_line --index 2 '-A XRAY_TPROXY -j MARK --set-xmark 0x200/0x200'
}
```

  And: `"the platform's offload target follows the direct mark"` (index 3 `-A XRAY_TPROXY -j PPE`), `"no
  offload rule when the platform names no target"`, `"puts the direct rule in"` (`16894:` line),
  `"refuses a direct mark that collides with its own"` (`XRAY_FWMARK_MASK=0x200` → failure, no chain).
  Add `"-A XRAY_TPROXY_NEW -j MARK --set-xmark 0x200/0x200"` to the loop of "fails before the TPROXY
  targets when a rule that bounds them fails", and `"-A XRAY_TPROXY_NEW -j PPE"` (with the platform naming
  PPE) to the loop of "a RETURN that only narrows the proxy keeps the TPROXY targets".

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/tproxy.bats`
  Expected: FAIL on index 2.

- [ ] **Step 3: Implement** as in Interfaces.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/unit/tproxy.bats router/test/unit/tunnel.bats router/test/integration`
  Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/tproxy.sh router/test/unit/tproxy.bats \
  .claude/rules/xray-tproxy.md .claude/rules/packet-flow.md
git commit -m "feat(tproxy): mark what Xray lets through direct, off the fast path"
```

### Task 5: Xray guards

**Files:**
- Modify: `router/opt/vpn-director/lib/tproxy.sh` (`_tproxy_guard_lines`, `_tproxy_guards_wanted`,
  `tproxy_apply`, `tproxy_prune`, `tproxy_stop`, `tproxy_status`)
- Modify: `router/test/unit/tproxy.bats`, `router/test/unit/tunnel.bats` (full-apply tests)
- Modify: `.claude/rules/xray-tproxy.md` (Fail-Safe, ready marker, Key Functions)

**Interfaces:**
- Consumes: `guard_sync xray`, `guard_clear xray`, `guard_direct_release`, `guard_list xray` (Task 1).
- Produces:
  - `_tproxy_guards_wanted` → rc 0 when the live `$XRAY_CHAIN` holds the direct-mark rule
    (`iptables -t mangle -C "$XRAY_CHAIN" -j MARK --set-xmark <D>/<D>`). Without it an excluded packet
    would meet the Xray guard and be refused, so no guard is synced (soft-fails, an old chain).
  - `_tproxy_guard_lines` → for each member of `$XRAY_CLIENTS_IPSET` (the lines after `Members:` of
    `ipset list`, sorted), per LAN interface `L`: `from <X> to <r> iif L lookup main` for each private
    range of the Global Constraints, then for each `XRAY_EXCLUDE_IPS` entry `_tproxy_validate_ipv4_cidr`
    accepts, then `from <X> iif L prohibit`.
  - `tproxy_apply`: after `_tproxy_setup_iptables`, when `_tproxy_guards_wanted`, `guard_sync xray`;
    failure withholds the ready marker. `tproxy_prune`: the same sync after a successful swap.
    `tproxy_stop`: `guard_clear xray`, then `guard_direct_release` after the chain is gone.

- [ ] **Step 1: Write the failing tests** (`load_tproxy_module`, `use_stateful_iptables`,
  `use_stateful_ipset`):

```bash
@test "tproxy_apply: an Xray client gets its guards once the chain is in" {
    load_tproxy_module_with_clients '["192.168.1.100"]'   # the fixture's exclude_ips: 5.6.7.8, 10.20.0.0/16
    use_stateful_iptables
    use_stateful_ipset
    run tproxy_apply
    assert_success
    run ip rule show
    assert_line --index 3 $'16897:\tfrom 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main'
    assert_line --index 7 $'16897:\tfrom 192.168.1.100 to 224.0.0.0/4 iif br0 lookup main'
    assert_line --index 8 $'16897:\tfrom 192.168.1.100 to 5.6.7.8 iif br0 lookup main'
    assert_line --index 9 $'16897:\tfrom 192.168.1.100 to 10.20.0.0/16 iif br0 lookup main'
    assert_line --index 10 $'16897:\tfrom 192.168.1.100 iif br0 prohibit'
}
```

  (Index 0 is the mock's `local` rule, then the TPROXY rule at 200 and the direct rule; adjust the
  indexes only if those three are not the lines ahead.) And:
  - `"no Xray guard while the chain is not in place"`: `platform_load_module() { return 1; }` → nothing
    at 16897/16898.
  - `"tproxy_prune: a client that left Xray loses its guards after the swap"`.
  - `"tproxy_prune: a client the prune keeps keeps its guards"`: a named address the live set holds.
  - `"tproxy_apply: a refused guard withholds ready"`.
  - `"tproxy_apply: a paused client gets no guard"`.
  - `"tproxy_stop: removes the Xray guards"`.
  - `"tproxy_status: lists the Xray guards"`.
  - In `tunnel.bats`, `"full apply: a client on Xray and on a tunnel meets the tunnel's guard first"`
    (`load_full_apply` with `192.168.1.5` on Xray too): `16895:` `lookup wgc1` and `16897:` `prohibit`
    lines for it.

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/tproxy.bats router/test/unit/tunnel.bats`
  Expected: the new tests FAIL (no `16897:` lines).

- [ ] **Step 3: Implement** as in Interfaces.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/unit/tproxy.bats router/test/unit/tunnel.bats router/test/integration`
  Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/tproxy.sh router/test/unit/tproxy.bats router/test/unit/tunnel.bats \
  .claude/rules/xray-tproxy.md
git commit -m "feat(tproxy): guards refuse Xray clients instead of leaking them when XRAY_TPROXY is gone"
```

### Task 6: A snapshot of our rules after every apply

**Files:**
- Create: `router/opt/vpn-director/lib/fwsnap.sh`
- Create: `router/test/unit/fwsnap.bats`
- Modify: `router/opt/vpn-director/vpn-director.sh` (`_load_modules` sources `lib/fwsnap.sh` last;
  `fwsnap_save` at the end of each `cmd_apply` branch and of `cmd_update`; `fwsnap_remove` in `cmd_stop`
  right after the stopped marker, for every component)
- Modify: `router/files.manifest` (`common   router/opt/vpn-director/lib/fwsnap.sh`)
- Modify: `router/test/integration/vpn_director.bats`
- Modify: `.claude/rules/tunnel-director.md` (State Tracking table)

**Interfaces:**
- Produces:
  - `FWSNAP_DIR` (`${FWSNAP_DIR:-/tmp/vpn-director/fwsnap}`).
  - `fwsnap_save` → rc 0 always (a failed write is a WARN). Writes, each through a temp file and `mv`:
    - `$FWSNAP_DIR/mangle.rules` when `$XRAY_CHAIN` or `$TUN_DIR_CHAIN` exists, else removes it:

```
*mangle
:XRAY_TPROXY - [0:0]                                   # each chain that exists
:TUN_DIR - [0:0]
-A XRAY_TPROXY ...                                     # every rule, as iptables -S prints it
-A TUN_DIR ...
-I PREROUTING 1 -i br0 -j XRAY_TPROXY                  # the jumps to our chains in listing order,
-I PREROUTING 2 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR   # Xray's first, numbered from 1
-I INPUT 1 -m mark --mark 0x100/0x100 -j ACCEPT        # when "-A INPUT -m mark --mark $XRAY_FWMARK/$XRAY_FWMARK_MASK -j ACCEPT" is listed
-I FORWARD 1 <spec>                                    # each TCPMSS rule naming an interface of IFTUNNEL_STATE_FILE
COMMIT
```

    - `$FWSNAP_DIR/nat.rules`: one `-o <iface> -j MASQUERADE` per interface of `IFTUNNEL_STATE_FILE` whose
      rule nat POSTROUTING lists; removed when there is none.
  - `fwsnap_remove` → removes both files.

- [ ] **Step 1: Write the failing tests** in `router/test/unit/fwsnap.bats` (`load_tunnel_module`,
  `source "$LIB_DIR/tproxy.sh" --source-only`, `source "$LIB_DIR/fwsnap.sh"`, `use_stateful_iptables`,
  `FWSNAP_DIR="$BATS_TEST_TMPDIR/fwsnap"`; the chains filled with `iptables -t mangle -N/-A` directly):
  - `"fwsnap_save: both chains, their jumps in order and the INPUT accept, as restore input"`:
    `assert_equal "$(cat "$FWSNAP_DIR/mangle.rules")" "<the exact text for two rules per chain>"`.
  - `"fwsnap_save: no mangle snapshot when neither chain exists"`.
  - `"fwsnap_save: an interface tunnel's MSS rules go with mangle, its MASQUERADE with nat"`:
    `IFTUNNEL_STATE_FILE` holding `awg1 2000`.
  - `"fwsnap_remove: leaves no snapshot"`.
  - In `vpn_director.bats`: `"stop: removes the snapshot"` (a planted `$FWSNAP_DIR/mangle.rules`, `stop`,
    file gone) and `"apply: writes the snapshot"` (stateful iptables if the file's harness allows,
    otherwise a `fwsnap_save` call recorded through `FWSNAP_DIR` being created).

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/fwsnap.bats router/test/integration/vpn_director.bats`
  Expected: FAIL (`fwsnap_save: command not found`).

- [ ] **Step 3: Implement** `lib/fwsnap.sh` and the calls.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/unit/fwsnap.bats router/test/integration`
  Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/fwsnap.sh router/test/unit/fwsnap.bats \
  router/opt/vpn-director/vpn-director.sh router/files.manifest \
  router/test/integration/vpn_director.bats .claude/rules/tunnel-director.md
git commit -m "feat: snapshot the rules every apply leaves, removed by stop"
```

### Task 7: The hooks restore the snapshot before they queue the apply

**Files:**
- Create: `router/opt/vpn-director/lib/fwsnap-restore.sh` (POSIX sh, `#!/bin/sh`)
- Create: `router/test/mocks/stateful/iptables-restore`, `router/test/mocks/iptables-restore`
- Modify: `router/opt/etc/ndm/netfilter.d/50-vpn-director.sh`, `router/jffs/scripts/firewall-start`,
  `router/jffs/scripts/nat-start`
- Modify: `router/files.manifest` (`common   router/opt/vpn-director/lib/fwsnap-restore.sh`)
- Modify: `router/test/unit/fwsnap.bats`, `router/test/unit/hooks.bats`
- Modify: `.claude/rules/keenetic.md` (Hooks table), `.claude/rules/shell-conventions.md` ("Merlin's
  firewall start empties every mangle chain and deletes none")

**Interfaces:**
- Consumes: the files `fwsnap_save` writes (Task 6).
- Produces: `fwsnap-restore.sh mangle|nat` → always exits 0. Environment, for tests and overrides:
  `FWSNAP_DIR`, `VPD_STOPPED_FILE` (default `/tmp/vpn-director/stopped`), `VPD_TUN_DIR_HASH` (default
  `/tmp/tunnel_director/tun_dir_rules.sha256`). Sets PATH to `/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin`.
  - Nothing when the stopped marker exists or the table's file is missing; any other table: nothing.
  - `mangle`: the chain names are the `:NAME` lines of `mangle.rules`. When `iptables -t mangle -S
    PREROUTING` (captured in a variable, not piped) holds a line ending in `-j <NAME>` for any of them,
    nothing; otherwise `iptables-restore --noflush < mangle.rules`, and on success remove
    `$VPD_TUN_DIR_HASH` and `logger -t vpn-director "Restored the mangle rules from the snapshot"`.
  - `nat`: per line `iptables -t nat -C POSTROUTING <spec> || iptables -t nat -I POSTROUTING 1 <spec>`.
  - `type iptables-restore` failing, or a failed restore: nothing more, exit 0.
- Produces: hooks. `VPD_RESTORE="${VPD_RESTORE:-/opt/vpn-director/lib/fwsnap-restore.sh}"`, run when
  executable, its output discarded, before the apply: `netfilter.d` with `"$table"`, after the existing
  CLI check and before the `nohup` check; `firewall-start` with `mangle`, `nat-start` with `nat`, before
  `--wait apply`.
- Produces: `mocks/stateful/iptables-restore` (on PATH with `use_stateful_iptables`): only `--noflush`;
  `*<table>` starts a table and saves a copy of `$BATS_IPT_DIR/<table>`; `:<CHAIN> ...` flushes a user
  chain that exists or creates it; `-A`/`-I` lines go to the stateful `iptables -t <table>`; `COMMIT`
  ends the table; any failure puts the copy back and exits 1. Every call is logged as `iptables-restore
  $*` to `/tmp/bats_iptables_calls.log`. `mocks/iptables-restore` logs the same and exits 0.

- [ ] **Step 1: Write the failing tests.** `fwsnap.bats` (state built with `tunnel_apply` and
  `tproxy_apply` on the stateful mocks, then `fwsnap_save`; the script run as `run sh
  "$LIB_DIR/fwsnap-restore.sh" mangle` with `VPD_TUN_DIR_HASH="$TUN_DIR_HASH"`):
  - `"restores both chains and their jumps after NDM deleted them"`: jumps deleted and both chains `-F`'d
    and `-X`'d, then the restore; PREROUTING lists `-j XRAY_TPROXY` then the `TUN_DIR` jump at the top,
    the chains hold their rules, `[ ! -f "$TUN_DIR_HASH" ]`.
  - `"refills the chains Merlin's firewall start emptied"`: `iptables -t mangle -F`, restore, chains
    filled, jumps back.
  - `"leaves a table that still holds our jumps alone"`: no `iptables-restore` in the call log.
  - `"a second restore leaves one jump per chain and interface"`: restore twice after one wipe; `grep -c
    -- '-j TUN_DIR$'` on the PREROUTING listing is 1.
  - `"restores nothing after stop"` and `"does nothing without a snapshot"`.
  - `"nat: puts back a missing MASQUERADE, once"`.
  - `"exits 0 when iptables-restore fails or is missing"`.
  - `"the rebuild after a restore converges"`: wipe, restore, `tunnel_apply` again → output has
    "rebuilding", one `TUN_DIR` jump, `[ ! -s "$BATS_IPT_DIR/live_flushes" ]`.
  - `"is POSIX sh and executable in the tree"`: `[ -x ... ]`, first line `#!/bin/sh`, `sh -n`.

  `hooks.bats`: a fake `$VPD_RESTORE` that appends `restore $*` to the file `nohup()` writes to, so one
  log shows the order:
  - `"netfilter.d: restores the table it was called for, then queues the apply"` (`restore mangle` line
    before the `--wait apply` line).
  - `"firewall-start: restores mangle, then queues the apply"`, `"nat-start: restores nat, then queues the
    apply"`.
  - `"hooks: a missing restore script does not stop the apply"`.

- [ ] **Step 2: Run them to verify they fail**

  Run: `bats router/test/unit/fwsnap.bats router/test/unit/hooks.bats`
  Expected: FAIL (no restore script, hooks call nothing).

- [ ] **Step 3: Implement** the script, the mocks and the hook lines.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `bats router/test/*.bats router/test/unit router/test/integration`
  Expected: PASS (the whole suite).

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/fwsnap-restore.sh router/test/mocks/stateful/iptables-restore \
  router/test/mocks/iptables-restore router/opt/etc/ndm/netfilter.d/50-vpn-director.sh \
  router/jffs/scripts/firewall-start router/jffs/scripts/nat-start router/files.manifest \
  router/test/unit/fwsnap.bats router/test/unit/hooks.bats .claude/rules/keenetic.md \
  .claude/rules/shell-conventions.md
git commit -m "feat(hooks): restore the rules from the snapshot before the apply"
```

### Task 8: Documentation

**Files:**
- Modify: `.claude/rules/packet-flow.md`, `.claude/rules/keenetic.md`, `.claude/rules/testing.md`,
  `CLAUDE.md`, `README.md`, `README.ru.md`

- [ ] **Step 1: Write the changes.**
  - `packet-flow.md`: the `ip rule lookup` box gains 16894 (direct), 16895/16896 (Tunnel Director guards),
    16897/16898 (Xray guards); the first point of "Left open" says what a rebuild costs now (tunnel
    clients stay in the tunnel, Xray clients are refused, excluded countries go through the tunnel, the
    hook's restore); "Fwmark Bit Layout" names `0x200` direct; "The firmware fast path" gains the fact of
    spec section 4: the fast path routes a flow it took by `ip rule` (source, destination, iif), never by a
    mark, hence `PPE` on every protected flow.
  - `keenetic.md`: what triggered NDM's rebuilds on the KN-4521 (each WAN DHCP renewal, each OpenVPN
    reconnect, the LTE ping-check), the fast path's routing, the restore in the `netfilter.d` row, and
    that a protected client's direct-marked traffic no longer reaches `_NDM_DNSRT_PREROUTING_MANGLE`
    (spec 5.3).
  - `testing.md`: the `ip` mock's `from`/`to`/`iif`, `mocks/stateful/iptables-restore` and
    `mocks/iptables-restore`, `unit/guard.bats` and `unit/fwsnap.bats` in the tree.
  - `CLAUDE.md`: Architecture rows for `lib/guard.sh`, `lib/fwsnap.sh`, `lib/fwsnap-restore.sh`; the kill
    switch bullet of Tunnel Director names the guards.
  - `README.md` and `README.ru.md`: on KeeneticOS a protected client's traffic to its exclusions is not
    accelerated; during a firmware firewall rebuild Xray clients are briefly refused and a tunnel client's
    excluded-country traffic briefly goes through the tunnel.

- [ ] **Step 2: Check every cited name exists**

  Run: `grep -n "guard_sync\|fwsnap_save\|fwsnap-restore.sh\|direct_mark" CLAUDE.md .claude/rules/*.md README*.md`
  Expected: each name the docs cite is one the code defines (`grep -n` in `router/opt/vpn-director/lib`).

- [ ] **Step 3: Commit**

```bash
git add .claude/rules/packet-flow.md .claude/rules/keenetic.md .claude/rules/testing.md CLAUDE.md README.md README.ru.md
git commit -m "docs: guards, the direct mark and the snapshot restore"
```

### Task 9: Acceptance on the owner's KeeneticOS router

Run by the controller in the main session, never by a subagent, and only after the owner explicitly
allows each router change. Traffic of the owner's own sessions crosses this router.

- [ ] **Step 1: Ask the owner for permission**, naming the files to copy and the rebuild to trigger.
- [ ] **Step 2: Back up and deploy.** On `root@192.168.1.1 -p 222`: `tar czf
  /opt/vpn-director-backup-guard.tgz` of `/opt/vpn-director/lib`, `/opt/vpn-director/vpn-director.sh`,
  `/opt/vpn-director/vpn-director.json` and `/opt/etc/ndm/netfilter.d/50-vpn-director.sh`; copy the
  branch's versions of `vpn-director.sh`, `lib/*.sh` and the hook; add `"direct_mark": "0x200"` only if
  the owner wants it explicit (the default applies without it).
- [ ] **Step 3: Apply and inspect.** `vpn-director.sh apply` logs "rebuilding in place"; `ip rule show`
  holds 16894 and the `192.168.1.10` pair at 16895 (`lookup 2000`, `prohibit`); `iptables -t mangle -S
  TUN_DIR` opens with `-j PPE`; `/tmp/vpn-director/fwsnap/mangle.rules` exists.
- [ ] **Step 4: Repeat the spec's section 4 probes across a real NDM rebuild** (the trigger chosen in
  Step 1), with `tcpdump` on `mitino`'s `enp1s0` for the test addresses. Expected: 0 packets to them
  around the tunnel; keep-alive connections and the slow download survive; `mirror.yandex.ru` downloads
  directly at full rate in normal operation; the hook's `Restored the mangle rules` line, and the time
  from the rebuild to it, recorded.
- [ ] **Step 5: Asuswrt-Merlin, if the owner has a router at hand:** the same steps with
  `service restart_firewall` as the rebuild, checking in addition that its acceleration keeps an excluded
  flow direct and that bit `0x200` carries no firmware mark (`iptables-save -t mangle | grep 0x200`).
- [ ] **Step 6: Report** the measurements to the owner. On any failure, restore the backup and run
  `vpn-director.sh apply`.
