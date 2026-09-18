#!/usr/bin/env bats

load '../test_helper'

# The router hooks: KeeneticOS's under router/opt/etc/ndm, Asuswrt-Merlin's
# under router/jffs/scripts. NDM runs its hooks serially under a 24-second
# timeout, so each one starts the CLI detached through nohup and exits; a fake
# nohup records what it was asked to run and runs nothing.
HOOKS="$PROJECT_ROOT/opt/etc/ndm"
MERLIN_HOOKS="$PROJECT_ROOT/jffs/scripts"

setup_hook_env() {
    # The hooks name the router's own directories in PATH, the way the Merlin
    # hooks do, so a fake nohup dropped into a temp dir would never be found.
    # Shell functions are looked up before PATH: define them in the shell that
    # then sources the hook. That shell is /bin/sh - dash here, the closest
    # thing on a workstation to the busybox ash NDM runs the hooks with.
    cat > "$BATS_TEST_TMPDIR/fakes.sh" <<EOF
nohup() { echo "\$*" >> "$BATS_TEST_TMPDIR/nohup.log"; }
logger() { echo "logger \$*" >> "$BATS_TEST_TMPDIR/logger.log"; }
EOF
    printf '#!/bin/bash\n' > "$BATS_TEST_TMPDIR/vpn-director.sh"
    chmod +x "$BATS_TEST_TMPDIR/vpn-director.sh"
    export VPD_SCRIPT="$BATS_TEST_TMPDIR/vpn-director.sh"
}

# run_hook <hook> [argv...] - source the hook in a POSIX shell that already
# has the fakes, with argv as its positional parameters and $0 as its name.
run_hook() {
    local hook="$1"
    shift
    run sh -c 'fakes=$1; hook=$2; shift 2; . "$fakes"; . "$hook"' \
        50-vpn-director.sh "$BATS_TEST_TMPDIR/fakes.sh" "$hook" "$@"
}

# The hook backgrounds nohup and exits at once; give the child a moment.
wait_for_apply() {
    local i
    for i in 1 2 3 4 5 6 7 8 9 10; do
        [[ -s "$BATS_TEST_TMPDIR/nohup.log" ]] && return 0
        sleep 0.2
    done
    return 1
}

assert_no_apply() {
    sleep 0.5
    [ ! -e "$BATS_TEST_TMPDIR/nohup.log" ]
}

# ---------------------------------------------------------------- netfilter.d
#
# NDM calls netfilter.d with argv "start" - measured on a KN-4521 running
# KeeneticOS 5.1.5, where one interface cycle produced type=iptables with
# table mangle x6, nat x2 and filter x2, argv "start" every time. The hook
# does not gate on $1, so that a future NDM verb still re-applies; the tests
# pass what the device passes.

@test "netfilter.d: re-applies after an IPv4 table rebuild" {
    setup_hook_env
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "$VPD_SCRIPT --wait apply"
}

@test "netfilter.d: every table NDM rebuilds counts" {
    setup_hook_env
    type=iptables table=filter run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    rm -f "$BATS_TEST_TMPDIR/nohup.log"
    type=iptables table=nat run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
}

@test "netfilter.d: ignores ip6tables and tables that are not ours" {
    setup_hook_env
    type=ip6tables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    assert_no_apply
    type=iptables table=raw run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    assert_no_apply
}

# ---------------------------------------------------------------------- wan.d

@test "wan.d: re-applies when a WAN connection comes up, not when it goes down" {
    setup_hook_env
    run_hook "$HOOKS/wan.d/50-vpn-director.sh" stop
    assert_success
    assert_no_apply
    run_hook "$HOOKS/wan.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "$VPD_SCRIPT --wait apply"
}

# ------------------------------------------------------------ interface hook
#
# iflayerchanged.d, not ifstatechanged.d: both fire on 5.1.5, but one OpenVPN0
# down/up cycle gives iflayerchanged.d exactly one "layer=ipv4 level=disabled"
# and one "layer=ipv4 level=running", while ifstatechanged.d's change=connected
# came only on the way up - the down edge would be missed.

@test "iflayerchanged.d: re-applies for the IPv4 layer of a VPN client interface" {
    setup_hook_env
    id=OpenVPN0 system_name=ovpn_br0 layer=ipv4 level=running \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" hook
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "$VPD_SCRIPT --wait apply"
}

@test "iflayerchanged.d: also when that layer goes down" {
    setup_hook_env
    id=Wireguard1 system_name=nwg1 layer=ipv4 level=disabled \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" hook
    assert_success
    wait_for_apply
}

@test "iflayerchanged.d: ignores other interfaces, other layers and other invocations" {
    setup_hook_env
    id=Bridge0 system_name=br0 layer=ipv4 level=running \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" hook
    assert_success
    assert_no_apply
    id=OpenVPN0 system_name=ovpn_br0 layer=link level=running \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" hook
    assert_success
    assert_no_apply
    id=OpenVPN0 system_name=ovpn_br0 layer=ipv4 level=running \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" start
    assert_success
    assert_no_apply
}

# ------------------------------------------------------------ Asuswrt-Merlin
#
# The firmware starts firewall-start and wan-event without waiting for them
# (run_custom_script with no timeout), so they call the CLI directly. Every
# firewall start empties every mangle chain - TUN_DIR and XRAY_TPROXY included -
# and a plain apply exits at once while another one holds the lock: when that
# was the apply of the last firewall start, the chains stayed empty until the
# next event. --wait queues it behind the running one instead.

# setup_merlin_hook_env - the fakes, and a CLI that notes what it was asked.
setup_merlin_hook_env() {
    setup_hook_env
    printf '#!/bin/sh\necho "$*" >> "%s/apply.log"\n' "$BATS_TEST_TMPDIR" > "$VPD_SCRIPT"
}

@test "firewall-start: re-applies, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/firewall-start" eth0
    assert_success
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "--wait apply"
}

@test "wan-event: re-applies when a WAN connection comes up, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/wan-event" 0 connected
    assert_success
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "--wait apply"
}

@test "wan-event: leaves the other WAN events alone" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/wan-event" 0 disconnected
    assert_success
    [ ! -e "$BATS_TEST_TMPDIR/apply.log" ]
}

# ------------------------------------------------------------------- common

@test "hooks: exit 0 and log when the CLI is not installed" {
    setup_hook_env
    export VPD_SCRIPT="$BATS_TEST_TMPDIR/absent.sh"
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    assert_no_apply
    grep -q "vpn-director not found" "$BATS_TEST_TMPDIR/logger.log"
}

# nohup is Entware's coreutils-nohup, which a partially provisioned router can
# be missing. Unguarded, "nohup: not found" goes into the hook's >/dev/null,
# the hook exits 0 and nothing is applied and nothing is logged - the silent
# failure these hooks exist to prevent. All three, because the guard is a copy
# in each file.
@test "hooks: exit 0 and log when nohup is not installed" {
    setup_hook_env
    # The fakes are shell functions, which are found before PATH, so a missing
    # nohup is staged by answering the one lookup the hook makes for it.
    cat >> "$BATS_TEST_TMPDIR/fakes.sh" <<'EOF'
command() { [ "$2" = nohup ] && return 1; return 0; }
EOF
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    run_hook "$HOOKS/wan.d/50-vpn-director.sh" start
    assert_success
    id=OpenVPN0 system_name=ovpn_br0 layer=ipv4 level=running \
        run_hook "$HOOKS/iflayerchanged.d/50-vpn-director.sh" hook
    assert_success
    assert_no_apply
    run grep -c "opkg install coreutils-nohup" "$BATS_TEST_TMPDIR/logger.log"
    assert_output "3"
}

@test "hooks: are POSIX sh and executable in the tree" {
    for hook in netfilter.d wan.d iflayerchanged.d; do
        [ -x "$HOOKS/$hook/50-vpn-director.sh" ]
        head -n1 "$HOOKS/$hook/50-vpn-director.sh" | grep -qx '#!/bin/sh'
        sh -n "$HOOKS/$hook/50-vpn-director.sh"
    done
}
