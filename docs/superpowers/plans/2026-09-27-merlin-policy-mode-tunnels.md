# Merlin Policy-Mode Tunnels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On Asuswrt-Merlin, Tunnel Director neither offers nor routes through an OpenVPN client whose "Redirect Internet traffic through tunnel" is not "VPN Director (policy rules)", and it takes out the default route it left in such a client's table in "No" mode.

**Architecture:** Every firmware fact stays in `lib/platform/merlin.sh`. One predicate, `_merlin_tunnel_routable`, filters `platform_tunnels`, makes `platform_tunnel_route_ensure` refuse and gates the cleanup. A new contract function, `platform_tunnel_unlisted_reason`, names the reason, and `lib/tunnel.sh` prints it in its two warnings without testing the platform. KeeneticOS gets the new function as a stub that returns 1. Nothing under `server/` or `web/` changes.

**Tech Stack:** Bash 5 (Entware's on the router), bats-core with bats-support and bats-assert, shellcheck 0.11, the `nvram`, `ip` and `iptables` mocks under `router/test/mocks`.

**Spec:** `docs/superpowers/specs/2026-09-27-merlin-policy-mode-tunnels-design.md` (read it first: section 2 explains the firmware behaviour every task relies on).

## Global Constraints

- A routable OpenVPN client has `vpn_clientN_rgw` = `2`. `0`, `1` and an empty value are refused. "Mode 0" means `0` or an empty value (the firmware reads an empty value as 0).
- `wgcN`, `main` and KeeneticOS behave as they do today. Nothing under `server/` or `web/` changes.
- Only `_merlin_tunnel_routable` states the rule: no other code compares `vpn_clientN_rgw` with `2`.
- The cleanup command is exactly `ip route del default dev tun1N table ovpncN`, its failure ignored, run only for an `ovpncN` in mode 0 and never in mode 1.
- The reason line is exactly `OpenVPN client N is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "<setting>")`, where `<setting>` is `No` for `0` or an empty value, `Yes (all)` for `1` and the raw value otherwise.
- The warnings are exactly `Tunnel '<id>' is skipped: <reason>` and `Tunnel '<id>': route not installed: <reason>; Tunnel Director does not route through it`. Without a reason, today's texts stay word for word.
- Platform functions never call `log`: `configure.sh` loads the contract without `common.sh`. The core never tests `VPD_PLATFORM`.
- Shell style (`.claude/rules/shell-conventions.md`): `[[ ]]`, `local` for every function variable, no `tr '[:upper:]'`, never `cmd | grep -q` on a live listing.
- shellcheck stays at 53 findings: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l` prints `53`.
- Run single bats files directly. Run the whole suite (`bats router/test/*.bats router/test/unit router/test/integration`) in the background, its output in a log file, and read the counts from the log.
- Stage files by name. Never touch or stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, the other files under `docs/superpowers/plans/`, `review.diff` or `test_exit.sh`: they are the owner's.
- Commit messages follow the house style: `type(scope): subject`, a blank line, one plain paragraph, no trailers.

## Review Focus

1. A client switched from VPN Director to "No" while the config stays, followed by `restart` or `restart tunnel` (`TUN_DIR_FORCE_REBUILD=1`): the rebuild must drop the recorded slot, its ip rule and its default. Pinned in Task 4.
2. `vpn-director.sh stop` with a recorded `ovpncN` in "No" mode: `tunnel_stop` must take the default out. Pinned in Task 2.
3. A failover tunnel whose client is in "No" mode: no slot and no `failover_ready`, so the watch keeps Xray membership. Pinned in Task 4.
4. An nvram that answers nothing for any key: `vpn-director.sh platform` lists the WireGuard tables and no OpenVPN client, and exits 0. Pinned in Task 1.
5. A recorded client in "Yes (all)" on the up-to-date branch: the firmware's default is neither replaced nor deleted. Pinned in Task 2.

## File Structure

| File | Change | Task |
|---|---|---|
| `router/opt/vpn-director/lib/platform/merlin.sh` | `_merlin_ovpn_rgw`, `_merlin_tunnel_routable`, a filtered `platform_tunnels` (1); `_merlin_drop_default`, a refusing `platform_tunnel_route_ensure`, a cleaning `platform_tunnel_table_release` (2); `platform_tunnel_unlisted_reason` (3) | 1, 2, 3 |
| `router/opt/vpn-director/lib/platform/keenetic.sh` | `platform_tunnel_unlisted_reason`, returning 1 | 3 |
| `router/opt/vpn-director/lib/platform.sh` | the contract header | 3 |
| `router/opt/vpn-director/lib/tunnel.sh` | the reason in two warnings; comments; the header | 4 |
| `router/test/mocks/nvram` | `vpn_client1_rgw` and `vpn_client2_rgw` answer `2` | 1 |
| `router/test/unit/platform_merlin.bats` | the `with_ovpn_modes` helper and tests | 1, 2, 3 |
| `router/test/unit/platform_keenetic.bats` | one test | 3 |
| `router/test/unit/tunnel.bats` | the `ovpn_mode` and `tun11_addressed` helpers and end-to-end tests | 2, 4 |
| `router/test/integration/vpn_director.bats` | two `platform` tests | 1 |
| `CLAUDE.md`, `.claude/rules/tunnel-director.md`, `README.md`, `README.ru.md` | documentation | 5 |

How the tests see the firmware: `test_helper.bash` sets `VPD_PLATFORM=merlin`, puts `router/test/mocks` first in `PATH` and points `RT_TABLES_FILE` at `router/test/fixtures/rt_tables`, which names `wgc1`, `wgc2`, `ovpnc1` and `ovpnc2`. The `ip` mock appends every call, as `ip <args>`, to `/tmp/bats_ip_calls.log`, keeps the ip rules it was given in `/tmp/bats_test_ip_rules`, and answers `ip -4 -o addr show <iface>` from `$BATS_IP_ADDRS_FILE` (`<iface> <cidr>` per line) when that is set. `log` prints to stderr, which `run` captures, and appends to `$LOG_FILE`.

---

### Task 1: Merlin lists an OpenVPN client only in VPN Director mode

**Files:**
- Modify: `router/test/mocks/nvram`
- Modify: `router/opt/vpn-director/lib/platform/merlin.sh:44-51` (`platform_tunnels`)
- Test: `router/test/unit/platform_merlin.bats`
- Test: `router/test/integration/vpn_director.bats`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `_merlin_ovpn_rgw <ovpncN>`: prints `vpn_clientN_rgw` (`""` when unset), always rc 0.
  - `_merlin_tunnel_routable <id>`: rc 0 when Tunnel Director may route through `<id>` (an `ovpncN` whose `vpn_clientN_rgw` is `2`, every `wgcN`, `main`, any other id); rc 1 for any other `ovpncN`. Prints nothing.
  - `with_ovpn_modes <client 1 rgw> <client 2 rgw>` in `platform_merlin.bats`: replaces `nvram` with a mock that answers only those two keys (`""` for everything else).

- [ ] **Step 1: Give the nvram mock the two redirect modes**

In `router/test/mocks/nvram`, add two lines after `"get vpn_client5_addr") echo "" ;;`:

```bash
    "get vpn_client1_rgw") echo "2" ;;
    "get vpn_client2_rgw") echo "2" ;;
```

Both clients the fixture `rt_tables` names stay in VPN Director mode, so every existing test sees the tunnels it sees today.

- [ ] **Step 2: Write the failing unit test**

In `router/test/unit/platform_merlin.bats`, add after the test `platform_tunnels: only main when rt_tables is missing`:

```bash
# "Redirect Internet traffic through tunnel" is nvram vpn_clientN_rgw: 0 "No",
# 1 "Yes (all)", 2 "VPN Director (policy rules)"; the firmware reads an empty
# value as 0. In "No" and "Yes (all)" it adds "from all lookup ovpncN" at
# priority 10000+N while the client runs, so every packet of the router reads
# the client's table first, and a default Tunnel Director put there would take
# them all into the tunnel.

# with_ovpn_modes <client 1> <client 2> - an nvram that answers the redirect
# mode of OpenVPN clients 1 and 2 as given ("" for unset) and nothing for any
# other key.
with_ovpn_modes() {
    with_mock nvram "case \"\$*\" in
    \"get vpn_client1_rgw\") echo '$1' ;;
    \"get vpn_client2_rgw\") echo '$2' ;;
    *) echo '' ;;
esac"
}

@test "platform_tunnels: an OpenVPN client is listed only in VPN Director mode" {
    load_platform
    with_ovpn_modes 0 2
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 ovpnc2 main)"
    with_ovpn_modes 2 1
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 ovpnc1 main)"
    with_ovpn_modes "" ""
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 main)"
}
```

- [ ] **Step 3: Write the failing integration tests**

In `router/test/integration/vpn_director.bats`, add after the test `vpn-director: platform lists every tunnel except main`:

```bash
# An OpenVPN client in "No" or "Yes (all)" routes the whole router through its
# table, so the platform leaves it out. vpn_client1_rgw is 0 here; every other
# key comes from the standard mock, and the fixture rt_tables names ovpnc1 and
# ovpnc2.
@test "vpn-director: platform leaves out an OpenVPN client that is not in VPN Director mode" {
    mkdir -p "$BATS_TEST_TMPDIR/mock"
    printf '#!/bin/bash\n[ "$*" = "get vpn_client1_rgw" ] && { echo 0; exit 0; }\nexec "$TEST_ROOT/mocks/nvram" "$@"\n' \
        > "$BATS_TEST_TMPDIR/mock/nvram"
    chmod +x "$BATS_TEST_TMPDIR/mock/nvram"
    PATH="$BATS_TEST_TMPDIR/mock:$PATH" run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    [ "${#lines[@]}" -eq 1 ]
    echo "$output" | jq -e '[.tunnels[].id] == ["wgc1","wgc2","ovpnc2"]' >/dev/null
}
```

In the test `vpn-director: platform reports wan_if as empty when the platform has no answer`, add after its line `echo "$output" | jq -e '.wan_if == ""' >/dev/null`:

```bash
    # With no redirect mode to read, no OpenVPN client is routable; the
    # WireGuard tables do not depend on nvram.
    echo "$output" | jq -e '[.tunnels[].id] == ["wgc1","wgc2"]' >/dev/null
```

- [ ] **Step 4: Run the new tests to verify they fail**

Run: `bats -f 'listed only in VPN Director mode' router/test/unit/platform_merlin.bats`
Expected: FAIL: the output lists `ovpnc1` in mode 0.

Run: `bats -f 'platform' router/test/integration/vpn_director.bats`
Expected: 2 failures, `platform leaves out an OpenVPN client…` and `platform reports wan_if as empty…`, both on their `jq -e` line.

- [ ] **Step 5: Implement the filter**

In `router/opt/vpn-director/lib/platform/merlin.sh`, replace

```bash
# wgcN first, ovpncN next, main always last. RT_TABLES_FILE overrides the path
# for tests.
platform_tunnels() {
    local rt_tables="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}"
    { awk '$0!~/^#/ && $2 ~ /^wgc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null | sort; } || true
    { awk '$0!~/^#/ && $2 ~ /^ovpnc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null | sort; } || true
    printf '%s\n' main
}
```

with

```bash
# The "Redirect Internet traffic through tunnel" setting of OpenVPN client N
# (ovpncN): nvram vpn_clientN_rgw, "" when it is not set. 0 "No", 1 "Yes (all)",
# 2 "VPN Director (policy rules)"; the firmware reads an empty value as 0
# (nvram_pf_get_int).
_merlin_ovpn_rgw() {
    nvram get "vpn_client${1#ovpnc}_rgw" 2>/dev/null || true
}

# Whether Tunnel Director may route through <id>. In "No" and "Yes (all)" the
# firmware adds "from all lookup ovpncN" at priority 10000+N while the client
# runs (amvpn_set_routing_rules in libovpn/amvpn_routing.c, Merlin 388): every
# packet of the router reads that table ahead of Tunnel Director's fwmark rule,
# and a default there takes them all into the tunnel - the direct clients, the
# router itself, Xray's upstream and the other tunnels' clients. Only VPN
# Director mode leaves the table to the rules that name their sources. A
# WireGuard client has no such setting - the firmware routes every wgcN by VPN
# Director rules alone (it sets rgw = OVPN_RGW_POLICY for WireGuard) - and main
# is no tunnel. Read on every call: the mode can be switched at any time.
_merlin_tunnel_routable() {
    case "${1:-}" in
        ovpnc[0-9]*) [[ "$(_merlin_ovpn_rgw "$1")" == 2 ]] ;;
        *)           return 0 ;;
    esac
}

# wgcN first, ovpncN next, main always last. RT_TABLES_FILE overrides the path
# for tests. An OpenVPN client is listed only in VPN Director mode
# (_merlin_tunnel_routable). A TAP client never is: its page offers no such
# mode, and Tunnel Director, which routes through tun1N, could not use it.
platform_tunnels() {
    local rt_tables="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}" ovpn id
    { awk '$0!~/^#/ && $2 ~ /^wgc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null | sort; } || true
    ovpn="$({ awk '$0!~/^#/ && $2 ~ /^ovpnc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null | sort; } || true)"
    while IFS= read -r id; do
        if [[ -n $id ]] && _merlin_tunnel_routable "$id"; then
            printf '%s\n' "$id"
        fi
    done <<< "$ovpn"
    printf '%s\n' main
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `bats router/test/unit/platform_merlin.bats router/test/integration/vpn_director.bats router/test/unit/tunnel.bats router/test/unit/configure.bats`
Expected: every test passes, the new ones included.

- [ ] **Step 7: Check shellcheck**

Run: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l`
Expected: `53`

- [ ] **Step 8: Commit**

```bash
git add router/test/mocks/nvram router/opt/vpn-director/lib/platform/merlin.sh \
    router/test/unit/platform_merlin.bats router/test/integration/vpn_director.bats
git commit -F - <<'EOF'
fix(merlin): list an OpenVPN client only in VPN Director mode

In "No" and "Yes (all)" the firmware adds "from all lookup ovpncN" at
priority 10000+N while the client runs, so the default Tunnel Director
installs in that table takes every packet of the router into the tunnel.
platform_tunnels now lists an ovpncN only while vpn_clientN_rgw is 2, and
the Web UI, the bot, the failover and configure.sh stop offering the rest.
EOF
```

---

### Task 2: No default into the table of such a client, and the stray one taken out

**Files:**
- Modify: `router/opt/vpn-director/lib/platform/merlin.sh:156-173` (`platform_tunnel_route_ensure`, `platform_tunnel_table_release`)
- Test: `router/test/unit/platform_merlin.bats`
- Test: `router/test/unit/tunnel.bats` (append at the end)

**Interfaces:**
- Consumes: `_merlin_ovpn_rgw`, `_merlin_tunnel_routable` and `with_ovpn_modes` (Task 1); `with_tun_addr` (already in `platform_merlin.bats`); `load_tunnel_module_with`, `marks_in_place` (already in `tunnel.bats`); `platform_tunnel_iface` (already in `merlin.sh`, `ovpncN` → `tun1N`).
- Produces:
  - `_merlin_drop_default <id>`: for an `ovpncN` in mode 0, runs `ip route del default dev tun1N table ovpncN`, its failure ignored; nothing for any other id or mode. No output, always rc 0.
  - `platform_tunnel_route_ensure <id> <idx> [gateway]`: rc 1 with no `ip route replace` for an id `_merlin_tunnel_routable` refuses, after `_merlin_drop_default`; otherwise as today.
  - `platform_tunnel_table_release <id> <idx>`: `_merlin_drop_default <id>`, then rc 0.
  - In `tunnel.bats`: `ovpn_mode <client> <rgw>` defines an `nvram` shell function that answers `get vpn_client<client>_rgw` with `<rgw>` and defers every other key to the mock; `tun11_addressed` exports `BATS_IP_ADDRS_FILE` with `br0 192.168.1.1/24` and `tun11 10.8.0.2/24`.

- [ ] **Step 1: Write the failing platform tests**

In `router/test/unit/platform_merlin.bats`, add after the test `platform_tunnel_table_release: does not flush a firmware table`:

```bash
# Out of VPN Director mode the client's table is the one the firmware routes the
# whole router through, so no default goes in. In "No" the firmware deletes its
# own default whenever the client comes up, so one found there is Tunnel
# Director's and goes; in "Yes (all)" the default is the firmware's and stays.
@test "platform_tunnel_route_ensure: installs nothing for an OpenVPN client out of VPN Director mode" {
    load_platform
    with_tun_addr "tun11 10.73.149.53/24"
    local mode
    for mode in 0 1 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_route_ensure ovpnc1 0
        assert_failure
        refute_output
        refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
    done
}

@test "platform_tunnel_route_ensure: takes the default out of the table of a client in No mode" {
    load_platform
    local mode
    for mode in 0 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_route_ensure ovpnc1 0
        assert_failure
        grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    done
    with_ovpn_modes 1 2
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure ovpnc1 0
    assert_failure
    refute grep -q 'ip route del' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_table_release: takes the default out of a table in No mode only" {
    load_platform
    local mode
    for mode in 0 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_table_release ovpnc1 0
        assert_success
        grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    done
    with_ovpn_modes 1 2
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_table_release ovpnc1 0
    assert_success
    run platform_tunnel_table_release ovpnc2 1
    assert_success
    run platform_tunnel_table_release wgc1 0
    assert_success
    [ ! -s /tmp/bats_ip_calls.log ]
}
```

In the same file, release is now a no-op only in VPN Director mode. Replace the comment block above `with_tun_addr`

```bash
# Firmware OpenVPN in policy mode (rgw=2) copies WAN into ovpncN when the
# server does not push redirect-gateway. Tunnel Director then marks packets
# into a table whose default is still the WAN. The spec is the default that
# replace puts in that table; release stays a no-op because the rest of the
# table is firmware's.
```

with

```bash
# Firmware OpenVPN in policy mode (rgw=2) copies WAN into ovpncN when the
# server does not push redirect-gateway. Tunnel Director then marks packets
# into a table whose default is still the WAN. The spec is the default that
# replace puts in that table. In that mode release stays a no-op because the
# rest of the table is firmware's.
```

- [ ] **Step 2: Write the failing end-to-end tests**

Append to the end of `router/test/unit/tunnel.bats`:

```bash

# ============================================================================
# Merlin: an OpenVPN client out of VPN Director mode
# ============================================================================

# In "No" and "Yes (all)" the firmware routes the whole router through the
# client's table (a "from all" rule at 10000+N). These run the real Merlin
# implementation: its platform_tunnels leaves such a client out, its route
# ensure installs no default for it, and in "No" its route ensure and release
# take out a default an earlier apply left there.

# ovpn_mode <client> <rgw> - from here on, nvram answers the redirect mode of
# that OpenVPN client as given ("" for unset) and defers to the mock for every
# other key. A function, so the subshells of `run` see it as well.
ovpn_mode() {
    OVPN_MODE_KEY="get vpn_client$1_rgw"
    OVPN_MODE_VALUE="$2"
    nvram() {
        if [[ $* == "$OVPN_MODE_KEY" ]]; then
            printf '%s\n' "$OVPN_MODE_VALUE"
            return 0
        fi
        command nvram "$@"
    }
}

# tun11_addressed - tun11 carries 10.8.0.2/24, so a route ensure that is not
# refused installs "default via 10.8.0.1 dev tun11 table ovpnc1".
tun11_addressed() {
    export BATS_IP_ADDRS_FILE="$BATS_TEST_TMPDIR/addrs"
    printf '%s\n' "br0 192.168.1.1/24" "tun11 10.8.0.2/24" > "$BATS_IP_ADDRS_FILE"
}

# The mode is switched after the apply that recorded the tunnel; the config
# stays, so the next apply takes the up-to-date branch, which ensures the
# route of every tunnel on record.
@test "tunnel_apply: the up-to-date path installs no default for a client switched out of VPN Director mode" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.5"]}}'
    tun11_addressed
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    grep -qx 'ip route replace default via 10.8.0.1 dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    [ -f "$TUN_DIR_HASH" ]

    ovpn_mode 1 0
    fw_chain_exists() { return 0; }
    marks_in_place
    : > /tmp/bats_ip_calls.log
    : > "$LOG_FILE"
    # Not through `run`: TUNNEL_UNCARRIED has to outlive the call.
    tunnel_apply

    grep -q "Rules are applied and up-to-date" "$LOG_FILE"
    refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
    grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    run tunnel_uncarried
    assert_output "192.168.1.5"
}

@test "tunnel_apply: the up-to-date path leaves the firmware's default of a client in Yes (all) mode" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.5"]}}'
    tun11_addressed
    run tunnel_apply
    assert_success

    ovpn_mode 1 1
    fw_chain_exists() { return 0; }
    marks_in_place
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Rules are applied and up-to-date"
    refute grep -qE 'ip route (replace|del)' /tmp/bats_ip_calls.log
}

@test "tunnel_stop: takes out the default of a recorded client switched out of VPN Director mode" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success

    ovpn_mode 1 0
    : > /tmp/bats_ip_calls.log
    run tunnel_stop
    assert_success
    grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    [ ! -f "$TUN_DIR_TABLES" ]
}
```

- [ ] **Step 3: Run the new tests to verify they fail**

Run: `bats -f 'VPN Director mode|No mode|Yes \(all\)' router/test/unit/platform_merlin.bats router/test/unit/tunnel.bats`
Expected: the 6 new tests fail: the route ensure installs `default via … dev tun11 table ovpnc1`, or no `ip route del` appears in the log. Task 1's test `platform_tunnels: an OpenVPN client is listed only in VPN Director mode` matches the filter too and passes.

- [ ] **Step 4: Implement the refusal and the cleanup**

In `router/opt/vpn-director/lib/platform/merlin.sh`, replace

```bash
# "ip route replace" is idempotent, which tunnel.sh relies on: it calls this
# on every apply, including those that change nothing, so the default follows
# the interface across OpenVPN flaps once wan-event fires an apply.
platform_tunnel_route_ensure() {
    local id="${1:-}" idx="${2:-}" gateway="${3:-}" table spec
    table="$(platform_tunnel_table "$id" "$idx")" || return 1
    [[ $table != main ]] || return 0
    spec="$(platform_tunnel_route "$id" "$gateway")" || return 1
    # shellcheck disable=SC2086
    ip route replace $spec table "$table" 2>/dev/null
}

# The rest of ovpncN/wgcN is firmware's (LAN routes, the tunnel prefix, DNS).
# Flushing it would drop those. tunnel_stop only needs the ip rule gone; the
# default we installed is inert without it.
platform_tunnel_table_release() {
    return 0
}
```

with

```bash
# Take the default out of the table of an OpenVPN client in "No" mode
# (vpn_clientN_rgw 0 or unset). Whenever the client comes up in that mode the
# firmware deletes its own ("ip route del default table ovpncN" in
# libovpn/openvpn_control.c), so a default found there is one an earlier
# Tunnel Director apply installed - and the firmware's "from all" rule sends
# every packet of the router to it. The dev selector keeps the delete to a
# default through the tunnel, the only kind Tunnel Director installs. In "Yes
# (all)" the default is the firmware's and stays. Prints nothing; rc 0.
_merlin_drop_default() {
    local id="${1:-}" rgw iface
    case "$id" in
        ovpnc[0-9]*) ;;
        *)           return 0 ;;
    esac
    rgw="$(_merlin_ovpn_rgw "$id")"
    [[ -z $rgw || $rgw == 0 ]] || return 0
    iface="$(platform_tunnel_iface "$id")" || return 0
    ip route del default dev "$iface" table "$id" 2>/dev/null || true
}

# "ip route replace" is idempotent, which tunnel.sh relies on: it calls this
# on every apply, including those that change nothing, so the default follows
# the interface across OpenVPN flaps once wan-event fires an apply. A tunnel
# _merlin_tunnel_routable refuses gets none: the configuration can still name
# it, and TUN_DIR_TABLES can still hold it after its mode was switched, but
# its table is the one the firmware routes the whole router through. In "No"
# mode a default an earlier apply left there goes as well.
platform_tunnel_route_ensure() {
    local id="${1:-}" idx="${2:-}" gateway="${3:-}" table spec
    table="$(platform_tunnel_table "$id" "$idx")" || return 1
    [[ $table != main ]] || return 0
    if ! _merlin_tunnel_routable "$id"; then
        _merlin_drop_default "$id"
        return 1
    fi
    spec="$(platform_tunnel_route "$id" "$gateway")" || return 1
    # shellcheck disable=SC2086
    ip route replace $spec table "$table" 2>/dev/null
}

# The rest of ovpncN/wgcN is firmware's (LAN routes, the tunnel prefix, DNS).
# Flushing it would drop those. tunnel_stop only needs the ip rule gone; in VPN
# Director mode the default we installed is inert without it. In "No" mode the
# firmware's own "from all" rule reads the table, so that default goes.
platform_tunnel_table_release() {
    _merlin_drop_default "${1:-}"
    return 0
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bats router/test/unit/platform_merlin.bats router/test/unit/tunnel.bats router/test/integration/vpn_director.bats`
Expected: every test passes, the new ones included.

- [ ] **Step 6: Check shellcheck**

Run: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l`
Expected: `53`

- [ ] **Step 7: Commit**

```bash
git add router/opt/vpn-director/lib/platform/merlin.sh \
    router/test/unit/platform_merlin.bats router/test/unit/tunnel.bats
git commit -F - <<'EOF'
fix(merlin): no default into the table of a client out of VPN Director mode

TUN_DIR_TABLES can still hold a client whose mode was switched after it
was applied, and the up-to-date branch ensures every tunnel on record, so
the filter alone let the default back in. platform_tunnel_route_ensure
now refuses a client platform_tunnels does not list, and in "No" mode it
and platform_tunnel_table_release take out a default through the tunnel,
as the firmware does with its own whenever the client comes up in that
mode. A "Yes (all)" default is the firmware's and stays.
EOF
```

---

### Task 3: The contract's `platform_tunnel_unlisted_reason`

**Files:**
- Modify: `router/opt/vpn-director/lib/platform.sh:17-67` (the contract header)
- Modify: `router/opt/vpn-director/lib/platform/merlin.sh` (after `platform_tunnels`)
- Modify: `router/opt/vpn-director/lib/platform/keenetic.sh` (after `platform_tunnels`)
- Test: `router/test/unit/platform_merlin.bats`
- Test: `router/test/unit/platform_keenetic.bats`

**Interfaces:**
- Consumes: `_merlin_ovpn_rgw`, `_merlin_tunnel_routable`, `with_ovpn_modes` (Task 1).
- Produces: `platform_tunnel_unlisted_reason <id>` on both platforms. On Merlin, for an `ovpncN` that `rt_tables` names and `_merlin_tunnel_routable` refuses, it prints the reason line of the Global Constraints and returns 0; for anything else it prints nothing and returns 1. On KeeneticOS it always prints nothing and returns 1.

- [ ] **Step 1: Write the failing tests**

In `router/test/unit/platform_merlin.bats`, add after the test `platform_tunnels: an OpenVPN client is listed only in VPN Director mode`:

```bash
@test "platform_tunnel_unlisted_reason: names the redirect mode of an OpenVPN client it leaves out" {
    load_platform
    with_ovpn_modes 0 1
    run platform_tunnel_unlisted_reason ovpnc1
    assert_success
    assert_output 'OpenVPN client 1 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No")'
    run platform_tunnel_unlisted_reason ovpnc2
    assert_success
    assert_output 'OpenVPN client 2 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "Yes (all)")'
    with_ovpn_modes "" 3
    run platform_tunnel_unlisted_reason ovpnc1
    assert_success
    assert_output 'OpenVPN client 1 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No")'
    run platform_tunnel_unlisted_reason ovpnc2
    assert_success
    assert_output 'OpenVPN client 2 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "3")'
}

# A typo in the tunnel id has no mode: ovpnc9 is not in the fixture rt_tables,
# so its warning stays the generic "not a tunnel this platform knows".
@test "platform_tunnel_unlisted_reason: nothing for a listed tunnel, WireGuard, main or an id rt_tables does not name" {
    load_platform
    with_ovpn_modes 0 2
    local id
    for id in ovpnc2 wgc1 main ovpnc9 eth0 ""; do
        run platform_tunnel_unlisted_reason "$id"
        assert_failure
        refute_output
    done
}
```

In `router/test/unit/platform_keenetic.bats`, add after the test `platform_tunnels: only main when NDM does not answer`:

```bash
# Every OpenVPN and WireGuard interface RCI lists is one Tunnel Director may
# route through, and its tables are Tunnel Director's own: nothing to explain.
@test "platform_tunnel_unlisted_reason: Keenetic has no reason to give" {
    load_platform
    run platform_tunnel_unlisted_reason OpenVPN0
    assert_failure
    refute_output
    run platform_tunnel_unlisted_reason ovpnc1
    assert_failure
    refute_output
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats -f 'unlisted_reason' router/test/unit/platform_merlin.bats router/test/unit/platform_keenetic.bats`
Expected: 3 failures, each with `platform_tunnel_unlisted_reason: command not found` in the output.

- [ ] **Step 3: Implement it on Merlin**

In `router/opt/vpn-director/lib/platform/merlin.sh`, add right after the closing `}` of `platform_tunnels`:

```bash

# Why an OpenVPN client rt_tables names is not listed: its redirect mode, for
# the warning tunnel.sh logs where it would otherwise call the tunnel unknown.
# Nothing and rc 1 for a tunnel platform_tunnels lists, a WireGuard client,
# main, and an id rt_tables does not name - a typo has no mode.
platform_tunnel_unlisted_reason() {
    local id="${1:-}" rt_tables="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}" rgw setting
    case "$id" in
        ovpnc[0-9]*) ;;
        *)           return 1 ;;
    esac
    awk -v id="$id" '$0 !~ /^#/ && $2 == id { found = 1 } END { exit !found }' "$rt_tables" 2>/dev/null ||
        return 1
    if _merlin_tunnel_routable "$id"; then
        return 1
    fi
    rgw="$(_merlin_ovpn_rgw "$id")"
    case "$rgw" in
        ''|0) setting="No" ;;
        1)    setting="Yes (all)" ;;
        *)    setting="$rgw" ;;
    esac
    printf 'OpenVPN client %s is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "%s")\n' \
        "${id#ovpnc}" "$setting"
}
```

- [ ] **Step 4: Implement it on KeeneticOS**

In `router/opt/vpn-director/lib/platform/keenetic.sh`, add right after the closing `}` of `platform_tunnels`:

```bash

# Every OpenVPN and WireGuard interface RCI lists is one Tunnel Director may
# route through: its tables are Tunnel Director's own (KEENETIC_TABLE_BASE +
# idx), and no firmware rule reads them. There is never a reason to give.
platform_tunnel_unlisted_reason() {
    return 1
}
```

- [ ] **Step 5: Document it in the contract**

In `router/opt/vpn-director/lib/platform.sh`, replace the line

```bash
#   platform_tunnels                       tunnel ids, one per line, "main" last
```

with

```bash
#   platform_tunnels                       the tunnels Tunnel Director may route through,
#                                          one id per line, "main" last
#   platform_tunnel_unlisted_reason <id>   why a tunnel the firmware has is not listed,
#                                          one line for a log message; nothing and rc 1
#                                          when there is no such reason
```

and, in "What the core relies on beyond those signatures", add after the bullet that ends `not only when something changed.`:

```bash
#   * platform_tunnel_route_ensure may refuse a tunnel platform_tunnels does
#     not list. TUN_DIR_TABLES can still hold one whose mode changed after it
#     was applied, and on Merlin its table is then the one the firmware routes
#     the whole router through: a default there would take every packet along.
#   * platform_tunnel_unlisted_reason is read for messages only; nothing in
#     the core decides on it.
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `bats router/test/unit/platform_merlin.bats router/test/unit/platform_keenetic.bats router/test/unit/platform.bats`
Expected: every test passes, the new ones included.

- [ ] **Step 7: Check shellcheck**

Run: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l`
Expected: `53`

- [ ] **Step 8: Commit**

```bash
git add router/opt/vpn-director/lib/platform.sh router/opt/vpn-director/lib/platform/merlin.sh \
    router/opt/vpn-director/lib/platform/keenetic.sh \
    router/test/unit/platform_merlin.bats router/test/unit/platform_keenetic.bats
git commit -F - <<'EOF'
feat(platform): platform_tunnel_unlisted_reason says why a tunnel is left out

A new contract function names the reason a tunnel the firmware has is not
one platform_tunnels lists, for the warning tunnel.sh logs. On Merlin it
is the OpenVPN client's redirect mode; KeeneticOS lists every OpenVPN and
WireGuard interface it can route through and has none to give.
EOF
```

---

### Task 4: Tunnel Director says why

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (header at `:11-14`; `_tunnel_ensure_routes` and the comment above it at `:246-263`; `_tunnel_collect_applied` at `:432-441`; the comment at `:1158-1169` and the WARN at `:1173`)
- Test: `router/test/unit/tunnel.bats`

**Interfaces:**
- Consumes: `platform_tunnel_unlisted_reason` (Task 3); `ovpn_mode`, `tun11_addressed` and the two up-to-date tests (Task 2).
- Produces: the two warnings of the Global Constraints. No function or variable changes its signature.

- [ ] **Step 1: Write the failing tests**

In `router/test/unit/tunnel.bats`, in the test `tunnel_apply: the up-to-date path installs no default for a client switched out of VPN Director mode` (Task 2), add after `grep -q "Rules are applied and up-to-date" "$LOG_FILE"`:

```bash
    grep -qF "Tunnel 'ovpnc1': route not installed: OpenVPN client 1 is not in VPN Director mode (\"Redirect Internet traffic through tunnel\" is \"No\"); Tunnel Director does not route through it" "$LOG_FILE"
```

In the test `tunnel_apply: the up-to-date path leaves the firmware's default of a client in Yes (all) mode` (Task 2), add after `assert_output --partial "Rules are applied and up-to-date"`:

```bash
    assert_output --partial "Tunnel 'ovpnc1': route not installed: OpenVPN client 1 is not in VPN Director mode (\"Redirect Internet traffic through tunnel\" is \"Yes (all)\"); Tunnel Director does not route through it"
```

Append to the end of the file:

```bash

@test "tunnel_apply: skips an OpenVPN client out of VPN Director mode and says why" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.5"]}}'
    ovpn_mode 1 0
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'ovpnc1' is skipped: OpenVPN client 1 is not in VPN Director mode (\"Redirect Internet traffic through tunnel\" is \"No\")"
    refute_output --partial "not a tunnel this platform knows"
    assert_output --partial "not recorded as up-to-date"
    refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
    refute grep -q 'lookup ovpnc1' /tmp/bats_ip_calls.log
    [ ! -f "$TUN_DIR_HASH" ]
}

# `restart` and `restart tunnel` rebuild with the config unchanged
# (TUN_DIR_FORCE_REBUILD=1). The client's slot is on record from before the
# switch; the rebuild gives it none, so the slot is released after the swap -
# its ip rule, and in "No" mode the default in its table.
@test "tunnel_apply: a rebuild releases the slot of a client switched out of VPN Director mode" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    run cat "$TUN_DIR_TABLES"
    assert_output "0 ovpnc1"

    ovpn_mode 1 0
    export TUN_DIR_FORCE_REBUILD=1
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'ovpnc1' is skipped: OpenVPN client 1 is not in VPN Director mode"
    grep -qx 'ip rule del pref 16384 fwmark 0x10000/0xff0000 lookup ovpnc1' /tmp/bats_ip_calls.log
    grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    run cat "$TUN_DIR_TABLES"
    refute_output --partial "ovpnc1"
}

# The watch drops Xray membership on failover_ready. A failover tunnel the
# platform leaves out gets no slot, so no marker goes out and the Xray clients
# stay proxied. tun11 has an address: listed, the tunnel would be carried.
@test "tunnel_apply: no slot and no failover_ready for a failover tunnel out of VPN Director mode" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.8"]}}'
    export XRAY_FAILOVER_TUNNEL=ovpnc1 XRAY_FAILOVER_CLIENTS=192.168.1.8
    tun11_addressed
    ovpn_mode 1 0
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'ovpnc1' is skipped: OpenVPN client 1 is not in VPN Director mode"
    run cat "$TUN_DIR_TABLES"
    refute_output --partial "ovpnc1"
    [ ! -e "$TUN_DIR_FAILOVER_READY" ]
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bats -f 'VPN Director mode|Yes \(all\)' router/test/unit/tunnel.bats`
Expected: 5 failures: the two up-to-date tests on their new warning line and the three new tests on `is skipped`. `tunnel_stop: takes out the default…` still passes.

- [ ] **Step 3: Give the reason in the rebuild's skip**

In `router/opt/vpn-director/lib/tunnel.sh`, in `_tunnel_collect_applied`, replace

```bash
    local tunnel tunnel_type clients_type clients idx used tunnels
```

with

```bash
    local tunnel tunnel_type clients_type clients idx used tunnels reason
```

and replace

```bash
        if ! _tunnel_table_allowed "$tunnel"; then
            log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
```

with

```bash
        if ! _tunnel_table_allowed "$tunnel"; then
            reason="$(platform_tunnel_unlisted_reason "$tunnel")" || reason=""
            if [[ -n $reason ]]; then
                log -l WARN "Tunnel '$tunnel' is skipped: $reason"
            else
                log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
            fi
```

- [ ] **Step 4: Give the reason on the up-to-date branch**

In `_tunnel_ensure_routes`, replace the comment lines

```bash
# Reads TUN_DIR_TABLES ("<idx> <id>" per line, written by tunnel_apply) and calls
# platform_tunnel_route_ensure for each. A no-op on Merlin, where the firmware
# keeps the tunnel tables; on Keenetic the route follows the interface state.
# A tunnel whose route or ip rule is not in place routes its marks to main: its
# clients are not carried (TUNNEL_UNCARRIED).
```

with

```bash
# Reads TUN_DIR_TABLES ("<idx> <id>" per line, written by tunnel_apply) and calls
# platform_tunnel_route_ensure for each, so the default follows the tunnel's
# interface across a flap. The platform may refuse a tunnel it no longer lists -
# on Merlin, an OpenVPN client switched out of VPN Director mode after it was
# applied - and the warning then gives its reason (platform_tunnel_unlisted_reason).
# A tunnel whose route or ip rule is not in place routes its marks to main: its
# clients are not carried (TUNNEL_UNCARRIED).
```

then replace

```bash
    local idx tunnel rc=0 carried
```

with

```bash
    local idx tunnel rc=0 carried reason
```

and replace

```bash
        if ! platform_tunnel_route_ensure "$tunnel" "$idx" "$(_tunnel_gateway "$tunnel")"; then
            log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
            carried=0
```

with

```bash
        if ! platform_tunnel_route_ensure "$tunnel" "$idx" "$(_tunnel_gateway "$tunnel")"; then
            reason="$(platform_tunnel_unlisted_reason "$tunnel")" || reason=""
            if [[ -n $reason ]]; then
                log -l WARN "Tunnel '$tunnel': route not installed: $reason; Tunnel Director does not route through it"
            else
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
            fi
            carried=0
```

The rebuild's own `route not installed` warnings (in `tunnel_apply`, for the slots of the new layout) stay as they are: every tunnel there is one the platform lists.

- [ ] **Step 5: Correct the comment and the summary warning of step 4 of the rebuild**

In `tunnel_apply`, replace

```bash
    # routing until the next rebuild - a silent fail-open. On Merlin the only
    # case is a typo in the tunnel id, which then warns on every apply instead
    # of once. A refused chain rule is the same: the up-to-date branch looks for
```

with

```bash
    # routing until the next rebuild - a silent fail-open. On Merlin it is a
    # typo in the tunnel id, or an OpenVPN client out of VPN Director mode, which
    # can be switched back at any time; either warns on every apply instead of
    # once. A refused chain rule is the same: the up-to-date branch looks for
```

and replace

```bash
        log -l WARN "Tunnel Director: a configured tunnel is unknown to the platform (RCI down, or a typo in the id); this apply is not recorded as up-to-date and the next apply retries"
```

with

```bash
        log -l WARN "Tunnel Director: a configured tunnel is not one the platform lists (see the warning above); this apply is not recorded as up-to-date and the next apply retries"
```

- [ ] **Step 6: Name the new function in the file header**

Replace

```bash
#     platform_tunnel_route_ensure, platform_tunnel_table_release,
#     platform_prerouting_base_pos, platform_lan_ifaces)
```

with

```bash
#     platform_tunnel_route_ensure, platform_tunnel_table_release,
#     platform_tunnel_unlisted_reason, platform_prerouting_base_pos, platform_lan_ifaces)
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `bats router/test/unit/tunnel.bats router/test/integration/vpn_director.bats`
Expected: every test passes. The `wgc9` tests (`does not record a config whose only tunnel is unknown to the platform` and its neighbours) still see `not a tunnel this platform knows` and `not recorded as up-to-date`.

- [ ] **Step 8: Check shellcheck**

Run: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l`
Expected: `53`

- [ ] **Step 9: Commit**

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats
git commit -F - <<'EOF'
fix(tunnel): say why a tunnel the platform leaves out gets no route

A configured tunnel the platform does not list was reported as one the
platform does not know, and a recorded one whose route ensure was refused
as an interface that is down. When the platform names a reason, both
warnings now give it: on Merlin, the OpenVPN client's redirect mode.
Without one the texts stay as they were.
EOF
```

---

### Task 5: Documentation and the final check

**Files:**
- Modify: `CLAUDE.md` (Key Concepts)
- Modify: `.claude/rules/tunnel-director.md`
- Modify: `README.md`
- Modify: `README.ru.md`

**Interfaces:**
- Consumes: the behaviour of Tasks 1-4, as the Global Constraints state it.
- Produces: nothing code depends on.

- [ ] **Step 1: CLAUDE.md**

In `CLAUDE.md`, under Key Concepts, replace

```markdown
- A tunnel key is any id `platform_tunnels` lists (Merlin: `wgcN` and `ovpncN` from
  `/etc/iproute2/rt_tables`, plus `main`; Keenetic: `OpenVPNN` and `WireguardN` from
  RCI, plus `main`); a key the platform does not list is skipped with a warning
```

with

```markdown
- A tunnel key is any id `platform_tunnels` lists (Merlin: `wgcN` from
  `/etc/iproute2/rt_tables`, and `ovpncN` while that OpenVPN client is in "VPN Director
  (policy rules)" mode, plus `main`; Keenetic: `OpenVPNN` and `WireguardN` from RCI, plus
  `main`); a key the platform does not list is skipped with a warning, which on Merlin
  names the OpenVPN client's mode
```

- [ ] **Step 2: `.claude/rules/tunnel-director.md`**

Replace the table row

```markdown
| Tunnel key | A tunnel id `platform_tunnels` lists (Merlin: `wgcN`, `ovpncN` from `/etc/iproute2/rt_tables`, plus `main`; Keenetic: `OpenVPNN`, `WireguardN` from RCI, plus `main`) |
```

with

```markdown
| Tunnel key | A tunnel id `platform_tunnels` lists (Merlin: `wgcN` from `/etc/iproute2/rt_tables`, and `ovpncN` while its OpenVPN client is in "VPN Director (policy rules)" mode, plus `main`; Keenetic: `OpenVPNN`, `WireguardN` from RCI, plus `main`); see "Which tunnels the platform lists" |
```

Add this section between the `## Behavior` section and `## Chain Architecture`:

```markdown
## Which tunnels the platform lists

`platform_tunnels` lists the tunnels Tunnel Director may route through. On Merlin that is every
`wgcN` in `rt_tables`, and an `ovpncN` only while its OpenVPN client's "Redirect Internet traffic
through tunnel" (nvram `vpn_clientN_rgw`) is "VPN Director (policy rules)" (`2`). In "No" (`0`, or
unset: the firmware reads an empty value as 0) and "Yes (all)" (`1`) the firmware adds
`from all lookup ovpncN` at priority 10000+N while the client runs (`amvpn_set_routing_rules`,
`libovpn/amvpn_routing.c` in Merlin 388). Every packet of the router reads that table ahead of
Tunnel Director's fwmark rule, so a default there takes them all into the tunnel: the direct
clients, the router itself, Xray's upstream and the other tunnels' clients. A WireGuard client has
no such setting; the firmware routes every `wgcN` by VPN Director rules alone.

The configuration can still name such a client, and `TUN_DIR_TABLES` can still hold one whose mode
was switched after it was applied, which the up-to-date branch ensures like any tunnel on record.
So Merlin's `platform_tunnel_route_ensure` refuses a client `platform_tunnels` does not list, and
in "No" mode it and `platform_tunnel_table_release` take out a default through the tunnel
(`ip route del default dev tun1N table ovpncN`). The firmware deletes its own default in that mode
whenever the client comes up (`libovpn/openvpn_control.c`), so a default found there is one Tunnel
Director left. In "Yes (all)" the default is the firmware's and stays.

`tunnel.sh` asks `platform_tunnel_unlisted_reason` why a tunnel is left out and puts the answer in
its warning: `Tunnel 'ovpnc1' is skipped: OpenVPN client 1 is not in VPN Director mode ("Redirect
Internet traffic through tunnel" is "No")` on a rebuild, `route not installed: <reason>; Tunnel
Director does not route through it` for a recorded tunnel on the up-to-date branch. KeeneticOS has
no reason to give. A configuration that names such a tunnel records no hash, as one with a typo in
the id does: every apply rebuilds in place and repeats the warning, and the first apply after the
switch to VPN Director carries the tunnel's clients.
```

In `## Dependencies`, replace

```markdown
From the platform contract (`lib/platform.sh`, sourced by `common.sh`): `platform_tunnels`,
`platform_tunnel_table`, `platform_tunnel_route_ensure`, `platform_tunnel_table_release`,
`platform_tunnel_offload_target`, `platform_prerouting_base_pos`, `platform_lan_ifaces`
```

with

```markdown
From the platform contract (`lib/platform.sh`, sourced by `common.sh`): `platform_tunnels`,
`platform_tunnel_unlisted_reason`, `platform_tunnel_table`, `platform_tunnel_route_ensure`,
`platform_tunnel_table_release`, `platform_tunnel_offload_target`, `platform_prerouting_base_pos`,
`platform_lan_ifaces`
```

In `## Requirements`, add after `- VPN client must be active with NAT enabled`:

```markdown
- On Asuswrt-Merlin, an OpenVPN client in "VPN Director (policy rules)" mode (see "Which tunnels the platform lists")
```

- [ ] **Step 3: README.md**

Under `### Asuswrt-Merlin` in Requirements, replace

```markdown
- OpenVPN client configured in router UI (for Tunnel Director)
```

with

```markdown
- OpenVPN client configured in router UI, with "Redirect Internet traffic through tunnel" set to "VPN Director (policy rules)" (for Tunnel Director)
```

Under `### Tunnel Director`, replace

```markdown
A tunnel key is an id the platform lists (`wgc1` / `ovpnc1` on Merlin, `OpenVPN0` / `Wireguard1` on KeeneticOS). An OpenVPN tunnel may set
```

with

```markdown
A tunnel key is an id the platform lists (`wgc1` / `ovpnc1` on Merlin, `OpenVPN0` / `Wireguard1` on KeeneticOS). On Merlin an OpenVPN client is listed only in "VPN Director (policy rules)" mode: in "No" and "Yes (all)" the firmware sends all of the router's traffic through the client's routing table, so a route Tunnel Director put there would take every device into the tunnel. An apply skips such a client with a warning that names its mode, and takes out a default route an earlier version left in the table of a client in "No" mode. An OpenVPN tunnel may set
```

- [ ] **Step 4: README.ru.md**

Under `### Asuswrt-Merlin` in «Требования», replace

```markdown
- OpenVPN-клиент, настроенный в интерфейсе роутера (для Tunnel Director)
```

with

```markdown
- OpenVPN-клиент, настроенный в интерфейсе роутера, с «Redirect Internet traffic through tunnel» = «VPN Director (policy rules)» (для Tunnel Director)
```

Under `### Tunnel Director`, replace

```markdown
Ключ туннеля — идентификатор из списка платформы (`wgc1` / `ovpnc1` на Merlin, `OpenVPN0` / `Wireguard1` на KeeneticOS). У OpenVPN-туннеля
```

with

```markdown
Ключ туннеля — идентификатор из списка платформы (`wgc1` / `ovpnc1` на Merlin, `OpenVPN0` / `Wireguard1` на KeeneticOS). На Merlin OpenVPN-клиент попадает в список только в режиме «VPN Director (policy rules)»: в режимах «No» и «Yes (all)» прошивка пропускает весь трафик роутера через таблицу маршрутизации клиента, и маршрут, поставленный туда Tunnel Director, увёл бы в туннель все устройства. При применении конфигурации такой клиент пропускается с предупреждением, где назван его режим, а маршрут по умолчанию, оставленный прежней версией в таблице клиента в режиме «No», удаляется. У OpenVPN-туннеля
```

- [ ] **Step 5: Run the whole suite in the background**

Set `SCRATCH` to your scratchpad directory (`/tmp` when there is none) and run, in the background:

```bash
bats router/test/*.bats router/test/unit router/test/integration > "$SCRATCH/bats-full.log" 2>&1; echo "exit=$?" >> "$SCRATCH/bats-full.log"
```

Run nothing else under bats until it ends: the tests share files under `/tmp`.

Expected: the log's first line is `1..743`, `grep -c '^ok' "$SCRATCH/bats-full.log"` prints `743`, `grep -c '^not ok' "$SCRATCH/bats-full.log"` prints `0`, and the last line is `exit=0`. The count is the 729 tests of `master` (commit `3ff5acc`, all passing on 2026-09-27) plus the 14 new ones: 6 in `platform_merlin.bats` (1 in Task 1, 3 in Task 2, 2 in Task 3), 1 in `platform_keenetic.bats`, 6 in `tunnel.bats` (3 in Task 2, 3 in Task 4) and 1 in `vpn_director.bats`.

- [ ] **Step 6: Check shellcheck and the diff**

Run: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l`
Expected: `53`

Run: `git diff --stat master..HEAD -- . ':(exclude)docs/superpowers'`
Expected: only the files of the File Structure table.

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md .claude/rules/tunnel-director.md README.md README.ru.md
git commit -F - <<'EOF'
docs: Tunnel Director on Merlin needs an OpenVPN client in VPN Director mode

The README asked only for an OpenVPN client configured in the router UI.
In "No" and "Yes (all)" the firmware routes the whole router through the
client's table, so the platform no longer lists such a client, an apply
skips it with a warning that names its mode, and a default an earlier
version left in a "No" client's table goes. CLAUDE.md and the Tunnel
Director rules say which tunnels the platform lists and why.
EOF
```

---

## Finishing (with the owner, after Task 5)

- Ask the owner before any push and before opening a PR; the PR follows the house style of #54-#61.
- Before the PR, remove this plan and its spec from the branch (they stay in its history):

  ```bash
  git rm docs/superpowers/specs/2026-09-27-merlin-policy-mode-tunnels-design.md \
      docs/superpowers/plans/2026-09-27-merlin-policy-mode-tunnels.md
  git commit -m "docs: remove the design and plan documents before the PR"
  ```

- The PR description, and the v0.18.1 release notes if the owner releases, carry the one-time
  cleanup for a tunnel Tunnel Director no longer has on record: restart that OpenVPN client in the
  router UI, or run `ip route del default table ovpncN`.
- Post `@codex review` only with the owner's permission.
- After a release, read-only on the RT-AX86U (`ssh -o BatchMode=yes -p 2222 admin@192.168.1.1`):
  `/opt/vpn-director/vpn-director.sh platform` lists neither `ovpnc1` nor `ovpnc5`.
