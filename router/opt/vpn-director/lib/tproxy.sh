#!/usr/bin/env bash

###################################################################################################
# tproxy.sh - TPROXY routing module for VPN Director
# -------------------------------------------------------------------------------------------------
# Purpose:
#   Modular library for Xray TPROXY operations: status, apply, stop.
#   Migrated from xray_tproxy.sh to provide independent, testable functions.
#
# Dependencies:
#   - common.sh (log, tmp_file) and, through it, the platform contract
#     (platform_load_module, platform_vpn_endpoints, platform_tproxy_extra_rules,
#      platform_lan_ifaces)
#   - firewall.sh (create_fw_chain, delete_fw_chain, ensure_fw_rule, sync_fw_rule, purge_fw_rules)
#   - config.sh (XRAY_* variables)
#   - ipset.sh (_is_valid_country_code)
#
# Public API:
#   tproxy_status()              - show XRAY_TPROXY chain, routing, xray process
#   tproxy_apply()               - apply TPROXY rules (idempotent), soft-fail if unavailable
#   tproxy_stop()                - remove chain and routing
#   tproxy_restart_process()     - restart Xray process via Entware init script
#   tproxy_get_required_ipsets() - return list of valid exclude ipsets (unknown codes dropped with a WARN)
#
# Internal functions (for testing):
#   _tproxy_check_module()          - check if xt_TPROXY kernel module is available
#   _tproxy_resolve_exclude_set()   - resolve exclusion ipset name (prefer _ext variant)
#   _tproxy_exclude_sets [-q]       - validated, lower-cased XRAY_EXCLUDE_SETS on one line
#   _tproxy_check_required_ipsets() - fail-safe check for required ipsets
#   _tproxy_rule_is_ours()          - does an ip rule on our preference belong to us?
#   _tproxy_setup_routing()         - setup routing table and ip rule
#   _tproxy_table_label()           - the name the kernel prints for a table
#   _tproxy_teardown_routing()      - remove routing table and ip rule
#   _tproxy_setup_clients_ipset()   - setup clients ipset
#   _tproxy_validate_ipv4_cidr()    - validate IPv4 address or CIDR notation
#   _tproxy_setup_bypass_ipset()    - setup bypass ipset (3-source assembly)
#   _tproxy_setup_iptables()        - build iptables rules
#   _tproxy_teardown_iptables()     - remove iptables rules and ipsets
#   _tproxy_init()                  - initialize module state
#
# Usage:
#   source lib/tproxy.sh              # source and run main if any
#   source lib/tproxy.sh --source-only # source only for testing
###################################################################################################

# -------------------------------------------------------------------------------------------------
# Disable unneeded shellcheck warnings
# -------------------------------------------------------------------------------------------------
# shellcheck disable=SC2086

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
# Module state variables (initialized by _tproxy_init)
###################################################################################################

# Initialization flag
_tproxy_initialized=0

# Written only when TPROXY rules were actually installed. Soft-fail skips
# (no module, missing ipsets, iptables setup) remove it so the watch does
# not drop fallback membership on a SOCKS-only success.
XRAY_TPROXY_READY="${XRAY_TPROXY_READY:-/tmp/xray_tproxy/ready}"

###################################################################################################
# Internal helper functions (defined before --source-only for testability)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# _tproxy_init - initialize module state
# -------------------------------------------------------------------------------------------------
# Safe to call multiple times (idempotent).
# -------------------------------------------------------------------------------------------------
_tproxy_init() {
    # Skip if already initialized
    [[ $_tproxy_initialized -eq 1 ]] && return 0

    _tproxy_initialized=1
}

# -------------------------------------------------------------------------------------------------
# _tproxy_check_module - make sure the xt_TPROXY kernel module is loaded
# -------------------------------------------------------------------------------------------------
# Returns 0 if the platform loaded (or had loaded) the module, 1 otherwise.
# -------------------------------------------------------------------------------------------------
_tproxy_check_module() {
    if ! platform_load_module xt_TPROXY; then
        log -l ERROR "xt_TPROXY module not available (KeeneticOS: install the \"Kernel modules for Netfilter\" firmware component)"
        return 1
    fi
    return 0
}

# -------------------------------------------------------------------------------------------------
# _tproxy_resolve_exclude_set - resolve exclusion ipset name (prefer _ext variant)
# -------------------------------------------------------------------------------------------------
# Input: set key (e.g., "ru")
# Output: resolved ipset name (e.g., "ru_ext" or "ru")
# Returns 0 on success, 1 if ipset not found.
# -------------------------------------------------------------------------------------------------
_tproxy_resolve_exclude_set() {
    local set_key="$1"
    local ext_set="${set_key}_ext"

    # Try extended set first
    if ipset list "$ext_set" >/dev/null 2>&1; then
        printf '%s\n' "$ext_set"
        return 0
    fi

    # Fall back to standard set
    if ipset list "$set_key" >/dev/null 2>&1; then
        printf '%s\n' "$set_key"
        return 0
    fi

    return 1
}

# -------------------------------------------------------------------------------------------------
# _tproxy_exclude_sets [-q] - print the configured exclusion country codes on one line
# -------------------------------------------------------------------------------------------------
# Lower-cases each entry of XRAY_EXCLUDE_SETS and drops codes that are not in
# ALL_COUNTRY_CODES, mirroring parse_exclude_sets_from_json for tunnels. Each
# dropped code is logged at WARN unless -q is given, so the three callers of one
# apply produce a single warning. Before this filter a single bad code (e.g. "xx"
# typed in the Web UI) made ipset_ensure fail and cmd_apply abort before any rule
# was written; on boot that also skipped the nightly update cron.
# -------------------------------------------------------------------------------------------------
_tproxy_exclude_sets() {
    local quiet=0 set_key
    local -a exclude_sets_array valid=()

    [[ ${1:-} == "-q" ]] && quiet=1

    [[ -z ${XRAY_EXCLUDE_SETS:-} ]] && return 0
    read -ra exclude_sets_array <<< "$XRAY_EXCLUDE_SETS"

    for set_key in "${exclude_sets_array[@]}"; do
        [[ -n $set_key ]] || continue
        set_key=$(printf '%s' "$set_key" | tr 'A-Z' 'a-z')
        if ! _is_valid_country_code "$set_key"; then
            (( quiet )) || log -l WARN "Ignoring invalid country code '$set_key' in xray.exclude_sets"
            continue
        fi
        valid+=("$set_key")
    done

    (( ${#valid[@]} )) && printf '%s\n' "${valid[*]}"
    return 0
}

# -------------------------------------------------------------------------------------------------
# _tproxy_check_required_ipsets - fail-safe check for required exclusion ipsets
# -------------------------------------------------------------------------------------------------
# Returns 0 if all required ipsets exist, 1 otherwise.
# -------------------------------------------------------------------------------------------------
_tproxy_check_required_ipsets() {
    local set_key
    local -a exclude_sets_array

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets -q)"

    for set_key in "${exclude_sets_array[@]}"; do
        [[ -n $set_key ]] || continue
        if ! _tproxy_resolve_exclude_set "$set_key" >/dev/null; then
            log -l WARN "Required ipset '$set_key' not found; run 'vpn-director.sh apply' first"
            return 1
        fi
    done

    return 0
}

# -------------------------------------------------------------------------------------------------
# _tproxy_rule_is_ours <mark> <table> <want_table> - does an ip rule belong to this module?
# -------------------------------------------------------------------------------------------------
# Ours: the rule carries our mark value under any mask (an earlier
# fwmark_mask), or our table under any mark (an earlier route_table). The
# preference is not ownership: on KeeneticOS 5.1.5 pref 200 was free (a
# user connection policy landed at prefs 102/103, table 4097, fwmark
# 0xffffaab; built-in LTE backup stays at 100/101, 0xffffaaa, table 4096;
# no tables in the 40s). Other firmware versions may still park policies
# at 200, so a rule that is not ours is left alone. <mark> is what
# "ip rule show" printed ("0x100/0x100", "0x100" or "").
# -------------------------------------------------------------------------------------------------
_tproxy_rule_is_ours() {
    local mark="${1%%/*}" table="${2:-}" want_table="${3:-}"
    if [[ $mark =~ ^(0x[0-9a-fA-F]+|[0-9]+)$ ]] && (( mark == XRAY_FWMARK )); then
        return 0
    fi
    [[ -n $table && $table == "$want_table" ]]
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_routing - setup routing table and ip rule for TPROXY
# -------------------------------------------------------------------------------------------------
_tproxy_setup_routing() {
    local rt_exists want_mark want_table kept=0 line mark table

    # Check if route exists in our table
    rt_exists=$(ip route show table "$XRAY_ROUTE_TABLE" 2>/dev/null | grep -c "local default" || true)

    if [[ $rt_exists -eq 0 ]]; then
        ip route add local default dev lo table "$XRAY_ROUTE_TABLE"
        log "Added route: local default dev lo table $XRAY_ROUTE_TABLE"
    fi

    want_mark="$XRAY_FWMARK/$XRAY_FWMARK_MASK"
    want_table=$(_tproxy_table_label "$XRAY_ROUTE_TABLE")

    # Reconcile the rules on our preference: keep one that carries the
    # configured mark and table, drop the other copies of ours - the ones an
    # older version added on every apply, and a rule left over from an earlier
    # route_table or fwmark_mask, which would otherwise count as ours and keep
    # the new setting from ever being installed. Rules that are not ours stay
    # (_tproxy_rule_is_ours). Comparing what the kernel prints - not the table
    # number - is the whole point; see _tproxy_table_label.
    while IFS= read -r line; do
        [[ -n $line ]] || continue
        mark=$(printf '%s' "$line" | sed -n 's/.*fwmark \([^ ]*\).*/\1/p')
        table=$(printf '%s' "$line" | sed -n 's/.*lookup \([^ ]*\).*/\1/p')

        if [[ $kept -eq 0 && $mark == "$want_mark" && $table == "$want_table" ]]; then
            kept=1
            continue
        fi
        _tproxy_rule_is_ours "$mark" "$table" "$want_table" || continue

        if [[ -n $mark ]]; then
            ip rule del pref "$XRAY_RULE_PREF" fwmark "$mark" table "$table" 2>/dev/null || true
        elif [[ -n $table ]]; then
            ip rule del pref "$XRAY_RULE_PREF" table "$table" 2>/dev/null || true
        else
            ip rule del pref "$XRAY_RULE_PREF" 2>/dev/null || true
        fi
        log -l WARN "Removed stale ip rule: pref $XRAY_RULE_PREF fwmark ${mark:-none} lookup ${table:-none}"
    # "ip rule show pref N" is refused by iproute2 4.4 (Entware's ip-full on
    # KeeneticOS answers '"ip rule show" does not take any arguments.'), so the
    # preference is filtered here. Asking the tool would silently yield an empty
    # list, and the rule below would be added a second time - which the kernel
    # refuses with EEXIST, killing the apply under errexit.
    done <<< "$(ip rule show 2>/dev/null | grep "^$XRAY_RULE_PREF:" || true)"

    if [[ $kept -eq 0 ]]; then
        ip rule add pref "$XRAY_RULE_PREF" fwmark "$want_mark" table "$XRAY_ROUTE_TABLE"
        log "Added ip rule: pref $XRAY_RULE_PREF fwmark $want_mark table $XRAY_ROUTE_TABLE"
    fi
}

# -------------------------------------------------------------------------------------------------
# _tproxy_table_label - the name the kernel prints for a routing table
# -------------------------------------------------------------------------------------------------
# iproute2 renders a routing table by its rt_tables name and falls back to the
# number, so table 100 reads back as "lookup wan0" on Asuswrt-Merlin. When
# rt_tables is missing both sides fall back to the number and still agree.
_tproxy_table_label() {
    local name
    name=$(awk -v id="$1" '$0 !~ /^#/ && $1 == id { print $2; exit }' \
        "${RT_TABLES_FILE:-/etc/iproute2/rt_tables}" 2>/dev/null)
    printf '%s' "${name:-$1}"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_teardown_routing - remove routing table and ip rule
# -------------------------------------------------------------------------------------------------
_tproxy_teardown_routing() {
    local want_table line mark table
    want_table=$(_tproxy_table_label "$XRAY_ROUTE_TABLE")

    # Every rule on our preference that is ours (_tproxy_rule_is_ours): the
    # copies an older version left on every apply, and rules from an earlier
    # route_table or fwmark_mask. One delete per listed line - the kernel
    # removes one rule per call, and a duplicate is listed once per copy.
    # Rules of another owner at the same preference stay.
    while IFS= read -r line; do
        [[ -n $line ]] || continue
        mark=$(printf '%s' "$line" | sed -n 's/.*fwmark \([^ ]*\).*/\1/p')
        table=$(printf '%s' "$line" | sed -n 's/.*lookup \([^ ]*\).*/\1/p')
        _tproxy_rule_is_ours "$mark" "$table" "$want_table" || continue
        if [[ -n $mark ]]; then
            ip rule del pref "$XRAY_RULE_PREF" fwmark "$mark" table "$table" 2>/dev/null || true
        else
            ip rule del pref "$XRAY_RULE_PREF" table "$table" 2>/dev/null || true
        fi
    # "ip rule show pref N" is refused by iproute2 4.4 (Entware's ip-full on
    # KeeneticOS answers '"ip rule show" does not take any arguments.'), so the
    # preference is filtered here. Asking the tool would silently yield an empty
    # list, and the rule below would be added a second time - which the kernel
    # refuses with EEXIST, killing the apply under errexit.
    done <<< "$(ip rule show 2>/dev/null | grep "^$XRAY_RULE_PREF:" || true)"

    ip route del local default dev lo table "$XRAY_ROUTE_TABLE" 2>/dev/null || true
    log "Removed TPROXY routing configuration"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_clients_ipset - setup clients ipset
# -------------------------------------------------------------------------------------------------
# Always creates the ipset (even if empty) so iptables rules can reference it.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_clients_ipset() {
    local ip
    local -a clients_array=()

    # Create ipset if not exists
    if ! ipset list "$XRAY_CLIENTS_IPSET" >/dev/null 2>&1; then
        ipset create "$XRAY_CLIENTS_IPSET" hash:net
        log "Created ipset: $XRAY_CLIENTS_IPSET"
    fi

    # Flush and repopulate
    ipset flush "$XRAY_CLIENTS_IPSET"

    # Handle empty XRAY_CLIENTS gracefully
    if [[ -n ${XRAY_CLIENTS:-} ]]; then
        read -ra clients_array <<< "$XRAY_CLIENTS"
        for ip in "${clients_array[@]}"; do
            [[ -n $ip ]] || continue
            ipset add "$XRAY_CLIENTS_IPSET" "$ip" 2>/dev/null || {
                log -l WARN "Failed to add $ip to $XRAY_CLIENTS_IPSET"
            }
        done
    fi

    log "Populated $XRAY_CLIENTS_IPSET ipset (${#clients_array[@]} entries)"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_validate_ipv4_cidr - validate IPv4 address or CIDR notation
# -------------------------------------------------------------------------------------------------
# Returns 0 if valid, 1 otherwise.
# -------------------------------------------------------------------------------------------------
_tproxy_validate_ipv4_cidr() {
    local input="$1"
    local ip mask

    if [[ $input == */* ]]; then
        ip="${input%/*}"
        mask="${input#*/}"
        # Validate mask is 0-32
        [[ $mask =~ ^[0-9]+$ ]] || return 1
        [[ 10#$mask -ge 0 && 10#$mask -le 32 ]] || return 1
    else
        ip="$input"
    fi

    # Validate IPv4: exactly 4 octets, each 0-255
    [[ $ip =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
    local i
    for i in 1 2 3 4; do
        local octet="${BASH_REMATCH[$i]}"
        [[ 10#$octet -le 255 ]] || return 1
    done
    return 0
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_bypass_ipset - setup bypass ipset (3-source assembly)
# -------------------------------------------------------------------------------------------------
# Merges three sources into the bypass ipset:
#   1. Xray server IPs from config (xray.servers)
#   2. User-defined exclude IPs from config (xray.exclude_ips)
#   3. Firmware VPN client endpoints from platform_vpn_endpoints (resolved on the fly)
# Always creates the ipset (even if empty) so iptables rules can reference it.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_bypass_ipset() {
    local ip addr resolved
    local -a servers_array=()
    local -a exclude_ips_array=()
    local xray_count=0 user_count=0 ovpn_count=0

    # Create ipset if not exists
    if ! ipset list "$XRAY_BYPASS_IPSET" >/dev/null 2>&1; then
        ipset create "$XRAY_BYPASS_IPSET" hash:net
        log "Created ipset: $XRAY_BYPASS_IPSET"
    fi

    # Flush and repopulate
    ipset flush "$XRAY_BYPASS_IPSET"

    # Source 1: Xray server IPs from config
    if [[ -n ${XRAY_SERVERS:-} ]]; then
        read -ra servers_array <<< "$XRAY_SERVERS"
        for ip in "${servers_array[@]}"; do
            [[ -n $ip ]] || continue
            ipset add "$XRAY_BYPASS_IPSET" "$ip" 2>/dev/null && xray_count=$((xray_count + 1)) || {
                log -l WARN "Failed to add xray server $ip to $XRAY_BYPASS_IPSET"
            }
        done
    fi

    # Source 2: User-defined exclude IPs from config (validated)
    if [[ -n ${XRAY_EXCLUDE_IPS:-} ]]; then
        read -ra exclude_ips_array <<< "$XRAY_EXCLUDE_IPS"
        for ip in "${exclude_ips_array[@]}"; do
            [[ -n $ip ]] || continue
            # Validate IPv4 or IPv4 CIDR before adding
            if ! _tproxy_validate_ipv4_cidr "$ip"; then
                log -l WARN "Invalid exclude_ips entry '$ip', skipping"
                continue
            fi
            ipset add "$XRAY_BYPASS_IPSET" "$ip" 2>/dev/null && user_count=$((user_count + 1)) || {
                log -l WARN "Failed to add user exclude IP $ip to $XRAY_BYPASS_IPSET"
            }
        done
    fi

    # Source 3: endpoints of the firmware's own VPN clients, so their traffic
    # never enters the proxy (resolved on the fly)
    while IFS= read -r addr; do
        [[ -n $addr ]] || continue

        resolved=$(resolve_ip -a -q "$addr" 2>/dev/null) || {
            log -l WARN "Cannot resolve VPN endpoint $addr"
            continue
        }

        while IFS= read -r ip; do
            [[ -n $ip ]] || continue
            ipset add "$XRAY_BYPASS_IPSET" "$ip" 2>/dev/null && ovpn_count=$((ovpn_count + 1)) || true
        done <<< "$resolved"
    done < <(platform_vpn_endpoints || true)

    local total=$((xray_count + user_count + ovpn_count))
    log "Populated $XRAY_BYPASS_IPSET ipset: $xray_count xray, $user_count user, $ovpn_count openvpn = $total total"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_iptables - build iptables rules
# -------------------------------------------------------------------------------------------------
_tproxy_setup_iptables() {
    local exclude_set resolved_set
    local -a exclude_sets_array

    # The PREROUTING jumps below are what make this chain matter, so ask for the
    # LAN interfaces before touching any firewall state. A platform that cannot
    # name them would otherwise leave a fully populated chain with nothing
    # jumping to it: every packet the proxy exists to carry goes direct, and the
    # function still ends in "Applied TPROXY iptables rules".
    local lan_ifaces
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Cannot determine the LAN interfaces; TPROXY rules not applied"
        return 1
    fi

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets -q)"

    # Create chain
    create_fw_chain -f mangle "$XRAY_CHAIN"

    # Rule 1: Skip if source is not in our clients ipset
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -m set ! --match-set "$XRAY_CLIENTS_IPSET" src -j RETURN

    # Rule 2: Skip traffic to bypass destinations (Xray servers, user excludes, OpenVPN endpoints)
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -m set --match-set "$XRAY_BYPASS_IPSET" dst -j RETURN

    # Rule 3: Skip local destinations (loopback)
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 127.0.0.0/8 -j RETURN

    # Rule 4: Skip private network destinations (RFC1918)
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 10.0.0.0/8 -j RETURN
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 172.16.0.0/12 -j RETURN
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 192.168.0.0/16 -j RETURN

    # Rule 5: Skip link-local
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 169.254.0.0/16 -j RETURN

    # Rule 6: Skip multicast
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 224.0.0.0/4 -j RETURN

    # Rule 7: Skip broadcast
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -d 255.255.255.255/32 -j RETURN

    # Rule 8: Skip excluded country/custom ipsets
    for exclude_set in "${exclude_sets_array[@]}"; do
        [[ -n $exclude_set ]] || continue
        resolved_set="$(_tproxy_resolve_exclude_set "$exclude_set")" || {
            # Should not happen due to _tproxy_check_required_ipsets, but just in case
            log -l ERROR "Exclusion ipset '$exclude_set' not found; aborting"
            return 1
        }
        ensure_fw_rule -q mangle "$XRAY_CHAIN" \
            -m set --match-set "$resolved_set" dst -j RETURN
        log "Added exclusion for ipset: $resolved_set"
    done

    # Rule 9: Apply TPROXY for remaining traffic.
    # This function runs under "if !", which turns errexit off for the whole
    # body; a failed ensure_fw_rule would otherwise fall through to the log
    # and look like success. The watch publishes TPROXY ready from that.
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -p tcp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 1
    ensure_fw_rule -q mangle "$XRAY_CHAIN" \
        -p udp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 1

    # Rules the platform needs outside our chain (Keenetic: mangle INPUT accept).
    # The status is ours to report: this function runs under "if !", which turns
    # errexit off for its whole body, and it ends in a log - so a failure here
    # would otherwise be an apply that says "successfully" while the firmware
    # drops the proxied traffic.
    platform_tproxy_extra_rules apply "$XRAY_FWMARK/$XRAY_FWMARK_MASK" ||
        log -l WARN "Failed to apply platform TPROXY rules; proxied traffic may be dropped"

    # Jump from PREROUTING to our chain for every LAN interface, each at its own
    # position (the first = before Tunnel Director). One shared position would
    # make every interface displace the one before it, so sync_fw_rule would
    # find each jump off its position and purge-and-re-insert all of them on
    # every apply - and each rewrite is a window with no jump for that interface.
    local lan_if pos=1 jump_rc=0
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        if ! sync_fw_rule -q mangle PREROUTING "-i $lan_if -j $XRAY_CHAIN\$" \
            "-i $lan_if -j $XRAY_CHAIN" "$pos"; then
            jump_rc=1
        fi
        pos=$((pos + 1))
    done <<< "$lan_ifaces"
    [[ $jump_rc -eq 0 ]] || return 1

    log "Applied TPROXY iptables rules"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_teardown_iptables - remove all iptables rules
# -------------------------------------------------------------------------------------------------
_tproxy_teardown_iptables() {
    # Best effort: tproxy_stop calls this bare under errexit, so a platform whose
    # cleanup fails must not abort the teardown before the jump and the chain go.
    platform_tproxy_extra_rules stop "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || true
    purge_fw_rules -q "mangle PREROUTING" "-j $XRAY_CHAIN\$"
    delete_fw_chain -q mangle "$XRAY_CHAIN"

    # Remove ipsets
    ipset destroy "$XRAY_CLIENTS_IPSET" 2>/dev/null || true
    ipset destroy "$XRAY_BYPASS_IPSET" 2>/dev/null || true

    log "Removed TPROXY iptables rules and ipsets"
}

###################################################################################################
# Public API (defined before --source-only for testability)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# tproxy_restart_process - restart Xray process via Entware init script
# -------------------------------------------------------------------------------------------------
# Finds and executes the S*xray init script in /opt/etc/init.d/ to restart the Xray process.
# This is needed when xray/config.json is changed and Xray needs to reload its configuration.
# -------------------------------------------------------------------------------------------------
tproxy_restart_process() {
    local init_dir="${XRAY_INIT_DIR:-/opt/etc/init.d}"
    local xray_init
    xray_init=$(find "$init_dir" -maxdepth 1 -name 'S*xray' 2>/dev/null | head -1)

    if [[ -n $xray_init ]] && [[ -x $xray_init ]]; then
        log "Restarting Xray process..."
        # 200>&- keeps the caller's lock out of the daemon this starts. rc.func
        # backgrounds Xray, descriptors are not close-on-exec, and a flock lives
        # on the open file description: an inherited FD 200 would hold
        # /var/lock/vpn-director.lock for as long as Xray runs, so every later
        # acquire_lock would skip silently or time out.
        "$xray_init" restart 200>&-
        log "Xray process restarted"
    else
        log -l WARN "Xray init script not found in $init_dir"
    fi
}

# -------------------------------------------------------------------------------------------------
# tproxy_status - show TPROXY status
# -------------------------------------------------------------------------------------------------
# Displays kernel module, routing, chains, ipsets, and xray process.
# -------------------------------------------------------------------------------------------------
tproxy_status() {
    _tproxy_init

    printf '%s\n' "=== Xray TPROXY Status ==="
    printf '\n'

    printf '%s\n' "--- Kernel Module ---"
    lsmod | grep -E 'xt_TPROXY|nf_tproxy' || printf '%s\n' "TPROXY module not loaded"
    printf '\n'

    printf '%s\n' "--- Routing ---"
    printf 'Table %s:\n' "$XRAY_ROUTE_TABLE"
    ip route show table "$XRAY_ROUTE_TABLE" 2>/dev/null || printf '%s\n' "  (empty)"
    printf '\n'
    printf '%s\n' "IP Rules:"
    ip rule show | grep -E "$XRAY_ROUTE_TABLE|$XRAY_FWMARK" || printf '%s\n' "  (none)"
    printf '\n'

    printf '%s\n' "--- Clients Ipset ---"
    ipset list "$XRAY_CLIENTS_IPSET" 2>/dev/null || printf 'Ipset %s not found\n' "$XRAY_CLIENTS_IPSET"
    printf '\n'

    printf '%s\n' "--- Bypass Ipset ---"
    if ipset list "$XRAY_BYPASS_IPSET" >/dev/null 2>&1; then
        local total
        total=$(ipset list "$XRAY_BYPASS_IPSET" | grep -c '^[0-9]' || true)
        printf 'Ipset %s: %d entries\n' "$XRAY_BYPASS_IPSET" "$total"

        # Show config-based counts
        local -a srv_arr=() excl_arr=()
        [[ -n ${XRAY_SERVERS:-} ]] && read -ra srv_arr <<< "$XRAY_SERVERS"
        [[ -n ${XRAY_EXCLUDE_IPS:-} ]] && read -ra excl_arr <<< "$XRAY_EXCLUDE_IPS"
        printf '  Sources: %d xray servers, %d user exclude_ips, rest = openvpn endpoints\n' \
            "${#srv_arr[@]}" "${#excl_arr[@]}"
    else
        printf 'Ipset %s not found\n' "$XRAY_BYPASS_IPSET"
    fi
    printf '\n'

    printf '%s\n' "--- Iptables Chain ---"
    iptables -t mangle -S "$XRAY_CHAIN" 2>/dev/null || printf 'Chain %s not found\n' "$XRAY_CHAIN"
    printf '\n'

    printf '%s\n' "--- PREROUTING Jump ---"
    iptables -t mangle -S PREROUTING 2>/dev/null | grep "$XRAY_CHAIN" || printf 'No jump to %s\n' "$XRAY_CHAIN"
    printf '\n'

    printf '%s\n' "--- Xray Process ---"
    pgrep -la xray || printf '%s\n' "Xray not running"

    return 0
}

# -------------------------------------------------------------------------------------------------
# tproxy_get_required_ipsets - return list of exclude ipsets
# -------------------------------------------------------------------------------------------------
# Parses XRAY_EXCLUDE_SETS and returns the valid country codes, one per line.
# Unknown codes are dropped by _tproxy_exclude_sets with a WARN.
# -------------------------------------------------------------------------------------------------
tproxy_get_required_ipsets() {
    local set_key
    local -a exclude_sets_array

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets)"

    for set_key in "${exclude_sets_array[@]}"; do
        [[ -n $set_key ]] || continue
        printf '%s\n' "$set_key"
    done
}

# -------------------------------------------------------------------------------------------------
# tproxy_stop - remove chain and routing
# -------------------------------------------------------------------------------------------------
# Removes all TPROXY iptables rules and routing configuration.
# -------------------------------------------------------------------------------------------------
tproxy_stop() {
    _tproxy_init

    log "Stopping Xray TPROXY routing..."
    _tproxy_teardown_iptables
    _tproxy_teardown_routing
    rm -f "$XRAY_TPROXY_READY"
    log "Xray TPROXY routing removed"

    return 0
}

# -------------------------------------------------------------------------------------------------
# tproxy_apply - apply TPROXY rules (idempotent)
# -------------------------------------------------------------------------------------------------
# Applies TPROXY rules. Soft-fails if xt_TPROXY unavailable or ipsets missing.
# Returns 0 even on soft-fail to allow caller scripts to continue.
# -------------------------------------------------------------------------------------------------
tproxy_apply() {
    _tproxy_init

    log "Starting Xray TPROXY routing..."

    # Soft-fail: if xt_TPROXY module not available, return 0 without applying
    if ! _tproxy_check_module; then
        log -l WARN "xt_TPROXY module not available; skipping TPROXY setup"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    # Soft-fail: if required ipsets not ready, return 0 without applying
    if ! _tproxy_check_required_ipsets; then
        log -l WARN "Required ipsets not ready; exiting without applying rules"
        log -l WARN "Run 'vpn-director.sh apply' first to build required ipsets"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    _tproxy_setup_routing
    _tproxy_setup_clients_ipset
    _tproxy_setup_bypass_ipset

    # Soft-fail if iptables setup fails
    if ! _tproxy_setup_iptables; then
        log -l WARN "Failed to setup iptables rules; TPROXY may not be active"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    mkdir -p "$(dirname "$XRAY_TPROXY_READY")"
    printf 'ok\n' > "$XRAY_TPROXY_READY"
    log "Xray TPROXY routing applied successfully"

    return 0
}

###################################################################################################
# Allow sourcing for testing
###################################################################################################
if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi
