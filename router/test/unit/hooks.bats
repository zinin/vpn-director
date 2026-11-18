#!/usr/bin/env bats

load '../test_helper'

# The router hooks: KeeneticOS's under router/opt/etc/ndm, Asuswrt-Merlin's
# under router/opt/vpn-director/hooks, which the firmware's shared
# /jffs/scripts/<hook> calls through one line. NDM runs its hooks serially
# under a 24-second timeout, so each one starts the CLI detached through nohup
# and exits; a fake nohup records what it was asked to run and runs nothing.
HOOKS="$PROJECT_ROOT/opt/etc/ndm"
MERLIN_HOOKS="$PROJECT_ROOT/opt/vpn-director/hooks"

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
    fake_restore "$BATS_TEST_TMPDIR/nohup.log"
}

# fake_restore <log> - the restore the hooks run before they queue the apply
# notes what it was asked in <log>, the one the apply is noted in, so one log
# shows the order. Nothing on this machine is restored.
fake_restore() {
    printf '#!/bin/sh\necho "restore $*" >> "%s"\n' "$1" > "$BATS_TEST_TMPDIR/fwsnap-restore.sh"
    chmod +x "$BATS_TEST_TMPDIR/fwsnap-restore.sh"
    export VPD_RESTORE="$BATS_TEST_TMPDIR/fwsnap-restore.sh"
}

# run_hook <hook> [argv...] - source the hook in a POSIX shell that already
# has the fakes, with argv as its positional parameters and $0 as its name.
run_hook() {
    local hook="$1"
    shift
    run sh -c 'fakes=$1; hook=$2; shift 2; . "$fakes"; . "$hook"' \
        50-vpn-director.sh "$BATS_TEST_TMPDIR/fakes.sh" "$hook" "$@"
}

# The hook backgrounds nohup and exits at once; give the child a moment. The
# restore's line comes first and at once, so the wait is for the apply's.
wait_for_apply() {
    local i
    for i in 1 2 3 4 5 6 7 8 9 10; do
        grep -q -- '--wait apply' "$BATS_TEST_TMPDIR/nohup.log" 2>/dev/null && return 0
        sleep 0.2
    done
    return 1
}

assert_no_apply() {
    sleep 0.5
    ! grep -q -- '--wait apply' "$BATS_TEST_TMPDIR/nohup.log" 2>/dev/null
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
    assert_line "$VPD_SCRIPT --wait apply"
}

# The rules of the last apply go back first, from its snapshot, and the hook
# waits for that: until then the guards refuse the Xray clients' connections and
# send a tunnel client's excluded countries through its tunnel. The restore
# leaves a table that holds our rules alone, so NDM's several calls for one
# rebuild add nothing; the apply follows detached, as before.
@test "netfilter.d: restores the table it was called for, then queues the apply" {
    setup_hook_env
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "restore mangle
$VPD_SCRIPT --wait apply"

    rm -f "$BATS_TEST_TMPDIR/nohup.log"
    type=iptables table=nat run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "restore nat
$VPD_SCRIPT --wait apply"
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
    # Nor is anything restored.
    [ ! -e "$BATS_TEST_TMPDIR/nohup.log" ]
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
# (run_custom_script with no timeout). Every firewall start empties every
# mangle chain - TUN_DIR and XRAY_TPROXY included - and a plain apply exits at
# once while another one holds the lock: when that was the apply of the last
# firewall start, the chains stayed empty until the next event. --wait queues
# it behind the running one instead. The firmware runs the shared
# /jffs/scripts/<hook>, whose one line of ours calls these, so the apply is
# detached: the commands after ours in that file do not wait for it. The tests
# run these hooks with fd 3 closed, so a detached apply never holds bats'
# output, and wait for the apply's line.

# setup_merlin_hook_env - the fakes, and a CLI that notes what it was asked, in
# the log the restore notes its table in.
setup_merlin_hook_env() {
    setup_hook_env
    printf '#!/bin/sh\necho "$*" >> "%s/apply.log"\n' "$BATS_TEST_TMPDIR" > "$VPD_SCRIPT"
    fake_restore "$BATS_TEST_TMPDIR/apply.log"
}

# wait_for_merlin_applies <n> - until apply.log holds <n> detached applies, 3 s at most.
wait_for_merlin_applies() {
    local i n
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
        n=$(grep -c -- '^--wait apply$' "$BATS_TEST_TMPDIR/apply.log" 2>/dev/null) || n=0
        [ "$n" -ge "$1" ] && return 0
        sleep 0.2
    done
    return 1
}

assert_no_merlin_apply() {
    sleep 0.5
    ! grep -q -- '--wait apply' "$BATS_TEST_TMPDIR/apply.log" 2>/dev/null
}

@test "firewall-start: re-applies, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    wait_for_merlin_applies 1
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_line "--wait apply"
}

# The firewall start has emptied every mangle chain. The restore refills ours
# from the last apply's snapshot first: the apply after it takes seconds.
@test "firewall-start: restores mangle, then queues the apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    wait_for_merlin_applies 1
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output $'restore mangle\n--wait apply'
}

@test "wan-event: re-applies when a WAN connection comes up, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/wan-event" 0 connected 3>&-
    assert_success
    wait_for_merlin_applies 1
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "--wait apply"
}

@test "wan-event: leaves the other WAN events alone" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/wan-event" 0 disconnected 3>&-
    assert_success
    assert_no_merlin_apply
    [ ! -e "$BATS_TEST_TMPDIR/apply.log" ]
}

# The firmware rebuilds nat on its own - a WAN coming up, a port forward
# changed - and the rebuild takes the MASQUERADE of every interface tunnel.
@test "nat-start: re-applies, queued behind a running apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/nat-start" 3>&-
    assert_success
    wait_for_merlin_applies 1
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_line "--wait apply"
}

@test "nat-start: restores nat, then queues the apply" {
    setup_merlin_hook_env
    run_hook "$MERLIN_HOOKS/nat-start" 3>&-
    assert_success
    wait_for_merlin_applies 1
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output $'restore nat\n--wait apply'
}

# The apply is detached: the hook returns once the restore is done, so a
# command after ours in the shared /jffs/scripts file runs at once.
@test "Merlin hooks: return before the apply ends" {
    setup_merlin_hook_env
    printf '#!/bin/sh\nsleep 1\necho "$*" >> "%s/apply.log"\n' "$BATS_TEST_TMPDIR" > "$VPD_SCRIPT"
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "restore mangle"
    wait_for_merlin_applies 1
}

@test "Merlin hooks: are POSIX sh and executable in the tree" {
    for hook in firewall-start wan-event nat-start; do
        [ -x "$MERLIN_HOOKS/$hook" ]
        head -n1 "$MERLIN_HOOKS/$hook" | grep -qx '#!/bin/sh'
        sh -n "$MERLIN_HOOKS/$hook"
    done
}

# install.sh and the updater ship what files.manifest lists, by tag. KeeneticOS
# runs no nat-start: its netfilter.d hook already applies after a rebuild of nat.
@test "nat-start: ships to Asuswrt-Merlin only" {
    run awk '$2 == "router/opt/vpn-director/hooks/nat-start" { print $1 }' "$PROJECT_ROOT/files.manifest"
    assert_success
    assert_output "merlin"
}

# The firmware runs the shared /jffs/scripts/<hook>: #!/bin/sh, our line
# (platform_hooks_install puts it in), and whatever the owner and other add-ons
# keep there. Ours must neither end that file early nor make it wait.

# install_shared_hook <hook> <owner line> - the shared hook as
# platform_hooks_install leaves one that held the owner's line.
install_shared_hook() {
    mkdir -p "$VPD_JFFS_SCRIPTS_DIR"
    printf '#!/bin/sh\n%s\n' "$2" > "$VPD_JFFS_SCRIPTS_DIR/$1"
    chmod 755 "$VPD_JFFS_SCRIPTS_DIR/$1"
    (
        export VPD_PLATFORM=merlin
        . "$LIB_DIR/platform.sh"
        _merlin_hook_ensure "$1" >/dev/null
    )
}

@test "shared wan-event: the owner's line after ours runs on every WAN event" {
    setup_merlin_hook_env
    install_shared_hook wan-event "echo \"owner \$2\" >> \"$BATS_TEST_TMPDIR/owner.log\""
    run sh "$VPD_JFFS_SCRIPTS_DIR/wan-event" 0 disconnected 3>&-
    assert_success
    run cat "$BATS_TEST_TMPDIR/owner.log"
    assert_output "owner disconnected"
    assert_no_merlin_apply
}

@test "shared firewall-start: the owner's line after ours runs before the apply ends" {
    setup_merlin_hook_env
    printf '#!/bin/sh\nsleep 1\necho apply >> "%s/order.log"\n' "$BATS_TEST_TMPDIR" > "$VPD_SCRIPT"
    install_shared_hook firewall-start "echo owner >> \"$BATS_TEST_TMPDIR/order.log\""
    run sh "$VPD_JFFS_SCRIPTS_DIR/firewall-start" eth0 3>&-
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_output "owner"
    local i
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
        grep -q '^apply$' "$BATS_TEST_TMPDIR/order.log" && break
        sleep 0.2
    done
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_output $'owner\napply'
}

# ------------------------------------------------------------------- common

@test "hooks: exit 0 and log when the CLI is not installed" {
    setup_hook_env
    export VPD_SCRIPT="$BATS_TEST_TMPDIR/absent.sh"
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    assert_no_apply
    grep -q "vpn-director not found" "$BATS_TEST_TMPDIR/logger.log"
    # Nor is anything restored: the install it would restore for is not there.
    [ ! -e "$BATS_TEST_TMPDIR/nohup.log" ]
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
    # The restore needs no nohup, and netfilter.d still runs it.
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "restore mangle"
}

# The restore only shortens the window; the apply is what puts everything back.
# A router the restore script has not reached - or lost - still applies.
@test "hooks: a missing restore script does not stop the apply" {
    setup_hook_env
    export VPD_RESTORE="$BATS_TEST_TMPDIR/absent.sh"
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    wait_for_apply
    run cat "$BATS_TEST_TMPDIR/nohup.log"
    assert_output "$VPD_SCRIPT --wait apply"

    setup_merlin_hook_env
    export VPD_RESTORE="$BATS_TEST_TMPDIR/absent.sh"
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    run_hook "$MERLIN_HOOKS/nat-start" 3>&-
    assert_success
    wait_for_merlin_applies 2
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output $'--wait apply\n--wait apply'
}

# The restore exits 0 whatever it could not do; one that fails all the same -
# killed, a broken install - neither keeps the apply from being queued nor
# reaches the hook's output.
@test "hooks: a restore that fails does not stop the apply" {
    setup_hook_env
    printf '#!/bin/sh\necho restoring\necho "cannot restore" >&2\nexit 1\n' > "$VPD_RESTORE"
    type=iptables table=mangle run_hook "$HOOKS/netfilter.d/50-vpn-director.sh" start
    assert_success
    refute_output
    wait_for_apply

    setup_merlin_hook_env
    printf '#!/bin/sh\necho restoring\necho "cannot restore" >&2\nexit 1\n' > "$VPD_RESTORE"
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    refute_output
    run_hook "$MERLIN_HOOKS/nat-start" 3>&-
    assert_success
    refute_output
    wait_for_merlin_applies 2
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output $'--wait apply\n--wait apply'
}

@test "hooks: are POSIX sh and executable in the tree" {
    for hook in netfilter.d wan.d iflayerchanged.d; do
        [ -x "$HOOKS/$hook/50-vpn-director.sh" ]
        head -n1 "$HOOKS/$hook/50-vpn-director.sh" | grep -qx '#!/bin/sh'
        sh -n "$HOOKS/$hook/50-vpn-director.sh"
    done
}
