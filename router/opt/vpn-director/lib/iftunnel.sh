#!/usr/bin/env bash

###################################################################################################
# iftunnel.sh - interface tunnels: Linux interfaces Tunnel Director routes through
# -------------------------------------------------------------------------------------------------
# tunnel_director.interfaces names interfaces no firmware manages - the AmneziaWG awg1 an Entware
# init script brings up, say. Each is a tunnel like the firmware's own: Tunnel Director gives it a
# slot, and this module its routing table (IFTUNNEL_TABLE_BASE + idx), its default route and the
# NAT and MSS rules the firmware gives its own tunnels. The same on every platform: nothing here
# asks the firmware anything but which names are its own, its LAN and its WAN.
#
# Sourced by common.sh right after platform.sh, and by configure.sh, which sources platform.sh
# without common.sh: nothing here may need common.sh when it is sourced, and log is called only
# where it exists. The NAT and MSS rules use firewall.sh, which an apply loads.
###################################################################################################

# The first routing table of an interface tunnel: table = IFTUNNEL_TABLE_BASE + idx. Asuswrt-Merlin
# names 100, 111-120 and 200 in rt_tables; KeeneticOS's Tunnel Director tables are 2000 + idx as
# well (KEENETIC_TABLE_BASE), and a slot belongs to one tunnel, so no two tunnels share a table.
IFTUNNEL_TABLE_BASE=2000

# Set by iftunnel_init: the raw list it checked, the names it kept (space-separated) and one
# "<name><TAB><reason>" line for each name it rejected.
_iftunnel_checked=""
_iftunnel_checked_once=0
_iftunnel_valid=""
_iftunnel_rejected=""

# _iftunnel_warn <message> - a WARN where log exists; configure.sh has no common.sh.
_iftunnel_warn() {
    if declare -F log >/dev/null 2>&1; then
        log -l WARN "$1"
    fi
}

# -------------------------------------------------------------------------------------------------
# iftunnel_init - check TUN_DIR_INTERFACES once per value
# -------------------------------------------------------------------------------------------------
# A name is kept when the kernel would take it (1-15 characters of A-Za-z0-9_.-, not "." or "..")
# and nothing else claims it: not main, not a tunnel the platform lists, not a LAN interface, not
# the WAN (checked only when the platform names one). A rejected name logs one WARN and is no
# tunnel: a tunnel key that names it is one the platform does not know. A name listed twice
# counts once. Call it in the shell that goes on to use the list - tunnel.sh does, in
# _tunnel_init - so the WARNs come once and every $(iftunnel_*) or $(vpd_*) after it finds the
# list checked.
# -------------------------------------------------------------------------------------------------
iftunnel_init() {
    local raw="${TUN_DIR_INTERFACES:-}" name reason platform_ids lan wan
    local name_re='^[A-Za-z0-9_.-]{1,15}$'
    local -a names=()
    if [[ $_iftunnel_checked_once -eq 1 && $_iftunnel_checked == "$raw" ]]; then
        return 0
    fi
    _iftunnel_checked="$raw"
    _iftunnel_checked_once=1
    _iftunnel_valid=""
    _iftunnel_rejected=""
    [[ -n $raw ]] || return 0

    # Asked once per list. A platform that cannot answer rejects nothing on its count.
    platform_ids=" $(platform_tunnels 2>/dev/null | xargs || true) "
    lan=" $(platform_lan_ifaces 2>/dev/null | xargs || true) "
    wan="$(platform_wan_if 2>/dev/null)" || wan=""
    read -ra names <<< "$raw"
    for name in "${names[@]}"; do
        reason=""
        if [[ ! $name =~ $name_re || $name == . || $name == .. ]]; then
            reason="'$name' is no interface name (1-15 characters: letters, digits, _ . -)"
        elif [[ $name == main ]]; then
            reason="'main' is Tunnel Director's own route, not an interface"
        elif [[ $platform_ids == *" $name "* ]]; then
            reason="'$name' is a tunnel the firmware manages"
        elif [[ $lan == *" $name "* ]]; then
            reason="'$name' is a LAN interface"
        elif [[ -n $wan && $name == "$wan" ]]; then
            reason="'$name' is the WAN interface"
        elif [[ " $_iftunnel_valid " == *" $name "* ]]; then
            continue
        fi
        if [[ -n $reason ]]; then
            _iftunnel_rejected+="$name"$'\t'"$reason"$'\n'
            _iftunnel_warn "tunnel_director.interfaces: $reason; ignored"
            continue
        fi
        _iftunnel_valid+="${_iftunnel_valid:+ }$name"
    done
    return 0
}

# iftunnel_list - the kept names, one per line
iftunnel_list() {
    local -a valid=()
    iftunnel_init
    read -ra valid <<< "$_iftunnel_valid"
    [[ ${#valid[@]} -gt 0 ]] || return 0
    printf '%s\n' "${valid[@]}"
}

# iftunnel_is <id> - is <id> a kept interface name?
iftunnel_is() {
    [[ -n ${1:-} ]] || return 1
    iftunnel_init
    [[ " $_iftunnel_valid " == *" $1 "* ]]
}

# iftunnel_rejected_reason <id> - why the list's <id> was rejected; rc 1 for a kept name or one
# the list does not hold. For the warning tunnel.sh logs about a tunnel the platform does not know.
iftunnel_rejected_reason() {
    local name reason
    iftunnel_init
    while IFS=$'\t' read -r name reason; do
        [[ -n $name && $name == "${1:-}" ]] || continue
        printf '%s\n' "$reason"
        return 0
    done <<< "$_iftunnel_rejected"
    return 1
}

# iftunnel_table <idx> - the routing table of the interface tunnel in slot <idx>
iftunnel_table() {
    [[ ${1:-} =~ ^[0-9]+$ ]] || return 1
    printf '%d\n' $((IFTUNNEL_TABLE_BASE + 10#$1))
}

# iftunnel_iface <id> - the Linux interface of an interface tunnel: its name
iftunnel_iface() {
    iftunnel_is "${1:-}" || return 1
    printf '%s\n' "$1"
}

# _iftunnel_link <iface> - the interface's line of "ip -o link show", read whole (the pipefail
# pitfall in shell-conventions.md); rc 1 when there is none.
_iftunnel_link() {
    local out
    out="$(ip -o link show "${1:-}" 2>/dev/null)" || return 1
    [[ -n $out ]] || return 1
    printf '%s\n' "$out"
}

# _iftunnel_up <link line> - does it carry UP as a whole flag ("<...,UP,LOWER_UP>"), not the tail
# of LOWER_UP? The pattern of Merlin's platform_tunnel_info, in a variable because of its < >.
_iftunnel_up() {
    local re='<([^>]*,)?UP[,>]'
    [[ ${1:-} =~ $re ]]
}

# _iftunnel_mtu <link line> - the MTU it names; rc 1 when it names none
_iftunnel_mtu() {
    local re=' mtu ([0-9]+)'
    [[ ${1:-} =~ $re ]] || return 1
    printf '%s\n' "${BASH_REMATCH[1]}"
}

# iftunnel_info <id> - three lines, as platform_tunnel_info prints them: the type "interface",
# connected (1 while the interface exists and is UP) and an empty description.
iftunnel_info() {
    local link connected=0
    iftunnel_is "${1:-}" || return 1
    if link="$(_iftunnel_link "$1")" && _iftunnel_up "$link"; then
        connected=1
    fi
    printf '%s\n%s\n%s\n' interface "$connected" ""
}

# iftunnel_read_config <file> - tunnel_director.interfaces of a config file, space-separated, for
# the callers that load no config.sh (cmd_platform, configure.sh). The jq program is config.sh's:
# anything but an array of strings reads as no list.
iftunnel_read_config() {
    [[ -f ${1:-} ]] || return 0
    jq -r '.tunnel_director.interfaces // [] | if type == "array" then .[] | select(type == "string") else empty end' \
        "$1" 2>/dev/null | tr '\n' ' ' | sed 's/ $//' || true
}
