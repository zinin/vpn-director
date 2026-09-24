#!/usr/bin/env bats

# The negative checks below pass a flag to `run`, which bats only guarantees
# from 1.5.0; a bare `! cmd` in the middle of a test asserts nothing.
bats_require_minimum_version 1.5.0

load '../test_helper'

# testdata/substore is read by the Go store too
# (server/internal/vpnconfig/substore_test.go): both list the same
# subscriptions in the same order, and apply the same name rules.
FIXTURES="$PROJECT_ROOT/../testdata/substore"

setup() {
    source "$LIB_DIR/substore.sh"
}

@test "substore_list: the shared fixtures, in the order the daemons list them" {
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$FIXTURES' | jq -c '[.[] | [.id, .name]]'"
    assert_success
    assert_output '[["1b2c3d4e","Beta"],["0a1b2c3d","Alpha"],["2c3d4e5f","Gamma"]]'
}

@test "substore_list: no directory is no subscription" {
    run substore_list "$BATS_TEST_TMPDIR/none"
    assert_success
    assert_output '[]'
}

# A temp file of an atomic write, a backup, a directory and a file whose id is
# not its name are no subscription; a broken file is skipped with a warning.
@test "substore_list: skips what is no subscription" {
    local dir="$BATS_TEST_TMPDIR/subscriptions" good
    mkdir -p "$dir/3d4e5f6a.json"
    good='{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    printf '%s' "$good" > "$dir/0a1b2c3d.json"
    printf '%s' "$good" > "$dir/.0a1b2c3d.json.tmp-123"
    printf '%s' "$good" > "$dir/0a1b2c3d.json.Ab12Cd"
    printf '%s' "${good/0a1b2c3d/0A1B2C3D}" > "$dir/0A1B2C3D.json"
    printf '%s' "$good" > "$dir/backup.json"
    printf '%s' "${good/0a1b2c3d/ffffffff}" > "$dir/2c3d4e5f.json"
    printf '{"id":"1b2c3d4e",' > "$dir/1b2c3d4e.json"

    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' 2>'$BATS_TEST_TMPDIR/err' | jq -c '[.[].id]'"

    assert_success
    assert_output '["0a1b2c3d"]'
    run cat "$BATS_TEST_TMPDIR/err"
    assert_output --partial "Skipping subscription file 1b2c3d4e.json"
    assert_output --partial "Skipping subscription file 2c3d4e5f.json"
}

# A file edited by hand fails no reader. A field of the wrong type, or more
# than one JSON value, skips the file, as either fails Go's json.Unmarshal; a
# null field is an absent one, and a name or servers absent read as "" and [],
# as Go reads them - Go writes "servers": null for a subscription it never
# gave any.
@test "substore_list: a file edited by hand" {
    local dir="$BATS_TEST_TMPDIR/subscriptions" id
    mkdir -p "$dir"
    printf '%s' '{"id":"0a1b2c3d","added":"2026-09-24T18:00:00Z"}' > "$dir/0a1b2c3d.json"
    printf '%s' '{"id":"1b2c3d4e","name":null,"url":null,"added":"2026-09-24T18:01:00Z","refreshed":null,"error":null,"servers":null}' > "$dir/1b2c3d4e.json"
    printf '%s' '{"id":"2c3d4e5f","name":5}' > "$dir/2c3d4e5f.json"
    printf '%s' '{"id":"3d4e5f6a","servers":5}' > "$dir/3d4e5f6a.json"
    printf '%s' '{"id":"4e5f6a7b","servers":["Oslo"]}' > "$dir/4e5f6a7b.json"
    printf '%s' '{"id":"5f6a7b8c","url":{}}' > "$dir/5f6a7b8c.json"
    printf '%s' '["6a7b8c9d"]' > "$dir/6a7b8c9d.json"
    printf '%s' '{"id":"7b8c9d0e"}{"id":"7b8c9d0e"}' > "$dir/7b8c9d0e.json"
    : > "$dir/8c9d0e1f.json"

    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' 2>'$BATS_TEST_TMPDIR/err' | jq -c '[.[] | [.id, .name, .servers]]'"

    assert_success
    assert_output '[["0a1b2c3d","",[]],["1b2c3d4e","",[]]]'
    run cat "$BATS_TEST_TMPDIR/err"
    for id in 2c3d4e5f 3d4e5f6a 4e5f6a7b 5f6a7b8c 6a7b8c9d 7b8c9d0e 8c9d0e1f; do
        assert_output --partial "Skipping subscription file $id.json"
    done
    refute_output --partial "0a1b2c3d.json"
    refute_output --partial "1b2c3d4e.json"
}

# Every field of every server has the type Go gives it too (vpnconfig.Server),
# but the outbound, which Go keeps raw; one server of the wrong shape skips
# the file. A server without a name or an address lists with "" for both, as
# Go reads it.
@test "substore_list: a server edited by hand" {
    local dir="$BATS_TEST_TMPDIR/subscriptions" id
    mkdir -p "$dir"
    printf '%s' '{"id":"0a1b2c3d","added":"2026-09-24T18:00:00Z","servers":[{"port":443,"ips":[null,"192.0.2.1"],"outbound":"raw"}]}' > "$dir/0a1b2c3d.json"
    printf '%s' '{"id":"1b2c3d4e","added":"2026-09-24T18:01:00Z","servers":[{"name":"Oslo","address":null,"port":null,"ips":null,"alpn":null,"uuid":null}]}' > "$dir/1b2c3d4e.json"
    printf '%s' '{"id":"2c3d4e5f","servers":[{"name":5}]}' > "$dir/2c3d4e5f.json"
    printf '%s' '{"id":"3d4e5f6a","servers":[{"port":"443"}]}' > "$dir/3d4e5f6a.json"
    printf '%s' '{"id":"4e5f6a7b","servers":[{"ips":"192.0.2.1"}]}' > "$dir/4e5f6a7b.json"
    printf '%s' '{"id":"5f6a7b8c","servers":[{"ips":[5]}]}' > "$dir/5f6a7b8c.json"
    printf '%s' '{"id":"6a7b8c9d","servers":[{"alpn":["h2",1]}]}' > "$dir/6a7b8c9d.json"
    printf '%s' '{"id":"7b8c9d0e","servers":[{"name":"Riga"},{"public_key":{}}]}' > "$dir/7b8c9d0e.json"

    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' 2>'$BATS_TEST_TMPDIR/err' | jq -c '[.[] | [.id, (.servers[] | [.name, .address, .port, .ips])]]'"

    assert_success
    assert_output '[["0a1b2c3d",["","",443,[null,"192.0.2.1"]]],["1b2c3d4e",["Oslo","",null,null]]]'
    run cat "$BATS_TEST_TMPDIR/err"
    for id in 2c3d4e5f 3d4e5f6a 4e5f6a7b 5f6a7b8c 6a7b8c9d 7b8c9d0e; do
        assert_output --partial "Skipping subscription file $id.json"
    done
    refute_output --partial "0a1b2c3d.json"
    refute_output --partial "1b2c3d4e.json"
}

@test "substore_valid_id: 8 lowercase hex digits" {
    substore_valid_id 0a1b2c3d
    run ! substore_valid_id 0A1B2C3D
    run ! substore_valid_id 0a1b2c3
    run ! substore_valid_id 0a1b2c3d0
    run ! substore_valid_id ../0a1b2
    run ! substore_valid_id ''
}

@test "substore_new_id: a valid id no file has" {
    local id
    id=$(substore_new_id "$BATS_TEST_TMPDIR")
    substore_valid_id "$id"
    [[ ! -e $BATS_TEST_TMPDIR/$id.json ]]
}

# A BusyBox built without od must not stop an Add.
@test "substore_new_id: the kernel's uuid where od gives nothing" {
    run bash -c "source '$LIB_DIR/substore.sh'; od() { return 127; }; substore_new_id '$BATS_TEST_TMPDIR'"
    assert_success
    substore_valid_id "$output"
}

@test "substore_new_id: with nothing to draw from, it fails rather than spins" {
    run timeout 10 bash -c "source '$LIB_DIR/substore.sh'; od() { return 127; }; cut() { return 1; }; substore_new_id '$BATS_TEST_TMPDIR'"
    assert_failure 1
    assert_output --partial "Cannot draw a subscription id"
}

# The same table as TestCleanSubscriptionName in Go.
@test "substore_clean_name: the daemons' rules" {
    local long
    long=$(printf 'я%.0s' $(seq 1 32))
    [[ $(substore_clean_name '  Alpha  ') == "Alpha" ]]
    [[ $(substore_clean_name 'Бета VPN') == "Бета VPN" ]]
    [[ $(substore_clean_name "$long") == "$long" ]]
    run substore_clean_name "${long}я"
    assert_failure
    assert_output --partial "longer than 32"
    run substore_clean_name '   '
    assert_failure
    run substore_clean_name $'Al\tpha'
    assert_failure
    run substore_clean_name $'\tAlpha'
    assert_failure
    run substore_clean_name $'Al\xc2\x85pha'   # U+0085, a C1 control, as UTF-8 bytes
    assert_failure
    run substore_clean_name $'Al\x7fpha'
    assert_failure
    run substore_clean_name $'\xffAlpha'       # not UTF-8: jq would read U+FFFD
    assert_failure
    assert_output --partial "not valid UTF-8"
}

# The same table as TestSubscriptionNameTaken_FoldsASCIIOnly in Go.
@test "substore_name_taken: ASCII case folding only" {
    local subs='[{"id":"0a1b2c3d","name":"Beta"},{"id":"1b2c3d4e","name":"Бета"}]'
    substore_name_taken "$subs" beta
    substore_name_taken "$subs" BETA
    run ! substore_name_taken "$subs" beta 0a1b2c3d
    run ! substore_name_taken "$subs" бета
    substore_name_taken "$subs" Бета
    run ! substore_name_taken "$subs" Gamma
}

# The same table as TestDefaultSubscriptionName in Go, and a base that is not
# UTF-8: Go drops the byte (strings.ToValidUTF8), and a U+FFFD of a base that
# is UTF-8 stays.
@test "substore_default_name: the daemons' choice" {
    local host40 a31
    host40="$(printf 'a%.0s' $(seq 1 36)).com"
    a31=$(printf 'a%.0s' $(seq 1 31))
    [[ $(substore_default_name '[]' sub.example.com) == "sub.example.com" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"Sub.Example.com"}]' sub.example.com) == "sub.example.com-2" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"sub.example.com"},{"id":"00000001","name":"sub.example.com-2"}]' sub.example.com) == "sub.example.com-3" ]]
    [[ $(substore_default_name '[]' "$host40") == "${host40:0:32}" ]]
    [[ $(substore_default_name "[{\"id\":\"00000000\",\"name\":\"${host40:0:32}\"}]" "$host40") == "${host40:0:30}-2" ]]
    [[ $(substore_default_name '[]' "$a31 bc") == "$a31" ]]
    [[ $(substore_default_name '[]' $'  list\x07.txt ') == "list.txt" ]]
    [[ $(substore_default_name '[]' '') == "subscription" ]]
    [[ $(substore_default_name '[]' $' \x01 ') == "subscription" ]]
    [[ $(substore_default_name '[]' $'sub\xff.example.com') == "sub.example.com" ]]
    [[ $(substore_default_name '[]' $'sub\xef\xbf\xbd') == $'sub\xef\xbf\xbd' ]]
}

@test "substore_ips: every address of every server, sorted, once" {
    run substore_ips "$(substore_list "$FIXTURES")"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"]'
    # Go reads a null in ips as "", which it skips, and a server without ips
    # as none.
    run substore_ips '[{"servers":[{"ips":["192.0.2.1",null,""]},{"ips":null},{}]},{"servers":[]}]'
    assert_output '["192.0.2.1"]'
}

# A list with its outbounds passes the 131,071 bytes a single argument may
# hold well inside the limit of ten subscriptions: every function that takes
# the list has to hand it to jq on stdin.
@test "the functions that take the list read a list past the argument limit" {
    local big config="$BATS_TEST_TMPDIR/vpn-director.json" ips
    big=$(jq -cn '[range(10) as $i | {id: "0000000\($i)", name: "Sub\($i)",
        servers: [range(60) | {name: "S", ips: ["192.0.2.\($i)"], outbound: {pad: ("x" * 300)}}]}]')
    (( ${#big} > 131072 ))
    ips=$(jq -cn '[range(10) | "192.0.2.\(.)"]')
    printf '{}' > "$config"

    [[ $(substore_default_name "$big" sub3) == "sub3-2" ]]
    substore_name_taken "$big" SUB3
    run substore_ips "$big"
    assert_output "$ips"
    substore_sync_config "$config" "$big"
    run jq -c '.xray.servers' "$config"
    assert_output "$ips"
}

@test "substore_write: a file only its owner reads, in a directory only its owner opens" {
    local dir="$BATS_TEST_TMPDIR/data/subscriptions"
    substore_write "$dir" '{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    run stat -c %a "$dir" "$dir/0a1b2c3d.json"
    assert_output $'700\n600'
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' | jq -r '.[0].name'"
    assert_output "Alpha"
    run ls -A "$dir"
    assert_output "0a1b2c3d.json"
}

@test "substore_write: refuses an id the store would not read" {
    run substore_write "$BATS_TEST_TMPDIR" '{"id":"../x","name":"X"}'
    assert_failure
    run substore_write "$BATS_TEST_TMPDIR" '{"id":12345678,"name":"X"}'
    assert_failure
    [[ ! -e $BATS_TEST_TMPDIR/12345678.json ]]
}

@test "substore_delete: removes the file of that id only" {
    local dir="$BATS_TEST_TMPDIR/s"
    mkdir -p "$dir"
    printf '{}' > "$dir/0a1b2c3d.json"
    printf '{}' > "$dir/1b2c3d4e.json"
    printf '{}' > "$BATS_TEST_TMPDIR/0a1b2.json"
    substore_delete "$dir" 0a1b2c3d
    run ! substore_delete "$dir" ../0a1b2
    [[ -e $BATS_TEST_TMPDIR/0a1b2.json ]]
    run ls -A "$dir"
    assert_output "1b2c3d4e.json"
}

@test "substore_lock: waits for another holder, then gives up" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{}' > "$config"
    flock "$BATS_TEST_TMPDIR/.vpn-director.json.lock" sleep 30 3>&- &
    local holder=$!
    sleep 0.5
    VPD_CONFIG_LOCK_WAIT=1 run substore_lock "$config"
    pkill -P "$holder" 2>/dev/null || true
    kill "$holder" 2>/dev/null || true
    assert_failure
}

# The lock the daemons take (service.ConfigService.LockPath), held from
# substore_lock to substore_unlock.
@test "substore_lock: holds the lock beside the config until substore_unlock" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json" lock="$BATS_TEST_TMPDIR/.vpn-director.json.lock"
    printf '{}' > "$config"
    run bash -c "source '$LIB_DIR/substore.sh'
        substore_lock '$config' || exit 1
        flock -n '$lock' true && exit 2
        substore_unlock
        flock -n '$lock' true || exit 3"
    assert_success
}

@test "substore_sync_config: xray.servers in step, the old link gone, the filter applied" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '%s' '{"webui":{"jwt_secret":"s"},"xray":{"clients":["192.168.1.8"],"servers":["9.9.9.9"],"subscription_url":"https://old.example/s/t","preferred_server":{"subscription":"0a1b2c3d","name":"Oslo"}}}' > "$config"

    substore_sync_config "$config" "$(substore_list "$FIXTURES")" 'if (.xray.preferred_server.subscription // "") == "0a1b2c3d" then del(.xray.preferred_server) else . end'

    run jq -c '[.xray.servers, (.xray | has("subscription_url")), (.xray | has("preferred_server")), .webui.jwt_secret, .xray.clients]' "$config"
    assert_output '[["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"],false,false,"s",["192.168.1.8"]]'
    run stat -c %a "$config"
    assert_output "600"
}

@test "substore_sync_config: no config yet is nothing to keep in step" {
    run substore_sync_config "$BATS_TEST_TMPDIR/none.json" '[]'
    assert_success
    [[ ! -e $BATS_TEST_TMPDIR/none.json ]]
}

# Entware's jq is built without oniguruma: a regex builtin works on a
# workstation and fails on the router.
@test "lib/substore.sh: its jq uses no regex builtin" {
    run grep -nE '(^|[^a-zA-Z_])(test|match|capture|scan|splits|sub|gsub)\(|split\([^)]*;' "$LIB_DIR/substore.sh"
    assert_failure
}
