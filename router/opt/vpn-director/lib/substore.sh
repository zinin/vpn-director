#!/usr/bin/env bash
# shellcheck shell=bash

###############################################################################
# substore.sh - the subscription files: <data_dir>/subscriptions/<id>.json, one
# per subscription, holding its link, its status and its servers. The Go
# daemons read and write the same files (server/internal/vpnconfig/substore.go)
# under the same lock, and apply the same rules to ids and names.
#
# Self-contained: configure.sh sources it without lib/common.sh, so nothing
# here calls log(); warnings go to stderr. Entware's jq is built without
# oniguruma, so nothing here uses a regex builtin. A subscription list reaches
# jq on stdin, never as an argument: with its outbounds it outgrows the
# 128 KiB one argument may hold well inside SUBSTORE_MAX.
###############################################################################

# The most subscriptions a router keeps, and the longest name in characters.
# shellcheck disable=SC2034  # read by import_server_list.sh
SUBSTORE_MAX=10
SUBSTORE_NAME_MAX=32

# substore_dir <data_dir> - where the data directory keeps the subscriptions.
substore_dir() {
    printf '%s/subscriptions\n' "$1"
}

# substore_valid_id <id> - an id of 8 lowercase hex digits, the only names the
# store reads or writes.
substore_valid_id() {
    [[ ${#1} -eq 8 && $1 != *[!0123456789abcdef]* ]]
}

# substore_list <dir> - every subscription as one JSON array, ordered by added,
# then by id, as the daemons order them. A file named anything but
# <8 hex digits>.json is no subscription - a temp file of an atomic write
# among them. One that does not parse, whose id is not its name, or with a
# field of the wrong type is skipped with a warning, as the daemons skip what
# json.Unmarshal refuses: the fields of the file and of its servers have the
# types vpnconfig.Subscription and vpnconfig.Server give them, a null being an
# absent field, and a server's outbound, which Go keeps raw, may be anything.
# An absent name or servers reads as "" or [], and a server's absent name or
# address as "", as the daemons read them.
substore_list() {
    local dir=$1 file name id sub
    local -a found=()
    if [[ -d $dir ]]; then
        for file in "$dir"/*.json; do
            [[ -f $file ]] || continue
            name=${file##*/}
            id=${name%.json}
            substore_valid_id "$id" || continue
            sub=$(jq -cs --arg id "$id" '
                def text: . == null or type == "string";
                def texts: . == null or (type == "array" and all(text));
                def server: type == "object"
                    and all(.address, .name, .uuid, .security, .network, .flow, .sni,
                            .fingerprint, .public_key, .short_id; text)
                    and (.port == null or (.port | type == "number"))
                    and all(.ips, .alpn; texts);
                select(length == 1) | .[0]
                | select(type == "object" and .id == $id
                    and all(.name, .url, .added, .refreshed, .error; text)
                    and (.servers == null or (.servers | type == "array" and all(server))))
                | .name //= "" | .servers //= []
                | .servers[] |= (.name //= "" | .address //= "")' "$file" 2>/dev/null) || sub=""
            if [[ -z $sub ]]; then
                printf 'Skipping subscription file %s: it does not parse, its id is not its name, or a field has the wrong type\n' "$name" >&2
                continue
            fi
            found+=("$sub")
        done
    fi
    if [[ ${#found[@]} -eq 0 ]]; then
        printf '[]\n'
        return 0
    fi
    printf '%s\n' "${found[@]}" | jq -cs 'sort_by(.added, .id)'
}

# substore_new_id <dir> - a random id no file in <dir> has yet: 4 bytes of
# /dev/urandom through od or, where od gives nothing (a BusyBox built without
# it), the first 8 hex digits of a kernel uuid. It fails after 20 draws rather
# than spin.
substore_new_id() {
    local id tries
    for ((tries = 0; tries < 20; tries++)); do
        id=$(od -An -N4 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n') || id=""
        if ! substore_valid_id "$id"; then
            id=$(cut -c1-8 /proc/sys/kernel/random/uuid 2>/dev/null) || id=""
        fi
        if substore_valid_id "$id" && [[ ! -e $1/$id.json ]]; then
            printf '%s\n' "$id"
            return 0
        fi
    done
    printf 'Cannot draw a subscription id: neither od nor /proc/sys/kernel/random/uuid gave a free one\n' >&2
    return 1
}

# _substore_is_utf8 <string> - whether <string> is UTF-8. jq reads a byte that
# is not as U+FFFD, so such a string does not come back from jq unchanged; the
# "." keeps $(...) from eating a trailing newline.
_substore_is_utf8() {
    [[ $(jq -jn --arg s "$1" '$s + "."') == "$1." ]]
}

# substore_clean_name <name> - <name> without the spaces around it, when what is
# left is UTF-8 of 1 to 32 characters, none of them a control character (C0,
# DEL, C1); otherwise it fails with the reason on stderr.
# vpnconfig.CleanSubscriptionName applies the same rules.
substore_clean_name() {
    local name=$1 verdict
    name=${name#"${name%%[! ]*}"}
    name=${name%"${name##*[! ]}"}
    if ! _substore_is_utf8 "$name"; then
        printf 'The name is not valid UTF-8\n' >&2
        return 1
    fi
    verdict=$(jq -rn --arg n "$name" --argjson max "$SUBSTORE_NAME_MAX" '
        ($n | explode) as $c
        | if ($c | length) == 0 then "empty"
          elif ($c | length) > $max then "long"
          elif ($c | any(. < 32 or . == 127 or (. >= 128 and . <= 159))) then "control"
          else "ok" end')
    case $verdict in
        ok) printf '%s\n' "$name" ;;
        empty) printf 'The name is empty\n' >&2; return 1 ;;
        long) printf 'The name is longer than %d characters\n' "$SUBSTORE_NAME_MAX" >&2; return 1 ;;
        *) printf 'The name has a control character\n' >&2; return 1 ;;
    esac
}

# substore_name_taken <subs_json> <name> [except_id] - whether a subscription
# other than except_id has <name> under ASCII case folding: jq's
# ascii_downcase, the fold the daemons apply too.
substore_name_taken() {
    jq -e --arg n "$2" --arg except "${3:-}" \
        'any(.[]; .id != $except and ((.name | ascii_downcase) == ($n | ascii_downcase)))' <<< "$1" >/dev/null
}

# substore_default_name <subs_json> <base> - <base> (a link's host, a file's
# name) cut to 32 characters, or <base>-2, <base>-3 and so on, cut so the suffix
# fits, whichever no subscription has yet. Control characters, the spaces
# around <base> and the bytes of a <base> that is not UTF-8 go first (jq reads
# such a byte as U+FFFD); an empty <base> is "subscription".
# vpnconfig.DefaultSubscriptionName makes the same choice.
substore_default_name() {
    local utf8=true
    _substore_is_utf8 "$2" || utf8=false
    jq -r --arg base "$2" --argjson utf8 "$utf8" --argjson max "$SUBSTORE_NAME_MAX" '
        def trim_spaces: explode
            | until(length == 0 or .[0] != 32; .[1:])
            | until(length == 0 or .[-1] != 32; .[:-1])
            | implode;
        . as $subs
        | ($base | explode
            | map(select(. >= 32 and (. < 127 or . > 159) and ($utf8 or . != 65533)))
            | implode | trim_spaces) as $b0
        | (if $b0 == "" then "subscription" else $b0 end) as $b
        | [$subs[].name | ascii_downcase] as $taken
        | first(range(1; $max + 100) as $n
            | (if $n == 1 then "" else "-\($n)" end) as $suffix
            | (($b | .[0:($max - ($suffix | length))]) | trim_spaces) + $suffix
            | select(. as $cand | ($taken | any(. == ($cand | ascii_downcase))) | not))' <<< "$1"
}

# substore_ips <subs_json> - xray.servers for these subscriptions: every address
# of every server, sorted, each once - the set TPROXY_BYPASS takes. A null in
# ips is no address, as it is to the daemons.
substore_ips() {
    jq -c '[.[].servers[]?.ips[]? | strings | select(. != "")] | unique' <<< "$1"
}

# substore_write <dir> <subscription_json> - writes <dir>/<id>.json: a temp file
# beside it, mode 600 - it holds the link and every server's credentials - then
# a rename, so a reader sees the old file or the new one. The caller holds the
# config lock.
substore_write() {
    local dir=$1 sub=$2 id tmp
    id=$(jq -r '.id | strings' <<< "$sub" 2>/dev/null) || id=""
    if ! substore_valid_id "$id"; then
        printf 'Invalid subscription id: %s\n' "$id" >&2
        return 1
    fi
    if ! mkdir -p "$dir" || ! chmod 700 "$dir"; then
        return 1
    fi
    tmp=$(mktemp "$dir/$id.json.XXXXXX") || return 1
    if ! jq . <<< "$sub" > "$tmp" || ! chmod 600 "$tmp" || ! mv -f "$tmp" "$dir/$id.json"; then
        rm -f "$tmp"
        return 1
    fi
}

# substore_delete <dir> <id> - removes <dir>/<id>.json. The caller holds the lock.
substore_delete() {
    if ! substore_valid_id "$2"; then
        printf 'Invalid subscription id: %s\n' "$2" >&2
        return 1
    fi
    rm -f "$1/$2.json"
}

# substore_lock <config> - takes the lock the daemons and configure.sh take
# around vpn-director.json, on FD 9, waiting up to VPD_CONFIG_LOCK_WAIT seconds
# (30). BusyBox flock has no -w, hence the loop.
substore_lock() {
    local waited=0
    exec 9>"${1%/*}/.${1##*/}.lock" || return 1
    until flock -n 9; do
        if [[ $waited -ge ${VPD_CONFIG_LOCK_WAIT:-30} ]]; then
            exec 9>&-
            return 1
        fi
        [[ $waited -eq 0 ]] && printf 'Waiting for the config lock...\n' >&2
        sleep 1
        waited=$((waited + 1))
    done
}

# substore_unlock - releases what substore_lock took.
substore_unlock() {
    flock -u 9
    exec 9>&-
}

# substore_sync_config <config> <subs_json> [jq filter] - under the lock the
# caller holds: xray.servers for <subs_json>, the single link of earlier
# releases gone, and <filter> on top. With no config yet - before configure.sh
# has run - there is nothing to keep in step.
substore_sync_config() {
    local config=$1 subs=$2 filter=${3:-.} ips tmp
    [[ -f $config ]] || return 0
    ips=$(substore_ips "$subs") || return 1
    tmp=$(mktemp "$config.XXXXXX") || return 1
    if ! jq --argjson ips "$ips" ".xray.servers = \$ips | del(.xray.subscription_url) | $filter" "$config" > "$tmp" \
        || ! chmod 600 "$tmp" || ! mv -f "$tmp" "$config"; then
        rm -f "$tmp"
        return 1
    fi
}
