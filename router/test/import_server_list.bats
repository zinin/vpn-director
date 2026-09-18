#!/usr/bin/env bats

load 'test_helper'

# Test URI for basic parsing (ASCII name, no special chars)
TEST_URI_BASIC='vless://11111111-2222-3333-4444-555555555555@server1.test.example:8443?type=tcp&security=tls#Prague, Czechia'

# URI with emoji flag + cyrillic name
TEST_URI_EMOJI_CYRILLIC='vless://11111111-2222-3333-4444-555555555555@server2.test.example:8443?type=tcp&security=tls#🇷🇺 Россия, Москва'

# URI with only emoji (should fallback to hostname)
TEST_URI_EMOJI_ONLY='vless://11111111-2222-3333-4444-555555555555@server3.test.example:8443?type=tcp&security=tls#🇺🇸🌟✨'

# URI whose fragment is a truncated two-byte sequence followed by a field separator
# (percent-encoded \xC3|evil): the filter must not carry the | into the output
TEST_URI_TRUNCATED_UTF8='vless://11111111-2222-3333-4444-555555555555@server2.test.example:8443?type=tcp&security=tls#%C3%7Cevil'

# URI with URL-encoded spaces
TEST_URI_URLENCODED='vless://11111111-2222-3333-4444-555555555555@server4.test.example:8443?type=tcp&security=tls#New%20York%20City'

# URI with cyrillic only (no emoji)
TEST_URI_CYRILLIC='vless://11111111-2222-3333-4444-555555555555@server5.test.example:8443?type=tcp&security=tls#Казахстан, Алматы'

# What a real subscription actually sends: the whole fragment percent-encoded,
# flag emoji included. Decoding only %20 left the hex digits of every other
# byte in the name, and the filter below keeps digits - so servers.json held
# "F09F87B3F09F87B1 D090D0BC..." instead of a country.
TEST_URI_PERCENT_ENCODED='vless://11111111-2222-3333-4444-555555555555@server6.test.example:8443?type=tcp&security=tls#%F0%9F%87%B3%F0%9F%87%B1%20%D0%90%D0%BC%D1%81%D1%82%D0%B5%D1%80%D0%B4%D0%B0%D0%BC%2C%20%D0%9D%D0%B8%D0%B4%D0%B5%D1%80%D0%BB%D0%B0%D0%BD%D0%B4%D1%8B%2C%20Extra'

# ============================================================================
# parse_vless_uri: Field extraction
# ============================================================================

@test "parse_vless_uri: extracts server hostname" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_BASIC")
    server=$(printf '%s' "$result" | cut -d'|' -f1)
    [ "$server" = "server1.test.example" ]
}

@test "parse_vless_uri: extracts port number" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_BASIC")
    port=$(printf '%s' "$result" | cut -d'|' -f2)
    [ "$port" = "8443" ]
}

@test "parse_vless_uri: extracts UUID" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_BASIC")
    uuid=$(printf '%s' "$result" | cut -d'|' -f3)
    [ "$uuid" = "11111111-2222-3333-4444-555555555555" ]
}

@test "parse_vless_uri: extracts ASCII name" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_BASIC")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "Prague, Czechia" ]
}

@test "parse_vless_uri: decodes a percent-encoded name" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_PERCENT_ENCODED")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "Амстердам, Нидерланды, Extra" ]
}

# The routers have no UTF-8 locale - KeeneticOS has no locale at all - so awk
# works on bytes there. A character-wise filter splits a two-byte letter in
# half and keeps whichever byte happens to match an ASCII class, which turned
# "Амстердам" into "Амс?е?дам" on the device while passing here.
@test "parse_vless_uri: keeps the name intact in the routers' byte locale" {
    load_import_server_list
    result=$(LC_ALL=C parse_vless_uri "$TEST_URI_PERCENT_ENCODED")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "Амстердам, Нидерланды, Extra" ]

    result=$(LC_ALL=C parse_vless_uri "$TEST_URI_EMOJI_CYRILLIC")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "Россия, Москва" ]
}

@test "parse_vless_uri extracts reality stream params" {
    load_import_server_list
    # Real subscription format includes headerType=none before type=tcp — guards the
    # `_vless_query_get type` lookup against matching `headerType`.
    uri='vless://uuid@1.2.3.4:443?security=reality&encryption=none&fp=firefox&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=cdn3-87.yahoo.com&pbk=PBKEY&sid=55e6#NL'
    run parse_vless_uri "$uri"
    [ "$status" -eq 0 ]
    # fields: server|port|uuid|name|security|network|flow|sni|fp|pbk|sid|alpn
    [ "$(printf '%s' "$output" | cut -d'|' -f5)" = "reality" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f6)" = "tcp" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f7)" = "xtls-rprx-vision" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f8)" = "cdn3-87.yahoo.com" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f9)" = "firefox" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f10)" = "PBKEY" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f11)" = "55e6" ]
}

@test "parse_vless_uri keeps core fields without params" {
    load_import_server_list
    run parse_vless_uri 'vless://uuid@1.2.3.4:443#Name'
    [ "$status" -eq 0 ]
    [ "$(printf '%s' "$output" | cut -d'|' -f1)" = "1.2.3.4" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f3)" = "uuid" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f5)" = "" ]
}

# ============================================================================
# parse_vless_uri: Name handling
# ============================================================================

@test "parse_vless_uri: filters emoji from name, keeps cyrillic" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_EMOJI_CYRILLIC")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    # Emoji flag should be removed, cyrillic preserved
    [ "$name" = "Россия, Москва" ]
}

@test "parse_vless_uri: a truncated two-byte sequence does not smuggle a field separator" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_TRUNCATED_UTF8")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    security=$(printf '%s' "$result" | cut -d'|' -f5)
    # The lone \xC3 is dropped with its non-continuation byte, so no | shifts the fields
    [ "$name" = "evil" ]
    [ "$security" = "tls" ]
}

@test "parse_vless_uri: falls back to hostname when name is only emoji" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_EMOJI_ONLY")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    # All emoji filtered out, should fallback to server hostname
    [ "$name" = "server3.test.example" ]
}

@test "parse_vless_uri: decodes URL-encoded spaces" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_URLENCODED")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "New York City" ]
}

@test "parse_vless_uri: handles cyrillic-only name" {
    load_import_server_list
    result=$(parse_vless_uri "$TEST_URI_CYRILLIC")
    name=$(printf '%s' "$result" | cut -d'|' -f4)
    [ "$name" = "Казахстан, Алматы" ]
}

# ============================================================================
# decode_vless_content: Format detection
# ============================================================================

@test "decode_vless_content: detects plaintext format (single URI)" {
    load_import_server_list
    content="vless://uuid@server:443?type=tcp#Name"
    result=$(decode_vless_content "$content")
    [ "$result" = "$content" ]
}

@test "decode_vless_content: detects plaintext format (multiple URIs)" {
    load_import_server_list
    content="vless://uuid1@server1:443?type=tcp#Name1
vless://uuid2@server2:443?type=tcp#Name2"
    result=$(decode_vless_content "$content")
    [ "$result" = "$content" ]
}

@test "decode_vless_content: handles plaintext with leading empty lines" {
    load_import_server_list
    content="

vless://uuid@server:443?type=tcp#Name"
    result=$(decode_vless_content "$content")
    [ "$result" = "$content" ]
}

@test "decode_vless_content: decodes base64 format" {
    load_import_server_list
    plaintext="vless://uuid@server:443?type=tcp#Name"
    encoded=$(printf '%s' "$plaintext" | base64)
    result=$(decode_vless_content "$encoded")
    [ "$result" = "$plaintext" ]
}

@test "decode_vless_content: decodes base64 with multiple URIs" {
    load_import_server_list
    plaintext="vless://uuid1@server1:443#Name1
vless://uuid2@server2:443#Name2"
    encoded=$(printf '%s' "$plaintext" | base64)
    result=$(decode_vless_content "$encoded")
    [ "$result" = "$plaintext" ]
}

@test "decode_vless_content: fails on invalid content" {
    load_import_server_list
    run decode_vless_content "not-base64-and-not-vless!!!"
    assert_failure
}

@test "decode_vless_content: fails on whitespace-only content" {
    load_import_server_list
    run decode_vless_content "

    "
    assert_failure
}

@test "decode_vless_content: decodes valid base64 even if not VLESS (validation is downstream)" {
    load_import_server_list
    plaintext="just some random text"
    encoded=$(printf '%s' "$plaintext" | base64)
    result=$(decode_vless_content "$encoded")
    # Function succeeds - content validation is handled downstream
    [ "$result" = "$plaintext" ]
}

@test "decode_vless_content: decodes URL-safe base64 alphabet" {
    load_import_server_list
    # "????" standard base64 is "Pz8/Pw=="; the URL-safe form replaces / with _.
    # The standard decoder rejects _, so this exercises the url-safe fallback.
    # Use command substitution (not run) so the log line on stderr is excluded.
    result=$(decode_vless_content "Pz8_Pw==")
    [ "$result" = "????" ]
}

@test "decode_vless_content: url-safe base64 maps both - and _ (full alphabet)" {
    load_import_server_list
    # ">>>???" standard base64 is "Pj4+Pz8/" (contains BOTH + and /); the URL-safe
    # form replaces + with - and / with _, so this exercises the full -_ -> +/
    # mapping. A reversed set (e.g. tr '_-' '+/') decodes to the wrong bytes.
    result=$(decode_vless_content "Pj4-Pz8_")
    [ "$result" = ">>>???" ]
}

# ============================================================================
# parse_vless_uri: IPv6 literal host
# ============================================================================

@test "parse_vless_uri: parses bracketed IPv6 host and port" {
    load_import_server_list
    run parse_vless_uri 'vless://uuid@[2001:db8::1]:443?type=tcp#v6'
    [ "$status" -eq 0 ]
    [ "$(printf '%s' "$output" | cut -d'|' -f1)" = "2001:db8::1" ]
    [ "$(printf '%s' "$output" | cut -d'|' -f2)" = "443" ]
}

# ============================================================================
# _redact_uri: mask UUID for DEBUG logging
# ============================================================================

@test "_redact_uri: masks UUID and strips fragment, keeps host" {
    load_import_server_list
    run _redact_uri 'vless://11111111-2222-3333-4444-555555555555@server.example:443?type=tcp#MyName'
    [ "$status" -eq 0 ]
    # UUID is a secret and must not leak into logs
    [[ "$output" != *11111111-2222-3333-4444-555555555555* ]]
    # host:port and params are kept; the #fragment is stripped
    [[ "$output" == *server.example:443* ]]
    [[ "$output" != *MyName* ]]
}

# ============================================================================
# _url_decode / _vless_query_get: percent-decoding (valid %XX like url.ParseQuery;
# lenient on malformed % — Go rejects, shell keeps literal; see ticket #41)
# ============================================================================

@test "_url_decode: decodes %XX escapes" {
    load_import_server_list
    result=$(_url_decode 'h2%2Chttp/1.1')
    [ "$result" = "h2,http/1.1" ]
}

@test "_url_decode: maps + to space, leaves lone/incomplete % intact" {
    load_import_server_list
    # Lenient on malformed % (Go's url.ParseQuery would reject these); see #41.
    [ "$(_url_decode 'a+b')" = "a b" ]
    [ "$(_url_decode '50%')" = "50%" ]
    [ "$(_url_decode 'x%2y')" = "x%2y" ]
}

@test "_vless_query_get: URL-decodes the value (valid %XX like url.ParseQuery)" {
    load_import_server_list
    result=$(_vless_query_get 'type=tcp&alpn=h2%2Chttp/1.1' alpn)
    [ "$result" = "h2,http/1.1" ]
}

@test "parse_vless_uri: URL-decodes percent-encoded alpn into comma list" {
    load_import_server_list
    result=$(parse_vless_uri 'vless://uuid@1.2.3.4:443?type=tcp&alpn=h2%2Chttp/1.1#N')
    [ "$(printf '%s' "$result" | cut -d'|' -f12)" = "h2,http/1.1" ]
}

# ============================================================================
# step_parse_and_save_servers: JSON output
# ============================================================================

@test "step_parse_and_save_servers: saves ips array instead of ip" {
    load_import_server_list

    DATA_DIR="/tmp/bats_test_import_data"
    SERVERS_FILE="$DATA_DIR/servers.json"
    mkdir -p "$DATA_DIR"

    # Override VPD_CONFIG to a temp config with our data_dir
    VPD_CONFIG="/tmp/bats_test_import_data/vpn-director.json"
    printf '{"data_dir": "%s"}\n' "$DATA_DIR" > "$VPD_CONFIG"

    VLESS_SERVERS="vless://test-uuid@example.com:443?type=tcp#TestServer"

    step_parse_and_save_servers

    # Check that servers.json has "ips" array, not "ip" string
    result=$(jq -r '.[0].ips | type' "$SERVERS_FILE")
    [ "$result" = "array" ]

    # Check that "ip" field does not exist
    result=$(jq -r '.[0] | has("ip")' "$SERVERS_FILE")
    [ "$result" = "false" ]

    # Check the resolved IP is in the ips array
    result=$(jq -r '.[0].ips[0]' "$SERVERS_FILE")
    [ "$result" = "93.184.216.34" ]

    rm -rf "$DATA_DIR"
}

@test "step_parse_and_save_servers writes reality params to servers.json" {
    load_import_server_list

    tmp_data="$BATS_TEST_TMPDIR/data"
    mkdir -p "$tmp_data"
    # get_data_dir reads VPD_CONFIG/VPD_TEMPLATE; override to a temp config
    cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{"data_dir":"%s"}' "$tmp_data" > "$cfg"
    VPD_CONFIG="$cfg"
    VLESS_SERVERS='vless://uuid@1.2.3.4:443?security=reality&flow=xtls-rprx-vision&sni=cdn.example.com&pbk=PBK&sid=sid1&type=tcp#NL'
    run step_parse_and_save_servers
    [ "$status" -eq 0 ]
    out="$tmp_data/servers.json"
    [ "$(jq -r '.[0].security' "$out")" = "reality" ]
    [ "$(jq -r '.[0].flow' "$out")" = "xtls-rprx-vision" ]
    [ "$(jq -r '.[0].public_key' "$out")" = "PBK" ]
    [ "$(jq -r '.[0].short_id' "$out")" = "sid1" ]
    [ "$(jq -r '.[0].sni' "$out")" = "cdn.example.com" ]
    [ "$(jq -r '.[0] | has("alpn")' "$out")" = "false" ]
}

@test "step_parse_and_save_servers skips out-of-range port, keeps valid server" {
    load_import_server_list

    tmp_data="$BATS_TEST_TMPDIR/data"
    mkdir -p "$tmp_data"
    cfg="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{"data_dir":"%s"}' "$tmp_data" > "$cfg"
    VPD_CONFIG="$cfg"
    # First URI has an out-of-range port (must be skipped); the second is valid
    # and must still be saved (one bad entry does not drop the rest).
    VLESS_SERVERS=$'vless://uuid@1.2.3.4:99999?type=tcp#Bad\nvless://uuid@5.6.7.8:443?type=tcp#Good'
    run step_parse_and_save_servers
    [ "$status" -eq 0 ]
    out="$tmp_data/servers.json"
    [ "$(jq length "$out")" -eq 1 ]
    [ "$(jq -r '.[0].address' "$out")" = "5.6.7.8" ]
    [ "$(jq -r '.[0].port' "$out")" -eq 443 ]
}

# ============================================================================
# step_save_subscription_url: the link a refresh fetches
# ============================================================================

# load_import_into sources the importer with VPD_DIR and VPD_CONFIG pointed at
# a scratch directory, the way a caller that overrides them does.
load_import_into() {
    export VPD_DIR="$BATS_TEST_TMPDIR/vpn-director"
    export VPD_CONFIG="$VPD_DIR/vpn-director.json"
    mkdir -p "$VPD_DIR"
    load_import_server_list
}

# write_saved_link stands in for a config the Web UI saved a link into.
write_saved_link() {
    printf '%s\n' '{"webui":{"jwt_secret":"secret"},"xray":{"clients":["192.168.50.10"],"subscription_url":"https://old.example/s/a"}}' \
        > "$VPD_CONFIG"
}

# The bot's subscription watch re-imports xray.subscription_url when the Xray
# outbound dies, and the Web UI and /import re-import it on request. A list
# imported here from another link was replaced by the old link's on the next
# refresh: the link now goes with the list.
@test "step_save_subscription_url: an https link is saved with the list" {
    load_import_into
    write_saved_link
    VLESS_INPUT="https://cdn.example/s/b"

    run step_save_subscription_url

    assert_success
    run jq -c '[.xray.subscription_url, .xray.clients, .webui.jwt_secret]' "$VPD_CONFIG"
    assert_output '["https://cdn.example/s/b",["192.168.50.10"],"secret"]'
    run stat -c %a "$VPD_CONFIG"
    assert_output "600"
}

# Neither the watch nor the Web UI fetches a file or a plain-http link, and the
# saved link is no longer what the list came from.
@test "step_save_subscription_url: a file or an http link clears the saved one" {
    load_import_into
    for input in "$BATS_TEST_TMPDIR/servers.txt" "http://cdn.example/s/b"; do
        write_saved_link
        VLESS_INPUT="$input"

        run step_save_subscription_url

        assert_success
        run jq -c '.xray | has("subscription_url")' "$VPD_CONFIG"
        assert_output "false"
    done
}

# Before configure.sh has run there is no config, and nothing refreshes a list.
@test "step_save_subscription_url: creates no config" {
    load_import_into
    VLESS_INPUT="https://cdn.example/s/b"

    run step_save_subscription_url

    assert_success
    [[ ! -e $VPD_CONFIG ]]
}

# The Web UI, the bot and configure.sh write this file under one lock; a write
# outside it is lost to theirs, or theirs to it.
@test "step_save_subscription_url: refuses while another writer holds the config lock" {
    load_import_into
    write_saved_link
    flock "$VPD_DIR/.vpn-director.json.lock" sleep 30 &
    local holder=$!
    sleep 0.5
    VLESS_INPUT="https://cdn.example/s/b"

    VPD_CONFIG_LOCK_WAIT=1
    run step_save_subscription_url
    kill "$holder" 2>/dev/null || true

    assert_failure
    assert_output --partial "locked"
    run jq -r '.xray.subscription_url' "$VPD_CONFIG"
    assert_output "https://old.example/s/a"
}
