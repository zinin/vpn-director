#!/usr/bin/env bats

load '../test_helper'

# load_iftunnel [interfaces] - common.sh, which sources platform.sh (Merlin: the
# fixture rt_tables lists wgc1 wgc2 ovpnc1 ovpnc2, the nvram mock makes eth0 the
# WAN) and iftunnel.sh, and firewall.sh; tunnel_director.interfaces as config.sh
# leaves it - space-separated, as written - and the state file in the test's own
# directory.
load_iftunnel() {
    load_firewall
    # common.sh points LOG_FILE at /tmp/vpn-director.log, which every run
    # appends to; a count or a refute has to read the test's own lines.
    LOG_FILE="$BATS_TEST_TMPDIR/vpn-director.log"
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
