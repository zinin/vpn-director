# Interface Tunnels and the Tunnel Kill Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tunnel Director routes LAN clients through any Linux interface the configuration lists (first: the AmneziaWG `awg1` on the owner's RT-AX86U), refuses a tunnel's traffic whenever the tunnel's table has no route instead of letting it leak to the WAN, and takes IPv4 addresses and CIDRs in a tunnel's `exclude`.

**Architecture:** A new library, `lib/iftunnel.sh`, owns interface tunnels - the checked list, table `2000 + idx`, the default route, NAT and MSS rules, a state file - and holds the `vpd_*` layer through which the core reaches every tunnel: a listed interface goes to `iftunnel_*`, any other id to the unchanged platform contract (Tasks 1-3). `lib/tunnel.sh` routes through the layer (Task 4), adds a `prohibit` ip rule per slot right after the lookup range - the kill switch (Task 5) - gives a tunnel the platform does not know a slot with that rule (Task 6) and returns on address exclusions (Task 7). `vpn-director.sh` gets `ensure-routes` and the cron job that runs it, and Asuswrt-Merlin a `nat-start` hook (Task 8). The Go config keeps the new list and the bot and Web UI stop promising the WAN (Task 9). Documentation follows (Task 10), then whole-branch verification (Task 11) and the owner's acceptance on the router (Task 12).

**Tech Stack:** Bash 5 (on the router Entware's bash, BusyBox 1.25 tools, iproute2, iptables), Bats 1.13 with bats-support and bats-assert; Go 1.25 (module `github.com/zinin/vpn-director/server`), standard `testing`; Vue 3 + TypeScript (`web/`).

**Spec:** `docs/superpowers/specs/2026-10-15-interface-tunnels-design.md`

Out of scope: everything the spec lists in section 13, and its section 12 ("To do in wireguard-network"), which the owner does in that repository.

## Global Constraints

- The platform contract does not change: `router/opt/vpn-director/lib/platform.sh`, `lib/platform/merlin.sh`, `lib/platform/keenetic.sh` and their tests are not edited. `lib/tunnel.sh`, `cmd_platform` and `configure.sh` reach tunnels only through the `vpd_*` functions of `lib/iftunnel.sh`.
- `IFTUNNEL_TABLE_BASE=2000`: an interface tunnel's routing table is `2000 + idx`.
- An interface name is kept when it is 1-15 characters of `A-Za-z0-9_.-`, is not `.` or `..`, is not `main`, is not an id `platform_tunnels` lists, is not a `platform_lan_ifaces` name and is not `platform_wan_if` (that last check is skipped when the platform cannot answer). Each rejected name logs exactly one WARN: `tunnel_director.interfaces: <reason>; ignored`.
- Block rule: preference `TUN_DIR_PREF_BASE + _tunnel_mark_field_max + idx`, selector `fwmark <mark>/<mask>`, action `prohibit`, for every slot except `main`'s. With the defaults the lookups take 16384-16638 and the blocks 16639-16893. When `TUN_DIR_PREF_BASE + 2 * _tunnel_mark_field_max > 32766`, `tunnel_apply` logs an ERROR and returns 1 before it touches a rule.
- NAT and MSS rules, exactly as `iptables -S` prints them, each inserted at position 1:
  - `nat POSTROUTING`: `-o <iface> -j MASQUERADE`
  - `mangle FORWARD`: `-o <iface> -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu`
  - `mangle FORWARD`: `-i <iface> -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss <MTU - 40>`
- State: `IFTUNNEL_STATE_FILE`, default `/tmp/tunnel_director/iftunnels`, one `<iface> <table>` line per interface tunnel with a slot; `IFTUNNEL_CRON_MARKER`, default `/tmp/tunnel_director/routes_cron`. Both are overridable from the environment.
- Cron job: name `vpn_director_routes`, schedule `* * * * *`, command `$SCRIPT_DIR/vpn-director.sh ensure-routes`.
- Hash: with `TUN_DIR_INTERFACES` empty the hash input is exactly what it is today; otherwise the line `interfaces: $TUN_DIR_INTERFACES` follows `TUN_DIR_TUNNELS_JSON`.
- Log texts, exactly:
  - WARN `Tunnel '<t>' is not on the platform's list: <reason>; its clients are refused`
  - WARN `Tunnel '<t>' is not a tunnel this platform knows; its clients are refused`
  - WARN `Tunnel '<t>': route not installed: <reason>; its clients are refused`
  - WARN `Tunnel '<t>': route not installed (interface down or not mapped?); its clients are refused until it is`
  - ERROR `Tunnel '<t>': the block rule did not go in (pref=<p> fwmark=<m>); while its table has no route its clients fall through to main`
  - ERROR `Tunnel Director: the block rules would reach main's rule (32766) with pref_base <b> and <F> slots; rules not applied`
  - WARN `Tunnel '<t>': exclusion '<x>' is IPv6; Tunnel Director routes IPv4 only, skipping it`
  - INFO `Interface tunnel back without its route: <ifaces>; applying Tunnel Director`
  - INFO `Scheduled the route check of the interface tunnels (every minute)`
  - WARN `Could not schedule the route check of the interface tunnels; an interface re-created by its own scripts keeps its clients refused until the next apply`
  - INFO `Dropped the route check of the interface tunnels`
  - Unchanged for a tunnel the platform does not list and that has no clients: `Tunnel '<t>' is skipped: <reason>` and `Tunnel '<t>' is not a tunnel this platform knows; skipping`.
- Bot texts (`moveQuestion`), exactly:
  - `%s is down: until it is up, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?`
  - `%s is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?`
- Web UI texts (`ClientsTab.vue`), exactly:
  - `` `${route} is down: until it is up, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``
  - `` `${route} is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``
- Shell rules (`.claude/rules/shell-conventions.md`): never `cmd | grep -q` - read an output whole, then search it; no `[:upper:]`/`[:lower:]`; no jq regex builtins (`test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub`) in a jq program; a bash regex that holds `<` or `>` lives in a variable; `[[ ]]`, not `[ ]`, in bash files.
- `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh` prints nothing.
- The repository is public: tests and docs use documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) and RFC1918 LAN addresses, never a real server's address.
- English for code, comments, tests, commit messages and docs. Comments follow the voice of `lib/tunnel.sh`: full sentences that say why.
- Commits: `type(scope): summary`, a body that explains why, no trailers. Stage files by name only. Never stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, `docs/superpowers/2026-*.md`, `docs/superpowers/plans/*-prompt.md`, `review.diff`, `test_exit.sh`.
- TDD: every new test is written first and seen failing for the reason its step names. A test marked **guard** pins behaviour that must not change and passes before and after.
- An existing test changes only where this plan says so, and only because the rule it pinned changed. Any other test that fails is a stop-and-ask, not a fix.
- Bats runs as CI runs it, from the repository root: `bats router/test/*.bats router/test/unit router/test/integration`. Go runs from `server/`: `go vet ./...`, `go test ./... -count=1`, `gofmt -l .` prints nothing. The web check is `cd web && npm ci && npm test && npm run build`.
- In the main session, builds and tests go through the `build-forge:build-runner` agent, model `opus`; subagents are `opus` too. Never Fable.
- No command touches the router before Task 12, and Task 12 runs each step only after the owner's yes. home2 (192.168.1.3) is the agent's own machine and reaches the Claude API through the tunnel.

## Review Focus

1. **Two interfaces whose names share a prefix** (`awg1`, `awg10`): releasing one, or replacing its inbound MSS rule, must leave the other's rules alone. Test: `iftunnel_table_release: leaves another interface's rules alone, a longer name included` (Task 2).
2. **An interface taken off `tunnel_director.interfaces` while its tunnel keeps clients:** the clients are refused, not routed on through the old table; the lookup rule, the route, NAT and MSS go. Test: `tunnel_apply: an interface taken off the list stops routing and its clients are refused` (Task 6).
3. **The firmware's rebuilds:** Merlin's `iptables -t mangle -F` takes the MSS clamps with the marks, and a rebuild of `nat` takes `MASQUERADE`; the next apply puts them back. Tests: `tunnel_apply: after Merlin emptied mangle, the rebuild puts the MSS clamps back` and `tunnel_apply: after the firmware rebuilt nat, the up-to-date path puts MASQUERADE back` (Task 4).
4. **An interface that exists but is not UP yet** (`S51awg1` sets it UP last): no route through it, and `ensure-routes` waits for UP. Tests: `iftunnel_route_ensure: no route through an interface that is not UP` (Task 2), `vpn-director: ensure-routes waits for an interface that is not UP yet` (Task 8).
5. **KeeneticOS while RCI is silent:** `platform_tunnels` answers `main` alone and every tunnel is unknown; a tunnel whose table the platform still names keeps routing instead of being cut off. Test: `tunnel_apply: a tunnel whose table the platform names keeps routing while the platform does not list it` (Task 6).

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `router/opt/vpn-director/lib/iftunnel.sh` (new) | The interface list and its check, table, route, NAT/MSS, state file, status; the `vpd_*` layer | 1, 2, 3 |
| `router/opt/vpn-director/lib/common.sh` | Sources `iftunnel.sh` after `platform.sh` | 1 |
| `router/opt/vpn-director/lib/config.sh` | `TUN_DIR_INTERFACES` | 1 |
| `router/opt/vpn-director/lib/tunnel.sh` | Calls through the layer, hash, state, status; block rules; unknown tunnels; address exclusions | 4, 5, 6, 7 |
| `router/opt/vpn-director/vpn-director.sh` | `cmd_platform` through the layer; `ensure-routes`; the cron job; help | 3, 8 |
| `router/opt/vpn-director/configure.sh` | The wizard's tunnel menu through the layer | 3 |
| `router/jffs/scripts/nat-start` (new) | Merlin hook after a rebuild of `nat` | 8 |
| `router/files.manifest` | Ships `lib/iftunnel.sh` and `jffs/scripts/nat-start` | 1, 8 |
| `router/test/test_helper.bash` | `ip_links` helper | 1 |
| `router/test/mocks/ip` | `link show`, a route store, `prohibit` rules | 1, 2, 5 |
| `router/test/mocks/stateful/iptables` | Knows the `MASQUERADE` and `TCPMSS` targets | 2 |
| `router/test/unit/iftunnel.bats` (new) | The module and the layer | 1, 2, 3 |
| `router/test/unit/tunnel.bats` | New tests and the updates this plan names | 4, 5, 6, 7 |
| `router/test/config.bats` | Loading `tunnel_director.interfaces` | 1 |
| `router/test/integration/vpn_director.bats` | `platform`, `ensure-routes`, the cron job, help | 3, 8 |
| `router/test/unit/configure.bats` | The wizard's menu | 3 |
| `router/test/unit/hooks.bats` | `nat-start` | 8 |
| `server/internal/vpnconfig/vpnconfig.go`, `vpnconfig_test.go` | `TunnelDirectorConfig.Interfaces`; `PlatformTunnel.Type` comment | 9 |
| `server/internal/handler/clients.go`, `clients_test.go` | Bot texts | 9 |
| `web/src/components/ClientsTab.vue` | Web UI texts | 9 |
| `.claude/rules/tunnel-director.md`, `.claude/rules/packet-flow.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/testing.md`, `README.md`, `README.ru.md`, `CLAUDE.md` | Documentation | 10 |

Helpers already in the suite that the tests below use: `load_common`, `load_firewall`, `load_config`, `load_tunnel_module`, `use_stateful_iptables` (`router/test/test_helper.bash`); `load_tunnel_module_with '<tunnels JSON>'` (sources the modules without `config.sh`, so a test may reassign `TUN_DIR_TUNNELS_JSON`), `marks_in_place`, `ovpn_mode <client> <rgw>`, `tun11_addressed` (`router/test/unit/tunnel.bats`); `run_stubbed_cli`, `run_cli` (`router/test/integration/vpn_director.bats`); `load_wizard` (`router/test/unit/configure.bats`); `setup_merlin_hook_env`, `run_hook`, `MERLIN_HOOKS` (`router/test/unit/hooks.bats`). The `ip` mock logs every call to `/tmp/bats_ip_calls.log` and keeps ip rules in `/tmp/bats_test_ip_rules`; the stateless `iptables` mock logs to `/tmp/bats_iptables_calls.log` and answers every `-C` with "missing". The fixture `rt_tables` names `wgc1`, `wgc2`, `ovpnc1`, `ovpnc2`; the `nvram` mock makes `eth0` the WAN and both OpenVPN clients VPN Director mode. Teardown removes `/tmp/bats_test_*` and `/tmp/tunnel_director/*`.

---

### Task 1: The interface list, its check and the table

**Files:**
- Create: `router/opt/vpn-director/lib/iftunnel.sh`, `router/test/unit/iftunnel.bats`
- Modify: `router/opt/vpn-director/lib/common.sh` (end of file), `router/opt/vpn-director/lib/config.sh` (section 4 and the `readonly` list), `router/files.manifest`, `router/test/mocks/ip` (top-level `case`), `router/test/test_helper.bash`
- Test: `router/test/unit/iftunnel.bats`, `router/test/config.bats`

**Interfaces:**
- Consumes: the platform contract's `platform_tunnels`, `platform_lan_ifaces`, `platform_wan_if`; `log` when it is defined.
- Produces (later tasks rely on these names):
  - `IFTUNNEL_TABLE_BASE` (2000).
  - `iftunnel_init` - checks `TUN_DIR_INTERFACES` once per value; always rc 0.
  - `iftunnel_list` - the kept names, one per line.
  - `iftunnel_is <id>` - rc 0 for a kept name.
  - `iftunnel_rejected_reason <id>` - prints why a listed name was rejected; rc 1 otherwise.
  - `iftunnel_table <idx>` - prints `2000 + idx`; rc 1 when idx is not digits.
  - `iftunnel_iface <id>` - prints the id for a kept name; rc 1 otherwise.
  - `iftunnel_info <id>` - three lines `interface`, `1|0`, empty; rc 1 for anything but a kept name.
  - `iftunnel_read_config <file>` - the file's `tunnel_director.interfaces`, space-separated; nothing for a missing file or a wrong type.
  - Private: `_iftunnel_link <iface>` (the `ip -o link show` line, rc 1 when there is none), `_iftunnel_up <line>`, `_iftunnel_mtu <line>`.
  - `TUN_DIR_INTERFACES` from `config.sh` (read-only once loaded).
  - Test helper `ip_links "<iface> <flags> <mtu>"...`.

- [ ] **Step 1: Teach the `ip` mock `link show`, and add `ip_links`**

In `router/test/mocks/ip`, add this branch to the top-level `case "$1" in`, right before its last `*) exit 0 ;;`:

```bash
    link)
        # "ip -o link show <iface>" for the interfaces of BATS_IP_LINKS_FILE, one
        # "<iface> <flags> <mtu>" per line (ip_links in test_helper.bash). A name
        # the file does not list does not exist. Without the file every name
        # prints nothing, as it did before the file existed.
        if [[ ${2:-} == show && -n ${BATS_IP_LINKS_FILE:-} ]]; then
            entry=$(awk -v i="${3:-}" '$1 == i { print; exit }' "$BATS_IP_LINKS_FILE" 2>/dev/null)
            if [[ -z $entry ]]; then
                echo "Device \"${3:-}\" does not exist." >&2
                exit 1
            fi
            read -r l_name l_flags l_mtu <<< "$entry"
            printf '9: %s: %s mtu %s qdisc fq_codel state UNKNOWN mode DEFAULT group default qlen 500\\    link/none \n' \
                "$l_name" "$l_flags" "$l_mtu"
        fi
        ;;
```

In `router/test/test_helper.bash`, after `use_stateful_ipset`, add:

```bash
# ip_links "<iface> <flags> <mtu>"... - for the rest of the test, the interfaces
# the ip mock's "link show" knows, one argument each, e.g.
# "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"; any other name does not exist.
# Without a call the mock prints nothing for every name. Exported, so the
# processes a test starts see the same interfaces.
ip_links() {
    export BATS_IP_LINKS_FILE="$BATS_TEST_TMPDIR/ip_links"
    printf '%s\n' "$@" > "$BATS_IP_LINKS_FILE"
}
```

- [ ] **Step 2: Write the failing tests**

Create `router/test/unit/iftunnel.bats`:

```bash
#!/usr/bin/env bats

load '../test_helper'

# load_iftunnel [interfaces] - common.sh, which sources platform.sh (Merlin: the
# fixture rt_tables lists wgc1 wgc2 ovpnc1 ovpnc2, the nvram mock makes eth0 the
# WAN) and iftunnel.sh, and firewall.sh; tunnel_director.interfaces as config.sh
# leaves it - space-separated, as written - and the state file in the test's own
# directory.
load_iftunnel() {
    load_firewall
    export TUN_DIR_INTERFACES="${1:-}"
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
}

# ============================================================================
# The list and its check
# ============================================================================

@test "iftunnel_list: the listed interfaces in their order" {
    load_iftunnel "awg1 wg0"
    run iftunnel_list
    assert_success
    assert_output $'awg1\nwg0'
}

@test "iftunnel_list: nothing for an empty list" {
    load_iftunnel ""
    run iftunnel_list
    assert_success
    refute_output
}

# Each of these would send Tunnel Director's clients somewhere the list cannot
# mean: a name the kernel refuses matches no interface, main and a firmware
# tunnel are the platform's, the LAN would loop and the WAN would send the
# clients out direct with no tunnel at all.
@test "iftunnel_init: rejects what is no interface tunnel, one WARN each" {
    load_iftunnel "awg1 bad/name main wgc1 br0 eth0 a23456789012345x . .."
    iftunnel_init
    run iftunnel_list
    assert_output "awg1"
    assert_equal "$(grep -c 'tunnel_director.interfaces: .*; ignored' "$LOG_FILE")" 8
    grep -qF "tunnel_director.interfaces: 'wgc1' is a tunnel the firmware manages; ignored" "$LOG_FILE"
    grep -qF "tunnel_director.interfaces: 'br0' is a LAN interface; ignored" "$LOG_FILE"
    grep -qF "tunnel_director.interfaces: 'eth0' is the WAN interface; ignored" "$LOG_FILE"
    grep -qF "tunnel_director.interfaces: 'main' is Tunnel Director's own route, not an interface; ignored" "$LOG_FILE"
}

@test "iftunnel_init: a name listed twice counts once, with no WARN" {
    load_iftunnel "awg1 awg1"
    iftunnel_init
    run iftunnel_list
    assert_output "awg1"
    refute grep -q 'tunnel_director.interfaces' "$LOG_FILE"
}

@test "iftunnel_init: checks a list once, and again when it changes" {
    load_iftunnel "bad/name"
    iftunnel_init
    iftunnel_init
    assert_equal "$(grep -c '; ignored' "$LOG_FILE")" 1
    TUN_DIR_INTERFACES="main"
    iftunnel_init
    assert_equal "$(grep -c '; ignored' "$LOG_FILE")" 2
}

@test "iftunnel_is: a kept name only" {
    load_iftunnel "awg1 main"
    iftunnel_init
    run iftunnel_is awg1
    assert_success
    run iftunnel_is main
    assert_failure
    run iftunnel_is wg0
    assert_failure
    run iftunnel_is ""
    assert_failure
}

@test "iftunnel_rejected_reason: why a listed name was rejected; nothing for any other" {
    load_iftunnel "awg1 br0"
    iftunnel_init
    run iftunnel_rejected_reason br0
    assert_success
    assert_output "'br0' is a LAN interface"
    run iftunnel_rejected_reason awg1
    assert_failure
    refute_output
    run iftunnel_rejected_reason wg0
    assert_failure
}

# ============================================================================
# Table, interface, info
# ============================================================================

@test "iftunnel_table: 2000 + idx; nothing for what is no index" {
    load_iftunnel "awg1"
    run iftunnel_table 0
    assert_output "2000"
    run iftunnel_table 07
    assert_output "2007"
    run iftunnel_table x
    assert_failure
    refute_output
}

@test "iftunnel_iface: the interface itself, for a listed one only" {
    load_iftunnel "awg1"
    run iftunnel_iface awg1
    assert_output "awg1"
    run iftunnel_iface wg0
    assert_failure
}

@test "iftunnel_info: interface, connected while UP, no description" {
    load_iftunnel "awg1 awg2 awg3"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280" "awg2 <POINTOPOINT,NOARP> 1280"
    run iftunnel_info awg1
    assert_success
    assert_output $'interface\n1'
    run iftunnel_info awg2
    assert_output $'interface\n0'
    run iftunnel_info awg3
    assert_output $'interface\n0'
    run iftunnel_info wg0
    assert_failure
}

@test "iftunnel_read_config: the interfaces of a config file; nothing for a wrong type or no file" {
    load_iftunnel ""
    local cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    jq '.tunnel_director.interfaces = ["awg1", 7, "wg0"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$cfg"
    run iftunnel_read_config "$cfg"
    assert_output "awg1 wg0"
    jq '.tunnel_director.interfaces = "awg1"' "$TEST_ROOT/fixtures/vpn-director.json" > "$cfg"
    run iftunnel_read_config "$cfg"
    assert_success
    refute_output
    run iftunnel_read_config "$BATS_TEST_TMPDIR/absent.json"
    assert_success
    refute_output
}
```

Append to `router/test/config.bats`:

```bash
# ============================================================================
# config.sh: tunnel_director.interfaces
# ============================================================================

@test "config.sh: loads tunnel_director.interfaces into TUN_DIR_INTERFACES" {
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-interfaces.json"
    jq '.tunnel_director.interfaces = ["awg1", "wg0"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [ "$TUN_DIR_INTERFACES" = "awg1 wg0" ]
}

# A string where the array belongs must not end the load under set -e: the rest
# of the configuration still applies, and the list is empty.
@test "config.sh: an interface list of the wrong type loads as empty, and the rest loads" {
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-interfaces-string.json"
    jq '.tunnel_director.interfaces = "awg1"' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [ -z "$TUN_DIR_INTERFACES" ]
    [ "$TUN_DIR_CHAIN" = "TUN_DIR" ]
}

@test "config.sh: no interface list loads as empty" {
    load_config
    [ -z "$TUN_DIR_INTERFACES" ]
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `bats router/test/unit/iftunnel.bats router/test/config.bats`
Expected: every `iftunnel_*` test fails with `iftunnel_list: command not found` (or the name of the function it calls); `config.sh: loads tunnel_director.interfaces into TUN_DIR_INTERFACES` fails on `[ "$TUN_DIR_INTERFACES" = "awg1 wg0" ]`. `config.sh: no interface list loads as empty` passes already (the variable is unset) - that is fine.

- [ ] **Step 4: Create `lib/iftunnel.sh`**

Create `router/opt/vpn-director/lib/iftunnel.sh`:

```bash
#!/usr/bin/env bash

###################################################################################################
# iftunnel.sh - interface tunnels: Linux interfaces Tunnel Director routes through
# -------------------------------------------------------------------------------------------------
# tunnel_director.interfaces names interfaces no firmware manages - the AmneziaWG awg1 an Entware
# init script brings up, say. Each is a tunnel like the firmware's own: Tunnel Director gives it a
# slot, and this module its routing table (IFTUNNEL_TABLE_BASE + idx), its default route and the
# NAT and MSS rules the firmware gives its own tunnels. The same on every platform: nothing here
# asks the firmware anything but which names are its own, its LAN and its WAN.
#
# Sourced by common.sh right after platform.sh, and by configure.sh, which sources platform.sh
# without common.sh: nothing here may need common.sh when it is sourced, and log is called only
# where it exists. The NAT and MSS rules use firewall.sh, which an apply loads.
###################################################################################################

# The first routing table of an interface tunnel: table = IFTUNNEL_TABLE_BASE + idx. Asuswrt-Merlin
# names 100, 111-120 and 200 in rt_tables; KeeneticOS's Tunnel Director tables are 2000 + idx as
# well (KEENETIC_TABLE_BASE), and a slot belongs to one tunnel, so no two tunnels share a table.
IFTUNNEL_TABLE_BASE=2000

# Set by iftunnel_init: the raw list it checked, the names it kept (space-separated) and one
# "<name><TAB><reason>" line for each name it rejected.
_iftunnel_checked=""
_iftunnel_checked_once=0
_iftunnel_valid=""
_iftunnel_rejected=""

# _iftunnel_warn <message> - a WARN where log exists; configure.sh has no common.sh.
_iftunnel_warn() {
    if declare -F log >/dev/null 2>&1; then
        log -l WARN "$1"
    fi
}

# -------------------------------------------------------------------------------------------------
# iftunnel_init - check TUN_DIR_INTERFACES once per value
# -------------------------------------------------------------------------------------------------
# A name is kept when the kernel would take it (1-15 characters of A-Za-z0-9_.-, not "." or "..")
# and nothing else claims it: not main, not a tunnel the platform lists, not a LAN interface, not
# the WAN (checked only when the platform names one). A rejected name logs one WARN and is no
# tunnel: a tunnel key that names it is one the platform does not know. A name listed twice
# counts once. Call it in the shell that goes on to use the list - tunnel.sh does, in
# _tunnel_init - so the WARNs come once and every $(iftunnel_*) or $(vpd_*) after it finds the
# list checked.
# -------------------------------------------------------------------------------------------------
iftunnel_init() {
    local raw="${TUN_DIR_INTERFACES:-}" name reason platform_ids lan wan
    local name_re='^[A-Za-z0-9_.-]{1,15}$'
    local -a names=()
    if [[ $_iftunnel_checked_once -eq 1 && $_iftunnel_checked == "$raw" ]]; then
        return 0
    fi
    _iftunnel_checked="$raw"
    _iftunnel_checked_once=1
    _iftunnel_valid=""
    _iftunnel_rejected=""
    [[ -n $raw ]] || return 0

    # Asked once per list. A platform that cannot answer rejects nothing on its count.
    platform_ids=" $(platform_tunnels 2>/dev/null | xargs || true) "
    lan=" $(platform_lan_ifaces 2>/dev/null | xargs || true) "
    wan="$(platform_wan_if 2>/dev/null)" || wan=""
    read -ra names <<< "$raw"
    for name in "${names[@]}"; do
        reason=""
        if [[ ! $name =~ $name_re || $name == . || $name == .. ]]; then
            reason="'$name' is no interface name (1-15 characters: letters, digits, _ . -)"
        elif [[ $name == main ]]; then
            reason="'main' is Tunnel Director's own route, not an interface"
        elif [[ $platform_ids == *" $name "* ]]; then
            reason="'$name' is a tunnel the firmware manages"
        elif [[ $lan == *" $name "* ]]; then
            reason="'$name' is a LAN interface"
        elif [[ -n $wan && $name == "$wan" ]]; then
            reason="'$name' is the WAN interface"
        elif [[ " $_iftunnel_valid " == *" $name "* ]]; then
            continue
        fi
        if [[ -n $reason ]]; then
            _iftunnel_rejected+="$name"$'\t'"$reason"$'\n'
            _iftunnel_warn "tunnel_director.interfaces: $reason; ignored"
            continue
        fi
        _iftunnel_valid+="${_iftunnel_valid:+ }$name"
    done
    return 0
}

# iftunnel_list - the kept names, one per line
iftunnel_list() {
    local -a valid=()
    iftunnel_init
    read -ra valid <<< "$_iftunnel_valid"
    [[ ${#valid[@]} -gt 0 ]] || return 0
    printf '%s\n' "${valid[@]}"
}

# iftunnel_is <id> - is <id> a kept interface name?
iftunnel_is() {
    [[ -n ${1:-} ]] || return 1
    iftunnel_init
    [[ " $_iftunnel_valid " == *" $1 "* ]]
}

# iftunnel_rejected_reason <id> - why the list's <id> was rejected; rc 1 for a kept name or one
# the list does not hold. For the warning tunnel.sh logs about a tunnel the platform does not know.
iftunnel_rejected_reason() {
    local name reason
    iftunnel_init
    while IFS=$'\t' read -r name reason; do
        [[ -n $name && $name == "${1:-}" ]] || continue
        printf '%s\n' "$reason"
        return 0
    done <<< "$_iftunnel_rejected"
    return 1
}

# iftunnel_table <idx> - the routing table of the interface tunnel in slot <idx>
iftunnel_table() {
    [[ ${1:-} =~ ^[0-9]+$ ]] || return 1
    printf '%d\n' $((IFTUNNEL_TABLE_BASE + 10#$1))
}

# iftunnel_iface <id> - the Linux interface of an interface tunnel: its name
iftunnel_iface() {
    iftunnel_is "${1:-}" || return 1
    printf '%s\n' "$1"
}

# _iftunnel_link <iface> - the interface's line of "ip -o link show", read whole (the pipefail
# pitfall in shell-conventions.md); rc 1 when there is none.
_iftunnel_link() {
    local out
    out="$(ip -o link show "${1:-}" 2>/dev/null)" || return 1
    [[ -n $out ]] || return 1
    printf '%s\n' "$out"
}

# _iftunnel_up <link line> - does it carry UP as a whole flag ("<...,UP,LOWER_UP>"), not the tail
# of LOWER_UP? The pattern of Merlin's platform_tunnel_info, in a variable because of its < >.
_iftunnel_up() {
    local re='<([^>]*,)?UP[,>]'
    [[ ${1:-} =~ $re ]]
}

# _iftunnel_mtu <link line> - the MTU it names; rc 1 when it names none
_iftunnel_mtu() {
    local re=' mtu ([0-9]+)'
    [[ ${1:-} =~ $re ]] || return 1
    printf '%s\n' "${BASH_REMATCH[1]}"
}

# iftunnel_info <id> - three lines, as platform_tunnel_info prints them: the type "interface",
# connected (1 while the interface exists and is UP) and an empty description.
iftunnel_info() {
    local link connected=0
    iftunnel_is "${1:-}" || return 1
    if link="$(_iftunnel_link "$1")" && _iftunnel_up "$link"; then
        connected=1
    fi
    printf '%s\n%s\n%s\n' interface "$connected" ""
}

# iftunnel_read_config <file> - tunnel_director.interfaces of a config file, space-separated, for
# the callers that load no config.sh (cmd_platform, configure.sh). The jq program is config.sh's:
# anything but an array of strings reads as no list.
iftunnel_read_config() {
    [[ -f ${1:-} ]] || return 0
    jq -r '.tunnel_director.interfaces // [] | if type == "array" then .[] | select(type == "string") else empty end' \
        "$1" 2>/dev/null | tr '\n' ' ' | sed 's/ $//' || true
}
```

- [ ] **Step 5: Source it from `common.sh`, load the list in `config.sh`, ship it**

At the end of `router/opt/vpn-director/lib/common.sh`, after `. "${BASH_SOURCE[0]%/*}/platform.sh"`, add:

```bash

###################################################################################################
# Interface tunnels and the vpd_* layer (lib/iftunnel.sh)
###################################################################################################
# After platform.sh: the layer hands every id that is no interface tunnel to the platform
# contract. Not optional either, for the reason given above.
if [[ ! -f "${BASH_SOURCE[0]%/*}/iftunnel.sh" ]]; then
    log -l ERROR "Interface tunnel library not found: ${BASH_SOURCE[0]%/*}/iftunnel.sh - re-run install.sh to reinstall it"
    # shellcheck disable=SC2317
    return 1 2>/dev/null || exit 1
fi
# shellcheck source=iftunnel.sh
. "${BASH_SOURCE[0]%/*}/iftunnel.sh"
```

In `router/opt/vpn-director/lib/config.sh`, section 4, right before `IPS_BDR_DIR=$(_cfg '.data_dir')`, add:

```bash
# tunnel_director.interfaces, space-separated as written: lib/iftunnel.sh checks
# the names (it asks the platform, which this file cannot) and tunnel.sh hashes
# the list as written. The jq program is iftunnel_read_config's; anything but an
# array of strings loads as no list, never as an error that ends the load.
TUN_DIR_INTERFACES=$(jq -r '.tunnel_director.interfaces // [] | if type == "array" then .[] | select(type == "string") else empty end' "$VPD_CONFIG_FILE" | tr '\n' ' ' | sed 's/ $//')
```

and in section 9 change the line `TUN_DIR_TUNNELS_JSON IPS_BDR_DIR \` to `TUN_DIR_TUNNELS_JSON TUN_DIR_INTERFACES IPS_BDR_DIR \`.

In `router/files.manifest`, after the line `keenetic router/opt/vpn-director/lib/platform/keenetic.sh`, add:

```
common   router/opt/vpn-director/lib/iftunnel.sh
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `bats router/test/unit/iftunnel.bats router/test/config.bats`
Expected: all pass.

- [ ] **Step 7: Lint the new file and run the whole suite (guard)**

Run: `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh`
Expected: no output.

Run: `bats router/test/*.bats router/test/unit router/test/integration`
Expected: all pass - with `TUN_DIR_INTERFACES` empty nothing calls the module yet.

Run: `cd server && go test ./internal/updater/ -count=1`
Expected: PASS (`TestRepoManifest_ShipsEveryRouterFile` finds `lib/iftunnel.sh` in the manifest).

- [ ] **Step 8: Commit**

```bash
git add router/opt/vpn-director/lib/iftunnel.sh router/opt/vpn-director/lib/common.sh \
    router/opt/vpn-director/lib/config.sh router/files.manifest router/test/mocks/ip \
    router/test/test_helper.bash router/test/unit/iftunnel.bats router/test/config.bats
git commit -m "feat(tunnel): the list of interface tunnels, its check and the table" -m "tunnel_director.interfaces names Linux interfaces no firmware manages - the
AmneziaWG awg1 on the RT-AX86U first. lib/iftunnel.sh checks the names once
per list (a name the kernel refuses, main, a firmware tunnel, the LAN and the
WAN are rejected with one WARN each) and gives an interface tunnel table
2000 + idx, beside the KeeneticOS tables of the same numbering."
```

---

### Task 2: An interface tunnel's route, NAT and MSS

**Files:**
- Modify: `router/opt/vpn-director/lib/iftunnel.sh` (constants after `IFTUNNEL_TABLE_BASE`; functions appended), `router/test/mocks/ip` (`route` branch), `router/test/mocks/stateful/iptables` (`target_known`)
- Test: `router/test/unit/iftunnel.bats`

**Interfaces:**
- Consumes: Task 1's `iftunnel_table`, `iftunnel_is`, `_iftunnel_link`, `_iftunnel_up`, `_iftunnel_mtu`; `lib/firewall.sh`'s `ensure_fw_rule`, `find_fw_rules`, `purge_fw_rules`.
- Produces:
  - `IFTUNNEL_STATE_FILE`, `IFTUNNEL_CRON_MARKER` (environment overrides kept).
  - `iftunnel_route_ensure <iface> <idx>` - rc 0 when the route went in; 1 when the interface is missing or not UP, or the route did not go in. NAT and the outbound clamp go in either way.
  - `iftunnel_table_release <iface> <idx>` - rc 0 (1 only for an idx that is no number).
  - `iftunnel_state_write <tables-file>` - writes or removes `IFTUNNEL_STATE_FILE`.
  - `iftunnel_recorded <iface>` - prints the recorded table; rc 1 when none.
  - `iftunnel_routes_missing` - prints the recorded interfaces that are UP with no default in their table; rc 1 when none.
  - `iftunnel_status` - one line per recorded interface tunnel, or `No interface tunnels applied.`
  - Private: `_iftunnel_default_route <table>`, `_iftunnel_mss_pattern <iface>`, `_iftunnel_rules_ensure <iface>`, `_iftunnel_mss_in_ensure <iface> <mtu>`, `_iftunnel_rules_release <iface>`.

- [ ] **Step 1: Teach the mocks routes and the two targets**

In `router/test/mocks/ip`, after the line `RT_TABLES="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}"`, add:

```bash
# Routes the kernel would hold per table, "<table><TAB><route>" per line: what
# add and replace put in, del and flush took out. Cleared by the test_helper
# teardown, which removes /tmp/bats_test_*.
ROUTES_FILE="${BATS_IP_ROUTES_FILE:-/tmp/bats_test_ip_routes}"

# _route_args <args...> - split "ip route add|replace|del ..." into the table
# (_rt_table) and the route without it (_rt_spec); _rt_dst is the destination,
# the route's first word.
_route_args() {
    _rt_table=""
    _rt_spec=""
    while [[ $# -gt 0 ]]; do
        if [[ $1 == table ]]; then
            _rt_table="${2:-}"
            shift 2
            continue
        fi
        _rt_spec+="${_rt_spec:+ }$1"
        shift
    done
    _rt_dst="${_rt_spec%% *}"
}

# _routes_drop <table> [dst] - remove the routes of a table, or its route to dst
_routes_drop() {
    [[ -f $ROUTES_FILE ]] || return 0
    awk -F'\t' -v t="$1" -v d="${2:-}" \
        '!($1 == t && (d == "" || $2 == d || index($2, d " ") == 1))' \
        "$ROUTES_FILE" > "$ROUTES_FILE.tmp" && mv "$ROUTES_FILE.tmp" "$ROUTES_FILE"
}
```

and replace the whole `route)` branch of the top-level `case` with:

```bash
    route)
        case "$2" in
            show)
                # The default route of a family, as a Keenetic reads its WAN
                # and its IPv6 state; a table, as its routes stand; anything
                # else is empty.
                if [[ ${3:-} == default ]]; then
                    case "$FAMILY" in
                        -4) v="${BATS_IP_DEFAULT_ROUTE4-default via 192.168.100.1 dev eth2.4  metric 1000}" ;;
                        -6) v="${BATS_IP_DEFAULT_ROUTE6-}" ;;
                        *)  v="" ;;
                    esac
                    [[ -n $v ]] && printf '%s\n' "$v"
                elif [[ ${3:-} == table && -n ${4:-} ]]; then
                    awk -F'\t' -v t="$4" '$1 == t { print $2 }' "$ROUTES_FILE" 2>/dev/null || true
                else
                    echo ""
                fi
                ;;
            add|replace)
                _route_args "${@:3}"
                if [[ -n $_rt_table ]]; then
                    _routes_drop "$_rt_table" "$_rt_dst"
                    printf '%s\t%s\n' "$_rt_table" "$_rt_spec" >> "$ROUTES_FILE"
                fi
                exit 0
                ;;
            del)
                _route_args "${@:3}"
                [[ -z $_rt_table ]] || _routes_drop "$_rt_table" "$_rt_dst"
                exit 0
                ;;
            flush)
                [[ ${3:-} != table || -z ${4:-} ]] || _routes_drop "$4"
                exit 0
                ;;
        esac
        ;;
```

In `router/test/mocks/stateful/iptables`, in `target_known`, change `RETURN|ACCEPT|DROP|REJECT|MARK|CONNMARK|TPROXY|PPE|LOG) ;;` to `RETURN|ACCEPT|DROP|REJECT|MARK|CONNMARK|TPROXY|PPE|LOG|MASQUERADE|TCPMSS) ;;`.

- [ ] **Step 2: Write the failing tests**

Append to `router/test/unit/iftunnel.bats`:

```bash
# ============================================================================
# Route, NAT and MSS
# ============================================================================

AWG1_UP="awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"

@test "iftunnel_route_ensure: the default route of table 2000+idx, MASQUERADE and both MSS clamps" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    run iftunnel_route_ensure awg1 1
    assert_success
    run ip route show table 2001
    assert_output "default dev awg1"
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu'
    assert_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240'
}

@test "iftunnel_route_ensure: a second call adds no rule twice" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    iftunnel_route_ensure awg1 1
    iftunnel_route_ensure awg1 1
    run iptables -t mangle -S FORWARD
    assert_equal "$(grep -c 'TCPMSS' <<< "$output")" 2
    run iptables -t nat -S POSTROUTING
    assert_equal "$(grep -c 'MASQUERADE' <<< "$output")" 1
}

# "ip link del" takes the route along, and the rules have to be in place for the
# moment the interface is back: iptables matches its name, not the interface.
@test "iftunnel_route_ensure: without the interface, the rules that need none and no route" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links
    run iftunnel_route_ensure awg1 1
    assert_failure
    run ip route show table 2001
    refute_output
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu'
    refute_output --partial 'TCPMSS --set-mss'
}

# Review Focus 4: S51awg1 sets the interface UP last, and the kernel refuses a
# route through one that is down.
@test "iftunnel_route_ensure: no route through an interface that is not UP" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "awg1 <POINTOPOINT,NOARP> 1280"
    : > /tmp/bats_ip_calls.log
    run iftunnel_route_ensure awg1 1
    assert_failure
    refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
}

@test "iftunnel_route_ensure: a new MTU replaces the inbound clamp" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    iftunnel_route_ensure awg1 1
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1420"
    iftunnel_route_ensure awg1 1
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1380'
    refute_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240'
}

@test "iftunnel_table_release: the route and the three rules gone" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    iftunnel_route_ensure awg1 1
    run iftunnel_table_release awg1 1
    assert_success
    run ip route show table 2001
    refute_output
    run iptables -t nat -S POSTROUTING
    assert_output '-P POSTROUTING ACCEPT'
    run iptables -t mangle -S FORWARD
    assert_output '-P FORWARD ACCEPT'
}

# Review Focus 1: the inbound clamp is found by a pattern, and "awg1" must not
# take "awg10"'s.
@test "iftunnel_table_release: leaves another interface's rules alone, a longer name included" {
    load_iftunnel "awg1 awg10"
    use_stateful_iptables
    ip_links "$AWG1_UP" "awg10 <POINTOPOINT,NOARP,UP,LOWER_UP> 1420"
    iftunnel_route_ensure awg1 1
    iftunnel_route_ensure awg10 2
    iftunnel_table_release awg1 1
    run ip route show table 2002
    assert_output "default dev awg10"
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg10 -j MASQUERADE'
    refute_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -i awg10 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1380'
    assert_line '-A FORWARD -o awg10 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu'
    refute_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240'
}

# ============================================================================
# The record of the applied interface tunnels
# ============================================================================

@test "iftunnel_state_write: the interface tunnels of a slot record, as <iface> <table>" {
    load_iftunnel "awg1"
    printf '0 wgc1\n1 awg1\n' > "$BATS_TEST_TMPDIR/slots"
    iftunnel_state_write "$BATS_TEST_TMPDIR/slots"
    run cat "$IFTUNNEL_STATE_FILE"
    assert_output "awg1 2001"
}

@test "iftunnel_state_write: no interface tunnel, no file" {
    load_iftunnel "awg1"
    printf '1 awg1\n' > "$BATS_TEST_TMPDIR/slots"
    iftunnel_state_write "$BATS_TEST_TMPDIR/slots"
    printf '0 wgc1\n' > "$BATS_TEST_TMPDIR/slots"
    iftunnel_state_write "$BATS_TEST_TMPDIR/slots"
    [ ! -e "$IFTUNNEL_STATE_FILE" ]
}

@test "iftunnel_recorded: the recorded table; nothing for an interface on no record" {
    load_iftunnel ""
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    run iftunnel_recorded awg1
    assert_success
    assert_output "2001"
    run iftunnel_recorded awg2
    assert_failure
    refute_output
}

@test "iftunnel_routes_missing: an interface UP with no default in its table" {
    load_iftunnel ""
    printf 'awg1 2001\nawg2 2002\nawg3 2003\nawg4 2004\n' > "$IFTUNNEL_STATE_FILE"
    ip_links "$AWG1_UP" "awg2 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280" "awg3 <POINTOPOINT,NOARP> 1280"
    ip route replace default dev awg2 table 2002
    run iftunnel_routes_missing
    assert_success
    assert_output "awg1"
}

@test "iftunnel_routes_missing: nothing to say with every route in place, or no record" {
    load_iftunnel ""
    run iftunnel_routes_missing
    assert_failure
    refute_output
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    ip_links "$AWG1_UP"
    ip route replace default dev awg1 table 2001
    run iftunnel_routes_missing
    assert_failure
    refute_output
}

@test "iftunnel_status: the table, route and rules of each recorded interface tunnel" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    iftunnel_route_ensure awg1 1
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    run iftunnel_status
    assert_success
    assert_output "awg1: table 2001, route default dev awg1, NAT yes, MSS out yes, MSS in yes"
}

@test "iftunnel_status: says so when none is applied" {
    load_iftunnel ""
    run iftunnel_status
    assert_output "No interface tunnels applied."
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `bats router/test/unit/iftunnel.bats`
Expected: the Task 1 tests pass; the new ones fail with `iftunnel_route_ensure: command not found` and the like.

- [ ] **Step 4: Implement**

In `router/opt/vpn-director/lib/iftunnel.sh`, right after `IFTUNNEL_TABLE_BASE=2000`, add:

```bash

# Where the last apply recorded its interface tunnels, "<iface> <table>" per line, and the marker
# of the cron job that puts their routes back (vpn-director.sh, _routes_cron_sync). Beside
# TUN_DIR_TABLES (lib/ipset.sh), named here because "ensure-routes" reads them every minute
# without loading ipset.sh or the config.
IFTUNNEL_STATE_FILE="${IFTUNNEL_STATE_FILE:-/tmp/tunnel_director/iftunnels}"
IFTUNNEL_CRON_MARKER="${IFTUNNEL_CRON_MARKER:-/tmp/tunnel_director/routes_cron}"
```

Append to the end of the file:

```bash

###################################################################################################
# Route, NAT and MSS of an interface tunnel
###################################################################################################

# _iftunnel_default_route <table> - the table's default route, the listing read whole; rc 1 when
# there is none.
_iftunnel_default_route() {
    local routes line
    routes="$(ip route show table "${1:-}" 2>/dev/null)" || return 1
    while IFS= read -r line; do
        if [[ $line == default* ]]; then
            printf '%s\n' "$line"
            return 0
        fi
    done <<< "$routes"
    return 1
}

# _iftunnel_mss_pattern <iface> - the ERE that finds the inbound --set-mss rule whatever its MSS,
# against "iptables -S" lines. The dots of the name escaped, or "wg0.5" would take "wg0x5"'s rule;
# the space after the name keeps "awg1" off "awg10"'s.
_iftunnel_mss_pattern() {
    printf -- '-i %s -p tcp .*-j TCPMSS --set-mss ' "${1//./\\.}"
}

# _iftunnel_rules_ensure <iface> - MASQUERADE out of the interface and the outbound MSS clamp, at
# the head of their chains, where no firmware rule takes the packet first. Neither needs the
# interface: iptables matches its name, so both go in before it exists and outlive its
# re-creation. A rule that does not go in logs its own ERROR (ensure_fw_rule); the route does not
# depend on it.
_iftunnel_rules_ensure() {
    ensure_fw_rule -q nat POSTROUTING -I 1 -o "$1" -j MASQUERADE || true
    ensure_fw_rule -q mangle FORWARD -I 1 -o "$1" -p tcp -m tcp --tcp-flags SYN,RST SYN \
        -j TCPMSS --clamp-mss-to-pmtu || true
}

# _iftunnel_mss_in_ensure <iface> <mtu> - the inbound SYN-ACK clamp, MTU - 40: there
# --clamp-mss-to-pmtu would read the LAN's MTU. One such rule per interface; another MSS replaces
# it. Not sync_fw_rule: its position check would re-insert the rule whenever another went in
# above it.
_iftunnel_mss_in_ensure() {
    local iface="$1" mss=$(( $2 - 40 )) pattern have
    local -a want=(-i "$iface" -p tcp -m tcp --tcp-flags "SYN,RST" SYN -j TCPMSS --set-mss "$mss")
    pattern="$(_iftunnel_mss_pattern "$iface")"
    have="$(find_fw_rules "mangle FORWARD" "$pattern")"
    [[ $have != "-A FORWARD ${want[*]}" ]] || return 0
    [[ -z $have ]] || purge_fw_rules -q "mangle FORWARD" "$pattern"
    ensure_fw_rule -q mangle FORWARD -I 1 "${want[@]}" || true
}

# _iftunnel_rules_release <iface> - the three rules gone. Removing a rule that is not there is no
# error: a release can come for a slot that was never ensured.
_iftunnel_rules_release() {
    ensure_fw_rule -q nat POSTROUTING -D -o "$1" -j MASQUERADE || true
    ensure_fw_rule -q mangle FORWARD -D -o "$1" -p tcp -m tcp --tcp-flags SYN,RST SYN \
        -j TCPMSS --clamp-mss-to-pmtu || true
    purge_fw_rules -q "mangle FORWARD" "$(_iftunnel_mss_pattern "$1")" || true
}

# -------------------------------------------------------------------------------------------------
# iftunnel_route_ensure <iface> <idx> - the NAT and MSS rules, then the default route of its table
# -------------------------------------------------------------------------------------------------
# Idempotent: tunnel.sh calls it for every applied interface tunnel on every apply, so a firewall
# rebuild of the firmware gets the rules back with the next apply, and a re-created interface gets
# its route back once an apply comes (ensure-routes brings one within a minute). Returns 1 when
# the interface is missing or not UP - "ip link del" takes the route along, and the kernel refuses
# one through a down interface - or when the route does not go in.
# -------------------------------------------------------------------------------------------------
iftunnel_route_ensure() {
    local iface="${1:-}" table link mtu
    table="$(iftunnel_table "${2:-}")" || return 1
    _iftunnel_rules_ensure "$iface"
    link="$(_iftunnel_link "$iface")" || return 1
    _iftunnel_up "$link" || return 1
    if mtu="$(_iftunnel_mtu "$link")"; then
        _iftunnel_mss_in_ensure "$iface" "$mtu"
    fi
    ip route replace default dev "$iface" table "$table" 2>/dev/null
}

# iftunnel_table_release <iface> <idx> - the route and the three rules gone. A table that holds
# nothing is no error: tunnel.sh releases slots that were never ensured.
iftunnel_table_release() {
    local table
    table="$(iftunnel_table "${2:-}")" || return 1
    ip route flush table "$table" 2>/dev/null || true
    _iftunnel_rules_release "${1:-}"
    return 0
}

###################################################################################################
# The record of the applied interface tunnels
###################################################################################################

# iftunnel_state_write <tables-file> - record the interface tunnels among the "<idx> <id>" lines of
# TUN_DIR_TABLES: "<iface> <table>" per line in IFTUNNEL_STATE_FILE, through a file beside it and
# a rename. No interface tunnel, no file. "ensure-routes" reads it without the config, and the
# layer reads it to know an interface the list no longer names - so an apply writes it after its
# releases.
iftunnel_state_write() {
    local idx id lines=""
    if [[ -f ${1:-} ]]; then
        while read -r idx id _; do
            [[ -n $id ]] || continue
            iftunnel_is "$id" || continue
            lines+="$id $(iftunnel_table "$idx")"$'\n'
        done < "$1"
    fi
    if [[ -z $lines ]]; then
        rm -f "$IFTUNNEL_STATE_FILE"
        return 0
    fi
    mkdir -p "$(dirname "$IFTUNNEL_STATE_FILE")" &&
        printf '%s' "$lines" > "$IFTUNNEL_STATE_FILE.new" &&
        mv -f "$IFTUNNEL_STATE_FILE.new" "$IFTUNNEL_STATE_FILE"
}

# iftunnel_recorded <iface> - the table the last apply recorded for an interface tunnel; rc 1 when
# it recorded none.
iftunnel_recorded() {
    local iface table
    [[ -n ${1:-} && -f $IFTUNNEL_STATE_FILE ]] || return 1
    while read -r iface table _; do
        if [[ $iface == "$1" && -n $table ]]; then
            printf '%s\n' "$table"
            return 0
        fi
    done < "$IFTUNNEL_STATE_FILE"
    return 1
}

# iftunnel_routes_missing - the recorded interface tunnels that are UP with no default in their
# table, one per line: interfaces re-created since the last apply. An interface that is not UP yet
# is left for a later look - a route through it would be refused. rc 1 when there is none.
iftunnel_routes_missing() {
    local iface table link found=1
    [[ -s $IFTUNNEL_STATE_FILE ]] || return 1
    while read -r iface table _; do
        [[ -n $iface && -n $table ]] || continue
        link="$(_iftunnel_link "$iface")" || continue
        _iftunnel_up "$link" || continue
        ! _iftunnel_default_route "$table" >/dev/null || continue
        printf '%s\n' "$iface"
        found=0
    done < "$IFTUNNEL_STATE_FILE"
    return "$found"
}

# iftunnel_status - one line per recorded interface tunnel: its table, its route, its rules
iftunnel_status() {
    local iface table route nat mss_out mss_in
    if [[ ! -s $IFTUNNEL_STATE_FILE ]]; then
        printf '%s\n' "No interface tunnels applied."
        return 0
    fi
    while read -r iface table _; do
        [[ -n $iface && -n $table ]] || continue
        route="$(_iftunnel_default_route "$table")" || route="none"
        nat=no
        mss_out=no
        mss_in=no
        if iptables -t nat -C POSTROUTING -o "$iface" -j MASQUERADE 2>/dev/null; then
            nat=yes
        fi
        if iptables -t mangle -C FORWARD -o "$iface" -p tcp -m tcp --tcp-flags SYN,RST SYN \
            -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
            mss_out=yes
        fi
        if [[ -n $(find_fw_rules "mangle FORWARD" "$(_iftunnel_mss_pattern "$iface")") ]]; then
            mss_in=yes
        fi
        printf '%s: table %s, route %s, NAT %s, MSS out %s, MSS in %s\n' \
            "$iface" "$table" "$route" "$nat" "$mss_out" "$mss_in"
    done < "$IFTUNNEL_STATE_FILE"
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `bats router/test/unit/iftunnel.bats`
Expected: all pass.

- [ ] **Step 6: Lint and run the whole suite (guard)**

Run: `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh` - no output.
Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass. The route store changes nothing for the existing tests: they read the call log, and `ip route show default` answers as before.

- [ ] **Step 7: Commit**

```bash
git add router/opt/vpn-director/lib/iftunnel.sh router/test/mocks/ip \
    router/test/mocks/stateful/iptables router/test/unit/iftunnel.bats
git commit -m "feat(tunnel): an interface tunnel's route, NAT and MSS clamps" -m "The firmware gives its own tunnels MASQUERADE; an interface it does not manage
gets nothing, and a WireGuard peer drops the LAN's addresses. iftunnel_route_ensure
puts MASQUERADE and the outbound clamp in whether the interface exists or not -
iptables matches the name - and the inbound clamp and the default route once it
is UP. A record of the applied interface tunnels lets ensure-routes find a
re-created interface without loading the config."
```

---

### Task 3: The `vpd_*` layer; `platform` and the wizard list interface tunnels

**Files:**
- Modify: `router/opt/vpn-director/lib/iftunnel.sh` (layer appended), `router/opt/vpn-director/vpn-director.sh` (`cmd_platform`), `router/opt/vpn-director/configure.sh` (sourcing, `wizard_tunnel_choices`, `wizard_tunnel_by_number`)
- Test: `router/test/unit/iftunnel.bats`, `router/test/integration/vpn_director.bats`, `router/test/unit/configure.bats`

**Interfaces:**
- Consumes: Tasks 1-2.
- Produces (the core calls only these from Task 4 on):
  - `vpd_tunnels` - firmware ids without `main`, then `iftunnel_list`, then `main`; the rc of `platform_tunnels`.
  - `vpd_tunnel_iface <id>`, `vpd_tunnel_info <id>` - as the platform functions.
  - `vpd_tunnel_table <id> <idx>` - interface: `iftunnel_table`; an id `iftunnel_recorded` knows: the recorded table; else `platform_tunnel_table`.
  - `vpd_tunnel_route_ensure <id> <idx> [gateway]` - interface: `iftunnel_route_ensure <id> <idx>`; else `platform_tunnel_route_ensure`.
  - `vpd_tunnel_table_release <id> <idx>` - interface or recorded: `iftunnel_table_release`; else `platform_tunnel_table_release`.
  - `vpd_tunnel_unlisted_reason <id>` - `iftunnel_rejected_reason`, else `platform_tunnel_unlisted_reason`.
  - `vpd_tunnel_retired <id>` - rc 0 for an id the last apply recorded and the list no longer names.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/iftunnel.bats`:

```bash
# ============================================================================
# The vpd_* layer
# ============================================================================

@test "vpd_tunnels: the platform's tunnels, then the interfaces, main last" {
    load_iftunnel "awg1"
    iftunnel_init
    run vpd_tunnels
    assert_success
    assert_output $'wgc1\nwgc2\novpnc1\novpnc2\nawg1\nmain'
}

@test "vpd_tunnels: an incomplete platform inventory still fails, with the interfaces and main" {
    load_iftunnel "awg1"
    export RT_TABLES_FILE="$BATS_TEST_TMPDIR/absent"
    iftunnel_init
    run vpd_tunnels
    assert_failure
    assert_output $'awg1\nmain'
}

@test "vpd_tunnel_*: an interface goes to iftunnel_*, any other id to the platform" {
    load_iftunnel "awg1"
    iftunnel_init
    ip_links "$AWG1_UP"
    run vpd_tunnel_table awg1 3
    assert_output "2003"
    run vpd_tunnel_table wgc1 3
    assert_output "wgc1"
    run vpd_tunnel_iface awg1
    assert_output "awg1"
    run vpd_tunnel_iface ovpnc1
    assert_output "tun11"
    run vpd_tunnel_info awg1
    assert_output $'interface\n1'
    iftunnel_route_ensure() { echo "iftunnel $*"; }
    platform_tunnel_route_ensure() { echo "platform $*"; }
    run vpd_tunnel_route_ensure awg1 3 10.0.0.1
    assert_output "iftunnel awg1 3"
    run vpd_tunnel_route_ensure wgc1 0 10.0.0.1
    assert_output "platform wgc1 0 10.0.0.1"
}

@test "vpd_tunnel_table_release: an interface, one the last apply recorded, and the platform's" {
    load_iftunnel "awg1"
    iftunnel_init
    printf 'awg1 2001\nawg9 2002\n' > "$IFTUNNEL_STATE_FILE"
    iftunnel_table_release() { echo "iftunnel $*"; }
    platform_tunnel_table_release() { echo "platform $*"; }
    run vpd_tunnel_table_release awg1 1
    assert_output "iftunnel awg1 1"
    run vpd_tunnel_table_release awg9 2
    assert_output "iftunnel awg9 2"
    run vpd_tunnel_table_release wgc1 0
    assert_output "platform wgc1 0"
}

# Taken off the list, an interface is no tunnel any more - but its slot still
# holds a rule into its table until an apply releases it, and that needs the
# table's number.
@test "vpd_tunnel_table: an interface the list no longer names keeps its recorded table" {
    load_iftunnel ""
    iftunnel_init
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    run vpd_tunnel_table awg1 1
    assert_output "2001"
    run vpd_tunnel_retired awg1
    assert_success
    TUN_DIR_INTERFACES="awg1"
    iftunnel_init
    run vpd_tunnel_retired awg1
    assert_failure
    run vpd_tunnel_retired wgc1
    assert_failure
}

@test "vpd_tunnel_unlisted_reason: why the list rejected a name, else the platform's answer" {
    load_iftunnel "br0"
    iftunnel_init
    run vpd_tunnel_unlisted_reason br0
    assert_output "'br0' is a LAN interface"
    run vpd_tunnel_unlisted_reason wgc9
    assert_failure
    refute_output
}
```

In `router/test/integration/vpn_director.bats`, after the test `vpn-director: platform lists every tunnel except main`, add:

```bash
@test "vpn-director: platform lists the interfaces of tunnel_director.interfaces after the firmware's tunnels" {
    local cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    jq '.tunnel_director.interfaces = ["awg1", "awg2"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$cfg"
    export VPD_CONFIG_FILE="$cfg"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    [ "${#lines[@]}" -eq 1 ]
    echo "$output" | jq -e '[.tunnels[].id] == ["wgc1","wgc2","ovpnc1","ovpnc2","awg1","awg2"]' >/dev/null
    echo "$output" | jq -e '.tunnels[4] == {id:"awg1", iface:"awg1", type:"interface", connected:true, description:""}' >/dev/null
    echo "$output" | jq -e '.tunnels[5] == {id:"awg2", iface:"awg2", type:"interface", connected:false, description:""}' >/dev/null
}
```

In `router/test/unit/configure.bats`, after the test `wizard_tunnel_by_number: numbers exactly the lines wizard_tunnel_choices does`, add:

```bash
@test "wizard_tunnel_choices: offers the interfaces of tunnel_director.interfaces after the platform's tunnels" {
    load_wizard
    jq '.tunnel_director.interfaces = ["awg1"]' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"
    run wizard_tunnel_choices
    assert_success
    assert_line --index 4 "  5) awg1 (down)"
    run wizard_tunnel_by_number 5
    assert_output "awg1"
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `bats router/test/unit/iftunnel.bats router/test/unit/configure.bats router/test/integration/vpn_director.bats`
Expected: the `vpd_*` tests fail with `vpd_tunnels: command not found`; the platform test fails on the id list (`awg1` and `awg2` missing); the wizard test fails on `--index 4` (only four lines).

- [ ] **Step 3: Append the layer to `lib/iftunnel.sh`**

```bash

###################################################################################################
# vpd_* - the tunnels of Tunnel Director: the firmware's and the interfaces
# -------------------------------------------------------------------------------------------------
# The core calls these, never platform_tunnel* directly: an interface of tunnel_director.interfaces
# goes to iftunnel_*, any other id to the platform contract (lib/platform.sh), with the same
# arguments and the same answers. The contract keeps describing the firmware alone.
###################################################################################################

# vpd_tunnels - the platform's tunnels without main, the interfaces, main last. rc 1 when the
# platform's inventory failed, as platform_tunnels' own: cmd_platform refuses an incomplete list.
vpd_tunnels() {
    local list rc=0 id
    list="$(platform_tunnels)" || rc=$?
    while IFS= read -r id; do
        [[ -n $id && $id != main ]] || continue
        printf '%s\n' "$id"
    done <<< "$list"
    iftunnel_list
    printf '%s\n' main
    return "$rc"
}

vpd_tunnel_iface() {
    if iftunnel_is "${1:-}"; then
        iftunnel_iface "$1"
    else
        platform_tunnel_iface "$@"
    fi
}

vpd_tunnel_info() {
    if iftunnel_is "${1:-}"; then
        iftunnel_info "$1"
    else
        platform_tunnel_info "$@"
    fi
}

# vpd_tunnel_table <id> <idx> - an interface's own table. For an interface the list no longer
# names, the table the last apply recorded for it: its slot still holds a rule into that table
# until an apply drops it, and the drop has to find the rule.
vpd_tunnel_table() {
    local recorded
    if iftunnel_is "${1:-}"; then
        iftunnel_table "${2:-}"
    elif recorded="$(iftunnel_recorded "${1:-}")"; then
        printf '%s\n' "$recorded"
    else
        platform_tunnel_table "$@"
    fi
}

vpd_tunnel_route_ensure() {
    if iftunnel_is "${1:-}"; then
        iftunnel_route_ensure "$1" "${2:-}"
    else
        platform_tunnel_route_ensure "$@"
    fi
}

# vpd_tunnel_table_release <id> <idx> - an interface the list no longer names is released as an
# interface too: its table, its NAT and MSS rules.
vpd_tunnel_table_release() {
    if iftunnel_is "${1:-}" || iftunnel_recorded "${1:-}" >/dev/null; then
        iftunnel_table_release "$1" "${2:-}"
    else
        platform_tunnel_table_release "$@"
    fi
}

# vpd_tunnel_unlisted_reason <id> - why the list rejected a name, else the platform's reason
vpd_tunnel_unlisted_reason() {
    iftunnel_rejected_reason "${1:-}" || platform_tunnel_unlisted_reason "$@"
}

# vpd_tunnel_retired <id> - an interface the last apply routed and the list no longer names
vpd_tunnel_retired() {
    ! iftunnel_is "${1:-}" && iftunnel_recorded "${1:-}" >/dev/null
}
```

- [ ] **Step 4: `cmd_platform` through the layer**

In `router/opt/vpn-director/vpn-director.sh`, in the comment block above `cmd_platform`, change `# Tunnels are listed without "main"; a tunnel whose interface or info lookup` to `# Tunnels are listed without "main" - the firmware's, then the interfaces of tunnel_director.interfaces; a tunnel whose interface or info lookup`. In `cmd_platform`, right after `_load_common`, add:

```bash

    # The interfaces of tunnel_director.interfaces are tunnels as well. This
    # command takes no lock and loads no config.sh - the daemons run it on every
    # poll - so it reads the one key it needs.
    TUN_DIR_INTERFACES="$(iftunnel_read_config "${VPD_CONFIG_FILE:-/opt/vpn-director/vpn-director.json}")"
    iftunnel_init
```

and in the same function replace `platform_tunnels` with `vpd_tunnels`, `platform_tunnel_iface` with `vpd_tunnel_iface`, and `platform_tunnel_info` with `vpd_tunnel_info` (one call each).

- [ ] **Step 5: The wizard through the layer**

In `router/opt/vpn-director/configure.sh`, after `. "$VPD_DIR/lib/platform.sh"`, add:

```bash

# Interface tunnels and the vpd_* layer: the interfaces of
# tunnel_director.interfaces are offered beside the firmware's tunnels. Like
# platform.sh it needs no common.sh, and it logs nothing without it.
. "$VPD_DIR/lib/iftunnel.sh"
```

Replace `wizard_tunnel_choices` and `wizard_tunnel_by_number` (comments included) with:

```bash
# wizard_interfaces - tunnel_director.interfaces of the config on disk: an
# interface tunnel is offered like a firmware one.
wizard_interfaces() {
    iftunnel_read_config "$VPD_DIR/vpn-director.json"
}

# wizard_tunnel_choices - the tunnels this router has, one numbered line each
# ("  N) <id>  <description> (down)"); main is not offered.
wizard_tunnel_choices() {
    local id info desc connected n=0
    local TUN_DIR_INTERFACES
    TUN_DIR_INTERFACES="$(wizard_interfaces)"
    iftunnel_init
    while IFS= read -r id; do
        [[ -n $id && $id != main ]] || continue
        n=$((n + 1))
        desc=""
        connected=1
        if info="$(vpd_tunnel_info "$id")"; then
            connected="$(printf '%s\n' "$info" | sed -n 2p)"
            desc="$(printf '%s\n' "$info" | sed -n 3p)"
        fi
        printf '  %d) %s%s%s\n' "$n" "$id" "${desc:+  $desc}" "$([[ $connected == 1 ]] || printf ' (down)')"
    done < <(vpd_tunnels || true)
}

# wizard_tunnel_by_number <n> - the id at position n of wizard_tunnel_choices,
# nothing for anything else. The awk filter is the one wizard_tunnel_choices
# numbers by, so a number read off the menu cannot name a different tunnel.
# Never fails, whatever the user typed: the caller assigns its output under
# "set -e", so a non-zero return would abort the wizard instead of asking
# again - hence [1-9] rather than [0-9], since sed rejects the line address 0
# with an error, and the trailing "|| true" for a list that fails.
wizard_tunnel_by_number() {
    [[ ${1:-} =~ ^[1-9][0-9]*$ ]] || return 0
    local TUN_DIR_INTERFACES
    TUN_DIR_INTERFACES="$(wizard_interfaces)"
    iftunnel_init
    vpd_tunnels 2>/dev/null | awk 'length && $0 != "main"' | sed -n "${1}p" || true
}
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `bats router/test/unit/iftunnel.bats router/test/unit/configure.bats router/test/integration/vpn_director.bats`
Expected: all pass, the existing platform and wizard tests included (with no list the output is what it was).

- [ ] **Step 7: Lint, the whole suite (guard), commit**

Run: `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh` - no output.
Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/lib/iftunnel.sh router/opt/vpn-director/vpn-director.sh \
    router/opt/vpn-director/configure.sh router/test/unit/iftunnel.bats \
    router/test/integration/vpn_director.bats router/test/unit/configure.bats
git commit -m "feat(tunnel): the vpd_* layer; platform and the wizard list interface tunnels" -m "The core reached the firmware's tunnels through the platform contract alone.
The vpd_* functions put the interfaces of tunnel_director.interfaces beside
them without touching the contract: an interface goes to iftunnel_*, any other
id to platform_*. vpn-director.sh platform lists them with type interface, so
the Web UI, the bot and watchd offer them like any tunnel."
```

---

### Task 4: Tunnel Director routes interface tunnels

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (header, `_tunnel_init`, `_tunnel_ensure_routes`, `_tunnel_rule_ensure`, `_tunnel_failover_rule_present`, `_tunnel_collect_applied`, `_tunnel_slot_release`, `tunnel_status`, `tunnel_stop`, `tunnel_apply`)
- Test: `router/test/unit/tunnel.bats` (new section at the end)

**Interfaces:**
- Consumes: Task 3's `vpd_*`; `iftunnel_init`, `iftunnel_state_write`, `iftunnel_status`, `IFTUNNEL_STATE_FILE`.
- Produces: `tunnel_apply` writes `IFTUNNEL_STATE_FILE` on both branches; `tunnel_stop` removes it; `tunnel_status` prints a section headed `--- Interface tunnels ---`.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/tunnel.bats`:

```bash
# ============================================================================
# Interface tunnels (tunnel_director.interfaces)
# ============================================================================

# load_awg1 <tunnels JSON> - Tunnel Director with tunnel_director.interfaces =
# awg1, UP with MTU 1280, on the stateful iptables mock.
load_awg1() {
    load_tunnel_module_with "$1"
    export TUN_DIR_INTERFACES=awg1
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    use_stateful_iptables
}

@test "tunnel_apply: routes an interface tunnel through table 2000+idx, with its NAT and MSS" {
    load_awg1 '{"wgc1":{"clients":["192.168.1.5"]},"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    run ip rule show
    assert_line $'16385:\tfrom all fwmark 0x20000/0xff0000 lookup 2001'
    run ip route show table 2001
    assert_output "default dev awg1"
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.30 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x20000/0xff0000'
    run cat "$IFTUNNEL_STATE_FILE"
    assert_output "awg1 2001"
    [ -f "$TUN_DIR_HASH" ]
}

@test "tunnel_apply: the hash covers tunnel_director.interfaces, and an empty list leaves it as it was" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    assert_equal "$(cat "$TUN_DIR_HASH")" "$(printf '%s' "$TUN_DIR_TUNNELS_JSON" | compute_hash)"

    export TUN_DIR_INTERFACES=awg1
    run tunnel_apply
    assert_success
    assert_output --partial "Configuration changed; rebuilding in place"
}

@test "tunnel_apply: the up-to-date path puts back the route an interface's re-creation took" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    ip route flush table 2000
    run tunnel_apply
    assert_success
    assert_output --partial "Rules are applied and up-to-date"
    run ip route show table 2000
    assert_output "default dev awg1"
}

# Review Focus 3: Merlin's firewall start empties every mangle chain - the MSS
# clamps with the marks - and firewall-start applies again.
@test "tunnel_apply: after Merlin emptied mangle, the rebuild puts the MSS clamps back" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    iptables -t mangle -F
    run tunnel_apply
    assert_success
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu'
    assert_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240'
}

# Review Focus 3: a rebuild of nat by the firmware (a WAN coming up, a port
# forward changed) takes MASQUERADE; nat-start applies, and the up-to-date path
# puts it back.
@test "tunnel_apply: after the firmware rebuilt nat, the up-to-date path puts MASQUERADE back" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    iptables -t nat -F
    run tunnel_apply
    assert_success
    assert_output --partial "Rules are applied and up-to-date"
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg1 -j MASQUERADE'
}

@test "tunnel_apply: an interface tunnel that is down gets no route, keeps its NAT and reports its clients" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    ip_links "awg1 <POINTOPOINT,NOARP> 1280"
    tunnel_apply
    assert_equal "${TUNNEL_UNCARRIED[*]}" "192.168.1.30"
    run ip route show table 2000
    refute_output
    run iptables -t nat -S POSTROUTING
    assert_line '-A POSTROUTING -o awg1 -j MASQUERADE'
}

@test "tunnel_stop: releases an interface tunnel - route, NAT, MSS - and its record" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    run tunnel_stop
    assert_success
    run ip route show table 2000
    refute_output
    run iptables -t nat -S POSTROUTING
    refute_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    [ ! -e "$IFTUNNEL_STATE_FILE" ]
}

@test "tunnel_status: lists the interface tunnels" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    run tunnel_status
    assert_success
    assert_output --partial "Interface tunnels"
    assert_output --partial "awg1: table 2000, route default dev awg1, NAT yes, MSS out yes, MSS in yes"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/tunnel.bats`
Expected: the new tests fail - `awg1` is "not a tunnel this platform knows" and gets no slot (no rule `16385 ... lookup 2001`), the second apply of the hash test reports "up-to-date", `tunnel_status` has no interface section. Every other test passes.

- [ ] **Step 3: Route through the layer**

In `router/opt/vpn-director/lib/tunnel.sh`:

1. In the header, replace the three lines

```bash
#     and, through it, the platform contract (platform_tunnels, platform_tunnel_table,
#     platform_tunnel_route_ensure, platform_tunnel_table_release,
#     platform_tunnel_unlisted_reason, platform_prerouting_base_pos, platform_lan_ifaces)
```

with

```bash
#     and, through it, the tunnels of lib/iftunnel.sh's vpd_* layer (vpd_tunnels,
#     vpd_tunnel_table, vpd_tunnel_route_ensure, vpd_tunnel_table_release,
#     vpd_tunnel_unlisted_reason; iftunnel_init, iftunnel_state_write, iftunnel_status)
#     and the platform contract (platform_prerouting_base_pos, platform_lan_ifaces,
#     platform_tunnel_offload_target)
```

2. In `_tunnel_init`, replace

```bash
    # Valid tunnel ids come from the platform (Merlin: wgcN tables from
    # rt_tables, and ovpncN ones while their OpenVPN client is in VPN Director
    # mode; Keenetic: OpenVPN*/Wireguard* interfaces), "main" last.
    local tables_list
    tables_list="$(platform_tunnels || true)"
```

with

```bash
    # Valid tunnel ids come through the vpd_* layer (lib/iftunnel.sh): the
    # platform's (Merlin: wgcN tables from rt_tables, and ovpncN ones while
    # their OpenVPN client is in VPN Director mode; Keenetic: OpenVPN*/Wireguard*
    # interfaces), then the interfaces of tunnel_director.interfaces, "main"
    # last. The list is checked here, in this shell, so its WARNs come once and
    # every $(vpd_*) below finds it checked.
    iftunnel_init
    local tables_list
    tables_list="$(vpd_tunnels || true)"
```

3. Replace every remaining call of the contract's tunnel functions with the layer's, one for one:
   - `_tunnel_ensure_routes`: `platform_tunnel_route_ensure` → `vpd_tunnel_route_ensure`, `platform_tunnel_unlisted_reason` → `vpd_tunnel_unlisted_reason`.
   - `_tunnel_rule_ensure` and `_tunnel_failover_rule_present`: `platform_tunnel_table` → `vpd_tunnel_table`.
   - `_tunnel_collect_applied`: `platform_tunnel_unlisted_reason` → `vpd_tunnel_unlisted_reason`.
   - `_tunnel_slot_release`: `platform_tunnel_table` → `vpd_tunnel_table`, `platform_tunnel_table_release` → `vpd_tunnel_table_release`.
   - `tunnel_stop`: `platform_tunnel_table_release` → `vpd_tunnel_table_release`.
   - `tunnel_apply`, the routing step: both `platform_tunnel_route_ensure` → `vpd_tunnel_route_ensure`, `platform_tunnel_table_release` → `vpd_tunnel_table_release`, `platform_tunnel_table` → `vpd_tunnel_table`.

   Afterwards `grep -n 'platform_tunnel' router/opt/vpn-director/lib/tunnel.sh` prints only `platform_tunnel_offload_target` (the call in `tunnel_apply` and comments).

4. In `tunnel_apply`, replace the hash block

```bash
    # Compute config hash for change detection. Failover snapshot clients are
    # extra MARK rules, not a key reorder, so they belong in the hash. With no
    # failover, hash only the tunnels JSON so an upgrade does not rebuild TUN_DIR.
    local new_hash old_hash empty_hash
    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]]; then
        new_hash=$(printf '%s\n%s\n%s' "$TUN_DIR_TUNNELS_JSON" "$XRAY_FAILOVER_TUNNEL" "${XRAY_FAILOVER_CLIENTS:-}" | compute_hash)
    else
        new_hash=$(printf '%s' "$TUN_DIR_TUNNELS_JSON" | compute_hash)
    fi
```

with

```bash
    # Compute config hash for change detection. Failover snapshot clients are
    # extra MARK rules, not a key reorder, so they belong in the hash, and so do
    # the interfaces of tunnel_director.interfaces - as written: the checked list
    # changes with the platform's answers. With neither, hash only the tunnels
    # JSON so an upgrade does not rebuild TUN_DIR.
    local new_hash old_hash empty_hash hash_input="$TUN_DIR_TUNNELS_JSON"
    if [[ -n ${TUN_DIR_INTERFACES:-} ]]; then
        hash_input+=$'\n'"interfaces: $TUN_DIR_INTERFACES"
    fi
    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]]; then
        new_hash=$(printf '%s\n%s\n%s' "$hash_input" "$XRAY_FAILOVER_TUNNEL" "${XRAY_FAILOVER_CLIENTS:-}" | compute_hash)
    else
        new_hash=$(printf '%s' "$hash_input" | compute_hash)
    fi
```

5. In `tunnel_apply`, on the up-to-date branch, replace

```bash
        log "Rules are applied and up-to-date"
        return 0
```

with

```bash
        iftunnel_state_write "$TUN_DIR_TABLES" ||
            log -l WARN "Tunnel Director: cannot write $IFTUNNEL_STATE_FILE; ensure-routes does not see the interface tunnels"
        log "Rules are applied and up-to-date"
        return 0
```

6. In `tunnel_apply`, right after the step 3 block (the `if [[ $jumps_ok -eq 1 ]]; then ... fi` that releases dropped slots), add:

```bash

    # The record of the interface tunnels, after the releases above: a release
    # reads the last apply's record to know an interface the list no longer names.
    iftunnel_state_write "$TUN_DIR_TABLES" ||
        log -l WARN "Tunnel Director: cannot write $IFTUNNEL_STATE_FILE; ensure-routes does not see the interface tunnels"
```

7. In `tunnel_stop`, change `rm -f "$TUN_DIR_HASH" "$TUN_DIR_FAILOVER_READY"` to `rm -f "$TUN_DIR_HASH" "$TUN_DIR_FAILOVER_READY" "$IFTUNNEL_STATE_FILE"`.

8. In `tunnel_status`, right before its final `return 0`, add:

```bash
    printf '%s\n' "--- Interface tunnels ---"
    iftunnel_status
    printf '\n'

```

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/tunnel.bats`
Expected: all pass, the new ones included. The existing tests that redefine `platform_tunnel_*` still steer the apply: the layer calls the platform functions by name at call time.

- [ ] **Step 5: The whole suite (guard) and commit**

Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats
git commit -m "feat(tunnel): Tunnel Director routes interface tunnels" -m "tunnel.sh reaches every tunnel through the vpd_* layer, so a listed interface
gets a slot, table 2000 + idx, its route and its NAT and MSS rules on every
apply - the firmware's rebuilds of mangle and nat included. The hash covers the
list as written, unchanged while it is empty, and each apply records the
interface tunnels for ensure-routes."
```

---

### Task 5: A tunnel with no route refuses its clients' traffic

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (new helpers before `_tunnel_slot_release`; `_tunnel_rule_listed`; `_tunnel_ensure_routes`; `_tunnel_slot_release`; `tunnel_stop`; `tunnel_apply`), `router/test/mocks/ip` (rule actions)
- Test: `router/test/unit/tunnel.bats` (new section; updates to three existing tests)

**Interfaces:**
- Consumes: Task 4.
- Produces: `_tunnel_block_pref <idx>`, `_tunnel_block_listed <pref> <mark>`, `_tunnel_block_ensure <idx> <tunnel>` (rc 1 when the rule did not go in), `_tunnel_route_warn <tunnel>`, `_tunnel_ip_rule_listed <pref> <mark> <action words>`.

- [ ] **Step 1: Teach the `ip` mock the rule actions**

In `router/test/mocks/ip`:
- after `_table=""` at the top add `_action=""`;
- in `_parse_rule_args`, set `_action=""` with the others, and add the case line `prohibit|blackhole|unreachable) _action="$1"; shift ;;` before `*) shift ;;`;
- replace `_rule_line` with:

```bash
# _rule_line renders the parsed selectors as "ip rule show" would print them: an
# action such as prohibit in place of the lookup.
_rule_line() {
    if [[ -n $_action ]]; then
        printf '%s:\tfrom all%s %s' "$_pref" "${_fwmark:+ fwmark $_fwmark}" "$_action"
    else
        printf '%s:\tfrom all%s lookup %s' "$_pref" "${_fwmark:+ fwmark $_fwmark}" "$(_table_name "$_table")"
    fi
}
```

- in the `del)` branch change `if [[ -n $_fwmark || -n $_table ]]; then` to `if [[ -n $_fwmark || -n $_table || -n $_action ]]; then`.

- [ ] **Step 2: Write the failing tests and update the three tests whose rule changed**

Append to `router/test/unit/tunnel.bats`:

```bash
# ============================================================================
# The kill switch: a mark whose table has no route is refused, never sent to main
# ============================================================================

@test "tunnel_apply: every slot but main's gets a block rule right after the lookup range" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"main":{"clients":["192.168.1.6"]},"ovpnc2":{"clients":["192.168.1.7"]}}'
    run tunnel_apply
    assert_success
    run ip rule show
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
    assert_line $'16641:\tfrom all fwmark 0x30000/0xff0000 prohibit'
    refute_line --partial '16640:'
}

@test "tunnel_apply: a new slot's block rule goes in before its lookup rule" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    run tunnel_apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_line --index 0 'ip rule del pref 16639'
    assert_line --index 1 'ip rule add pref 16639 fwmark 0x10000/0xff0000 prohibit'
    assert_line --index 2 'ip rule del pref 16384'
    assert_line --index 3 'ip rule add pref 16384 fwmark 0x10000/0xff0000 lookup wgc1'
}

# The upgrade: a router an earlier version applied has its hash, its chain and
# its lookup rules, and no block rule. Its first apply takes the up-to-date path.
@test "tunnel_apply: the up-to-date path puts a missing block rule back, without a rebuild" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    ip rule del pref 16639 fwmark 0x10000/0xff0000 prohibit
    fw_chain_exists() { return 0; }
    marks_in_place
    run tunnel_apply
    assert_success
    assert_output --partial "Rules are applied and up-to-date"
    run ip rule show
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
}

@test "tunnel_apply: a slot the layout dropped loses its block rule after the swap" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    TUN_DIR_TUNNELS_JSON='{"ovpnc2":{"clients":["192.168.1.6"]}}'
    run tunnel_apply
    assert_success
    run ip rule show
    refute_line --partial '16639:'
    assert_line $'16640:\tfrom all fwmark 0x20000/0xff0000 prohibit'
}

@test "tunnel_stop: clears the block range with the lookup range" {
    load_tunnel_module
    : > /tmp/bats_ip_calls.log
    run tunnel_stop
    assert_success
    grep -qx 'ip rule del pref 16384' /tmp/bats_ip_calls.log
    grep -qx 'ip rule del pref 16639' /tmp/bats_ip_calls.log
    grep -qx 'ip rule del pref 16893' /tmp/bats_ip_calls.log
    refute grep -qx 'ip rule del pref 16894' /tmp/bats_ip_calls.log
}

@test "tunnel_apply: refuses a layout whose block rules would reach main, before touching a rule" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    export TUN_DIR_PREF_BASE=32300
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_failure
    assert_output --partial "the block rules would reach main's rule (32766)"
    refute grep -q 'ip rule' /tmp/bats_ip_calls.log
}

@test "tunnel_apply: a route that does not go in says its clients are refused" {
    load_tunnel_module
    platform_tunnel_route_ensure() { return 1; }
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'wgc1': route not installed (interface down or not mapped?); its clients are refused until it is"
}

# iproute2 4.4 ends each rule with a space, as it does a lookup rule.
@test "_tunnel_block_ensure: recognises its rule as iproute2 4.4 prints it" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    _tunnel_init
    ip() {
        if [[ $1 == rule && $2 == show ]]; then
            printf '0:\tfrom all lookup local \n16639:\tfrom all fwmark 0x10000/0xff0000 prohibit \n'
            return 0
        fi
        echo "ip $*" >> "$BATS_TEST_TMPDIR/ip_writes.log"
    }
    run _tunnel_block_ensure 0 wgc1
    assert_success
    [ ! -e "$BATS_TEST_TMPDIR/ip_writes.log" ]
}
```

Update three existing tests in `router/test/unit/tunnel.bats`:

1. `tunnel_apply: a new slot's ip rule goes in before the swap, a released one's goes after it` - its four `assert_line --index` lines become:

```bash
    assert_line --index 0 'ip rule del pref 16640'
    assert_line --index 1 'ip rule add pref 16640 fwmark 0x20000/0xff0000 prohibit'
    assert_line --index 2 'ip rule del pref 16385'
    assert_line --index 3 'ip rule add pref 16385 fwmark 0x20000/0xff0000 lookup ovpnc2'
    assert_line --index 4 'iptables -t mangle -E TUN_DIR_NEW TUN_DIR'
    assert_line --index 5 'ip rule del pref 16384 fwmark 0x10000/0xff0000 lookup wgc1'
    assert_line --index 6 'ip rule del pref 16639 fwmark 0x10000/0xff0000 prohibit'
```

2. `tunnel_apply: the up-to-date path installs no default for a client switched out of VPN Director mode` - in its `grep -qF "Tunnel 'ovpnc1': route not installed: ..."` line, replace `; Tunnel Director does not route through it` with `; its clients are refused`.

3. `tunnel_apply: the up-to-date path leaves the firmware's default of a client in Yes (all) mode` - the same replacement in its `assert_output --partial` line.

- [ ] **Step 3: Run the tests to see them fail**

Run: `bats router/test/unit/tunnel.bats`
Expected: the new tests and the three updated ones fail - no `prohibit` rule exists, the WARN still names main, the refusal test applies. Every other test passes.

- [ ] **Step 4: Implement the kill switch**

In `router/opt/vpn-director/lib/tunnel.sh`:

1. Replace the body of `_tunnel_rule_listed` - keep its comment - so that it reads:

```bash
_tunnel_rule_listed() {
    _tunnel_ip_rule_listed "$1" "$2" "lookup $(rt_table_label "$3")"
}

# _tunnel_block_listed <pref> <mark> - is this module's block rule at that preference: "from all
# fwmark <mark>/<mask> prohibit"? Read as _tunnel_rule_listed reads a lookup rule.
_tunnel_block_listed() {
    _tunnel_ip_rule_listed "$1" "$2" "prohibit"
}

# _tunnel_ip_rule_listed <pref> <mark> <action> - is "from all fwmark <mark>/<mask> <action>" at
# that preference, every part a whole word (iproute2 4.4 ends each rule with a space), a mask of
# all ones left out as iproute2 leaves it out? The listing is read whole first: piped into
# "grep -q" it was wrong under pipefail (shell-conventions.md).
_tunnel_ip_rule_listed() {
    local pref="$1" fwmark="fwmark $2/$_tunnel_mark_mask_hex" action="$3" rules line
    [[ $_tunnel_mark_mask_hex != 0xffffffff ]] || fwmark="fwmark $2"
    rules=$(ip rule show 2>/dev/null) || true
    while IFS= read -r line; do
        [[ $line == "$pref:"* ]] || continue
        line=" ${line#*:} "
        line=${line//$'\t'/ }
        if [[ $line == *" from all "* && $line == *" $fwmark "* && $line == *" $action "* ]]; then
            return 0
        fi
    done <<< "$rules"
    return 1
}
```

2. Right before `_tunnel_slot_release`, add:

```bash
# _tunnel_block_pref <idx> - the preference of the slot's block rule: right after the lookup
# range, which ends at pref_base + field_max - 1. 16639-16893 with the defaults.
_tunnel_block_pref() {
    printf '%d' $((TUN_DIR_PREF_BASE + _tunnel_mark_field_max + $1))
}

# _tunnel_block_ensure <idx> <tunnel> - the kill switch of a slot: "fwmark <mark>/<mask>
# prohibit" after its lookup rule. When the tunnel's table holds no route - the interface is
# missing or down, the route never went in, the firmware flushed the table - the lookup falls
# through to this rule, never to main, and the client gets ICMP "administratively prohibited"
# instead of its traffic leaving through the WAN. main's slot has none: main is no tunnel. A rule
# that is in place is left alone; another rule on the preference makes way - the range is this
# module's. Returns 1 when the rule did not go in.
_tunnel_block_ensure() {
    local idx="$1" tunnel="$2" pref mark_hex
    [[ $tunnel != main ]] || return 0
    pref=$(_tunnel_block_pref "$idx")
    mark_hex=$(_tunnel_mark_hex "$idx")
    _tunnel_block_listed "$pref" "$mark_hex" && return 0
    ip rule del pref "$pref" 2>/dev/null || true
    if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" prohibit 2>/dev/null; then
        log -l ERROR "Tunnel '$tunnel': the block rule did not go in (pref=$pref fwmark=$mark_hex); while its table has no route its clients fall through to main"
        return 1
    fi
    return 0
}

# _tunnel_route_warn <tunnel> - a tunnel's route did not go in: say so, and why when the layer
# knows. Its marks meet the slot's block rule, never main.
_tunnel_route_warn() {
    local reason
    reason="$(vpd_tunnel_unlisted_reason "$1")" || reason=""
    if [[ -n $reason ]]; then
        log -l WARN "Tunnel '$1': route not installed: $reason; its clients are refused"
    else
        log -l WARN "Tunnel '$1': route not installed (interface down or not mapped?); its clients are refused until it is"
    fi
}
```

3. In `_tunnel_ensure_routes`, change `local idx tunnel rc=0 carried reason` to `local idx tunnel rc=0 carried`, and replace

```bash
        carried=1
        if ! vpd_tunnel_route_ensure "$tunnel" "$idx" "$(_tunnel_gateway "$tunnel")"; then
            reason="$(vpd_tunnel_unlisted_reason "$tunnel")" || reason=""
            if [[ -n $reason ]]; then
                log -l WARN "Tunnel '$tunnel': route not installed: $reason; Tunnel Director does not route through it"
            else
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
            fi
            carried=0
```

with

```bash
        carried=1
        # The block rule first: a router an earlier version applied has none,
        # and this path is the one its first apply takes.
        _tunnel_block_ensure "$idx" "$tunnel" || true
        if ! vpd_tunnel_route_ensure "$tunnel" "$idx" "$(_tunnel_gateway "$tunnel")"; then
            _tunnel_route_warn "$tunnel"
            carried=0
```

and in its header comment replace `A tunnel whose route or ip rule is not in place routes its marks to main: its clients are not carried (TUNNEL_UNCARRIED).` with `A tunnel whose route or ip rule is not in place refuses its marks (the block rule): its clients are not carried (TUNNEL_UNCARRIED).`

4. In `_tunnel_slot_release`, change `local idx="$1" tunnel="$2" pref mark_hex table` to `local idx="$1" tunnel="$2" pref bpref mark_hex table`, add `bpref=$(_tunnel_block_pref "$idx")` after `pref=$((TUN_DIR_PREF_BASE + idx))`, and right after `vpd_tunnel_table_release "$tunnel" "$idx" || true` add:

```bash
    if [[ $tunnel != main ]] && _tunnel_block_listed "$bpref" "$mark_hex"; then
        if ! ip rule del pref "$bpref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" prohibit 2>/dev/null; then
            log -l WARN "Tunnel '$tunnel': the block rule at pref $bpref did not go; it refuses a mark nothing sets any more"
        fi
    fi
```

and in its comment change `drop the ip rule and the table of a slot` to `drop the ip rules and the table of a slot`.

5. In `tunnel_stop`, replace

```bash
    # Remove ip rules in our pref range
    local pref_base="${TUN_DIR_PREF_BASE:-16384}"
    local max_rules="${_tunnel_mark_field_max:-255}"
    local i

    for ((i = 0; i < max_rules; i++)); do
        ip rule del pref $((pref_base + i)) 2>/dev/null || true
    done
```

with

```bash
    # Remove the ip rules in our pref range: the lookup rules, then the block
    # rules right after them (_tunnel_block_pref).
    local pref_base="${TUN_DIR_PREF_BASE:-16384}"
    local max_rules="${_tunnel_mark_field_max:-255}"
    local i

    for ((i = 0; i < 2 * max_rules; i++)); do
        ip rule del pref $((pref_base + i)) 2>/dev/null || true
    done
```

6. In `tunnel_apply`, right after the `# Validate JSON before modifying firewall state` block, add:

```bash

    # The block rules take the preferences right after the lookup range. They
    # have to stay ahead of main's rule at 32766, or a mark with no route in its
    # table would reach main after all.
    if (( TUN_DIR_PREF_BASE + 2 * _tunnel_mark_field_max > 32766 )); then
        log -l ERROR "Tunnel Director: the block rules would reach main's rule (32766) with pref_base $TUN_DIR_PREF_BASE and $_tunnel_mark_field_max slots; rules not applied"
        return 1
    fi
```

7. In `tunnel_apply`, the routing step: replace the line `local tunnel_idx tunnel table pref mark_hex route_ok rule_ok` and the whole `while read -r tunnel_idx tunnel; do ... done < "$slots_tmp"` loop right after it (step 1, "Routing, before any packet carries a new mark") with:

```bash
    local tunnel_idx tunnel table pref mark_hex route_ok rule_ok
    while read -r tunnel_idx tunnel; do
        [[ -n $tunnel ]] || continue
        route_ok=1
        rule_ok=1
        # The block rule first: from here on a mark whose table holds no route
        # is refused, never sent to main (the kill switch, tunnel-director.md).
        # One that did not go in logged its ERROR; the up-to-date path retries it.
        _tunnel_block_ensure "$tunnel_idx" "$tunnel" || warnings=1
        if grep -qxF "$tunnel_idx $tunnel" "$prev_tmp"; then
            # A kept slot is carrying its clients now: its route and its rule
            # are ensured, never released - an empty table is a window in which
            # they are refused.
            if ! vpd_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                _tunnel_route_warn "$tunnel"
                warnings=1
                route_ok=0
            fi
            if ! _tunnel_rule_ensure "$tunnel_idx" "$tunnel"; then
                warnings=1
                rule_ok=0
            fi
        else
            # A new slot starts from an empty table. An apply that died after
            # ensuring a route but before recording its slot left the route
            # with no record, and a route_ensure that fails now (interface
            # down) would leave the previous owner's route behind the ip rule
            # installed below. No-op on Merlin.
            vpd_tunnel_table_release "$tunnel" "$tunnel_idx" || true
            if ! vpd_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                _tunnel_route_warn "$tunnel"
                warnings=1
                route_ok=0
            fi
            # The ip rule goes in even when the route does not: a lookup in an
            # empty table falls through to the block rule.
            table="$(vpd_tunnel_table "$tunnel" "$tunnel_idx")"
            pref=$((TUN_DIR_PREF_BASE + tunnel_idx))
            mark_hex=$(_tunnel_mark_hex "$tunnel_idx")
            ip rule del pref "$pref" 2>/dev/null || true
            if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
                log -l ERROR "Failed to add ip rule: pref=$pref fwmark=$mark_hex lookup=$table"
                warnings=1
                rule_ok=0
            fi
        fi
        if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" ]]; then
            fo_applied=1
            [[ $route_ok -eq 1 ]] || fo_route_ok=0
            [[ $rule_ok -eq 1 ]] || fo_rule_ok=0
        fi
        # Marked or not, a client of this slot is refused, not carried.
        if [[ $route_ok -eq 0 || $rule_ok -eq 0 ]]; then
            _tunnel_not_carried_on "$tunnel" "$(_tunnel_clients_of "$tunnel")"
        fi
    done < "$slots_tmp"
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `bats router/test/unit/tunnel.bats`
Expected: all pass. If `tunnel_apply: after NDM deleted the chain, the rebuild touches no ip rule of a kept tunnel` fails, the block rule of the kept slot was re-added: check that the mock renders `prohibit` (Step 1) and that `_tunnel_block_ensure` finds it.

- [ ] **Step 6: The whole suite (guard) and commit**

Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/mocks/ip router/test/unit/tunnel.bats
git commit -m "feat(tunnel): a tunnel with no route refuses its clients' traffic" -m "A tunnel whose table held no route - down, its interface re-created, its
table flushed by the firmware - let the lookup fall through to main, and its
clients left through the WAN. Every slot but main's now has an ip rule
'fwmark <mark> prohibit' right after the lookup range, put in before the lookup
rule and re-ensured on the up-to-date path, so a router gets it on its first
apply after the upgrade. Excluded destinations still go direct."
```

---

### Task 6: A tunnel the platform does not know refuses its clients

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (`_tunnel_collect_applied`; new `_tunnel_routeless`, `_tunnel_unroute`; the routing loop of `tunnel_apply`)
- Test: `router/test/unit/tunnel.bats` (new section; updates to five existing tests)

**Interfaces:**
- Consumes: Task 5's `_tunnel_block_ensure`, `_tunnel_route_warn`; Task 3's `vpd_tunnel_retired`, `vpd_tunnel_table`, `vpd_tunnel_table_release`.
- Produces: `_tunnel_routeless <tunnel> <idx>` (rc 0 when an unlisted tunnel has no table to route through), `_tunnel_unroute <idx> <tunnel>`.

- [ ] **Step 1: Write the failing tests and update the five tests whose rule changed**

Append to `router/test/unit/tunnel.bats`:

```bash
# ============================================================================
# A tunnel the platform does not know: its clients are refused
# ============================================================================

@test "tunnel_apply: a tunnel no platform knows keeps a slot, its marks and the block rule only" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"foo1":{"clients":["192.168.1.6"]}}'
    use_stateful_iptables
    tunnel_apply
    grep -qF "Tunnel 'foo1' is not a tunnel this platform knows; its clients are refused" "$LOG_FILE"
    run cat "$TUN_DIR_TABLES"
    assert_output $'0 wgc1\n1 foo1'
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.6 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x20000/0xff0000'
    run ip rule show
    assert_line $'16640:\tfrom all fwmark 0x20000/0xff0000 prohibit'
    refute_line --partial '16385:'
    assert_equal "${TUNNEL_UNCARRIED[*]}" "192.168.1.6"
    [ ! -f "$TUN_DIR_HASH" ]
}

# Review Focus 5: KeeneticOS answers "main" alone while RCI is silent, and the
# netfilter.d hook applies during exactly such an NDM rebuild. Skipping every
# tunnel sent every client out through the WAN; refusing them all would cut the
# LAN off. WireguardN's table is 2000 + idx whatever RCI says, so the tunnel
# keeps its route and its lookup rule.
@test "tunnel_apply: a tunnel whose table the platform names keeps routing while the platform does not list it" {
    export VPD_PLATFORM=keenetic
    load_tunnel_module_with '{"Wireguard0":{"clients":["192.168.1.6"]}}'
    _rci_get() { return 1; }
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'Wireguard0' is not a tunnel this platform knows; its clients are refused"
    grep -qx 'ip route replace default dev nwg0 table 2000' /tmp/bats_ip_calls.log
    run ip rule show
    assert_line $'16384:\tfrom all fwmark 0x10000/0xff0000 lookup 2000'
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
}

# Review Focus 2: awg1 taken off tunnel_director.interfaces while its tunnel
# keeps clients. The platform does not know it, and its table would carry them
# on into awg1: the lookup rule and the route go, with the NAT and MSS rules,
# and the block rule refuses the clients.
@test "tunnel_apply: an interface taken off the list stops routing and its clients are refused" {
    load_awg1 '{"awg1":{"clients":["192.168.1.30"]}}'
    run tunnel_apply
    assert_success
    run ip route show table 2000
    assert_output "default dev awg1"

    TUN_DIR_INTERFACES=""
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'awg1' is not a tunnel this platform knows; its clients are refused"
    run ip rule show
    refute_line --partial '16384:'
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
    run ip route show table 2000
    refute_output
    run iptables -t nat -S POSTROUTING
    refute_line '-A POSTROUTING -o awg1 -j MASQUERADE'
    run cat "$TUN_DIR_TABLES"
    assert_output "0 awg1"
    [ ! -e "$IFTUNNEL_STATE_FILE" ]
}
```

Update five existing tests in `router/test/unit/tunnel.bats`:

1. `tunnel_apply: applies the known tunnel and still does not record the hash when another is unknown` - replace `assert_output "0 wgc1"` with:

```bash
    assert_output $'0 wgc1\n1 wgc9'
    grep -q 'ip rule add pref 16640 fwmark 0x20000/0xff0000 prohibit' /tmp/bats_ip_calls.log
```

2. `tunnel_apply: releases a recorded slot the new layout drops when no hash was recorded` - replace its first `assert_output "0 wgc1"` with `assert_output $'0 wgc1\n1 wgc9'`. Its comment's last sentence stays true.

3. `tunnel_apply: skips an OpenVPN client out of VPN Director mode and says why` - rename it `tunnel_apply: an OpenVPN client out of VPN Director mode keeps a slot that refuses its clients, and says why`, and replace its assertions (everything after `assert_success`) with:

```bash
    assert_output --partial "Tunnel 'ovpnc1' is not on the platform's list: OpenVPN client 1 is not in VPN Director mode (\"Redirect Internet traffic through tunnel\" is \"No\"); its clients are refused"
    refute_output --partial "not a tunnel this platform knows"
    assert_output --partial "not recorded as up-to-date"
    refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
    grep -qx 'ip rule add pref 16639 fwmark 0x10000/0xff0000 prohibit' /tmp/bats_ip_calls.log
    grep -qx 'ip rule add pref 16384 fwmark 0x10000/0xff0000 lookup ovpnc1' /tmp/bats_ip_calls.log
    [[ ! -f "$TUN_DIR_HASH" ]]
```

4. `tunnel_apply: a rebuild releases the slot of a client switched out of VPN Director mode` - rename it `tunnel_apply: a rebuild keeps the slot of a client switched out of VPN Director mode, and its default goes`; replace its comment with `# restart and restart tunnel rebuild with the config unchanged (TUN_DIR_FORCE_REBUILD=1). The client switched out of VPN Director mode keeps its slot - its clients are refused, not skipped - and in "No" mode the default in its table goes.`; and replace everything after the second `assert_success` with:

```bash
    assert_output --partial "Tunnel 'ovpnc1' is not on the platform's list: OpenVPN client 1 is not in VPN Director mode"
    refute grep -qx 'ip rule del pref 16384 fwmark 0x10000/0xff0000 lookup ovpnc1' /tmp/bats_ip_calls.log
    grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    run cat "$TUN_DIR_TABLES"
    assert_output "0 ovpnc1"
```

5. `tunnel_apply: no slot and no failover_ready for a failover tunnel out of VPN Director mode` - rename it `tunnel_apply: no failover_ready for a failover tunnel out of VPN Director mode`; in its comment replace `A failover tunnel the platform leaves out gets no slot, so no marker goes out` with `A failover tunnel the platform leaves out has no route, so no marker goes out`; replace `assert_output --partial "Tunnel 'ovpnc1' is skipped: OpenVPN client 1 is not in VPN Director mode"` with `assert_output --partial "Tunnel 'ovpnc1' is not on the platform's list: OpenVPN client 1 is not in VPN Director mode"` and `refute_output --partial "ovpnc1"` (after `run cat "$TUN_DIR_TABLES"`) with `assert_output "0 ovpnc1"`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `bats router/test/unit/tunnel.bats`
Expected: the three new tests and the five updated ones fail (the unknown tunnels get no slot; the old WARN "is skipped" is logged). Every other test passes - `records the hash when a tunnel out of VPN Director mode has no clients` and `records the hash when a tunnel unknown to the platform has no clients` among them: a tunnel with no clients keeps the old warning.

- [ ] **Step 3: Implement**

In `router/opt/vpn-director/lib/tunnel.sh`:

1. In the comment above `_tunnel_collect_applied`, replace `The clients of a tunnel that gets no slot - one the platform does not list, one the mark field has no room for - are not carried (TUNNEL_UNCARRIED).` with `A tunnel the platform does not list keeps a slot when it has clients: their MARK rules and the slot's block rule refuse what they would send through it. Its clients are not carried, nor are those of a tunnel the mark field has no room for (TUNNEL_UNCARRIED).`

2. In `_tunnel_collect_applied`, replace the block

```bash
        if ! _tunnel_table_allowed "$tunnel"; then
            reason="$(vpd_tunnel_unlisted_reason "$tunnel")" || reason=""
            if [[ -n $reason ]]; then
                log -l WARN "Tunnel '$tunnel' is skipped: $reason"
            else
                log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
            fi
            warnings=1
            # A tunnel with no clients gets no slot whether the platform lists it
            # or not, so there is nothing for a later apply to retry: it does not
            # keep the hash from being recorded.
            clients="$(_tunnel_clients_of "$tunnel")"
            if [[ -n $clients ]]; then
                skipped_unknown=1
                _tunnel_not_carried_on "$tunnel" "$clients"
            fi
            continue
        fi
```

with

```bash
        if ! _tunnel_table_allowed "$tunnel"; then
            reason="$(vpd_tunnel_unlisted_reason "$tunnel")" || reason=""
            warnings=1
            # A tunnel with no clients gets no slot whether the platform lists it
            # or not, so there is nothing for a later apply to retry: it does not
            # keep the hash from being recorded.
            clients="$(_tunnel_clients_of "$tunnel")"
            if [[ -z $clients ]]; then
                if [[ -n $reason ]]; then
                    log -l WARN "Tunnel '$tunnel' is skipped: $reason"
                else
                    log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
                fi
                continue
            fi
            # One with clients keeps a slot: their MARK rules and the slot's block
            # rule refuse what they would send through it. Skipped, they went
            # unmarked - out through the WAN, every client of every tunnel at once
            # while KeeneticOS's RCI was silent. The hash stays unrecorded, so the
            # next apply looks again.
            if [[ -n $reason ]]; then
                log -l WARN "Tunnel '$tunnel' is not on the platform's list: $reason; its clients are refused"
            else
                log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; its clients are refused"
            fi
            skipped_unknown=1
            _tunnel_not_carried_on "$tunnel" "$clients"
        fi
```

3. Right after `_tunnel_route_warn` (Task 5), add:

```bash
# _tunnel_routeless <tunnel> <idx> - has a tunnel the platform does not list no table to route
# through? An interface taken off tunnel_director.interfaces has none - though the layer still
# names the table the last apply recorded, that table would carry its clients on into the
# interface - and any other id has none when the layer names no table for it. KeeneticOS's
# WireguardN and Merlin's ovpncN keep theirs: the platform decides about their routes.
_tunnel_routeless() {
    vpd_tunnel_retired "$1" && return 0
    ! vpd_tunnel_table "$1" "$2" >/dev/null
}

# _tunnel_unroute <idx> <tunnel> - what a known past left on the slot of a routeless tunnel: its
# lookup rule (ours only) and its table - for an interface the list no longer names, the route,
# the NAT and MSS rules too (vpd_tunnel_table_release).
_tunnel_unroute() {
    local idx="$1" tunnel="$2" pref mark_hex table
    pref=$((TUN_DIR_PREF_BASE + idx))
    mark_hex=$(_tunnel_mark_hex "$idx")
    if table="$(vpd_tunnel_table "$tunnel" "$idx")" && _tunnel_rule_listed "$pref" "$mark_hex" "$table"; then
        ip rule del pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null || true
    fi
    vpd_tunnel_table_release "$tunnel" "$idx" || true
}
```

4. In the routing loop of `tunnel_apply` (Task 5's version), replace

```bash
        _tunnel_block_ensure "$tunnel_idx" "$tunnel" || warnings=1
        if grep -qxF "$tunnel_idx $tunnel" "$prev_tmp"; then
```

with

```bash
        _tunnel_block_ensure "$tunnel_idx" "$tunnel" || warnings=1
        if ! _tunnel_table_allowed "$tunnel" && _tunnel_routeless "$tunnel" "$tunnel_idx"; then
            # Not listed, and no table to route it through - a typo, an
            # interface taken off tunnel_director.interfaces: the block rule
            # alone refuses its clients. What a known past left on the slot goes.
            _tunnel_unroute "$tunnel_idx" "$tunnel"
            route_ok=0
        elif grep -qxF "$tunnel_idx $tunnel" "$prev_tmp"; then
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/tunnel.bats`
Expected: all pass.

- [ ] **Step 5: The whole suite (guard) and commit**

Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats
git commit -m "feat(tunnel): a tunnel the platform does not know refuses its clients" -m "A configured tunnel the platform did not list was skipped, and its clients
went unmarked - out through the WAN. On KeeneticOS a silent RCI made that every
tunnel at once. Such a tunnel now keeps a slot with its clients' marks and the
block rule; when the layer still names its table its route and lookup rule go
in as before, so a silent RCI costs nothing, and when it names none - a typo,
an interface taken off the list - the block rule alone refuses the clients and
what the old layout routed through goes."
```

---

### Task 7: Addresses and CIDRs in a tunnel's `exclude`

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh` (`_tunnel_emit_client`)
- Test: `router/test/unit/tunnel.bats` (new section)

**Interfaces:**
- Consumes: `is_ipv4_net` (`lib/common.sh`), `ensure_fw_rule`.
- Produces: nothing new for later tasks.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/tunnel.bats`:

```bash
# ============================================================================
# Addresses and CIDRs in a tunnel's exclude
# ============================================================================

@test "tunnel_apply: an address or CIDR in exclude returns ahead of the mark, beside the countries" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.30"],"exclude":["ru","203.0.113.7","198.51.100.0/24"]}}'
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    run grep -- '-A TUN_DIR_NEW' /tmp/bats_iptables_calls.log
    assert_line --index 0 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.1.30 -m set --match-set ru dst -j RETURN'
    assert_line --index 1 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.1.30 -d 203.0.113.7 -j RETURN'
    assert_line --index 2 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.1.30 -d 198.51.100.0/24 -j RETURN'
    assert_line --index 3 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.1.30 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
    assert_equal "${#lines[@]}" 4
}

@test "tunnel_apply: an IPv6 exclusion is skipped with a WARN" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.30"],"exclude":["2001:db8::/32"]}}'
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Tunnel 'wgc1': exclusion '2001:db8::/32' is IPv6; Tunnel Director routes IPv4 only, skipping it"
    refute grep -q '2001:db8' /tmp/bats_iptables_calls.log
}

# iptables would read 203.0.113.010 as .8: what is no address as written is no
# address, and no set by that name exists either.
@test "tunnel_apply: an address with a leading zero is no address" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.30"],"exclude":["203.0.113.010"]}}'
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "Exclude ipset '203.0.113.010' not found"
    refute grep -q -- '-d 203.0.113' /tmp/bats_iptables_calls.log
}

@test "tunnel_get_required_ipsets: an address in exclude asks for no ipset" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.30"],"exclude":["ru","203.0.113.7"]}}'
    run tunnel_get_required_ipsets
    assert_success
    assert_output "ru"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/tunnel.bats`
Expected: the first two fail (`Exclude ipset '203.0.113.7' not found` is logged instead of a RETURN rule; the IPv6 entry gets the same WARN). The other two pass already (**guard**): they pin what must not change.

- [ ] **Step 3: Implement**

In `_tunnel_emit_client`, replace

```bash
    local excl excl_set
    while IFS= read -r excl; do
        [[ -n $excl ]] || continue
        excl_set=$(printf '%s' "$excl" | tr 'A-Z' 'a-z')
```

with

```bash
    local excl excl_set
    while IFS= read -r excl; do
        [[ -n $excl ]] || continue
        # An address or a CIDR returns its destination as it stands, anything
        # else names a set. is_ipv4_net is iptables' own reading - no leading
        # zeros - so "203.0.113.010" falls through to the set lookup and its WARN.
        if is_ipv4_net "$excl"; then
            if ! ensure_fw_rule -q mangle "$chain" -s "$client" -d "$excl" -j RETURN; then
                warnings=1
                incomplete=1
            fi
            continue
        fi
        if [[ $excl == *:* ]]; then
            log -l WARN "Tunnel '$tunnel': exclusion '$excl' is IPv6; Tunnel Director routes IPv4 only, skipping it"
            warnings=1
            continue
        fi
        excl_set=$(printf '%s' "$excl" | tr 'A-Z' 'a-z')
```

and in the function's comment change `One client's RETURN / offload / MARK in <chain>.` to `One client's RETURNs (country sets, addresses and CIDRs) / offload / MARK in <chain>.`

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/tunnel.bats` - all pass.

- [ ] **Step 5: The whole suite (guard) and commit**

Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats
git commit -m "feat(tunnel): addresses and CIDRs in a tunnel's exclude" -m "A tunnel's exclude took country codes only, so a LAN host's own tunnel to the
server behind the router's tunnel - the overlay channels to yop - went inside
it, an AWG link inside another with the smaller MTU. An IPv4 address or CIDR in
exclude now returns its destination direct, beside the countries; the ipset
builder already ignores such entries."
```

---

### Task 8: `ensure-routes`, its cron job and the `nat-start` hook

**Files:**
- Modify: `router/opt/vpn-director/vpn-director.sh` (header usage, `show_help`, `cmd_apply`, `cmd_update`, new `_routes_cron_sync` and `cmd_ensure_routes`, `cmd_cron`, dispatch), `router/files.manifest`
- Create: `router/jffs/scripts/nat-start` (mode 0755)
- Test: `router/test/integration/vpn_director.bats`, `router/test/unit/hooks.bats`

**Interfaces:**
- Consumes: `iftunnel_routes_missing`, `IFTUNNEL_STATE_FILE`, `IFTUNNEL_CRON_MARKER` (Task 2); `platform_cron_add`, `platform_cron_del`, `platform_cron_requirements`; `cmd_apply`, `_skip_when_stopped`.
- Produces: the command `vpn-director.sh ensure-routes` (`cmd_ensure_routes`); `_routes_cron_sync`, called after every `tunnel_apply` of the CLI.

- [ ] **Step 1: Write the failing tests**

In `router/test/integration/vpn_director.bats`, after the test `vpn-director: help lists platform and cron`, add:

```bash
@test "vpn-director: help lists ensure-routes" {
    run "$SCRIPTS_DIR/vpn-director.sh" --help
    assert_output --partial "ensure-routes"
}

# ============================================================================
# ensure-routes: the route an interface tunnel lost comes back within a minute
# ============================================================================

# run_ensure_routes - cmd_ensure_routes with cmd_apply noting what it was asked
# for; the record of the applied interface tunnels, the links and the routes are
# the test's.
run_ensure_routes() {
    export VPD_STOPPED_FILE="${VPD_STOPPED_FILE:-$BATS_TEST_TMPDIR/stopped}"
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    run bash -c '
        script=$1 calls=$2
        shift 2
        source "$script" --source-only "$@"
        cmd_apply() { echo "cmd_apply COMPONENT=$COMPONENT UNLESS_STOPPED=$UNLESS_STOPPED" >> "$calls"; }
        cmd_ensure_routes
    ' -- "$SCRIPTS_DIR/vpn-director.sh" "$BATS_TEST_TMPDIR/calls" ensure-routes
}

@test "vpn-director: ensure-routes is quiet when no interface tunnel is applied" {
    run_ensure_routes
    assert_success
    refute_output
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

@test "vpn-director: ensure-routes applies Tunnel Director when an interface is UP without its route" {
    printf 'awg1 2001\n' > "$BATS_TEST_TMPDIR/iftunnels"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    run_ensure_routes
    assert_success
    assert_output --partial "Interface tunnel back without its route: awg1; applying Tunnel Director"
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output "cmd_apply COMPONENT=tunnel UNLESS_STOPPED=1"
}

@test "vpn-director: ensure-routes leaves a route that is in place alone" {
    printf 'awg1 2001\n' > "$BATS_TEST_TMPDIR/iftunnels"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    ip route replace default dev awg1 table 2001
    run_ensure_routes
    assert_success
    refute_output
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

# Review Focus 4: S51awg1 sets the interface UP last, and a route through it is
# refused until then; the next minute finds it UP.
@test "vpn-director: ensure-routes waits for an interface that is not UP yet" {
    printf 'awg1 2001\n' > "$BATS_TEST_TMPDIR/iftunnels"
    ip_links "awg1 <POINTOPOINT,NOARP> 1280"
    run_ensure_routes
    assert_success
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

@test "vpn-director: ensure-routes does nothing once stop has run" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    printf 'awg1 2001\n' > "$BATS_TEST_TMPDIR/iftunnels"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    run_ensure_routes
    assert_success
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

# An apply that holds the lock does the same work, so ensure-routes does not
# queue behind it. The test holds the CLI's lock on FD 201.
@test "vpn-director: ensure-routes leaves the work to an apply that holds the lock" {
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    exec 201>"/var/lock/vpn-director.lock"
    flock -n 201
    run "$SCRIPTS_DIR/vpn-director.sh" ensure-routes
    flock -u 201
    assert_success
    assert_output --partial "Another instance is already running"
}

# ============================================================================
# The cron job of ensure-routes follows the interface tunnels
# ============================================================================

@test "vpn-director: an apply with an interface tunnel schedules ensure-routes every minute, once" {
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    export IFTUNNEL_CRON_MARKER="$BATS_TEST_TMPDIR/routes_cron"
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    : > /tmp/bats_cru_calls.log
    run_stubbed_cli apply
    assert_success
    local vpd_dir
    vpd_dir="$(cd "$SCRIPTS_DIR" && pwd)"
    grep -qF "cru a vpn_director_routes * * * * * $vpd_dir/vpn-director.sh ensure-routes" /tmp/bats_cru_calls.log
    [ -e "$IFTUNNEL_CRON_MARKER" ]

    : > /tmp/bats_cru_calls.log
    run_stubbed_cli apply tunnel
    assert_success
    refute grep -q 'vpn_director_routes' /tmp/bats_cru_calls.log
}

@test "vpn-director: an apply with no interface tunnel left drops the route check" {
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    export IFTUNNEL_CRON_MARKER="$BATS_TEST_TMPDIR/routes_cron"
    : > "$IFTUNNEL_CRON_MARKER"
    : > /tmp/bats_cru_calls.log
    run_stubbed_cli update
    assert_success
    grep -qF "cru d vpn_director_routes" /tmp/bats_cru_calls.log
    [ ! -e "$IFTUNNEL_CRON_MARKER" ]
}

@test "vpn-director: a cron that refuses the route check costs a WARN, not the apply" {
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    export IFTUNNEL_CRON_MARKER="$BATS_TEST_TMPDIR/routes_cron"
    printf 'awg1 2001\n' > "$IFTUNNEL_STATE_FILE"
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/bash\nexit 1\n' > "$BATS_TEST_TMPDIR/bin/cru"
    chmod +x "$BATS_TEST_TMPDIR/bin/cru"
    PATH="$BATS_TEST_TMPDIR/bin:$PATH" run_stubbed_cli apply
    assert_success
    assert_output --partial "Could not schedule the route check of the interface tunnels"
    [ ! -e "$IFTUNNEL_CRON_MARKER" ]
}

@test "vpn-director: cron remove drops the route check with the daily update" {
    export IFTUNNEL_CRON_MARKER="$BATS_TEST_TMPDIR/routes_cron"
    : > "$IFTUNNEL_CRON_MARKER"
    : > /tmp/bats_cru_calls.log
    run "$SCRIPTS_DIR/vpn-director.sh" cron remove
    assert_success
    grep -qF "cru d vpn_director_routes" /tmp/bats_cru_calls.log
    grep -qF "cru d vpn_director_update" /tmp/bats_cru_calls.log
    [ ! -e "$IFTUNNEL_CRON_MARKER" ]
}
```

In `router/test/unit/hooks.bats`, after the test `wan-event: leaves the other WAN events alone`, add:

```bash
# The firmware rebuilds nat on its own - a WAN coming up, a port forward
# changed - and the rebuild takes the MASQUERADE of every interface tunnel.
@test "nat-start: re-applies, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/nat-start"
    assert_success
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "--wait apply"
}

@test "nat-start: is POSIX sh and executable in the tree" {
    [ -x "$MERLIN_HOOKS/nat-start" ]
    head -n1 "$MERLIN_HOOKS/nat-start" | grep -qx '#!/bin/sh'
    sh -n "$MERLIN_HOOKS/nat-start"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/integration/vpn_director.bats router/test/unit/hooks.bats`
Expected: the new tests fail (`cmd_ensure_routes: command not found`, `Unknown command: ensure-routes`, no `cru a vpn_director_routes`, no `nat-start` file). Every other test passes.

- [ ] **Step 3: Implement the CLI side**

In `router/opt/vpn-director/vpn-director.sh`:

1. In the header usage block, after `#   vpn-director cron install|remove         - Schedule or drop the daily ipset update`, add `#   vpn-director ensure-routes               - Put back a route an interface tunnel lost (cron runs it)`.

2. In `show_help`, after the line `  cron install|remove         Schedule or drop the daily "update" job`, add `  ensure-routes               Put back a route an interface tunnel lost (cron runs it every minute)`.

3. In `cmd_apply`, in the `""|all)` branch add `_routes_cron_sync` after `_prune_keeping "$(tunnel_uncarried)"`, and in the `tunnel)` branch add `_routes_cron_sync` after `tunnel_apply`. In `cmd_update`, add `_routes_cron_sync` after `_prune_keeping "$(tunnel_uncarried)"`.

4. Right before the comment block of `cmd_platform`, add:

```bash
###################################################################################################
# _routes_cron_sync - the cron job of ensure-routes follows the interface tunnels
# -------------------------------------------------------------------------------------------------
# Called after every tunnel_apply. With an interface tunnel applied (lib/iftunnel.sh's record) and
# no marker, it schedules "ensure-routes" every minute and leaves the marker; with none and the
# marker, it drops both. The marker lives in /tmp, as Merlin's cru jobs do, so the first apply
# after a reboot schedules the job again, and an upgrade gets it with its first apply. A cron
# that refuses the job costs a WARN and no marker - the next apply tries again - never the apply.
###################################################################################################
_routes_cron_sync() {
    local cron_req
    if [[ -s $IFTUNNEL_STATE_FILE ]]; then
        [[ ! -e $IFTUNNEL_CRON_MARKER ]] || return 0
        if platform_cron_add vpn_director_routes "* * * * *" "$SCRIPT_DIR/vpn-director.sh ensure-routes"; then
            mkdir -p "$(dirname "$IFTUNNEL_CRON_MARKER")"
            : > "$IFTUNNEL_CRON_MARKER"
            log "Scheduled the route check of the interface tunnels (every minute)"
        else
            log -l WARN "Could not schedule the route check of the interface tunnels; an interface re-created by its own scripts keeps its clients refused until the next apply"
            if cron_req="$(platform_cron_requirements)"; then
                log -l WARN "$cron_req"
            fi
        fi
    elif [[ -e $IFTUNNEL_CRON_MARKER ]]; then
        platform_cron_del vpn_director_routes || true
        rm -f "$IFTUNNEL_CRON_MARKER"
        log "Dropped the route check of the interface tunnels"
    fi
}

###################################################################################################
# cmd_ensure_routes - put back the route of an interface tunnel whose interface came back
# -------------------------------------------------------------------------------------------------
# Deleting an interface takes its routes along: S51awg1's watchdog restarts awg1 with
# "ip link del", and the tunnel's table stays empty - its clients refused - until an apply puts
# the default back. The cron job vpn_director_routes runs this every minute. A minute with nothing
# missing costs common.sh, one state file and two ip calls per interface tunnel: no config, no
# jq. Once stop has run it does nothing; an apply that holds the lock does the same work, so it
# does not wait for one (cmd_apply takes the lock without --wait).
###################################################################################################
cmd_ensure_routes() {
    _load_common
    [[ ! -e ${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped} ]] || return 0
    local missing
    missing="$(iftunnel_routes_missing)" || return 0
    log "Interface tunnel back without its route: ${missing//$'\n'/ }; applying Tunnel Director"
    UNLESS_STOPPED=1
    COMPONENT=tunnel cmd_apply
}
```

5. In `cmd_cron`, in the `remove)` branch, before `log "Removed daily ipset update"`, add:

```bash
            # The route check of the interface tunnels goes with it: S99 stop
            # runs this, and an apply schedules it again.
            platform_cron_del vpn_director_routes || true
            rm -f "$IFTUNNEL_CRON_MARKER"
```

6. In the dispatch `case "$COMMAND" in`, after the `cron)` branch, add:

```bash
    ensure-routes)
        cmd_ensure_routes
        ;;
```

- [ ] **Step 4: Create the hook and ship it**

Create `router/jffs/scripts/nat-start`:

```sh
#!/bin/sh
# Asuswrt-Merlin runs this after it rebuilt the nat table - a WAN coming up, a
# port forward changed - without waiting for it. The rebuild replaces the whole
# table, so the MASQUERADE of every interface tunnel (tunnel_director.interfaces)
# is gone until an apply puts it back. A plain apply exits at once while another
# instance holds the lock; --wait queues it behind the running one instead.

PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
VPD_SCRIPT="${VPD_SCRIPT:-/opt/vpn-director/vpn-director.sh}"

if [ -x "$VPD_SCRIPT" ]; then
    "$VPD_SCRIPT" --wait apply
else
    logger -t "nat-start" "vpn-director not found at $VPD_SCRIPT"
fi
```

Run: `chmod 755 router/jffs/scripts/nat-start`.

In `router/files.manifest`, after `merlin   router/jffs/scripts/wan-event`, add `merlin   router/jffs/scripts/nat-start`.

- [ ] **Step 5: Run the tests to see them pass**

Run: `bats router/test/integration/vpn_director.bats router/test/unit/hooks.bats` - all pass.
Run: `cd server && go test ./internal/updater/ -count=1` - PASS (the manifest ships `nat-start`).

- [ ] **Step 6: The whole suite (guard) and commit**

Run: `bats router/test/*.bats router/test/unit router/test/integration` - all pass.

```bash
git add router/opt/vpn-director/vpn-director.sh router/jffs/scripts/nat-start router/files.manifest \
    router/test/integration/vpn_director.bats router/test/unit/hooks.bats
git commit -m "feat(cli): ensure-routes, its cron job and the nat-start hook" -m "An interface re-created by its own init script loses the route in its table,
and with the kill switch its clients are refused until an apply puts it back.
ensure-routes, run every minute by the cron job an apply schedules while an
interface tunnel exists, finds such an interface without loading the config and
applies Tunnel Director. Merlin rebuilds nat apart from firewall-start; the new
nat-start hook applies after it, so MASQUERADE comes back."
```

---

### Task 9: Keep the list in Go; the bot and the Web UI stop promising the WAN

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`TunnelDirectorConfig`, `PlatformTunnel`), `server/internal/handler/clients.go` (`moveQuestion` and its comment), `web/src/components/ClientsTab.vue` (`isDown`/`isUnlisted` comments, `confirmDown`, `confirmUnlisted`)
- Test: `server/internal/vpnconfig/vpnconfig_test.go`, `server/internal/handler/clients_test.go`

**Interfaces:**
- Consumes: the JSON key `tunnel_director.interfaces` and `type: "interface"` (Tasks 1, 3).
- Produces: `vpnconfig.TunnelDirectorConfig.Interfaces []string` (`json:"interfaces,omitempty"`).

- [ ] **Step 1: Write the failing Go tests and update the one whose text changed**

Append to `server/internal/vpnconfig/vpnconfig_test.go`:

```go
// A daemon writes the config from its struct: a key the struct lacks is gone
// after the first save - a client moved in the Web UI, a failover commit.
func TestSaveVPNDirectorConfig_KeepsTunnelDirectorInterfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpn-director.json")
	in := `{"data_dir":"/data","tunnel_director":{"interfaces":["awg1"],"tunnels":{"awg1":{"clients":["192.168.1.30"],"exclude":["ru","203.0.113.7"]}}},"xray":{"clients":[],"servers":[],"exclude_ips":[],"exclude_sets":[]}}`
	if err := os.WriteFile(path, []byte(in), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	again, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("load again: %v", err)
	}
	if !reflect.DeepEqual(again.TunnelDirector.Interfaces, []string{"awg1"}) {
		t.Errorf("interfaces = %v, want [awg1]", again.TunnelDirector.Interfaces)
	}
	if got := again.TunnelDirector.Tunnels["awg1"].Exclude; !reflect.DeepEqual(got, []string{"ru", "203.0.113.7"}) {
		t.Errorf("exclude = %v, want [ru 203.0.113.7]", got)
	}
}

func TestSaveVPNDirectorConfig_OmitsAnEmptyInterfaceList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpn-director.json")
	cfg := &VPNDirectorConfig{
		DataDir:        "/data",
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{}},
		Xray:           XrayConfig{Clients: []string{}, Servers: []string{}, ExcludeSets: []string{}},
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "interfaces") {
		t.Errorf("an empty interface list was written:\n%s", data)
	}
}
```

Append to `server/internal/handler/clients_test.go`:

```go
// The kill switch: a tunnel that carries nothing refuses what its clients would
// send through it, so neither question may promise the WAN any more.
func TestMoveQuestion_TheTunnelRefusesWhatItWouldCarry(t *testing.T) {
	info := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "awg1", Iface: "awg1", Type: "interface", Connected: false},
	}}
	want := "awg1 is down: until it is up, Tunnel Director blocks the traffic 192.168.1.30 would send through it (excluded destinations still go direct). Move anyway?"
	if got := moveQuestion(info, "awg1", "192.168.1.30"); got != want {
		t.Errorf("down:\n got %q\nwant %q", got, want)
	}
	want = "wgc9 is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic 192.168.1.30 would send through it (excluded destinations still go direct). Move anyway?"
	if got := moveQuestion(info, "wgc9", "192.168.1.30"); got != want {
		t.Errorf("unknown:\n got %q\nwant %q", got, want)
	}
}
```

In `TestClientsHandler_MoveToAnUnknownTunnelAsksFirst` (same file), replace the `want :=` line with:

```go
	want := "wgc1 is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic 192.168.50.10 would send through it (excluded destinations still go direct). Move anyway?"
```

and its comment's last two sentences (`is one Tunnel Director skips. The move asks first, as for a tunnel that is down.`) with `refuses its clients until it is listed. The move asks first, as for a tunnel that is down.`

- [ ] **Step 2: Run them to see them fail**

Run (from `server/`): `go test ./internal/vpnconfig/ ./internal/handler/ -count=1`
Expected: FAIL - `again.TunnelDirector.Interfaces undefined (type TunnelDirectorConfig has no field or method Interfaces)` stops the vpnconfig package from compiling; the handler tests fail on the texts.

- [ ] **Step 3: Implement the Go side**

In `server/internal/vpnconfig/vpnconfig.go`, make `TunnelDirectorConfig` read:

```go
type TunnelDirectorConfig struct {
	// Interfaces are the Linux interfaces Tunnel Director may route through
	// beside the firmware's tunnels (tunnel_director.interfaces,
	// lib/iftunnel.sh): a tunnel key equal to one of them is an interface
	// tunnel. Kept here so a save by a daemon does not drop the list.
	Interfaces []string `json:"interfaces,omitempty"`
	// Note: Go's json.Marshal sorts map keys alphabetically, so tunnel order
	// in saved JSON is deterministic. This matches jq 'keys[]' behavior.
	// If insertion-order priority is needed, this should be changed to a slice.
	Tunnels map[string]TunnelConfig `json:"tunnels"`
}
```

and replace the comment and the `Type` line of `PlatformTunnel`:

```go
// PlatformTunnel is one tunnel Tunnel Director may route through, as
// `vpn-director.sh platform` lists it: a firmware VPN client, or an interface
// of tunnel_director.interfaces. ID is what tunnel_director.tunnels is keyed by.
type PlatformTunnel struct {
	ID          string `json:"id"`
	Iface       string `json:"iface"`
	Type        string `json:"type"` // openvpn, wireguard or interface
```

In `server/internal/handler/clients.go`, replace `moveQuestion` and its comment with:

```go
// moveQuestion is what a move of ip to the tunnel route asks before it goes
// ahead, given what the platform answered, or "" when it asks nothing. A tunnel
// listed as down has no route in its table, and Tunnel Director refuses what
// the client would send through it until it is up (the block rule after the
// tunnel's lookup rule). A configured tunnel the platform does not list -
// "(unknown)": a typo, a connection deleted in the router, RCI silent - refuses
// its clients the same way until it is listed. Excluded destinations go direct
// either way. main is Tunnel Director's own route, which no platform lists.
func moveQuestion(info vpnconfig.PlatformInfo, route, ip string) string {
	if route == "main" {
		return ""
	}
	for _, t := range info.Tunnels {
		if t.ID == route {
			if t.Connected {
				return ""
			}
			return fmt.Sprintf("%s is down: until it is up, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?", route, ip)
		}
	}
	return fmt.Sprintf("%s is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic %s would send through it (excluded destinations still go direct). Move anyway?", route, ip)
}
```

- [ ] **Step 4: The Web UI texts**

In `web/src/components/ClientsTab.vue`:
- the comment above `isDown`: replace `// A tunnel the platform reports down has no route in its table, and a client` / `// put on it goes out through the WAN until it comes up.` with `// A tunnel the platform reports down has no route in its table, and Tunnel` / `// Director refuses what a client put on it would send through it until it is up.`
- `confirmDown`'s text becomes `` `${route} is down: until it is up, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``
- the comment above `isUnlisted`: replace `// router - is one Tunnel Director skips until it is listed, and a client put` / `// on it goes out through the WAN meanwhile. Asked only when the platform` with `// router - refuses its clients until it is listed. Asked only when the platform`
- `confirmUnlisted`'s text becomes `` `${route} is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic this client would send through it (excluded destinations still go direct). ${verb} anyway?` ``

- [ ] **Step 5: Run the checks**

Run (from `server/`): `go test ./... -count=1` - PASS. `go vet ./...` - clean. `gofmt -l .` - prints nothing.
Run: `cd web && npm ci && npm test && npm run build` - passes (`npm run build` is `vue-tsc -b && vite build`, the type check).

- [ ] **Step 6: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go \
    server/internal/handler/clients.go server/internal/handler/clients_test.go \
    web/src/components/ClientsTab.vue
git commit -m "feat(server,web): keep tunnel_director.interfaces; texts of the kill switch" -m "SaveVPNDirectorConfig writes the config from its struct, so the first save by a
daemon would have dropped tunnel_director.interfaces. The questions before a
move to a tunnel that is down or not listed promised the WAN; with the kill
switch the tunnel refuses the client's traffic, and excluded destinations
still go direct."
```

---

### Task 10: Documentation

**Files:**
- Modify: `.claude/rules/tunnel-director.md`, `.claude/rules/packet-flow.md`, `.claude/rules/telegram-bot.md`, `.claude/rules/testing.md`, `README.md`, `README.ru.md`, `CLAUDE.md`

**Interfaces:** none.

- [ ] **Step 1: `.claude/rules/tunnel-director.md`**

1. In "Configuration Format", replace the JSON example with:

```json
{
  "tunnel_director": {
    "interfaces": ["awg1"],
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
      },
      "awg1": {
        "clients": ["192.168.1.30"],
        "exclude": ["<country_code>", "203.0.113.7"]
      }
    }
  }
}
```

2. In the field table, add the first row `| `interfaces` | Linux interfaces Tunnel Director may route through beside the firmware's tunnels; a tunnel key equal to one of them is an interface tunnel (see "Interface tunnels") |`, and change the `exclude` row to `| `exclude` | Array of country codes, IPv4 addresses and CIDRs; a client's traffic to them goes direct |`.

3. In "Behavior", add the bullet `- A tunnel whose table holds no route refuses its clients' traffic instead of sending it to the WAN (see "The kill switch"); excluded destinations still go direct`.

4. In "Which tunnels the platform lists", replace the paragraph that begins `` `tunnel.sh` asks `platform_tunnel_unlisted_reason` `` with:

```markdown
`tunnel.sh` asks `vpd_tunnel_unlisted_reason` why a tunnel is left out - a rejected name of
`tunnel_director.interfaces` first, the platform's reason otherwise - and puts the answer in its
warning. A tunnel with clients that the platform does not list keeps a slot, its clients' MARK
rules and the slot's block rule: `Tunnel 'ovpnc1' is not on the platform's list: OpenVPN client 1
is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No"); its clients are
refused`, or `Tunnel 'wgc9' is not a tunnel this platform knows; its clients are refused`. When the
layer still names its table - KeeneticOS's `WireguardN` by its naming convention while RCI is
silent, Merlin's `ovpncN` - its route and lookup rule go in as for a listed tunnel, and the
platform decides about the route (Merlin refuses `ovpncN` in "No" mode). When it names none - a
typo, an interface taken off `tunnel_director.interfaces` - the slot keeps the block rule alone,
and the lookup rule and table a known past left go (`_tunnel_routeless`, `_tunnel_unroute`).
`route not installed: <reason>; its clients are refused` is the warning for a recorded tunnel on
the up-to-date branch. A configuration that gives such a tunnel clients records no hash, as one
with a typo in the id does: every apply rebuilds in place and repeats the warning, and the first
apply after the switch to VPN Director carries the tunnel's clients. One left with no clients
(moved, deleted or paused) gets no slot and keeps the old warning (`is skipped: <reason>`), and
records the hash: it has nothing to carry.
```

5. After the section "Which tunnels the platform lists", add:

```markdown
## Interface tunnels

`tunnel_director.interfaces` lists Linux interfaces no firmware manages - an AmneziaWG or WireGuard
interface an Entware init script brings up. `lib/iftunnel.sh` checks the list once per run
(`iftunnel_init`): a name is kept when it is 1-15 characters of `A-Za-z0-9_.-` (not `.` or `..`)
and is not `main`, an id `platform_tunnels` lists, a LAN interface or the WAN; each rejected name
logs one WARN, `tunnel_director.interfaces: <reason>; ignored`, and a tunnel key that names it is
unknown. The core reaches every tunnel through the `vpd_*` layer of the same file: a kept name goes
to `iftunnel_*`, any other id to the platform contract, which describes the firmware alone.

An interface tunnel gets:

- table `2000 + idx` (`IFTUNNEL_TABLE_BASE`; KeeneticOS's own tables have the same numbering, and a
  slot belongs to one tunnel) and `ip route replace default dev <iface> table <T>` while the
  interface exists and is UP;
- at the head of their chains, `nat POSTROUTING -o <iface> -j MASQUERADE`, `mangle FORWARD -o
  <iface> -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu` and `mangle FORWARD
  -i <iface> ... --set-mss <MTU - 40>` (the SYN-ACK coming back, for which `--clamp-mss-to-pmtu`
  would read the LAN's MTU). The first two need no interface - iptables matches its name - and
  outlive its re-creation.

`iftunnel_route_ensure` runs on every apply, so the firmware's rebuilds get the rules back with
the next apply: Merlin's `iptables -t mangle -F` with `firewall-start`, a rebuild of `nat` with
`nat-start`, KeeneticOS's with its netfilter hook. `iftunnel_table_release` flushes the table and
removes the rules for a dropped slot and on `stop`.

Deleting an interface takes its route along. Each apply records the interface tunnels in
`/tmp/tunnel_director/iftunnels` (`<iface> <table>`), and the cron job `vpn_director_routes`, which
an apply schedules while one exists, runs `vpn-director.sh ensure-routes` every minute: an
interface that is UP with no default in its table makes it run `apply tunnel` (no config, no jq on
a minute with nothing missing; nothing once `stop` has run). The interface's own init script can
run `vpn-director.sh --unless-stopped --wait apply tunnel` after bringing it up - in the background,
its own lock descriptors closed - to make it seconds instead of a minute.

`vpn-director.sh platform` lists each kept interface after the firmware's tunnels with
`type: "interface"` and `connected` while it is UP, so the Web UI, the bot and watchd treat it like
any tunnel. `status tunnel` shows each one's table, route and rules.

## The kill switch

Every slot but `main`'s has two ip rules: the lookup rule at `pref_base + idx` and, right after the
lookup range, the block rule at `pref_base + field_max + idx` (16639-16893 with the defaults):
`fwmark <mark>/<mask> prohibit`. When the tunnel's table holds no route - the interface is missing
or down, the route never went in, the firmware flushed the table - the lookup falls through to the
block rule, never to `main`, and the client gets ICMP "administratively prohibited". Exclusions
return ahead of the mark, so an excluded destination still goes direct.

A rebuild puts each slot's block rule in before its route and lookup rule; the up-to-date path
ensures it beside the lookup rule (`_tunnel_block_ensure`), which is how a router an earlier
version applied gets it on its first apply after the upgrade; a dropped slot loses it after the
swap; `tunnel_stop` clears both ranges. `tunnel_apply` refuses, with an ERROR and before touching
a rule, a `pref_base` and mark field whose block range would reach main's rule at 32766. The
firmware's own killswitch (`vpn_clientN_enforce`, `wgcN_enforce`) no longer matters for Tunnel
Director's clients.
```

6. In "What tunnel_apply reports as not carried", change the first table row to `| Rebuild | The clients of a tunnel the platform does not list (it keeps a slot whose block rule refuses them), and of one the mark field has no room for |` and the second row's `its mark reaches `main`` to `its mark meets the block rule`.

7. In "State Tracking", add the rows `| `/tmp/tunnel_director/iftunnels` | The applied interface tunnels, `<iface> <table>` per line (`IFTUNNEL_STATE_FILE`) |` and `| `/tmp/tunnel_director/routes_cron` | Marker: the cron job of `ensure-routes` is scheduled (`IFTUNNEL_CRON_MARKER`) |`.

8. In "Key Functions", internal table, add the rows `| `_tunnel_block_ensure(idx, tunnel)` | The slot's block rule, put back when it is gone |`, `| `_tunnel_route_warn(tunnel)` | The warning for a route that did not go in, with the layer's reason |`, `| `_tunnel_routeless(tunnel, idx)` | Has a tunnel the platform does not list no table to route through? |` and `| `_tunnel_unroute(idx, tunnel)` | Drop the lookup rule and the table a known past left on such a slot |`.

9. In "Dependencies", replace the paragraph `From the platform contract (...)` with `` Through the `vpd_*` layer of `lib/iftunnel.sh` (sourced by `common.sh`): `vpd_tunnels`, `vpd_tunnel_unlisted_reason`, `vpd_tunnel_table`, `vpd_tunnel_route_ensure`, `vpd_tunnel_table_release`, `vpd_tunnel_retired`, and `iftunnel_init`, `iftunnel_state_write`, `iftunnel_status`. From the platform contract directly: `platform_tunnel_offload_target`, `platform_prerouting_base_pos`, `platform_lan_ifaces`. ``

10. In "Requirements", change `- VPN client must be active with NAT enabled` to `- A firmware VPN client must be active with NAT enabled; an interface tunnel gets its NAT from Tunnel Director`.

- [ ] **Step 2: `.claude/rules/packet-flow.md`**

1. In the "ip rule lookup" box of "Packet Processing Order", after the line `│  pref 16385: fwmark 0x20000/0xff0000 ──► table ovpnc1 / 2001│`, add a line `│  pref 16639+: same marks ──► prohibit (no route in the table)│` (keep the box's width).
2. In "Left open", replace `- A tunnel that is down sends its clients to `main`. One that comes from Xray while the tunnel's route cannot be installed stays proxied instead (step 3).` with `- A tunnel that is down refuses its clients' traffic (the block rule after its lookup rule); excluded destinations still go direct. One that comes from Xray while the tunnel's route cannot be installed stays proxied instead (step 3).`
3. In "IP Rules" (Tunnel Director Details), after the existing code block add:

```bash
# Right after the lookup range, for every slot but main's (the kill switch):
ip rule add pref {16384 + 255 + idx} fwmark {mark}/{mask} prohibit
```

4. In the TUN_DIR chain structure block, after `-s 192.168.1.5 -m set --match-set <country> dst → RETURN` add a line `  -s 192.168.1.5 -d 203.0.113.7 → RETURN                      (an address in exclude)`.
5. In "Key Code Locations", add the rows `| `lib/tunnel.sh` | `_tunnel_block_ensure()` | The slot's `prohibit` rule after its lookup rule |` and `| `lib/iftunnel.sh` | `iftunnel_route_ensure()`, `vpd_*` | Interface tunnels; the layer the core calls for any tunnel |`.

- [ ] **Step 3: `.claude/rules/telegram-bot.md` and `.claude/rules/testing.md`**

In `telegram-bot.md`, section "Clients (`/clients`)", replace `` and so does an "(unknown)" one, which Tunnel Director skips until the platform lists it: "`<route>` is not on the router's tunnel list: until it is, Tunnel Director does not route `<ip>` through it. Move anyway?". `` with `` and so does an "(unknown)" one, which refuses its clients until the platform lists it: "`<route>` is not on the router's tunnel list: until it is, Tunnel Director blocks the traffic `<ip>` would send through it (excluded destinations still go direct). Move anyway?"; the question for a tunnel that is down says the same with "is down: until it is up". ``

`.claude/rules/webui.md` holds no copy of the two confirmation texts (they live in `ClientsTab.vue`, changed in Task 9): nothing to change there.

In `testing.md`, in the test tree under `unit/`, add `│   ├── iftunnel.bats         # Tests for lib/iftunnel.sh (interface tunnels, the vpd_* layer)`, and in the helper table add `| `ip_links "<iface> <flags> <mtu>"...` | For the rest of the test, the interfaces the `ip` mock's `link show` knows; any other name does not exist |`. In "Mocks", extend the `ip` line to `- **ip**: Returns mock interface info; keeps ip rules (lookup and prohibit) and, per table, the routes add/replace put in and del/flush took out; `link show` answers from `ip_links``.

- [ ] **Step 4: `README.md` and `README.ru.md`**

In `README.md`, section "Tunnel Director", after the JSON example, add:

````markdown
Tunnel Director also routes through a Linux interface no firmware manages - an AmneziaWG or WireGuard interface an Entware script brings up. List it in `tunnel_director.interfaces` and use its name as a tunnel key: the Web UI, the bot and the failover watch offer it like `wgc1`, and Tunnel Director gives it its own routing table, a default route through the interface, `MASQUERADE` and MSS clamping. When the interface is re-created, the cron job `vpn_director_routes` (`vpn-director.sh ensure-routes`, every minute) puts its route back; the script that brings the interface up can run `vpn-director.sh --unless-stopped --wait apply tunnel` to do it at once. A tunnel's `exclude` takes IPv4 addresses and CIDRs beside country codes.

```json
{
  "tunnel_director": {
    "interfaces": ["awg1"],
    "tunnels": {
      "awg1": { "clients": ["192.168.1.30"], "exclude": ["<country_code>", "203.0.113.7"] }
    }
  }
}
```

A tunnel with no route in its table - down, its interface re-created, not on the platform's list - refuses its clients' traffic instead of sending it out through the WAN; excluded destinations still go direct.
````

and in "What remains" replace the bullet `- A tunnel that is down sends its clients through the WAN (on Merlin, the VPN client's killswitch, when enabled, prevents that). The Web UI and the bot ask before they move a client to a tunnel that is down or that the router does not list.` with `- A tunnel that is down refuses its clients' traffic until it is up (excluded destinations still go direct). The Web UI and the bot ask before they move a client to a tunnel that is down or that the router does not list.` In "Commands", after `/opt/vpn-director/vpn-director.sh update              # Update ipsets + reapply`, add `/opt/vpn-director/vpn-director.sh ensure-routes       # Put back a route an interface tunnel lost (cron, every minute)`.

In `README.ru.md`, the same three places:

````markdown
Tunnel Director также маршрутизирует через Linux-интерфейс, которым прошивка не управляет, - например, интерфейс AmneziaWG или WireGuard, который поднимает скрипт Entware. Перечислите его в `tunnel_director.interfaces` и используйте его имя как ключ туннеля: Web UI, бот и failover предлагают его наравне с `wgc1`, а Tunnel Director даёт ему свою таблицу маршрутизации, маршрут по умолчанию через интерфейс, `MASQUERADE` и прижатие MSS. Когда интерфейс пересоздан, маршрут возвращает задание cron `vpn_director_routes` (`vpn-director.sh ensure-routes`, раз в минуту); скрипт, который поднимает интерфейс, может сразу вызвать `vpn-director.sh --unless-stopped --wait apply tunnel`. В `exclude` туннеля, кроме кодов стран, можно указывать IPv4-адреса и CIDR.

```json
{
  "tunnel_director": {
    "interfaces": ["awg1"],
    "tunnels": {
      "awg1": { "clients": ["192.168.1.30"], "exclude": ["<country_code>", "203.0.113.7"] }
    }
  }
}
```

Туннель без маршрута в своей таблице - упавший, с пересоздаваемым интерфейсом, неизвестный платформе - блокирует трафик своих клиентов, а не отпускает его в WAN; исключения по-прежнему идут напрямую.
````

"Что остаётся": replace `- Упавший туннель отправляет своих клиентов в WAN (на Merlin этого не будет, если у VPN-клиента прошивки включён killswitch). Web UI и бот спрашивают подтверждение, прежде чем перенести клиента на упавший туннель или на туннель, которого роутер не знает.` with `- Упавший туннель блокирует трафик своих клиентов, пока не поднимется (исключения идут напрямую). Web UI и бот спрашивают подтверждение, прежде чем перенести клиента на упавший туннель или на туннель, которого роутер не знает.` "Команды": add `/opt/vpn-director/vpn-director.sh ensure-routes       # Вернуть маршрут, потерянный туннелем-интерфейсом (cron, раз в минуту)`.

- [ ] **Step 5: `CLAUDE.md`**

1. In "Commands", after `/opt/vpn-director/vpn-director.sh cron install        # Schedule the daily update (S99 does this)`, add `/opt/vpn-director/vpn-director.sh ensure-routes       # Put back a route an interface tunnel lost (its cron job runs it every minute)`.
2. In "Architecture", after the `lib/platform/keenetic.sh` row, add `| `router/opt/vpn-director/lib/iftunnel.sh` | Interface tunnels (`tunnel_director.interfaces`): table 2000+idx, route, NAT/MSS; the `vpd_*` layer the core calls for any tunnel |`, and after the `router/jffs/scripts/wan-event` row add `| `router/jffs/scripts/nat-start` | Asuswrt-Merlin hook after a rebuild of nat |`.
3. In "Key Concepts", Tunnel Director, after the bullet that begins `- A tunnel key is any id `platform_tunnels` lists`, add:

```markdown
- A tunnel key may also be an interface listed in `tunnel_director.interfaces` (an AmneziaWG
  `awg1` an Entware script brings up): Tunnel Director gives it table 2000+idx, its route, NAT
  and MSS (`lib/iftunnel.sh`); `ensure-routes` puts a route its re-creation took back
- A tunnel whose table holds no route refuses its clients' traffic (a `prohibit` rule per slot
  after the lookup range); one the platform does not list keeps a slot that refuses its clients
- `exclude` takes IPv4 addresses and CIDRs beside country codes
```

- [ ] **Step 6: Check and commit**

Run: `grep -rn "goes out through the WAN until\|Tunnel Director does not route" README.md README.ru.md .claude/rules/ web/src server/internal --include=*.md --include=*.vue --include=*.go`
Expected: no line that describes a tunnel that is down or unknown.

```bash
git add .claude/rules/tunnel-director.md .claude/rules/packet-flow.md .claude/rules/telegram-bot.md \
    .claude/rules/testing.md README.md README.ru.md CLAUDE.md
git commit -m "docs: interface tunnels and the kill switch" -m "The rules, the README in both languages and CLAUDE.md said a tunnel that is down
sends its clients through the WAN and named the firmware's tunnels as the only
ones. They now describe tunnel_director.interfaces, the block rule after each
lookup rule, unknown tunnels that keep a slot, ensure-routes with its cron job,
nat-start, and addresses in exclude."
```

---

### Task 11: Whole-branch verification

**Files:** none changed, unless a check fails - then fix in the task that owns the code and commit there.

- [ ] **Step 1: Shell**

Run: `bats router/test/*.bats router/test/unit router/test/integration`
Expected: every test passes; the count is the count on `master` plus the tests this plan adds.

Run: `shellcheck -s bash router/opt/vpn-director/lib/iftunnel.sh` - no output.
Run: `sh -n router/jffs/scripts/nat-start` - no output.
Run: `grep -rnE '(^|[^A-Za-z_])(test|match|capture|scan|splits)\(' router/opt/vpn-director/lib/iftunnel.sh` - no output.

- [ ] **Step 2: Go and web**

Run (from `server/`): `go vet ./...`, `go test ./... -count=1`, `gofmt -l .`
Expected: clean, PASS, no output.

Run: `cd web && npm ci && npm test && npm run build`
Expected: pass.

- [ ] **Step 3: The branch holds what it should**

Run: `git status --short` - only the untracked files the Global Constraints name, and `.claude/settings.local.json` modified.
Run: `git log --oneline master..HEAD` - the spec, the plan and one commit per task.
Run: `git diff master --stat -- router/opt/vpn-director/lib/platform.sh router/opt/vpn-director/lib/platform/` - empty: the platform contract did not change.

---

### Task 12: Acceptance on the RT-AX86U (owner-gated)

Not for a subagent. Each step runs only after the owner's explicit yes for that step, and only after the owner's own check of 2026-10-16 (`awg1` comes back by itself after a reboot and holds for a day). Read-only commands on the router need no yes (`ssh -p 2222 admin@192.168.1.1`); never read `/opt/etc/amneziawg/awg1.conf`, never run `awg showconf`. home2 (192.168.1.3) runs the agent and reaches the Claude API through the tunnel: a step that moves it or stops its tunnel is announced first.

- [ ] **Step 1: Install the branch and apply.** `ip rule` shows a `prohibit` rule at 16639 beside the lookup rule at 16384; `vpn-director.sh status tunnel` reads clean; the clients of `ovpnc2` work as before.
- [ ] **Step 2: The kill switch on `ovpnc2`.** With the notebook (192.168.1.30) on `ovpnc2`, `service stop_vpnclient2`: the notebook's traffic to the tunnel is refused, not sent out through the WAN (`curl -4 ifconfig.me` from the notebook fails rather than showing the home address); excluded destinations still work. `service start_vpnclient2`: back. home2 loses its exit for that time as well - announce it.
- [ ] **Step 3: `awg1` as a tunnel.** Add `"interfaces": ["awg1"]` to `tunnel_director` in `/opt/vpn-director/vpn-director.json`, apply, move the notebook onto `awg1` in the Web UI. Its external address is yop's; `status tunnel` shows table `2000+idx`, the route and the three rules; `cru l` shows `vpn_director_routes`; `/tmp/tunnel_director/iftunnels` names `awg1`.
- [ ] **Step 4: Re-creation.** `/opt/etc/init.d/S51awg1 restart`: the notebook is refused during the restart, and within a minute `ensure-routes` logs `Interface tunnel back without its route: awg1` and the route is back.
- [ ] **Step 5: Firewall restart.** `service restart_firewall`: the marks, `MASQUERADE` and both MSS rules are back.
- [ ] **Step 6: nat rebuild.** A rebuild of `nat` by the firmware (the owner picks the trigger): `/tmp/syslog.log` shows the `nat-start` apply, and `MASQUERADE -o awg1` is back.
- [ ] **Step 7: The overlay channels.** Add yop's address to the `exclude` of `awg1` and apply: the router's conntrack shows the notebook's flow to yop:8443 through `eth0`, and the notebook's overlay channel to yop is alive.
- [ ] **Step 8: home2.** Move home2 onto `awg1` - announced; its overlay channel to yop is alive. The owner then decides what happens to `ovpnc2`.
- [ ] **Step 9: Hand over.** Tell the owner the "To do in wireguard-network" items of the spec (section 12): the background `vpn-director.sh --unless-stopped --wait apply tunnel` call in `S51awg1` after a successful `do_start`, with FD 9 closed; `docs/nodes/asus.md`; the two backlog items.

Before the PR (the owner's workflow): `git rm` every file under `docs/superpowers/` that the branch added (the spec and this plan) and commit, so that neither appears in the PR diff; they stay in the branch history.
