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

# Only the two MSS rules iftunnel.sh puts in for an interface tunnel go with the snapshot: another
# package's MSS rule that names the interface is not ours, and the restore would put it back at the
# head of FORWARD. Both foreign rules here sit between and after the own ones, which keep their
# order and their numbers.
@test "fwsnap_save: another package's MSS rule for an interface tunnel stays out of the snapshot" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t mangle -A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
    iptables -t mangle -A FORWARD -d 10.0.0.0/8 -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    iptables -t mangle -A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    iptables -t mangle -A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1300

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

# The hooks hand the snapshot to iptables-restore as root. A directory of its own that others can
# write to - an older release's stop left /tmp/vpn-director 0777 under Merlin's umask 0 - loses
# that write, and the file goes in 0644. A symlink in its place is refused with a WARN naming it,
# and nothing is written behind it.
@test "fwsnap_save: tightens a snapshot directory of its own, and refuses a symlink in its place" {
    load_fwsnap
    tun_dir_applied
    mkdir -p "$FWSNAP_DIR"
    chmod 777 "$FWSNAP_DIR"
    run fwsnap_save
    assert_success
    refute_output
    run stat -c %a "$FWSNAP_DIR" "$FWSNAP_DIR/mangle.rules"
    assert_output $'755\n644'

    rm -rf "$FWSNAP_DIR"
    mkdir "$BATS_TEST_TMPDIR/elsewhere"
    ln -s "$BATS_TEST_TMPDIR/elsewhere" "$FWSNAP_DIR"
    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot write $FWSNAP_DIR/mangle.rules: $FWSNAP_DIR is a symlink, no directory or not this user's alone"
    run ls -A "$BATS_TEST_TMPDIR/elsewhere"
    refute_output
}

# A listing of mangle that fails says nothing of the rules in place, and the run ends there: the
# snapshot of an earlier apply goes, both files. Left in place, it would put back after the next
# rebuild of the firewall rules the configuration may have dropped since.
@test "fwsnap_save: a mangle listing that fails removes the snapshot of an earlier apply" {
    load_fwsnap
    mkdir -p "$FWSNAP_DIR"
    printf '*mangle\n:TUN_DIR - [0:0]\nCOMMIT\n' > "$FWSNAP_DIR/mangle.rules"
    printf -- '-o awg1 -j MASQUERADE\n' > "$FWSNAP_DIR/nat.rules"
    tun_dir_applied
    iptables() {
        [[ ${1:-} == -t && ${2:-} == mangle && ${3:-} == -S ]] && return 4
        command iptables "$@"
    }

    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot list mangle; the snapshot of an earlier apply removed"
    [ ! -e "$FWSNAP_DIR/mangle.rules" ]
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
}

# One of nat POSTROUTING that fails takes nat.rules alone: the mangle.rules this run has just
# written is current, and stays.
@test "fwsnap_save: a nat listing that fails removes nat.rules and keeps the new mangle.rules" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ -f "$FWSNAP_DIR/nat.rules" ]
    printf '*mangle\nCOMMIT\n' > "$FWSNAP_DIR/mangle.rules"
    iptables() {
        [[ ${1:-} == -t && ${2:-} == nat && ${3:-} == -S ]] && return 4
        command iptables "$@"
    }

    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot list nat POSTROUTING; the nat snapshot of an earlier apply removed"
    [ ! -e "$FWSNAP_DIR/nat.rules" ]
    run cat "$FWSNAP_DIR/mangle.rules"
    assert_line ':TUN_DIR - [0:0]'
    assert_line '-A TUN_DIR -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
}

# A write that fails in the directory it vetted - here the file beside the snapshot is a
# directory, so nothing goes in - takes the file of an earlier apply away with it.
@test "fwsnap_save: a write that fails removes the file of an earlier apply" {
    load_fwsnap
    tun_dir_applied
    mkdir -p "$FWSNAP_DIR"
    printf '*mangle\n:TUN_DIR - [0:0]\nCOMMIT\n' > "$FWSNAP_DIR/mangle.rules"
    mkdir "$FWSNAP_DIR/mangle.rules.new"

    run fwsnap_save
    assert_success
    assert_output --partial "WARN"
    assert_output --partial "Firewall snapshot: cannot write $FWSNAP_DIR/mangle.rules; the one of an earlier apply removed"
    [ ! -e "$FWSNAP_DIR/mangle.rules" ]
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

# ============================================================================
# fwsnap-restore.sh
# ============================================================================

# The PATH fwsnap-restore.sh runs with in these tests (VPD_RESTORE_PATH): the stateful mocks, then
# the stateless ones, ahead of /usr/bin:/bin. Its own default names the router's directories, which
# on this machine hold the real iptables and iptables-restore.
RESTORE_PATH="$TEST_ROOT/mocks/stateful:$TEST_ROOT/mocks:/usr/bin:/bin"

# applied [lan...] - the fixture applied as vpn-director.sh applies it, Xray first, from the LAN
# interfaces named (br0 without one), and the snapshot of the rules it left.
applied() {
    load_fwsnap
    if [[ $# -gt 0 ]]; then
        LAN_IFACES="$*"
        platform_lan_ifaces() { printf '%s\n' $LAN_IFACES; }
    fi
    run tproxy_apply
    assert_success
    run tunnel_apply
    assert_success
    fwsnap_save
    [ -f "$FWSNAP_DIR/mangle.rules" ]
    [ -f "$TUN_DIR_HASH" ]
}

# ndm_rebuild - what NDM's rebuild of mangle does to our chains: every jump to them goes, then the
# chains, emptied and deleted. INPUT and FORWARD keep what they hold.
ndm_rebuild() {
    local line chain
    local -a spec
    while IFS= read -r line; do
        [[ $line == "-A PREROUTING "*" -j XRAY_TPROXY" || $line == "-A PREROUTING "*" -j TUN_DIR" ]] ||
            continue
        read -ra spec <<< "${line#"-A PREROUTING "}"
        iptables -t mangle -D PREROUTING "${spec[@]}"
    done <<< "$(iptables -t mangle -S PREROUTING)"
    for chain in XRAY_TPROXY TUN_DIR; do
        iptables -t mangle -S "$chain" >/dev/null 2>&1 || continue
        iptables -t mangle -F "$chain"
        iptables -t mangle -X "$chain"
    done
}

# restore <table> - fwsnap-restore.sh run as the hooks run it, a process of its own under sh, on
# RESTORE_PATH, with the test's stopped marker and the Tunnel Director state tunnel.sh keeps.
# Neither iptables nor iptables-restore may resolve outside the mocks and the test's own fakes
# there: no test reaches this machine's own.
restore() {
    local tool found mocks fakes
    mocks="$(readlink -f "$TEST_ROOT/mocks")"
    fakes="$(readlink -f "$BATS_TEST_TMPDIR")"
    for tool in iptables iptables-restore; do
        found="$(PATH="$RESTORE_PATH" command -v "$tool")" || continue
        found="$(readlink -f "$found")"
        [[ $found == "$mocks/"* || $found == "$fakes/"* ]] ||
            fail "$tool resolves to $found on $RESTORE_PATH"
    done
    VPD_RESTORE_PATH="$RESTORE_PATH" VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped" \
        VPD_TUN_DIR_HASH="$TUN_DIR_HASH" VPD_TUN_DIR_RESTORED="$TUN_DIR_RESTORED" \
        run sh "$LIB_DIR/fwsnap-restore.sh" "$@"
}

# KeeneticOS: NDM deletes our chains with their jumps on every rebuild of mangle. The restore puts
# both back in one commit, Xray's jump first at the top of PREROUTING, takes Tunnel Director's hash
# away and leaves its restored marker, so the apply the hook queues next rebuilds; the copy it fed
# iptables-restore goes.
@test "fwsnap-restore: restores both chains and their jumps after NDM deleted them" {
    applied
    local xray tun_dir
    xray="$(iptables -t mangle -S XRAY_TPROXY)"
    tun_dir="$(iptables -t mangle -S TUN_DIR)"
    ndm_rebuild
    : > /tmp/bats_logger_calls.log

    restore mangle
    assert_success
    refute_output
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    assert_equal "$(iptables -t mangle -S XRAY_TPROXY)" "$xray"
    assert_equal "$(iptables -t mangle -S TUN_DIR)" "$tun_dir"
    [ ! -e "$TUN_DIR_HASH" ]
    [ -e "$TUN_DIR_RESTORED" ]
    run cat /tmp/bats_logger_calls.log
    assert_output 'logger -t vpn-director Restored the mangle rules from the snapshot'
    run ls "$FWSNAP_DIR"
    assert_output 'mangle.rules'
}

# Asuswrt-Merlin: every firewall start runs "iptables -t mangle -F", which empties every chain of
# the table, PREROUTING included, and deletes none; the firmware's own rules go back in before
# firewall-start runs. iptables-restore --noflush flushes a chain it declares that exists and fills
# it in the same commit, and our jumps go in at the top, ahead of the firmware's iface-mark rules
# until the rebuild that follows puts TUN_DIR's behind them.
@test "fwsnap-restore: refills the chains Merlin's firewall start emptied" {
    applied
    local xray tun_dir
    xray="$(iptables -t mangle -S XRAY_TPROXY)"
    tun_dir="$(iptables -t mangle -S TUN_DIR)"
    iptables -t mangle -F
    iptables -t mangle -A PREROUTING -i wgc1 -j MARK --set-xmark 0x1/0x1

    restore mangle
    assert_success
    refute_output
    assert_equal "$(iptables -t mangle -S XRAY_TPROXY)" "$xray"
    assert_equal "$(iptables -t mangle -S TUN_DIR)" "$tun_dir"
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR\n-A PREROUTING -i wgc1 -j MARK --set-xmark 0x1/0x1'
    [ -e "$TUN_DIR_RESTORED" ]
}

# A hook called for a table the rebuild left as it was restores nothing: KeeneticOS calls
# netfilter.d for every table it rebuilds, and Merlin's firewall start keeps mangle while
# traditional QoS is on. A jump to any chain of the snapshot is enough: a second one would follow
# a restore over it.
@test "fwsnap-restore: leaves a table that still holds our jumps alone" {
    applied
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute_output
    iptables -t mangle -D PREROUTING -i br0 -j XRAY_TPROXY
    restore mangle
    assert_success
    refute_output
    refute grep -q '^iptables-restore' /tmp/bats_iptables_calls.log
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
}

# NDM calls netfilter.d for mangle several times in one rebuild - six in one interface cycle on
# 5.1.5 - and the first call has put the rules back by the second: one jump per chain and LAN
# interface, whatever the count.
@test "fwsnap-restore: a second restore leaves one jump per chain and interface" {
    applied br0 br1
    ndm_rebuild
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    restore mangle
    assert_success
    run grep -c '^iptables-restore' /tmp/bats_iptables_calls.log
    assert_output 1
    run iptables -t mangle -S PREROUTING
    assert_output "$(cat <<'LIST'
-P PREROUTING ACCEPT
-A PREROUTING -i br0 -j XRAY_TPROXY
-A PREROUTING -i br1 -j XRAY_TPROXY
-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
-A PREROUTING -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR
LIST
)"
    run grep -c -- '-i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR$' <<< "$output"
    assert_output 1
}

# stop leaves its marker before it tears anything down, and removes the snapshot as well: a
# rebuild of the firewall during the teardown or after it brings nothing back. The hook still
# queues its apply, as it always has.
@test "fwsnap-restore: restores nothing after stop" {
    applied
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ -f "$FWSNAP_DIR/nat.rules" ]
    ndm_rebuild
    iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE
    printf '1\n' > "$BATS_TEST_TMPDIR/stopped"
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute_output
    restore nat
    assert_success
    refute_output
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
}

@test "fwsnap-restore: does nothing without a snapshot" {
    load_fwsnap
    tun_dir_applied
    iptables -t mangle -D PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute_output
    restore nat
    assert_success
    refute_output
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ ! -e "$TUN_DIR_RESTORED" ]
}

# both_snapshots - the fixture applied with an interface tunnel, the snapshot of both tables, and
# a rebuild of both after it: a restore that goes in has something to put back.
both_snapshots() {
    applied
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ -f "$FWSNAP_DIR/nat.rules" ]
    ndm_rebuild
    iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE
}

# not_restored - the restore of mangle and of nat touched nothing: not one iptables call, the hash
# kept, no marker, and a syslog line for each saying why.
not_restored() {
    : > /tmp/bats_iptables_calls.log
    : > /tmp/bats_logger_calls.log
    restore mangle
    assert_success
    refute_output
    restore nat
    assert_success
    refute_output
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
    run grep -c "not restored: the file or its directory is not root's alone" /tmp/bats_logger_calls.log
    assert_output 2
}

# Whoever can write the snapshot hands root the rules it restores. A snapshot file, its directory or
# that directory's parent that group or others can write to restores nothing. Back to the owner's
# alone - or with a parent that has the sticky bit, as /tmp has - the same snapshot goes in.
@test "fwsnap-restore: a snapshot that group or others can write to is not restored" {
    both_snapshots
    local path mode
    for path in "$FWSNAP_DIR" "$BATS_TEST_TMPDIR"; do
        for mode in g+w o+w; do
            chmod "$mode" "$path"
            not_restored
            chmod go-w "$path"
        done
    done
    for mode in g+w o+w; do
        chmod "$mode" "$FWSNAP_DIR/mangle.rules" "$FWSNAP_DIR/nat.rules"
        not_restored
        chmod go-w "$FWSNAP_DIR/mangle.rules" "$FWSNAP_DIR/nat.rules"
    done

    chmod 1777 "$BATS_TEST_TMPDIR"
    restore mangle
    assert_success
    restore nat
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    run iptables -t nat -S POSTROUTING
    assert_output $'-P POSTROUTING ACCEPT\n-A POSTROUTING -o awg1 -j MASQUERADE'
    [ -e "$TUN_DIR_RESTORED" ]
}

# A symlink in place of a snapshot file, of its directory or of that directory's parent restores
# nothing either: whoever planted it chose what root reads.
@test "fwsnap-restore: a snapshot reached through a symlink is not restored" {
    both_snapshots
    local name
    mkdir "$BATS_TEST_TMPDIR/real"
    for name in mangle.rules nat.rules; do
        mv "$FWSNAP_DIR/$name" "$BATS_TEST_TMPDIR/real/$name"
        ln -s "$BATS_TEST_TMPDIR/real/$name" "$FWSNAP_DIR/$name"
    done
    not_restored
    for name in mangle.rules nat.rules; do
        rm "$FWSNAP_DIR/$name"
        mv "$BATS_TEST_TMPDIR/real/$name" "$FWSNAP_DIR/$name"
    done

    mv "$FWSNAP_DIR" "$BATS_TEST_TMPDIR/realdir"
    ln -s "$BATS_TEST_TMPDIR/realdir" "$FWSNAP_DIR"
    not_restored
    rm "$FWSNAP_DIR"
    mv "$BATS_TEST_TMPDIR/realdir" "$FWSNAP_DIR"

    mkdir "$BATS_TEST_TMPDIR/realparent"
    mv "$FWSNAP_DIR" "$BATS_TEST_TMPDIR/realparent/fwsnap"
    ln -s "$BATS_TEST_TMPDIR/realparent" "$BATS_TEST_TMPDIR/parent"
    export FWSNAP_DIR="$BATS_TEST_TMPDIR/parent/fwsnap"
    not_restored
}

# The restored marker sits in /tmp/tunnel_director, where a symlink planted at its path had its
# target truncated as root. What stands there goes, and the marker is made anew: a regular empty
# file, the target as it was.
@test "fwsnap-restore: a symlink planted at the marker's path goes, its target untouched" {
    applied
    ndm_rebuild
    printf 'keep\n' > "$BATS_TEST_TMPDIR/target"
    rm -f "$TUN_DIR_RESTORED"
    ln -s "$BATS_TEST_TMPDIR/target" "$TUN_DIR_RESTORED"

    restore mangle
    assert_success
    assert_equal "$(cat "$BATS_TEST_TMPDIR/target")" keep
    [ -f "$TUN_DIR_RESTORED" ]
    [ ! -L "$TUN_DIR_RESTORED" ]
    [ ! -s "$TUN_DIR_RESTORED" ]
}

# The hooks restore the table the firmware rebuilt, and only mangle and nat hold rules of ours.
@test "fwsnap-restore: leaves every other table alone" {
    applied
    ndm_rebuild
    : > /tmp/bats_iptables_calls.log

    restore filter
    assert_success
    refute_output
    restore
    assert_success
    refute_output
    [ ! -s /tmp/bats_iptables_calls.log ]
    [ -f "$TUN_DIR_HASH" ]
}

# Merlin's nat-start runs after every rebuild of nat, which takes an interface tunnel's MASQUERADE;
# KeeneticOS calls netfilter.d for nat twice in one interface cycle. The rule goes back at the head
# of POSTROUTING, ahead of the firmware's own, and once.
@test "fwsnap-restore: nat: puts back a missing MASQUERADE, once" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    [ -f "$FWSNAP_DIR/nat.rules" ]
    # The rebuild: the firmware's rules back, ours not.
    iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE
    iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE

    restore nat
    assert_success
    refute_output
    restore nat
    assert_success
    refute_output
    run iptables -t nat -S POSTROUTING
    assert_output $'-P POSTROUTING ACCEPT\n-A POSTROUTING -o awg1 -j MASQUERADE\n-A POSTROUTING -o eth0 -j MASQUERADE'
}

# A router whose firmware and Entware lack iptables-restore, and a restore the kernel refuses:
# nothing changes, nothing is logged, and the hook goes on to queue its apply. iptables-restore
# commits a table whole or not at all.
@test "fwsnap-restore: exits 0 when iptables-restore fails or is missing" {
    applied
    ndm_rebuild
    mkdir "$BATS_TEST_TMPDIR/bin"
    ln -s "$TEST_ROOT/mocks/stateful/iptables" "$BATS_TEST_TMPDIR/bin/iptables"
    ln -s "$TEST_ROOT/mocks/logger" "$BATS_TEST_TMPDIR/bin/logger"
    local path="$RESTORE_PATH"
    RESTORE_PATH="$BATS_TEST_TMPDIR/bin:/usr/bin:/bin"
    : > /tmp/bats_logger_calls.log
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute_output
    [ ! -s /tmp/bats_iptables_calls.log ]
    run iptables -t mangle -S PREROUTING
    assert_output '-P PREROUTING ACCEPT'
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]

    RESTORE_PATH="$path"
    sed -i 's/^COMMIT$/-A NO_SUCH_CHAIN -j RETURN\nCOMMIT/' "$FWSNAP_DIR/mangle.rules"
    restore mangle
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output '-P PREROUTING ACCEPT'
    run iptables -t mangle -S XRAY_TPROXY
    assert_failure
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
    [ ! -s /tmp/bats_logger_calls.log ]
    run ls "$FWSNAP_DIR"
    assert_output 'mangle.rules'
}

# A listing of PREROUTING that fails - iptables 1.4.21 fails at once while another iptables holds
# the xtables lock, an apply's racing this restore among them - says nothing of the jumps: nothing
# is restored, as a restore over the jumps in place would double them.
@test "fwsnap-restore: a PREROUTING listing that fails restores nothing" {
    applied
    mkdir "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/iptables" <<EOF
#!/bin/bash
[[ \$* == "-t mangle -S PREROUTING" ]] && exit 4
exec "$TEST_ROOT/mocks/stateful/iptables" "\$@"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/iptables"
    local listing
    listing="$(iptables -t mangle -S PREROUTING)"
    RESTORE_PATH="$BATS_TEST_TMPDIR/bin:$RESTORE_PATH"
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute grep -q '^iptables-restore' /tmp/bats_iptables_calls.log
    assert_equal "$(iptables -t mangle -S PREROUTING)" "$listing"
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
}

# The apply the hook queues after the restore rebuilds Tunnel Director, and its swaps take over the
# restored chains: one jump to each - a jump left beside the restore's, as an apply racing it can
# leave one, included - no flush of a chain a jump leads to, the hash recorded, the marker gone.
@test "fwsnap-restore: the rebuild after a restore converges" {
    applied
    ndm_rebuild
    restore mangle
    assert_success
    iptables -t mangle -I PREROUTING 1 -i br0 -j XRAY_TPROXY
    iptables -t mangle -A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR

    run tproxy_apply
    assert_success
    run tunnel_apply
    assert_success
    assert_output --partial "rebuilding"
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
    [ -f "$TUN_DIR_HASH" ]
    [ ! -e "$TUN_DIR_RESTORED" ]
}

# A rebuild that took our chains and their jumps and left INPUT and FORWARD as they were: the
# snapshot's rules there go in only where iptables -C does not find them, at the head of their
# chains. A doubled MSS clamp would outlive its tunnel's release.
@test "fwsnap-restore: a snapshot restored while FORWARD still holds the MSS rules leaves one copy of each" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t mangle -A INPUT -m mark --mark 0x100/0x100 -j ACCEPT
    iptables -t mangle -A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
    iptables -t mangle -A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    fwsnap_save
    local input forward
    input="$(iptables -t mangle -S INPUT)"
    forward="$(iptables -t mangle -S FORWARD)"

    # NDM's rebuild takes them all: all of them come back, in their order.
    ndm_rebuild
    iptables -t mangle -F INPUT
    iptables -t mangle -F FORWARD
    restore mangle
    assert_success
    assert_equal "$(iptables -t mangle -S INPUT)" "$input"
    assert_equal "$(iptables -t mangle -S FORWARD)" "$forward"

    # One that took the chains alone: the chains come back, and a single copy of each other rule.
    ndm_rebuild
    restore mangle
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    assert_equal "$(iptables -t mangle -S INPUT)" "$input"
    assert_equal "$(iptables -t mangle -S FORWARD)" "$forward"

    # And one MSS rule with them: that one goes back at the head, the other stays single.
    ndm_rebuild
    iptables -t mangle -D FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    restore mangle
    assert_success
    run iptables -t mangle -S FORWARD
    assert_output "$(cat <<'LIST'
-P FORWARD ACCEPT
-A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
-A FORWARD -i awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1240
LIST
)"
    assert_equal "$(iptables -t mangle -S INPUT)" "$input"
}

# iptables 1.4.21 exits 4 at once while another iptables holds the xtables lock, and the restore
# races the applies by design. Such a -C says nothing of the rule: read as "not there", it doubled
# a rule that was, and the copy outlived the rule's release - every later snapshot carried both.
# Mangle restores nothing then, as for a failed listing of PREROUTING; nat inserts nothing, and the
# apply the hook queues puts a missing rule back. With the lock free, -C answers 0 for a rule that
# is there and 1 for one that is not, and the restore goes as before.
@test "fwsnap-restore: an iptables -C that cannot answer restores no mangle rule and inserts no MASQUERADE" {
    load_fwsnap
    export IFTUNNEL_STATE_FILE="$BATS_TEST_TMPDIR/iftunnels"
    printf 'awg1 2000\n' > "$IFTUNNEL_STATE_FILE"
    tun_dir_applied
    iptables -t mangle -A FORWARD -o awg1 -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
    iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE
    fwsnap_save
    local tun_dir forward nat
    tun_dir="$(iptables -t mangle -S TUN_DIR)"
    forward="$(iptables -t mangle -S FORWARD)"
    nat="$(iptables -t nat -S POSTROUTING)"
    # A rebuild that took our chains and their jumps, and left FORWARD and nat as they were.
    ndm_rebuild
    mkdir "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/iptables" <<EOF
#!/bin/bash
[[ " \$* " == *" -C "* ]] && exit 4
exec "$TEST_ROOT/mocks/stateful/iptables" "\$@"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/iptables"
    local path="$RESTORE_PATH"
    RESTORE_PATH="$BATS_TEST_TMPDIR/bin:$RESTORE_PATH"
    : > /tmp/bats_iptables_calls.log

    restore mangle
    assert_success
    refute grep -q '^iptables-restore' /tmp/bats_iptables_calls.log
    run iptables -t mangle -S PREROUTING
    assert_output '-P PREROUTING ACCEPT'
    assert_equal "$(iptables -t mangle -S FORWARD)" "$forward"
    [ ! -e "$TUN_DIR_RESTORED" ]
    restore nat
    assert_success
    assert_equal "$(iptables -t nat -S POSTROUTING)" "$nat"

    # The lock free: the FORWARD rule found (0) stays single, the chain comes back; the
    # MASQUERADE a rebuild of nat took is not found (1) and goes back in.
    RESTORE_PATH="$path"
    restore mangle
    assert_success
    assert_equal "$(iptables -t mangle -S TUN_DIR)" "$tun_dir"
    assert_equal "$(iptables -t mangle -S FORWARD)" "$forward"
    iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE
    restore nat
    assert_success
    assert_equal "$(iptables -t nat -S POSTROUTING)" "$nat"
}

# The hooks name no state file: the restore's own defaults have to be the files tunnel.sh keeps
# its hash in and looks for the marker at (TUN_DIR_HASH, TUN_DIR_RESTORED).
@test "fwsnap-restore: by default it drops the hash and leaves the marker where tunnel.sh looks" {
    applied
    ndm_rebuild

    VPD_RESTORE_PATH="$RESTORE_PATH" VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped" \
        run sh "$LIB_DIR/fwsnap-restore.sh" mangle
    assert_success
    [ ! -e "$TUN_DIR_HASH" ]
    [ -e "$TUN_DIR_RESTORED" ]
}

# The hooks run it with /bin/sh: BusyBox's on Asuswrt-Merlin, which has no "command" builtin, and
# no shell there needs "local", which POSIX lacks.
@test "fwsnap-restore: is POSIX sh and executable in the tree" {
    local script="$LIB_DIR/fwsnap-restore.sh"
    [ -x "$script" ]
    head -n1 "$script" | grep -qx '#!/bin/sh'
    sh -n "$script"
    run grep -nE '^[^#]*\b(command|local)\b' "$script"
    assert_failure
}
