#!/usr/bin/env bash

###################################################################################################
# tunnel.sh - tunnel director module for VPN Director
# -------------------------------------------------------------------------------------------------
# Purpose:
#   Modular library for tunnel director operations: status, apply, stop.
#   Routes LAN client traffic through VPN tunnels with exclusion-based routing.
#
# Dependencies:
#   - common.sh (log, tmp_file, compute_hash, is_lan_ip, is_ipv4_net, rt_table_label)
#     and, through it, the platform contract (platform_tunnels, platform_tunnel_table,
#     platform_tunnel_route_ensure, platform_tunnel_table_release,
#     platform_prerouting_base_pos, platform_lan_ifaces)
#   - firewall.sh (delete_fw_chain, ensure_fw_rule, sync_fw_rule, swap_fw_chain,
#                  purge_fw_rules, find_fw_rules, fw_chain_exists)
#   - config.sh (TUN_DIR_TUNNELS_JSON, TUN_DIR_CHAIN, TUN_DIR_PREF_BASE,
#                TUN_DIR_MARK_MASK, TUN_DIR_MARK_SHIFT; XRAY_CHAIN, to keep the
#                PREROUTING jump behind the Xray jumps)
#   - ipset.sh (_ipset_exists, parse_exclude_sets_from_json, TUN_DIR_HASH, TUN_DIR_TABLES)
#
# Public API:
#   tunnel_status()              - show TUN_DIR chain, ip rules, configured tunnels
#   tunnel_apply()               - apply rules from config (idempotent; a rebuild happens in place)
#   tunnel_stop()                - remove chain, ip rules and tunnel tables
#   tunnel_get_required_ipsets() - return list of ipsets needed for rules
#
# Internal functions (for testing):
#   _tunnel_table_allowed()      - check if a tunnel id is one the platform lists
#   _tunnel_gateway()            - the configured gateway of a tunnel, or nothing
#   _tunnel_ensure_routes()      - re-install the routes and ip rules of every applied tunnel
#   _tunnel_jumps_ensure()       - put back a missing PREROUTING jump without a rebuild
#   _tunnel_marks_present()      - is every client's MARK rule still in TUN_DIR
#   _tunnel_collect_applied()    - the slot of every tunnel: kept from TUN_DIR_TABLES, or the lowest free
#   _tunnel_build_chain()        - fill a fresh chain with every client's rules (swap_fw_chain's build_fn)
#   _tunnel_slot_release()       - drop the ip rule and the table of a slot the new layout dropped
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
        # The rule outlives an interface flap, but one that never went in at
        # rebuild time is retried nowhere else. The failover check below reads
        # the result; a rule that cannot be installed has logged its own error.
        _tunnel_rule_ensure "$idx" "$tunnel" || true
    done < "$TUN_DIR_TABLES"
    return "$rc"
}

# -------------------------------------------------------------------------------------------------
# _tunnel_rule_ensure - put back the ip rule of a recorded tunnel
# -------------------------------------------------------------------------------------------------
# A rebuild records TUN_DIR_HASH even when an "ip rule add" failed, so every
# later apply lands in the up-to-date branch and this is the only place left to
# retry. Without it a rule that never went in stays gone until the configuration
# changes: those clients fall through to main, and for the failover tunnel
# failover_ready is never written, so the watch keeps its Xray clients on a dead
# outbound.
#
# A rule that is in place is left alone. Deleting and re-adding it would open a
# window in which marked packets fall through to main. Another rule on the
# preference makes way for it: the range is this module's, as the rebuild and
# tunnel_stop treat it.
# -------------------------------------------------------------------------------------------------
_tunnel_rule_ensure() {
    local idx="$1" tunnel="$2"
    local pref=$((TUN_DIR_PREF_BASE + idx))
    local mark_hex table
    mark_hex=$(_tunnel_mark_hex "$idx")
    table="$(platform_tunnel_table "$tunnel" "$idx")"
    if _tunnel_rule_listed "$pref" "$mark_hex" "$table"; then
        return 0
    fi
    ip rule del pref "$pref" 2>/dev/null || true
    if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
        log -l ERROR "Tunnel '$tunnel': ip rule still not installed: pref=$pref fwmark=$mark_hex lookup=$table"
        return 1
    fi
    log "Tunnel '$tunnel': re-installed the ip rule at pref $pref"
    return 0
}

# _tunnel_mark_hex <idx> - the fwmark of the tunnel in slot idx, as ip prints it
_tunnel_mark_hex() {
    printf '0x%x' $(( ($1 + 1) << _tunnel_mark_shift_val ))
}

# _tunnel_rule_listed <pref> <mark> <table> - is this module's rule at that
# preference: "from all fwmark <mark>/<mask> lookup <table>"? Another rule can
# hold the same preference - a firmware's, a user's, one an older layout left -
# and a look at the preference alone took it for ours: the up-to-date path never
# put ours back, and failover_ready went out with the failover clients' marks
# routed by that rule or by main. The table is compared as the kernel prints it
# (rt_table_label) and every part as a whole word: iproute2 4.4 ends each rule
# with a space, and a mask of all ones is left out of the listing.
#
# "ip rule show pref N" is refused by iproute2 4.4 (Entware's ip-full), so the
# unfiltered listing is searched here - read whole first. Piped into "grep -q"
# it was wrong under pipefail: grep leaves at its match, iproute2 writes each
# rule as it prints it, and the SIGPIPE that ends "ip" made the pipeline report
# 141, so a rule in place read as missing.
_tunnel_rule_listed() {
    local pref="$1" fwmark="fwmark $2/$_tunnel_mark_mask_hex" lookup rules line
    [[ $_tunnel_mark_mask_hex != 0xffffffff ]] || fwmark="fwmark $2"
    lookup="lookup $(rt_table_label "$3")"
    rules=$(ip rule show 2>/dev/null) || true
    while IFS= read -r line; do
        [[ $line == "$pref:"* ]] || continue
        line=" ${line#*:} "
        line=${line//$'\t'/ }
        if [[ $line == *" from all "* && $line == *" $fwmark "* && $line == *" $lookup "* ]]; then
            return 0
        fi
    done <<< "$rules"
    return 1
}

# True when the failover tunnel's own ip rule is at pref TUN_DIR_PREF_BASE+idx.
_tunnel_failover_rule_present() {
    local idx table
    [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]] || return 0
    [[ -f $TUN_DIR_TABLES ]] || return 1
    idx=$(awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { print $1; exit }' "$TUN_DIR_TABLES")
    [[ -n $idx ]] || return 1
    table=$(platform_tunnel_table "$XRAY_FAILOVER_TUNNEL" "$idx") || return 1
    _tunnel_rule_listed $((TUN_DIR_PREF_BASE + idx)) "$(_tunnel_mark_hex "$idx")" "$table"
}

# _tunnel_failover_carried - does TUN_DIR mark every failover client still on
# the failover tunnel? One outside RFC1918 is TPROXY's to take but never marked
# here: dropped from Xray on failover_ready, it would leave through the WAN.
# An entry that is no IPv4 address at all is neither TPROXY's nor ours and does
# not count. A MARK rule the kernel refused is the rebuild's to see - that
# rebuild is not recorded, so the up-to-date path never runs on one - and one
# that went missing since sends the apply back through a rebuild before this is
# asked (_tunnel_marks_present).
_tunnel_failover_carried() {
    local fo_client fo_on_tunnel
    fo_on_tunnel=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "${XRAY_FAILOVER_TUNNEL:-}" '.[$t].clients // [] | .[]')
    for fo_client in ${XRAY_FAILOVER_CLIENTS:-}; do
        grep -qxF -- "$fo_client" <<< "$fo_on_tunnel" || continue
        is_ipv4_net "$fo_client" || continue
        is_lan_ip "${fo_client%%/*}" || return 1
    done
    return 0
}

# _tunnel_marks_present - does TUN_DIR still hold the MARK rule of every client
# the recorded rebuild marked? The hash, the chain and TUN_DIR_TABLES say that a
# rebuild ran, not that its rules are still there: Merlin's firewall start runs
# "iptables -t mangle -F", which empties every chain of the table and deletes
# none, and firewall-start then applies again. The up-to-date path used to put
# the jumps back into the empty chain and write failover_ready, and every Tunnel
# Director client - the failover clients the watch then took off Xray among
# them - left through the WAN until the configuration changed.
#
# Each client of each recorded tunnel is looked up with "iptables -C", as
# ensure_fw_rule looks before it adds; the failover clients are clients of their
# tunnel under its mark, so they are among them. A client the rebuild skips - no
# IPv4 address, outside RFC1918 - has no rule to find, and looking for one would
# rebuild the chain on every apply. The exclusion and offload rules are not
# looked for: a flush takes the MARK rules with them.
_tunnel_marks_present() {
    local idx tunnel mark_hex clients client
    [[ -f $TUN_DIR_TABLES ]] || return 1
    while read -r idx tunnel; do
        [[ -n $tunnel ]] || continue
        mark_hex=$(_tunnel_mark_hex "$idx")
        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        while IFS= read -r client; do
            [[ -n $client ]] || continue
            is_ipv4_net "$client" || continue
            is_lan_ip "${client%%/*}" || continue
            if ! iptables -t mangle -C "$TUN_DIR_CHAIN" -s "$client" \
                -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
                -j MARK --set-xmark "$mark_hex/$_tunnel_mark_mask_hex" 2>/dev/null; then
                log -l WARN "Tunnel '$tunnel': client '$client' has no MARK rule in $TUN_DIR_CHAIN any more (the firewall flushed the chain?)"
                return 1
            fi
        done <<< "$clients"
    done < "$TUN_DIR_TABLES"
    return 0
}

# True when the failover tunnel still has clients after pause filtering.
_tunnel_failover_needed() {
    [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]] || return 1
    local c
    c=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].clients // [] | .[]')
    [[ -n $c ]]
}

# _tunnel_collect_applied <prev> - print "idx tunnel" for every tunnel that gets a
# slot, in JSON order. A tunnel <prev> (the previous TUN_DIR_TABLES) records keeps
# its idx - its mark, preference and table - so a rebuild puts its chain in place
# over the rules it already has. A tunnel new to the layout takes the lowest idx
# that neither layout holds: a slot this apply frees still carries marks until the
# swap, and its ip rule goes only after it. warnings, skipped_unknown and
# incomplete are tunnel_apply's locals (bash dynamic scope). One pass, so the
# failover MARK slots cannot drift from the rules.
_tunnel_collect_applied() {
    local prev="${1:-/dev/null}"
    local tunnel tunnel_type clients_type clients idx used tunnels
    tunnels=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r 'keys_unsorted[]')
    used=" $(awk '{ printf "%s ", $1 }' "$prev" 2>/dev/null) "
    while IFS= read -r tunnel; do
        [[ -n $tunnel ]] || continue
        if ! _tunnel_table_allowed "$tunnel"; then
            log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
            warnings=1
            skipped_unknown=1
            continue
        fi
        tunnel_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t] | type')
        if [[ $tunnel_type != "object" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid config (expected object, got $tunnel_type); skipping"
            warnings=1
            continue
        fi
        clients_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients | type')
        if [[ $clients_type != "array" ]] && [[ $clients_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid clients (expected array, got $clients_type); skipping"
            warnings=1
            continue
        fi
        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        if [[ -z $clients ]]; then
            log -l WARN "Tunnel '$tunnel' has no clients; skipping"
            warnings=1
            continue
        fi
        idx=$(awk -v id="$tunnel" '$2 == id { print $1; exit }' "$prev" 2>/dev/null)
        [[ $idx =~ ^[0-9]+$ ]] || idx=""
        if [[ -z $idx ]]; then
            idx=0
            while [[ $used == *" $idx "* ]]; do
                idx=$((idx + 1))
            done
        fi
        if [[ $((idx + 1)) -gt $_tunnel_mark_field_max ]]; then
            # The field can be full while the tunnels would fit it: a slot this
            # apply frees is not handed out before the swap. Not recorded, so the
            # next apply rebuilds and gives this tunnel the slot freed by then.
            log -l WARN "Too many tunnels (max $_tunnel_mark_field_max); skipping '$tunnel'"
            warnings=1
            incomplete=1
            continue
        fi
        used+="$idx "
        printf '%s %s\n' "$idx" "$tunnel"
    done <<< "$tunnels"
}

# One client's RETURN / offload / MARK in <chain>. Offload sits immediately
# before MARK with the same match so excluded destinations keep acceleration.
# warnings, changes, incomplete and offload_target are tunnel_apply's locals
# (bash dynamic scope); incomplete says a rule did not go in, so the rebuild is
# not recorded.
#
# Returns 0 when the client is marked, 1 when it is skipped as no IPv4 address
# or CIDR at all - nothing TPROXY can take either - 2 when it is skipped as
# outside RFC1918, and 3 when its MARK rule did not go in. The callers run it
# under "||", which turns errexit off here, so every rule is checked by hand:
# under errexit one refused rule used to end the whole apply half-way.
_tunnel_emit_client() {
    local chain="$1" client="$2" tunnel="$3" mark_hex="$4" excludes="$5"
    local client_ip="${client%%/*}"
    # is_lan_ip looks at the prefix only: 192.168.1.1000 passes it and then
    # makes "iptables -s" fail, 192.168.1.010 would be read as .8.
    if ! is_ipv4_net "$client"; then
        log -l WARN "Client '$client' is not an IPv4 address or CIDR; skipping"
        warnings=1
        return 1
    fi
    if ! is_lan_ip "$client_ip"; then
        log -l WARN "Client '$client' is not RFC1918; skipping"
        warnings=1
        return 2
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
        if ! ensure_fw_rule -q mangle "$chain" \
            -s "$client" -m set --match-set "$excl_set" dst -j RETURN; then
            warnings=1
            incomplete=1
        fi
    done <<< "$excludes"

    if [[ -n $offload_target ]]; then
        if ! ensure_fw_rule -q mangle "$chain" \
            -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
            -j "$offload_target"; then
            warnings=1
            incomplete=1
        fi
    fi

    if ! ensure_fw_rule -q mangle "$chain" \
        -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
        -j MARK --set-xmark "$mark_hex/$_tunnel_mark_mask_hex"; then
        log -l ERROR "Client '$client' is not marked for tunnel '$tunnel'; its traffic falls through to main"
        warnings=1
        incomplete=1
        return 3
    fi

    log "Added: client=$client tunnel=$tunnel mark=$mark_hex"
    changes=1
}

# _tunnel_prerouting_pos - where the first TUN_DIR jump goes
# The platform's base position, never ahead of the Xray jumps. XRAY_TPROXY
# inserts from position 1 and runs before this chain by design (packet-flow.md);
# on Merlin the base position is 1 as well when the firmware has no iface-mark
# rules, so a rebuild put TUN_DIR first and the next apply found the Xray jump
# off its position and purged and re-inserted it - a window with no TPROXY jump
# on every apply. Count the jumps already there and go behind them. Returns 1
# when the platform cannot say.
_tunnel_prerouting_pos() {
    local base_pos xray_jumps
    base_pos=$(platform_prerouting_base_pos) || base_pos=""
    [[ -n $base_pos ]] || return 1
    xray_jumps=$(iptables -t mangle -S PREROUTING 2>/dev/null | grep -c -- "-j ${XRAY_CHAIN:-XRAY_TPROXY}\$" || true)
    if (( base_pos <= xray_jumps )); then
        base_pos=$((xray_jumps + 1))
    fi
    printf '%s\n' "$base_pos"
}

# _tunnel_jumps_ensure - put back a PREROUTING jump that is missing
# A rebuild whose jumps did not all go in records no hash, and the next apply
# rebuilds; a jump that goes missing after a recorded rebuild is put back here,
# on the up-to-date path. A jump that is there stays where it is: moving it is a
# window with no jump. Returns 1 when a jump is still missing.
_tunnel_jumps_ensure() {
    local lan_ifaces lan_if pos="" idx=0 rc=0
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    [[ -n $lan_ifaces ]] || return 1
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        if [[ -z $(find_fw_rules "mangle PREROUTING" "-i $lan_if .*-j ${TUN_DIR_CHAIN}\$") ]]; then
            if [[ -z $pos ]]; then
                pos=$(_tunnel_prerouting_pos) || return 1
            fi
            if sync_fw_rule -q mangle PREROUTING "-i $lan_if .*-j ${TUN_DIR_CHAIN}\$" \
                "-i $lan_if -m mark --mark 0x0/$_tunnel_mark_mask_hex -j $TUN_DIR_CHAIN" "$((pos + idx))"; then
                log "Tunnel Director: re-installed the PREROUTING jump for $lan_if"
            else
                rc=1
            fi
        fi
        idx=$((idx + 1))
    done <<< "$lan_ifaces"
    return "$rc"
}

# _tunnel_build_chain <chain> - fill a fresh chain with every client's rules
# The build_fn of swap_fw_chain. It reads the new layout from slots_tmp and sets
# warnings, incomplete, changes and fo_carried - tunnel_apply's locals (bash
# dynamic scope; swap_fw_chain's own locals carry a _sw_ prefix). The failover
# snapshot clients come first, under the failover tunnel's mark, so a covering
# earlier rule (often main) does not send them to the WAN; the other tunnels
# follow in JSON order. Returns 0: a refused rule costs its client only
# (_tunnel_emit_client says which), and the chain always goes in.
_tunnel_build_chain() {
    local chain="$1"
    local tunnel_idx tunnel mark_hex exclude_type clients excludes client
    local fo_idx fo_mark="" fo_on_tunnel="" fo_excl_type fo_excludes="" fo_client fo_rc
    local fo_have fo_still fo_skip fo_c

    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
        fo_idx=$(awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { print $1; exit }' "$slots_tmp")
        if [[ -n $fo_idx ]]; then
            fo_mark=$(_tunnel_mark_hex "$fo_idx")
            fo_on_tunnel=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].clients // [] | .[]')
        fi
    fi
    if [[ -n $fo_mark ]]; then
        fo_excl_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude | type')
        if [[ $fo_excl_type == array ]]; then
            fo_excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude // [] | .[]')
        fi
        for fo_client in $XRAY_FAILOVER_CLIENTS; do
            # DELETE /api/clients drops the address from the tunnel but leaves
            # xray.failover. An override for an IP no longer on this tunnel
            # would first-match it onto the old fallback.
            fo_still=0
            while IFS= read -r fo_have; do
                if [[ $fo_have == "$fo_client" ]]; then
                    fo_still=1
                    break
                fi
            done <<< "$fo_on_tunnel"
            [[ $fo_still -eq 1 ]] || continue
            fo_rc=0
            _tunnel_emit_client "$chain" "$fo_client" "$XRAY_FAILOVER_TUNNEL" "$fo_mark" "$fo_excludes" || fo_rc=$?
            # 1 is no address at all, which TPROXY cannot take either. 2 and 3
            # are clients TPROXY takes and TUN_DIR does not mark: dropped from
            # Xray on failover_ready, they would leave through the WAN.
            [[ $fo_rc -lt 2 ]] || fo_carried=0
        done
    fi

    while read -r tunnel_idx tunnel; do
        [[ -n $tunnel ]] || continue

        # Validate exclude is an array (if present)
        exclude_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude | type')
        if [[ $exclude_type != "array" ]] && [[ $exclude_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid exclude (expected array, got $exclude_type); skipping exclusions"
            warnings=1
            exclude_type="null"  # Skip excludes but continue with clients
        fi

        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        if [[ $exclude_type == "array" ]]; then
            excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude // [] | .[]')
        else
            excludes=""
        fi
        mark_hex=$(_tunnel_mark_hex "$tunnel_idx")

        # Add rules for each client. Snapshot IPs were already emitted first.
        while IFS= read -r client; do
            [[ -n $client ]] || continue
            if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
                fo_skip=0
                for fo_c in $XRAY_FAILOVER_CLIENTS; do
                    if [[ $client == "$fo_c" ]]; then
                        fo_skip=1
                        break
                    fi
                done
                [[ $fo_skip -eq 0 ]] || continue
            fi
            # A client that is skipped or not marked has said so; the rest of
            # the tunnel, and every other tunnel, still goes in.
            _tunnel_emit_client "$chain" "$client" "$tunnel" "$mark_hex" "$excludes" || true
        done <<< "$clients"
    done < "$slots_tmp"
    return 0
}

# _tunnel_slot_release <idx> <tunnel> - drop the ip rule and the table of a slot
# the new layout no longer holds. Called after the swap, when no packet carries
# the slot's mark any more. Only this module's rule goes (_tunnel_rule_listed):
# another owner's rule on the preference stays, as the up-to-date path leaves it.
_tunnel_slot_release() {
    local idx="$1" tunnel="$2" pref mark_hex table
    pref=$((TUN_DIR_PREF_BASE + idx))
    mark_hex=$(_tunnel_mark_hex "$idx")
    if table="$(platform_tunnel_table "$tunnel" "$idx")" && _tunnel_rule_listed "$pref" "$mark_hex" "$table"; then
        if ! ip rule del pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
            log -l WARN "Tunnel '$tunnel': the ip rule at pref $pref did not go; it routes a mark nothing sets any more"
        fi
    fi
    platform_tunnel_table_release "$tunnel" "$idx" || true
    log "Tunnel '$tunnel': released slot $idx"
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

    # Remove the PREROUTING jumps, those to a shadow an interrupted swap left included
    purge_fw_rules -q "mangle PREROUTING" "-j ${TUN_DIR_CHAIN}(_NEW)?\$"

    # Delete chain if exists
    if fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        delete_fw_chain -q mangle "$TUN_DIR_CHAIN"
        log "Removed chain: $TUN_DIR_CHAIN"
    fi
    if fw_chain_exists mangle "${TUN_DIR_CHAIN}_NEW"; then
        delete_fw_chain -q mangle "${TUN_DIR_CHAIN}_NEW" || true
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
    rm -f "$TUN_DIR_HASH" "$TUN_DIR_FAILOVER_READY"

    log "Tunnel Director stopped"
    return 0
}

# -------------------------------------------------------------------------------------------------
# tunnel_apply - apply rules from config (idempotent)
# -------------------------------------------------------------------------------------------------
# Applies TUN_DIR_TUNNELS_JSON configuration. Single chain, exclusion-based routing.
#
# A rebuild happens in place, make before break. The routes and ip rules of the new layout go in
# first, the chain is built beside the live one and swapped in (swap_fw_chain), and only then do
# the slots the new layout dropped lose their rules and tables. A tunnel keeps its slot - its
# mark, preference and table - for as long as it has clients (_tunnel_collect_applied), so a swap
# changes the chain alone. The rebuild used to start with tunnel_stop: every Tunnel Director
# client left through the WAN until the last rule was back, on every change of any client.
# TUN_DIR_FORCE_REBUILD=1 rebuilds even when the state reads up to date ("restart").
# -------------------------------------------------------------------------------------------------
tunnel_apply() {
    _tunnel_init

    local changes=0
    local warnings=0
    local skipped_unknown=0
    local incomplete=0

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
    # extra MARK rules, not a key reorder, so they belong in the hash. With no
    # failover, hash only the tunnels JSON so an upgrade does not rebuild TUN_DIR.
    local new_hash old_hash empty_hash
    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]]; then
        new_hash=$(printf '%s\n%s\n%s' "$TUN_DIR_TUNNELS_JSON" "$XRAY_FAILOVER_TUNNEL" "${XRAY_FAILOVER_CLIENTS:-}" | compute_hash)
    else
        new_hash=$(printf '%s' "$TUN_DIR_TUNNELS_JSON" | compute_hash)
    fi
    empty_hash=$(printf '' | compute_hash)
    old_hash=$(cat "$TUN_DIR_HASH" 2>/dev/null || printf '%s' "$empty_hash")

    # Check if rebuild needed
    local rebuild=0
    if [[ ${TUN_DIR_FORCE_REBUILD:-0} == 1 ]]; then
        rebuild=1
    elif [[ $new_hash != "$old_hash" ]]; then
        rebuild=1
    elif ! fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        rebuild=1
    elif [[ ! -f $TUN_DIR_TABLES ]]; then
        rebuild=1
    elif ! _tunnel_marks_present; then
        rebuild=1
    fi

    if [[ $rebuild -eq 0 ]]; then
        # Always re-install recorded routes first. Gating that on the failover
        # row being in TUN_DIR_TABLES skipped Keenetic route repair for every
        # other tunnel after an interface flap.
        local route_rc=0 jumps_rc=0
        _tunnel_ensure_routes || route_rc=$?
        if ! _tunnel_jumps_ensure; then
            log -l ERROR "Tunnel Director: a PREROUTING jump to $TUN_DIR_CHAIN is missing and did not go back in"
            jumps_rc=1
        fi
        if _tunnel_failover_needed; then
            if ! awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { found = 1 } END { exit !found }' "$TUN_DIR_TABLES" \
                || [[ $route_rc -ne 0 ]] \
                || [[ $jumps_rc -ne 0 ]] \
                || ! _tunnel_failover_carried \
                || ! _tunnel_failover_rule_present; then
                rm -f "$TUN_DIR_FAILOVER_READY"
                log -l WARN "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
            else
                printf '%s\n' "$XRAY_FAILOVER_TUNNEL" > "$TUN_DIR_FAILOVER_READY"
            fi
        else
            rm -f "$TUN_DIR_FAILOVER_READY"
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

    # Where the jumps go is asked first for the same reason: an rc 1 here used to
    # trip errexit mid-rebuild and end the whole CLI run without a log line.
    # swap_fw_chain asks again for the insert, on the listing as it is then.
    if ! _tunnel_prerouting_pos >/dev/null; then
        log -l ERROR "Cannot determine the PREROUTING insert position; Tunnel Director rules not applied"
        return 1
    fi

    if [[ ${TUN_DIR_FORCE_REBUILD:-0} == 1 ]]; then
        log "Rebuilding Tunnel Director in place..."
    elif [[ $new_hash != "$old_hash" ]]; then
        log "Configuration changed; rebuilding in place..."
    else
        # A rebuild also fires with the hash unchanged - a missing chain, a
        # missing TUN_DIR_TABLES, a MARK rule gone - and that is no change.
        log "Applied rules are incomplete; rebuilding..."
    fi
    # Written back only by a rebuild that completes. One that dies part-way
    # leaves the next apply a rebuild as well, and that one finishes the swap
    # the dead one left and releases the slots it left on record. The failover
    # marker goes with the hash, as it went with the tunnel_stop that used to
    # stand here: the watch trusts the marker over the apply's exit status, so a
    # rebuild that dies part-way must not leave a stale "ready" behind.
    rm -f "$TUN_DIR_HASH" "$TUN_DIR_FAILOVER_READY"
    changes=1

    # A platform whose firmware accelerates established forwarded flows past
    # mangle names the target that opts a flow out of it (KeeneticOS: PPE);
    # one that needs none prints nothing and returns 1. Asked once rather than
    # per client: one answer keeps every client's block consistent, and the
    # platform is not made to answer the same question N times.
    local offload_target
    offload_target="$(platform_tunnel_offload_target)" || offload_target=""

    local fo_applied=0 fo_route_ok=1 fo_rule_ok=1 fo_carried=1
    local prev_tmp slots_tmp
    prev_tmp="$(tmp_file)"
    slots_tmp="$(tmp_file)"
    if [[ -f $TUN_DIR_TABLES ]]; then
        cp -f "$TUN_DIR_TABLES" "$prev_tmp"
    else
        : > "$prev_tmp"
    fi
    _tunnel_collect_applied "$prev_tmp" > "$slots_tmp"

    # 1. Routing, before any packet carries a new mark. TUN_DIR_TABLES holds
    # both layouts until the release below: an apply that dies in between
    # leaves every slot in use on record, and the next one releases the extras.
    mkdir -p "$(dirname "$TUN_DIR_TABLES")"
    awk '!seen[$0]++' "$prev_tmp" "$slots_tmp" > "$TUN_DIR_TABLES"

    local tunnel_idx tunnel table pref mark_hex route_ok rule_ok
    while read -r tunnel_idx tunnel; do
        [[ -n $tunnel ]] || continue
        route_ok=1
        rule_ok=1
        if grep -qxF "$tunnel_idx $tunnel" "$prev_tmp"; then
            # A kept slot is carrying its clients now: its route and its rule
            # are ensured, never released - an empty table is a window in which
            # they fall through to main.
            if ! platform_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
                warnings=1
                route_ok=0
            fi
            if ! _tunnel_rule_ensure "$tunnel_idx" "$tunnel"; then
                warnings=1
                rule_ok=0
            fi
        else
            # A new slot starts from an empty table. An apply that died after
            # ensuring a route but before recording its slot left the route
            # with no record, and a route_ensure that fails now (interface
            # down) would leave the previous owner's route behind the ip rule
            # installed below. No-op on Merlin.
            platform_tunnel_table_release "$tunnel" "$tunnel_idx" || true
            if ! platform_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
                warnings=1
                route_ok=0
            fi
            # The ip rule goes in even when the route does not: a lookup in an
            # empty table falls through to main.
            table="$(platform_tunnel_table "$tunnel" "$tunnel_idx")"
            pref=$((TUN_DIR_PREF_BASE + tunnel_idx))
            mark_hex=$(_tunnel_mark_hex "$tunnel_idx")
            ip rule del pref "$pref" 2>/dev/null || true
            if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
                log -l ERROR "Failed to add ip rule: pref=$pref fwmark=$mark_hex lookup=$table"
                warnings=1
                rule_ok=0
            fi
        fi
        if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" ]]; then
            fo_applied=1
            [[ $route_ok -eq 1 ]] || fo_route_ok=0
            [[ $rule_ok -eq 1 ]] || fo_rule_ok=0
        fi
    done < "$slots_tmp"

    # 2. The chain, built beside the live one and swapped in, one jump per LAN
    # interface. The mark test on the jump makes the first match win: a packet an
    # earlier rule marked skips this chain.
    local lan_if swap_rc=0 jumps_ok=1
    local -a jump_matches=()
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        jump_matches+=("-i $lan_if -m mark --mark 0x0/$_tunnel_mark_mask_hex")
    done <<< "$lan_ifaces"
    swap_fw_chain mangle "$TUN_DIR_CHAIN" _tunnel_build_chain _tunnel_prerouting_pos "${jump_matches[@]}" || swap_rc=$?
    if [[ $swap_rc -ge 2 ]]; then
        log -l ERROR "Tunnel Director: the rebuilt chain did not take over every LAN interface; the next apply finishes it"
        warnings=1
        jumps_ok=0
    fi

    # 3. Release the slots the new layout dropped - only once the new chain
    # carries every interface, since until then the old chain marks with them.
    if [[ $jumps_ok -eq 1 ]]; then
        local old_idx old_tunnel
        while read -r old_idx old_tunnel; do
            [[ -n $old_tunnel ]] || continue
            grep -qxF "$old_idx $old_tunnel" "$slots_tmp" && continue
            _tunnel_slot_release "$old_idx" "$old_tunnel"
        done < "$prev_tmp"
        cp -f "$slots_tmp" "$TUN_DIR_TABLES"
    fi

    # 4. The hash is what makes the next apply take the up-to-date branch, so it
    # is recorded only when every configured tunnel was applied, every client
    # rule went in and the chain took over. A tunnel the platform does not list
    # is not a configuration state: on Keenetic platform_tunnels answers only
    # "main" while RCI does not reply, and the netfilter.d hook fires exactly
    # during an NDM rebuild, when it may well not. Recording the hash there would
    # send every later apply down the up-to-date branch and never restore the
    # routing until the next rebuild - a silent fail-open. On Merlin the only
    # case is a typo in the tunnel id, which then warns on every apply instead
    # of once. A refused chain rule is the same: the up-to-date branch looks for
    # the MARK rules alone, so a refused exclusion or offload rule would stay
    # missing, and only a rebuild retries it.
    if [[ $skipped_unknown -eq 0 && $incomplete -eq 0 && $jumps_ok -eq 1 ]]; then
        printf '%s\n' "$new_hash" > "$TUN_DIR_HASH"
    elif [[ $skipped_unknown -ne 0 ]]; then
        log -l WARN "Tunnel Director: a configured tunnel is unknown to the platform (RCI down, or a typo in the id); this apply is not recorded as up-to-date and the next apply retries"
    elif [[ $jumps_ok -eq 0 ]]; then
        log -l WARN "Tunnel Director: the rebuild did not finish; this apply is not recorded as up-to-date and the next apply finishes it"
    else
        log -l WARN "Tunnel Director: a client rule did not go in; this apply is not recorded as up-to-date and the next apply rebuilds"
    fi

    # A failover tunnel that is not ready does not fail the apply: S99 start,
    # hooks and Web UI Apply would then skip cron and report failure while the
    # fallback interface is still coming up. The watch reads
    # TUN_DIR_FAILOVER_READY instead of the apply exit status, and drops Xray
    # membership on it: every failover client has to be marked, and the jump
    # that sends LAN traffic to those marks has to be there.
    if _tunnel_failover_needed && [[ $fo_applied -eq 1 && $fo_route_ok -eq 1 && $fo_rule_ok -eq 1 \
        && $fo_carried -eq 1 && $jumps_ok -eq 1 ]]; then
        printf '%s\n' "$XRAY_FAILOVER_TUNNEL" > "$TUN_DIR_FAILOVER_READY"
    else
        rm -f "$TUN_DIR_FAILOVER_READY"
        if _tunnel_failover_needed; then
            log -l WARN "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
        fi
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
