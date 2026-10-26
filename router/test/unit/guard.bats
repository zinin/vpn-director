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

# The rule of an earlier direct_mark would route a mark nothing sets any more for good.
@test "guard_direct_ensure: removes what else sits at its preference and keeps its own rule" {
    load_tunnel_module
    guard_direct_ensure
    ip rule add pref 16894 fwmark 0x400/0x400 lookup main
    : > /tmp/bats_ip_calls.log
    run guard_direct_ensure
    assert_success
    run grep -E '^ip rule (add|del)' /tmp/bats_ip_calls.log
    assert_output 'ip rule del pref 16894 from all fwmark 0x400/0x400 lookup main'
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(grep -c '^16894:' <<< "$output")" 1
}

@test "guard_direct_ensure: its own rule goes in before the others at its preference go" {
    load_tunnel_module
    ip rule add pref 16894 fwmark 0x400/0x400 lookup main
    ip rule add pref 16894 from 10.8.0.0/24 lookup wgc1
    : > /tmp/bats_ip_calls.log
    run guard_direct_ensure
    assert_success
    run grep -E '^ip rule (add|del)' /tmp/bats_ip_calls.log
    assert_line --index 0 'ip rule add pref 16894 fwmark 0x200/0x200 lookup main'
    assert_line --index 1 'ip rule del pref 16894 from all fwmark 0x400/0x400 lookup main'
    assert_line --index 2 'ip rule del pref 16894 from 10.8.0.0/24 lookup wgc1'
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(grep -c '^16894:' <<< "$output")" 1
}

# While its own rule is not in, the rule of an earlier direct_mark is all that sends to main what
# a chain built under that mark lets go direct.
@test "guard_direct_ensure: a refused rule is an ERROR and rc 1, and the others stay" {
    load_tunnel_module
    ip rule add pref 16894 fwmark 0x400/0x400 lookup main
    ip() {
        [[ ${1:-} == rule && ${2:-} == add ]] && return 2
        command ip "$@"
    }
    run guard_direct_ensure
    assert_failure
    grep -q 'ERROR.*direct rule.*pref 16894' "$LOG_FILE"
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x400/0x400 lookup main'
}

# Each of the conditions alone keeps the rule: either chain could mark a packet direct, and a guard
# at any of the four preferences would take such a packet if the rule went.
@test "guard_direct_release: keeps the direct rule while a chain or a guard is there" {
    load_tunnel_module
    use_stateful_iptables
    guard_direct_ensure

    iptables -t mangle -N TUN_DIR
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    iptables -t mangle -X TUN_DIR

    iptables -t mangle -N XRAY_TPROXY
    guard_direct_release
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    iptables -t mangle -X XRAY_TPROXY

    local pref
    for pref in 16895 16896 16897 16898; do
        ip rule add pref "$pref" from 192.168.1.5 iif br0 prohibit
        guard_direct_release
        run ip rule show
        assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
        ip rule del pref "$pref" from 192.168.1.5 iif br0 prohibit
    done

    guard_direct_release
    run ip rule show
    refute_line --partial '16894:'
}

@test "guard_direct_release: removes every rule at its preference, an earlier direct_mark's too" {
    load_tunnel_module
    use_stateful_iptables
    guard_direct_ensure
    ip rule add pref 16894 fwmark 0x400/0x400 lookup main
    ip rule add pref 16639 fwmark 0x10000/0xff0000 prohibit
    run guard_direct_release
    assert_success
    run ip rule show
    refute_line --partial '16894:'
    assert_line $'16639:\tfrom all fwmark 0x10000/0xff0000 prohibit'
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
    local list=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    guard_sync tunnel "$list"
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel "$list"
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

# 192.168.1.5 moves, so the new list goes in at the other preference, and 192.168.1.6's lines are
# refused there.
@test "guard_sync: a refused rule keeps the old list and returns 1" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip() {
        [[ ${1:-} == rule && ${2:-} == add && " $* " == *" 192.168.1.6 "* ]] && return 2
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.5 iif br0 prohibit\nfrom 192.168.1.6 iif br0 lookup wgc1\nfrom 192.168.1.6 iif br0 prohibit'
    assert_failure
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    assert_line $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    grep -q 'ERROR.*from 192.168.1.6 iif br0 lookup wgc1' "$LOG_FILE"
    grep -q 'ERROR.*from 192.168.1.6 iif br0 prohibit' "$LOG_FILE"
}

# The list that guards sits at the second preference after every second change. A sync that does
# not get a client's new guard in leaves that client its old one - and so does the next sync of
# the same list, which must not clear the old list to start over.
@test "guard_sync: a client whose new guard is refused keeps its old one, sync after sync" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.6 iif br0 lookup wgc1'
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 lookup wgc1'
    run ip rule show
    assert_line $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    refute_line --partial '16895:'
    ip() {
        [[ ${1:-} == rule && ${2:-} == add && " $* " == *" ovpnc1 "* ]] && return 2
        command ip "$@"
    }
    local list=$'from 192.168.1.5 iif br0 lookup ovpnc1\nfrom 192.168.1.6 iif br0 lookup wgc1'
    run guard_sync tunnel "$list"
    assert_failure
    run ip rule show
    assert_line $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    run guard_sync tunnel "$list"
    assert_failure
    run ip rule show
    assert_line $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    assert_line $'16896:\tfrom 192.168.1.6 iif br0 lookup wgc1'
}

@test "guard_sync: a list that only grows gets the new lines appended where it is" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit\nfrom 192.168.1.6 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 prohibit'
    assert_success
    run grep -E '^ip rule (add|del)' /tmp/bats_ip_calls.log
    assert_output $'ip rule add pref 16895 from 192.168.1.6 iif br0 lookup wgc2\nip rule add pref 16895 from 192.168.1.6 iif br0 prohibit'
    run ip rule show
    assert_line --index 1 $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line --index 2 $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    assert_line --index 3 $'16895:\tfrom 192.168.1.6 iif br0 lookup wgc2'
    assert_line --index 4 $'16895:\tfrom 192.168.1.6 iif br0 prohibit'
    refute_line --partial '16896:'
}

# Neither preference holds a part of the new list in its order: the one holding fewer rules is
# rewritten, and the longer list - the one that guarded so far - goes only once the new one is in.
@test "guard_sync: with no part of the list in place, rewrites the preference holding fewer rules" {
    load_tunnel_module
    ip rule add pref 16895 from 192.168.1.5 iif br0 lookup wgc1
    ip rule add pref 16895 from 192.168.1.6 iif br0 lookup wgc1
    ip rule add pref 16896 from 192.168.1.7 iif br0 lookup wgc1
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 lookup wgc2'
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_line --index 0 'ip rule del pref 16896 from 192.168.1.7 iif br0 lookup wgc1'
    assert_line --index 1 'ip rule add pref 16896 from 192.168.1.5 iif br0 lookup wgc2'
    assert_line --index 2 'ip rule add pref 16896 from 192.168.1.6 iif br0 lookup wgc2'
    assert_line --index 3 'ip rule del pref 16895 from 192.168.1.5 iif br0 lookup wgc1'
    assert_line --index 4 'ip rule del pref 16895 from 192.168.1.6 iif br0 lookup wgc1'
    run ip rule show
    refute_line --partial '16895:'
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

# last_add_before_first_del <log> - does every "rule add" of an order log come before its first
# "rule del"? A del before the last add is a moment in which a client can be without a guard.
last_add_before_first_del() {
    local last_add first_del
    last_add=$(grep -n '^ip rule add ' "$1" | tail -1 | cut -d: -f1)
    first_del=$(grep -n '^ip rule del ' "$1" | head -1 | cut -d: -f1)
    [[ -n $last_add && -n $first_del ]] && (( last_add < first_del ))
}

# An apply killed between its adds and the old list's deletes: the old list A whole at 16895, the
# first line of the new list B at 16896.
@test "guard_sync: finishes a list an interrupted sync left half-written" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    ip rule add pref 16896 from 192.168.1.6 iif br0 lookup wgc2
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.6 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 prohibit'
    assert_success
    last_add_before_first_del "$BATS_TEST_TMPDIR/order.log"
    run ip rule show
    assert_line --index 1 $'16896:\tfrom 192.168.1.6 iif br0 lookup wgc2'
    assert_line --index 2 $'16896:\tfrom 192.168.1.6 iif br0 prohibit'
    refute_line --partial '16895:'
}

# The mirror: the list that guarded so far whole at 16896, where it sits after every second change,
# the first line of the new one at 16895.
@test "guard_sync: finishes a list half-written at the first preference, the old one at the second" {
    load_tunnel_module
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.6 iif br0 lookup wgc1'
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.6 iif br0 lookup wgc1'
    ip rule add pref 16895 from 192.168.1.5 iif br0 lookup ovpnc1
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup ovpnc1\nfrom 192.168.1.6 iif br0 lookup ovpnc1'
    assert_success
    last_add_before_first_del "$BATS_TEST_TMPDIR/order.log"
    run ip rule show
    assert_line --index 1 $'16895:\tfrom 192.168.1.5 iif br0 lookup ovpnc1'
    assert_line --index 2 $'16895:\tfrom 192.168.1.6 iif br0 lookup ovpnc1'
    refute_line --partial '16896:'
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
