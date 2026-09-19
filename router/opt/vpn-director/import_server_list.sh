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
# import_server_list.sh - Import servers from a subscription URL or a file:
# share links (vless, vmess, trojan, ss, hysteria2), base64 or plain, or Xray
# JSON. lib/subscription.sh reads the list - the same rules the bot and the
# Web UI import with - and this script resolves the servers and publishes the
# list under the config lock. Run after install.sh.
###############################################################################

# Source common utilities (use BASH_SOURCE for correct path when sourced)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/subscription.sh
. "$SCRIPT_DIR/lib/subscription.sh"

# Paths; VPD_DIR and VPD_CONFIG can be set by the caller, as configure.sh's can
VPD_DIR="${VPD_DIR:-/opt/vpn-director}"
VPD_CONFIG="${VPD_CONFIG:-$VPD_DIR/vpn-director.json}"
VPD_TEMPLATE="$VPD_DIR/vpn-director.json.template"

###############################################################################
# Helper functions
###############################################################################

read_input() {
    printf "%s: " "$1" >&2
    read -r INPUT_RESULT
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

# Reads the subscription into SUB_RESULT, the Result JSON of
# subscription_decode, and logs every entry it skipped.
step_get_subscription() {
    log -l TRACE "Step 1: Subscription"

    printf "Enter a subscription URL or the path to a file:\n"
    printf "(Share links - vless, vmess, trojan, ss, hysteria2 - base64 or plain, or Xray JSON)\n\n"

    read_input "URL or path"
    SUB_INPUT="$INPUT_RESULT"

    if [[ -z "$SUB_INPUT" ]]; then
        log -l ERROR "No input provided"
        exit 1
    fi

    local content
    case "$SUB_INPUT" in
        http://*|https://*)
            log "Downloading from URL..."
            content=$(curl -fsSL --connect-timeout 10 --max-time 60 "$SUB_INPUT") || {
                log -l ERROR "Failed to download from $SUB_INPUT"
                exit 1
            }
            ;;
        *)
            if [[ ! -f "$SUB_INPUT" ]]; then
                log -l ERROR "File not found: $SUB_INPUT"
                exit 1
            fi
            content=$(cat "$SUB_INPUT")
            ;;
    esac

    local err
    err=$(tmp_file)
    if ! SUB_RESULT=$(printf '%s' "$content" | subscription_decode 2>"$err"); then
        log -l ERROR "Cannot read the subscription: $(cat "$err")"
        exit 1
    fi

    local skipped line
    skipped=$(printf '%s' "$SUB_RESULT" | jq -r '.skipped[] | "Skipping \(.name): \(.reason) (\(.detail))"')
    if [[ -n $skipped ]]; then
        while IFS= read -r line; do
            log -l WARN "$line"
        done <<< "$skipped"
    fi

    if [[ $(printf '%s' "$SUB_RESULT" | jq '.servers | length') -eq 0 ]]; then
        log -l ERROR "No supported servers in subscription"
        exit 1
    fi
}

###############################################################################
# Step 2: Resolve servers
###############################################################################

# The list goes to a temp file, and step 3 publishes it. Resolving takes
# seconds per host, and servers.json written in place sat empty for all of
# them - and was gone when none resolved.
step_parse_servers() {
    log -l TRACE "Step 2: Resolving Servers"

    DATA_DIR=$(get_data_dir)
    SERVERS_FILE="$DATA_DIR/servers.json"
    SERVERS_TMP=$(tmp_file)

    local records server address name ips_raw ips_json dns_errors=0
    records=$(tmp_file)
    while IFS= read -r server; do
        address=$(printf '%s' "$server" | jq -r '.address')
        name=$(printf '%s' "$server" | jq -r '.name')
        ips_raw=$(resolve_ip -a -q "$address" 2>/dev/null) || ips_raw=""
        if [[ -z "$ips_raw" ]]; then
            log -l WARN "Cannot resolve $address, skipping"
            dns_errors=$((dns_errors + 1))
            continue
        fi
        printf "  %s (%s) -> %s\n" "$name" "$address" "$(printf '%s' "$ips_raw" | tr '\n' ',' | sed 's/,$//')" >&2
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
        log -l ERROR "No servers could be resolved"
        exit 1
    fi
}

###############################################################################
# Step 3: Publish the list with the link it came from
###############################################################################

# servers.json, xray.servers (the proxy's own addresses, which TPROXY bypasses)
# and xray.subscription_url go out together, under the lock the daemons and
# configure.sh take. The bot's subscription watch, the Web UI and /import
# publish the same three under it; a list written outside it could land beside
# another import's bypass set or link. Nothing is written before the lock is
# held, so an import that cannot get it leaves the previous one whole.
#
# The watch re-imports xray.subscription_url when the Xray outbound dies, and
# the Web UI and /import re-import it on request. A list imported here from
# another link would be replaced by the old link's on the next refresh, so an
# https link is saved with it. Anything else clears the saved link: neither
# fetches a file or a plain-http link, and the old link no longer produced the
# list. Before configure.sh has run there is no config: the list alone is
# published, and nothing refreshes it.
step_publish_servers() {
    local link='del(.xray.subscription_url)'
    if [[ $SUB_INPUT == https://* ]]; then
        # shellcheck disable=SC2016  # $url is jq's, set with --arg below
        link='.xray.subscription_url = $url'
    fi
    local ips
    ips=$(jq -c '[.[].ips[]?] | unique' "$SERVERS_TMP")
    mkdir -p "$DATA_DIR"

    # BusyBox flock has no -w, hence the loop.
    exec 9>"${VPD_CONFIG%/*}/.${VPD_CONFIG##*/}.lock"
    local waited=0
    until flock -n 9; do
        if [[ $waited -ge ${VPD_CONFIG_LOCK_WAIT:-30} ]]; then
            exec 9>&-
            log -l ERROR "Config is locked by the Web UI or the bot; nothing was imported. Run the import again"
            return 1
        fi
        [[ $waited -eq 0 ]] && log "Waiting for the config lock..."
        sleep 1
        waited=$((waited + 1))
    done

    # Temp files beside their targets and renames: a '>' redirect would
    # truncate a live file before its new content is there, and jq would read
    # the config it had just truncated.
    local list config=""
    list=$(mktemp "$SERVERS_FILE.XXXXXX")
    cp "$SERVERS_TMP" "$list"
    chmod 600 "$list"
    if [[ -f $VPD_CONFIG ]]; then
        config=$(mktemp "$VPD_CONFIG.XXXXXX")
        if ! jq --argjson ips "$ips" --arg url "$SUB_INPUT" ".xray.servers = \$ips | $link" \
            "$VPD_CONFIG" > "$config"; then
            rm -f "$list" "$config"
            flock -u 9
            exec 9>&-
            log -l ERROR "Failed to update $VPD_CONFIG; nothing was imported"
            return 1
        fi
        chmod 600 "$config"
    fi
    mv -f "$list" "$SERVERS_FILE"
    [[ -z $config ]] || mv -f "$config" "$VPD_CONFIG"
    flock -u 9
    exec 9>&-

    log "Saved $SERVER_COUNT servers to $SERVERS_FILE"
}

###############################################################################
# Main
###############################################################################

main() {
    log -l TRACE "Import Server List"
    printf "This will download and parse the servers of a subscription.\n\n"

    step_get_subscription
    step_parse_servers
    step_publish_servers

    log -l TRACE "Import Complete"
    printf "Server list saved. Run /opt/vpn-director/configure.sh to continue setup.\n"
}

if [[ "${IMPORT_TEST_MODE:-0}" != "1" ]]; then
    main "$@"
fi
