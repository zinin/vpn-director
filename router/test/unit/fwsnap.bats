#!/usr/bin/env bats

load '../test_helper'

# load_fwsnap - the modules as vpn-director.sh loads them, fwsnap.sh last, on the stateful
# iptables: a test fills the chains with iptables -N and -A, and fwsnap_save lists what it wrote.
# FWSNAP_DIR is the test's own (test_helper.bash).
load_fwsnap() {
    load_tunnel_module
    source "$LIB_DIR/tproxy.sh" --source-only
    source "$LIB_DIR/fwsnap.sh"
    use_stateful_iptables
}

# tun_dir_applied - TUN_DIR as tunnel_apply leaves it for the fixture's wgc1, cut down to the
# client's MARK rule, and its jump from br0.
tun_dir_applied() {
    iptables -t mangle -N TUN_DIR
    iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 \
        -j MARK --set-xmark 0x10000/0xff0000
    iptables -t mangle -A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
}

# ============================================================================
# fwsnap_save: mangle.rules
# ============================================================================

# Restore input for iptables-restore --noflush: every chain declared before any rule, each
# declared chain's rules as iptables -S lists them, then the jumps inserted from the top - Xray's
# first, numbered in listing order - and the INPUT accept of Xray's mark. Two LAN interfaces, and
# a rule of the firmware's between the jumps (Merlin's iface-mark rules), which is not ours.
@test "fwsnap_save: both chains, their jumps in order and the INPUT accept, as restore input" {
    load_fwsnap
    iptables -t mangle -N XRAY_TPROXY
    iptables -t mangle -A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN
    iptables -t mangle -A XRAY_TPROXY -j MARK --set-xmark 0x200/0x200
    iptables -t mangle -N TUN_DIR
    iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN
    iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 \
        -j MARK --set-xmark 0x10000/0xff0000
    iptables -t mangle -A PREROUTING -i br0 -j XRAY_TPROXY
    iptables -t mangle -A PREROUTING -i br1 -j XRAY_TPROXY
    iptables -t mangle -A PREROUTING -i wgc1 -j MARK --set-xmark 0x1/0x1
    iptables -t mangle -A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
    iptables -t mangle -A PREROUTING -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR
    iptables -t mangle -A INPUT -m mark --mark 0x100/0x100 -j ACCEPT
    iptables -t mangle -A INPUT -p udp -m udp --dport 53 -j ACCEPT

    run fwsnap_save
    assert_success
    refute_output

    local want
    want="$(cat <<'EOF'
*mangle
:XRAY_TPROXY - [0:0]
:TUN_DIR - [0:0]
-A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN
-A XRAY_TPROXY -j MARK --set-xmark 0x200/0x200
-A TUN_DIR -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN
-A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000
-I PREROUTING 1 -i br0 -j XRAY_TPROXY
-I PREROUTING 2 -i br1 -j XRAY_TPROXY
-I PREROUTING 3 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
-I PREROUTING 4 -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR
-I INPUT 1 -m mark --mark 0x100/0x100 -j ACCEPT
COMMIT
EOF
)"
    assert_equal "$(cat "$FWSNAP_DIR/mangle.rules")" "$want"
    # iptables-restore takes COMMIT only as a whole line; $(cat) above drops the last newline.
    [ "$(tail -c 1 "$FWSNAP_DIR/mangle.rules" | wc -l)" -eq 1 ]
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
}

# An older release could leave TUN_DIR's jump ahead of XRAY_TPROXY's. The snapshot puts Xray's in
# first all the same: Xray takes its clients before Tunnel Director sees them.
@test "fwsnap_save: Xray's jumps go in first, whatever order the listing has" {
    load_fwsnap
    tun_dir_applied
    iptables -t mangle -N XRAY_TPROXY
    iptables -t mangle -A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN
    iptables -t mangle -A PREROUTING -i br0 -j XRAY_TPROXY

    fwsnap_save
    run grep -- '^-I PREROUTING ' "$FWSNAP_DIR/mangle.rules"
    assert_output $'-I PREROUTING 1 -i br0 -j XRAY_TPROXY\n-I PREROUTING 2 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
}

# An interrupted swap leaves <chain>_NEW, its jumps in or not, and the next apply finishes the swap
# or deletes it (swap_fw_chain). The snapshot holds each chain by its name alone; with Xray not
# applied it declares TUN_DIR alone.
@test "fwsnap_save: our chains by their names, never the _NEW of an interrupted swap" {
    load_fwsnap
    iptables -t mangle -N TUN_DIR
    iptables -t mangle -A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 \
        -j MARK --set-xmark 0x10000/0xff0000
    iptables -t mangle -N TUN_DIR_NEW
    iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 \
        -j MARK --set-xmark 0x20000/0xff0000
    iptables -t mangle -A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW
    iptables -t mangle -A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR

    fwsnap_save
    local want
    want="$(cat <<'EOF'
*mangle
:TUN_DIR - [0:0]
-A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000
-I PREROUTING 1 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
COMMIT
EOF
)"
    assert_equal "$(cat "$FWSNAP_DIR/mangle.rules")" "$want"
}

# Nothing of ours in mangle - neither module applied, both stopped - is nothing to restore, and the
# snapshot an earlier apply left goes.
@test "fwsnap_save: no mangle snapshot when neither chain exists" {
    load_fwsnap
    mkdir -p "$FWSNAP_DIR"
    printf '*mangle\n:TUN_DIR - [0:0]\nCOMMIT\n' > "$FWSNAP_DIR/mangle.rules"
    iptables -t mangle -A PREROUTING -i wgc1 -j MARK --set-xmark 0x1/0x1

    run fwsnap_save
    assert_success
    refute_output
    [ ! -e "$FWSNAP_DIR/mangle.rules" ]
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
}

# ============================================================================
# fwsnap_save: the interface tunnels
# ============================================================================

# An interface tunnel's rules go with the firmware's rebuilds too (iftunnel.sh). Its MSS rules go
# into mangle.rules at the head of FORWARD, in their order; its MASQUERADE into nat.rules, a line
# for the restore to check and insert. A rule that names another interface is the firmware's, and
# an interface whose MASQUERADE is not in place has none to restore; with none left, nat.rules goes.
@test "fwsnap_save: an interface tunnel's MSS rules go with mangle, its MASQUERADE with nat" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\nawg2 2001\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    # As iftunnel_route_ensure leaves them: each inserted at 1, the inbound clamp last.
    iptables -t mangle -A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
    iptables -t mangle -A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    iptables -t mangle -A FORWARD -o wgc1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE

    fwsnap_save
    local want
    want="$(cat <<'EOF'
*mangle
:TUN_DIR - [0:0]
-A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000
-I PREROUTING 1 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
-I FORWARD 1 -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
-I FORWARD 2 -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
COMMIT
EOF
)"
    assert_equal "$(cat "$FWSNAP_DIR/mangle.rules")" "$want"
    assert_equal "$(cat "$FWSNAP_DIR/nat.rules")" "-o awg1 -j MASQUERADE"

    iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
}

# ============================================================================
# fwsnap_save: what it never costs
# ============================================================================

# /tmp/vpn-director is also watchd's runtime directory, and watchd refuses one that group or others
# can write to; the hooks hand the snapshot to iptables-restore as root. An apply brings its
# caller's umask - 0 on Asuswrt-Merlin - and none of the three follows it.
@test "fwsnap_save: neither the snapshot nor its directories follow the umask" {
    load_fwsnap
    tun_dir_applied
    export FWSNAP_DIR="$BATS_TEST_TMPDIR/run/fwsnap"
    umask 000

    fwsnap_save
    run stat -c %a "$BATS_TEST_TMPDIR/run" "$FWSNAP_DIR" "$FWSNAP_DIR/mangle.rules"
    assert_output $'755\n755\n644'
}

# The snapshot only shortens a window the guards keep off the WAN already: one that cannot be
# written costs a WARN, never the apply.
@test "fwsnap_save: a snapshot that cannot be written is a WARN" {
    load_fwsnap
    tun_dir_applied
    : > "$BATS_TEST_TMPDIR/file"
    export FWSNAP_DIR="$BATS_TEST_TMPDIR/file/fwsnap"

    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot write $FWSNAP_DIR/mangle.rules"
}

# A listing that fails says nothing of the rules in place: the snapshot of an earlier apply stays,
# and the apply the hooks queue after restoring it reconciles what it holds that is stale.
@test "fwsnap_save: a listing that fails leaves the snapshot as it was" {
    load_fwsnap
    mkdir -p "$FWSNAP_DIR"
    printf '*mangle\n:TUN_DIR - [0:0]\nCOMMIT\n' > "$FWSNAP_DIR/mangle.rules"
    tun_dir_applied
    iptables() {
        [[ ${1:-} == -t && ${2:-} == mangle && ${3:-} == -S ]] && return 4
        command iptables "$@"
    }

    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot list mangle"
    assert_equal "$(cat "$FWSNAP_DIR/mangle.rules")" $'*mangle\n:TUN_DIR - [0:0]\nCOMMIT'
}

# ============================================================================
# fwsnap_remove
# ============================================================================

@test "fwsnap_remove: leaves no snapshot" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ -f "$FWSNAP_DIR/mangle.rules" ]
    [ -f "$FWSNAP_DIR/nat.rules" ]

    run fwsnap_remove
    assert_success
    refute_output
    [ ! -e "$FWSNAP_DIR/mangle.rules" ]
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
}
