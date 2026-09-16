#!/usr/bin/env bash

###################################################################################################
# tunnel.sh - tunnel director module for VPN Director
# -------------------------------------------------------------------------------------------------
# Purpose:
#   Modular library for tunnel director operations: status, apply, stop.
#   Routes LAN client traffic through VPN tunnels with exclusion-based routing.
#
# Dependencies:
#   - common.sh (log, tmp_file, compute_hash, is_lan_ip) and, through it, the
#     platform contract (platform_tunnels, platform_tunnel_table,
#     platform_tunnel_route_ensure, platform_tunnel_table_release,
#     platform_prerouting_base_pos, platform_lan_ifaces)
#   - firewall.sh (create_fw_chain, delete_fw_chain, ensure_fw_rule, sync_fw_rule,
#                  purge_fw_rules, fw_chain_exists)
#   - config.sh (TUN_DIR_TUNNELS_JSON, TUN_DIR_CHAIN, TUN_DIR_PREF_BASE,
#                TUN_DIR_MARK_MASK, TUN_DIR_MARK_SHIFT; XRAY_CHAIN, to keep the
#                PREROUTING jump behind the Xray jumps)
#   - ipset.sh (_ipset_exists, parse_exclude_sets_from_json, TUN_DIR_HASH, TUN_DIR_TABLES)
#
# Public API:
#   tunnel_status()              - show TUN_DIR chain, ip rules, configured tunnels
#   tunnel_apply()               - apply rules from config (idempotent)
#   tunnel_stop()                - remove chain, ip rules and tunnel tables
#   tunnel_get_required_ipsets() - return list of ipsets needed for rules
#
# Internal functions (for testing):
#   _tunnel_table_allowed()      - check if a tunnel id is one the platform lists
#   _tunnel_gateway()            - the configured gateway of a tunnel, or nothing
#   _tunnel_ensure_routes()      - re-install the routes of every applied tunnel
#   _tunnel_init()               - initialize module state
#
# Usage:
#   source lib/tunnel.sh              # source and run main if any
#   source lib/tunnel.sh --source-only # source only for testing
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
# Module state variables (initialized by _tunnel_init)
###################################################################################################

# Valid routing tables (space-separated)
_tunnel_valid_tables=""

# Fwmark helpers (computed from config)
_tunnel_mark_mask_val=""
_tunnel_mark_shift_val=""
_tunnel_mark_field_max=""
_tunnel_mark_mask_hex=""

# Initialization flag
_tunnel_initialized=0

###################################################################################################
# Internal helper functions (defined before --source-only for testability)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# _tunnel_init - initialize module state
# -------------------------------------------------------------------------------------------------
# Builds the valid_tables list from platform_tunnels and computes fwmark helpers.
# Safe to call multiple times (idempotent).
# -------------------------------------------------------------------------------------------------
_tunnel_init() {
    # Skip if already initialized
    [[ $_tunnel_initialized -eq 1 ]] && return 0

    # Valid tunnel ids come from the platform (Merlin: wgcN and ovpncN tables
    # from rt_tables; Keenetic: OpenVPN*/Wireguard* interfaces), "main" last.
    local tables_list
    tables_list="$(platform_tunnels || true)"

    # Single-line, space-separated (for matching)
    _tunnel_valid_tables="$(printf '%s\n' "$tables_list" | xargs)"

    # Precompute numeric and hex helpers from config
    _tunnel_mark_mask_val=$((TUN_DIR_MARK_MASK))
    _tunnel_mark_shift_val=$((TUN_DIR_MARK_SHIFT))
    _tunnel_mark_field_max=$((_tunnel_mark_mask_val >> _tunnel_mark_shift_val))
    _tunnel_mark_mask_hex="$(printf '0x%x' "$_tunnel_mark_mask_val")"

    _tunnel_initialized=1
}

# -------------------------------------------------------------------------------------------------
# _tunnel_table_allowed - check the tunnel id is one the platform lists
# -------------------------------------------------------------------------------------------------
# Returns 0 if the id is in _tunnel_valid_tables (built from platform_tunnels), 1 otherwise.
# -------------------------------------------------------------------------------------------------
_tunnel_table_allowed() {
    local table="${1:-}"

    # Empty table is not allowed
    [[ -z $table ]] && return 1

    # Ensure initialized
    [[ $_tunnel_initialized -eq 0 ]] && _tunnel_init

    # Check if table is in valid list
    [[ " $_tunnel_valid_tables " == *" $table "* ]]
}

# -------------------------------------------------------------------------------------------------
# _tunnel_gateway <id> - the configured gateway of a tunnel, or nothing
# -------------------------------------------------------------------------------------------------
# tunnel_director.tunnels.<id>.gateway (spec 12): the Keenetic route of an
# OpenVPN tunnel uses it, every other platform ignores it. Anything but a
# dotted IPv4 address is dropped with a WARN so a typo never reaches ip route.
# -------------------------------------------------------------------------------------------------
_tunnel_gateway() {
    local raw gateway o
    raw="$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "${1:-}" '.[$t].gateway // empty' 2>/dev/null)"
    [[ -n $raw ]] || return 0
    gateway="$raw"
    if [[ $gateway =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]]; then
        for o in "${BASH_REMATCH[@]:1}"; do
            if [[ $((10#$o)) -gt 255 ]]; then
                gateway=""
                break
            fi
        done
    else
        gateway=""
    fi
    if [[ -z $gateway ]]; then
        log -l WARN "Tunnel '$1': invalid gateway '$raw' ignored (expected an IPv4 address)"
        return 0
    fi
    printf '%s\n' "$gateway"
}

# -------------------------------------------------------------------------------------------------
# _tunnel_ensure_routes - re-install the routes of every applied tunnel
# -------------------------------------------------------------------------------------------------
# Reads TUN_DIR_TABLES ("<idx> <id>" per line, written by tunnel_apply) and calls
# platform_tunnel_route_ensure for each. A no-op on Merlin, where the firmware
# keeps the tunnel tables; on Keenetic the route follows the interface state.
# -------------------------------------------------------------------------------------------------
_tunnel_ensure_routes() {
    local idx tunnel rc=0
    [[ -f $TUN_DIR_TABLES ]] || return 0
    while read -r idx tunnel; do
        [[ -n $tunnel ]] || continue
        if ! platform_tunnel_route_ensure "$tunnel" "$idx" "$(_tunnel_gateway "$tunnel")"; then
            log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
            if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" ]]; then
                rc=1
            fi
        fi
    done < "$TUN_DIR_TABLES"
    return "$rc"
}

# True when the failover tunnel has an ip rule at pref TUN_DIR_PREF_BASE+idx.
# "ip rule show pref N" is refused by iproute2 4.4 (Entware's ip-full), so this
# greps the unfiltered listing the way tproxy.sh does.
_tunnel_failover_rule_present() {
    local idx pref
    [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]] || return 0
    [[ -f $TUN_DIR_TABLES ]] || return 1
    idx=$(awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { print $1; exit }' "$TUN_DIR_TABLES")
    [[ -n $idx ]] || return 1
    pref=$((TUN_DIR_PREF_BASE + idx))
    ip rule show 2>/dev/null | grep -q "^${pref}:"
}

# True when the failover tunnel still has clients after pause filtering.
_tunnel_failover_needed() {
    [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]] || return 1
    local c
    c=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].clients // [] | .[]')
    [[ -n $c ]]
}

# One client's RETURN / offload / MARK in TUN_DIR. Offload sits immediately
# before MARK with the same match so excluded destinations keep acceleration.
# warnings and changes are tunnel_apply's locals (bash dynamic scope).
_tunnel_emit_client() {
    local client="$1" tunnel="$2" mark_hex="$3" excludes="$4"
    local client_ip="${client%%/*}"
    if ! is_lan_ip "$client_ip"; then
        log -l WARN "Client '$client' is not RFC1918; skipping"
        warnings=1
        return 0
    fi

    local excl excl_set
    while IFS= read -r excl; do
        [[ -n $excl ]] || continue
        excl_set=$(printf '%s' "$excl" | tr 'A-Z' 'a-z')
        if ! _ipset_exists "$excl_set"; then
            log -l WARN "Exclude ipset '$excl_set' not found; skipping exclusion"
            warnings=1
            continue
        fi
        ensure_fw_rule -q mangle "$TUN_DIR_CHAIN" \
            -s "$client" -m set --match-set "$excl_set" dst -j RETURN
    done <<< "$excludes"

    if [[ -n $offload_target ]]; then
        ensure_fw_rule -q mangle "$TUN_DIR_CHAIN" \
            -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
            -j "$offload_target"
    fi

    ensure_fw_rule -q mangle "$TUN_DIR_CHAIN" \
        -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
        -j MARK --set-xmark "$mark_hex/$_tunnel_mark_mask_hex"

    log "Added: client=$client tunnel=$tunnel mark=$mark_hex"
    changes=1
}

###################################################################################################
# Public API (defined before --source-only for testability)
###################################################################################################

# -------------------------------------------------------------------------------------------------
# tunnel_status - show tunnel director status
# -------------------------------------------------------------------------------------------------
# Displays TUN_DIR chain rules, ip rules, and configured tunnels.
# -------------------------------------------------------------------------------------------------
tunnel_status() {
    _tunnel_init

    printf '%s\n' "=== Tunnel Director Status ==="
    printf '\n'

    # Show chain rules
    printf '%s\n' "--- Chain: $TUN_DIR_CHAIN ---"
    if fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        iptables -t mangle -S "$TUN_DIR_CHAIN" 2>/dev/null | tail -n +2
    else
        printf '%s\n' "Chain not found (not applied)"
    fi
    printf '\n'

    # Show IP rules
    printf '%s\n' "--- IP Rules (fwmark-based) ---"
    local ip_rules
    ip_rules=$(ip rule show 2>/dev/null | grep -E "fwmark.*lookup" || true)

    if [[ -z $ip_rules ]]; then
        printf '%s\n' "No fwmark-based ip rules found."
    else
        printf '%s\n' "$ip_rules"
    fi
    printf '\n'

    # Show configured tunnels
    printf '%s\n' "--- Configured Tunnels ---"
    if [[ -z $TUN_DIR_TUNNELS_JSON ]] || [[ $TUN_DIR_TUNNELS_JSON == "{}" ]]; then
        printf '%s\n' "No tunnels configured."
    else
        printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '
            to_entries[] |
            "Tunnel: \(.key)\n  Clients: \(.value.clients // [] | join(", "))\n  Exclude: \(.value.exclude // [] | join(", "))"
        ' 2>/dev/null || printf '%s\n' "Error: Failed to parse tunnels JSON"
    fi
    printf '\n'

    return 0
}

# -------------------------------------------------------------------------------------------------
# tunnel_get_required_ipsets - return list of ipsets needed for rules
# -------------------------------------------------------------------------------------------------
# Parses TUN_DIR_TUNNELS_JSON and returns exclude country codes.
# -------------------------------------------------------------------------------------------------
tunnel_get_required_ipsets() {
    if [[ -z $TUN_DIR_TUNNELS_JSON ]] || [[ $TUN_DIR_TUNNELS_JSON == "{}" ]]; then
        return 0
    fi

    printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | parse_exclude_sets_from_json
}

# -------------------------------------------------------------------------------------------------
# tunnel_stop - remove chain, ip rules and tunnel tables
# -------------------------------------------------------------------------------------------------
# Removes TUN_DIR chain, all associated ip rules and the tunnel tables this
# module owns (a no-op on a platform whose firmware owns them).
# -------------------------------------------------------------------------------------------------
tunnel_stop() {
    _tunnel_init

    log "Stopping Tunnel Director..."

    # Remove PREROUTING jump
    purge_fw_rules -q "mangle PREROUTING" "-j ${TUN_DIR_CHAIN}\$"

    # Delete chain if exists
    if fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        delete_fw_chain -q mangle "$TUN_DIR_CHAIN"
        log "Removed chain: $TUN_DIR_CHAIN"
    fi

    # Remove ip rules in our pref range
    local pref_base="${TUN_DIR_PREF_BASE:-16384}"
    local max_rules="${_tunnel_mark_field_max:-255}"
    local i

    for ((i = 0; i < max_rules; i++)); do
        ip rule del pref $((pref_base + i)) 2>/dev/null || true
    done

    # Release the tunnel tables this module owns (no-op on Merlin)
    local idx tunnel
    if [[ -f $TUN_DIR_TABLES ]]; then
        while read -r idx tunnel; do
            [[ -n $tunnel ]] || continue
            platform_tunnel_table_release "$tunnel" "$idx" || true
        done < "$TUN_DIR_TABLES"
        rm -f "$TUN_DIR_TABLES"
    fi

    # Clear hash file
    rm -f "$TUN_DIR_HASH"

    log "Tunnel Director stopped"
    return 0
}

# -------------------------------------------------------------------------------------------------
# tunnel_apply - apply rules from config (idempotent)
# -------------------------------------------------------------------------------------------------
# Applies TUN_DIR_TUNNELS_JSON configuration. Single chain, exclusion-based routing.
# -------------------------------------------------------------------------------------------------
tunnel_apply() {
    _tunnel_init

    local changes=0
    local warnings=0
    local skipped_unknown=0

    # Check if tunnels config is empty. A leftover chain/jump/ip rule would
    # still force previously matched clients into the tunnel, so tear down.
    if [[ -z $TUN_DIR_TUNNELS_JSON ]] || [[ $TUN_DIR_TUNNELS_JSON == "{}" ]]; then
        log "No tunnels configured"
        # Only when there is something to tear down: a state file, or a chain
        # that outlived its state files in /tmp. Every Xray-only install (the
        # template's default) takes this branch on every hook and cron run,
        # and an unconditional tunnel_stop is 255 `ip rule del` spawns, a
        # PREROUTING purge and two syslog lines each time.
        if [[ -f $TUN_DIR_HASH || -f $TUN_DIR_TABLES ]] || fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
            tunnel_stop
        fi
        return 0
    fi

    # Validate JSON before modifying firewall state
    if ! printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -e 'type == "object"' >/dev/null 2>&1; then
        log -l ERROR "Invalid tunnels JSON configuration"
        return 1
    fi

    # Compute config hash for change detection. Failover snapshot clients are
    # extra MARK rules, not a key reorder, so they belong in the hash.
    local new_hash old_hash empty_hash
    new_hash=$(printf '%s\n%s\n%s' "$TUN_DIR_TUNNELS_JSON" "${XRAY_FAILOVER_TUNNEL:-}" "${XRAY_FAILOVER_CLIENTS:-}" | compute_hash)
    empty_hash=$(printf '' | compute_hash)
    old_hash=$(cat "$TUN_DIR_HASH" 2>/dev/null || printf '%s' "$empty_hash")

    # Check if rebuild needed
    local rebuild=0
    if [[ $new_hash != "$old_hash" ]]; then
        rebuild=1
    elif ! fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        rebuild=1
    elif [[ ! -f $TUN_DIR_TABLES ]]; then
        rebuild=1
    fi

    if [[ $rebuild -eq 0 ]]; then
        if _tunnel_failover_needed; then
            if ! awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { found = 1 } END { exit !found }' "$TUN_DIR_TABLES"; then
                log -l ERROR "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
                return 1
            fi
            if ! _tunnel_ensure_routes; then
                log -l ERROR "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
                return 1
            fi
            if ! _tunnel_failover_rule_present; then
                log -l ERROR "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
                return 1
            fi
        else
            _tunnel_ensure_routes || true
        fi
        log "Rules are applied and up-to-date"
        return 0
    fi

    # The PREROUTING jumps below are what make the chain matter, so ask for the
    # LAN interfaces before touching any firewall state. A platform that cannot
    # name them would otherwise leave a fully populated chain with nothing
    # jumping to it - every client routed direct instead of through its tunnel -
    # and tunnel_apply would still return 0. Asking first leaves the rules that
    # are already installed alone.
    local lan_ifaces
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Cannot determine the LAN interfaces; Tunnel Director rules not applied"
        return 1
    fi

    # Stop existing rules when there is any prior state to clean up. Both state
    # files have to be consulted, not just the hash: an apply that skipped a
    # tunnel unknown to the platform records TUN_DIR_TABLES but deliberately no
    # hash, and keying the cleanup off the hash alone left those tables
    # allocated while the next apply handed their indices to other tunnels - on
    # Keenetic table 2000+idx kept the previous tunnel's route, so marked
    # traffic left through the wrong tunnel while the log reported the fallback
    # to main. tunnel_stop is what walks TUN_DIR_TABLES and releases them.
    if [[ $old_hash != "$empty_hash" || -f $TUN_DIR_TABLES ]]; then
        # rebuild also fires with the hash unchanged - a missing chain, or a
        # missing TUN_DIR_TABLES - and that is not a configuration change.
        if [[ $new_hash != "$old_hash" ]]; then
            log "Configuration changed; removing existing rules..."
        else
            log "Applied rules are incomplete; rebuilding..."
        fi
        tunnel_stop
        changes=1
    fi

    # Create the single chain
    create_fw_chain -q -f mangle "$TUN_DIR_CHAIN"

    # Get base position in PREROUTING. Unguarded, an rc 1 here would trip
    # errexit and end the whole CLI run without a log line - after tunnel_stop
    # and create_fw_chain, and before tproxy_apply. An empty base_pos would be
    # worse than the abort: the first jump would keep append semantics, but the
    # second would land at position 1, ahead of the firmware's iface-mark rules.
    local base_pos
    base_pos=$(platform_prerouting_base_pos) || base_pos=""
    if [[ -z $base_pos ]]; then
        log -l ERROR "Cannot determine the PREROUTING insert position; Tunnel Director rules not applied"
        return 1
    fi

    # Never ahead of the Xray jumps. XRAY_TPROXY inserts from position 1 and
    # runs before this chain by design (packet-flow.md); on Merlin the base
    # position is 1 as well when the firmware has no iface-mark rules, so a
    # rebuild put TUN_DIR first and the next apply found the Xray jump off its
    # position and purged and re-inserted it - a window with no TPROXY jump on
    # every apply. Count the jumps already there and go behind them.
    local xray_jumps
    xray_jumps=$(iptables -t mangle -S PREROUTING 2>/dev/null | grep -c -- "-j ${XRAY_CHAIN:-XRAY_TPROXY}\$" || true)
    if (( base_pos <= xray_jumps )); then
        base_pos=$((xray_jumps + 1))
    fi

    # A platform whose firmware accelerates established forwarded flows past
    # mangle names the target that opts a flow out of it (KeeneticOS: PPE);
    # one that needs none prints nothing and returns 1. Asked once rather than
    # per client: one answer keeps every client's block consistent, and the
    # platform is not made to answer the same question N times.
    local offload_target
    offload_target="$(platform_tunnel_offload_target)" || offload_target=""

    # Process each tunnel
    local tunnel_idx=0
    local fo_applied=0 fo_route_ok=1 fo_rule_ok=1
    local tables_tmp
    tables_tmp="$(tmp_file)"
    local tunnels
    # Use keys_unsorted to preserve JSON file order (not alphabetical sorting)
    tunnels=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r 'keys_unsorted[]')

    # Failover snapshot IPs get MARK first (first-match) with the failover
    # tunnel's mark. Reordering the whole tunnel would pull its other clients
    # ahead of earlier rules (main containing a host that a later CIDR also
    # covers). Slot assignment here must match the apply loop below.
    local fo_mark="" fo_on_tunnel=""
    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
        local plan_idx=0 plan_t plan_type plan_clients_type plan_clients plan_slot
        while IFS= read -r plan_t; do
            [[ -n $plan_t ]] || continue
            _tunnel_table_allowed "$plan_t" || continue
            plan_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$plan_t" '.[$t] | type')
            [[ $plan_type == object ]] || continue
            plan_clients_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$plan_t" '.[$t].clients | type')
            [[ $plan_clients_type == array || $plan_clients_type == null ]] || continue
            plan_clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$plan_t" '.[$t].clients // [] | .[]')
            [[ -n $plan_clients ]] || continue
            plan_slot=$((plan_idx + 1))
            if [[ $plan_slot -gt $_tunnel_mark_field_max ]]; then
                break
            fi
            if [[ $plan_t == "$XRAY_FAILOVER_TUNNEL" ]]; then
                fo_mark=$(printf '0x%x' $(( plan_slot << _tunnel_mark_shift_val )))
                fo_on_tunnel=$plan_clients
                break
            fi
            plan_idx=$((plan_idx + 1))
        done <<< "$tunnels"
    fi
    if [[ -n $fo_mark ]]; then
        local fo_excl_type fo_excludes="" fo_client
        fo_excl_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude | type')
        if [[ $fo_excl_type == array ]]; then
            fo_excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude // [] | .[]')
        fi
        for fo_client in $XRAY_FAILOVER_CLIENTS; do
            # DELETE /api/clients drops the address from the tunnel but leaves
            # xray.failover. An override for an IP no longer on this tunnel
            # would first-match it onto the old fallback.
            local fo_still=0 fo_have
            while IFS= read -r fo_have; do
                if [[ $fo_have == "$fo_client" ]]; then
                    fo_still=1
                    break
                fi
            done <<< "$fo_on_tunnel"
            [[ $fo_still -eq 1 ]] || continue
            _tunnel_emit_client "$fo_client" "$XRAY_FAILOVER_TUNNEL" "$fo_mark" "$fo_excludes"
        done
    fi

    while IFS= read -r tunnel; do
        [[ -n $tunnel ]] || continue

        # Validate the tunnel is one the platform knows
        if ! _tunnel_table_allowed "$tunnel"; then
            log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
            warnings=1
            skipped_unknown=1
            continue
        fi

        # Validate tunnel config is an object (not string or other type)
        local tunnel_type
        tunnel_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t] | type')
        if [[ $tunnel_type != "object" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid config (expected object, got $tunnel_type); skipping"
            warnings=1
            continue
        fi

        # Validate clients is an array
        local clients_type
        clients_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients | type')
        if [[ $clients_type != "array" ]] && [[ $clients_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid clients (expected array, got $clients_type); skipping"
            warnings=1
            continue
        fi

        # Validate exclude is an array (if present)
        local exclude_type
        exclude_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude | type')
        if [[ $exclude_type != "array" ]] && [[ $exclude_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid exclude (expected array, got $exclude_type); skipping exclusions"
            warnings=1
            exclude_type="null"  # Skip excludes but continue with clients
        fi

        # Get clients and excludes for this tunnel
        local clients excludes
        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        if [[ $exclude_type == "array" ]]; then
            excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude // [] | .[]')
        else
            excludes=""
        fi

        if [[ -z $clients ]]; then
            log -l WARN "Tunnel '$tunnel' has no clients; skipping"
            warnings=1
            continue
        fi

        # Compute fwmark for this tunnel
        local slot=$((tunnel_idx + 1))
        if [[ $slot -gt $_tunnel_mark_field_max ]]; then
            log -l WARN "Too many tunnels (max $_tunnel_mark_field_max); skipping '$tunnel'"
            warnings=1
            continue
        fi

        local mark_val=$(( slot << _tunnel_mark_shift_val ))
        local mark_hex
        mark_hex=$(printf '0x%x' "$mark_val")

        # Add rules for each client. Snapshot IPs were already emitted first.
        while IFS= read -r client; do
            [[ -n $client ]] || continue
            if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
                local fo_skip=0 fo_c
                for fo_c in $XRAY_FAILOVER_CLIENTS; do
                    if [[ $client == "$fo_c" ]]; then
                        fo_skip=1
                        break
                    fi
                done
                [[ $fo_skip -eq 0 ]] || continue
            fi
            _tunnel_emit_client "$client" "$tunnel" "$mark_hex" "$excludes"
        done <<< "$clients"

        # Routing table for this tunnel: a firmware table on Merlin, one this
        # module owns on Keenetic. The ip rule is installed even when the route
        # is not: a lookup in an empty table falls through to main.
        local table
        table="$(platform_tunnel_table "$tunnel" "$tunnel_idx")"
        # Start from an empty table. An apply that died after ensuring this
        # route but before TUN_DIR_TABLES was written (NDM wiping the chain
        # mid-apply, a busy xtables lock, a signal) left the route with no
        # record, so the cleanup above never released it, and a route_ensure
        # that fails now (interface down) would leave the previous owner's
        # route behind the ip rule installed below. No-op on Merlin.
        platform_tunnel_table_release "$tunnel" "$tunnel_idx" || true
        local route_ok=1 rule_ok=1
        if ! platform_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
            log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
            warnings=1
            route_ok=0
        fi

        local pref=$((TUN_DIR_PREF_BASE + tunnel_idx))
        ip rule del pref "$pref" 2>/dev/null || true
        if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
            log -l ERROR "Failed to add ip rule: pref=$pref fwmark=$mark_hex lookup=$table"
            warnings=1
            rule_ok=0
        fi

        printf '%s %s\n' "$tunnel_idx" "$tunnel" >> "$tables_tmp"
        if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" ]]; then
            fo_applied=1
            [[ $route_ok -eq 1 ]] || fo_route_ok=0
            [[ $rule_ok -eq 1 ]] || fo_rule_ok=0
        fi
        tunnel_idx=$((tunnel_idx + 1))
    done <<< "$tunnels"

    # Jump from PREROUTING to TUN_DIR for traffic from every LAN interface, each
    # at its own position (base_pos, base_pos + 1, ...). One shared position
    # would make every interface displace the one before it, so sync_fw_rule
    # would find each jump off its position and purge-and-re-insert all of them
    # on every apply - and each rewrite is a window with no jump for that
    # interface.
    local lan_if pos="$base_pos"
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        sync_fw_rule -q mangle PREROUTING "-i $lan_if .*-j ${TUN_DIR_CHAIN}\$" \
            "-i $lan_if -m mark --mark 0x0/$_tunnel_mark_mask_hex -j $TUN_DIR_CHAIN" "$pos"
        pos=$((pos + 1))
    done <<< "$lan_ifaces"

    # Save hash and the applied tunnel table. The hash is what makes the next
    # apply take the up-to-date branch, so it is recorded only when every
    # configured tunnel was actually applied. A tunnel the platform does not list
    # is not a configuration state: on Keenetic platform_tunnels answers only
    # "main" while RCI does not reply, and the netfilter.d hook fires exactly
    # during an NDM rebuild, when it may well not. Recording the hash there would
    # send every later apply down the up-to-date branch and never restore the
    # routing until the next rebuild wipes the chain - a silent fail-open. On
    # Merlin the only case is a typo in the tunnel id, which then warns on every
    # apply instead of once.
    mkdir -p "$(dirname "$TUN_DIR_HASH")"
    cp -f "$tables_tmp" "$TUN_DIR_TABLES"

    if [[ $skipped_unknown -eq 0 ]]; then
        printf '%s\n' "$new_hash" > "$TUN_DIR_HASH"
    else
        rm -f "$TUN_DIR_HASH"
        log -l WARN "Tunnel Director: a configured tunnel is unknown to the platform (RCI down, or a typo in the id); this apply is not recorded as up-to-date and the next apply retries"
    fi

    # Still return 1 so the watch does not drop Xray membership. Keep the hash
    # when every configured tunnel was applied: deleting it forced the next
    # apply through tunnel_stop, which takes TUN_DIR down for every client
    # while the fallback interface is still coming up.
    if _tunnel_failover_needed && [[ $fo_applied -eq 0 || $fo_route_ok -eq 0 || $fo_rule_ok -eq 0 ]]; then
        log -l ERROR "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
        return 1
    fi

    if [[ $changes -eq 0 ]]; then
        log "No changes applied"
    elif [[ $warnings -eq 0 ]]; then
        log "All rules applied successfully"
    else
        log -l WARN "Completed with warnings"
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
