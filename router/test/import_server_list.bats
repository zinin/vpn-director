#!/usr/bin/env bats

load 'test_helper'

# The decoding itself - every scheme, container and skip reason - is
# router/test/unit/subscription.bats, on the cases the Go importer shares.
# This file is the script around it: resolution, the report and the
# subscription menu.

# ============================================================================
# step_parse_servers: resolution and the list it leaves for publication
# ============================================================================

# decode_into_result reads a subscription the way fetch_subscription does.
decode_into_result() {
    SUB_RESULT=$(printf '%s' "$1" | subscription_decode)
}

# step_parse_servers leaves the list in $SERVERS_TMP for publish_add and
# publish_refresh, so these call it directly: "run" would keep the variable in
# its subshell.
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

# A skip's detail holds what the subscription decoded: an escape sequence in it
# reached the terminal, the log file and syslog, and a line break made a line
# of its own that the subscription wrote.
@test "step_get_subscription: a skip line carries no control character from the subscription" {
    load_import_server_list
    # common.sh points LOG_FILE at /tmp/vpn-director.log, which every run
    # appends to; what this test refutes has to be looked for in its own lines.
    LOG_FILE="$BATS_TEST_TMPDIR/vpn-director.log"
    printf '%s\n' 'vless://u@5.6.7.8:443?type=%1B%5B31mX%0Afake#Evil' 'vless://u@5.6.7.9:443#Good' > "$BATS_TEST_TMPDIR/list.txt"

    step_get_subscription <<< "$BATS_TEST_TMPDIR/list.txt"

    run cat "$LOG_FILE"
    assert_output --partial "Skipping Evil: unsupported (transport [31mXfake)"
    refute_output --partial $'\x1b'
    refute_line --regexp '^fake'
}

# ============================================================================
# The host of a link: a subscription's default name and the menu's column
# ============================================================================

# What Go's url.Hostname gives: a query right after the host - where a link
# may carry its token - and the brackets of an IPv6 address are no part of it.
@test "link_host: the host of a link, as Go's url.Hostname gives it" {
    load_import_server_list

    run link_host 'https://sub.example.com?token=abc'
    assert_output "sub.example.com"
    run link_host 'https://user:pw@[2001:db8::1]:443/s'
    assert_output "2001:db8::1"
    run link_host 'http://cdn.example:8080/s/b#top'
    assert_output "cdn.example"
}

# The menu shows the host of a link, never the link: its path or its query
# carries the token.
@test "show_subscriptions: a line each, with the host in place of the link" {
    load_import_server_list

    run show_subscriptions '[
        {"id": "0a1b2c3d", "name": "Alpha", "url": "https://sub.example.com/s/secret-token", "refreshed": "2026-09-24T18:05:00Z", "servers": [{}, {}]},
        {"id": "0a1b2c3e", "name": "Beta", "url": "https://panel.example.net?token=secret-token", "error": "Failed to download the subscription", "servers": [{}]},
        {"id": "0a1b2c3f", "name": "Gamma", "refreshed": "2026-09-24T18:00:00Z", "servers": []}]'

    assert_success
    assert_line "  1) Alpha   sub.example.com   2 servers   refreshed 2026-09-24 18:05 UTC"
    assert_line "  2) Beta   panel.example.net   1 servers   error: Failed to download the subscription"
    assert_line "  3) Gamma   static list   0 servers   refreshed 2026-09-24 18:00 UTC"
    refute_output --partial "secret-token"
}

# ============================================================================
# The menu: the subscription files, xray.servers and what the previous
# release left behind
# ============================================================================

# load_import_into points VPD_DIR and VPD_CONFIG at a scratch directory, the way
# a caller that overrides them does, and sources the importer there.
load_import_into() {
    export VPD_DIR="$BATS_TEST_TMPDIR/vpn-director"
    export VPD_CONFIG="$VPD_DIR/vpn-director.json"
    mkdir -p "$VPD_DIR"
    load_import_server_list
}

# write_imported_state stands in for a router the previous release imported
# to: its single list, the bypass set built from it and the link the Web UI
# saved it from.
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

# write_oversized_list writes write_list_file's servers followed by a line that
# is no link, 1048577 bytes in all: one byte past the 1 MiB cap, and a list
# that would import but for its size.
write_oversized_list() {
    write_list_file
    local size
    size=$(wc -c < "$BATS_TEST_TMPDIR/servers.txt")
    head -c $(( 1048577 - size )) /dev/zero | tr '\0' '#' >> "$BATS_TEST_TMPDIR/servers.txt"
}

# serve_list_file puts a curl first on PATH that answers any link with that
# list, the way a subscription host would.
serve_list_file() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/sh\ncat "%s"\n' "$BATS_TEST_TMPDIR/servers.txt" > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

# run_import runs import_server_list.sh the way a user does: each argument is
# the answer to one prompt, and the end of the input quits the menu.
run_import() {
    run env IMPORT_TEST_MODE=0 bash "$SCRIPTS_DIR/import_server_list.sh" < <(printf '%s\n' "$@")
}

# subs_dir is where the scratch router keeps its subscriptions.
subs_dir() {
    printf '%s/data/subscriptions' "$VPD_DIR"
}

# write_config writes a config the way configure.sh leaves one: a data
# directory, the Web UI's secret and one Xray client.
write_config() {
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data, webui: {jwt_secret: "secret"}, xray: {clients: ["192.168.50.10"]}}' > "$VPD_CONFIG"
}

# write_list_file_named <file> <link...> writes another list file.
write_list_file_named() {
    local file=$1
    shift
    printf '%s\n' "$@" > "$file"
}

# serve_failing_download puts a curl first on PATH that fails every download,
# as a host that answers 403 does.
serve_failing_download() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/sh\nexit 22\n' > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
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

# A first run has no subscription: the menu opens on Add. The list becomes a
# subscription file, 600 in a 700 directory, and xray.servers follows it.
@test "import_server_list.sh: a first run adds a subscription and brings the config in step" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -c '[.name, has(\"url\"), [.servers[].name]]' '$(subs_dir)'/*.json"
    assert_output '["servers",false,["A","B","C"]]'
    run jq -c '[.xray.servers, .xray.clients, .webui.jwt_secret]' "$VPD_CONFIG"
    assert_output '[["1.2.3.4","5.6.7.8"],["192.168.50.10"],"secret"]'
    run bash -c "stat -c %a '$(subs_dir)' '$(subs_dir)'/*.json"
    assert_output $'700\n600'
}

@test "import_server_list.sh: an https link is saved and names the subscription after its host" {
    load_import_into
    write_config
    write_list_file
    serve_list_file

    run_import "https://cdn.example/s/b" ""

    assert_success
    run bash -c "jq -c '[.name, .url]' '$(subs_dir)'/*.json"
    assert_output '["cdn.example","https://cdn.example/s/b"]'
}

@test "import_server_list.sh: a name given is the subscription's name" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" "  My List  "

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "My List"
}

# The single list and link of the previous release go with the first
# subscription written; the rest of the config stays.
@test "import_server_list.sh: removes servers.json and xray.subscription_url of the previous release" {
    load_import_into
    write_imported_state
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    [[ ! -e $VPD_DIR/data/servers.json ]]
    run jq -c '[(.xray | has("subscription_url")), .xray.clients]' "$VPD_CONFIG"
    assert_output '[false,["192.168.50.10"]]'
}

@test "import_server_list.sh: Add puts a second subscription beside the first" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "Beta" q

    assert_success
    # Both were added within one second, and the random ids order them then.
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$(subs_dir)' | jq -c '[.[].name] | sort'"
    assert_output '["Alpha","Beta"]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["1.2.3.4","5.6.7.8","9.9.9.9"]'
}

@test "import_server_list.sh: the same https link refreshes its subscription" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"

    run_import a "https://cdn.example/s/b" "" q

    assert_success
    assert_output --partial "saved already"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Alpha"
}

@test "import_server_list.sh: refuses a name another subscription has, whatever its case" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "ALPHA" q

    assert_output --partial "Another subscription is named ALPHA"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
}

# A refresh whose download fails keeps the list and says why in the file,
# as the daemons do.
@test "import_server_list.sh: Refresh records why a download failed and keeps the list" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    serve_failing_download

    run_import r 1 q

    run bash -c "jq -c '[.error, (.servers | length)]' '$(subs_dir)'/*.json"
    assert_output '["Failed to download the subscription",3]'
}

# Deleted while its download runs: the list is not published, and the file
# is not brought back.
@test "import_server_list.sh: a subscription deleted while it downloads stays deleted" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    printf '#!/bin/sh\nrm -f "%s"/*.json\ncat "%s"\n' "$(subs_dir)" "$BATS_TEST_TMPDIR/servers.txt" > "$BATS_TEST_TMPDIR/bin/curl"

    run_import r 1 q

    assert_output --partial "deleted or changed while it downloaded"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
}

@test "import_server_list.sh: Rename" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import n 1 "Main" q

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Main"
}

# The choice the watch kept from the deleted subscription ends with it; the
# running Xray is left alone, and the user is told.
@test "import_server_list.sh: Delete ends the choice kept from it and warns about the running server" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"
    local id
    id=$(jq -r '.id' "$(subs_dir)"/*.json)
    jq --arg id "$id" '.xray.active_server = {subscription: $id, name: "A"} | .xray.preferred_server = {subscription: $id, name: "B"}' \
        "$VPD_CONFIG" > "$VPD_CONFIG.new" && mv "$VPD_CONFIG.new" "$VPD_CONFIG"

    run_import d 1 y q

    assert_success
    assert_output --partial "The running Xray server came from Alpha"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
    run jq -c '[(.xray | has("preferred_server")), .xray.active_server.name, .xray.servers]' "$VPD_CONFIG"
    assert_output '[false,"A",[]]'
}

# The Web UI, the bot and configure.sh write under one lock, and so does the
# watch. Nothing is written before the lock is held.
@test "import_server_list.sh: publishes nothing while another writer holds the config lock" {
    load_import_into
    write_config
    write_list_file
    hold_config_lock
    export VPD_CONFIG_LOCK_WAIT=1

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""
    release_config_lock

    assert_failure
    assert_output --partial "nothing was imported"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
    run jq -c '.xray | has("servers")' "$VPD_CONFIG"
    assert_output "false"
}

@test "import_server_list.sh: a list without a usable server adds nothing" {
    load_import_into
    write_config
    printf '%s\n' 'vless://uuid@5.6.7.8:99999?type=tcp#Bad' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# Before configure.sh has run there is no config, and none is created.
@test "import_server_list.sh: creates no config" {
    load_import_into
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data}' > "$VPD_DIR/vpn-director.json.template"
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "jq -c '[.servers[].name]' '$(subs_dir)'/*.json"
    assert_output '["A","B","C"]'
    [[ ! -e $VPD_CONFIG ]]
}

# The new provider serves no links at all: its subscription is an array of
# Xray configs. The proxy outbound of each is the server; a config with more
# than one proxy is skipped as composite.
@test "import_server_list.sh: imports an Xray JSON subscription" {
    load_import_into
    write_config
    cat > "$BATS_TEST_TMPDIR/servers.txt" <<'JSON'
[{"remarks": "Oslo", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.10", "port": 443, "password": "p"}]}, "streamSettings": {"network": "tcp", "security": "tls"}}, {"tag": "direct", "protocol": "freedom"}]},
 {"remarks": "Auto", "outbounds": [{"protocol": "vless", "settings": {"address": "198.51.100.11", "port": 443, "id": "u"}}, {"protocol": "vless", "settings": {"address": "198.51.100.12", "port": 443, "id": "u"}}]}]
JSON

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    assert_output --partial "Skipping Auto: composite (2 proxy outbounds)"
    run bash -c "jq -c '[.servers[] | [.name, .outbound.protocol, .outbound.settings.servers[0].password, .ips]]' '$(subs_dir)'/*.json"
    assert_output '[["Oslo","trojan","p",["198.51.100.10"]]]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["198.51.100.10"]'
}

# An HTML page - what some panels answer a browser with - is no subscription.
@test "import_server_list.sh: refuses a body it cannot read" {
    load_import_into
    write_config
    printf '%s\n' '<!doctype html><html><body>Open this link in your VPN app</body></html>' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    assert_output --partial "Cannot read the subscription: unrecognized subscription format"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# The daemons refuse a subscription over 1 MiB rather than cut it short, and
# so does the router.
@test "import_server_list.sh: refuses a subscription file over 1 MiB" {
    load_import_into
    write_config
    write_oversized_list

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl --max-filesize stops a transfer it knows to be too large with exit 63:
# the subscription's size, not a download that failed.
@test "import_server_list.sh: refuses a download curl stops at 1 MiB" {
    load_import_into
    write_config
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<MOCK
#!/bin/sh
printf '%s\n' "\$*" > "$BATS_TEST_TMPDIR/curl.args"
exit 63
MOCK
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    run cat "$BATS_TEST_TMPDIR/curl.args"
    assert_output --partial -- "--max-filesize 1048576"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl before 8.4.0 does not stop a transfer whose size it did not know in
# advance, so what it downloaded is measured again.
@test "import_server_list.sh: refuses a download over 1 MiB that curl let through" {
    load_import_into
    write_config
    write_oversized_list
    serve_list_file

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}
