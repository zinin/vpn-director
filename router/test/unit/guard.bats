#!/usr/bin/env bats

load '../test_helper'

# Note: load_tunnel_module is provided by test_helper.bash
# It loads: common.sh, config.sh, guard.sh, ipset.sh, firewall.sh, tunnel.sh

# load_guard - common.sh, firewall.sh and guard.sh, without config.sh. config.sh makes the
# settings readonly, so a test that varies one sets it after this; guard.sh reads each when it is
# called, and has a default for each that is not set.
load_guard() {
    load_common
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/guard.sh"
}

# ============================================================================
# The layout: preferences and the direct mark
# ============================================================================

@test "guard_owner_prefs: the tunnel's two follow the direct rule, Xray's follow those" {
    load_tunnel_module
    assert_equal "$(guard_direct_pref)" 16894
    assert_equal "$(guard_owner_prefs tunnel)" "16895 16896"
    assert_equal "$(guard_owner_prefs xray)" "16897 16898"
    run guard_owner_prefs main
    assert_failure
    refute_output
}

@test "guard_direct_mark_hex: prints the mark as ip prints it" {
    load_guard
    assert_equal "$(guard_direct_mark_hex)" 0x200
    TUN_DIR_DIRECT_MARK=1024
    assert_equal "$(guard_direct_mark_hex)" 0x400
    TUN_DIR_DIRECT_MARK=0X0800
    assert_equal "$(guard_direct_mark_hex)" 0x800
}

@test "guard_layout_check: accepts the defaults" {
    load_tunnel_module
    run guard_layout_check
    assert_success
    refute_output
}

@test "guard_layout_check: refuses a direct mark inside Tunnel Director's field" {
    load_guard
    TUN_DIR_DIRECT_MARK=0x10000
    run guard_layout_check
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "direct_mark 0x10000"
    assert_output --partial "Tunnel Director's mark field"
}

@test "guard_layout_check: refuses a direct mark that is Xray's bit" {
    load_guard
    XRAY_FWMARK_MASK=0x100
    TUN_DIR_DIRECT_MARK=0x100
    run guard_layout_check
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "direct_mark 0x100"
    assert_output --partial "Xray's fwmark_mask"
}

@test "guard_layout_check: refuses a direct mark of more than one bit" {
    load_guard
    TUN_DIR_DIRECT_MARK=0x300
    run guard_layout_check
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "direct_mark '0x300' is not a single bit"
}

# Arithmetic on a value that is no number would end the caller with a bash error, not this ERROR;
# a leading zero would make bash read the rest as octal. 0 is no bit, and a mark has 32 bits.
@test "guard_layout_check: refuses a direct mark that is no bit of the fwmark" {
    load_guard
    local mark
    for mark in 0x2g0 0200 0 0x100000000; do
        TUN_DIR_DIRECT_MARK=$mark
        run guard_layout_check
        assert_failure
        assert_output --partial "direct_mark '$mark' is not a single bit"
    done
}

# 32252: the block range still fits (32252 + 2 * 255 = 32762 is the direct rule), and the last
# Xray guard would sit on main's rule at 32766. One less, and it ends at 32765.
@test "guard_layout_check: refuses guards that would reach main's rule" {
    load_guard
    TUN_DIR_PREF_BASE=32252
    run guard_layout_check
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "32762-32766"
    assert_output --partial "main's rule (32766)"
    TUN_DIR_PREF_BASE=32251
    run guard_layout_check
    assert_success
}

# ============================================================================
# guard_direct_ensure / guard_direct_release
# ============================================================================

@test "guard_direct_ensure: puts the direct rule after the block range, once" {
    load_tunnel_module
    guard_direct_ensure; guard_direct_ensure
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(grep -c '^16894:' <<< "$output")" 1
}

@test "guard_direct_ensure: a refused rule is an ERROR and rc 1" {
    load_tunnel_module
    ip() {
        [[ ${1:-} == rule && ${2:-} == add ]] && return 2
        command ip "$@"
    }
    run guard_direct_ensure
    assert_failure
    grep -q 'ERROR.*direct rule.*pref 16894' "$LOG_FILE"
}

@test "guard_direct_release: keeps the direct rule while a chain or a guard is there" {
    load_tunnel_module
    use_stateful_iptables
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    iptables -t mangle -N TUN_DIR
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'

    # The chain gone, Tunnel Director's guard still there.
    iptables -t mangle -X TUN_DIR
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'

    # Xray's chain, then Xray's guard.
    guard_clear tunnel
    iptables -t mangle -N XRAY_TPROXY
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    iptables -t mangle -X XRAY_TPROXY
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'

    guard_clear xray
    guard_direct_release
    run ip rule show
    refute_line --partial '16894:'
}

# ============================================================================
# guard_sync
# ============================================================================

@test "guard_sync: installs the lines in order at the owner's first preference" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main\nfrom 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    run ip rule show
    assert_line --index 1 $'16895:\tfrom 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main'
    assert_line --index 2 $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line --index 3 $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    refute_line --partial '16896:'
}

@test "guard_sync: an unchanged list adds and deletes nothing" {
    load_tunnel_module
    local lines=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    guard_sync tunnel "$lines"
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel "$lines"
    assert_success
    refute grep -qE '^ip rule (add|del)' /tmp/bats_ip_calls.log
}

@test "guard_sync: a changed list goes in at the other preference before the old one goes" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.5 iif br0 prohibit'
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_line --index 0 'ip rule add pref 16896 from 192.168.1.5 iif br0 lookup wgc2'
    assert_line --index 1 'ip rule add pref 16896 from 192.168.1.5 iif br0 prohibit'
    assert_line --index 2 'ip rule del pref 16895 from 192.168.1.5 iif br0 lookup wgc1'
    assert_line --index 3 'ip rule del pref 16895 from 192.168.1.5 iif br0 prohibit'
    run ip rule show
    assert_line --index 1 $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    assert_line --index 2 $'16896:\tfrom 192.168.1.5 iif br0 prohibit'
    refute_line --partial '16895:'
}

@test "guard_sync: a refused rule keeps the old list and returns 1" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip() {
        [[ ${1:-} == rule && ${2:-} == add && " $* " == *" 192.168.1.6 "* ]] && return 2
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit\nfrom 192.168.1.6 iif br0 lookup wgc1\nfrom 192.168.1.6 iif br0 prohibit'
    assert_failure
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    grep -q 'ERROR.*from 192.168.1.6 iif br0 lookup wgc1' "$LOG_FILE"
    grep -q 'ERROR.*from 192.168.1.6 iif br0 prohibit' "$LOG_FILE"
}

@test "guard_sync: an empty list clears both preferences" {
    load_tunnel_module
    ip rule add pref 16895 from 192.168.1.5 iif br0 prohibit
    ip rule add pref 16896 from 192.168.1.6 iif br0 prohibit
    run guard_sync tunnel ""
    assert_success
    run ip rule show
    refute_line --partial '16895:'
    refute_line --partial '16896:'
}

# An apply killed between its adds and the old list's deletes: the old list A whole at 16895, the
# first line of the new list B at 16896.
@test "guard_sync: finishes a list an interrupted sync left half-written" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip rule add pref 16896 from 192.168.1.6 iif br0 lookup wgc2
    run guard_sync tunnel $'from 192.168.1.6 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 prohibit'
    assert_success
    run ip rule show
    assert_line --index 1 $'16896:\tfrom 192.168.1.6 iif br0 lookup wgc2'
    assert_line --index 2 $'16896:\tfrom 192.168.1.6 iif br0 prohibit'
    refute_line --partial '16895:'
}

# iproute2 prints a host without its /32: compared as written, the list would never match what is
# listed, and every apply would rewrite it.
@test "guard_sync: a client spelled x/32 is not rewritten on every apply" {
    load_tunnel_module
    guard_sync tunnel 'from 192.168.1.5/32 iif br0 prohibit'
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel 'from 192.168.1.5/32 iif br0 prohibit'
    assert_success
    refute grep -q '^ip rule add' /tmp/bats_ip_calls.log
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
}

# The same for a table: the kernel prints it by its rt_tables name (111 is wgc1 in the fixture).
@test "guard_sync: a table given by its number is not rewritten on every apply" {
    load_tunnel_module
    guard_sync tunnel 'from 192.168.1.5 iif br0 lookup 111'
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel 'from 192.168.1.5 iif br0 lookup 111'
    assert_success
    refute grep -q '^ip rule add' /tmp/bats_ip_calls.log
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
}

# An Xray exclusion can repeat a private range, or name a host twice as x and x/32. The kernel
# refuses the second copy (an older one keeps a duplicate), which would fail the sync on every
# apply.
@test "guard_sync: a line given twice goes in once, where it came first" {
    load_tunnel_module
    run guard_sync xray $'from 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main\nfrom 192.168.1.100 to 5.6.7.8 iif br0 lookup main\nfrom 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main\nfrom 192.168.1.100 to 5.6.7.8/32 iif br0 lookup main\nfrom 192.168.1.100 iif br0 prohibit'
    assert_success
    run ip rule show
    assert_line --index 1 $'16897:\tfrom 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main'
    assert_line --index 2 $'16897:\tfrom 192.168.1.100 to 5.6.7.8 iif br0 lookup main'
    assert_line --index 3 $'16897:\tfrom 192.168.1.100 iif br0 prohibit'
    assert_equal "${#lines[@]}" 4
}

# ============================================================================
# guard_clear / guard_list
# ============================================================================

@test "guard_clear: removes the owner's rules and no other" {
    load_tunnel_module
    guard_direct_ensure
    ip rule add pref 16639 fwmark 0x10000/0xff0000 prohibit
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip rule add pref 16896 from 192.168.1.6 iif br0 prohibit
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    run guard_clear tunnel
    assert_success
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
    assert_line $'16897:\tfrom 192.168.1.100 iif br0 prohibit'
    refute_line --partial '16895:'
    refute_line --partial '16896:'
}

@test "guard_list: prints the owner's rules as ip rule show lists them" {
    load_tunnel_module
    guard_direct_ensure
    guard_sync tunnel $'from 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main\nfrom 192.168.1.5 iif br0 lookup wgc1'
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    run guard_list tunnel
    assert_success
    assert_output $'16895:\tfrom 192.168.1.5 to 192.168.100.0/24 iif br0 lookup main\n16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    run guard_list xray
    assert_success
    assert_output $'16897:\tfrom 192.168.1.100 iif br0 prohibit'
}
