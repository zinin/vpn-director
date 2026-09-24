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
    SUBS_JSON='[{"id":"0a1b2c3d","name":"Main","servers":[{"name":"Oslo","address":"1.2.3.4","port":443,"ips":["1.2.3.4"]}]}]'
    SELECTED_SUBSCRIPTION_ID="0a1b2c3d"
    # Where check_subscriptions found them: step 5 reads xray.servers from the
    # files there, not from SUBS_JSON.
    SUB_DIR="$VPD_DIR/data/subscriptions"
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

# write_subscriptions <subs_json> writes each subscription of the list to its
# file in SUB_DIR, as import_server_list.sh and the daemons leave them.
write_subscriptions() {
    local sub
    while IFS= read -r sub; do
        substore_write "$SUB_DIR" "$sub"
    done < <(jq -c '.[]' <<< "$1")
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

# In the test's own shell, not through run: the exit of run's subshell would
# release the lock whether the step did or not. Step 6 restarts Xray, and a
# process that inherited the lock would hold it for as long as it runs.
@test "step_generate_configs: releases the config lock when it is done" {
    load_wizard
    write_daemon_config

    step_generate_configs > /dev/null

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
    run jq -r '.xray.active_server | "\(.name)|\(.address)|\(.port)|\(.subscription)"' "$VPD_DIR/vpn-director.json"
    assert_output "Осло, Норвегия, Extra|1.2.3.4|443|0a1b2c3d"
}

# The subscription watch keeps the server the user chose while its walk has
# active_server on another one. A wizard run is a new choice, as a Web UI or
# /xray selection is: the next walk must start from it, not from the old one.
@test "step_generate_configs: ends the server choice the watch remembered" {
    load_wizard
    write_daemon_config
    jq '.xray.preferred_server = {"name":"Oslo","address":"oslo.example","port":443}' \
        "$VPD_DIR/vpn-director.json" > "$VPD_DIR/vpn-director.json.new"
    mv "$VPD_DIR/vpn-director.json.new" "$VPD_DIR/vpn-director.json"

    run step_generate_configs

    assert_success
    run jq -c '.xray | has("preferred_server")' "$VPD_DIR/vpn-director.json"
    assert_output "false"
}

# The Web UI serves this file over /api/config. The record names the server and
# its subscription and stops there; the server's UUID and the REALITY material
# stay in the subscription's file and in the Xray config, which nobody hands to
# a browser.
@test "step_generate_configs: keeps credentials out of the recorded server" {
    load_wizard
    write_daemon_config

    run step_generate_configs

    assert_success
    run jq -r '.xray.active_server | keys | join(",")' "$VPD_DIR/vpn-director.json"
    assert_output "address,name,port,subscription"
}

# An import stores the server's outbound; the wizard writes it into config.json.
@test "step_generate_configs: writes the stored outbound of the selected server" {
    load_wizard
    write_daemon_config
    SELECTED_SERVER_JSON='{"name":"Canada SS","address":"198.51.100.12","port":2030,"ips":["198.51.100.12"],"outbound":{"protocol":"shadowsocks","settings":{"servers":[{"address":"198.51.100.12","port":2030,"method":"aes-256-gcm","password":"p"}]}}}'

    run step_generate_configs

    assert_success
    run jq -c '[.outbounds[] | [.tag, .protocol]]' "$XRAY_CONFIG_DIR/config.json"
    assert_output '[["proxy-out","shadowsocks"]]'
}

# A config Xray rejects would take every Xray client offline at the next
# restart; the one that runs stays.
@test "step_generate_configs: a config Xray rejects leaves config.json alone" {
    load_wizard
    write_daemon_config
    printf '%s\n' '{"outbounds":["previous"]}' > "$XRAY_CONFIG_DIR/config.json"
    export XRAY_MOCK_EXIT=23 XRAY_MOCK_OUTPUT='Failed to start: bad key'

    run step_generate_configs

    assert_failure
    assert_output --partial "xray rejected the config: Failed to start: bad key"
    run cat "$XRAY_CONFIG_DIR/config.json"
    assert_output '{"outbounds":["previous"]}'
    [[ -z "$(find "$XRAY_CONFIG_DIR" -name 'config.json.??????')" ]]
}

@test "step_select_xray_server: lists each server with its protocol" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Legacy","address":"legacy.example.com","port":443,"ips":["1.2.3.4"],"security":"reality"},
 {"name":"Old TLS","address":"old.example.com","port":443,"ips":["1.2.3.5"]},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"Canada SS","address":"ss.example.com","port":2030,"ips":["1.2.3.7"],"outbound":{"protocol":"shadowsocks"}},
 {"name":"Gaming","address":"hy.example.com","port":8443,"ips":["1.2.3.8"],"outbound":{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Legacy [vless·reality]"
    assert_output --partial "2) Old TLS [vless·tls]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) Canada SS [ss]"
    assert_output --partial "5) Gaming [hysteria2]"
}

# An outbound is stored as the subscription wrote it. jq dies on a
# streamSettings that is not an object, and the wizard dies with it under
# pipefail - with no list and nothing to select. Server.Label answers "?", and
# so it does to an outbound that names no protocol.
@test "step_select_xray_server: an outbound it cannot read is listed as ?" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Broken","address":"broken.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":"tcp"}},
 {"name":"Null","address":"null.example.com","port":443,"ips":["1.2.3.5"],"outbound":null},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"No protocol","address":"noproto.example.com","port":443,"ips":["1.2.3.7"],"outbound":{"streamSettings":{"network":"ws","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Broken [?]"
    assert_output --partial "2) Null [?]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) No protocol [?]"
}

# A name and an outbound are subscription text: an escape sequence in either
# must not reach the terminal. The wizard's own colours are escape sequences
# too, so they are switched off: any ESC left in the output came from the list.
@test "step_select_xray_server: control characters from a subscription do not reach the terminal" {
    load_wizard
    RED='' GREEN='' YELLOW='' BLUE='' NC=''
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Bad\u001b[31mName","address":"bad.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"\u001b]0;owned\u0007tls"}}}]
JSON
)

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial $'\x1b'
    assert_output --partial "1) Bad[31mName [vless·ws·]0;ownedtls]"
}

two_subscriptions='[
  {"id":"0a1b2c3d","name":"Alpha","servers":[
    {"name":"Oslo","address":"a.example.com","port":443,"ips":["192.0.2.10"]},
    {"name":"Germany-1","address":"b.example.com","port":443,"ips":["192.0.2.11"]}]},
  {"id":"1b2c3d4e","name":"Beta","servers":[
    {"name":"Germany-1","address":"198.51.100.20","port":8443,"ips":["198.51.100.20"]}]}]'

@test "step_select_xray_server: with several subscriptions it asks for the subscription first" {
    load_wizard
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    run step_select_xray_server < <(printf '2\n1\n')

    assert_success
    assert_output --partial "1) Alpha (2 servers)"
    assert_output --partial "2) Beta (1 servers)"
    assert_output --partial "Selected: Germany-1 (198.51.100.20)"
}

# Both subscriptions have a Germany-1. The server picked from Beta is Beta's, and
# Beta is the subscription step 5 records with it: the Web UI, the bot and the
# watch then find the server that runs in the subscription the user chose.
@test "step_select_xray_server: a name two subscriptions share is the chosen subscription's" {
    load_wizard
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    step_select_xray_server < <(printf '2\n1\n') > /dev/null

    assert_equal "$SELECTED_SUBSCRIPTION_ID" "1b2c3d4e"
    assert_equal "$(jq -r '"\(.address):\(.port)"' <<< "$SELECTED_SERVER_JSON")" "198.51.100.20:8443"
}

# A name is the user's text, and a "|" in it is part of the name, not the start
# of the next column.
@test "step_select_xray_server: a subscription name with a | is shown whole" {
    load_wizard
    SUBS_JSON=$(jq -c '.[1].name = "Beta | Premium"' <<< "$two_subscriptions")

    run step_select_xray_server < <(printf '2\n1\n')

    assert_success
    assert_output --partial "2) Beta | Premium (1 servers)"
}

@test "step_select_xray_server: one subscription goes straight to its servers" {
    load_wizard

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial "Select subscription"
    assert_output --partial "1) Oslo [vless·tls]"
}

@test "step_generate_configs: xray.servers covers every subscription" {
    load_wizard
    write_daemon_config
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")
    write_subscriptions "$SUBS_JSON"

    run step_generate_configs

    assert_success
    run jq -c '.xray.servers' "$VPD_DIR/vpn-director.json"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20"]'
}

# The wizard reads the subscriptions when it starts and writes the config at
# step 5, minutes later. A refresh in between - by the watch, the Web UI or the
# bot - has written its addresses to xray.servers, and the list the wizard
# started with must not take them back out of TPROXY_BYPASS.
@test "step_generate_configs: xray.servers comes from the files as step 5 finds them" {
    load_wizard
    write_daemon_config
    # Main as a refresh left it while the wizard was open: Oslo has moved.
    write_subscriptions '[{"id":"0a1b2c3d","name":"Main","servers":[{"name":"Oslo","address":"192.0.2.44","port":443,"ips":["192.0.2.44"]}]}]'

    run step_generate_configs

    assert_success
    run jq -c '.xray.servers' "$VPD_DIR/vpn-director.json"
    assert_output '["192.0.2.44"]'
}

@test "check_subscriptions: reads the shared fixtures" {
    load_wizard
    mkdir -p "$VPD_DIR/data/subscriptions"
    cp "$PROJECT_ROOT/../testdata/substore/"*.json "$VPD_DIR/data/subscriptions/"
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"
    SUB_DIR=""

    check_subscriptions > /dev/null

    # Step 5 reads xray.servers from there.
    assert_equal "$SUB_DIR" "$VPD_DIR/data/subscriptions"
    [[ $(jq -c '[.[].name]' <<< "$SUBS_JSON") == '["Beta","Alpha","Gamma"]' ]]
}

# A subscription can list no server: a file made by hand, say, or one whose list
# was never set, which the daemons write as "servers": null. Offered, it would
# ask for a server in [1-0] for ever.
@test "check_subscriptions: a subscription without servers is not offered" {
    load_wizard
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"
    write_subscriptions '[{"id":"3c4d5e6f","name":"Empty","servers":null}]'

    run check_subscriptions

    assert_failure
    assert_output --partial "No subscription with servers"
}

@test "check_subscriptions: no subscription sends the user to the import" {
    load_wizard
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"

    run check_subscriptions

    assert_failure
    assert_output --partial "Run import_server_list.sh first"
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
