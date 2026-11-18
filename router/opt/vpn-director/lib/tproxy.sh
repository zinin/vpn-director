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
#      platform_lan_ifaces, platform_tunnel_offload_target)
#   - firewall.sh (delete_fw_chain, ensure_fw_rule, purge_fw_rules, swap_fw_chain, fw_chain_exists)
#   - config.sh (XRAY_* variables)
#   - guard.sh (guard_layout_check, guard_direct_mark_hex, guard_direct_ensure: the direct mark
#               the chain sets on what it lets through, and the direct rule that routes it;
#               guard_sync, guard_clear, guard_list, guard_status, guard_direct_release: the
#               guards, which refuse an Xray client's packets while the firmware has deleted the
#               chain)
#   - ipset.sh (_is_valid_country_code, _ipset_exists)
#
# Public API:
#   tproxy_status()              - show XRAY_TPROXY chain, routing, the guards, xray process
#   tproxy_apply()               - the make half of an apply: routing, sets, the direct rule, chain
#                                  swapped in, the guards; clients added, never removed; soft-fail
#                                  if unavailable; rc 1, nothing touched, for a refused layout
#   tproxy_prune [<addr>...]     - the break half: XRAY_CLIENTS becomes the effective clients, plus
#                                  every <addr> it holds (the clients Tunnel Director does not
#                                  carry), and the guards follow it
#   tproxy_stop()                - remove chain, routing and the guards; the direct rule once
#                                  neither module needs it
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
#   _tproxy_add_to_live_clients()   - the same for the soft-fails, only when the clients ipset exists
#   _tproxy_shadow_set()            - an empty <set>_NEW to fill and swap in
#   _tproxy_swap_set()              - put <set>_NEW in place of <set> in one step
#   _tproxy_setup_bypass_ipset()    - build the bypass ipset beside the live one (3-source assembly)
#   _tproxy_build_chain()           - fill a fresh chain with the TPROXY rules (swap_fw_chain's build_fn)
#   _tproxy_jump_pos()              - where the first XRAY_TPROXY jump goes: position 1
#   _tproxy_setup_iptables()        - platform rules, then the chain swapped in with its jumps
#   _tproxy_guards_wanted()         - does the live chain mark what it lets through direct?
#   _tproxy_members()               - the members of the clients ipset, sorted
#   _tproxy_guard_lines()           - the guards of what the clients ipset holds, in order
#   _tproxy_guards_sync()           - make the guards those of the clients ipset (guard_sync)
#   _tproxy_guards_prune()          - drop the guards of the clients the ipset let go, add none
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
# _tproxy_add_to_live_clients - add every effective client to XRAY_CLIENTS, if there is one
# -------------------------------------------------------------------------------------------------
# For the soft-fails of tproxy_apply, which return before the rules. The rules of an earlier apply
# still intercept what the live set holds, and a client that joined Xray has to be in it before
# tunnel_apply takes its tunnel mark away, as on every other apply: added only by the prune, it went
# out through the WAN in between. Without a set there is no rule to intercept anything, and none is
# made.
# -------------------------------------------------------------------------------------------------
_tproxy_add_to_live_clients() {
    _ipset_exists "$XRAY_CLIENTS_IPSET" || return 0
    _tproxy_setup_clients_ipset || true
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
# The rules ahead of the targets are of two kinds. Rule 1 and the private ranges of rule 6 bound
# what the targets take - without rule 1 every LAN client, without rule 6 traffic to the router
# and the rest of the LAN - and the direct mark of rule 2 is what the direct rule sends to main:
# without it, what the chain lets through meets the guards instead, and Xray's refuse it. A
# failure there, or of a target itself, returns 2: the chain must not go in, and the chain
# already in place keeps working. The others only decide what a client reaches directly instead
# of through the proxy, or, the offload opt-out of rule 3, whether the platform may accelerate
# it: a failure there is logged and returns 1, and the chain goes in without the rule, the ready
# marker withheld. Keeping the chain out for those as well left every Xray client on the WAN over
# one busy xtables lock.
# -------------------------------------------------------------------------------------------------
_tproxy_build_chain() {
    local chain="$1" exclude_set resolved_set direct_mark offload_target narrow_rc=0 offload_ok=1
    local -a exclude_sets_array

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets -q)"

    # guard_layout_check, in tproxy_apply, has refused a direct mark that is none. A platform
    # whose firmware accelerates forwarded flows past mangle names the target that opts a flow out
    # of it (KeeneticOS: PPE); one that needs none prints nothing and returns 1.
    direct_mark="$(guard_direct_mark_hex)" || return 2
    offload_target="$(platform_tunnel_offload_target)" || offload_target=""

    # Rule 1: Skip if source is not in our clients ipset
    ensure_fw_rule -q mangle "$chain" \
        -m set ! --match-set "$XRAY_CLIENTS_IPSET" src -j RETURN || return 2

    # Rule 2: The direct mark, on every packet of an Xray client. What the RETURNs below let go
    # leaves the chain with it, and so does a protocol TPROXY does not take: the direct rule
    # (lib/guard.sh) sends it to main ahead of the guards. A packet TPROXY takes gets Xray's mark
    # beside it (--tproxy-mark leaves every bit outside fwmark_mask alone) and meets Xray's rule
    # first. TUN_DIR tests its own field alone, so what Xray lets through of a client that is on a
    # tunnel too is still marked for the tunnel, whose lookup rule comes before the direct rule.
    # _tproxy_guards_wanted asks for this rule in this very form, no match ahead of the target:
    # the two go together.
    ensure_fw_rule -q mangle "$chain" \
        -j MARK --set-xmark "$direct_mark/$direct_mark" || return 2

    # Rule 3: The platform's offload opt-out, for what Xray lets through. The fast path routes a
    # flow it took over by the ip rules alone, never by a mark, so an accelerated flow would meet
    # the guards after its first packets. A flow TPROXY takes ends in a local socket, and no fast
    # path takes it.
    if [[ -n $offload_target ]] && ! ensure_fw_rule -q mangle "$chain" -j "$offload_target"; then
        log -l WARN "The TPROXY offload opt-out did not go in; what Xray lets through can be accelerated past its direct mark until the next apply"
        offload_ok=0
    fi

    # Rule 4: Skip traffic to bypass destinations (Xray servers, user excludes, OpenVPN endpoints)
    ensure_fw_rule -q mangle "$chain" \
        -m set --match-set "$XRAY_BYPASS_IPSET" dst -j RETURN || narrow_rc=1

    # Rule 5: Skip local destinations (loopback)
    ensure_fw_rule -q mangle "$chain" \
        -d 127.0.0.0/8 -j RETURN || narrow_rc=1

    # Rule 6: Skip private network destinations (RFC1918)
    ensure_fw_rule -q mangle "$chain" \
        -d 10.0.0.0/8 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 172.16.0.0/12 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 192.168.0.0/16 -j RETURN || return 2

    # Rule 7: Skip link-local
    ensure_fw_rule -q mangle "$chain" \
        -d 169.254.0.0/16 -j RETURN || narrow_rc=1

    # Rule 8: Skip multicast
    ensure_fw_rule -q mangle "$chain" \
        -d 224.0.0.0/4 -j RETURN || narrow_rc=1

    # Rule 9: Skip broadcast
    ensure_fw_rule -q mangle "$chain" \
        -d 255.255.255.255/32 -j RETURN || narrow_rc=1

    # Rule 10: Skip excluded country/custom ipsets
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

    # Rule 11: Apply TPROXY for remaining traffic.
    ensure_fw_rule -q mangle "$chain" \
        -p tcp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2
    ensure_fw_rule -q mangle "$chain" \
        -p udp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2

    [[ $offload_ok -eq 1 ]] || return 1
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
# _tproxy_guards_wanted - does the live XRAY_TPROXY mark what it lets through direct?
# -------------------------------------------------------------------------------------------------
# Asks iptables for rule 2 as _tproxy_build_chain writes it - "-j MARK --set-xmark <D>/<D>", no
# match, D from guard_direct_mark_hex: the two go together. Without that rule what goes direct
# leaves unmarked and would meet the clients' guards, which refuse it, so they are not synced and
# stay as they are: a chain an earlier version put in returns bare from every exclusion, and with
# no chain at all - xt_TPROXY missing, the firmware's rebuild - every packet goes unmarked. Returns
# 1 then.
# -------------------------------------------------------------------------------------------------
_tproxy_guards_wanted() {
    local mark
    mark="$(guard_direct_mark_hex)" || return 1
    iptables -t mangle -C "$XRAY_CHAIN" -j MARK --set-xmark "$mark/$mark" 2>/dev/null
}

# -------------------------------------------------------------------------------------------------
# _tproxy_members - the members of XRAY_CLIENTS_IPSET, one per line, sorted
# -------------------------------------------------------------------------------------------------
# The first word of each line after "Members:" of "ipset list", "/32" dropped, as "ip rule show"
# prints a host in a guard's from. Sorted: the kernel lists them in hash order, which a swap of
# the set changes, and the same guards in another order would be written anew. Returns 1 with an
# ERROR, printing nothing, when the set cannot be listed.
# -------------------------------------------------------------------------------------------------
_tproxy_members() {
    local listing members
    if ! listing="$(ipset list "$XRAY_CLIENTS_IPSET" 2>/dev/null)" ||
        ! members="$(awk 'm && NF { sub(/\/32$/, "", $1); print $1 } /^Members:/ { m = 1 }' \
            <<< "$listing" | LC_ALL=C sort)"; then
        log -l ERROR "Xray: cannot list $XRAY_CLIENTS_IPSET; the guards stay as they are"
        return 1
    fi
    [[ -z $members ]] || printf '%s\n' "$members"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_guard_lines - Xray's guards, one ip rule's selectors per line, in the order they are to
# match (guard_sync)
# -------------------------------------------------------------------------------------------------
# The firmware's firewall rebuilds take XRAY_TPROXY away - NDM deletes it, Merlin's firewall start
# empties it - and until the apply puts it back an Xray client's packets are neither proxied nor
# marked: they would reach main and leave through the WAN. ip rules survive the rebuilds. Xray
# cannot be reached without the chain, so a client's guards refuse such a packet, but for what the
# chain lets go direct. Per member X of XRAY_CLIENTS_IPSET and LAN interface L:
#
#   from <X> to <R> iif L lookup main    each private range R: 10.0.0.0/8, 172.16.0.0/12,
#                                        192.168.0.0/16, 169.254.0.0/16, 224.0.0.0/4
#   from <X> to <E> iif L lookup main    each address or CIDR E of xray.exclude_ips
#   from <X> iif L prohibit              everything else
#
# A packet the chain marked - for Xray, or direct - meets Xray's rule or the direct rule first, so
# in normal operation none reaches a guard. The members are what the set holds (_tproxy_members):
# in tproxy_apply the clients it added and those an earlier apply left, after tproxy_prune the
# effective clients and those it kept. They come sorted, and which comes first changes no route:
# each gets the same guards.
#
# "iif" keeps the router's own traffic out - a client's CIDR may hold the router's LAN address -
# and the local table, at preference 0, answers traffic to the router before any guard. An entry
# of xray.exclude_ips gets its line only where the bypass set can hold it as written: an address
# or a CIDR that ip and ipset read as such (is_ipv4_net), and no /0, which no hash:net set takes.
# _tproxy_validate_ipv4_cidr lets a leading zero through, which ipset refuses and ip reads as
# octal - 010 is 8: the guard would send another address direct, or be refused on every apply. A
# country of xray.exclude_sets is no ip rule - no rule matches a set - and neither are the Xray
# servers and the firmware's VPN endpoints the bypass set holds: while the chain is gone, their
# traffic is refused as well. Returns 1 with an ERROR, printing nothing, when the platform names no
# LAN interface or the set cannot be listed: the empty list would take every guard away.
# -------------------------------------------------------------------------------------------------
_tproxy_guard_lines() {
    local lan_ifaces members member lan_if range excl
    local -a ranges=(10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 224.0.0.0/4)
    local -a excludes=()
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Xray: cannot determine the LAN interfaces; the guards stay as they are"
        return 1
    fi
    members="$(_tproxy_members)" || return 1
    [[ -z ${XRAY_EXCLUDE_IPS:-} ]] || read -ra excludes <<< "$XRAY_EXCLUDE_IPS"

    while IFS= read -r member; do
        [[ -n $member ]] || continue
        while IFS= read -r lan_if; do
            [[ -n $lan_if ]] || continue
            for range in "${ranges[@]}"; do
                printf 'from %s to %s iif %s lookup main\n' "$member" "$range" "$lan_if"
            done
            for excl in ${excludes[@]+"${excludes[@]}"}; do
                is_ipv4_net "$excl" || continue
                [[ $excl != */0 ]] || continue
                printf 'from %s to %s iif %s lookup main\n' "$member" "${excl%/32}" "$lan_if"
            done
            printf 'from %s iif %s prohibit\n' "$member" "$lan_if"
        done <<< "$lan_ifaces"
    done <<< "$members"
    return 0
}

# -------------------------------------------------------------------------------------------------
# _tproxy_guards_sync - make Xray's guards those of what XRAY_CLIENTS_IPSET holds
# -------------------------------------------------------------------------------------------------
# The new list goes in before the old one goes (guard_sync), and only while the live chain marks
# what it lets through direct (_tproxy_guards_wanted): otherwise the guards stay as they are, and
# it returns 2. Returns 1 when a guard did not go in - guard_sync logs an ERROR for each and keeps
# the old guards of the clients the list still names - or when _tproxy_guard_lines cannot say what
# they are (it logs the ERROR), the guards left as they are.
# -------------------------------------------------------------------------------------------------
_tproxy_guards_sync() {
    local lines
    _tproxy_guards_wanted || return 2
    lines="$(_tproxy_guard_lines)" || return 1
    guard_sync xray "$lines"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_guards_prune - drop the guards of the clients XRAY_CLIENTS_IPSET no longer holds, and
# add none
# -------------------------------------------------------------------------------------------------
# For tproxy_prune while the live chain does not answer for its direct mark (_tproxy_guards_sync
# returned 2). No guard may go in then, but a client the swap let go is returned by rule 1
# unmarked, and in normal operation its refusal would meet it where it goes direct. guard_sync gets
# the guards in place (guard_list, the preference stripped, in their order) of every member the
# set still holds, its from compared with the members as _tproxy_guard_lines reads them
# (_tproxy_members): it writes no line that is not in place already, and takes the rest. Nothing
# changes when no guard is to go, nor when guard_list prints nothing: no guard, or a listing that
# failed, which it does not tell apart, and the empty list would take every guard away. Returns 1
# when the members cannot be read (the ERROR is logged) or guard_sync returns 1.
# -------------------------------------------------------------------------------------------------
_tproxy_guards_prune() {
    local members listed line from kept="" dropped=0
    local -a words=()
    members="$(_tproxy_members)" || return 1
    listed="$(guard_list xray)"
    [[ -n $listed ]] || return 0
    while IFS= read -r line; do
        read -ra words <<< "${line#*:}"
        [[ ${#words[@]} -gt 0 ]] || continue
        from=""
        [[ ${words[0]} != from ]] || from="${words[1]:-}"
        if [[ -n $from && $'\n'$members$'\n' == *$'\n'"${from%/32}"$'\n'* ]]; then
            kept+="${words[*]}"$'\n'
        else
            dropped=1
        fi
    done <<< "$listed"
    [[ $dropped -eq 1 ]] || return 0
    guard_sync xray "$kept"
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
# Displays kernel module, routing, the direct rule and the guards, ipsets, chains, and xray process.
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
    # Xray's own: its mark under any mask, or its table (_tproxy_rule_is_ours). The table number
    # alone matched any rule holding its digits, the guards of 192.168.1.100 among them.
    ip rule show | grep -E "fwmark $XRAY_FWMARK/|lookup $(rt_table_label "$XRAY_ROUTE_TABLE")( |\$)" ||
        printf '%s\n' "  (none)"
    printf '\n'

    # The guards, which refuse an Xray client's packets while the firmware has deleted the chain,
    # and the direct rule ahead of them, which sends what the chains mark direct to main: listed,
    # or counted per preference in the full status (GUARD_STATUS_BRIEF=1, guard_status).
    printf '%s\n' "--- Guards (ip rules by source) ---"
    guard_status xray
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
    # A swap that stopped half-way leaves XRAY_TPROXY_NEW behind; one that stopped at the rename
    # leaves it carrying the traffic until the next apply, with XRAY_TPROXY gone.
    if fw_chain_exists mangle "${XRAY_CHAIN}_NEW"; then
        printf '%s\n' "--- Chain: ${XRAY_CHAIN}_NEW (left by an interrupted swap; the next apply finishes or removes it) ---"
        iptables -t mangle -S "${XRAY_CHAIN}_NEW" 2>/dev/null | tail -n +2
    fi
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
# tproxy_stop - remove chain, routing and the guards
# -------------------------------------------------------------------------------------------------
# Removes all TPROXY iptables rules and routing configuration, then Xray's guards, and the direct
# rule once neither module needs it: the module that stops last removes it.
# -------------------------------------------------------------------------------------------------
tproxy_stop() {
    _tproxy_init

    log "Stopping Xray TPROXY routing..."
    _tproxy_teardown_iptables
    _tproxy_teardown_routing
    # The guards, and with them the direct rule once neither module needs it:
    # guard_direct_release leaves it while TUN_DIR is in mangle or Tunnel
    # Director's guards are in place, and runs once the chain is gone, which
    # could still mark a packet direct. Those a former pref_base left go too, at
    # the preferences guard.sh recorded; nothing from main's rule at 32766 on.
    guard_clear xray
    guard_direct_release
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
#
# The chain sets the direct mark on every packet of an Xray client, and the direct rule that
# routes it (guard_direct_ensure) goes in before the chain; one that does not withholds the ready
# marker. Once the chain is in, every client the set holds gets its guards, which refuse its
# packets while the firmware has deleted the chain (_tproxy_guards_sync); a guard that does not go
# in withholds the marker too, and so does a check that finds no direct mark in the chain just
# swapped in. The soft-fails sync them as well, for the clients they add to a live set. A direct
# mark or guard preferences that guard_layout_check refuses return 1 before anything is touched,
# and the run ends there, as it would in tunnel_apply.
# -------------------------------------------------------------------------------------------------
tproxy_apply() {
    _tproxy_init

    log "Starting Xray TPROXY routing..."

    # The direct mark is a bit of its own: in Xray's fwmark_mask it would read as
    # Xray's mark, in Tunnel Director's field as a tunnel's. The direct rule and
    # the guards stay ahead of main's rule as well. guard_layout_check logs the
    # ERROR, and nothing is touched: not even the soft-fails below run, which
    # add to the live client set.
    guard_layout_check || return 1

    # Soft-fail: if xt_TPROXY module not available, return 0 without applying.
    # A live XRAY_CLIENTS still gets every client, as on every apply (below), and
    # the clients get their guards while the live chain marks direct: it still
    # intercepts with the set, and a client moving from a tunnel to Xray loses
    # its tunnel guard in tunnel_apply.
    if ! _tproxy_check_module; then
        log -l WARN "xt_TPROXY module not available; skipping TPROXY setup"
        rm -f "$XRAY_TPROXY_READY"
        _tproxy_add_to_live_clients
        _tproxy_guards_sync || true
        return 0
    fi

    # Soft-fail: if required ipsets not ready, return 0 without applying; the live
    # set and the guards as above.
    if ! _tproxy_check_required_ipsets; then
        log -l WARN "Required ipsets not ready; exiting without applying rules"
        log -l WARN "Run 'vpn-director.sh apply' first to build required ipsets"
        rm -f "$XRAY_TPROXY_READY"
        _tproxy_add_to_live_clients
        _tproxy_guards_sync || true
        return 0
    fi

    _tproxy_setup_routing
    local clients_ok=1
    if ! _tproxy_setup_clients_ipset; then
        clients_ok=0
    fi
    # A bypass set that could not be swapped keeps its entries and has said so.
    _tproxy_setup_bypass_ipset || true

    # The direct rule before the chain: from the swap on, every packet of an Xray
    # client carries the direct mark, and the rule sends what the chain lets
    # through to main ahead of the guards. One in place is left alone. One that
    # does not go in (guard_direct_ensure logs the ERROR) withholds the marker
    # below; the chain and the guards go in all the same, the side that leaks
    # nothing: until an apply puts the rule in, what the chain lets through
    # meets the guards and is refused, but for the private ranges and
    # xray.exclude_ips, which they send direct themselves.
    local direct_ok=1
    if ! guard_direct_ensure; then
        direct_ok=0
    fi

    local iptables_ok=1
    if ! _tproxy_setup_iptables; then
        iptables_ok=0
    fi

    # The guards, once the chain is in: from the swap on it marks what it lets
    # through direct, which the guards would refuse otherwise. Where the new chain
    # stayed out, the one in place decides (_tproxy_guards_wanted). They follow
    # what XRAY_CLIENTS holds, which only grows here: a client that left Xray keeps
    # its guards until the prune lets it go. One that does not go in (guard_sync
    # logs the ERROR) withholds the marker below, and so does a check that finds
    # no direct mark in the chain (rc 2).
    local guards_rc=0
    _tproxy_guards_sync || guards_rc=$?

    # Soft-fail if iptables setup fails
    if [[ $iptables_ok -eq 0 ]]; then
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

    # Without the direct rule, what the chain marks direct meets the guards
    # instead of main.
    if [[ $direct_ok -eq 0 ]]; then
        log -l WARN "The direct rule is not in place; TPROXY readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    # The watch takes Xray clients off the fallback tunnel on the marker, and one
    # without its guards would leave through the WAN in the next rebuild of the
    # firewall. The chain was just built with the direct-mark rule, so a check
    # that does not find it is an error of the moment - a busy xtables lock - and
    # no guard was synced; the next apply checks again.
    if [[ $guards_rc -eq 2 ]]; then
        log -l WARN "The direct mark was not found in $XRAY_CHAIN, or iptables could not check it (a busy xtables lock); the Xray guards were not synced, TPROXY readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi
    if [[ $guards_rc -ne 0 ]]; then
        log -l WARN "Not every Xray client has its guards; TPROXY readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi

    mkdir -p "$(dirname "$XRAY_TPROXY_READY")"
    printf 'ok\n' > "$XRAY_TPROXY_READY"
    log "Xray TPROXY routing applied successfully"

    return 0
}

# -------------------------------------------------------------------------------------------------
# tproxy_prune [<addr>...] - let go of the clients that left Xray, except the ones nothing carries
# -------------------------------------------------------------------------------------------------
# The break half of an apply (vpn-director.sh): tproxy_apply only adds to XRAY_CLIENTS, and a client
# that left Xray stays intercepted until this runs. The set built as XRAY_CLIENTS_NEW and swapped in
# holds the effective clients - xray.clients less paused_clients, each an IPv4 address or CIDR - and
# every <addr> the live set holds. The caller names there the clients the live TUN_DIR may not
# carry: a full apply the ones tunnel_apply reported (TUNNEL_UNCARRIED), "apply xray" and "restart
# xray" every client the config puts on a tunnel (tunnel_clients; main is none), since they run no
# Tunnel Director. A client on its way from Xray to a tunnel stays proxied that way until an apply
# in which Tunnel Director carries it; one that left Xray for direct - paused, deleted, moved to
# main - is let go. An <addr> the live set does not hold was not proxied, and is not made so. One
# the new set already matches gets no element of its own: the kernel's test finds a host through
# any network that holds it, so a host inside an effective network is in, and so is x/32 of an
# effective x. One WARN names the addresses kept.
#
# A client the new set does not take would lose its interception with the swap, so the prune stops
# there instead: the clients that left stay proxied until the next apply, which is no leak. Once
# the set is swapped in, the guards follow it (_tproxy_guards_sync): a client it let go loses its
# guards then, and not before. While the chain does not answer for its direct mark, no guard goes
# in, and only those of the clients the set let go are taken away (_tproxy_guards_prune). Always
# returns 0, and leaves the ready marker alone: every effective client is in the set either way,
# and keeps its guards when a new one is refused.
# -------------------------------------------------------------------------------------------------
tproxy_prune() {
    _tproxy_init

    local ip shadow="${XRAY_CLIENTS_IPSET}_NEW" count=0 kept=""
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
    for ip in "$@"; do
        [[ -n $ip ]] || continue
        # Matched by the new set already, as the kernel tests it - an effective client, x/32 of one,
        # a host inside an effective network, an address named before. An element of its own would
        # add nothing but a WARN, on every run.
        ipset test "$shadow" "$ip" >/dev/null 2>&1 && continue
        # Not proxied now: the prune does not make it so.
        ipset test "$XRAY_CLIENTS_IPSET" "$ip" >/dev/null 2>&1 || continue
        if ! ipset add -exist "$shadow" "$ip" 2>/dev/null; then
            log -l WARN "Cannot add $ip to $shadow; clients that left Xray stay proxied until the next apply"
            ipset destroy "$shadow" 2>/dev/null || true
            return 0
        fi
        kept+="$ip "
    done
    if ! _tproxy_swap_set "$XRAY_CLIENTS_IPSET"; then
        log -l WARN "Cannot swap $shadow into $XRAY_CLIENTS_IPSET; clients that left Xray stay proxied until the next apply"
        return 0
    fi
    if [[ -n $kept ]]; then
        log -l WARN "Kept in $XRAY_CLIENTS_IPSET: ${kept% } - they stay proxied until an apply in which Tunnel Director carries them"
    fi
    log "Pruned $XRAY_CLIENTS_IPSET to the $count effective Xray clients${kept:+ and the ones kept}"
    # The guards follow the set, after the swap: a client it let go was proxied
    # until then, and its guards kept it off the WAN in a rebuild of the firewall.
    # A guard that does not go in (guard_sync logs the ERROR) leaves the old ones
    # of the clients the set still holds. While the chain does not answer for its
    # direct mark (rc 2), no guard may go in, but those of a client the set let
    # go are taken away all the same: rule 1 returns its packets unmarked now,
    # and its refusal would meet them in normal operation.
    local guards_rc=0
    _tproxy_guards_sync || guards_rc=$?
    if [[ $guards_rc -eq 2 ]]; then
        log -l WARN "The direct mark was not found in $XRAY_CHAIN, or iptables could not check it (a busy xtables lock); the Xray guards were not synced, only those of the clients that left the set removed"
        _tproxy_guards_prune || true
    fi
    return 0
}

###################################################################################################
# Allow sourcing for testing
###################################################################################################
if [[ ${1:-} == "--source-only" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi
