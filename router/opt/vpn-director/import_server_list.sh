#!/bin/sh
# shellcheck shell=bash
# KeeneticOS has no /usr/bin/env and mounts / read-only, so the usual
# "#!/usr/bin/env bash" cannot start this script there. Begin as a POSIX shell
# and hand over to bash by absolute path - Entware's first, a workstation's
# after it. PATH is no help here: Asuswrt-Merlin's /bin/sh has no "command"
# builtin (see .claude/rules/shell-conventions.md) and its /bin/bash is a
# symlink to busybox rather than bash.
if [ -z "${BASH_VERSION:-}" ]; then
    for _vpd_bash in /opt/bin/bash /usr/bin/bash /bin/bash; do
        # Asuswrt-Merlin's /bin/bash is busybox: a POSIX shell that never sets
        # BASH_VERSION, so exec-ing it would re-run this block forever. Only a
        # candidate that proves it is bash gets the script.
        [ -x "$_vpd_bash" ] && "$_vpd_bash" -c '[ -n "$BASH_VERSION" ]' 2>/dev/null &&
            exec "$_vpd_bash" "$0" "$@"
    done
    echo "$0: bash not found; install it (Entware package \"bash\")" >&2
    exit 1
fi
set -euo pipefail

# Debug mode: set DEBUG=1 to enable tracing
if [[ ${DEBUG:-0} == 1 ]]; then
    set -x
    PS4='+${BASH_SOURCE[0]##*/}:${LINENO}:${FUNCNAME[0]:-main}: '
fi

###############################################################################
# import_server_list.sh - the subscriptions of this router, the files
# lib/substore.sh keeps: a menu adds, refreshes, renames and deletes them under
# the config lock, and xray.servers follows every change. A subscription comes
# from a URL or a file: share links (vless, vmess, trojan, ss, hysteria2),
# base64 or plain, or Xray JSON. lib/subscription.sh reads the list - the same
# rules the bot and the Web UI import with - and this script resolves the
# servers. Run after install.sh.
###############################################################################

# Source common utilities (use BASH_SOURCE for correct path when sourced)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/subscription.sh
. "$SCRIPT_DIR/lib/subscription.sh"
# shellcheck source=lib/substore.sh
. "$SCRIPT_DIR/lib/substore.sh"

# Paths; VPD_DIR and VPD_CONFIG can be set by the caller, as configure.sh's can
VPD_DIR="${VPD_DIR:-/opt/vpn-director}"
VPD_CONFIG="${VPD_CONFIG:-$VPD_DIR/vpn-director.json}"
VPD_TEMPLATE="$VPD_DIR/vpn-director.json.template"

###############################################################################
# Helper functions
###############################################################################

read_input() {
    printf "%s: " "$1" >&2
    read -r INPUT_RESULT || INPUT_RESULT=""
}

# abort <message> - logs <message> as an error, leaves it in $FAIL_REASON_FILE
# for a refresh to record, and ends the action it runs in.
abort() {
    log -l ERROR "$1"
    if [[ -n ${FAIL_REASON_FILE:-} ]]; then
        printf '%s' "$1" > "$FAIL_REASON_FILE"
    fi
    exit 1
}

# run_action <function> [args] - runs one action in a subshell with errexit
# on: its abort or exit ends the action and not the menu. ACTION_RC is its
# status. Never call it on the left of || or &&: bash turns errexit off inside.
run_action() {
    set +e
    ( set -e; "$@" )
    ACTION_RC=$?
    set -e
}

# jq that drops control characters - C0, DEL and C1 - from a string. A skip's
# name and detail and a server's address are subscription text, and would
# otherwise take an escape sequence or a line break of their own to the
# terminal, the log file and syslog.
JQ_PRINTABLE='def printable: explode | map(select(. >= 32 and (. < 127 or . > 159))) | implode;'

# jq that gives the host of a link, as Go's url.Hostname does: after "//", up
# to the first "/", "?" or "#", without the user info up to the last "@", then
# what "[...]" holds - an IPv6 address - or what comes before ":port". The host
# stands in for the link wherever one is shown: the link's path or query
# carries its token. No regex: Entware's jq has none.
JQ_HOST='def host:
    def upto(s): if . == "" then . else split(s)[0] end;
    split("//") | if length < 2 then "" else .[1:] | join("//") end
    | upto("/") | upto("?") | upto("#")
    | if . == "" then . else split("@") | last end
    | if startswith("[") then .[1:] | upto("]") else upto(":") end;'

# link_host <link> - the host of <link> without control characters: the
# default name of a subscription added from it, and all a message shows of it.
link_host() {
    jq -Rr "$JQ_PRINTABLE$JQ_HOST"' host | printable' <<< "$1"
}

# link_scheme <input> - "https" or "http" when <input> is a link of that
# scheme, in any case: Go's url.Parse reads "Https://" as https, and the Web
# UI and the bot save a link as it was written. Nothing for anything else.
link_scheme() {
    case $1 in
        [Hh][Tt][Tt][Pp][Ss]://*) printf 'https\n' ;;
        [Hh][Tt][Tt][Pp]://*) printf 'http\n' ;;
    esac
}

###############################################################################
# Get data directory from config
###############################################################################

get_data_dir() {
    local config_file="$VPD_CONFIG"

    # Fall back to template if config doesn't exist
    if [[ ! -f "$config_file" ]]; then
        config_file="$VPD_TEMPLATE"
    fi

    if [[ ! -f "$config_file" ]]; then
        log -l ERROR "Config not found: $VPD_CONFIG or $VPD_TEMPLATE"
        exit 1
    fi

    jq -r '.data_dir // "/opt/vpn-director/data"' "$config_file"
}

###############################################################################
# Step 1: Get the subscription
###############################################################################

# step_get_subscription - asks for a URL or a file and reads it
# (fetch_subscription).
step_get_subscription() {
    log -l TRACE "Step 1: Subscription"

    printf "Enter a subscription URL or the path to a file:\n"
    printf "(Share links - vless, vmess, trojan, ss, hysteria2 - base64 or plain, or Xray JSON)\n\n"

    read_input "URL or path"
    fetch_subscription "$INPUT_RESULT"
}

# fetch_subscription <link or path> - downloads the link or reads the file into
# SUB_RESULT, the Result JSON of subscription_decode, and logs every entry it
# skipped. SUB_INPUT keeps <link or path>.
fetch_subscription() {
    SUB_INPUT=$1

    if [[ -z "$SUB_INPUT" ]]; then
        abort "No input provided"
    fi

    # The largest subscription taken, in bytes: 1 MiB, the cap of the daemons'
    # download (service.MaxSubscriptionBody). A larger one is refused, never
    # cut short: cut, a base64 list decodes to a shorter one.
    local -r max_bytes=1048576
    local content rc=0
    case $(link_scheme "$SUB_INPUT") in
        http|https)
            log "Downloading from URL..."
            content=$(curl -fsSL --connect-timeout 10 --max-time 60 --max-filesize "$max_bytes" "$SUB_INPUT") || rc=$?
            if (( rc == 63 )); then
                # curl's "maximum file size exceeded"
                abort "Subscription exceeds 1 MiB; nothing was imported"
            elif (( rc != 0 )); then
                abort "Failed to download the subscription"
            fi
            # curl before 8.4.0 does not stop a transfer whose size it did not
            # know in advance.
            if (( $(printf '%s' "$content" | wc -c) > max_bytes )); then
                abort "Subscription exceeds 1 MiB; nothing was imported"
            fi
            ;;
        *)
            if [[ ! -f "$SUB_INPUT" ]]; then
                if [[ $SUB_INPUT == *://* ]]; then
                    # A link of another scheme - a share link pasted in place
                    # of its subscription, say - shows nothing of itself: even
                    # its "host" can be a key, as a vmess link's base64
                    # payload is.
                    abort "Unsupported link: only http and https links are downloaded"
                fi
                abort "File not found: $SUB_INPUT"
            fi
            if (( $(wc -c < "$SUB_INPUT") > max_bytes )); then
                abort "Subscription exceeds 1 MiB; nothing was imported"
            fi
            content=$(cat "$SUB_INPUT")
            ;;
    esac

    local err
    err=$(tmp_file)
    if ! SUB_RESULT=$(printf '%s' "$content" | subscription_decode 2>"$err"); then
        abort "Cannot read the subscription: $(cat "$err")"
    fi

    local skipped line
    skipped=$(printf '%s' "$SUB_RESULT" | jq -r "$JQ_PRINTABLE"'
        .skipped[] | "Skipping \(.name | printable): \(.reason) (\(.detail | printable))"')
    if [[ -n $skipped ]]; then
        while IFS= read -r line; do
            log -l WARN "$line"
        done <<< "$skipped"
    fi

    if [[ $(printf '%s' "$SUB_RESULT" | jq '.servers | length') -eq 0 ]]; then
        abort "No supported servers in subscription"
    fi
}

###############################################################################
# Step 2: Resolve servers
###############################################################################

# step_parse_servers - resolves the servers of SUB_RESULT into $SERVERS_TMP, a
# temp file the caller may have set: a refresh resolves in a subshell and reads
# the list from that file once the subshell is gone. Resolving takes seconds
# per host; a subscription's file is written once, with the whole list, and
# not at all when no server resolved.
step_parse_servers() {
    log -l TRACE "Step 2: Resolving Servers"

    SERVERS_TMP=${SERVERS_TMP:-$(tmp_file)}

    local records server address shown_address name ips_raw ips_json dns_errors=0
    records=$(tmp_file)
    while IFS= read -r server; do
        # The address as it is for the lookup, a printable copy for the output.
        address=$(printf '%s' "$server" | jq -r '.address')
        shown_address=$(printf '%s' "$server" | jq -r "$JQ_PRINTABLE"' .address | printable')
        name=$(printf '%s' "$server" | jq -r "$JQ_PRINTABLE"' .name | printable')
        ips_raw=$(resolve_ip -a -q "$address" 2>/dev/null) || ips_raw=""
        if [[ -z "$ips_raw" ]]; then
            log -l WARN "Cannot resolve $shown_address, skipping"
            dns_errors=$((dns_errors + 1))
            continue
        fi
        printf "  %s (%s) -> %s\n" "$name" "$shown_address" "$(printf '%s' "$ips_raw" | tr '\n' ',' | sed 's/,$//')" >&2
        ips_json=$(printf '%s\n' "$ips_raw" | jq -R 'select(length > 0)' | jq -s .)
        printf '%s' "$server" | jq -c --argjson ips "$ips_json" '. + {ips: $ips}' >> "$records"
    done <<< "$(printf '%s' "$SUB_RESULT" | jq -c '.servers[]')"
    jq -s '.' "$records" > "$SERVERS_TMP"

    SERVER_COUNT=$(jq length "$SERVERS_TMP")

    # The counts in the order the bot and the Web UI give them
    # (subscription.Import.Counts).
    local counts
    counts=$(printf '%s' "$SUB_RESULT" | jq -r --argjson dns "$dns_errors" '
        (reduce .skipped[].reason as $r ({}; .[$r] += 1)) as $c
        | [ (("unsupported", "composite", "invalid") as $r | select(($c[$r] // 0) > 0) | "\($c[$r]) \($r)"),
            (($c.placeholder // 0) | if . == 1 then "1 placeholder" elif . > 1 then "\(.) placeholders" else empty end),
            ($dns | if . == 1 then "1 DNS error" elif . > 1 then "\(.) DNS errors" else empty end) ]
        | join(", ")')
    if [[ -n $counts ]]; then
        log "Found $SERVER_COUNT servers in $(printf '%s' "$SUB_RESULT" | jq '.total') entries ($counts)"
    else
        log "Found $SERVER_COUNT servers"
    fi

    if [[ "$SERVER_COUNT" -eq 0 ]]; then
        abort "No servers could be resolved"
    fi
}

###############################################################################
# Publishing: every write under the config lock
###############################################################################

# now - the time as the subscription files keep it: UTC, whole seconds.
now() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

# store_and_sync <subscription_json> - under the lock the caller holds: writes
# the file, brings xray.servers in step, takes away what the previous release
# kept (servers.json, xray.subscription_url), and releases the lock.
store_and_sync() {
    if ! substore_write "$SUB_DIR" "$1"; then
        substore_unlock
        abort "Failed to write the subscription; nothing was saved"
    fi
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")"; then
        substore_unlock
        abort "The subscription is saved, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    substore_unlock
}

# publish_add <input> <name> <base> - saves $SERVERS_TMP as a subscription. An
# https link already saved makes it a refresh of that subscription, and a
# rename too when <name> is given; a file or a plain-http link is a static
# list, always a new one. <base> names a new subscription when <name> is empty.
publish_add() {
    local input=$1 name=$2 base=$3 url="" subs id sub stamp
    [[ $(link_scheme "$input") == https ]] && url=$input
    stamp=$(now)
    if ! substore_lock "$VPD_CONFIG"; then
        abort "Config is locked by the Web UI or the bot; nothing was imported. Run the import again"
    fi
    subs=$(substore_list "$SUB_DIR")
    id=""
    if [[ -n $url ]]; then
        id=$(jq -r --arg u "$url" 'first(.[] | select(.url == $u) | .id) // ""' <<< "$subs")
    fi
    if [[ -n $id ]]; then
        sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$subs")
        if [[ -n $name && $name != "$(jq -r '.name' <<< "$sub")" ]]; then
            if substore_name_taken "$subs" "$name" "$id"; then
                substore_unlock
                abort "Another subscription is named $name; nothing was imported"
            fi
            sub=$(jq -c --arg n "$name" '.name = $n' <<< "$sub")
        fi
        log "The link is saved already as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub"); its list is refreshed"
    else
        if (( $(jq length <<< "$subs") >= SUBSTORE_MAX )); then
            substore_unlock
            abort "There are $SUBSTORE_MAX subscriptions already; delete one first"
        fi
        if [[ -z $name ]]; then
            name=$(substore_default_name "$subs" "$base")
        elif substore_name_taken "$subs" "$name"; then
            substore_unlock
            abort "Another subscription is named $name; nothing was imported"
        fi
        id=$(substore_new_id "$SUB_DIR")
        # The fields in the order the daemons write them, the list last.
        sub=$(jq -nc --arg id "$id" --arg n "$name" --arg u "$url" --arg now "$stamp" \
            '{id: $id, name: $n} + (if $u == "" then {} else {url: $u} end) + {added: $now}')
    fi
    sub=$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$stamp" \
        '.refreshed = $now | del(.error) | .servers = $servers[0]' <<< "$sub")
    store_and_sync "$sub"
    log "Saved $SERVER_COUNT servers as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")"
}

# publish_refresh <id> <link> <name> - saves $SERVERS_TMP as the list of
# subscription <id>, only while it still exists with <link>: one deleted, or
# deleted and added again, while its download ran is not brought back.
publish_refresh() {
    local id=$1 url=$2 name=$3 sub
    if ! substore_lock "$VPD_CONFIG"; then
        abort "Config is locked by the Web UI or the bot; $name was not refreshed"
    fi
    sub=$(jq -c --arg id "$id" --arg u "$url" 'first(.[] | select(.id == $id and .url == $u)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -z $sub ]]; then
        substore_unlock
        abort "$name was deleted or changed while it downloaded; nothing was written"
    fi
    store_and_sync "$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$(now)" \
        '.refreshed = $now | del(.error) | .servers = $servers[0]' <<< "$sub")"
    log "Refreshed $name: $SERVER_COUNT servers"
}

# record_refresh_error <id> <link> <refreshed> <reason> - notes why a refresh
# failed; the list stays. Nothing is written when the subscription is gone,
# has another link, or was refreshed since <refreshed> by someone else. A lock
# or a write that fails is a WARN, as it is to the daemons: the file then
# still reads as refreshed.
record_refresh_error() {
    local id=$1 url=$2 seen=$3 reason=$4 sub
    if ! substore_lock "$VPD_CONFIG"; then
        log -l WARN "Failed to record why the subscription did not refresh: the config is locked by the Web UI or the bot"
        return 0
    fi
    sub=$(jq -c --arg id "$id" --arg u "$url" --arg seen "$seen" \
        'first(.[] | select(.id == $id and .url == $u and (.refreshed // "") == $seen)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -n $sub ]] && ! substore_write "$SUB_DIR" "$(jq -c --arg r "$reason" '.error = $r' <<< "$sub")"; then
        log -l WARN "Failed to record why the subscription did not refresh"
    fi
    substore_unlock
}

###############################################################################
# The menu
###############################################################################

# show_subscriptions <subs_json> - the subscriptions, numbered, a line each.
# The files keep UTC, and the time says so: the log lines around it are local.
show_subscriptions() {
    printf '\nSubscriptions:\n'
    jq -r "$JQ_PRINTABLE$JQ_HOST"'
        to_entries[]
        | .key as $i | .value
        | "  \($i + 1)) \(.name | printable)   \(if (.url // "") == "" then "static list" else (.url | host | printable) end)   \(.servers | length) servers   "
          + (if (.error // "") != "" then "error: \(.error | printable)"
             else "refreshed \((.refreshed // "") | .[0:10]) \((.refreshed // "") | .[11:16]) UTC" end)' <<< "$1"
}

# pick_subscription <subs_json> <prompt> - asks for a number of the list and
# prints the id at it; fails for an answer that is no number of it, and does
# not repeat that answer: a link pasted there carries its token. 10# reads
# "08" as eight, where bash would take it for octal.
pick_subscription() {
    local count
    count=$(jq length <<< "$1")
    (( count > 0 )) || abort "There is no subscription yet"
    read_input "$2 [1-$count]"
    if [[ $INPUT_RESULT =~ ^[0-9]+$ ]] && (( 10#$INPUT_RESULT >= 1 && 10#$INPUT_RESULT <= count )); then
        jq -r ".[$((10#$INPUT_RESULT - 1))].id" <<< "$1"
        return 0
    fi
    abort "There is no subscription with that number"
}

# menu_add - asks for a link or a file and a name, downloads and resolves the
# list, and saves it (publish_add).
menu_add() {
    local base name=""
    step_get_subscription
    case $(link_scheme "$SUB_INPUT") in
        http|https) base=$(link_host "$SUB_INPUT") ;;
        *) base=${SUB_INPUT##*/}; base=${base%.*} ;;
    esac
    read_input "Name (Enter for $base)"
    if [[ -n $INPUT_RESULT ]]; then
        name=$(substore_clean_name "$INPUT_RESULT") || abort "The name was refused; nothing was imported"
    fi
    step_parse_servers
    publish_add "$SUB_INPUT" "$name" "$base"
}

# fetch_and_resolve <link> - one refresh's download and resolution.
fetch_and_resolve() {
    fetch_subscription "$1"
    step_parse_servers
}

# refresh_one <subscription_json> - downloads the subscription's link again
# and publishes the list. A download or a list that fails is recorded in the
# subscription, whose list stays, and ends the action.
refresh_one() {
    local sub=$1 id url seen name reason
    id=$(jq -r '.id' <<< "$sub")
    url=$(jq -r '.url' <<< "$sub")
    seen=$(jq -r '.refreshed // ""' <<< "$sub")
    name=$(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")
    log "Refreshing $name"
    SERVERS_TMP=$(tmp_file)
    FAIL_REASON_FILE=$(tmp_file)
    run_action fetch_and_resolve "$url"
    if (( ACTION_RC != 0 )); then
        reason=$(cat "$FAIL_REASON_FILE")
        record_refresh_error "$id" "$url" "$seen" "${reason:-the refresh failed}"
        exit 1
    fi
    FAIL_REASON_FILE=""
    SERVER_COUNT=$(jq length "$SERVERS_TMP")
    publish_refresh "$id" "$url" "$name"
}

# linked_subscriptions <subs_json> - those that have a link to refresh.
linked_subscriptions() {
    jq -c 'map(select((.url // "") != ""))' <<< "$1"
}

menu_refresh() {
    local linked id
    linked=$(linked_subscriptions "$1")
    [[ $(jq length <<< "$linked") -gt 0 ]] || abort "No subscription has a link to refresh"
    show_subscriptions "$linked"
    id=$(pick_subscription "$linked" "Refresh subscription")
    refresh_one "$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$linked")"
}

menu_refresh_all() {
    local sub total=0 failed=0
    while IFS= read -r sub; do
        total=$((total + 1))
        run_action refresh_one "$sub"
        if (( ACTION_RC != 0 )); then
            failed=$((failed + 1))
        fi
    done < <(jq -c '.[]' <<< "$(linked_subscriptions "$1")")
    log "Refreshed $((total - failed)) of $total subscriptions"
}

menu_rename() {
    local subs=$1 id sub name
    id=$(pick_subscription "$subs" "Rename subscription")
    read_input "New name"
    name=$(substore_clean_name "$INPUT_RESULT") || abort "The name was refused; nothing was renamed"
    substore_lock "$VPD_CONFIG" || abort "Config is locked by the Web UI or the bot; nothing was renamed"
    subs=$(substore_list "$SUB_DIR")
    sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id)) // empty' <<< "$subs")
    if [[ -z $sub ]]; then
        substore_unlock
        abort "That subscription is gone"
    fi
    if substore_name_taken "$subs" "$name" "$id"; then
        substore_unlock
        abort "Another subscription is named $name"
    fi
    store_and_sync "$(jq -c --arg n "$name" '.name = $n' <<< "$sub")"
    log "Renamed to $name"
}

menu_delete() {
    local subs=$1 id name active=""
    id=$(pick_subscription "$subs" "Delete subscription")
    name=$(jq -r --arg id "$id" "$JQ_PRINTABLE"' first(.[] | select(.id == $id)) | .name | printable' <<< "$subs")
    read_input "Delete $name and its servers? [y/N]"
    if [[ $INPUT_RESULT != [yY]* ]]; then
        log "Nothing was deleted"
        return 0
    fi
    substore_lock "$VPD_CONFIG" || abort "Config is locked by the Web UI or the bot; nothing was deleted"
    if ! substore_delete "$SUB_DIR" "$id"; then
        substore_unlock
        abort "Failed to delete $name; nothing was deleted"
    fi
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")" \
        "if (.xray.preferred_server.subscription // \"\") == \"$id\" then del(.xray.preferred_server) else . end"; then
        substore_unlock
        abort "$name is deleted, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    if [[ -f $VPD_CONFIG ]]; then
        active=$(jq -r '.xray.active_server.subscription // ""' "$VPD_CONFIG")
    fi
    substore_unlock
    log "Deleted $name"
    if [[ $active == "$id" ]]; then
        log -l WARN "The running Xray server came from $name. Xray keeps running it until you select another (configure.sh, the Web UI or /xray)"
    fi
}

menu() {
    local subs
    while :; do
        subs=$(substore_list "$SUB_DIR")
        show_subscriptions "$subs"
        printf '\na) Add  r) Refresh  R) Refresh all  n) Rename  d) Delete  q) Quit\n'
        read_input "Choice"
        case $INPUT_RESULT in
            a) run_action menu_add ;;
            r) run_action menu_refresh "$subs" ;;
            R) run_action menu_refresh_all "$subs" ;;
            n) run_action menu_rename "$subs" ;;
            d) run_action menu_delete "$subs" ;;
            q|Q|"") return 0 ;;
            # Not repeated: a link pasted here carries its token.
            *) printf 'Unknown choice; answer a, r, R, n, d or q\n' ;;
        esac
    done
}

###############################################################################
# Main
###############################################################################

main() {
    log -l TRACE "Import Server List"
    printf "Subscriptions: the server lists this router can run.\n"

    DATA_DIR=$(get_data_dir)
    SUB_DIR=$(substore_dir "$DATA_DIR")
    if [[ $(substore_list "$SUB_DIR" | jq length) -eq 0 ]]; then
        # A first run: there is nothing to choose from yet.
        run_action menu_add
        if (( ACTION_RC != 0 )); then
            exit "$ACTION_RC"
        fi
    fi
    menu

    log -l TRACE "Import Complete"
    printf "Run /opt/vpn-director/configure.sh to select a server and finish the setup.\n"
}

if [[ "${IMPORT_TEST_MODE:-0}" != "1" ]]; then
    main "$@"
fi
