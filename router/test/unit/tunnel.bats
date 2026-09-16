#!/usr/bin/env bats

# The gateway test below passes a flag to `run`, which bats only guarantees
# from 1.5.0; without this the suite prints a BW02 warning for it.
bats_require_minimum_version 1.5.0

load '../test_helper'

# Note: load_tunnel_module is provided by test_helper.bash
# It loads: common.sh, config.sh, ipset.sh, firewall.sh, tunnel.sh

# ============================================================================
# _tunnel_table_allowed - validate routing tables
# ============================================================================

@test "_tunnel_table_allowed: accepts wgc1 as valid table" {
    load_tunnel_module
    _tunnel_init
    run _tunnel_table_allowed "wgc1"
    assert_success
}

@test "_tunnel_table_allowed: accepts ovpnc1 as valid table" {
    load_tunnel_module
    _tunnel_init
    run _tunnel_table_allowed "ovpnc1"
    assert_success
}

@test "_tunnel_table_allowed: accepts main as valid table" {
    load_tunnel_module
    _tunnel_init
    run _tunnel_table_allowed "main"
    assert_success
}

@test "_tunnel_table_allowed: rejects unknown table" {
    load_tunnel_module
    _tunnel_init
    run _tunnel_table_allowed "invalid_table"
    assert_failure
}

@test "_tunnel_table_allowed: rejects empty table" {
    load_tunnel_module
    _tunnel_init
    run _tunnel_table_allowed ""
    assert_failure
}

# ============================================================================
# tunnel_get_required_ipsets - parse tunnels JSON and return required ipsets
# ============================================================================

@test "tunnel_get_required_ipsets: returns exclude sets from config" {
    load_tunnel_module
    result=$(tunnel_get_required_ipsets)
    # From fixture: wgc1 has exclude: ["ru"]
    echo "$result" | grep -q "ru"
}

@test "tunnel_get_required_ipsets: handles empty tunnels gracefully" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export TUN_DIR_TUNNELS_JSON='{}'
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    result=$(tunnel_get_required_ipsets)
    [ -z "$result" ]
}

# ============================================================================
# tunnel_status - display status information
# ============================================================================

@test "tunnel_status: outputs status header" {
    load_tunnel_module
    run tunnel_status
    assert_success
    assert_output --partial "Tunnel Director Status"
}

@test "tunnel_status: shows chain section with TUN_DIR name" {
    load_tunnel_module
    run tunnel_status
    assert_success
    assert_output --partial "Chain: TUN_DIR"
}

@test "tunnel_status: shows ip rules section" {
    load_tunnel_module
    run tunnel_status
    assert_success
    assert_output --partial "IP Rules"
}

@test "tunnel_status: shows configured tunnels section" {
    load_tunnel_module
    run tunnel_status
    assert_success
    assert_output --partial "Configured Tunnels"
}

# ============================================================================
# PREROUTING insert position comes from the platform
# ============================================================================

@test "tunnel_apply: inserts the PREROUTING jump at platform_prerouting_base_pos" {
    load_tunnel_module
    platform_prerouting_base_pos() { printf '4\n'; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    grep -q -- '-t mangle -I PREROUTING 4 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR' /tmp/bats_iptables_calls.log
}

# ============================================================================
# _tunnel_init - initialization function
# ============================================================================

@test "_tunnel_init: sets valid_tables variable" {
    load_tunnel_module
    _tunnel_init
    [ -n "$_tunnel_valid_tables" ]
}

@test "_tunnel_init: includes main in valid tables" {
    load_tunnel_module
    _tunnel_init
    [[ " $_tunnel_valid_tables " == *" main "* ]]
}

@test "_tunnel_init: sets mark mask value" {
    load_tunnel_module
    _tunnel_init
    [ -n "$_tunnel_mark_mask_val" ]
}

@test "_tunnel_init: sets mark shift value" {
    load_tunnel_module
    _tunnel_init
    [ -n "$_tunnel_mark_shift_val" ]
}

@test "_tunnel_init: computes mark field max" {
    load_tunnel_module
    _tunnel_init
    # With mask 0x00ff0000 and shift 16, max should be 255
    [ "$_tunnel_mark_field_max" -eq 255 ]
}

@test "_tunnel_init: sets mark mask hex" {
    load_tunnel_module
    _tunnel_init
    [ "$_tunnel_mark_mask_hex" = "0xff0000" ]
}

# ============================================================================
# tunnel_stop - remove single TUN_DIR chain and ip rules
# ============================================================================

@test "tunnel_stop: returns success when chain does not exist" {
    load_tunnel_module
    run tunnel_stop
    assert_success
}

@test "tunnel_stop: removes single TUN_DIR chain" {
    load_tunnel_module
    run tunnel_stop
    assert_success
    assert_output --partial "Stopping Tunnel Director"
    assert_output --partial "Tunnel Director stopped"
}

# ============================================================================
# tunnel_apply - apply rules from config (idempotent)
# ============================================================================

@test "tunnel_apply: returns success" {
    load_tunnel_module
    run tunnel_apply
    assert_success
}

@test "tunnel_apply: creates single TUN_DIR chain" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    # Check that it references TUN_DIR chain and tunnel name in log
    assert_output --partial "wgc1"
}

@test "tunnel_apply: logs client routing info" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    # Should mention the client from fixture (192.168.50.0/24)
    assert_output --partial "192.168.50.0/24"
}

@test "tunnel_apply: PREROUTING jump includes -i br0 interface" {
    load_tunnel_module
    # Clear iptables log
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    # Verify PREROUTING rule includes -i br0 (from mock log)
    grep -q -- '-i br0.*-j TUN_DIR' /tmp/bats_iptables_calls.log
}

# ============================================================================
# Module loading
# ============================================================================

@test "tunnel.sh: can be sourced with --source-only" {
    load_common
    load_config
    source "$LIB_DIR/ipset.sh" --source-only
    run source "$LIB_DIR/tunnel.sh" --source-only
    # Note: 'run source' doesn't work well, use direct sourcing
    source "$LIB_DIR/tunnel.sh" --source-only
    # If we get here without error, the test passes
    [ $? -eq 0 ]
}

# ============================================================================
# Edge cases - Invalid JSON structure
# ============================================================================

@test "tunnel_apply: handles string instead of object in tunnels (invalid structure)" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director-invalid-string.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    # Should not crash
    assert_success
    # Should log warning about invalid tunnel config structure
    assert_output --partial "WARN"
    assert_output --partial "wgc1"
    assert_output --partial "invalid"
}

@test "tunnel_apply: handles clients as string instead of array" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director-clients-string.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    # Should not crash
    assert_success
    # Should log warning about clients being wrong type
    assert_output --partial "WARN"
    assert_output --partial "wgc1"
    assert_output --partial "clients"
    # Should skip this tunnel (no MARK rules created)
    refute_output --partial "Added:"
}

@test "tunnel_apply: handles exclude as string instead of array" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director-exclude-string.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    # Should not crash
    assert_success
    # Should log warning about exclude being wrong type
    assert_output --partial "WARN"
    assert_output --partial "exclude"
    # Should still create MARK rule for valid clients (exclusions skipped)
    assert_output --partial "Added:"
    assert_output --partial "192.168.50.0/24"
}

@test "tunnel_apply: failover snapshot clients are marked before overlapping tunnels" {
    load_common
    source "$LIB_DIR/firewall.sh"
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-failover-clients.json"
    jq '.tunnel_director.tunnels = {
            "main": {"clients":["192.168.1.20"],"exclude":[]},
            "ovpnc2": {"clients":["192.168.1.0/24","192.168.1.8"],"exclude":[]}
        } |
        .xray.failover = {"tunnel":"ovpnc2","clients":["192.168.1.8"]}' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    assert_success

    local snap main_host cidr
    snap=$(printf '%s\n' "$output" | grep -n 'Added: client=192.168.1.8 tunnel=ovpnc2' | head -1 | cut -d: -f1)
    main_host=$(printf '%s\n' "$output" | grep -n 'Added: client=192.168.1.20 tunnel=main' | head -1 | cut -d: -f1)
    cidr=$(printf '%s\n' "$output" | grep -n 'Added: client=192.168.1.0/24 tunnel=ovpnc2' | head -1 | cut -d: -f1)
    [[ -n $snap && -n $main_host && -n $cidr ]]
    (( snap < main_host ))
    (( main_host < cidr ))
}

@test "tunnel_apply: handles overlapping clients in different tunnels (first-match wins)" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director-overlapping.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    # Clear iptables log
    : > /tmp/bats_iptables_calls.log

    run tunnel_apply
    assert_success

    # Both tunnels should be configured
    assert_output --partial "wgc1"
    assert_output --partial "ovpnc1"

    # Both clients should have MARK rules (first-match-wins via fwmark condition)
    assert_output --partial "192.168.50.0/24"
    assert_output --partial "192.168.50.100"
}

@test "tunnel_apply: handles non-existent exclude ipset (warns but creates MARK rule)" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director-nonexistent-ipset.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    assert_success

    # Should warn about non-existent ipset
    assert_output --partial "WARN"
    assert_output --partial "xx"
    assert_output --partial "not found"

    # But should still create the MARK rule for the client
    assert_output --partial "192.168.50.0/24"
    assert_output --partial "mark="
}

# ============================================================================
# Tunnel tables and routes through the platform contract
# ============================================================================

@test "_tunnel_init: valid tables come from platform_tunnels" {
    load_tunnel_module
    platform_tunnels() { printf '%s\n' OpenVPN0 Wireguard0 main; }
    _tunnel_init
    [ "$_tunnel_valid_tables" = "OpenVPN0 Wireguard0 main" ]
}

@test "tunnel_apply: looks the ip rule up in the platform's table" {
    load_tunnel_module
    platform_tunnel_table() { printf '2000\n'; }
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    grep -q 'ip rule add pref 16384 fwmark 0x10000/0xff0000 lookup 2000' /tmp/bats_ip_calls.log
}

@test "tunnel_apply: records applied tunnels as '<idx> <id>' in TUN_DIR_TABLES" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    [ -f "$TUN_DIR_TABLES" ]
    run cat "$TUN_DIR_TABLES"
    assert_output "0 wgc1"
}

@test "tunnel_apply: ensures the route and still installs the ip rule when the route fails" {
    load_tunnel_module
    platform_tunnel_route_ensure() { echo "ensure $1 $2" >> "$BATS_TEST_TMPDIR/ensure.log"; return 1; }
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "route not installed"
    grep -q "ensure wgc1 0" "$BATS_TEST_TMPDIR/ensure.log"
    grep -q 'ip rule add pref 16384 fwmark 0x10000/0xff0000 lookup wgc1' /tmp/bats_ip_calls.log
}

@test "tunnel_apply: up-to-date path re-ensures every recorded route" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    platform_tunnel_route_ensure() { echo "ensure $1 $2" >> "$BATS_TEST_TMPDIR/ensure.log"; return 0; }
    # The chain exists as far as the mock is concerned once the hash matches
    fw_chain_exists() { return 0; }
    run tunnel_apply
    assert_success
    assert_output --partial "up-to-date"
    grep -q "ensure wgc1 0" "$BATS_TEST_TMPDIR/ensure.log"
}

@test "tunnel_apply: a missing TUN_DIR_TABLES forces a rebuild even when the hash matches" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    rm -f "$TUN_DIR_TABLES"
    fw_chain_exists() { return 0; }
    run tunnel_apply
    assert_success
    refute_output --partial "up-to-date"
    [ -f "$TUN_DIR_TABLES" ]
}

# config.sh makes TUN_DIR_TUNNELS_JSON read-only, so a test that needs a tunnel
# list of its own sources the modules without it - the same way the teardown test
# further down does.
load_tunnel_module_with() {
    load_common
    source "$LIB_DIR/firewall.sh"
    export TUN_DIR_CHAIN=TUN_DIR
    export TUN_DIR_PREF_BASE=16384
    export TUN_DIR_MARK_MASK=0x00ff0000
    export TUN_DIR_MARK_SHIFT=16
    export TUN_DIR_TUNNELS_JSON="$1"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only
}

# A tunnel the platform does not list is not a configuration state: on Keenetic
# platform_tunnels answers only "main" while RCI does not reply, and the
# netfilter.d hook fires exactly during an NDM rebuild. Recording the hash there
# would send every later apply down the up-to-date branch and leave the routing
# torn down until the next rebuild - a silent fail-open. The fixture rt_tables
# lists wgc1, wgc2, ovpnc1 and ovpnc2, so wgc9 is unknown here.
@test "tunnel_apply: does not record a config whose only tunnel is unknown to the platform" {
    load_tunnel_module_with '{"wgc9":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    assert_output --partial "not a tunnel this platform knows"
    assert_output --partial "not recorded as up-to-date"
    [ ! -f "$TUN_DIR_HASH" ]
}

@test "tunnel_apply: retries an unknown tunnel on the next apply instead of reporting up-to-date" {
    load_tunnel_module_with '{"wgc9":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    # The chain exists as far as the mock is concerned; only a recorded hash
    # could take the second apply down the up-to-date branch.
    fw_chain_exists() { return 0; }
    run tunnel_apply
    assert_success
    # Not the bare "up-to-date": the warning above carries that phrase itself.
    refute_output --partial "Rules are applied and up-to-date"
    assert_output --partial "not a tunnel this platform knows"
}

@test "tunnel_apply: applies the known tunnel and still does not record the hash when another is unknown" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"wgc9":{"clients":["192.168.1.6"]}}'
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    grep -q 'ip rule add pref 16384 fwmark 0x10000/0xff0000 lookup wgc1' /tmp/bats_ip_calls.log
    run cat "$TUN_DIR_TABLES"
    assert_output "0 wgc1"
    [ ! -f "$TUN_DIR_HASH" ]
}

# Recording the hash is skipped when a tunnel is unknown to the platform, but
# TUN_DIR_TABLES is written all the same. The cleanup gate keyed off the hash
# alone, so the next apply skipped tunnel_stop and never released the recorded
# tables: on Keenetic table 2000+idx keeps the previous tunnel's route while the
# index is handed to another tunnel, and marked traffic leaves through the wrong
# tunnel while the log reports the fallback to main.
@test "tunnel_apply: releases the recorded tables before reusing their indices when no hash was recorded" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"wgc9":{"clients":["192.168.1.6"]}}'
    run tunnel_apply
    assert_success
    [ ! -f "$TUN_DIR_HASH" ]
    run cat "$TUN_DIR_TABLES"
    assert_output "0 wgc1"

    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/release.log"; }
    TUN_DIR_TUNNELS_JSON='{"wgc2":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    grep -q "release wgc1 0" "$BATS_TEST_TMPDIR/release.log"
}

@test "tunnel_stop: releases every recorded table and removes the state file" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/release.log"; }
    run tunnel_stop
    assert_success
    grep -q "release wgc1 0" "$BATS_TEST_TMPDIR/release.log"
    [ ! -e "$TUN_DIR_TABLES" ]
}

# An apply of {} (or empty) used to log "No tunnels configured" and return 0,
# leaving the chain, the PREROUTING jump, ip rule 16384 and the hash in place.
# Only `stop tunnel` removed them. Wizards write {} when the last TD client
# is gone; a leftover MARK still forces that client into the tunnel.
@test "tunnel_apply: empty tunnels tears down previously applied Tunnel Director" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export TUN_DIR_CHAIN=TUN_DIR
    export TUN_DIR_PREF_BASE=16384
    export TUN_DIR_MARK_MASK=0x00ff0000
    export TUN_DIR_MARK_SHIFT=16
    export TUN_DIR_TUNNELS_JSON='{"wgc1":{"clients":["192.168.50.0/24"],"exclude":["ru"]}}'
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tunnel.sh" --source-only

    run tunnel_apply
    assert_success
    [ -f "$TUN_DIR_HASH" ]
    [ -f "$TUN_DIR_TABLES" ]

    : > /tmp/bats_iptables_calls.log
    : > /tmp/bats_ip_calls.log
    TUN_DIR_TUNNELS_JSON='{}'
    run tunnel_apply
    assert_success
    [ ! -e "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_TABLES" ]
    grep -q -- '-t mangle -X TUN_DIR' /tmp/bats_iptables_calls.log
    grep -q -- '-t mangle -S PREROUTING' /tmp/bats_iptables_calls.log
    grep -q 'ip rule del pref 16384' /tmp/bats_ip_calls.log
}

# Every Xray-only install applies with tunnels = {} (the template's default) on
# every hook and cron run. Unconditional, the teardown above cost 255
# `ip rule del` spawns, a PREROUTING purge and two syslog lines each time, with
# nothing to tear down.
@test "tunnel_apply: empty tunnels with no prior state runs no teardown" {
    load_tunnel_module_with '{}'
    fw_chain_exists() { return 1; }
    : > /tmp/bats_ip_calls.log
    run tunnel_apply
    assert_success
    assert_output --partial "No tunnels configured"
    refute_output --partial "Stopping Tunnel Director"
    refute grep -q 'ip rule del' /tmp/bats_ip_calls.log
}

# The state files live in /tmp; a chain that outlived them is still torn down.
@test "tunnel_apply: empty tunnels still tears down a chain left without state files" {
    load_tunnel_module_with '{}'
    fw_chain_exists() { return 0; }
    rm -f "$TUN_DIR_HASH" "$TUN_DIR_TABLES"
    run tunnel_apply
    assert_success
    assert_output --partial "Stopping Tunnel Director"
}

# An apply that died after ensuring a route but before recording TUN_DIR_TABLES
# (NDM wiping the chain mid-apply, a busy xtables lock, a signal) leaves the
# route in its table with no record, so the cleanup at the top of the rebuild
# never releases it. The next apply hands that index to whichever tunnel comes
# first now; if that tunnel's route cannot be installed (interface down), the
# ip rule installed for it resolves to the previous owner's route - the wrong
# tunnel, while the log reports the fallback to main.
@test "tunnel_apply: releases a tunnel's table before ensuring its route" {
    load_tunnel_module
    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/calls.log"; }
    platform_tunnel_route_ensure() { echo "ensure $1 $2" >> "$BATS_TEST_TMPDIR/calls.log"; return 1; }
    run tunnel_apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls.log"
    assert_output $'release wgc1 0\nensure wgc1 0'
}

# XRAY_TPROXY inserts from position 1 and runs before this chain by design. On
# Merlin without firmware iface-mark rules the platform's base position is 1 as
# well, so a rebuild put TUN_DIR ahead of the Xray jump and the next apply
# found that jump off its position and purged and re-inserted it - a window
# with no TPROXY jump on every apply.
@test "tunnel_apply: the jump goes behind the XRAY_TPROXY jumps already in PREROUTING" {
    load_tunnel_module
    iptables() {
        if [[ "$*" == "-t mangle -S PREROUTING" ]]; then
            echo "iptables $*" >> /tmp/bats_iptables_calls.log
            printf -- '-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n'
            return 0
        fi
        command iptables "$@"
    }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    grep -q -- '-I PREROUTING 2 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR' /tmp/bats_iptables_calls.log
}

# The jump is the whole point of the chain. A platform that cannot name its LAN
# interfaces used to run the loop zero times, leaving a fully populated chain
# with nothing jumping to it and tunnel_apply returning 0 - every client routed
# direct instead of through its tunnel. Unreachable on Merlin, where
# platform_lan_ifaces is a constant.
@test "tunnel_apply: fails and touches no firewall state when the platform names no LAN interface" {
    load_tunnel_module
    platform_lan_ifaces() { return 1; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_failure
    assert_output --partial "Cannot determine the LAN interfaces"
    refute grep -q -- "-j TUN_DIR" /tmp/bats_iptables_calls.log
    refute grep -q -- "-N TUN_DIR" /tmp/bats_iptables_calls.log
}

# An empty answer with rc 0 is the same failure: zero jumps installed.
@test "tunnel_apply: fails when the platform prints no LAN interface" {
    load_tunnel_module
    platform_lan_ifaces() { return 0; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_failure
    assert_output --partial "Cannot determine the LAN interfaces"
    refute grep -q -- "-j TUN_DIR" /tmp/bats_iptables_calls.log
}

# Unguarded, an rc 1 from platform_prerouting_base_pos tripped errexit and ended
# the whole CLI run with no log line at all - and tproxy_apply, which runs after
# tunnel_apply, never ran either.
@test "tunnel_apply: fails with an ERROR when the platform has no PREROUTING position" {
    load_tunnel_module
    platform_prerouting_base_pos() { return 1; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_failure
    assert_output --partial "Cannot determine the PREROUTING insert position"
    refute grep -q -- "-j TUN_DIR" /tmp/bats_iptables_calls.log
    # No hash written, so the next apply rebuilds instead of reporting up-to-date.
    [ ! -e "$TUN_DIR_HASH" ]
}

@test "tunnel_apply: one PREROUTING jump per platform LAN interface" {
    load_tunnel_module
    platform_prerouting_base_pos() { printf '4\n'; }
    platform_lan_ifaces() { printf 'br0\nbr1\n'; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    # Each interface asks for its own position (base_pos, base_pos + 1, ...), so
    # no jump displaces another and the next apply finds both already in place
    # instead of purging and re-inserting every one of them.
    grep -q -- '-I PREROUTING 4 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR' /tmp/bats_iptables_calls.log
    grep -q -- '-I PREROUTING 5 -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR' /tmp/bats_iptables_calls.log
}

# ============================================================================
# The configured gateway reaches the platform's route (spec 12)
# ============================================================================

@test "_tunnel_gateway: prints the configured gateway of a tunnel, nothing without one" {
    load_common
    jq '.tunnel_director.tunnels.wgc1.gateway = "10.8.0.1"' "$TEST_ROOT/fixtures/vpn-director.json" \
        > "$BATS_TEST_TMPDIR/vpn-director.json"
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tunnel.sh" --source-only
    run _tunnel_gateway wgc1
    assert_success
    assert_output "10.8.0.1"
    run _tunnel_gateway ovpnc1
    assert_success
    refute_output
}

@test "_tunnel_gateway: drops a value that is not an IPv4 address with a WARN" {
    load_common
    source "$LIB_DIR/tunnel.sh" --source-only
    # Bypass config.sh: the load-time filter would strip this key before the
    # apply-time belt sees it. config.bats covers that path.
    TUN_DIR_TUNNELS_JSON='{"wgc1":{"gateway":"gateway.example"}}'
    run --separate-stderr _tunnel_gateway wgc1
    assert_success
    refute_output
    grep -q "WARN.*invalid gateway 'gateway.example'" "$LOG_FILE"
}

# The dotted shape matches, the value is still not an address: the octet check
# is the only thing between "10.8.0.999" and ip route.
@test "_tunnel_gateway: drops an address whose octet is above 255 with a WARN" {
    load_common
    source "$LIB_DIR/tunnel.sh" --source-only
    TUN_DIR_TUNNELS_JSON='{"wgc1":{"gateway":"10.8.0.999"}}'
    run --separate-stderr _tunnel_gateway wgc1
    assert_success
    refute_output
    grep -q "WARN.*invalid gateway '10.8.0.999'" "$LOG_FILE"
}

@test "tunnel_apply: hands the configured gateway to platform_tunnel_route_ensure" {
    load_common
    jq '.tunnel_director.tunnels.wgc1.gateway = "10.8.0.1"' "$TEST_ROOT/fixtures/vpn-director.json" \
        > "$BATS_TEST_TMPDIR/vpn-director.json"
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tunnel.sh" --source-only
    platform_tunnel_route_ensure() { echo "ensure $1 $2 [$3]" >> "$BATS_TEST_TMPDIR/ensure.log"; return 0; }
    run tunnel_apply
    assert_success
    grep -qF "ensure wgc1 0 [10.8.0.1]" "$BATS_TEST_TMPDIR/ensure.log"
    # The up-to-date path passes it too.
    fw_chain_exists() { return 0; }
    run tunnel_apply
    assert_success
    assert_equal "$(grep -cF 'ensure wgc1 0 [10.8.0.1]' "$BATS_TEST_TMPDIR/ensure.log")" 2
}

# ============================================================================
# The platform's fast-path opt-out
# ============================================================================

# The opt-out has to carry the same match as the MARK rule and sit immediately
# before it. Above it, the exclusion RETURNs fire first, so an excluded
# destination keeps its acceleration; below it, `--mark 0x0/0xff0000` would no
# longer match a packet MARK had just marked, and the rule would never fire.
@test "tunnel_apply: the platform's offload target goes immediately before MARK, same match" {
    load_tunnel_module
    platform_tunnel_offload_target() { printf 'PPE\n'; }
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    run grep -- '-A TUN_DIR' /tmp/bats_iptables_calls.log
    assert_line --index 0 'iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN'
    assert_line --index 1 'iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j PPE'
    assert_line --index 2 'iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
}

# Merlin names no target (platform_tunnel_offload_target returns 1), and the
# chain must then look exactly as it did before this feature existed - a rule
# naming a target the kernel does not have would fail the append and take the
# whole apply down with it.
@test "tunnel_apply: no offload rule when the platform names no target" {
    load_tunnel_module
    : > /tmp/bats_iptables_calls.log
    run tunnel_apply
    assert_success
    run grep -- '-A TUN_DIR' /tmp/bats_iptables_calls.log
    assert_line --index 0 'iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN'
    assert_line --index 1 'iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
    assert_equal "${#lines[@]}" 2
}
