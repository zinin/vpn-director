#!/usr/bin/env bats

load '../test_helper'

# configure.sh is sourced with --source-only, so main() never runs. VPD_DIR must
# point at the repository copy while sourcing (the script loads lib/xrayconf.sh
# from it) and is redirected into BATS_TEST_TMPDIR afterwards.
load_wizard() {
    VPD_DIR="$SCRIPTS_DIR"
    source "$SCRIPTS_DIR/configure.sh" --source-only

    VPD_DIR="$BATS_TEST_TMPDIR/vpn-director"
    XRAY_CONFIG_DIR="$BATS_TEST_TMPDIR/xray"
    mkdir -p "$VPD_DIR" "$XRAY_CONFIG_DIR"
    cp "$SCRIPTS_DIR/vpn-director.json.template" "$VPD_DIR/vpn-director.json.template"
    cp "$PROJECT_ROOT/opt/etc/xray/config.json.template" "$XRAY_CONFIG_DIR/config.json.template"

    # The answers the wizard collected in steps 1-3.
    SELECTED_SERVER_JSON='{"address":"1.2.3.4","port":443,"uuid":"u1","security":"reality","network":"tcp","flow":"xtls-rprx-vision","sni":"cdn.example.com","fingerprint":"firefox","public_key":"PBK","short_id":"sid1"}'
    XRAY_CLIENTS_LIST="192.168.50.10"
    XRAY_EXCLUDE_SETS_LIST="ru"
    TUN_DIR_TUNNELS_JSON='{"wgc1":{"clients":["192.168.50.20"],"exclude":["ru"]}}'
    SERVERS_FILE="$BATS_TEST_TMPDIR/servers.json"
    printf '%s' '[{"address":"1.2.3.4","ips":["1.2.3.4"]}]' > "$SERVERS_FILE"
}

# write_daemon_config stands in for the config as the daemons left it: a
# generated jwt_secret, a paused client, an excluded IP, and stale wizard fields.
write_daemon_config() {
    jq '.webui.jwt_secret = "secret-from-the-daemon" |
        .webui.log_level = "debug" |
        .paused_clients = ["192.168.50.30"] |
        .xray.exclude_ips = ["203.0.113.7"] |
        .xray.clients = ["10.0.0.9"] |
        .xray.exclude_sets = ["de"] |
        .tunnel_director.tunnels = {"ovpnc1": {"clients": ["10.0.0.9"], "exclude": []}}' \
        "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"
}

@test "step_generate_configs: keeps the jwt_secret the Web UI generated" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -r '.webui.jwt_secret' "$VPD_DIR/vpn-director.json"
    assert_output "secret-from-the-daemon"
}

@test "step_generate_configs: keeps the rest of the webui section" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -r '.webui.log_level' "$VPD_DIR/vpn-director.json"
    assert_output "debug"
}

@test "step_generate_configs: keeps paused_clients and exclude_ips" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -c '[.paused_clients, .xray.exclude_ips]' "$VPD_DIR/vpn-director.json"
    assert_output '[["192.168.50.30"],["203.0.113.7"]]'
}

@test "step_generate_configs: still overwrites the fields the wizard owns" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -c '[.xray.clients, .xray.exclude_sets, (.tunnel_director.tunnels | keys)]' \
        "$VPD_DIR/vpn-director.json"
    assert_output '[["192.168.50.10"],["ru"],["wgc1"]]'
}

# C6: a hand-set gateway (Keenetic OpenVPN topology p2p) lives on the on-disk
# tunnel object. The wizard rebuilds tunnels as {clients, exclude} only and
# used to replace the whole object, dropping gateway on every save.
@test "step_generate_configs: keeps an existing tunnel gateway" {
    load_wizard
    write_daemon_config
    jq '.tunnel_director.tunnels.wgc1 = {"clients":["192.168.50.99"],"exclude":["de"],"gateway":"10.73.149.1"}' \
        "$VPD_DIR/vpn-director.json" > "$VPD_DIR/vpn-director.json.new"
    mv "$VPD_DIR/vpn-director.json.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run jq -r '.tunnel_director.tunnels.wgc1.gateway' "$VPD_DIR/vpn-director.json"
    assert_output "10.73.149.1"
    run jq -c '.tunnel_director.tunnels.wgc1.clients' "$VPD_DIR/vpn-director.json"
    assert_output '["192.168.50.20"]'
}

# A string in place of the tunnel object is a shape tunnel_apply tolerates with a
# WARN, so the gateway lookup must not error on it and fail the whole save.
@test "step_generate_configs: survives a string in place of a tunnel object" {
    load_wizard
    write_daemon_config
    jq '.tunnel_director.tunnels.wgc1 = "oops"' \
        "$VPD_DIR/vpn-director.json" > "$VPD_DIR/vpn-director.json.new"
    mv "$VPD_DIR/vpn-director.json.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run jq -c '[.tunnel_director.tunnels.wgc1.clients, .tunnel_director.tunnels.wgc1.exclude]' \
        "$VPD_DIR/vpn-director.json"
    assert_output '[["192.168.50.20"],["ru"]]'
    run jq -e '.tunnel_director.tunnels.wgc1 | has("gateway")' "$VPD_DIR/vpn-director.json"
    assert_failure
}

# `//` replaces only null and false, so an array (or a string, or a number) in
# place of the tunnels object reached `$existing[.key]` and jq exited 5: the
# whole save failed and the answers were lost, where master overwrote the bad
# value. The daemons cannot load such a file at all, so the SSH wizard is the
# repair path.
@test "step_generate_configs: survives an array in place of the tunnels object" {
    load_wizard
    write_daemon_config
    jq '.tunnel_director.tunnels = []' \
        "$VPD_DIR/vpn-director.json" > "$VPD_DIR/vpn-director.json.new"
    mv "$VPD_DIR/vpn-director.json.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run jq -c '.tunnel_director.tunnels.wgc1.clients' "$VPD_DIR/vpn-director.json"
    assert_output '["192.168.50.20"]'
}

# A tunnel the wizard just created has no on-disk gateway to copy. Writing
# "gateway": "" would be worse than omitting the key.
@test "step_generate_configs: a new tunnel has no gateway key" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -e '.tunnel_director.tunnels.wgc1 | has("gateway")' "$VPD_DIR/vpn-director.json"
    assert_failure
}

@test "step_generate_configs: falls back to template defaults on a first run" {
    load_wizard

    run step_generate_configs

    assert_success
    run jq -r '[.webui.jwt_secret, (.paused_clients // "absent" | tostring)] | join("|")' \
        "$VPD_DIR/vpn-director.json"
    assert_output "|absent"
}

@test "step_generate_configs: writes the config with mode 600" {
    load_wizard
    write_daemon_config
    chmod 644 "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run stat -c '%a' "$VPD_DIR/vpn-director.json"
    assert_output "600"
}

@test "step_generate_configs: keeps data_dir and the advanced section" {
    load_wizard
    write_daemon_config
    jq '.data_dir = "/tmp/moved-storage" | .advanced.xray.tproxy_port = 23456' \
        "$VPD_DIR/vpn-director.json" > "$VPD_DIR/vpn-director.json.new"
    mv "$VPD_DIR/vpn-director.json.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run jq -r '[.data_dir, (.advanced.xray.tproxy_port | tostring)] | join("|")' \
        "$VPD_DIR/vpn-director.json"
    assert_output "/tmp/moved-storage|23456"
}

@test "step_generate_configs: the Xray inbound follows the preserved tproxy_port" {
    load_wizard
    write_daemon_config
    jq '.advanced.xray.tproxy_port = 23456' "$VPD_DIR/vpn-director.json" > "$VPD_DIR/cfg.new"
    mv "$VPD_DIR/cfg.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    # The TPROXY rules send traffic to advanced.xray.tproxy_port, so Xray has
    # to listen there; the template default would leave that port unserved.
    run jq -r '.inbounds[] | select(.tag == "tproxy-in") | .port' "$XRAY_CONFIG_DIR/config.json"
    assert_output "23456"
    run jq -r '.advanced.xray.tproxy_port' "$VPD_DIR/vpn-director.json"
    assert_output "23456"
}

@test "step_generate_configs: picks up a key an update added to the template" {
    load_wizard
    write_daemon_config
    jq '. + {"new_setting": "from-the-template"}' \
        "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/tpl.new"
    mv "$VPD_DIR/tpl.new" "$VPD_DIR/vpn-director.json.template"

    run step_generate_configs

    assert_success
    run jq -r '.new_setting' "$VPD_DIR/vpn-director.json"
    assert_output "from-the-template"
}

@test "step_generate_configs: refuses while another writer holds the config lock" {
    load_wizard
    write_daemon_config
    flock "$VPD_DIR/.vpn-director.json.lock" sleep 30 &
    local holder=$!
    sleep 0.5

    VPD_CONFIG_LOCK_WAIT=1
    run step_generate_configs
    kill "$holder" 2>/dev/null || true

    assert_failure
    assert_output --partial "Config is locked"
    run jq -r '.xray.clients[0]' "$VPD_DIR/vpn-director.json"
    assert_output "10.0.0.9"
}

@test "step_generate_configs: releases the config lock when it is done" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run flock -n "$VPD_DIR/.vpn-director.json.lock" true
    assert_success
}

@test "step_generate_configs: a broken template leaves the existing config alone" {
    load_wizard
    write_daemon_config
    printf '%s' 'not json' > "$VPD_DIR/vpn-director.json.template"

    run step_generate_configs

    assert_failure
    run jq -r '.webui.jwt_secret' "$VPD_DIR/vpn-director.json"
    assert_output "secret-from-the-daemon"
    [[ -z "$(find "$VPD_DIR" -name 'vpn-director.json.??????')" ]]
}

@test "step_generate_configs: records the server the Xray config was built from" {
    load_wizard
    write_daemon_config
    SELECTED_SERVER_JSON='{"name":"Осло, Норвегия, Extra","address":"1.2.3.4","port":443,"uuid":"u1","security":"reality","network":"tcp","sni":"cdn.example.com","fingerprint":"firefox","public_key":"PBK","short_id":"sid1"}'

    run step_generate_configs

    assert_success
    run jq -r '.xray.active_server | "\(.name)|\(.address)|\(.port)"' "$VPD_DIR/vpn-director.json"
    assert_output "Осло, Норвегия, Extra|1.2.3.4|443"
}

# The Web UI serves this file over /api/config. The record names the server and
# stops there; the subscription UUID and the REALITY material stay in
# servers.json and in the Xray config, which nobody hands to a browser.
@test "step_generate_configs: keeps credentials out of the recorded server" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -r '.xray.active_server | keys | join(",")' "$VPD_DIR/vpn-director.json"
    assert_output "address,name,port"
}

# ============================================================================
# The tunnel prompt lists what the platform has (Merlin here: rt_tables fixture)
# ============================================================================

@test "wizard_tunnel_choices: one numbered line per platform tunnel, main excluded" {
    load_wizard
    run wizard_tunnel_choices
    assert_success
    assert_line --index 0 "  1) wgc1  Office WG (down)"
    assert_line --index 1 "  2) wgc2 (down)"
    assert_line --index 2 "  3) ovpnc1  Office OVPN (down)"
    assert_line --index 3 "  4) ovpnc2 (down)"
    [ "${#lines[@]}" -eq 4 ]
}

@test "wizard_tunnel_by_number: the id at that position, nothing for the rest" {
    load_wizard
    run wizard_tunnel_by_number 3
    assert_output "ovpnc1"
    run wizard_tunnel_by_number 9
    refute_output
    run wizard_tunnel_by_number x
    refute_output
    # 0 is not "the id at position 0": sed rejects line address 0 outright.
    # It has to fail like any other bad input, and quietly - step 3 runs under
    # "set -e", so a non-zero return here would take the whole wizard with it.
    run wizard_tunnel_by_number 0
    assert_success
    refute_output
}

# The two helpers must filter the platform's list identically, or the number
# the user reads off the menu picks a different tunnel than the one it names.
@test "wizard_tunnel_by_number: numbers exactly the lines wizard_tunnel_choices does" {
    load_wizard
    platform_tunnels() { printf 'wgc1\n\nmain\novpnc1\n'; }
    run wizard_tunnel_choices
    assert_line --index 1 "  2) ovpnc1  Office OVPN (down)"
    run wizard_tunnel_by_number 2
    assert_output "ovpnc1"
}

# Only the prefix used to be checked, so 192.168.1.1000 went into the config
# and then into "iptables -s", which refuses it, and 192.168.1.08 into iptables,
# which reads a leading zero as octal. Tunnel Director carries Xray clients
# during a failover, and one such address used to end the whole apply.
@test "step_configure_clients: an address iptables would refuse or misread is asked again" {
    run bash -c '
        set -euo pipefail
        VPD_DIR="$SCRIPTS_DIR"
        . "$VPD_DIR/configure.sh" --source-only
        printf "192.168.1.08\n192.168.1.1000\ndone\n" | step_configure_clients
        printf "clients=[%s]\n" "$XRAY_CLIENTS_LIST"
    '
    assert_success
    assert_output --partial "Invalid LAN IP: 192.168.1.08"
    assert_output --partial "Invalid LAN IP: 192.168.1.1000"
    assert_output --partial "clients=[]"
}

# The prompt asks again on bad input. It must never abort: an aborted step 3
# loses every answer the user gave in steps 1-3. Its own shell, because bats
# clears errexit around "run" and the wizard's "set -e" is the whole point here.
@test "step_configure_clients: a tunnel number off the list asks again, it does not abort" {
    run bash -c '
        set -euo pipefail
        VPD_DIR="$SCRIPTS_DIR"
        . "$VPD_DIR/configure.sh" --source-only
        printf "192.168.50.10\n2\n0\ndone\n" | step_configure_clients
    '
    assert_success
    assert_output --partial "Invalid tunnel number"
}
