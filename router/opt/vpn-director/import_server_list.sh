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
# import_server_list.sh - Import VLESS servers from file/URL
# Supports both plaintext and base64-encoded VLESS URI lists
# Run after install.sh to download and parse server list
###############################################################################

# Source common utilities (use BASH_SOURCE for correct path when sourced)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

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

# Decode VLESS content from base64 or return as-is if plaintext
# Input: raw content as $1
# Output: decoded VLESS URIs to stdout
# Returns: 0 on success, 1 on decode failure
decode_vless_content() {
    local content="$1"
    local first_line

    # Get first non-empty line
    first_line=$(printf '%s\n' "$content" | grep -v '^[[:space:]]*$' | head -n1)

    if [[ "$first_line" == vless://* ]]; then
        log "Detected plaintext format"
        printf '%s' "$content"
    else
        log "Detected base64 format, decoding..."
        # Standard alphabet first; fall back to URL-safe base64 (RFC 4648 §5):
        # map url-safe -_ to standard +/ and pad to a multiple of 4, then decode.
        # The tr sets are written '_-' '/+' (dash LAST in SET1, no leading '-' and
        # no '--') so busybox tr parses them as operands, not options, while the
        # _->/ and -->+ mapping is preserved. The Go importer accepts url-safe
        # blobs too, so this keeps shell/Go parity.
        local decoded b64 pad
        if decoded=$(printf '%s' "$content" | base64 -d 2>/dev/null); then
            printf '%s' "$decoded"
            return 0
        fi
        b64=$(printf '%s' "$content" | tr -d '\r\n\t ' | tr '_-' '/+')
        [[ -n "$b64" ]] || return 1
        case $(( ${#b64} % 4 )) in
            2) pad='==' ;;
            3) pad='=' ;;
            1) return 1 ;;
            *) pad='' ;;
        esac
        printf '%s%s' "$b64" "$pad" | base64 -d 2>/dev/null || return 1
    fi
}

###############################################################################
# VLESS URI Parser
###############################################################################

# URL-decode %XX escapes and "+" (space). Matches Go url.ParseQuery on VALID
# escapes. NOTE: not exact parity on malformed input — Go rejects the whole
# query on a bad %-escape, whereas this leaves a lone/incomplete/non-hex %
# intact (more lenient). Real subscriptions are URL-safe, so the two agree on
# current data; see ticket #41.
# busybox-safe: validate each %XX as hex via sed, rewrite to \xHH, then let
# printf '%b' emit the bytes. Backslashes in the data are escaped first so
# printf cannot misinterpret them. The data is the %b ARGUMENT (never the
# format string), so a literal % is safe.
_url_decode() {
    local s="${1//+/ }"
    s=$(printf '%s' "$s" | sed -e 's/\\/\\\\/g' -e 's/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')
    printf '%b' "$s"
}

# Extract a query parameter value (no external tools), URL-decoded via
# _url_decode (matches Go url.ParseQuery on valid %XX; lenient on malformed %).
# _vless_query_get <query_string> <key> -> prints value (empty if absent)
_vless_query_get() {
    local q="&$1&" v
    case "$q" in
        *"&$2="*)
            v="${q#*"&$2="}"
            _url_decode "${v%%&*}"
            ;;
    esac
}

# Redact the UUID credential from a VLESS URI for safe DEBUG logging. Strips the
# #fragment and replaces the "uuid@" credential with "***@".
_redact_uri() {
    local u="${1%%#*}"
    case "$u" in
        vless://*@*) printf 'vless://***@%s' "${u#vless://*@}" ;;
        *)           printf '%s' "$u" ;;
    esac
}

# Parse a single VLESS URI and extract components
# Format: vless://uuid@server:port?params#name
# Output: pipe-separated fields
#   server|port|uuid|name|security|network|flow|sni|fp|pbk|sid|alpn
parse_vless_uri() {
    local uri="$1"
    local rest raw_name name uuid server_port server port query
    local security network flow sni fp pbk sid alpn

    # Remove vless:// prefix
    rest="${uri#vless://}"

    # Extract name (after #, URL-decoded). Guard against URIs without a
    # #fragment so the query string is not captured as the name.
    if [[ "$rest" == *#* ]]; then
        raw_name="${rest##*#}"
    else
        raw_name=""
    fi
    # Percent-decode the fragment. A subscription encodes the whole name, so
    # decoding only %20 left every other byte as its own hex digits - which the
    # filter below keeps, being digits. That is how "Амстердам" reached
    # servers.json as "D090D0BCD181D182...". Backslashes are escaped first
    # because printf %b expands them; then %XX becomes \xXX for %b to turn back
    # into the byte. A decoded newline would split the record, so drop those.
    raw_name=$(printf '%s' "$raw_name" |
        sed 's/\\/\\\\/g; s/+/ /g; s/%\([0-9a-fA-F][0-9a-fA-F]\)/\\x\1/g')
    raw_name=$(printf '%b' "$raw_name" | tr -d '\n\r')

    # Keep letters, digits, spaces and basic punctuation; drop emoji.
    # Byte-oriented, and pinned to LC_ALL=C on purpose: the routers have no
    # UTF-8 locale - KeeneticOS has no locale at all - so awk works on bytes
    # there. A character-wise filter cuts a two-byte letter in half and keeps
    # whichever half matches an ASCII class, which turned "Амстердам" into
    # "Амс?е?дам" on the device while passing on a workstation. Two-byte
    # sequences are the alphabets (Cyrillic, Greek, accented Latin); the
    # three- and four-byte ones are the symbols and emoji this drops.
    name=$(printf '%s' "$raw_name" | LC_ALL=C gawk '{
        out = ""
        n = length($0)
        i = 1
        while (i <= n) {
            c = substr($0, i, 1)
            if (c ~ /[a-zA-Z0-9 .,;:!?()-]/) { out = out c; i++; continue }
            if (c ~ /[\xC0-\xDF]/) { if (substr($0, i + 1, 1) ~ /[\x80-\xBF]/) { out = out substr($0, i, 2); i += 2 } else { i++ }; continue }
            if (c ~ /[\xE0-\xEF]/) { i += 3; continue }
            if (c ~ /[\xF0-\xF4]/) { i += 4; continue }
            i++
        }
        gsub(/^[ ,]+|[ ,]+$/, "", out)
        print out
    }')
    rest="${rest%%#*}"

    # Extract UUID (before @)
    uuid="${rest%%@*}"
    rest="${rest#*@}"

    # Extract server:port (before ?). Handle bracketed IPv6 literals
    # ([2001:db8::1]:443) by splitting on the ] delimiter, not the colon.
    # NOTE: the stored address drops the brackets (2001:db8::1); the Go parser
    # (vless/parser.go) keeps them ([2001:db8::1]). Both are equivalent to Xray,
    # whose ParseAddress strips brackets for the standalone address field.
    server_port="${rest%%\?*}"
    case "$server_port" in
        \[*)
            server="${server_port#\[}"
            server="${server%%\]*}"
            port="${server_port##*\]:}"
            [[ "$port" == "$server_port" ]] && port=""
            ;;
        *)
            server="${server_port%%:*}"
            port="${server_port##*:}"
            ;;
    esac

    # Extract query string (after ?), then stream params
    if [[ "$rest" == *\?* ]]; then
        query="${rest#*\?}"
    else
        query=""
    fi

    security=$(_vless_query_get "$query" security)
    network=$(_vless_query_get "$query" type)
    flow=$(_vless_query_get "$query" flow)
    sni=$(_vless_query_get "$query" sni)
    fp=$(_vless_query_get "$query" fp)
    pbk=$(_vless_query_get "$query" pbk)
    sid=$(_vless_query_get "$query" sid)
    alpn=$(_vless_query_get "$query" alpn)

    # Fallback: if name is empty after filtering, use server hostname
    if [[ -z "$name" ]]; then
        name="$server"
    fi

    printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "$server" "$port" "$uuid" "$name" \
        "$security" "$network" "$flow" "$sni" "$fp" "$pbk" "$sid" "$alpn"
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
# Step 1: Get VLESS file
###############################################################################

step_get_vless_file() {
    log -l TRACE "Step 1: VLESS Server List"

    printf "Enter path to VLESS file or URL:\n"
    printf "(Supports plaintext or base64-encoded VLESS URIs)\n\n"

    read_input "Path or URL"
    VLESS_INPUT="$INPUT_RESULT"

    if [[ -z "$VLESS_INPUT" ]]; then
        log -l ERROR "No input provided"
        exit 1
    fi

    # Check if it's a URL or file path
    case "$VLESS_INPUT" in
        http://*|https://*)
            log "Downloading from URL..."
            VLESS_CONTENT=$(curl -fsSL --connect-timeout 10 --max-time 60 "$VLESS_INPUT") || {
                log -l ERROR "Failed to download from $VLESS_INPUT"
                exit 1
            }
            ;;
        *)
            if [[ ! -f "$VLESS_INPUT" ]]; then
                log -l ERROR "File not found: $VLESS_INPUT"
                exit 1
            fi
            VLESS_CONTENT=$(cat "$VLESS_INPUT")
            ;;
    esac

    # Decode content (auto-detect format: plaintext or base64)
    VLESS_DECODED=$(decode_vless_content "$VLESS_CONTENT" 2>/dev/null) || {
        log -l ERROR "Failed to decode content (not valid base64 or plaintext VLESS URIs)"
        exit 1
    }

    # Count servers
    SERVER_COUNT=$(printf '%s\n' "$VLESS_DECODED" | grep -c '^vless://' || true)

    if [[ "$SERVER_COUNT" -eq 0 ]]; then
        log -l ERROR "No VLESS servers found in file"
        exit 1
    fi

    log "Found $SERVER_COUNT VLESS servers"
    VLESS_SERVERS="$VLESS_DECODED"
}

###############################################################################
# Step 2: Parse servers
###############################################################################

# The list goes to a temp file, and step 3 publishes it. Resolving takes
# seconds per host, and servers.json written in place sat empty for all of
# them - and was gone when none resolved.
step_parse_servers() {
    log -l TRACE "Step 2: Parsing Servers"

    DATA_DIR=$(get_data_dir)
    SERVERS_FILE="$DATA_DIR/servers.json"
    SERVERS_TMP=$(tmp_file)

    # Parse servers, resolve IPs, emit one JSON object per server, slurp to array
    printf '%s\n' "$VLESS_SERVERS" | grep '^vless://' | while IFS= read -r uri; do
        log -l DEBUG "URI: $(_redact_uri "$uri")"

        parsed=$(parse_vless_uri "$uri")
        server=$(printf '%s' "$parsed" | cut -d'|' -f1)
        port=$(printf '%s' "$parsed" | cut -d'|' -f2)
        uuid=$(printf '%s' "$parsed" | cut -d'|' -f3)
        name=$(printf '%s' "$parsed" | cut -d'|' -f4)
        security=$(printf '%s' "$parsed" | cut -d'|' -f5)
        network=$(printf '%s' "$parsed" | cut -d'|' -f6)
        flow=$(printf '%s' "$parsed" | cut -d'|' -f7)
        sni=$(printf '%s' "$parsed" | cut -d'|' -f8)
        fp=$(printf '%s' "$parsed" | cut -d'|' -f9)
        pbk=$(printf '%s' "$parsed" | cut -d'|' -f10)
        sid=$(printf '%s' "$parsed" | cut -d'|' -f11)
        alpn=$(printf '%s' "$parsed" | cut -d'|' -f12)

        if [[ -z "$server" ]] || [[ -z "$port" ]] || [[ -z "$uuid" ]]; then
            log -l WARN "Skipping invalid URI (missing server/port/uuid)"
            continue
        fi
        # Reject non-numeric and out-of-range ports here so a broken entry never
        # lands in servers.json. 10# forces base-10 so a zero-padded port (e.g.
        # 0443) is not misread as octal. Arithmetic in an if-condition is exempt
        # from set -e, and 10#$port only runs once $port is known all-digit.
        if ! printf '%s' "$port" | grep -qE '^[0-9]+$' || (( 10#$port < 1 || 10#$port > 65535 )); then
            log -l WARN "Skipping $server: invalid port '$port'"
            continue
        fi

        ips_raw=$(resolve_ip -a -q "$server" 2>/dev/null) || ips_raw=""
        if [[ -z "$ips_raw" ]]; then
            log -l WARN "Cannot resolve $server, skipping"
            continue
        fi
        ips_oneline=$(printf '%s' "$ips_raw" | tr '\n' ',' | sed 's/,$//')
        printf "  %s (%s) -> %s\n" "$name" "$server" "$ips_oneline" >&2

        ips_json=$(printf '%s\n' "$ips_raw" | jq -R 'select(length > 0)' | jq -s .)
        if [[ -n "$alpn" ]]; then
            alpn_json=$(printf '%s' "$alpn" | tr ',' '\n' | jq -R 'select(length > 0)' | jq -s .)
        else
            alpn_json='[]'
        fi

        jq -c -n \
            --arg address "$server" --argjson port "$port" --arg uuid "$uuid" --arg name "$name" \
            --argjson ips "$ips_json" \
            --arg security "$security" --arg network "$network" --arg flow "$flow" \
            --arg sni "$sni" --arg fingerprint "$fp" --arg public_key "$pbk" --arg short_id "$sid" \
            --argjson alpn "$alpn_json" \
            '{address:$address, port:$port, uuid:$uuid, name:$name, ips:$ips,
              security:$security, network:$network, flow:$flow, sni:$sni,
              fingerprint:$fingerprint, public_key:$public_key, short_id:$short_id, alpn:$alpn}
             | with_entries(select(.value != null and .value != "" and .value != []))'
    done | jq -s '.' > "$SERVERS_TMP"

    # Validate JSON
    if ! jq empty "$SERVERS_TMP" 2>/dev/null; then
        log -l ERROR "Generated invalid JSON"
        cat "$SERVERS_TMP"
        exit 1
    fi

    SERVER_COUNT=$(jq length "$SERVERS_TMP")

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
    if [[ $VLESS_INPUT == https://* ]]; then
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
        if ! jq --argjson ips "$ips" --arg url "$VLESS_INPUT" ".xray.servers = \$ips | $link" \
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
    log -l TRACE "Import VLESS Server List"
    printf "This will download and parse VLESS servers.\n\n"

    step_get_vless_file
    step_parse_servers
    step_publish_servers

    log -l TRACE "Import Complete"
    printf "Server list saved. Run /opt/vpn-director/configure.sh to continue setup.\n"
}

if [[ "${IMPORT_TEST_MODE:-0}" != "1" ]]; then
    main "$@"
fi
