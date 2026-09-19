#!/usr/bin/env bats

load 'test_helper'

# The decoding itself - every scheme, container and skip reason - is
# router/test/unit/subscription.bats, on the cases the Go importer shares.
# This file is the script around it: resolution, the report and publication.

# ============================================================================
# step_parse_servers: resolution and the list it leaves for publication
# ============================================================================

# decode_into_result reads a subscription the way step_get_subscription does.
decode_into_result() {
    SUB_RESULT=$(printf '%s' "$1" | subscription_decode)
}

# step_parse_servers leaves the list in $SERVERS_TMP for step_publish_servers,
# so these call it directly: "run" would keep the variable in its subshell.
@test "step_parse_servers: every server keeps its outbound and gets its IPs" {
    load_import_server_list
    VPD_CONFIG="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{"data_dir":"%s"}' "$BATS_TEST_TMPDIR/data" > "$VPD_CONFIG"
    decode_into_result 'vless://test-uuid@example.com:443?security=reality&flow=xtls-rprx-vision&sni=cdn.example.com&pbk=PBK&fp=firefox&sid=sid1#TestServer'

    step_parse_servers

    run jq -c '.[0] | [.name, .address, .port, .ips]' "$SERVERS_TMP"
    assert_output '["TestServer","example.com",443,["93.184.216.34"]]'
    run jq -c '.[0].outbound.streamSettings.realitySettings | [.publicKey, .shortId, .serverName]' "$SERVERS_TMP"
    assert_output '["PBK","sid1","cdn.example.com"]'
    run jq -c '.[0] | [has("uuid"), has("security"), has("ip")]' "$SERVERS_TMP"
    assert_output '[false,false,false]'
}

@test "step_parse_servers: a server whose host does not resolve is left out and counted" {
    load_import_server_list
    VPD_CONFIG="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{"data_dir":"%s"}' "$BATS_TEST_TMPDIR/data" > "$VPD_CONFIG"
    decode_into_result $'vless://u@nx.example.com:443#Gone\nvless://u@5.6.7.8:443#Good\nvless://u@[2001:db8::1]:443#Six\ntuic://u:p@5.6.7.9:443#TUIC'

    step_parse_servers 2>"$BATS_TEST_TMPDIR/err"

    run jq -c '[.[].name]' "$SERVERS_TMP"
    assert_output '["Good"]'
    run cat "$LOG_FILE"
    assert_output --partial "Found 1 servers in 4 entries (1 unsupported, 2 DNS errors)"
}

@test "step_get_subscription: logs every skipped entry with its reason" {
    load_import_server_list
    printf '%s\n' 'vless://u@5.6.7.8:443?type=kcp#KCP' 'vless://u@5.6.7.9:443#Good' > "$BATS_TEST_TMPDIR/list.txt"

    step_get_subscription <<< "$BATS_TEST_TMPDIR/list.txt"

    run cat "$LOG_FILE"
    assert_output --partial "Skipping KCP: unsupported (transport kcp)"
    run jq -c '[.servers[].name]' <<< "$SUB_RESULT"
    assert_output '["Good"]'
}

# ============================================================================
# Publishing: servers.json, xray.servers and the link a refresh fetches
# ============================================================================

# load_import_into points VPD_DIR and VPD_CONFIG at a scratch directory, the way
# a caller that overrides them does, and sources the importer there.
load_import_into() {
    export VPD_DIR="$BATS_TEST_TMPDIR/vpn-director"
    export VPD_CONFIG="$VPD_DIR/vpn-director.json"
    mkdir -p "$VPD_DIR"
    load_import_server_list
}

# write_imported_state stands in for a router with an earlier import: its list,
# the bypass set built from it and the link the Web UI saved it from.
write_imported_state() {
    mkdir -p "$VPD_DIR/data"
    printf '%s\n' '[{"address":"9.9.9.9","port":443,"uuid":"old","name":"Old","ips":["9.9.9.9"]}]' \
        > "$VPD_DIR/data/servers.json"
    jq -n --arg data "$VPD_DIR/data" '{
        data_dir: $data,
        webui: {jwt_secret: "secret"},
        xray: {clients: ["192.168.50.10"], servers: ["9.9.9.9"], subscription_url: "https://old.example/s/a"}
    }' > "$VPD_CONFIG"
}

# write_list_file writes the subscription an import is given: three servers,
# two of them on one address.
write_list_file() {
    printf '%s\n' \
        'vless://uuid-a@5.6.7.8:443?type=tcp&security=tls#A' \
        'vless://uuid-b@1.2.3.4:443?type=tcp&security=tls#B' \
        'vless://uuid-c@5.6.7.8:8443?type=tcp&security=tls#C' \
        > "$BATS_TEST_TMPDIR/servers.txt"
}

# serve_list_file puts a curl first on PATH that answers any link with that
# list, the way a subscription host would.
serve_list_file() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/sh\ncat "%s"\n' "$BATS_TEST_TMPDIR/servers.txt" > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

# run_import runs import_server_list.sh the way a user does, the answer to its
# prompt on stdin.
run_import() {
    run env IMPORT_TEST_MODE=0 bash "$SCRIPTS_DIR/import_server_list.sh" <<< "$1"
}

# hold_config_lock takes the lock the daemons and configure.sh take, the way
# another writer would, until release_config_lock. FD 3 is closed, or bats
# would wait for the holder.
hold_config_lock() {
    flock "$VPD_DIR/.vpn-director.json.lock" sleep 30 3>&- &
    LOCK_HOLDER=$!
    sleep 0.5
}

release_config_lock() {
    pkill -P "$LOCK_HOLDER" 2>/dev/null || true
    kill "$LOCK_HOLDER" 2>/dev/null || true
}

# The watch, the Web UI and /import publish a list with the bypass set built
# from it; a list imported here left TPROXY bypassing the previous list's
# addresses until configure.sh ran. servers.json holds every server's UUID, and
# the daemons write it 0600.
@test "import_server_list.sh publishes the list with its bypass set" {
    load_import_into
    write_imported_state
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt"

    assert_success
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["A","B","C"]'
    run jq -c '[.xray.servers, .xray.clients, .webui.jwt_secret]' "$VPD_CONFIG"
    assert_output '[["1.2.3.4","5.6.7.8"],["192.168.50.10"],"secret"]'
    run stat -c %a "$VPD_DIR/data/servers.json" "$VPD_CONFIG"
    assert_output $'600\n600'
    run ls -A "$VPD_DIR/data"
    assert_output "servers.json"
}

# The bot's subscription watch re-imports xray.subscription_url when the Xray
# outbound dies, and the Web UI and /import re-import it on request. A list
# imported here from another link was replaced by the old link's on the next
# refresh: the link goes with the list.
@test "import_server_list.sh saves an https link with its list" {
    load_import_into
    write_imported_state
    write_list_file
    serve_list_file

    run_import "https://cdn.example/s/b"

    assert_success
    run jq -r '.xray.subscription_url' "$VPD_CONFIG"
    assert_output "https://cdn.example/s/b"
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["A","B","C"]'
}

# Neither the watch nor the Web UI fetches a file or a plain-http link, and the
# saved link is no longer what the list came from.
@test "import_server_list.sh clears the saved link for a file or an http link" {
    load_import_into
    write_list_file
    serve_list_file
    for input in "$BATS_TEST_TMPDIR/servers.txt" "http://cdn.example/s/b"; do
        write_imported_state

        run_import "$input"

        assert_success
        run jq -c '.xray | has("subscription_url")' "$VPD_CONFIG"
        assert_output "false"
    done
}

# Before configure.sh has run there is no config, and nothing refreshes a list.
@test "import_server_list.sh creates no config" {
    load_import_into
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data}' > "$VPD_DIR/vpn-director.json.template"
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt"

    assert_success
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["A","B","C"]'
    [[ ! -e $VPD_CONFIG ]]
}

# The Web UI, the bot and configure.sh write under one lock, and the watch
# publishes a refreshed list under it. A list written before the lock was held
# could land beside another import's bypass set or link, and one written ahead
# of a lock that never came was left beside the old link, whose list the next
# refresh brought back.
@test "import_server_list.sh publishes nothing while another writer holds the config lock" {
    load_import_into
    write_imported_state
    write_list_file
    hold_config_lock
    export VPD_CONFIG_LOCK_WAIT=1

    run_import "$BATS_TEST_TMPDIR/servers.txt"
    release_config_lock

    assert_failure
    assert_output --partial "nothing was imported"
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["Old"]'
    run jq -c '[.xray.servers, .xray.subscription_url]' "$VPD_CONFIG"
    assert_output '[["9.9.9.9"],"https://old.example/s/a"]'
    run ls -A "$VPD_DIR/data"
    assert_output "servers.json"
}

# A failed import keeps the list the router has, as the Web UI and /import do.
@test "import_server_list.sh keeps the previous list when the new one has no usable server" {
    load_import_into
    write_imported_state
    printf '%s\n' 'vless://uuid@5.6.7.8:99999?type=tcp#Bad' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt"

    assert_failure
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["Old"]'
}

# The new provider serves no links at all: its subscription is an array of
# Xray configs. The proxy outbound of each is the server; a config with more
# than one proxy is skipped as composite.
@test "import_server_list.sh imports an Xray JSON subscription" {
    load_import_into
    write_imported_state
    cat > "$BATS_TEST_TMPDIR/servers.txt" <<'JSON'
[{"remarks": "Oslo", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.10", "port": 443, "password": "p"}]}, "streamSettings": {"network": "tcp", "security": "tls"}}, {"tag": "direct", "protocol": "freedom"}]},
 {"remarks": "Auto", "outbounds": [{"protocol": "vless", "settings": {"address": "198.51.100.11", "port": 443, "id": "u"}}, {"protocol": "vless", "settings": {"address": "198.51.100.12", "port": 443, "id": "u"}}]}]
JSON

    run_import "$BATS_TEST_TMPDIR/servers.txt"

    assert_success
    assert_output --partial "Skipping Auto: composite (2 proxy outbounds)"
    run jq -c '[.[] | [.name, .outbound.protocol, .outbound.settings.servers[0].password, .ips]]' "$VPD_DIR/data/servers.json"
    assert_output '[["Oslo","trojan","p",["198.51.100.10"]]]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["198.51.100.10"]'
}

# An HTML page - what some panels answer a browser with - is no subscription;
# the list the router has stays.
@test "import_server_list.sh refuses a body it cannot read and keeps the previous list" {
    load_import_into
    write_imported_state
    printf '%s\n' '<!doctype html><html><body>Open this link in your VPN app</body></html>' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt"

    assert_failure
    assert_output --partial "Cannot read the subscription: unrecognized subscription format"
    run jq -c '[.[].name]' "$VPD_DIR/data/servers.json"
    assert_output '["Old"]'
}
