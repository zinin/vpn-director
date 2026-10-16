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

# Where the last apply recorded its interface tunnels, "<iface> <table>" per line, and the marker
# of the cron job that puts their routes back (vpn-director.sh, _routes_cron_sync). Beside
# TUN_DIR_TABLES (lib/ipset.sh), named here because "ensure-routes" reads them every minute
# without loading ipset.sh or the config.
IFTUNNEL_STATE_FILE="${IFTUNNEL_STATE_FILE:-/tmp/tunnel_director/iftunnels}"
IFTUNNEL_CRON_MARKER="${IFTUNNEL_CRON_MARKER:-/tmp/tunnel_director/routes_cron}"

# Set by iftunnel_init: the raw list it checked, the names it kept (space-separated) and one
# "<name><TAB><reason>" line for each name it rejected as no interface name, a LAN interface or
# the WAN. A name rejected as main or as a tunnel the platform lists gets no line: the platform
# answers for both. The check of a non-empty list keeps the platform's answer for vpd_tunnels:
# _iftunnel_platform_asked 1, what platform_tunnels printed and its exit status.
_iftunnel_checked=""
_iftunnel_checked_once=0
_iftunnel_valid=""
_iftunnel_rejected=""
_iftunnel_platform_asked=0
_iftunnel_platform_list=""
_iftunnel_platform_rc=0

# _iftunnel_warn <message> - a WARN where log exists; configure.sh has no common.sh.
_iftunnel_warn() {
    if declare -F log >/dev/null 2>&1; then
        log -l WARN "$1"
    fi
}

# iftunnel_name_ok <name> - would the kernel take <name> as an interface name: 1-15 characters of
# A-Za-z0-9_.-, not "." or "..". The rule iftunnel_init checks the list by.
iftunnel_name_ok() {
    local name="${1:-}" name_re='^[A-Za-z0-9_.-]{1,15}$'
    [[ $name =~ $name_re && $name != . && $name != .. ]]
}

# -------------------------------------------------------------------------------------------------
# iftunnel_init - check TUN_DIR_INTERFACES once per value
# -------------------------------------------------------------------------------------------------
# A name is kept when the kernel would take it (1-15 characters of A-Za-z0-9_.-, not "." or "..")
# and nothing else claims it: not main, not a tunnel the platform lists, not a LAN interface, not
# the WAN (checked only when the platform names one). A rejected name logs one WARN and is no
# interface tunnel: a tunnel key that names main or a tunnel the platform lists stays that, and
# any other is one the platform does not know. A name listed twice counts once, a rejected one
# too: one WARN, one reason. Call it in the shell that goes on to use the list - tunnel.sh does,
# in _tunnel_init - so the WARNs come once and every $(iftunnel_*) or $(vpd_*) after it finds the
# list checked.
# -------------------------------------------------------------------------------------------------
iftunnel_init() {
    local raw="${TUN_DIR_INTERFACES:-}" name reason record platform_ids lan wan seen=" "
    local -a names=()
    if [[ $_iftunnel_checked_once -eq 1 && $_iftunnel_checked == "$raw" ]]; then
        return 0
    fi
    _iftunnel_checked="$raw"
    _iftunnel_checked_once=1
    _iftunnel_valid=""
    _iftunnel_rejected=""
    _iftunnel_platform_asked=0
    [[ -n $raw ]] || return 0

    # Asked once per list, and kept with its status for vpd_tunnels. A platform that cannot
    # answer rejects nothing on its count.
    _iftunnel_platform_rc=0
    _iftunnel_platform_list="$(platform_tunnels 2>/dev/null)" || _iftunnel_platform_rc=$?
    _iftunnel_platform_asked=1
    platform_ids=" $(printf '%s\n' "$_iftunnel_platform_list" | xargs || true) "
    lan=" $(platform_lan_ifaces 2>/dev/null | xargs || true) "
    wan="$(platform_wan_if 2>/dev/null)" || wan=""
    read -ra names <<< "$raw"
    for name in "${names[@]}"; do
        # A repeat is skipped before the reasons, or a rejected name would warn again.
        [[ $seen != *" $name "* ]] || continue
        seen+="$name "
        reason=""
        # main and a tunnel the platform lists stay the platform's: they warn, and their reason
        # is not kept, so the platform's own answer stands for them (vpd_tunnel_unlisted_reason).
        record=1
        if ! iftunnel_name_ok "$name"; then
            reason="'$name' is no interface name (1-15 characters: letters, digits, _ . -)"
        elif [[ $name == main ]]; then
            reason="'main' is Tunnel Director's own route, not an interface"
            record=0
        elif [[ $platform_ids == *" $name "* ]]; then
            reason="'$name' is a tunnel the firmware manages"
            record=0
        elif [[ $lan == *" $name "* ]]; then
            reason="'$name' is a LAN interface"
        elif [[ -n $wan && $name == "$wan" ]]; then
            reason="'$name' is the WAN interface"
        fi
        if [[ -n $reason ]]; then
            if [[ $record -eq 1 ]]; then
                _iftunnel_rejected+="$name"$'\t'"$reason"$'\n'
            fi
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

# iftunnel_rejected_reason <id> - why the list's <id> was rejected; rc 1 for a kept name, for one
# the list does not hold, and for one rejected as main or as a tunnel the platform lists. For the
# warning tunnel.sh logs about a tunnel the platform does not know.
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

# iftunnel_rejections - the reason of every name iftunnel_init recorded, one per line, in list
# order; nothing, rc 0, when there is none. For the wizard, which has no log and shows them
# itself. A name rejected as main or as a tunnel the platform lists is not recorded and so not
# printed: the menu offers those as the platform's tunnels anyway.
iftunnel_rejections() {
    local name reason
    iftunnel_init
    while IFS=$'\t' read -r name reason; do
        [[ -n $name ]] || continue
        printf '%s\n' "$reason"
    done <<< "$_iftunnel_rejected"
    return 0
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
    # "dev" first: ip would read a name such as up, type or group as one of its keywords.
    out="$(ip -o link show dev "${1:-}" 2>/dev/null)" || return 1
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

###################################################################################################
# Route, NAT and MSS of an interface tunnel
###################################################################################################

# _iftunnel_default_route <table> - the table's default route, the listing read whole; rc 1 when
# there is none.
_iftunnel_default_route() {
    local routes line
    routes="$(ip route show table "${1:-}" 2>/dev/null)" || return 1
    while IFS= read -r line; do
        if [[ $line == default* ]]; then
            printf '%s\n' "$line"
            return 0
        fi
    done <<< "$routes"
    return 1
}

# _iftunnel_mss_pattern <iface> - the ERE that finds the inbound --set-mss rule whatever its MSS,
# against "iptables -S" lines. The dots of the name escaped, or "wg0.5" would take "wg0x5"'s rule;
# the space after the name keeps "awg1" off "awg10"'s.
_iftunnel_mss_pattern() {
    printf -- '-i %s -p tcp .*-j TCPMSS --set-mss ' "${1//./\\.}"
}

# _iftunnel_rules_ensure <iface> - MASQUERADE out of the interface and the outbound MSS clamp, at
# the head of their chains, where no firmware rule takes the packet first. Neither needs the
# interface: iptables matches its name, so both go in before it exists and outlive its
# re-creation. A rule that does not go in logs its own ERROR (ensure_fw_rule); the route does not
# depend on it.
_iftunnel_rules_ensure() {
    ensure_fw_rule -q nat POSTROUTING -I 1 -o "$1" -j MASQUERADE || true
    ensure_fw_rule -q mangle FORWARD -I 1 -o "$1" -p tcp -m tcp --tcp-flags SYN,RST SYN \
        -j TCPMSS --clamp-mss-to-pmtu || true
}

# _iftunnel_mss_in_ensure <iface> <mtu> - the inbound SYN-ACK clamp, MTU - 40: there
# --clamp-mss-to-pmtu would read the LAN's MTU. One such rule per interface; another MSS replaces
# it. Not sync_fw_rule: its position check would re-insert the rule whenever another went in
# above it.
_iftunnel_mss_in_ensure() {
    local iface="$1" mss=$(( $2 - 40 )) pattern have
    local -a want=(-i "$iface" -p tcp -m tcp --tcp-flags "SYN,RST" SYN -j TCPMSS --set-mss "$mss")
    pattern="$(_iftunnel_mss_pattern "$iface")"
    have="$(find_fw_rules "mangle FORWARD" "$pattern")"
    [[ $have != "-A FORWARD ${want[*]}" ]] || return 0
    [[ -z $have ]] || purge_fw_rules -q "mangle FORWARD" "$pattern"
    ensure_fw_rule -q mangle FORWARD -I 1 "${want[@]}" || true
}

# _iftunnel_rules_release <iface> - the three rules gone. Removing a rule that is not there is no
# error: a release can come for a slot that was never ensured.
_iftunnel_rules_release() {
    ensure_fw_rule -q nat POSTROUTING -D -o "$1" -j MASQUERADE || true
    ensure_fw_rule -q mangle FORWARD -D -o "$1" -p tcp -m tcp --tcp-flags SYN,RST SYN \
        -j TCPMSS --clamp-mss-to-pmtu || true
    purge_fw_rules -q "mangle FORWARD" "$(_iftunnel_mss_pattern "$1")" || true
}

# -------------------------------------------------------------------------------------------------
# iftunnel_route_ensure <iface> <idx> - the NAT and MSS rules, then the default route of its table
# -------------------------------------------------------------------------------------------------
# Idempotent: tunnel.sh calls it for every applied interface tunnel on every apply, so a firewall
# rebuild of the firmware gets the rules back with the next apply, and a re-created interface gets
# its route back once an apply comes (ensure-routes brings one within a minute). Returns 1 when
# the interface is missing or not UP - "ip link del" takes the route along, and the kernel refuses
# one through a down interface - or when the route does not go in.
# -------------------------------------------------------------------------------------------------
iftunnel_route_ensure() {
    local iface="${1:-}" table link mtu
    # No name, no rule: an older libxtables reads "-o ''" as any interface, and the first rule
    # below would be a bare MASQUERADE at the head of nat POSTROUTING.
    [[ -n $iface ]] || return 1
    table="$(iftunnel_table "${2:-}")" || return 1
    _iftunnel_rules_ensure "$iface"
    link="$(_iftunnel_link "$iface")" || return 1
    _iftunnel_up "$link" || return 1
    if mtu="$(_iftunnel_mtu "$link")"; then
        _iftunnel_mss_in_ensure "$iface" "$mtu"
    fi
    ip route replace default dev "$iface" table "$table" 2>/dev/null
}

# iftunnel_table_release <iface> <idx> - the route and the three rules gone. A table that holds
# nothing is no error: tunnel.sh releases slots that were never ensured. No name releases nothing:
# an older libxtables reads "-o ''" as any interface, and the delete could take someone else's
# bare MASQUERADE.
iftunnel_table_release() {
    local table
    [[ -n ${1:-} ]] || return 1
    table="$(iftunnel_table "${2:-}")" || return 1
    ip route flush table "$table" 2>/dev/null || true
    _iftunnel_rules_release "${1:-}"
    return 0
}

###################################################################################################
# The record of the applied interface tunnels
###################################################################################################

# iftunnel_state_write <tables-file> - record the interface tunnels among the "<idx> <id>" lines of
# TUN_DIR_TABLES: "<iface> <table>" per line in IFTUNNEL_STATE_FILE, through a file beside it and
# a rename. No interface tunnel, no file. "ensure-routes" reads it without the config, and the
# layer reads it to know an interface the list no longer names - so an apply writes it after its
# releases.
iftunnel_state_write() {
    local idx id lines=""
    if [[ -f ${1:-} ]]; then
        while read -r idx id _; do
            [[ -n $id ]] || continue
            iftunnel_is "$id" || continue
            lines+="$id $(iftunnel_table "$idx")"$'\n'
        done < "$1"
    fi
    if [[ -z $lines ]]; then
        rm -f "$IFTUNNEL_STATE_FILE"
        return 0
    fi
    mkdir -p "$(dirname "$IFTUNNEL_STATE_FILE")" &&
        printf '%s' "$lines" > "$IFTUNNEL_STATE_FILE.new" &&
        mv -f "$IFTUNNEL_STATE_FILE.new" "$IFTUNNEL_STATE_FILE"
}

# iftunnel_recorded <iface> - the table the last apply recorded for an interface tunnel; rc 1 when
# it recorded none.
iftunnel_recorded() {
    local iface table
    [[ -n ${1:-} && -f $IFTUNNEL_STATE_FILE ]] || return 1
    while read -r iface table _; do
        if [[ $iface == "$1" && -n $table ]]; then
            printf '%s\n' "$table"
            return 0
        fi
    done < "$IFTUNNEL_STATE_FILE"
    return 1
}

# iftunnel_routes_missing - the recorded interface tunnels that are UP with no default in their
# table, one per line: interfaces re-created since the last apply. An interface that is not UP yet
# is left for a later look - a route through it would be refused. rc 1 when there is none.
iftunnel_routes_missing() {
    local iface table link found=1
    [[ -s $IFTUNNEL_STATE_FILE ]] || return 1
    while read -r iface table _; do
        [[ -n $iface && -n $table ]] || continue
        link="$(_iftunnel_link "$iface")" || continue
        _iftunnel_up "$link" || continue
        ! _iftunnel_default_route "$table" >/dev/null || continue
        printf '%s\n' "$iface"
        found=0
    done < "$IFTUNNEL_STATE_FILE"
    return "$found"
}

# iftunnel_status - one line per recorded interface tunnel: its table, its route, its rules
iftunnel_status() {
    local iface table route nat mss_out mss_in
    if [[ ! -s $IFTUNNEL_STATE_FILE ]]; then
        printf '%s\n' "No interface tunnels applied."
        return 0
    fi
    while read -r iface table _; do
        [[ -n $iface && -n $table ]] || continue
        route="$(_iftunnel_default_route "$table")" || route="none"
        nat=no
        mss_out=no
        mss_in=no
        if iptables -t nat -C POSTROUTING -o "$iface" -j MASQUERADE 2>/dev/null; then
            nat=yes
        fi
        if iptables -t mangle -C FORWARD -o "$iface" -p tcp -m tcp --tcp-flags SYN,RST SYN \
            -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
            mss_out=yes
        fi
        if [[ -n $(find_fw_rules "mangle FORWARD" "$(_iftunnel_mss_pattern "$iface")") ]]; then
            mss_in=yes
        fi
        printf '%s: table %s, route %s, NAT %s, MSS out %s, MSS in %s\n' \
            "$iface" "$table" "$route" "$nat" "$mss_out" "$mss_in"
    done < "$IFTUNNEL_STATE_FILE"
}

###################################################################################################
# vpd_* - the tunnels of Tunnel Director: the firmware's and the interfaces
# -------------------------------------------------------------------------------------------------
# The core calls these, never platform_tunnel* directly: an interface of tunnel_director.interfaces
# goes to iftunnel_*, any other id to the platform contract (lib/platform.sh), with the same
# arguments and the same answers. The contract keeps describing the firmware alone.
###################################################################################################

# vpd_tunnels - the platform's tunnels without main, the interfaces, main last. rc 1 when the
# platform's inventory failed, as platform_tunnels' own: cmd_platform refuses an incomplete list.
# The platform's answer and its status are the ones iftunnel_init kept when it checked a
# non-empty list: one RCI round-trip a run on KeeneticOS, and the check and the list read the
# same answer. A process with an empty list asks the platform here, as before.
vpd_tunnels() {
    local list rc=0 id
    iftunnel_init
    if [[ $_iftunnel_platform_asked -eq 1 ]]; then
        list="$_iftunnel_platform_list"
        rc="$_iftunnel_platform_rc"
    else
        list="$(platform_tunnels)" || rc=$?
    fi
    while IFS= read -r id; do
        [[ -n $id && $id != main ]] || continue
        printf '%s\n' "$id"
    done <<< "$list"
    iftunnel_list
    printf '%s\n' main
    return "$rc"
}

vpd_tunnel_iface() {
    if iftunnel_is "${1:-}"; then
        iftunnel_iface "$1"
    else
        platform_tunnel_iface "$@"
    fi
}

vpd_tunnel_info() {
    if iftunnel_is "${1:-}"; then
        iftunnel_info "$1"
    else
        platform_tunnel_info "$@"
    fi
}

# vpd_tunnel_table <id> <idx> - an interface's own table. For an interface the list no longer
# names, the table the last apply recorded for it: its slot still holds a rule into that table
# until an apply drops it, and the drop has to find the rule.
vpd_tunnel_table() {
    local recorded
    if iftunnel_is "${1:-}"; then
        iftunnel_table "${2:-}"
    elif recorded="$(iftunnel_recorded "${1:-}")"; then
        printf '%s\n' "$recorded"
    else
        platform_tunnel_table "$@"
    fi
}

vpd_tunnel_route_ensure() {
    if iftunnel_is "${1:-}"; then
        iftunnel_route_ensure "$1" "${2:-}"
    else
        platform_tunnel_route_ensure "$@"
    fi
}

# vpd_tunnel_table_release <id> <idx> - an interface the list no longer names is released as an
# interface too: its table, its NAT and MSS rules.
vpd_tunnel_table_release() {
    if iftunnel_is "${1:-}" || iftunnel_recorded "${1:-}" >/dev/null; then
        iftunnel_table_release "$1" "${2:-}"
    else
        platform_tunnel_table_release "$@"
    fi
}

# vpd_tunnel_slot_table <id> <idx> - the table a lookup rule of the slot can point to: the
# layer's, or, when it names none, the slot's own interface table (iftunnel_table). A slot whose
# id the layer cannot name had a lookup rule only as an interface tunnel whose record is gone - an
# interrupted first apply, a failed write, a removed file - and that rule points to this table.
vpd_tunnel_slot_table() {
    vpd_tunnel_table "$@" || iftunnel_table "${2:-}"
}

# vpd_tunnel_slot_release <id> <idx> - the release of the slot's table: the layer's when it names
# one; otherwise, for a well-formed interface name, an interface's (iftunnel_table_release: the
# table 2000 + idx flushed, that name's MASQUERADE and MSS rules gone), for the same interface
# whose record is gone; otherwise nothing. Only a well-formed name goes into the NAT and MSS
# deletes: a key's regex characters would otherwise reach the MSS pattern. Always rc 0.
vpd_tunnel_slot_release() {
    if vpd_tunnel_table "${1:-}" "${2:-}" >/dev/null; then
        vpd_tunnel_table_release "$1" "${2:-}" || true
    elif iftunnel_name_ok "${1:-}"; then
        iftunnel_table_release "$1" "${2:-}" || true
    fi
    return 0
}

# vpd_tunnel_unlisted_reason <id> - why the list rejected a name, else the platform's reason; for
# main and a tunnel the platform lists, named by the list or not, the platform's: nothing, rc 1.
vpd_tunnel_unlisted_reason() {
    iftunnel_rejected_reason "${1:-}" || platform_tunnel_unlisted_reason "$@"
}

# vpd_tunnel_retired <id> - an interface the last apply routed and the list no longer names
vpd_tunnel_retired() {
    ! iftunnel_is "${1:-}" && iftunnel_recorded "${1:-}" >/dev/null
}
