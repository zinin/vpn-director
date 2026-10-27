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
@test "guard_direct_ensure: removes an earlier direct_mark's rule at its preference and keeps its own" {
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

# No guard belongs at the direct rule's preference: one there is a former layout's - a pref_base
# raised by one puts the direct rule on Tunnel Director's former guards - and, in ahead of the rule
# at that preference, it would take what the chains mark direct.
@test "guard_direct_ensure: its own rule goes in before the others at its preference go" {
    load_tunnel_module
    ip rule add pref 16894 fwmark 0x400/0x400 lookup main
    ip rule add pref 16894 from 192.168.1.5 iif br0 lookup wgc1
    : > /tmp/bats_ip_calls.log
    run guard_direct_ensure
    assert_success
    run grep -E '^ip rule (add|del)' /tmp/bats_ip_calls.log
    assert_line --index 0 'ip rule add pref 16894 fwmark 0x200/0x200 lookup main'
    assert_line --index 1 'ip rule del pref 16894 from all fwmark 0x400/0x400 lookup main'
    assert_line --index 2 'ip rule del pref 16894 from 192.168.1.5 iif br0 lookup wgc1'
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

@test "guard_direct_release: removes its rule at its preference, and an earlier direct_mark's" {
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

# The kernel keeps no selector of length 0: "to x/0" reads back as no "to" at all, "from x/0" as
# "from all". Compared as written, such a line would never match what is listed.
@test "guard_sync: a /0 exclusion or client is not rewritten on every apply" {
    load_guard
    guard_sync tunnel 'from 192.168.1.5 to 0.0.0.0/0 iif br0 lookup main'
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel 'from 192.168.1.5 to 0.0.0.0/0 iif br0 lookup main'
    assert_success
    refute grep -q '^ip rule add' /tmp/bats_ip_calls.log
    guard_sync xray 'from 10.0.0.0/0 iif br0 prohibit'
    : > /tmp/bats_ip_calls.log
    run guard_sync xray 'from 10.0.0.0/0 iif br0 prohibit'
    assert_success
    refute grep -q '^ip rule add' /tmp/bats_ip_calls.log
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup main'
    assert_line $'16897:\tfrom all iif br0 prohibit'
}

# A line refused on every apply keeps the list that guarded before at the other preference. A
# client gone from the list since has nothing left to protect it, and its old guards would go on
# routing it in normal operation: they go, while those of the clients still listed stay.
@test "guard_sync: a refusal drops the old guards of a client the list no longer names" {
    load_guard
    guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit\nfrom 192.168.1.6 iif br0 lookup wgc1\nfrom 192.168.1.6 iif br0 prohibit'
    ip() {
        [[ ${1:-} == rule && ${2:-} == add && " $* " == *" wgc2 "* ]] && return 2
        command ip "$@"
    }
    run guard_sync tunnel $'from 192.168.1.5 iif br0 lookup wgc2\nfrom 192.168.1.5 iif br0 prohibit'
    assert_failure
    grep -q 'ERROR.*from 192.168.1.5 iif br0 lookup wgc2' "$LOG_FILE"
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    assert_line $'16896:\tfrom 192.168.1.5 iif br0 prohibit'
    refute_line --partial '192.168.1.6'
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

# ============================================================================
# Preferences that move with pref_base: the record
# ============================================================================

# log_rule_changes - for the rest of the test, every "ip rule add" and "ip rule del" is appended
# to $BATS_TEST_TMPDIR/order.log before it runs.
log_rule_changes() {
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
}

# After a raise of pref_base the former direct rule sits ahead of Tunnel Director's whole new
# lookup range, and sends what carries a slot mark and the direct bit to main instead of into its
# tunnel. 20000 puts the direct rule at 20510.
@test "guard_direct_ensure: a pref_base change moves the direct rule, the former one going once the new one is in" {
    load_guard
    guard_direct_ensure
    assert_equal "$(cat "$GUARD_STATE_DIR/direct")" 16894
    TUN_DIR_PREF_BASE=20000
    log_rule_changes
    run guard_direct_ensure
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_output $'ip rule add pref 20510 fwmark 0x200/0x200 lookup main\nip rule del pref 16894 from all fwmark 0x200/0x200 lookup main'
    run ip rule show
    refute_line --partial '16894:'
    assert_line $'20510:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(cat "$GUARD_STATE_DIR/direct")" 20510
}

# The former guards would sit ahead of the new lookup range too, and route every packet of their
# clients whatever its mark. Tunnel Director's guards move to 20511 and 20512.
@test "guard_sync: a pref_base change moves an owner's guards, make before break" {
    load_guard
    local list=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    guard_sync tunnel "$list"
    assert_equal "$(cat "$GUARD_STATE_DIR/tunnel")" "16895 16896"
    TUN_DIR_PREF_BASE=20000
    log_rule_changes
    run guard_sync tunnel "$list"
    assert_success
    last_add_before_first_del "$BATS_TEST_TMPDIR/order.log"
    assert_equal "$(grep -c '^ip rule del pref 16895 ' "$BATS_TEST_TMPDIR/order.log")" 2
    run ip rule show
    assert_line $'20511:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'20511:\tfrom 192.168.1.5 iif br0 prohibit'
    refute_line --regexp '^1689[56]:'
    assert_equal "$(cat "$GUARD_STATE_DIR/tunnel")" "20511 20512"
}

# Until the list is whole at the new preferences, the former ones are all that guard its clients.
@test "guard_sync: a refused guard at the new preferences keeps the former ones and the record" {
    load_guard
    local list=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    guard_sync tunnel "$list"
    TUN_DIR_PREF_BASE=20000
    ip() {
        [[ ${1:-} == rule && ${2:-} == add && " $* " == *" prohibit "* ]] && return 2
        command ip "$@"
    }
    run guard_sync tunnel "$list"
    assert_failure
    run ip rule show
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    assert_line $'20511:\tfrom 192.168.1.5 iif br0 lookup wgc1'
    assert_equal "$(cat "$GUARD_STATE_DIR/tunnel")" "16895 16896"
}

# A stop after a change of pref_base, with no apply in between: the rules at the preferences the
# records name go as well as those at the current ones, and so do the records.
@test "guard_clear and guard_direct_release take the recorded preferences too" {
    load_guard
    use_stateful_iptables
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    TUN_DIR_PREF_BASE=20000
    ip rule add pref 20510 fwmark 0x200/0x200 lookup main
    ip rule add pref 20511 from 192.168.1.6 iif br0 prohibit
    ip rule add pref 20514 from 192.168.1.101 iif br0 prohibit
    run guard_clear tunnel
    assert_success
    run guard_clear xray
    assert_success
    run guard_direct_release
    assert_success
    run ip rule show
    refute_line --regexp '^(1689[4-8]|2051[0-4]):'
    [ ! -e "$GUARD_STATE_DIR/direct" ]
    [ ! -e "$GUARD_STATE_DIR/tunnel" ]
    [ ! -e "$GUARD_STATE_DIR/xray" ]
}

# Until a sync or a clear takes them, the guards at the preferences a record names are guards like
# any other: they would take what the chains mark direct, were the direct rule gone.
@test "guard_direct_release: a guard at a preference a record names keeps the direct rule" {
    load_guard
    use_stateful_iptables
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    TUN_DIR_PREF_BASE=20000
    run guard_direct_release
    assert_success
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(cat "$GUARD_STATE_DIR/direct")" 16894
}

# Nothing else says where the rules were: without a record, no rule at another preference goes.
@test "no record: nothing is deleted at another preference" {
    load_guard
    use_stateful_iptables
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    rm -rf "$GUARD_STATE_DIR"
    TUN_DIR_PREF_BASE=20000
    : > /tmp/bats_ip_calls.log
    run guard_direct_ensure
    assert_success
    run guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    assert_success
    run guard_clear tunnel
    assert_success
    run guard_clear xray
    assert_success
    run guard_direct_release
    assert_success
    run grep -E '^ip rule del pref 1689[4-8] ' /tmp/bats_ip_calls.log
    refute_output
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    assert_line $'16897:\tfrom 192.168.1.100 iif br0 prohibit'
}

# A pref_base change of less than five puts former preferences of one kind on current ones of
# another. Moved by one: the direct rule onto Tunnel Director's first former preference, Tunnel
# Director's guards onto their second and Xray's first, Xray's onto their second and the one after.
@test "a pref_base moved by one ends with each kind on its new preferences alone" {
    load_guard
    local tlist=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    local xlist=$'from 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main\nfrom 192.168.1.100 iif br0 prohibit'
    guard_direct_ensure
    guard_sync tunnel "$tlist"
    guard_sync xray "$xlist"
    TUN_DIR_PREF_BASE=16385
    run guard_direct_ensure
    assert_success
    run guard_sync xray "$xlist"
    assert_success
    run guard_sync tunnel "$tlist"
    assert_success
    run ip rule show
    assert_line $'16895:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(grep -c '^16895:' <<< "$output")" 1
    assert_equal "$(grep -cE '^1689[4-9]:' <<< "$output")" 5
    run guard_list tunnel
    assert_output $'16896:\tfrom 192.168.1.5 iif br0 lookup wgc1\n16896:\tfrom 192.168.1.5 iif br0 prohibit'
    run guard_list xray
    assert_output $'16898:\tfrom 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main\n16898:\tfrom 192.168.1.100 iif br0 prohibit'
}

# The same move, with Tunnel Director's list changed after it: the change goes in at 16897, Xray's
# first former preference, which Xray's record still names - nothing of Xray's has gone in since.
# "stop xray" must not take Tunnel Director's guards with its own.
@test "guard_clear of one owner leaves a preference another kind holds now" {
    load_guard
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 lookup wgc1'
    guard_sync xray 'from 192.168.1.100 iif br0 prohibit'
    TUN_DIR_PREF_BASE=16385
    guard_direct_ensure
    guard_sync tunnel 'from 192.168.1.5 iif br0 lookup wgc1'
    guard_sync tunnel 'from 192.168.1.5 iif br0 lookup wgc2'
    run guard_list tunnel
    assert_output $'16897:\tfrom 192.168.1.5 iif br0 lookup wgc2'
    assert_equal "$(cat "$GUARD_STATE_DIR/xray")" "16897 16898"
    run guard_clear xray
    assert_success
    run ip rule show
    assert_line $'16895:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_line $'16897:\tfrom 192.168.1.5 iif br0 lookup wgc2'
}

# The cost of a record that cannot be written is the release of a later change of pref_base.
@test "a record that cannot be written is a WARN" {
    load_guard
    : > "$BATS_TEST_TMPDIR/file"
    export GUARD_STATE_DIR="$BATS_TEST_TMPDIR/file/guards"
    run guard_direct_ensure
    assert_success
    run guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    assert_success
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_line $'16895:\tfrom 192.168.1.5 iif br0 prohibit'
    grep -q "WARN.*cannot record preferences 16894 in $GUARD_STATE_DIR/direct" "$LOG_FILE"
    grep -q "WARN.*cannot record preferences 16895 16896 in $GUARD_STATE_DIR/tunnel" "$LOG_FILE"
}

# A listing that fails shows no rule to release: the record goes on naming the former preference,
# whose rule the next apply takes.
@test "guard_direct_ensure: a listing that fails releases nothing, and the record stays" {
    load_guard
    guard_direct_ensure
    TUN_DIR_PREF_BASE=20000
    ip() {
        [[ ${1:-} == rule && ${2:-} == show ]] && return 1
        command ip "$@"
    }
    run guard_direct_ensure
    assert_success
    assert_equal "$(cat "$GUARD_STATE_DIR/direct")" 16894
    unset -f ip
    run guard_direct_ensure
    assert_success
    run ip rule show
    refute_line --partial '16894:'
    assert_line $'20510:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_equal "$(cat "$GUARD_STATE_DIR/direct")" 20510
}

# ============================================================================
# What this layer owns: its shapes at its preferences, below main's rule
# ============================================================================

# After a change of pref_base Tunnel Director's slot rules sit for a moment on what are guard
# preferences now, until it moves them; one deleted before then would leave its slot's marks
# refused, or with no rule at all. The slot of main looks the main table up too, under a mark
# that is not its mask; a rule with iif and a fwmark is no guard either.
@test "a rule of another shape at a guard or direct preference is neither listed nor deleted" {
    load_guard
    use_stateful_iptables
    ip rule add pref 16894 fwmark 0x10000/0xff0000 lookup wgc1
    ip rule add pref 16895 fwmark 0x10000/0xff0000 lookup wgc1
    ip rule add pref 16894 fwmark 0x20000/0xff0000 lookup main
    ip rule add pref 16896 fwmark 0x30000/0xff0000 iif br0 lookup wgc2
    local list=$'from 192.168.1.5 iif br0 lookup wgc1\nfrom 192.168.1.5 iif br0 prohibit'
    run guard_direct_ensure
    assert_success
    run guard_sync tunnel "$list"
    assert_success
    run guard_list tunnel
    assert_output $'16895:\tfrom 192.168.1.5 iif br0 lookup wgc1\n16895:\tfrom 192.168.1.5 iif br0 prohibit'
    : > /tmp/bats_ip_calls.log
    run guard_sync tunnel "$list"
    assert_success
    refute grep -qE '^ip rule (add|del)' /tmp/bats_ip_calls.log
    run guard_clear tunnel
    assert_success
    run guard_direct_release
    assert_success
    run ip rule show
    assert_line $'16894:\tfrom all fwmark 0x10000/0xff0000 lookup wgc1'
    assert_line $'16895:\tfrom all fwmark 0x10000/0xff0000 lookup wgc1'
    assert_line $'16894:\tfrom all fwmark 0x20000/0xff0000 lookup main'
    assert_line $'16896:\tfrom all fwmark 0x30000/0xff0000 iif br0 lookup wgc2'
    refute_line --partial '192.168.1.5'
    refute_line --partial '0x200/0x200'
}

# stop runs with whatever pref_base the configuration holds, which guard_layout_check refuses for an
# apply: with 32256 the direct rule's preference is main's, 32766, and the guards' come after it.
# Nothing there is this layer's to take, a rule of its shapes no more than main's and default's.
@test "nothing at 32766 or above is touched" {
    load_guard
    use_stateful_iptables
    printf '%s\n' $'32766:\tfrom all lookup main' $'32766:\tfrom all fwmark 0x200/0x200 lookup main' \
        $'32767:\tfrom all lookup default' $'32768:\tfrom 192.168.1.5 iif br0 prohibit' \
        >> "${BATS_IP_RULES_FILE:-/tmp/bats_test_ip_rules}"
    TUN_DIR_PREF_BASE=32256
    : > /tmp/bats_ip_calls.log
    run guard_clear tunnel
    assert_success
    run guard_clear xray
    assert_success
    run guard_direct_release
    assert_success
    run guard_list tunnel
    refute_output
    # A record that names such a preference, the preferences back below it.
    TUN_DIR_PREF_BASE=16384
    mkdir -p "$GUARD_STATE_DIR"
    echo 32766 > "$GUARD_STATE_DIR/direct"
    echo '32767 32768' > "$GUARD_STATE_DIR/tunnel"
    echo '32769 32770' > "$GUARD_STATE_DIR/xray"
    run guard_direct_ensure
    assert_success
    run guard_sync tunnel 'from 192.168.1.6 iif br0 prohibit'
    assert_success
    echo 32766 > "$GUARD_STATE_DIR/direct"
    echo '32767 32768' > "$GUARD_STATE_DIR/tunnel"
    run guard_clear tunnel
    assert_success
    run guard_clear xray
    assert_success
    run guard_direct_release
    assert_success
    run ip rule show
    assert_line $'32766:\tfrom all lookup main'
    assert_line $'32766:\tfrom all fwmark 0x200/0x200 lookup main'
    assert_line $'32767:\tfrom all lookup default'
    assert_line $'32768:\tfrom 192.168.1.5 iif br0 prohibit'
    run awk '$2 == "rule" && ($3 == "add" || $3 == "del") && $4 == "pref" && $5 >= 32766' /tmp/bats_ip_calls.log
    refute_output
}

@test "nothing goes in at 32766 or above: a WARN and rc 1" {
    load_guard
    TUN_DIR_PREF_BASE=32256
    : > /tmp/bats_ip_calls.log
    run guard_direct_ensure
    assert_failure
    assert_output --partial "WARN"
    assert_output --partial "pref 32766 reaches main's rule (32766)"
    run guard_sync tunnel 'from 192.168.1.5 iif br0 prohibit'
    assert_failure
    assert_output --partial "WARN"
    assert_output --partial "pref 32767 reaches main's rule (32766)"
    run grep -E '^ip rule (add|del)' /tmp/bats_ip_calls.log
    refute_output
}
