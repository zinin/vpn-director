#!/usr/bin/env bats
# test/firewall.bats - Tests for firewall.sh utility functions

load 'test_helper'

# ============================================================================
# validate_port
# ============================================================================

@test "validate_port: accepts valid port 80" {
    load_firewall
    run validate_port 80
    assert_success
}

@test "validate_port: accepts valid port 443" {
    load_firewall
    run validate_port 443
    assert_success
}

@test "validate_port: accepts valid port 65535" {
    load_firewall
    run validate_port 65535
    assert_success
}

@test "validate_port: rejects port 0" {
    load_firewall
    run validate_port 0
    assert_failure
}

@test "validate_port: rejects port 70000" {
    load_firewall
    run validate_port 70000
    assert_failure
}

@test "validate_port: rejects non-numeric" {
    load_firewall
    run validate_port "abc"
    assert_failure
}

@test "validate_port: rejects empty" {
    load_firewall
    run validate_port ""
    assert_failure
}

# ============================================================================
# validate_ports
# ============================================================================

@test "validate_ports: accepts 'any'" {
    load_firewall
    run validate_ports "any"
    assert_success
}

@test "validate_ports: accepts single port" {
    load_firewall
    run validate_ports "443"
    assert_success
}

@test "validate_ports: accepts port range" {
    load_firewall
    run validate_ports "1000-2000"
    assert_success
}

@test "validate_ports: accepts comma list" {
    load_firewall
    run validate_ports "80,443,8080"
    assert_success
}

@test "validate_ports: accepts mixed list with range" {
    load_firewall
    run validate_ports "80,443,1000-2000"
    assert_success
}

@test "validate_ports: rejects invalid range (start > end)" {
    load_firewall
    run validate_ports "2000-1000"
    assert_failure
}

# ============================================================================
# normalize_protos
# ============================================================================

@test "normalize_protos: returns tcp for tcp" {
    load_firewall
    run normalize_protos "tcp"
    assert_success
    assert_output "tcp"
}

@test "normalize_protos: returns udp for udp" {
    load_firewall
    run normalize_protos "udp"
    assert_success
    assert_output "udp"
}

@test "normalize_protos: returns tcp,udp for any" {
    load_firewall
    run normalize_protos "any"
    assert_success
    assert_output "tcp,udp"
}

@test "normalize_protos: normalizes udp,tcp to tcp,udp" {
    load_firewall
    run normalize_protos "udp,tcp"
    assert_success
    assert_output "tcp,udp"
}

# ============================================================================
# _spec_to_log
# ============================================================================

@test "_spec_to_log: empty input returns empty line" {
    load_firewall
    run _spec_to_log
    assert_success
    assert_output ""
}

@test "_spec_to_log: target only outputs arrow format without error" {
    load_firewall
    # This triggers the bug: empty left, only -j TARGET
    run _spec_to_log "-j ACCEPT"
    assert_success
    assert_output "-> ACCEPT"
}

@test "_spec_to_log: DNAT with only --to-destination outputs arrow format" {
    load_firewall
    run _spec_to_log "-j DNAT --to-destination 192.168.1.10:443"
    assert_success
    assert_output "-> 192.168.1.10:443"
}

@test "_spec_to_log: full spec with dest, proto, port and target" {
    load_firewall
    run _spec_to_log "-d 1.2.3.4 -p tcp --dport 443 -j ACCEPT"
    assert_success
    assert_output "dest=1.2.3.4 proto=tcp port=443 -> ACCEPT"
}

# ============================================================================
# block_wan_for_host / allow_wan_for_host take the WAN interface from the platform
# ============================================================================

@test "block_wan_for_host: blocks both directions on the platform WAN interface" {
    load_firewall
    platform_ipv6_enabled() { printf '0\n'; }
    : > /tmp/bats_iptables_calls.log
    run block_wan_for_host 192.168.50.10
    assert_success
    grep -q -- '-i eth0 -d 192.168.50.10 -j DROP' /tmp/bats_iptables_calls.log
    grep -q -- '-s 192.168.50.10 -o eth0 -j REJECT' /tmp/bats_iptables_calls.log
}

@test "block_wan_for_host: fails when the platform reports no WAN interface" {
    load_firewall
    platform_wan_if() { return 1; }
    run block_wan_for_host 192.168.50.10
    assert_failure
    assert_output --partial "WAN interface name is empty"
}

# ============================================================================
# sync_fw_rule
# ============================================================================

# The PREROUTING jumps of XRAY_TPROXY and TUN_DIR go in through here, and both
# callers publish a ready marker the subscription watch acts on. An insert the
# kernel refused - a busy xtables lock, say - used to come back as success, so
# the marker went out for a chain nothing jumped to.
@test "sync_fw_rule: fails when the rule cannot be inserted" {
    load_firewall
    iptables() {
        if [[ $* == *" -I PREROUTING "* ]]; then
            echo "iptables $*" >> /tmp/bats_iptables_calls.log
            return 4
        fi
        command iptables "$@"
    }
    run sync_fw_rule -q mangle PREROUTING "-i br0 -j XRAY_TPROXY\$" "-i br0 -j XRAY_TPROXY" 1
    assert_failure
}

@test "sync_fw_rule: succeeds without a change when the rule is already in place" {
    load_firewall
    iptables() {
        echo "iptables $*" >> /tmp/bats_iptables_calls.log
        if [[ $* == "-t mangle -S PREROUTING" ]]; then
            printf -- '-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n'
            return 0
        fi
        command iptables "$@"
    }
    : > /tmp/bats_iptables_calls.log
    run sync_fw_rule -q mangle PREROUTING "-i br0 -j XRAY_TPROXY\$" "-i br0 -j XRAY_TPROXY" 1
    assert_success
    refute grep -q -- '-I PREROUTING' /tmp/bats_iptables_calls.log
    refute grep -q -- '-D PREROUTING' /tmp/bats_iptables_calls.log
}

@test "allow_wan_for_host: looks the rules up on the platform WAN interface" {
    load_firewall
    platform_ipv6_enabled() { printf '0\n'; }
    : > /tmp/bats_iptables_calls.log
    run allow_wan_for_host 192.168.50.10
    assert_success
    # The iptables mock answers -C with "absent", so ensure_fw_rule -D stops at
    # the check; the check itself must already name the platform's WAN interface.
    grep -q -- '-C FORWARD -i eth0 -d 192.168.50.10 -j DROP' /tmp/bats_iptables_calls.log
}

# ============================================================================
# swap_fw_chain - rebuild a chain beside the live one, then move the jumps
# ============================================================================

# The callbacks the tests hand swap_fw_chain.
build_new() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; }
build_refused() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; return 2; }
build_incomplete() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; return 1; }
pos_one() { printf '1\n'; }
pos_two() { printf '2\n'; }

# live_chain <chain> <jump_match...> - a chain holding one old rule and a jump
# to it for every match, the way an earlier apply left them.
live_chain() {
    local chain="$1" match
    shift
    iptables -t mangle -N "$chain"
    iptables -t mangle -A "$chain" -d 10.0.0.0/8 -j RETURN
    for match in "$@"; do
        # shellcheck disable=SC2086
        iptables -t mangle -A PREROUTING $match -j "$chain"
    done
}

# Flushing the live chain and refilling it one rule per call left every packet
# that crossed it meanwhile unrouted - out through the WAN, on every apply.
@test "swap_fw_chain: builds beside the live chain and moves its jump over" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    : > /tmp/bats_iptables_calls.log

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
    # The new jump went in before the old one went, and the old chain went last.
    run grep -E -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW$|-D PREROUTING -i br0 -j XRAY_TPROXY$|-X XRAY_TPROXY$|-E XRAY_TPROXY_NEW XRAY_TPROXY$' \
        /tmp/bats_iptables_calls.log
    assert_line --index 0 --partial '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW'
    assert_line --index 1 --partial '-D PREROUTING -i br0 -j XRAY_TPROXY'
    assert_line --index 2 --partial '-X XRAY_TPROXY'
    assert_line --index 3 --partial '-E XRAY_TPROXY_NEW XRAY_TPROXY'
}

@test "swap_fw_chain: a chain nothing jumps to yet gets its jump where pos_fn says" {
    load_firewall
    use_stateful_iptables
    iptables -t mangle -A PREROUTING -p icmp -j ACCEPT
    iptables -t mangle -A PREROUTING -p igmp -j ACCEPT

    run swap_fw_chain mangle TUN_DIR build_new pos_two "-i br0 -m mark --mark 0x0/0xff0000"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output "$(printf '%s\n' '-P PREROUTING ACCEPT' '-A PREROUTING -p icmp -j ACCEPT' \
        '-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR' '-A PREROUTING -p igmp -j ACCEPT')"
}

# Merlin's firewall start empties every mangle chain, deletes none, and takes
# the jumps with it. The next apply swaps a full chain in over the empty one.
@test "swap_fw_chain: after a whole-table flush the chain comes back with its jump" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -F

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A rule that bounds what the chain takes did not go in: the chain that works
# stays, rather than one that would take too much.
@test "swap_fw_chain: a build that refuses leaves the live chain and its jump alone" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"

    run swap_fw_chain mangle XRAY_TPROXY build_refused pos_one "-i br0"
    assert_failure 2

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 10.0.0.0/8 -j RETURN'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
}

@test "swap_fw_chain: a build that misses a rule still goes in, and says so" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"

    run swap_fw_chain mangle XRAY_TPROXY build_incomplete pos_one "-i br0"
    assert_failure 1

    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
}

# An interface whose new jump the kernel refused keeps the old chain, and
# nothing is deleted under it. The next swap finishes the cutover first.
@test "swap_fw_chain: a jump that does not go in keeps that interface on the old chain" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0" "-i br1"
    iptables() {
        [[ $* == *"-I PREROUTING "*"-i br1 -j XRAY_TPROXY_NEW" ]] && return 1
        command iptables "$@"
    }

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0" "-i br1"
    assert_failure 3
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY_NEW\n-A PREROUTING -i br1 -j XRAY_TPROXY'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]

    unset -f iptables
    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0" "-i br1"
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br1 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A refused jump used to take a position all the same: the next interface's jump
# went one lower - behind whatever held the position pos_fn names, or past the
# end of a short PREROUTING.
@test "swap_fw_chain: a refused jump leaves its position to the next interface" {
    load_firewall
    use_stateful_iptables
    iptables -t mangle -A PREROUTING -p icmp -j ACCEPT
    iptables -t mangle -A PREROUTING -p igmp -j ACCEPT
    iptables() {
        [[ $* == *"-I PREROUTING "*"-i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW" ]] && return 1
        command iptables "$@"
    }

    run swap_fw_chain mangle TUN_DIR build_new pos_two \
        "-i br0 -m mark --mark 0x0/0xff0000" "-i br1 -m mark --mark 0x0/0xff0000"
    assert_failure 3
    unset -f iptables

    run iptables -t mangle -S PREROUTING
    assert_output "$(printf '%s\n' '-P PREROUTING ACCEPT' '-A PREROUTING -p icmp -j ACCEPT' \
        '-A PREROUTING -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' '-A PREROUTING -p igmp -j ACCEPT')"
}

# A swap that stopped after its new jump went in left two chains, the new one
# complete: the jumps go in only after the build.
@test "swap_fw_chain: finishes a swap that stopped after its jump went in" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN
    iptables -t mangle -I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success
    assert_output --partial "Finishing an interrupted swap of XRAY_TPROXY"

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A shadow nothing jumps to is what a build that died left, perhaps half-built:
# it is deleted, never swapped in. Finished as an interrupted swap, it would take
# the jump and carry the traffic until the fresh chain replaced it.
@test "swap_fw_chain: deletes a shadow chain nothing jumps to" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN
    : > /tmp/bats_iptables_calls.log

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success
    refute_output --partial "Finishing an interrupted swap"
    # One jump to XRAY_TPROXY_NEW went in: the fresh chain's.
    run grep -c -- '-I PREROUTING .*-j XRAY_TPROXY_NEW$' /tmp/bats_iptables_calls.log
    assert_output 1
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
}

# A swap that stopped half-way leaves XRAY_TPROXY_NEW live. A PREROUTING listing
# that failed - a busy xtables lock, say - read as "nothing jumps to it", and the
# delete that followed began with a flush of the chain under that jump.
@test "swap_fw_chain: a PREROUTING listing that fails leaves a live shadow alone" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br1"
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN
    iptables -t mangle -I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW
    iptables() {
        [[ $* == "-t mangle -S PREROUTING" ]] && return 4
        command iptables "$@"
    }

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0" "-i br1"
    assert_failure 2
    unset -f iptables

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY_NEW\n-A PREROUTING -i br1 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_output $'-N XRAY_TPROXY_NEW\n-A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 10.0.0.0/8 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# After a rename that failed, XRAY_TPROXY_NEW carries every interface. An
# existence check that failed once skipped the look at it, and the create that
# followed ("create_fw_chain -f") flushed it for the whole build.
@test "swap_fw_chain: an existence check that fails never flushes a live shadow" {
    load_firewall
    use_stateful_iptables
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN
    iptables -t mangle -A PREROUTING -i br0 -j XRAY_TPROXY_NEW
    # Only the first listing of the shadow fails, the way one call fails on a busy lock.
    iptables() {
        if [[ $* == "-t mangle -S XRAY_TPROXY_NEW" && ! -e $BATS_TEST_TMPDIR/listing_failed ]]; then
            : > "$BATS_TEST_TMPDIR/listing_failed"
            return 4
        fi
        command iptables "$@"
    }

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_failure 2
    unset -f iptables
    [ -e "$BATS_TEST_TMPDIR/listing_failed" ]

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY_NEW'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_output $'-N XRAY_TPROXY_NEW\n-A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

@test "swap_fw_chain: refuses a chain name that leaves no room for _NEW" {
    load_firewall
    use_stateful_iptables
    : > /tmp/bats_iptables_calls.log
    run swap_fw_chain mangle ABCDEFGHIJKLMNOPQRSTUVWXY build_new pos_one "-i br0"
    assert_failure 2
    assert_output --partial "leaves no room for the _NEW suffix"
    [ ! -s /tmp/bats_iptables_calls.log ]
}
