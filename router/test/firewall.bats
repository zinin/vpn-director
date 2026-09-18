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
