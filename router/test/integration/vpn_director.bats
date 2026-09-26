#!/usr/bin/env bats

# test/integration/vpn_director.bats
# Integration tests for vpn-director.sh CLI

# The platform tests below pass a flag to `run`, which bats only guarantees
# from 1.5.0; without this the suite prints a BW02 warning for each of them.
bats_require_minimum_version 1.5.0

load '../test_helper'

setup() {
    export PATH="$TEST_ROOT/mocks:$PATH"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    export LOG_FILE="/tmp/bats_test_vpn_director.log"
    export TEST_MODE=1
    # This setup() overrides the helper's, so the fixture path it exports has to
    # be repeated here: without it platform_tunnels reads the real
    # /etc/iproute2/rt_tables, which the machine running the suite need not have.
    export RT_TABLES_FILE="$TEST_ROOT/fixtures/rt_tables"
    # And the ready marker: tproxy_apply would otherwise write the machine's own
    # /tmp/xray_tproxy/ready.
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/xray_tproxy_ready"
    : > "$LOG_FILE"
}

# ============================================================================
# Help and basic CLI tests
# ============================================================================

@test "vpn-director: shows help with no args" {
    run "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output --partial "Usage:"
}

@test "vpn-director: --help shows usage" {
    run "$SCRIPTS_DIR/vpn-director.sh" --help
    assert_success
    assert_output --partial "Usage:"
    assert_output --partial "Commands:"
    assert_output --partial "Options:"
}

@test "vpn-director: -h shows usage" {
    run "$SCRIPTS_DIR/vpn-director.sh" -h
    assert_success
    assert_output --partial "Usage:"
}

@test "vpn-director: unknown command fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" unknown
    assert_failure
    assert_output --partial "Unknown command"
}

@test "vpn-director: unknown option fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" --badoption
    assert_failure
    assert_output --partial "Unknown option"
}

# ============================================================================
# Status command tests
# ============================================================================

@test "vpn-director: status command works" {
    run "$SCRIPTS_DIR/vpn-director.sh" status
    assert_success
    assert_output --partial "IPSet Status"
    assert_output --partial "Tunnel Director Status"
    assert_output --partial "Xray TPROXY Status"
}

@test "vpn-director: status ipset shows only ipset" {
    run "$SCRIPTS_DIR/vpn-director.sh" status ipset
    assert_success
    assert_output --partial "IPSet Status"
    refute_output --partial "Tunnel Director"
    refute_output --partial "Xray TPROXY"
}

@test "vpn-director: status tunnel shows only tunnel" {
    run "$SCRIPTS_DIR/vpn-director.sh" status tunnel
    assert_success
    assert_output --partial "Tunnel Director Status"
    refute_output --partial "IPSet Status"
    refute_output --partial "Xray TPROXY"
}

@test "vpn-director: status xray shows only tproxy" {
    run "$SCRIPTS_DIR/vpn-director.sh" status xray
    assert_success
    assert_output --partial "Xray TPROXY Status"
    refute_output --partial "IPSet Status"
    refute_output --partial "Tunnel Director"
}

@test "vpn-director: status tproxy alias works" {
    run "$SCRIPTS_DIR/vpn-director.sh" status tproxy
    assert_success
    assert_output --partial "Xray TPROXY Status"
}

@test "vpn-director: status unknown component fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" status badcomp
    assert_failure
    assert_output --partial "Unknown component"
}

# ============================================================================
# apply command tests
# ============================================================================

@test "vpn-director: apply --dry-run shows plan without applying" {
    run "$SCRIPTS_DIR/vpn-director.sh" apply --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
    assert_output --partial "would apply"
}

@test "vpn-director: apply tunnel --dry-run shows tunnel ipsets" {
    run "$SCRIPTS_DIR/vpn-director.sh" apply tunnel --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
    assert_output --partial "tunnel ipsets"
}

@test "vpn-director: apply xray --dry-run shows tproxy ipsets" {
    run "$SCRIPTS_DIR/vpn-director.sh" apply xray --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
    assert_output --partial "tproxy ipsets"
}

# stop leaves a marker the Telegram bot's subscription watch honours, and apply
# hands routing back to the watch by removing it. A dry run changes nothing, so
# the watch must stay paused: otherwise it could apply a failover on a router
# the user stopped.
@test "vpn-director: apply --dry-run keeps the stopped marker" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run "$SCRIPTS_DIR/vpn-director.sh" apply --dry-run
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
}

# run_stubbed_cli <arguments...> runs the command the CLI parses from its
# arguments with the lock, the boot wait and the ipsets stubbed. Each module call
# that would change routing appends its name to $BATS_TEST_TMPDIR/calls instead;
# tunnel_apply appends " forced" under TUN_DIR_FORCE_REBUILD, reports the clients
# in $TUNNEL_UNCARRIED_STUB as not carried and returns $TUNNEL_APPLY_RC;
# tproxy_prune appends the addresses it is told to keep.
run_stubbed_cli() {
    # cmd_apply and cmd_update remove the stopped marker: never the one of the
    # machine running the suite.
    export VPD_STOPPED_FILE="${VPD_STOPPED_FILE:-$BATS_TEST_TMPDIR/stopped}"
    run bash -c '
        script=$1 calls=$2
        shift 2
        source "$script" --source-only "$@"
        _load_modules
        acquire_lock() { :; }
        _ipset_boot_wait() { :; }
        _ensure_ipsets() { :; }
        tproxy_restart_process() { echo tproxy_restart_process >> "$calls"; }
        tproxy_stop() { echo tproxy_stop >> "$calls"; }
        tunnel_stop() { echo tunnel_stop >> "$calls"; }
        tproxy_apply() { echo tproxy_apply >> "$calls"; }
        tunnel_apply() {
            echo "tunnel_apply${TUN_DIR_FORCE_REBUILD:+ forced}" >> "$calls"
            read -ra TUNNEL_UNCARRIED <<< "${TUNNEL_UNCARRIED_STUB:-}"
            return "${TUNNEL_APPLY_RC:-0}"
        }
        tproxy_prune() { echo "tproxy_prune${*:+ $*}" >> "$calls"; }
        "cmd_$COMMAND"
    ' -- "$SCRIPTS_DIR/vpn-director.sh" "$BATS_TEST_TMPDIR/calls" "$@"
}

@test "vpn-director: apply removes the stopped marker" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli apply
    assert_success
    [ ! -e "$VPD_STOPPED_FILE" ]
}

@test "vpn-director: restart removes the stopped marker" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli restart
    assert_success
    [ ! -e "$VPD_STOPPED_FILE" ]
}

# The subscription watch applies with --unless-stopped. A stop that took the lock
# ahead of it - or finished while the watch was still probing - must survive: the
# check runs under the lock, where the marker that stop left is visible.
@test "vpn-director: apply --unless-stopped leaves a stopped router stopped" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli --unless-stopped apply
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

@test "vpn-director: apply --unless-stopped applies a running router" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    run_stubbed_cli --unless-stopped apply
    assert_success
    grep -qx tproxy_apply "$BATS_TEST_TMPDIR/calls"
    grep -qx tunnel_apply "$BATS_TEST_TMPDIR/calls"
}

@test "vpn-director: restart xray --unless-stopped leaves a stopped router stopped" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli --unless-stopped restart xray
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

# Turning one component back on - a Web UI server switch restarts xray - does
# not undo the stop of the rest. With the marker gone the watch would read the
# router as running, and its next failover apply would bring everything back.
@test "vpn-director: apply or restart of one component keeps the stopped marker" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli apply tunnel
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
    run_stubbed_cli restart xray
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
}

# The watch checks the marker under the config lock before every write it
# makes. A stop that only wrote it after the teardown left that whole teardown
# as a window in which a watch write still went through - onto a router about
# to be stopped, taking effect on its next manual apply.
@test "vpn-director: stop leaves its marker before it tears anything down" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    run bash -c '
        script=$1 calls=$2
        shift 2
        source "$script" --source-only "$@"
        _load_modules
        acquire_lock() { :; }
        seen() { if [[ -e $VPD_STOPPED_FILE ]]; then echo "$1 after the marker"; else echo "$1"; fi; }
        tproxy_stop() { seen tproxy_stop >> "$calls"; }
        tunnel_stop() { seen tunnel_stop >> "$calls"; }
        "cmd_$COMMAND"
    ' -- "$SCRIPTS_DIR/vpn-director.sh" "$BATS_TEST_TMPDIR/calls" stop
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_stop after the marker\ntunnel_stop after the marker'
}

# The subscription watch writes config.json for each server it tries and needs
# only the process to pick it up. "restart xray" would apply the TPROXY rules
# again after each one for nothing.
@test "vpn-director: restart xray-process restarts the Xray process and nothing else" {
    run_stubbed_cli restart xray-process
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output "tproxy_restart_process"
}

@test "vpn-director: restart xray-process --unless-stopped leaves a stopped router alone" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    printf '1\n' > "$VPD_STOPPED_FILE"
    run_stubbed_cli --unless-stopped restart xray-process
    assert_success
    [ -f "$VPD_STOPPED_FILE" ]
    [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}

# A restart stops nothing, so it leaves no marker of its own: the apply it runs
# removes the one a stop left, as an apply does.
@test "vpn-director: restart --unless-stopped re-applies a running router" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    run_stubbed_cli --unless-stopped restart
    assert_success
    grep -qx tproxy_apply "$BATS_TEST_TMPDIR/calls"
    grep -qx "tunnel_apply forced" "$BATS_TEST_TMPDIR/calls"
    [ ! -e "$VPD_STOPPED_FILE" ]
}

# Make before break: a client moving from Xray to a tunnel stays proxied until
# Tunnel Director carries it, so the prune that lets it go runs last.
@test "vpn-director: a full apply adds for Xray, applies Tunnel Director, then prunes" {
    run_stubbed_cli apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune'
}

@test "vpn-director: update applies in the same order" {
    run_stubbed_cli update
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune'
}

# apply xray runs no Tunnel Director, so the prune keeps every client the config
# puts on a tunnel - the fixture's wgc1 holds 192.168.50.0/24 - while Xray has it.
@test "vpn-director: apply xray adds and prunes, apply tunnel applies Tunnel Director alone" {
    run_stubbed_cli apply xray
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntproxy_prune 192.168.50.0/24'
    rm -f "$BATS_TEST_TMPDIR/calls"
    run_stubbed_cli apply tunnel
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output 'tunnel_apply'
}

# tunnel_apply hard-fails under errexit. The run ends there, before the prune:
# the clients that left Xray stay proxied rather than leave through the WAN.
@test "vpn-director: a Tunnel Director failure ends the apply before the prune" {
    export TUNNEL_APPLY_RC=1
    run_stubbed_cli apply
    assert_failure
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply'
}

# C1: tunnel_apply soft-fails with 0 - a refused rule, a tunnel the platform
# does not list - and reports the clients the live TUN_DIR does not carry. The
# prune is told to keep each of them: every full apply, update and restart.
@test "vpn-director: a full apply hands the prune what Tunnel Director did not carry" {
    export TUNNEL_UNCARRIED_STUB='192.168.1.100 10.0.0.0/24'
    run_stubbed_cli apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune 192.168.1.100 10.0.0.0/24'
    rm -f "$BATS_TEST_TMPDIR/calls"
    run_stubbed_cli update
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune 192.168.1.100 10.0.0.0/24'
    rm -f "$BATS_TEST_TMPDIR/calls"
    run_stubbed_cli restart
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_restart_process\ntproxy_apply\ntunnel_apply forced\ntproxy_prune 192.168.1.100 10.0.0.0/24'
}

# The clients of every tunnel key, main included, less paused_clients: the ones
# tunnel_apply would carry. config.sh subtracts the paused ones.
@test "vpn-director: apply xray keeps every effective Tunnel Director client, main included" {
    local cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    jq '.tunnel_director.tunnels.wgc1.clients += ["192.168.50.9"]
        | .tunnel_director.tunnels.main = {"clients": ["192.168.50.20"]}
        | .paused_clients = ["192.168.50.9"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$cfg"
    export VPD_CONFIG_FILE="$cfg"
    run_stubbed_cli apply xray
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntproxy_prune 192.168.50.0/24 192.168.50.20'
}

# ============================================================================
# Option parsing (both positions)
# ============================================================================

@test "vpn-director: options work before command" {
    run "$SCRIPTS_DIR/vpn-director.sh" --dry-run apply
    assert_success
    assert_output --partial "DRY-RUN"
}

@test "vpn-director: options work after command" {
    run "$SCRIPTS_DIR/vpn-director.sh" apply --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
}

@test "vpn-director: options work after component" {
    run "$SCRIPTS_DIR/vpn-director.sh" apply tunnel --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
}

@test "vpn-director: multiple options work together" {
    run "$SCRIPTS_DIR/vpn-director.sh" --force apply --dry-run
    assert_success
    assert_output --partial "DRY-RUN"
}

@test "vpn-director: component argument parsed correctly" {
    run "$SCRIPTS_DIR/vpn-director.sh" status tunnel
    assert_success
    assert_output --partial "Tunnel Director Status"
    refute_output --partial "IPSet Status"
}

# ============================================================================
# stop command tests
# ============================================================================

@test "vpn-director: stop unknown component fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" stop badcomp
    assert_failure
    assert_output --partial "Unknown component"
}

# ============================================================================
# restart command tests
# ============================================================================

@test "vpn-director: restart unknown component fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" restart badcomp
    assert_failure
    assert_output --partial "Unknown component"
}

# ============================================================================
# Verbose mode tests
# ============================================================================

@test "vpn-director: -v enables verbose mode" {
    run "$SCRIPTS_DIR/vpn-director.sh" -v --help
    assert_success
    assert_output --partial "Usage:"
}

@test "vpn-director: --verbose enables verbose mode" {
    run "$SCRIPTS_DIR/vpn-director.sh" --verbose --help
    assert_success
    assert_output --partial "Usage:"
}

# ============================================================================
# --source-only mode tests
# ============================================================================

@test "vpn-director: --source-only loads functions without executing" {
    run bash -c 'source "$1" --source-only && echo "sourced ok"' -- "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output "sourced ok"
}

@test "vpn-director: --source-only exports cmd functions" {
    run bash -c 'source "$1" --source-only && type cmd_status' -- "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output --partial "function"
}

@test "vpn-director: --source-only exports cmd_apply function" {
    run bash -c 'source "$1" --source-only && type cmd_apply' -- "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output --partial "function"
}

# ============================================================================
# Quiet mode tests
# ============================================================================

@test "vpn-director: -q option is parsed" {
    run "$SCRIPTS_DIR/vpn-director.sh" -q --help
    assert_success
    assert_output --partial "Usage:"
}

@test "vpn-director: --quiet option is parsed" {
    run "$SCRIPTS_DIR/vpn-director.sh" --quiet --help
    assert_success
    assert_output --partial "Usage:"
}

# ============================================================================
# Force option tests
# ============================================================================

@test "vpn-director: -f option is parsed" {
    run "$SCRIPTS_DIR/vpn-director.sh" -f --help
    assert_success
    assert_output --partial "Usage:"
}

@test "vpn-director: --force option is parsed" {
    run "$SCRIPTS_DIR/vpn-director.sh" --force --help
    assert_success
    assert_output --partial "Usage:"
}

# ============================================================================
# --wait option
# ============================================================================

# A queued apply must work from the config as it is when the lock is granted.
# Reading it first applies a snapshot the waiting time has already made stale,
# and two queued applies can then commit in either order while both report
# success. The test holds the lock on FD 201, edits the config while the apply
# waits, and asserts the plan reflects the edit.
@test "vpn-director: a waiting apply reads the config it was granted" {
    local cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    jq '.tunnel_director.tunnels.wgc1.exclude = ["de"]' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$cfg"

    exec 201>"/var/lock/vpn-director.lock"
    flock -n 201

    VPD_CONFIG_FILE="$cfg" "$SCRIPTS_DIR/vpn-director.sh" --wait=20 apply tunnel --dry-run \
        > "$BATS_TEST_TMPDIR/out" 2>&1 &
    local waiter=$!

    # Let it reach the lock, then supersede the config it would have read.
    sleep 2
    jq '.tunnel_director.tunnels.wgc1.exclude = ["fr"]' "$cfg" > "$cfg.new"
    mv "$cfg.new" "$cfg"
    flock -u 201
    wait "$waiter"

    run cat "$BATS_TEST_TMPDIR/out"
    assert_output --partial "ipsets needed: fr"
    refute_output --partial "ipsets needed: de"
}

@test "vpn-director: --wait exports VPD_LOCK_WAIT=120 by default" {
    run bash -c 'source "$1" --source-only --wait apply && echo "wait=$VPD_LOCK_WAIT cmd=$COMMAND"' -- "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output "wait=120 cmd=apply"
}

@test "vpn-director: --wait=SEC exports the given number of seconds" {
    run bash -c 'source "$1" --source-only --wait=30 apply && echo "wait=$VPD_LOCK_WAIT"' -- "$SCRIPTS_DIR/vpn-director.sh"
    assert_success
    assert_output "wait=30"
}

@test "vpn-director: --wait with a non-numeric value fails" {
    run "$SCRIPTS_DIR/vpn-director.sh" --wait=abc apply
    assert_failure
    assert_output --partial "Invalid --wait value"
}

@test "vpn-director: --help documents --wait" {
    run "$SCRIPTS_DIR/vpn-director.sh" --help
    assert_success
    assert_output --partial "--wait"
}

# ============================================================================
# platform subcommand
# ============================================================================

# log() writes to stderr, which bats folds into $output unless the streams are
# kept apart. --separate-stderr leaves $output as stdout alone, so a WARN about
# one tunnel could never break jq's parse of the document.

@test "vpn-director: platform prints the platform facts as JSON" {
    run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    [ "${#lines[@]}" -eq 1 ]
    echo "$output" | jq -e '.platform == "merlin"' >/dev/null
    echo "$output" | jq -e '.password_file == "/etc/shadow"' >/dev/null
    echo "$output" | jq -e '.lan_ifaces == ["br0"]' >/dev/null
    echo "$output" | jq -e '.wan_if == "eth0"' >/dev/null
    echo "$output" | jq -e '.arch | length > 0' >/dev/null
}

@test "vpn-director: platform lists every tunnel except main" {
    run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    [ "${#lines[@]}" -eq 1 ]
    echo "$output" | jq -e '[.tunnels[].id] == ["wgc1","wgc2","ovpnc1","ovpnc2"]' >/dev/null
    echo "$output" | jq -e '.tunnels[0] == {id:"wgc1", iface:"wgc1", type:"wireguard", connected:false, description:"Office WG"}' >/dev/null
    echo "$output" | jq -e '.tunnels[2] == {id:"ovpnc1", iface:"tun11", type:"openvpn", connected:false, description:"Office OVPN"}' >/dev/null
}

@test "vpn-director: platform reports wan_if as empty when the platform has no answer" {
    mkdir -p "$BATS_TEST_TMPDIR/mock"
    printf '#!/bin/bash\necho ""\n' > "$BATS_TEST_TMPDIR/mock/nvram"
    chmod +x "$BATS_TEST_TMPDIR/mock/nvram"
    PATH="$BATS_TEST_TMPDIR/mock:$PATH" run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    echo "$output" | jq -e '.wan_if == ""' >/dev/null
}

@test "vpn-director: platform on Keenetic lists NDM's tunnels with their Linux interfaces" {
    VPD_PLATFORM=keenetic PATH="$TEST_ROOT/mocks/keenetic:$PATH" \
        run --separate-stderr "$SCRIPTS_DIR/vpn-director.sh" platform
    assert_success
    # One line: the daemons read the last line of the combined output (C10).
    [ "${#lines[@]}" -eq 1 ]
    echo "$output" | jq -e '.platform == "keenetic"' >/dev/null
    echo "$output" | jq -e '.password_file == "/opt/etc/passwd"' >/dev/null
    echo "$output" | jq -e '.lan_ifaces == ["br0"]' >/dev/null
    echo "$output" | jq -e '.wan_if == "eth2.4"' >/dev/null
    echo "$output" | jq -e '[.tunnels[].id] == ["OpenVPN0","OpenVPN2","Wireguard1"]' >/dev/null
    echo "$output" | jq -e '.tunnels[0] == {id:"OpenVPN0", iface:"ovpn_br0", type:"openvpn", connected:true, description:"office-ovpn"}' >/dev/null
    echo "$output" | jq -e '.tunnels[1] == {id:"OpenVPN2", iface:"ovpn_br2", type:"openvpn", connected:false, description:"spare-ovpn"}' >/dev/null
    echo "$output" | jq -e '.tunnels[2] == {id:"Wireguard1", iface:"nwg1", type:"wireguard", connected:true, description:"wg-home"}' >/dev/null
}

# ============================================================================
# cron subcommand
# ============================================================================

@test "vpn-director: cron install schedules the daily update through the platform" {
    : > /tmp/bats_cru_calls.log
    run "$SCRIPTS_DIR/vpn-director.sh" cron install
    assert_success
    # The CLI resolves its own directory with pwd; SCRIPTS_DIR still contains "test/.."
    local vpd_dir
    vpd_dir="$(cd "$SCRIPTS_DIR" && pwd)"
    grep -qF "cru a vpn_director_update 0 3 * * * $vpd_dir/vpn-director.sh update" /tmp/bats_cru_calls.log
    # The job name of the versions before the unified CLI: dropped on every
    # install, or a router upgraded from one of them keeps two jobs.
    grep -qF "cru d update_ipsets" /tmp/bats_cru_calls.log
}

# cmd_cron called platform_cron_add bare under errexit: on a platform whose
# cron is not there the CLI died with the function's exit status and no
# output at all, leaving nothing to act on.
@test "vpn-director: cron install reports a platform cron that failed" {
    : > /tmp/bats_cru_calls.log
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/bash\nexit 1\n' > "$BATS_TEST_TMPDIR/bin/cru"
    chmod +x "$BATS_TEST_TMPDIR/bin/cru"
    PATH="$BATS_TEST_TMPDIR/bin:$PATH" run "$SCRIPTS_DIR/vpn-director.sh" cron install
    assert_failure
    assert_output --partial "Failed to schedule the daily ipset update"
    # Merlin requires nothing: no Entware advice on a router that has no opkg.
    refute_output --partial "opkg"
}

# The other half of the same seam: what the platform requires is the platform's
# answer, and cmd_cron is what logs it.
@test "vpn-director: cron install on Keenetic names the missing cron package" {
    VPD_PLATFORM=keenetic PATH="$TEST_ROOT/mocks/keenetic:$PATH" \
        VPD_CRON_D="$BATS_TEST_TMPDIR/cron.d" \
        VPD_CRON_INIT="$BATS_TEST_TMPDIR/no-such-S10cron" \
        run "$SCRIPTS_DIR/vpn-director.sh" cron install
    assert_failure
    assert_output --partial "Failed to schedule the daily ipset update"
    assert_output --partial "opkg install cron"
    [ -f "$BATS_TEST_TMPDIR/cron.d/vpn_director_update" ]
}

@test "vpn-director: cron remove drops the job" {
    : > /tmp/bats_cru_calls.log
    run "$SCRIPTS_DIR/vpn-director.sh" cron remove
    assert_success
    grep -qF "cru d vpn_director_update" /tmp/bats_cru_calls.log
    grep -qF "cru d update_ipsets" /tmp/bats_cru_calls.log
}

@test "vpn-director: cron without install|remove fails with usage" {
    run "$SCRIPTS_DIR/vpn-director.sh" cron
    assert_failure
    assert_output --partial "cron install|remove"
}

@test "vpn-director: help lists platform and cron" {
    run "$SCRIPTS_DIR/vpn-director.sh" --help
    assert_output --partial "platform"
    assert_output --partial "cron install|remove"
}

# A restart stops nothing: every apply swaps its chains and sets in whole, where
# a stop took the routing away until the apply put it back.
@test "vpn-director: restart rebuilds in place and stops nothing" {
    run_stubbed_cli restart
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_restart_process\ntproxy_apply\ntunnel_apply forced\ntproxy_prune'
}

# A server switch (the Web UI, /xray, the wizard) runs "restart xray". The TPROXY
# rules stay in place while the process restarts: its clients wait for it
# instead of leaving through the WAN.
@test "vpn-director: restart xray restarts the process and re-applies Xray without a stop" {
    run_stubbed_cli restart xray
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_restart_process\ntproxy_apply\ntproxy_prune 192.168.50.0/24'
}

@test "vpn-director: restart tunnel rebuilds Tunnel Director in place" {
    run_stubbed_cli restart tunnel
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output 'tunnel_apply forced'
}

# ============================================================================
# A component command after a move made by hand (I2)
# ============================================================================

# run_cli <arguments...> runs the command the CLI parses from its arguments with
# the real modules, on the stateful mocks the test turned on; only the lock, the
# boot wait and the download of the country sets are stubbed.
run_cli() {
    export VPD_STOPPED_FILE="${VPD_STOPPED_FILE:-$BATS_TEST_TMPDIR/stopped}"
    export XRAY_INIT_DIR="$BATS_TEST_TMPDIR/init.d"
    mkdir -p "$XRAY_INIT_DIR"
    run bash -c '
        script=$1
        shift
        source "$script" --source-only "$@"
        _load_modules
        acquire_lock() { :; }
        _ipset_boot_wait() { :; }
        _ensure_ipsets() { :; }
        "cmd_$COMMAND"
    ' -- "$SCRIPTS_DIR/vpn-director.sh" "$@"
}

# applied_then_moved_by_hand - 192.168.1.100 and 192.168.1.101 on Xray, applied
# in full; then, by hand in vpn-director.json, 192.168.1.100 moved to wgc1 and
# 192.168.1.101 deleted.
applied_then_moved_by_hand() {
    use_stateful_iptables
    use_stateful_ipset
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    jq '.xray.clients = ["192.168.1.100", "192.168.1.101"]' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$VPD_CONFIG_FILE"
    run_cli apply
    assert_success
    ipset test XRAY_CLIENTS 192.168.1.100 2>/dev/null
    ipset test XRAY_CLIENTS 192.168.1.101 2>/dev/null
    jq '.xray.clients = [] | .tunnel_director.tunnels.wgc1.clients += ["192.168.1.100"]' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$VPD_CONFIG_FILE"
}

# apply xray runs no Tunnel Director, so nothing marks the moved client yet: its
# prune let it go to the WAN until the next full apply. It now keeps every live
# member the config puts on a tunnel and lets go only of the client that left
# Xray for direct. The full apply that marks the moved client lets it go.
@test "vpn-director: apply xray keeps a client moved to a tunnel proxied and lets a deleted one go" {
    applied_then_moved_by_hand

    run_cli apply xray
    assert_success
    assert_output --partial "Kept in XRAY_CLIENTS: 192.168.1.100 - they stay proxied"
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
    run ipset test XRAY_CLIENTS 192.168.1.101
    assert_failure

    run_cli apply
    assert_success
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.100 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_failure
}

# The server switch of the Web UI and /xray runs exactly this.
@test "vpn-director: restart xray keeps a client moved to a tunnel proxied and lets a deleted one go" {
    applied_then_moved_by_hand

    run_cli restart xray
    assert_success
    assert_output --partial "Kept in XRAY_CLIENTS: 192.168.1.100 - they stay proxied"
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
    run ipset test XRAY_CLIENTS 192.168.1.101
    assert_failure
}
