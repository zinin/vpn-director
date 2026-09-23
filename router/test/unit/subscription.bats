#!/usr/bin/env bats
load '../test_helper'

# The cases testdata/subscription holds are the Go importer's as well
# (server/internal/subscription/fixtures_test.go): an entry the two decoders
# read differently fails one of the two suites.
FIXTURES="$PROJECT_ROOT/../testdata/subscription"

setup() {
    source "$LIB_DIR/subscription.sh"
}

# decode_fixture <in>: what a .want.json holds - the error, or the total, the
# servers and the skips by name and reason. A skip's detail is prose and is
# not compared.
decode_fixture() {
    local out rc=0
    out=$(subscription_decode < "$1" 2>"$BATS_TEST_TMPDIR/err") || rc=$?
    if (( rc != 0 )); then
        jq -Sn --arg e "$(cat "$BATS_TEST_TMPDIR/err")" '{error: $e}'
    else
        printf '%s' "$out" | jq -S '{total, servers, skipped: [.skipped[] | {name, reason}]}'
    fi
}

check_fixtures() {
    local in want got failed=0
    for in in "$FIXTURES"/*.in; do
        want=$(jq -S . "${in%.in}.want.json")
        got=$(decode_fixture "$in")
        if [[ $got != "$want" ]]; then
            printf '== %s decoded differently from its .want.json:\n' "${in##*/}"
            diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") || true
            failed=1
        fi
    done
    return "$failed"
}

@test "subscription_decode: every shared case decodes as its .want.json says" {
    run check_fixtures
    assert_success
}

# The routers run with no UTF-8 locale or with one; a byte-wise name filter
# and ASCII patterns must not care which.
@test "subscription_decode: the cases decode the same under a UTF-8 locale" {
    local utf8
    utf8=$(locale -a 2>/dev/null | grep -i -m1 -E '^(C|en_US)\.utf-?8$') || skip "no UTF-8 locale here"
    LC_ALL=$utf8 run check_fixtures
    assert_success
}

# A .want.json compares name and reason only: a detail is prose (spec 6.7).
# But import_server_list.sh prints it per skipped entry and the bot sends it on
# in Import.Details, so the same subscription must explain itself the same way
# on the router and in Telegram. These are the answers the Go decoder gives the
# same seven inputs.
@test "subscription_decode: a skip's detail words the reason as the Go decoder does" {
    local vmess
    vmess=$(printf '%s' '{"add":"h.example.com","port":443,"id":"11111111-1111-4111-8111-111111111111","host":"a\u0000b","ps":"Nul host"}' |
        base64 | tr -d '\n')
    detail_of() { subscription_decode <<< "$1" | jq -r '.skipped[0].detail'; }

    [ "$(detail_of 'vless://u@h.example.com:443?a=%zz#Bad escape')" = "query: bad percent escape" ]
    [ "$(detail_of 'vless://u@h.example.com:443?a=%00#Nul query')" = "query: NUL byte" ]
    [ "$(detail_of 'vless://%zz@h.example.com:443#Bad userinfo')" = "userinfo: bad percent escape" ]
    [ "$(detail_of 'vless://%00@h.example.com:443#Nul userinfo')" = "userinfo: NUL byte" ]
    [ "$(detail_of 'ss://%00@h.example.com:8388#Nul ss userinfo')" = "userinfo: NUL byte" ]
    [ "$(detail_of 'hysteria2://a@h.example.com:443?sni=%00#Nul hy2 query')" = "query: NUL byte" ]
    [ "$(detail_of "vmess://$vmess")" = "NUL byte in host" ]
    [ "$(detail_of "$(< "$FIXTURES/vmess-nul-port.in")")" = "NUL byte in port" ]
    [ "$(detail_of 'vless://u@h.example.com:4"4\5#Bad port')" = 'bad port "4"4\5"' ]
}

# Entware's jq is built without oniguruma: a regex builtin works on a
# workstation and fails on the router. _sub_clean_names is gawk, where sub and
# gsub are native.
@test "lib/subscription.sh: its jq uses no regex builtin" {
    run bash -c "sed '/^_sub_clean_names() {/,/^}/d' '$LIB_DIR/subscription.sh' |
        grep -nE '(^|[^a-zA-Z_])(test|match|capture|scan|splits|sub|gsub)\\(|split\\([^)]*;'"
    assert_failure
}

@test "_sub_unescape: strict %XX, + as a space only in a query" {
    _sub_unescape 'h2%2Chttp%2F1.1' 1
    [ "$_SUB_U" = "h2,http/1.1" ]
    _sub_unescape 'a+b%2B' 1
    [ "$_SUB_U" = "a b+" ]
    _sub_unescape 'a+b%2B' 0
    [ "$_SUB_U" = "a+b+" ]
    _sub_unescape 'back\slash%0A' 0
    [ "$_SUB_U" = $'back\\slash\n' ]
    run _sub_unescape '50%' 1
    assert_failure
    run _sub_unescape 'x%2y' 1
    assert_failure
    run _sub_unescape '%%41' 1
    assert_failure
    run _sub_unescape 'a%00b' 1
    assert_failure
}

@test "_sub_b64: either alphabet, padding optional, whitespace ignored" {
    [ "$(_sub_b64 'Pz8/Pw==')" = "????" ]
    [ "$(_sub_b64 'Pz8_Pw')" = "????" ]
    [ "$(_sub_b64 'Pj4-Pz8_')" = ">>>???" ]
    [ "$(_sub_b64 $'Pj4+\nPz8/\r\n')" = ">>>???" ]
    [ "$(_sub_b64 'YQBi')" = "ab" ]
    run _sub_b64 'a'
    assert_failure
    run _sub_b64 'ab=c'
    assert_failure
    run _sub_b64 '@@@'
    assert_failure
}

# The byte machine of cleanName (server/internal/subscription/names.go).
@test "_sub_clean_names: keeps two-byte letters, drops the rest, as the Go side does" {
    run _sub_clean_names <<< $'0\t\xF0\x9F\x87\xB3\xF0\x9F\x87\xB1 Амстердам, Нидерланды
0\t  Türkiye   Istanbul , 
0\tΕλλάδα
0\t\xC3|evil
0\t\xE2AB
0\t\xF0ABC
1\t100%25%20off%2%zz+plus%00!'
    assert_success
    assert_output '"Амстердам, Нидерланды"
"Türkiye Istanbul"
"Ελλάδα"
"evil"
"AB"
"ABC"
"100 off2zz plus!"'
}

# jq reads 4.43e2 as 443 - and without decNumber 443.0 as well - so a "port"
# literal that is not plain digits is marked in the text before jq reads it.
# Nothing else changes: not another key's number, not text inside a string.
@test "_sub_mark_ports: marks a port literal that is not plain digits, and nothing else" {
    run _sub_mark_ports <<< '{"port": 4.43e2, "a": {"port":443.0}, "b": [{"port" : 1e2}], "port2": 1.5, "c": "\"port\": 1.5", "port": 443, "d": "x\\", "port": -5, "e": "port", "f": 2.5}'
    assert_success
    assert_output '{"port": "\u0000port 4.43e2", "a": {"port":"\u0000port 443.0"}, "b": [{"port" : "\u0000port 1e2"}], "port2": 1.5, "c": "\"port\": 1.5", "port": 443, "d": "x\\", "port": "\u0000port -5", "e": "port", "f": 2.5}'
}

# A port marked outside the target - here the one the download half of an
# xhttp stream dials - goes back into the stored outbound as the number it was.
@test "subscription_decode: a marked port beside the target is stored as the number it was" {
    run subscription_decode <<< '{"remarks":"Split","outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"sp.example.com","port":443,"users":[{"id":"12121212-1212-4212-8212-121212121212","encryption":"none"}]}]},"streamSettings":{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","extra":{"downloadSettings":{"address":"dl.example.com","port":8443.0}}}}}]}'
    assert_success
    [ "$(jq -c '.servers[0].outbound.streamSettings.xhttpSettings.extra.downloadSettings.port | [type, . == 8443]' <<< "$output")" = '["number",true]' ]
}
