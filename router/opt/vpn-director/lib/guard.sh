#!/usr/bin/env bash

###################################################################################################
# guard.sh - ip rule guards that outlive the firmware's firewall rebuilds
# -------------------------------------------------------------------------------------------------
# Purpose:
#   What keeps a protected client off the WAN lives in two mangle chains, TUN_DIR and XRAY_TPROXY,
#   and the firmware's own firewall rebuilds take both away: KeeneticOS's NDM deletes them (every
#   WAN lease renewal did on the KN-4521), Asuswrt-Merlin's "iptables -t mangle -F" on every
#   firewall start empties them. ip rules survive both. Until the hook's apply puts the chains
#   back, a protected client's packets carry no mark, and its guards - ip rules by source address
#   and LAN interface, behind the fwmark rules - route them instead: into its tunnel, or nowhere.
#   A packet a chain marked never reaches a guard.
#
#   Both chains set the direct mark on what they let go direct, and the direct rule - "fwmark
#   <mark>/<mark> lookup main", right after Tunnel Director's block range - sends that to main
#   ahead of every guard. Both modules need it, so it lives here, and it stays while either has a
#   chain or a guard (guard_direct_release). Its preference is this layer's: whatever else sits
#   there - the rule of an earlier direct_mark among them - goes once the rule is in.
#
#   Preferences, with field_max = mark_mask >> mark_shift (255 by default): the direct rule at
#   pref_base + 2 * field_max (16894), Tunnel Director's guards at the next two (16895, 16896),
#   Xray's at the two after them (16897, 16898). An owner's guards sit at one of its two
#   preferences, and while a changed list goes in at one, the list at the other goes on guarding
#   (guard_sync).
#
# Dependencies:
#   - common.sh (log, rt_table_label)
#   - firewall.sh (fw_chain_exists)
#   - config.sh (TUN_DIR_PREF_BASE, TUN_DIR_MARK_MASK, TUN_DIR_MARK_SHIFT, TUN_DIR_DIRECT_MARK,
#                TUN_DIR_CHAIN; XRAY_CHAIN and XRAY_FWMARK_MASK). Read when a function runs, never
#                when this file is sourced. Each TUN_DIR_* setting has its default here, so a test
#                that skips config.sh can call everything; an XRAY_* one that is unset or empty is
#                no Xray to look at.
#
# Public API:
#   guard_direct_pref          - the preference of the direct rule
#   guard_owner_prefs <owner>  - the two preferences of an owner's guards (tunnel, xray)
#   guard_direct_mark_hex      - the direct mark as ip prints it
#   guard_layout_check         - refuse a direct mark or guard preferences that cannot work
#   guard_direct_ensure        - the direct rule, put in when it is not listed
#   guard_direct_release       - remove the direct rule once neither module needs it
#   guard_sync <owner> <lines> - make an owner's guards exactly <lines>, make before break
#   guard_clear <owner>        - remove an owner's guards
#   guard_list <owner>         - print an owner's guards as "ip rule show" lists them
#
# Internal functions (for testing):
#   _guard_direct_mark()       - the direct mark as a number; rc 1 for a value that is none
#   _guard_norm()              - an ip rule's selectors as they compare
#   _guard_listed()            - the rules a listing holds at one preference, as they compare
#   _guard_in_order()          - are the rules of one list rules of another, in its order?
#   _guard_pref_clear()        - delete the rules a listing holds at one preference
###################################################################################################

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
# Internal helper functions
###################################################################################################

# _guard_direct_mark - the direct mark as a number. rc 1, printing nothing, for a value that is
# none: 0x and 1-8 hex digits, or a decimal without a leading zero, which bash would read as
# octal. Bash arithmetic on anything else ends the caller with an error of its own, not an ERROR.
_guard_direct_mark() {
    local raw="${TUN_DIR_DIRECT_MARK:-0x200}"
    [[ $raw =~ ^(0[xX][0-9a-fA-F]{1,8}|[1-9][0-9]{0,9})$ ]] || return 1
    printf '%d' $((raw))
}

# _guard_norm <var> <selectors> - set <var> to an ip rule's selectors as they compare: split at
# tabs and runs of spaces, ends trimmed (iproute2 4.4 ends each rule with a space), "/32" dropped
# after from and to (the kernel keeps a host, and iproute2 prints it without a length), a lookup
# table as rt_table_label prints it (table 111 reads back as "lookup wgc1" on Asuswrt-Merlin).
# Only a number goes to rt_table_label: it prints a name as given, and each lookup is a process.
# The locals carry a _gn_ prefix, so none of them can stand in for the caller's <var>.
_guard_norm() {
    local -a _gn_words=()
    local _gn_w _gn_prev="" _gn_out=""
    read -ra _gn_words <<< "$2"
    for _gn_w in ${_gn_words[@]+"${_gn_words[@]}"}; do
        case "$_gn_prev" in
            from|to) _gn_w=${_gn_w%/32} ;;
            lookup)  [[ ! $_gn_w =~ ^[0-9]+$ ]] || _gn_w=$(rt_table_label "$_gn_w") ;;
        esac
        _gn_out+="${_gn_out:+ }$_gn_w"
        _gn_prev=$_gn_w
    done
    printf -v "$1" '%s' "$_gn_out"
}

# _guard_listed <var> <pref> <rules> - set <var> to the rules the listing <rules> holds at <pref>,
# each as _guard_norm writes it, one per line in the order listed: the order the kernel tries them
# in. Empty when there are none.
_guard_listed() {
    local _gl_line _gl_norm _gl_out=""
    while IFS= read -r _gl_line; do
        [[ $_gl_line == "$2:"* ]] || continue
        _guard_norm _gl_norm "${_gl_line#*:}"
        _gl_out+="$_gl_norm"$'\n'
    done <<< "$3"
    printf -v "$1" '%s' "$_gl_out"
}

# _guard_in_order <part> <whole> - is every line of <part> a line of <whole>, in the order of
# <whole>, other lines of <whole> allowed between them? Both are rules one per line, as
# _guard_listed writes them. An empty <part> is.
_guard_in_order() {
    local -a part=() whole=()
    local line i=0
    [[ -z $1 ]] || mapfile -t part <<< "${1%$'\n'}"
    [[ -z $2 ]] || mapfile -t whole <<< "${2%$'\n'}"
    for line in ${whole[@]+"${whole[@]}"}; do
        (( i < ${#part[@]} )) || break
        if [[ $line == "${part[i]}" ]]; then
            i=$((i + 1))
        fi
    done
    (( i == ${#part[@]} ))
}

# _guard_pref_clear <pref> <rules> [<keep>] - delete every rule the listing <rules> holds at <pref>
# but one that reads <keep> (as _guard_norm writes it), one by one, each by its full selector as
# listed: a delete takes the rule it names or none, where "ip rule del pref <pref>" alone takes
# whichever rule is first there by then. One that does not go is logged and costs nothing else.
_guard_pref_clear() {
    local line norm
    local -a sel=()
    while IFS= read -r line; do
        [[ $line == "$1:"* ]] || continue
        if [[ -n ${3:-} ]]; then
            _guard_norm norm "${line#*:}"
            [[ $norm != "$3" ]] || continue
        fi
        read -ra sel <<< "${line#*:}"
        [[ ${#sel[@]} -gt 0 ]] || continue
        if ! ip rule del pref "$1" "${sel[@]}" 2>/dev/null; then
            log -l WARN "The ip rule at pref $1 did not go: ${sel[*]}"
        fi
    done <<< "$2"
}

###################################################################################################
# Public API
###################################################################################################

# -------------------------------------------------------------------------------------------------
# guard_direct_pref - the preference of the direct rule
# -------------------------------------------------------------------------------------------------
# Right after Tunnel Director's block range, which ends at pref_base + 2 * field_max - 1: 16894
# with the defaults.
# -------------------------------------------------------------------------------------------------
guard_direct_pref() {
    local field_max=$(( ${TUN_DIR_MARK_MASK:-0x00ff0000} >> ${TUN_DIR_MARK_SHIFT:-16} ))
    printf '%d\n' $(( ${TUN_DIR_PREF_BASE:-16384} + 2 * field_max ))
}

# -------------------------------------------------------------------------------------------------
# guard_owner_prefs <owner> - the two preferences of an owner's guards, on one line
# -------------------------------------------------------------------------------------------------
# Tunnel Director's (tunnel) right after the direct rule, Xray's (xray) after those: 16895 16896
# and 16897 16898 with the defaults. Tunnel Director's come first, so a client on a tunnel and on
# Xray stays in its tunnel while the chains are gone. rc 1 for any other owner.
# -------------------------------------------------------------------------------------------------
guard_owner_prefs() {
    local direct
    direct=$(guard_direct_pref)
    case "${1:-}" in
        tunnel) printf '%d %d\n' $((direct + 1)) $((direct + 2)) ;;
        xray)   printf '%d %d\n' $((direct + 3)) $((direct + 4)) ;;
        *)      return 1 ;;
    esac
}

# -------------------------------------------------------------------------------------------------
# guard_direct_mark_hex - the direct mark as ip prints it
# -------------------------------------------------------------------------------------------------
# Lowercase hex, 0x200 by default. rc 1 for a value that is no number (guard_layout_check refuses
# it).
# -------------------------------------------------------------------------------------------------
guard_direct_mark_hex() {
    local mark
    mark=$(_guard_direct_mark) || return 1
    printf '0x%x\n' "$mark"
}

# -------------------------------------------------------------------------------------------------
# guard_layout_check - can the direct mark and the guard preferences work?
# -------------------------------------------------------------------------------------------------
# The direct mark is one bit of the 32-bit fwmark, outside Tunnel Director's mark field and
# outside Xray's fwmark_mask: inside either, a packet marked direct would read as one marked for a
# tunnel or for Xray. Xray's mask is looked at only when it is set - Tunnel Director's tests run
# without it. The direct rule and the guards stay ahead of main's rule at 32766, as tunnel_apply
# keeps the block rules there; their preferences follow the block range, so they never overlap it.
# Returns 1 with an ERROR naming the setting or the preferences, for a caller that refuses the
# apply before it touches a rule.
# -------------------------------------------------------------------------------------------------
guard_layout_check() {
    local raw="${TUN_DIR_DIRECT_MARK:-0x200}" mark direct
    mark=$(_guard_direct_mark) || mark=0
    if (( mark == 0 || mark > 0xffffffff || (mark & (mark - 1)) != 0 )); then
        log -l ERROR "advanced.tunnel_director.direct_mark '$raw' is not a single bit of the fwmark (such as 0x200); rules not applied"
        return 1
    fi
    if (( mark & ${TUN_DIR_MARK_MASK:-0x00ff0000} )); then
        log -l ERROR "advanced.tunnel_director.direct_mark $(guard_direct_mark_hex) lies in Tunnel Director's mark field (mark_mask ${TUN_DIR_MARK_MASK:-0x00ff0000}); rules not applied"
        return 1
    fi
    if [[ -n ${XRAY_FWMARK_MASK:-} ]] && (( mark & XRAY_FWMARK_MASK )); then
        log -l ERROR "advanced.tunnel_director.direct_mark $(guard_direct_mark_hex) is a bit of Xray's fwmark_mask ($XRAY_FWMARK_MASK); rules not applied"
        return 1
    fi
    direct=$(guard_direct_pref)
    if (( direct + 4 >= 32766 )); then
        log -l ERROR "Guards: the direct rule and the guards would take preferences $direct-$((direct + 4)) and reach main's rule (32766) with pref_base ${TUN_DIR_PREF_BASE:-16384}; rules not applied"
        return 1
    fi
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_direct_ensure - the direct rule, alone at its preference
# -------------------------------------------------------------------------------------------------
# "<pref>: from all fwmark <mark>/<mark> lookup main", put in when it is not listed and left alone
# when it is: deleting and re-adding it is a window in which what the chains mark direct meets the
# guards - an Xray client's excluded traffic refused, a tunnel client's taken into its tunnel. Once
# it is in, every other rule at its preference goes, each by its full selector: the preference is
# this layer's, and a module reconciles a preference it owns (shell-conventions.md, "`ip rule show`
# prints the table by name"). The rule of an earlier direct_mark would otherwise route a mark
# nothing sets any more for good, and outlive stop. Whether a rule is listed is read from the whole
# listing. Returns 1 with an ERROR when the rule did not go in, the other rules left as they are.
# -------------------------------------------------------------------------------------------------
guard_direct_ensure() {
    local pref mark rules listed want
    pref=$(guard_direct_pref)
    if ! mark=$(guard_direct_mark_hex); then
        log -l ERROR "Guards: the direct rule did not go in (pref $pref): advanced.tunnel_director.direct_mark '${TUN_DIR_DIRECT_MARK:-}' is no mark"
        return 1
    fi
    want="from all fwmark $mark/$mark lookup main"
    rules=$(ip rule show 2>/dev/null) || rules=""
    _guard_listed listed "$pref" "$rules"
    if [[ $'\n'$listed != *$'\n'"$want"$'\n'* ]]; then
        if ! ip rule add pref "$pref" fwmark "$mark/$mark" lookup main 2>/dev/null; then
            log -l ERROR "Guards: the direct rule did not go in (pref $pref, $want); what the chains mark direct meets the guards instead"
            return 1
        fi
    fi
    _guard_pref_clear "$pref" "$rules" "$want"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_direct_release - remove the direct rule once neither module needs it
# -------------------------------------------------------------------------------------------------
# Only when neither TUN_DIR_CHAIN nor XRAY_CHAIN is in mangle - either could still mark a packet
# direct - and neither owner's preferences hold a rule, which would take such a packet instead.
# Then every rule at the direct rule's preference goes, the rule of an earlier direct_mark too.
# Each module's stop calls it once its own chain and guards are gone, so the one that stops last
# removes it. A listing that fails says nothing about the guards, and the rule stays. Returns 0.
# -------------------------------------------------------------------------------------------------
guard_direct_release() {
    local rules p
    local -a prefs=()
    fw_chain_exists mangle "${TUN_DIR_CHAIN:-TUN_DIR}" && return 0
    if [[ -n ${XRAY_CHAIN:-} ]] && fw_chain_exists mangle "$XRAY_CHAIN"; then
        return 0
    fi
    rules=$(ip rule show 2>/dev/null) || return 0
    read -ra prefs <<< "$(guard_owner_prefs tunnel) $(guard_owner_prefs xray)"
    for p in "${prefs[@]}"; do
        [[ $'\n'$rules != *$'\n'"$p:"* ]] || return 0
    done
    _guard_pref_clear "$(guard_direct_pref)" "$rules"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_sync <owner> <lines> - make an owner's guards exactly <lines>, make before break
# -------------------------------------------------------------------------------------------------
# <lines>: ip rule selectors, one per line, in the order they are to match - such as
# "from 192.168.1.5 iif br0 lookup wgc1". Afterwards one of the owner's two preferences holds them
# in that order and the other holds nothing. The kernel tries the rules of one preference in the
# order they went in, and puts a new one after them, so the list is written at one preference while
# the other goes on guarding. Which one is decided by what each holds:
#
#   1. One that holds exactly <lines> keeps them, and the other is cleared: an unchanged list adds
#      and deletes nothing.
#   2. One whose rules are the start of <lines> - none at all is - gets the rest appended and loses
#      nothing: a client added at the end, or the rest of a list an apply killed half-way. Of two
#      such, the one that holds more, the first on a tie.
#   3. Otherwise <lines> is written anew at the one whose rules are lines of it in its order - the
#      list an earlier sync wrote for it, a line of it refused - or, of two such or none, at the
#      one that holds fewer rules, the second on a tie: cleared rule by rule, then given every line
#      in order. The other one, the list that guarded so far, keeps guarding meanwhile; a client
#      only the cleared list guarded - one new since that other list - is without a guard from
#      its delete to its add.
#
# Only once every line is in does the other preference go. A refused line leaves it, with an ERROR
# and rc 1, and so does every later sync of that list while the refusal lasts - each finds its own
# partial list by 2 or 3 - so a client whose new guard is refused keeps its old one.
#
# The listing is read once, whole (shell-conventions.md: never "ip rule show pref N", never piped
# into "grep -q"); one that fails changes nothing and returns 1. Lines compare as _guard_norm
# writes them, so a host written x/32, or a table given by its number, reads as the kernel lists
# it. A line given twice goes in once, where it came first: the kernel refuses an identical rule
# (EEXIST), and an older kernel keeps it as a second copy.
# -------------------------------------------------------------------------------------------------
guard_sync() {
    local owner="${1:-}" lines="${2:-}" prefs p0 p1 rules want="" norm line l0 l1 n0 n1 t o
    local i=0 rc=0 pre0=0 pre1=0 sub0=0 sub1=0 first=0
    local -a add=() sel=()
    prefs=$(guard_owner_prefs "$owner") || return 1
    read -r p0 p1 <<< "$prefs"
    if ! rules=$(ip rule show 2>/dev/null); then
        log -l ERROR "Guards of $owner: cannot list the ip rules; the guards stay as they are"
        return 1
    fi
    while IFS= read -r line; do
        _guard_norm norm "$line"
        [[ -n $norm && $'\n'$want != *$'\n'"$norm"$'\n'* ]] || continue
        want+="$norm"$'\n'
        add+=("$line")
    done <<< "$lines"
    _guard_listed l0 "$p0" "$rules"
    _guard_listed l1 "$p1" "$rules"

    # 1. In place already.
    if [[ $l0 == "$want" ]]; then
        _guard_pref_clear "$p1" "$rules"
        return 0
    fi
    if [[ $l1 == "$want" ]]; then
        _guard_pref_clear "$p0" "$rules"
        return 0
    fi

    # How many rules each preference holds: a newline ends each.
    n0=${l0//[!$'\n']/} n1=${l1//[!$'\n']/}
    n0=${#n0} n1=${#n1}
    if [[ $want == "$l0"* ]]; then
        pre0=1
    fi
    if [[ $want == "$l1"* ]]; then
        pre1=1
    fi
    if (( pre0 && (!pre1 || n0 >= n1) )); then
        # 2. The rest appended where the start is: the first preference...
        t=$p0 o=$p1 i=$n0
    elif (( pre1 )); then
        # ...or the second.
        t=$p1 o=$p0 i=$n1
    else
        # 3. Written anew where a part of it is, or where fewer rules are.
        if _guard_in_order "$l0" "$want"; then
            sub0=1
        fi
        if _guard_in_order "$l1" "$want"; then
            sub1=1
        fi
        if (( sub0 != sub1 )); then
            first=$sub0
        elif (( n0 < n1 )); then
            first=1
        fi
        if (( first )); then
            t=$p0 o=$p1
        else
            t=$p1 o=$p0
        fi
        _guard_pref_clear "$t" "$rules"
    fi

    for (( ; i < ${#add[@]}; i++ )); do
        read -ra sel <<< "${add[i]}"
        if ! ip rule add pref "$t" "${sel[@]}" 2>/dev/null; then
            log -l ERROR "Guard of $owner not installed at pref $t: ${sel[*]}"
            rc=1
        fi
    done
    [[ $rc -eq 0 ]] || return 1
    _guard_pref_clear "$o" "$rules"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_clear <owner> - remove an owner's guards
# -------------------------------------------------------------------------------------------------
# Every rule at the owner's two preferences, each by its full selector (_guard_pref_clear). A rule
# at any other preference stays, the direct rule too (guard_direct_release). Returns 0.
# -------------------------------------------------------------------------------------------------
guard_clear() {
    local prefs p0 p1 rules
    prefs=$(guard_owner_prefs "${1:-}") || return 1
    read -r p0 p1 <<< "$prefs"
    if ! rules=$(ip rule show 2>/dev/null); then
        log -l WARN "Guards of $1: cannot list the ip rules; none removed"
        return 0
    fi
    _guard_pref_clear "$p0" "$rules"
    _guard_pref_clear "$p1" "$rules"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_list <owner> - print an owner's guards as "ip rule show" lists them
# -------------------------------------------------------------------------------------------------
# The rules at the owner's two preferences, in the order listed, for a status.
# -------------------------------------------------------------------------------------------------
guard_list() {
    local prefs p0 p1 rules line
    prefs=$(guard_owner_prefs "${1:-}") || return 1
    read -r p0 p1 <<< "$prefs"
    rules=$(ip rule show 2>/dev/null) || return 0
    while IFS= read -r line; do
        [[ $line == "$p0:"* || $line == "$p1:"* ]] || continue
        printf '%s\n' "$line"
    done <<< "$rules"
    return 0
}
