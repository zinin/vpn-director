#!/usr/bin/env bash

###################################################################################################
# config.sh - load vpn-director.json and export variables
###################################################################################################

# shellcheck disable=SC2034

set -euo pipefail

# -------------------------------------------------------------------------------------------------
# Debug mode: set DEBUG=1 to enable tracing
# -------------------------------------------------------------------------------------------------
if [[ ${DEBUG:-0} == 1 ]]; then
    set -x
    PS4='+${BASH_SOURCE[0]##*/}:${LINENO}:${FUNCNAME[0]:-main}: '
fi

###################################################################################################
# 1. Configuration file path (can be overridden via environment)
###################################################################################################
VPD_CONFIG_FILE="${VPD_CONFIG_FILE:-/opt/vpn-director/vpn-director.json}"

###################################################################################################
# 2. Validate config exists and is valid JSON
###################################################################################################
if [[ ! -f $VPD_CONFIG_FILE ]]; then
    echo "ERROR: Config not found: $VPD_CONFIG_FILE" >&2
    exit 1
fi

if ! jq empty "$VPD_CONFIG_FILE" 2>/dev/null; then
    echo "ERROR: Invalid JSON: $VPD_CONFIG_FILE" >&2
    exit 1
fi

###################################################################################################
# 3. Helper functions
###################################################################################################
_cfg() { jq -r "$1 // empty" "$VPD_CONFIG_FILE"; }
_cfg_arr() { jq -r "$1 // [] | .[]" "$VPD_CONFIG_FILE" | tr '\n' ' ' | sed 's/ $//'; }

# Paused clients mask: filtered from all clients arrays
_PAUSED_CLIENTS_JSON=$(jq -c '.paused_clients // []' "$VPD_CONFIG_FILE")

# Array loader that subtracts paused_clients
_cfg_arr_active() {
    jq -r --argjson p "$_PAUSED_CLIENTS_JSON" \
        "($1 // []) | if type == \"array\" then (. - \$p)[] else empty end" \
        "$VPD_CONFIG_FILE" | tr '\n' ' ' | sed 's/ $//';
}

# Dotted IPv4, octets 0-255. Same check as _tunnel_gateway; this file cannot
# call that (tunnel.sh is not sourced here) and cannot use jq regex (Entware
# jq has none).
_cfg_is_ipv4() {
    local gw="${1:-}" o
    [[ $gw =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
    for o in "${BASH_REMATCH[@]:1}"; do
        [[ $((10#$o)) -le 255 ]] || return 1
    done
    return 0
}

###################################################################################################
# 4. Tunnel Director variables
###################################################################################################
# Subtract paused_clients only from tunnels whose clients is a real array; pass
# malformed (e.g. string-valued) clients through untouched so tunnel_apply can
# validate and warn. Without the array guard, "string - $p" is a jq type error
# that, under set -e, aborts sourcing this file.
# xray.failover.tunnel is applied first: TUN_DIR is first-match, and an
# earlier covering rule (often main's LAN CIDR) would send failover clients
# out the WAN after they leave xray.clients. keys_unsorted in tunnel.sh
# keeps this order.
TUN_DIR_TUNNELS_JSON=$(jq --argjson p "$_PAUSED_CLIENTS_JSON" \
    '(.xray.failover.tunnel // "") as $fo
     | (.tunnel_director.tunnels // {})
     | to_entries
     | map(if (.value | type) == "object" and ((.value.clients // []) | type) == "array"
           then .value.clients = ((.value.clients // []) - $p)
           else . end)
     | if $fo != "" then
         ([.[] | select(.key == $fo)] + [.[] | select(.key != $fo)])
       else . end
     | from_entries' "$VPD_CONFIG_FILE")

# Spec 12: drop a tunnel.gateway that is not a dotted IPv4. Validate in bash
# (jq on Keenetic has no regex). Do not abort the load; _tunnel_gateway is
# the apply-time belt for callers that skip this file.
while IFS=$'\t' read -r _tid _gw; do
    [[ -n ${_tid:-} ]] || continue
    _cfg_is_ipv4 "${_gw:-}" && continue
    if declare -F log >/dev/null 2>&1; then
        log -l WARN "Tunnel '${_tid}': invalid gateway '${_gw}' ignored (expected an IPv4 address)"
    fi
    TUN_DIR_TUNNELS_JSON=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq --arg t "$_tid" 'del(.[$t].gateway)')
done < <(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '
    to_entries[]
    | select(.value | type == "object" and has("gateway"))
    | "\(.key)\t\(.value.gateway | tostring)"
')
unset _tid _gw

IPS_BDR_DIR=$(_cfg '.data_dir')

###################################################################################################
# 5. Xray variables
###################################################################################################
XRAY_CLIENTS=$(_cfg_arr_active '.xray.clients')
XRAY_SERVERS=$(_cfg_arr '.xray.servers')
XRAY_EXCLUDE_IPS=$(_cfg_arr '.xray.exclude_ips')
XRAY_EXCLUDE_SETS=$(_cfg_arr '.xray.exclude_sets')

###################################################################################################
# 6. Advanced: Xray
###################################################################################################
XRAY_TPROXY_PORT=$(_cfg '.advanced.xray.tproxy_port')
XRAY_ROUTE_TABLE=$(_cfg '.advanced.xray.route_table')
XRAY_RULE_PREF=$(_cfg '.advanced.xray.rule_pref')
XRAY_FWMARK=$(_cfg '.advanced.xray.fwmark')
XRAY_FWMARK_MASK=$(_cfg '.advanced.xray.fwmark_mask')
XRAY_CHAIN=$(_cfg '.advanced.xray.chain')
XRAY_CLIENTS_IPSET=$(_cfg '.advanced.xray.clients_ipset')
XRAY_BYPASS_IPSET=$(_cfg '.advanced.xray.bypass_ipset')

###################################################################################################
# 7. Advanced: Tunnel Director
###################################################################################################
TUN_DIR_CHAIN=$(_cfg '.advanced.tunnel_director.chain')
TUN_DIR_PREF_BASE=$(_cfg '.advanced.tunnel_director.pref_base')
TUN_DIR_MARK_MASK=$(_cfg '.advanced.tunnel_director.mark_mask')
TUN_DIR_MARK_SHIFT=$(_cfg '.advanced.tunnel_director.mark_shift')

###################################################################################################
# 8. Advanced: Boot
###################################################################################################
MIN_BOOT_TIME=$(_cfg '.advanced.boot.min_time')
BOOT_WAIT_DELAY=$(_cfg '.advanced.boot.wait_delay')

###################################################################################################
# 9. Make all variables read-only
###################################################################################################
readonly \
    VPD_CONFIG_FILE \
    TUN_DIR_TUNNELS_JSON IPS_BDR_DIR \
    XRAY_CLIENTS XRAY_SERVERS XRAY_EXCLUDE_IPS XRAY_EXCLUDE_SETS \
    XRAY_TPROXY_PORT XRAY_ROUTE_TABLE XRAY_RULE_PREF \
    XRAY_FWMARK XRAY_FWMARK_MASK XRAY_CHAIN \
    XRAY_CLIENTS_IPSET XRAY_BYPASS_IPSET \
    TUN_DIR_CHAIN TUN_DIR_PREF_BASE \
    TUN_DIR_MARK_MASK TUN_DIR_MARK_SHIFT \
    MIN_BOOT_TIME BOOT_WAIT_DELAY
