#!/usr/bin/env bash

###################################################################################################
# tproxy.sh - TPROXY routing module for VPN Director
# -------------------------------------------------------------------------------------------------
# Purpose:
#   Modular library for Xray TPROXY operations: status, apply, stop.
#   Migrated from xray_tproxy.sh to provide independent, testable functions.
#
# Dependencies:
#   - common.sh (log, tmp_file, is_ipv4_net, rt_table_label) and, through it, the platform contract
#     (platform_load_module, platform_vpn_endpoints, platform_tproxy_extra_rules,
#      platform_lan_ifaces)
#   - firewall.sh (delete_fw_chain, ensure_fw_rule, purge_fw_rules, swap_fw_chain)
#   - config.sh (XRAY_* variables)
#   - ipset.sh (_is_valid_country_code, _ipset_exists)
#
# Public API:
#   tproxy_status()              - show XRAY_TPROXY chain, routing, xray process
#   tproxy_apply()               - the make half of an apply: routing, sets, chain swapped in;
#                                  clients added, never removed; soft-fail if unavailable
#   tproxy_prune()               - the break half: XRAY_CLIENTS becomes exactly xray.clients
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
#   _tproxy_teardown_routing()      - remove routing table and ip rule
#   _tproxy_setup_clients_ipset()   - create the clients ipset and add every client (never removes)
#   _tproxy_shadow_set()            - an empty <set>_NEW to fill and swap in
#   _tproxy_swap_set()              - put <set>_NEW in place of <set> in one step
#   _tproxy_setup_bypass_ipset()    - build the bypass ipset beside the live one (3-source assembly)
#   _tproxy_build_chain()           - fill a fresh chain with the TPROXY rules (swap_fw_chain's build_fn)
#   _tproxy_jump_pos()              - where the first XRAY_TPROXY jump goes: position 1
#   _tproxy_setup_iptables()        - platform rules, then the chain swapped in with its jumps
#   _tproxy_validate_ipv4_cidr()    - validate IPv4 address or CIDR notation
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

# Written only when TPROXY rules were actually installed, the platform's own
# rules outside the chain included. Soft-fail skips (no module, missing
# ipsets, iptables setup) remove it so the watch does not drop fallback
# membership on a SOCKS-only success.
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
    want_table=$(rt_table_label "$XRAY_ROUTE_TABLE")

    # Reconcile the rules on our preference: keep one that carries the
    # configured mark and table, drop the other copies of ours - the ones an
    # older version added on every apply, and a rule left over from an earlier
    # route_table or fwmark_mask, which would otherwise count as ours and keep
    # the new setting from ever being installed. Rules that are not ours stay
    # (_tproxy_rule_is_ours). Comparing what the kernel prints - not the table
    # number - is the whole point; see rt_table_label in common.sh.
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
# _tproxy_teardown_routing - remove routing table and ip rule
# -------------------------------------------------------------------------------------------------
_tproxy_teardown_routing() {
    local want_table line mark table
    want_table=$(rt_table_label "$XRAY_ROUTE_TABLE")

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
# _tproxy_setup_clients_ipset - create the clients ipset and add every effective client to it
# -------------------------------------------------------------------------------------------------
# Always creates the ipset (even if empty) so iptables rules can reference it.
#
# Added to, never flushed: a client on its way from Xray to a tunnel stays in the set - and
# intercepted - until tproxy_prune, which a full apply runs once Tunnel Director has taken it. A
# flush emptied the set until it was filled again, and every Xray client went out through the
# WAN in between.
#
# Returns 1 when any effective client is missing from the set. The chain RETURNs
# every source it does not list, so a missing client is not proxied at all, and
# the watch drops its fallback tunnel on the ready marker tproxy_apply writes.
# An entry that is no IPv4 address or CIDR is skipped with a WARN and does not
# count: an IPv6 address an older Web UI saved, a typo like 192.168.1.1000. No
# kernel set takes it and no tunnel carries it, and waiting for it withheld the
# marker for the whole LAN - every restore held on the fallback tunnel for good.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_clients_ipset() {
    local ip
    local rc=0
    local -a clients_array=()

    # Create ipset if not exists
    if ! ipset list "$XRAY_CLIENTS_IPSET" >/dev/null 2>&1; then
        if ipset create "$XRAY_CLIENTS_IPSET" hash:net; then
            log "Created ipset: $XRAY_CLIENTS_IPSET"
        else
            log -l ERROR "Failed to create $XRAY_CLIENTS_IPSET"
            return 1
        fi
    fi

    # Handle empty XRAY_CLIENTS gracefully
    if [[ -n ${XRAY_CLIENTS:-} ]]; then
        read -ra clients_array <<< "$XRAY_CLIENTS"
        for ip in "${clients_array[@]}"; do
            [[ -n $ip ]] || continue
            if ! is_ipv4_net "$ip"; then
                log -l WARN "Xray client '$ip' is not an IPv4 address or CIDR; skipping"
                continue
            fi
            # -exist: the client is usually in the set already, and xray.clients
            # is never validated - a repeated address is the one failure that
            # means nothing.
            ipset add -exist "$XRAY_CLIENTS_IPSET" "$ip" 2>/dev/null || {
                log -l WARN "Failed to add $ip to $XRAY_CLIENTS_IPSET"
                rc=1
            }
        done
    fi

    log "Added the Xray clients to $XRAY_CLIENTS_IPSET (${#clients_array[@]} entries)"
    return $rc
}

# -------------------------------------------------------------------------------------------------
# _tproxy_shadow_set <name> - an empty <name>_NEW to fill and then swap in for <name>
# -------------------------------------------------------------------------------------------------
# ipset takes names of up to 31 characters, so <name> may have 27 at most. No rule names
# <name>_NEW, so emptying one an interrupted apply left opens no window.
# -------------------------------------------------------------------------------------------------
_tproxy_shadow_set() {
    local shadow="${1}_NEW"
    if (( ${#shadow} > 31 )); then
        log -l ERROR "ipset name '$1' leaves no room for the _NEW suffix (27 characters at most)"
        return 1
    fi
    if _ipset_exists "$shadow"; then
        ipset flush "$shadow" 2>/dev/null
    else
        ipset create "$shadow" hash:net 2>/dev/null
    fi
}

# -------------------------------------------------------------------------------------------------
# _tproxy_swap_set <name> - put <name>_NEW in place of <name> in one step, then drop the old entries
# -------------------------------------------------------------------------------------------------
# "ipset swap" exchanges the two sets under the rules that name them atomically: a packet meets
# the old entries or the new ones, never an empty set.
# -------------------------------------------------------------------------------------------------
_tproxy_swap_set() {
    local name="$1" shadow="${1}_NEW"
    if ! _ipset_exists "$name"; then
        ipset create "$name" hash:net 2>/dev/null || return 1
    fi
    ipset swap "$shadow" "$name" 2>/dev/null || return 1
    ipset destroy "$shadow" 2>/dev/null || true
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
# The entries go into TPROXY_BYPASS_NEW, which is then swapped in whole: a flush emptied the live
# set until it was filled again, and a subscription server, an excluded address or a firmware VPN
# endpoint was proxied in between. Returns 1, the live set left as it was, when the new set
# cannot be made or swapped in.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_bypass_ipset() {
    local ip addr resolved
    local -a servers_array=()
    local -a exclude_ips_array=()
    local xray_count=0 user_count=0 ovpn_count=0
    local shadow="${XRAY_BYPASS_IPSET}_NEW"

    if ! _tproxy_shadow_set "$XRAY_BYPASS_IPSET"; then
        log -l ERROR "Cannot prepare $shadow; $XRAY_BYPASS_IPSET keeps the entries it has"
        return 1
    fi

    # Source 1: Xray server IPs from config
    if [[ -n ${XRAY_SERVERS:-} ]]; then
        read -ra servers_array <<< "$XRAY_SERVERS"
        for ip in "${servers_array[@]}"; do
            [[ -n $ip ]] || continue
            ipset add "$shadow" "$ip" 2>/dev/null && xray_count=$((xray_count + 1)) || {
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
            ipset add "$shadow" "$ip" 2>/dev/null && user_count=$((user_count + 1)) || {
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
            ipset add "$shadow" "$ip" 2>/dev/null && ovpn_count=$((ovpn_count + 1)) || true
        done <<< "$resolved"
    done < <(platform_vpn_endpoints || true)

    if ! _tproxy_swap_set "$XRAY_BYPASS_IPSET"; then
        log -l ERROR "Cannot swap $shadow into $XRAY_BYPASS_IPSET; it keeps the entries it has"
        return 1
    fi

    local total=$((xray_count + user_count + ovpn_count))
    log "Populated $XRAY_BYPASS_IPSET ipset: $xray_count xray, $user_count user, $ovpn_count openvpn = $total total"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_jump_pos - where the first XRAY_TPROXY jump goes: position 1, ahead of Tunnel Director
# -------------------------------------------------------------------------------------------------
_tproxy_jump_pos() {
    printf '1\n'
}

# -------------------------------------------------------------------------------------------------
# _tproxy_build_chain <chain> - fill a fresh chain with the TPROXY rules (swap_fw_chain's build_fn)
# -------------------------------------------------------------------------------------------------
# The rules ahead of the targets are of two kinds. Rule 1 and the private ranges of rule 4 bound
# what the targets take - without rule 1 every LAN client, without rule 4 traffic to the router
# and the rest of the LAN - so a failure there, or of a target itself, returns 2: the chain must
# not go in, and the chain already in place keeps working. The others only decide what a client
# reaches directly instead of through the proxy: a failure there is logged and returns 1, and the
# chain goes in without the rule, the ready marker withheld. Keeping the chain out for those as
# well left every Xray client on the WAN over one busy xtables lock.
# -------------------------------------------------------------------------------------------------
_tproxy_build_chain() {
    local chain="$1" exclude_set resolved_set narrow_rc=0
    local -a exclude_sets_array

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets -q)"

    # Rule 1: Skip if source is not in our clients ipset
    ensure_fw_rule -q mangle "$chain" \
        -m set ! --match-set "$XRAY_CLIENTS_IPSET" src -j RETURN || return 2

    # Rule 2: Skip traffic to bypass destinations (Xray servers, user excludes, OpenVPN endpoints)
    ensure_fw_rule -q mangle "$chain" \
        -m set --match-set "$XRAY_BYPASS_IPSET" dst -j RETURN || narrow_rc=1

    # Rule 3: Skip local destinations (loopback)
    ensure_fw_rule -q mangle "$chain" \
        -d 127.0.0.0/8 -j RETURN || narrow_rc=1

    # Rule 4: Skip private network destinations (RFC1918)
    ensure_fw_rule -q mangle "$chain" \
        -d 10.0.0.0/8 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 172.16.0.0/12 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 192.168.0.0/16 -j RETURN || return 2

    # Rule 5: Skip link-local
    ensure_fw_rule -q mangle "$chain" \
        -d 169.254.0.0/16 -j RETURN || narrow_rc=1

    # Rule 6: Skip multicast
    ensure_fw_rule -q mangle "$chain" \
        -d 224.0.0.0/4 -j RETURN || narrow_rc=1

    # Rule 7: Skip broadcast
    ensure_fw_rule -q mangle "$chain" \
        -d 255.255.255.255/32 -j RETURN || narrow_rc=1

    # Rule 8: Skip excluded country/custom ipsets
    for exclude_set in "${exclude_sets_array[@]}"; do
        [[ -n $exclude_set ]] || continue
        if ! resolved_set="$(_tproxy_resolve_exclude_set "$exclude_set")"; then
            # _tproxy_check_required_ipsets has just seen it; the set went away since.
            log -l ERROR "Exclusion ipset '$exclude_set' not found; its destinations are proxied"
            narrow_rc=1
            continue
        fi
        if ensure_fw_rule -q mangle "$chain" \
            -m set --match-set "$resolved_set" dst -j RETURN; then
            log "Added exclusion for ipset: $resolved_set"
        else
            narrow_rc=1
        fi
    done
    if [[ $narrow_rc -ne 0 ]]; then
        log -l WARN "A TPROXY exclusion did not go in; its destinations are proxied until the next apply"
    fi

    # Rule 9: Apply TPROXY for remaining traffic.
    ensure_fw_rule -q mangle "$chain" \
        -p tcp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2
    ensure_fw_rule -q mangle "$chain" \
        -p udp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2

    return "$narrow_rc"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_iptables - the platform's rules, then XRAY_TPROXY swapped in with its jumps
# -------------------------------------------------------------------------------------------------
# Returns 1 whenever the ready marker has to wait: the chain did not go in (the one in place
# stays), a narrowing RETURN or a jump is missing, or the platform's own rules are.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_iptables() {
    local lan_ifaces lan_if
    local -a jump_matches=()

    # The PREROUTING jumps are what make this chain matter, so ask for the
    # LAN interfaces before touching any firewall state. A platform that cannot
    # name them would otherwise leave a fully populated chain with nothing
    # jumping to it: every packet the proxy exists to carry goes direct, and the
    # function still ends in "Applied TPROXY iptables rules".
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Cannot determine the LAN interfaces; TPROXY rules not applied"
        return 1
    fi
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        jump_matches+=("-i $lan_if")
    done <<< "$lan_ifaces"

    # Rules the platform needs outside our chain (Keenetic: mangle INPUT accept),
    # in place before the new chain takes the jumps. The status is ours to
    # report: this function runs under "if !", which turns errexit off for its
    # whole body. A failure here fails the function only after the swap, which
    # still happens: a router without the subscription watch keeps its
    # interception, and what changes is the ready marker tproxy_apply writes -
    # the watch waits for it before Xray clients leave the fallback tunnel.
    local extra_rc=0
    if ! platform_tproxy_extra_rules apply "$XRAY_FWMARK/$XRAY_FWMARK_MASK"; then
        log -l WARN "Failed to apply platform TPROXY rules; proxied traffic may be dropped"
        extra_rc=1
    fi

    # The chain is built beside the live one and swapped in: flushing the live
    # chain and filling it one rule per call left every Xray client unproxied -
    # out through the WAN - for the length of the refill, on every apply. Each
    # LAN interface gets its own jump, from position 1: ahead of Tunnel
    # Director, and none displacing another.
    local swap_rc=0
    swap_fw_chain mangle "$XRAY_CHAIN" _tproxy_build_chain _tproxy_jump_pos "${jump_matches[@]}" || swap_rc=$?
    if [[ $swap_rc -eq 2 ]]; then
        log -l ERROR "TPROXY rules not rebuilt; the rules already in place stay"
    fi
    [[ $swap_rc -eq 0 && $extra_rc -eq 0 ]] || return 1

    log "Applied TPROXY iptables rules"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_teardown_iptables - remove all iptables rules
# -------------------------------------------------------------------------------------------------
_tproxy_teardown_iptables() {
    # Best effort: tproxy_stop calls this bare under errexit, so a platform whose
    # cleanup fails must not abort the teardown before the jump and the chain go.
    platform_tproxy_extra_rules stop "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || true
    # The jumps to a shadow an interrupted swap left go too.
    purge_fw_rules -q "mangle PREROUTING" "-j ${XRAY_CHAIN}(_NEW)?\$"
    delete_fw_chain -q mangle "$XRAY_CHAIN"
    delete_fw_chain -q mangle "${XRAY_CHAIN}_NEW" || true

    # Remove ipsets, and the shadows an interrupted apply left
    ipset destroy "$XRAY_CLIENTS_IPSET" 2>/dev/null || true
    ipset destroy "$XRAY_BYPASS_IPSET" 2>/dev/null || true
    ipset destroy "${XRAY_CLIENTS_IPSET}_NEW" 2>/dev/null || true
    ipset destroy "${XRAY_BYPASS_IPSET}_NEW" 2>/dev/null || true

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
# tproxy_apply - the make half of an apply: TPROXY routing, sets and chain (idempotent)
# -------------------------------------------------------------------------------------------------
# Adds every effective client to XRAY_CLIENTS and removes none: tproxy_prune does that after
# Tunnel Director has taken the clients that left Xray (vpn-director.sh cmd_apply). Soft-fails if
# xt_TPROXY is unavailable or ipsets are missing, returning 0 so caller scripts can continue.
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
    local clients_ok=1
    if ! _tproxy_setup_clients_ipset; then
        clients_ok=0
    fi
    # A bypass set that could not be swapped keeps its entries and has said so.
    _tproxy_setup_bypass_ipset || true

    # Soft-fail if iptables setup fails
    if ! _tproxy_setup_iptables; then
        log -l WARN "TPROXY rules are not complete; readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    # The watch reads the marker as "every Xray client is intercepted" and drops
    # their fallback-tunnel membership on it. Rules that RETURN a client the
    # ipset never took send it to the WAN instead, so an incomplete client set
    # withholds the marker even though the chain itself is in place.
    if [[ $clients_ok -eq 0 ]]; then
        log -l WARN "Not every Xray client is in $XRAY_CLIENTS_IPSET; TPROXY readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    mkdir -p "$(dirname "$XRAY_TPROXY_READY")"
    printf 'ok\n' > "$XRAY_TPROXY_READY"
    log "Xray TPROXY routing applied successfully"

    return 0
}

# -------------------------------------------------------------------------------------------------
# tproxy_prune - let go of the clients xray.clients no longer names
# -------------------------------------------------------------------------------------------------
# The break half of a full apply (vpn-director.sh): tproxy_apply only adds to XRAY_CLIENTS, and a
# client that left Xray stays intercepted until Tunnel Director has taken it. The exact set is
# built as XRAY_CLIENTS_NEW and swapped in. A client the new set does not take would lose its
# interception with the swap, so the prune stops there instead: the clients that left stay
# proxied until the next apply, which is no leak. Always returns 0.
# -------------------------------------------------------------------------------------------------
tproxy_prune() {
    _tproxy_init

    local ip shadow="${XRAY_CLIENTS_IPSET}_NEW" count=0
    local -a clients_array=()

    # tproxy_apply soft-failed before it made the set: there is nothing to prune.
    _ipset_exists "$XRAY_CLIENTS_IPSET" || return 0

    if ! _tproxy_shadow_set "$XRAY_CLIENTS_IPSET"; then
        log -l WARN "Cannot prepare $shadow; clients that left Xray stay proxied until the next apply"
        return 0
    fi
    if [[ -n ${XRAY_CLIENTS:-} ]]; then
        read -ra clients_array <<< "$XRAY_CLIENTS"
        for ip in "${clients_array[@]}"; do
            [[ -n $ip ]] || continue
            # Skipped by tproxy_apply too, with a WARN: no set takes it.
            is_ipv4_net "$ip" || continue
            if ! ipset add -exist "$shadow" "$ip" 2>/dev/null; then
                log -l WARN "Cannot add $ip to $shadow; clients that left Xray stay proxied until the next apply"
                ipset destroy "$shadow" 2>/dev/null || true
                return 0
            fi
            count=$((count + 1))
        done
    fi
    if ! _tproxy_swap_set "$XRAY_CLIENTS_IPSET"; then
        log -l WARN "Cannot swap $shadow into $XRAY_CLIENTS_IPSET; clients that left Xray stay proxied until the next apply"
        return 0
    fi
    log "Pruned $XRAY_CLIENTS_IPSET to the $count clients xray.clients names"
    return 0
}

###################################################################################################
# Allow sourcing for testing
###################################################################################################
if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi
