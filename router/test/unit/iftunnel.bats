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

@test "iftunnel_init: a rejected name listed twice warns once" {
    load_iftunnel "br0 awg1 main br0 main"
    iftunnel_init
    run iftunnel_list
    assert_output "awg1"
    assert_equal "$(grep -c 'tunnel_director.interfaces: .*; ignored' "$LOG_FILE")" 2
    assert_equal "$(grep -c "^br0"$'\t' <<< "$_iftunnel_rejected")" 1
    assert_equal "$(grep -c "^main"$'\t' <<< "$_iftunnel_rejected")" 0
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

# The wizard has no log and shows these itself. main and wgc1 are the
# platform's, recorded as nothing, and the menu offers them anyway.
@test "iftunnel_rejections: the recorded reasons in list order, one for a name listed twice" {
    load_iftunnel "eth0 awg1 main bad/name wgc1 br0 eth0 br0"
    iftunnel_init
    run iftunnel_rejections
    assert_success
    assert_line --index 0 "'eth0' is the WAN interface"
    assert_line --index 1 "'bad/name' is no interface name (1-15 characters: letters, digits, _ . -)"
    assert_line --index 2 "'br0' is a LAN interface"
    [ "${#lines[@]}" -eq 3 ]
}

@test "iftunnel_rejections: nothing for main, a platform-listed id or a kept name" {
    load_iftunnel "awg1 main wgc1"
    iftunnel_init
    run iftunnel_rejections
    assert_success
    refute_output
}

@test "iftunnel_name_ok: 1-15 characters of letters, digits, _ . -, not . or .." {
    load_iftunnel ""
    local name
    for name in awg1 wg0.5 a-b_c A23456789012345; do
        run iftunnel_name_ok "$name"
        assert_success
    done
    for name in "" . .. a234567890123456 bad/name 'a*b' 'a b'; do
        run iftunnel_name_ok "$name"
        assert_failure
    done
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

# ip reads a word such as "up", "type" or "group" after "link show" as a keyword
# - "ip -o link show up" lists every interface that is UP - and the name check
# takes such a word. After "dev" it is a name.
@test "iftunnel_info: asks ip for the interface after dev, so a name such as 'up' is no keyword" {
    load_iftunnel "up"
    ip_links "up <POINTOPOINT,NOARP,UP,LOWER_UP> 1280"
    : > /tmp/bats_ip_calls.log
    run iftunnel_info up
    assert_success
    assert_output $'interface\n1'
    grep -qx 'ip -o link show dev up' /tmp/bats_ip_calls.log
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

# Review Focus 1, the other half: the replacement finds the old clamp by a
# pattern too, and "awg1" must not take "awg10"'s.
@test "iftunnel_route_ensure: a new MTU leaves another interface's inbound clamp alone, a longer name's included" {
    load_iftunnel "awg1 awg10"
    use_stateful_iptables
    ip_links "$AWG1_UP" "awg10 <POINTOPOINT,NOARP,UP,LOWER_UP> 1420"
    iftunnel_route_ensure awg1 1
    iftunnel_route_ensure awg10 2
    ip_links "awg1 <POINTOPOINT,NOARP,UP,LOWER_UP> 1300" "awg10 <POINTOPOINT,NOARP,UP,LOWER_UP> 1420"
    iftunnel_route_ensure awg1 1
    run iptables -t mangle -S FORWARD
    assert_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1260'
    refute_line '-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240'
    assert_line '-A FORWARD -i awg10 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1380'
}

# An empty name would put "-o ''" rules in at position 1, and an older libxtables
# reads an empty interface as any: a bare MASQUERADE at the head of nat
# POSTROUTING.
@test "iftunnel_route_ensure: an empty name puts no rule and no route in" {
    load_iftunnel "awg1"
    use_stateful_iptables
    ip_links "$AWG1_UP"
    : > /tmp/bats_iptables_calls.log
    : > /tmp/bats_ip_calls.log
    run iftunnel_route_ensure "" 1
    assert_failure
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ ! -s /tmp/bats_ip_calls.log ]
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

# The release of an empty name would delete "-o ''" rules, which an older
# libxtables reads as rules of no interface: someone else's bare MASQUERADE.
@test "iftunnel_table_release: an empty name removes nothing" {
    load_iftunnel ""
    use_stateful_iptables
    : > /tmp/bats_iptables_calls.log
    : > /tmp/bats_ip_calls.log
    run iftunnel_table_release "" 1
    assert_failure
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ ! -s /tmp/bats_ip_calls.log ]
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

# stub_platform_tunnels <rc> <id>... - a platform_tunnels that prints the ids
# and returns <rc>, counting its calls in a file: vpd_tunnels runs in
# subshells, where a counter in a variable would be lost.
stub_platform_tunnels() {
    stub_rc="$1"
    shift
    stub_ids=("$@")
    platform_tunnels() {
        echo call >> "$BATS_TEST_TMPDIR/platform_calls"
        printf '%s\n' "${stub_ids[@]}"
        return "$stub_rc"
    }
}

# platform_calls - how many times platform_tunnels was asked
platform_calls() {
    local n=0
    if [[ -f $BATS_TEST_TMPDIR/platform_calls ]]; then
        n=$(wc -l < "$BATS_TEST_TMPDIR/platform_calls")
    fi
    echo $((n))
}

# On KeeneticOS each platform_tunnels is an RCI request - 5 s while RCI does
# not answer - and two of them can read two states: the check of a non-empty
# list keeps its answer, and vpd_tunnels lists from it.
@test "vpd_tunnels: lists from the answer iftunnel_init kept, one platform_tunnels a run" {
    load_iftunnel "awg1"
    stub_platform_tunnels 0 wgc1 wgc2 main
    iftunnel_init
    run vpd_tunnels
    assert_success
    assert_output $'wgc1\nwgc2\nawg1\nmain'
    assert_equal "$(platform_calls)" 1
}

@test "vpd_tunnels: a failed inventory iftunnel_init kept still fails, main last" {
    load_iftunnel "awg1"
    stub_platform_tunnels 1 main
    iftunnel_init
    run vpd_tunnels
    assert_failure 1
    assert_output $'awg1\nmain'
    assert_equal "$(platform_calls)" 1
}

# An empty list asks the platform nothing in iftunnel_init.
@test "vpd_tunnels: with an empty list asks the platform itself, once" {
    load_iftunnel ""
    stub_platform_tunnels 0 wgc1 wgc2 main
    iftunnel_init
    run vpd_tunnels
    assert_success
    assert_output $'wgc1\nwgc2\nmain'
    assert_equal "$(platform_calls)" 1
}

@test "iftunnel_init: asks the platform again once the list changes, and vpd_tunnels lists that answer" {
    load_iftunnel "awg1"
    stub_platform_tunnels 0 wgc1 main
    iftunnel_init
    iftunnel_init
    assert_equal "$(platform_calls)" 1
    stub_platform_tunnels 0 wgc3 main
    TUN_DIR_INTERFACES="awg1 awg2"
    iftunnel_init
    assert_equal "$(platform_calls)" 2
    run vpd_tunnels
    assert_output $'wgc3\nawg1\nawg2\nmain'
    assert_equal "$(platform_calls)" 2
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

# A lookup rule of a slot the layer names no table for - an interface whose
# record is gone - can point only to the slot's own interface table.
@test "vpd_tunnel_slot_table: the layer's table, else the slot's own interface table" {
    load_iftunnel "awg1"
    iftunnel_init
    printf 'awg9 2005\n' > "$IFTUNNEL_STATE_FILE"
    run vpd_tunnel_slot_table awg1 3
    assert_output "2003"
    run vpd_tunnel_slot_table wgc1 3
    assert_output "wgc1"
    run vpd_tunnel_slot_table awg9 3
    assert_output "2005"
    run vpd_tunnel_slot_table awg7 3
    assert_success
    assert_output "2003"
    run vpd_tunnel_slot_table 'a*b' 3
    assert_output "2003"
}

# The interface's release goes by the name alone, so it needs the caller's sign
# that the slot was routed as an interface tunnel: without one, the name may be a
# firmware interface whose own rules read the same.
@test "vpd_tunnel_slot_release: the layer's release, else an interface's for a routed slot of a well-formed name, else nothing" {
    load_iftunnel ""
    iftunnel_init
    vpd_tunnel_table_release() { echo "layer $*"; return 1; }
    iftunnel_table_release() { echo "iftunnel $*"; return 1; }
    run vpd_tunnel_slot_release wgc1 0
    assert_success
    assert_output "layer wgc1 0"
    run vpd_tunnel_slot_release awg1 1 1
    assert_success
    assert_output "iftunnel awg1 1"
    run vpd_tunnel_slot_release awg1 1 0
    assert_success
    refute_output
    run vpd_tunnel_slot_release awg1 1
    assert_success
    refute_output
    run vpd_tunnel_slot_release 'a*b' 2 1
    assert_success
    refute_output
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

# main and a tunnel the platform lists stay what they are when the list names
# them by mistake: they warn, and the platform's answer stands - nothing for a
# tunnel it lists. The list's reason would make the warning about a firmware
# tunnel whose route merely failed blame the list.
@test "vpd_tunnel_unlisted_reason: the platform's answer for main and a tunnel it lists, though the list names them" {
    load_iftunnel "wgc1 main br0"
    iftunnel_init
    run vpd_tunnel_unlisted_reason wgc1
    assert_failure
    refute_output
    run vpd_tunnel_unlisted_reason main
    assert_failure
    refute_output
    grep -qF "tunnel_director.interfaces: 'wgc1' is a tunnel the firmware manages; ignored" "$LOG_FILE"
    grep -qF "tunnel_director.interfaces: 'main' is Tunnel Director's own route, not an interface; ignored" "$LOG_FILE"
    run vpd_tunnel_unlisted_reason br0
    assert_success
    assert_output "'br0' is a LAN interface"
}
