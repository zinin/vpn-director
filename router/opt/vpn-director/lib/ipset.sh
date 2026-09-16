#!/usr/bin/env bash

###################################################################################################
# ipset.sh - ipset management module for VPN Director
# -------------------------------------------------------------------------------------------------
# Purpose:
#   Modular library for ipset operations: status, ensure, update.
#   Builds country ipsets for Tunnel Director and Xray TPROXY.
#
# Dependencies:
#   - common.sh (log, tmp_file, compute_hash, download_file)
#   - config.sh (IPS_BDR_DIR)
#
# Public API:
#   ipset_status()          - show loaded ipsets, sizes, cache info
#   ipset_ensure()          - ensure ipset exists (load from cache or download)
#   ipset_update()          - force fresh download of ipset
#
# Internal functions (for testing):
#   _next_pow2()            - round up to next power of 2
#   _calc_ipset_size()      - calculate hashsize from entry count (min 1024)
#   _derive_set_name()      - lowercase or hash for long names (>31 chars)
#   parse_exclude_sets_from_json() - extract exclude codes from tunnels JSON (stdin)
#   _ipset_exists()         - check if ipset exists
#   _ipset_count()          - get entry count
#
# Usage:
#   source lib/ipset.sh              # source and run main if any
#   source lib/ipset.sh --source-only # source only for testing
###################################################################################################

# -------------------------------------------------------------------------------------------------
# Disable unneeded shellcheck warnings
# -------------------------------------------------------------------------------------------------
# shellcheck disable=SC2086
# shellcheck disable=SC2155

# -------------------------------------------------------------------------------------------------
# Abort script on any error
# -------------------------------------------------------------------------------------------------
set -euo pipefail

# -------------------------------------------------------------------------------------------------
# Debug mode: set DEBUG=1 to enable tracing
# -------------------------------------------------------------------------------------------------
if [[ ${DEBUG:-0} == 1 ]]; then
    set -x
    PS4='+${BASH_SOURCE[0]##*/}:${LINENO}:${FUNCNAME[0]:-main}: '
fi

###################################################################################################
# State directories and files (from merged shared.sh)
###################################################################################################

# State directories
IPS_BUILDER_DIR="/tmp/ipset_builder"
TUN_DIRECTOR_DIR="/tmp/tunnel_director"

# State files
TUN_DIR_HASH="$TUN_DIRECTOR_DIR/tun_dir_rules.sha256"

# Applied tunnels as "<idx> <id>" lines; tunnel.sh re-ensures their routes on
# every apply and releases their tables on stop.
TUN_DIR_TABLES="$TUN_DIRECTOR_DIR/tun_dir_tables"

# Tunnel id when the failover tunnel's route and ip rule are installed.
# The watch reads this so a plain apply can return 0 while Xray membership stays.
TUN_DIR_FAILOVER_READY="$TUN_DIRECTOR_DIR/failover_ready"

# Ensure directories exist
mkdir -p "$IPS_BUILDER_DIR" "$TUN_DIRECTOR_DIR"

# Export for use by other modules
export IPS_BUILDER_DIR TUN_DIRECTOR_DIR TUN_DIR_HASH TUN_DIR_TABLES TUN_DIR_FAILOVER_READY

###################################################################################################
# Constants (defined before --source-only for testability)
###################################################################################################

# All two-letter ISO country codes, lowercase
ALL_COUNTRY_CODES='
ad ae af ag ai al am ao aq ar as at au aw ax az ba bb bd be bf bg bh bi bj bl bm bn bo bq br bs bt
bv bw by bz ca cc cd cf cg ch ci ck cl cm cn co cr cu cv cw cx cy cz de dj dk dm do dz ec ee eg eh
er es et fi fj fk fm fo fr ga gb gd ge gf gg gh gi gl gm gn gp gq gr gs gt gu gw gy hk hm hn hr ht
hu id ie il im in io iq ir is it je jm jo jp ke kg kh ki km kn kp kr kw ky kz la lb lc li lk lr ls
lt lu lv ly ma mc md me mf mg mh mk ml mm mn mo mp mq mr ms mt mu mv mw mx my mz na nc ne nf ng ni
nl no np nr nu nz om pa pe pf pg ph pk pl pm pn pr ps pt pw py qa re ro rs ru rw sa sb sc sd se sg
sh si sj sk sl sm sn so sr ss st sv sx sy sz tc td tf tg th tj tk tl tm tn to tr tt tv tw tz ua ug
um us uy uz va vc ve vg vi vn vu wf ws ye yt za zm zw
'

# Source URLs for country zone files (priority order)
# 1. GeoLite2 via FireHOL GitHub (most accurate)
GEOLITE2_GITHUB_URL='https://raw.githubusercontent.com/firehol/blocklist-ipsets/master/geolite2_country'
# 2. IPDeny via FireHOL GitHub (mirror, not blocked)
IPDENY_GITHUB_URL='https://raw.githubusercontent.com/firehol/blocklist-ipsets/master/ipdeny_country'
# 3. IPDeny direct (may be blocked in some regions)
IPDENY_DIRECT_URL='https://www.ipdeny.com/ipblocks/data/aggregated'

###################################################################################################
# Pure helper functions (defined before --source-only for testability)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# _ipset_boot_wait - defer execution if system just booted
# -------------------------------------------------------------------------------------------------
# Uses MIN_BOOT_TIME and BOOT_WAIT_DELAY from config.sh
# If uptime < MIN_BOOT_TIME, sleep for BOOT_WAIT_DELAY seconds - at most once
# per boot. On Keenetic every NDM hook of the boot burst runs `--wait apply`
# (120 s), and a 60 s sleep under the lock by each successive holder pushed the
# third and later waiters past their budget: `Timed out waiting for lock` at
# every boot. The network only has to be waited for once, so the first instance
# that sleeps records the boot id in a marker and the ones behind it skip the
# sleep. The lock is still taken before this runs, so the semantics for
# stop/restart are unchanged.
#
# The marker counts only while its content equals the current boot id: a /tmp
# that survived a reboot, or a truncated marker, must never suppress the wait.
# A boot id that cannot be read leaves the id empty, which no marker matches -
# the safe direction is to sleep. The marker is written after the sleep, so a
# sleeper that was killed does not mark the wait as done.
#
# Test seams: VPD_PROC_UPTIME, VPD_BOOT_ID_FILE, VPD_BOOT_WAIT_MARKER.
# -------------------------------------------------------------------------------------------------
_ipset_boot_wait() {
    local uptime_secs min_time wait_delay marker boot_id

    # Get config values (default to sensible values if not set)
    min_time="${MIN_BOOT_TIME:-120}"
    wait_delay="${BOOT_WAIT_DELAY:-30}"

    # Skip if wait disabled
    [[ $wait_delay -eq 0 ]] && return 0

    # Get current uptime in seconds
    uptime_secs=$(awk '{ print int($1) }' "${VPD_PROC_UPTIME:-/proc/uptime}")

    [[ $uptime_secs -lt $min_time ]] || return 0

    marker="${VPD_BOOT_WAIT_MARKER:-$IPS_BUILDER_DIR/boot_wait_done}"
    boot_id="$(cat "${VPD_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}" 2>/dev/null || true)"

    if [[ -n $boot_id && "$(cat "$marker" 2>/dev/null || true)" == "$boot_id" ]]; then
        log "Uptime ${uptime_secs}s < ${min_time}s, but an earlier instance already waited this boot; not sleeping again"
        return 0
    fi

    log "Uptime ${uptime_secs}s < ${min_time}s, sleeping ${wait_delay}s..."
    sleep "$wait_delay"
    mkdir -p "$(dirname "$marker")" 2>/dev/null || true
    printf '%s\n' "$boot_id" > "$marker" 2>/dev/null || true
}

# -------------------------------------------------------------------------------------------------
# _next_pow2 - round up to the next power of two (>= 1)
# -------------------------------------------------------------------------------------------------
_next_pow2() {
    local n=${1:-1} p=1

    [[ $n -lt 1 ]] && n=1

    while [[ $p -lt $n ]]; do
        p=$((p<<1))
    done

    printf '%s\n' "$p"
}

# -------------------------------------------------------------------------------------------------
# _calc_ipset_size - calculate ipset hashsize from element count
# -------------------------------------------------------------------------------------------------
# Uses target load factor of ~0.75:
#   - buckets = ceil(4 * n / 3)
#   - round buckets to next power of 2
#   - floor at 1024 for safety
# -------------------------------------------------------------------------------------------------
_calc_ipset_size() {
    local n=${1:-0} floor=1024 buckets

    buckets=$(((4 * n + 2) / 3))
    [[ $buckets -lt $floor ]] && buckets=$floor

    _next_pow2 "$buckets"
}

# -------------------------------------------------------------------------------------------------
# _derive_set_name - return lowercase name or SHA-256 prefix for long names
# -------------------------------------------------------------------------------------------------
# IPset names are limited to 31 characters. For longer names, we use a
# 24-character SHA-256 prefix as a stable alias.
# -------------------------------------------------------------------------------------------------
_derive_set_name() {
    local set="$1" max=31 set_lc hash

    set_lc=$(printf '%s' "$set" | tr 'A-Z' 'a-z')

    # Fits already? Return as-is
    if [[ "${#set_lc}" -le "$max" ]]; then
        printf '%s\n' "$set_lc"
        return 0
    fi

    hash="$(printf '%s' "$set_lc" | compute_hash | cut -c1-24)"

    log -l TRACE "Assigned alias='$hash' for set='$set_lc'" \
        "because set name exceeds $max chars"

    printf '%s\n' "$hash"
}

# -------------------------------------------------------------------------------------------------
# _is_valid_country_code - check if string is a valid 2-letter ISO country code
# -------------------------------------------------------------------------------------------------
_is_valid_country_code() {
    local code="$1"
    [[ $code =~ ^[a-z]{2}$ ]] && [[ " $ALL_COUNTRY_CODES " == *" $code "* ]]
}

# -------------------------------------------------------------------------------------------------
# _normalize_spec - normalize ipset spec (trim, lowercase, validate)
# -------------------------------------------------------------------------------------------------
# Input: raw spec (single country code only)
# Output: normalized spec or empty string if invalid
# Filters: trims whitespace, lowercases, validates country code
# Note: Combo sets (comma-separated) are no longer supported.
# -------------------------------------------------------------------------------------------------
_normalize_spec() {
    local raw="$1"

    # Remove leading/trailing whitespace and convert to lowercase
    # Note: use 'A-Z' 'a-z' instead of '[:upper:]' '[:lower:]' due to tr bug on some routers
    raw=$(printf '%s' "$raw" | tr 'A-Z' 'a-z' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')

    # Empty after trim?
    if [[ -z $raw ]]; then
        return 1
    fi

    # Reject comma-separated specs (combo support removed)
    if [[ $raw == *,* ]]; then
        log -l ERROR "Combo ipsets not supported: '$raw'"
        return 1
    fi

    # Validate as single country code
    if ! _is_valid_country_code "$raw"; then
        log -l ERROR "Invalid country code '$raw'"
        return 1
    fi

    printf '%s\n' "$raw"
}

# -------------------------------------------------------------------------------------------------
# parse_exclude_sets_from_json - extract exclude country codes from tunnels JSON
# -------------------------------------------------------------------------------------------------
# Reads tunnels JSON from stdin, extracts all exclude arrays, validates country codes.
# Output: one valid country code per line, sorted and deduplicated.
# -------------------------------------------------------------------------------------------------
parse_exclude_sets_from_json() {
    local valid_codes="$ALL_COUNTRY_CODES"

    # Filter: only process objects with array-type exclude fields
    jq -r '
        [.[] | select(type == "object") | .exclude | select(type == "array") | .[]] | unique | .[]
    ' 2>/dev/null | while read -r code; do
        # Validate against known country codes
        code=$(printf '%s' "$code" | tr 'A-Z' 'a-z')
        if [[ " $valid_codes " == *" $code "* ]]; then
            printf '%s\n' "$code"
        fi
    done | sort -u
}

###################################################################################################
# IPSet command wrappers (for testing via mocks)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# _ipset_exists - check if an ipset exists
# -------------------------------------------------------------------------------------------------
_ipset_exists() {
    ipset list -n "$1" >/dev/null 2>&1
}

# -------------------------------------------------------------------------------------------------
# _ipset_count - get number of entries in an ipset
# -------------------------------------------------------------------------------------------------
_ipset_count() {
    local name="$1" cnt
    # "Number of entries:" appears only on kernels whose set revision reports
    # it. KeeneticOS's 4.9 kernel prints a header with just "Size in memory:",
    # so the probe comes back empty - and an empty count reads as zero in
    # "[[ $cnt -eq 0 ]]", which reported every freshly built set as empty.
    # The dump is the portable answer: one "add" line per entry, everywhere.
    cnt=$(ipset list "$name" -t 2>/dev/null | awk '/Number of entries:/ { print $4; exit }' || true)
    if [[ -z $cnt ]]; then
        cnt=$(ipset save "$name" 2>/dev/null | grep -c '^add ' || true)
    fi
    printf '%s\n' "${cnt:-0}"
}

###################################################################################################
# Internal helper functions
###################################################################################################

# -------------------------------------------------------------------------------------------------
# _print_create_ipset - generate ipset create command
# -------------------------------------------------------------------------------------------------
_print_create_ipset() {
    local set_name="$1" cnt="${2:-0}" size
    size="$(_calc_ipset_size "$cnt")"

    printf 'create %s hash:net family inet hashsize %s maxelem %s\n' \
        "$set_name" "$size" "$size"
}

# -------------------------------------------------------------------------------------------------
# _print_add_entry - generate ipset add command
# -------------------------------------------------------------------------------------------------
_print_add_entry() {
    printf 'add %s %s\n' "$1" "$2"
}

# -------------------------------------------------------------------------------------------------
# _print_swap_and_destroy - generate ipset swap and destroy commands
# -------------------------------------------------------------------------------------------------
_print_swap_and_destroy() {
    printf 'swap %s %s\n' "$1" "$2"
    printf 'destroy %s\n' "$1"
}

# -------------------------------------------------------------------------------------------------
# _try_download_zone - download zone file from URL, filter comments, validate CIDR format
# -------------------------------------------------------------------------------------------------
# Args:
#   $1 - URL to download from
#   $2 - temporary file path for download
#   $3 - destination file path
# Returns:
#   0 on success, 1 on failure (download error, invalid format)
# -------------------------------------------------------------------------------------------------
_try_download_zone() {
    local url="$1" tmp_file="$2" dest="$3"

    # Download with 30 sec timeout
    if ! download_file "$url" "$tmp_file" 30; then
        rm -f "$tmp_file"
        return 1
    fi

    # Filter comments and empty lines (|| true prevents exit under set -e if all lines filtered)
    grep -v '^#' "$tmp_file" 2>/dev/null | grep -v '^[[:space:]]*$' > "${tmp_file}.filtered" || true
    mv "${tmp_file}.filtered" "$tmp_file"

    # Validate: file must be non-empty and first line must be CIDR format
    if [[ ! -s "$tmp_file" ]] || ! head -1 "$tmp_file" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+'; then
        log -l DEBUG "Invalid CIDR format from $url"
        rm -f "$tmp_file"
        return 1
    fi

    mv "$tmp_file" "$dest"
    return 0
}

# -------------------------------------------------------------------------------------------------
# _try_manual_fallback - prompt user to manually download zone file (interactive only)
# -------------------------------------------------------------------------------------------------
# Args:
#   $1 - country code
#   $2 - destination file path
# Returns:
#   0 on success, 1 on failure or non-interactive mode
# -------------------------------------------------------------------------------------------------
_try_manual_fallback() {
    local cc="$1" dest="$2"
    local fallback_path="/tmp/${cc}.zone"

    # Only in interactive mode
    [[ ! -t 0 ]] && return 1

    printf '\n'
    printf 'All automatic sources failed for: %s\n' "$cc"
    printf 'Please download manually and place at: %s\n' "$fallback_path"
    printf 'Press Enter when ready (or Ctrl+C to cancel): '
    read -r

    if [[ -f "$fallback_path" ]]; then
        # Filter comments and validate (|| true prevents exit under set -e)
        grep -v '^#' "$fallback_path" 2>/dev/null | grep -v '^[[:space:]]*$' > "$dest" || true
        if [[ -s "$dest" ]] && head -1 "$dest" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+'; then
            log "Using manually provided zone for '$cc'"
            return 0
        fi
        rm -f "$dest"
    fi

    log -l ERROR "Manual fallback failed for '$cc'"
    return 1
}

# -------------------------------------------------------------------------------------------------
# _download_zone_multi_source - download zone file trying multiple sources
# -------------------------------------------------------------------------------------------------
# Tries sources in priority order:
#   1. GeoLite2 via GitHub (most accurate)
#   2. IPDeny via GitHub (mirror)
#   3. IPDeny direct (may be blocked)
#   4. Manual fallback (interactive only)
#
# Args:
#   $1 - country code (2-letter ISO)
#   $2 - destination file path
# Returns:
#   0 on success, 1 if all sources failed
# -------------------------------------------------------------------------------------------------
_download_zone_multi_source() {
    local cc="$1" dest="$2"
    local url tmp_file

    tmp_file=$(tmp_file)

    # Source 1: GeoLite2 via GitHub
    url="${GEOLITE2_GITHUB_URL}/country_${cc}.netset"
    log "Trying geolite2-github for '$cc'..."
    if _try_download_zone "$url" "$tmp_file" "$dest"; then
        log "Downloaded zone for '$cc' from geolite2-github"
        return 0
    fi
    log -l ERROR "geolite2-github failed for '$cc'"

    # Source 2: IPDeny via GitHub
    url="${IPDENY_GITHUB_URL}/id_country_${cc}.netset"
    log "Trying ipdeny-github for '$cc'..."
    if _try_download_zone "$url" "$tmp_file" "$dest"; then
        log "Downloaded zone for '$cc' from ipdeny-github"
        return 0
    fi
    log -l ERROR "ipdeny-github failed for '$cc'"

    # Source 3: IPDeny direct
    url="${IPDENY_DIRECT_URL}/${cc}-aggregated.zone"
    log "Trying ipdeny-direct for '$cc'..."
    if _try_download_zone "$url" "$tmp_file" "$dest"; then
        log "Downloaded zone for '$cc' from ipdeny-direct"
        return 0
    fi
    log -l ERROR "ipdeny-direct failed for '$cc'"

    # Source 4: Manual fallback (interactive only)
    if _try_manual_fallback "$cc" "$dest"; then
        return 0
    fi

    rm -f "$tmp_file"
    return 1
}

# -------------------------------------------------------------------------------------------------
# _restore_from_cache - restore ipset from dump file
# -------------------------------------------------------------------------------------------------
# Returns 0 on success, 1 if restore failed or dump not found.
# Uses atomic swap for existing sets.
# -------------------------------------------------------------------------------------------------
_restore_from_cache() {
    local set_name="$1" dump="$2" force="${3:-0}"
    local cnt rc=0

    # If forcing update or dump missing, signal caller to rebuild
    if [[ $force -eq 1 ]] || [[ ! -f $dump ]]; then
        return 1
    fi

    if _ipset_exists "$set_name"; then
        # Existing set: restore into a temp clone, then swap
        local tmp_set="${set_name}_tmp" restore_script
        restore_script=$(tmp_file)
        ipset destroy "$tmp_set" 2>/dev/null || true

        {
            sed -e "s/^create $set_name /create $tmp_set /" \
                -e "s/^add $set_name /add $tmp_set /" "$dump"
            _print_swap_and_destroy "$tmp_set" "$set_name"
        } > "$restore_script"

        ipset restore -! < "$restore_script" || rc=$?
        rm -f "$restore_script"
    else
        # Set doesn't exist yet - restore directly
        ipset restore -! < "$dump" || rc=$?
    fi

    if [[ $rc -ne 0 ]]; then
        log -l WARN "Restore failed for ipset '$set_name'; will rebuild"
        return 1
    fi

    cnt=$(_ipset_count "$set_name")
    log "Restored ipset '$set_name' from dump ($cnt entries)"

    return 0
}

# -------------------------------------------------------------------------------------------------
# _build_country_ipset - build a country ipset from IPdeny
# -------------------------------------------------------------------------------------------------
_build_country_ipset() {
    local cc="$1" dump_dir="$2"
    local set_name="$cc"
    local dump="${dump_dir}/${set_name}.dump"
    local target_set cidr_list restore_script
    local cnt=0 rc=0

    log "Building country ipset '$set_name'..."

    # Decide whether we need a temporary set (for "hot" updates) or in-place create
    if _ipset_exists "$set_name"; then
        target_set="${set_name}_tmp"
        ipset destroy "$target_set" 2>/dev/null || true
    else
        target_set="$set_name"
    fi

    # Download CIDR list
    cidr_list=$(tmp_file)
    if ! _download_zone_multi_source "$cc" "$cidr_list"; then
        rm -f "$cidr_list"
        return 1
    fi

    cnt=$(wc -l < "$cidr_list")
    log "Total CIDRs for ipset '$set_name': $cnt"

    # Generate an ipset-restore script
    restore_script=$(tmp_file)
    {
        _print_create_ipset "$target_set" "$cnt"

        while IFS= read -r line; do
            [[ -n $line ]] || continue
            _print_add_entry "$target_set" "$line"
        done < "$cidr_list"

        if [[ $target_set != "$set_name" ]]; then
            _print_swap_and_destroy "$target_set" "$set_name"
        fi
    } > "$restore_script"

    # Apply
    log "Applying ipset '$set_name'..."
    ipset restore -! < "$restore_script" || rc=$?
    rm -f "$restore_script" "$cidr_list"

    if [[ $rc -ne 0 ]]; then
        log -l ERROR "Failed to build ipset '$set_name'"
        return 1
    fi

    # Save dump
    ipset save "$set_name" > "$dump"

    cnt=$(_ipset_count "$set_name")
    if [[ $target_set == "$set_name" ]]; then
        log "Created new ipset '$set_name' ($cnt entries)"
    else
        log "Updated existing ipset '$set_name' ($cnt entries)"
    fi

    if [[ $cnt -eq 0 ]]; then
        log -l WARN "ipset '$set_name' is empty"
    fi

    return 0
}


###################################################################################################
# Public API
###################################################################################################

# -------------------------------------------------------------------------------------------------
# ipset_status - show ipset information
# -------------------------------------------------------------------------------------------------
ipset_status() {
    local ipsets

    printf '%s\n' "=== IPSet Status ==="
    printf '\n'

    # List all ipsets
    ipsets=$(ipset list -n 2>/dev/null | sort)

    if [[ -z $ipsets ]]; then
        printf '%s\n' "No ipsets loaded."
        return 0
    fi

    printf '%-24s %10s %s\n' "Name" "Entries" "Type"
    printf '%-24s %10s %s\n' "----" "-------" "----"

    while IFS= read -r name; do
        local info type entries
        # Guard against concurrent set removal: if lookup fails, skip this set.
        # "-t" asks for the header alone. Without it the kernel also prints every
        # member - 148 KiB for a 9657-entry "ru" - and the awk below stops at the
        # header line, leaving that listing half-written in a full pipe. The
        # writer then takes SIGPIPE, pipefail makes it 141, and errexit ends the
        # report where it stands: every set sorting after the big one vanished,
        # and so did the Tunnel Director and Xray sections.
        if ! info=$(ipset list "$name" -t 2>/dev/null); then
            continue
        fi
        type=$(printf '%s\n' "$info" | awk '/^Type:/ { print $2 }')
        entries=$(printf '%s\n' "$info" | awk '/Number of entries:/ { print $4; exit }')
        # Same kernels as in _ipset_count: no count in the header.
        [[ -n $entries ]] || entries=$(_ipset_count "$name")
        printf '%-24s %10s %s\n' "$name" "${entries:-0}" "${type:-unknown}"
    done <<< "$ipsets"

    printf '\n'
    return 0
}

# -------------------------------------------------------------------------------------------------
# ipset_ensure - ensure ipset exists (load from cache or download)
# -------------------------------------------------------------------------------------------------
# Usage:
#   ipset_ensure "ru"           # ensure country ipset 'ru' exists
#
# Input is normalized and validated (trim, lowercase, valid ISO codes).
# Combo sets (comma-separated) are no longer supported.
# -------------------------------------------------------------------------------------------------
ipset_ensure() {
    local raw_spec="$1" spec
    local dump_dir="${IPS_BDR_DIR:-/opt/vpn-director/data}/ipsets"

    # Normalize and validate input
    if ! spec=$(_normalize_spec "$raw_spec"); then
        log -l ERROR "ipset_ensure: invalid spec '$raw_spec'"
        return 1
    fi

    mkdir -p "$dump_dir"

    # Single country set only (combo support removed)
    local set_name="$spec"
    local dump="${dump_dir}/${set_name}.dump"

    # Try restore from cache first (skip if IPSET_FORCE_UPDATE is set)
    if _restore_from_cache "$set_name" "$dump" "${IPSET_FORCE_UPDATE:-0}"; then
        return 0
    fi

    # Need to download and build
    _build_country_ipset "$spec" "$dump_dir"
}

# -------------------------------------------------------------------------------------------------
# ipset_update - force fresh download of ipset
# -------------------------------------------------------------------------------------------------
# Usage:
#   ipset_update "ru"           # force update country ipset 'ru'
#
# Input is normalized and validated (trim, lowercase, valid ISO codes).
# Combo sets (comma-separated) are no longer supported.
# -------------------------------------------------------------------------------------------------
ipset_update() {
    local raw_spec="$1" spec
    local dump_dir="${IPS_BDR_DIR:-/opt/vpn-director/data}/ipsets"

    # Normalize and validate input
    if ! spec=$(_normalize_spec "$raw_spec"); then
        log -l ERROR "ipset_update: invalid spec '$raw_spec'"
        return 1
    fi

    mkdir -p "$dump_dir"

    # Single country set - force rebuild
    _build_country_ipset "$spec" "$dump_dir"
}

# -------------------------------------------------------------------------------------------------
# ipset_cleanup - remove orphaned ipsets (placeholder for future implementation)
# -------------------------------------------------------------------------------------------------
ipset_cleanup() {
    log -l WARN "ipset_cleanup: not yet implemented"
    # TODO: Compare loaded ipsets with rules and remove orphans
    return 0
}

###################################################################################################
# Allow sourcing for testing
###################################################################################################
if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi
