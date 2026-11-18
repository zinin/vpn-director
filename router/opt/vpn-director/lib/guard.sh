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
#   chain or a guard (guard_direct_release). At its preference it stands alone: once it is in, the
#   rule of an earlier direct_mark goes, and so does any guard there - a former layout's. The mark
#   is direct_mark, or without it 0x200 - the lowest bit above it outside both mark masks where
#   one of them holds 0x200 (_guard_direct_mark).
#
#   Preferences, with field_max = mark_mask >> mark_shift (255 by default): the direct rule at
#   pref_base + 2 * field_max (16894), Tunnel Director's guards at the next two (16895, 16896),
#   Xray's at the two after them (16897, 16898). An owner's guards sit at one of its two
#   preferences, and while a changed list goes in at one, the list at the other goes on guarding
#   (guard_sync).
#
#   What this layer owns at a preference is a shape (_guard_is): at a direct rule's preference,
#   "from all fwmark <m>/<m> lookup main"; at an owner's, a guard - a rule with iif and without
#   fwmark. It lists, compares and deletes nothing else: a rule of another shape there is its
#   owner's, such as a slot rule of Tunnel Director's that a change of pref_base leaves for a
#   moment on what is a guard preference now. Nothing at 32766 or above is touched: main's rule
#   and default's are there, and stop runs with whatever pref_base the configuration holds.
#
#   The preferences move with pref_base, mark_mask and mark_shift, and the rules at the former ones
#   would stay for good: after a raise past the lookup range, the direct rule would send a packet
#   that carries a tunnel's mark beside the direct bit to main, and the guards would route every
#   packet of their clients whatever its mark. So each kind - direct, tunnel, xray - has a record
#   in GUARD_STATE_DIR of the preferences its rules last went in at. Once the rules are in at the
#   current ones, those at the recorded ones go, and guard_clear and guard_direct_release take
#   both, the record with them. A pref_base moved by less than five puts a former preference of
#   one kind on a current one of another. A guard left on another owner's is that owner's to take,
#   as its own; a guard left on the direct rule's, and a direct rule left on a guard's, are never
#   live there, and go with their former owner, on stop as on an apply (_guard_release_former).
#
# Dependencies:
#   - common.sh (log, rt_table_label, ensure_owned_dir)
#   - firewall.sh (fw_chain_exists)
#   - config.sh (TUN_DIR_PREF_BASE, TUN_DIR_MARK_MASK, TUN_DIR_MARK_SHIFT, TUN_DIR_DIRECT_MARK,
#                TUN_DIR_CHAIN; XRAY_CHAIN, XRAY_FWMARK_MASK and XRAY_RULE_PREF). Read when a
#                function runs, never when this file is sourced. Each TUN_DIR_* setting has its
#                default here - TUN_DIR_DIRECT_MARK the bit _guard_direct_mark picks outside the
#                mark masks - so a test that skips config.sh can call everything; an XRAY_* one
#                that is unset or empty is no Xray to look at.
#   - GUARD_STATE_DIR: the directory of the records, /tmp/vpn-director/guards unless set. /tmp
#     goes at a reboot, as the ip rules do. It and its parent are held to watchd's OwnedDir policy
#     (ensure_owned_dir), and a record is read only when it is this user's own (_guard_record).
#
# Public API:
#   guard_direct_pref          - the preference of the direct rule
#   guard_owner_prefs <owner>  - the two preferences of an owner's guards (tunnel, xray)
#   guard_direct_mark_hex      - the direct mark as ip prints it
#   guard_layout_check         - refuse a direct mark, guard preferences or an Xray rule_pref that
#                                cannot work
#   guard_layout_moved         - has the direct rule's preference moved since the last apply, with
#                                Tunnel Director's chain in mangle?
#   guard_direct_ensure        - the direct rule, alone at its preference; a former one goes
#   guard_direct_release       - remove the direct rule once neither module needs it
#   guard_sync <owner> <lines> - make an owner's guards exactly <lines>, make before break
#   guard_clear <owner>        - remove an owner's guards, at its former preferences too
#   guard_list <owner>         - print an owner's guards as "ip rule show" lists them
#
# Internal functions (for testing):
#   _guard_direct_mark()       - the direct mark as a number, direct_mark or the bit picked without
#                                it; rc 1 for a value that is none, or no bit free
#   _guard_norm()              - an ip rule's selectors as they compare
#   _guard_is()                - is a rule of a shape this layer owns: the direct rule's, a guard?
#   _guard_client()            - the client a guard is for: its from and its iif
#   _guard_listed()            - the rules of a shape a listing holds at one preference
#   _guard_in_order()          - are the rules of one list rules of another, in its order?
#   _guard_pref_clear()        - delete the rules of a shape a listing holds at one preference
#   _guard_record()            - the preferences a kind's rules last went in at
#   _guard_record_set()        - record them
#   _guard_record_drop()       - remove a kind's record
#   _guard_release_former()    - delete what a kind left at the preferences its record names
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

# The WARN guard_layout_check last logged for a mark _guard_direct_mark picked: a full apply
# checks the layout twice, in tproxy_apply and in tunnel_apply, and the WARN comes once per value.
_guard_direct_warned=""

# _guard_direct_mark - the direct mark as a number. direct_mark when it is set: rc 1, printing
# nothing, for a value that is none - 0x and 1-8 hex digits, or a decimal without a leading zero,
# which bash would read as octal. Bash arithmetic on anything else ends the caller with an error of
# its own, not an ERROR. Without it - the template leaves it out, so it is set only where the user
# set it - 0x200, unless that bit lies in Tunnel Director's mark_mask or in Xray's fwmark_mask,
# where guard_layout_check would refuse every apply: then the lowest bit above it outside both, up
# to 0x80000000. The same masks give the same bit; rc 1 when they take every one.
_guard_direct_mark() {
    local raw="${TUN_DIR_DIRECT_MARK:-}" taken bit
    if [[ -z $raw ]]; then
        taken=$(( ${TUN_DIR_MARK_MASK:-0x00ff0000} | ${XRAY_FWMARK_MASK:-0} ))
        for (( bit = 0x200; bit <= 0x80000000; bit <<= 1 )); do
            if (( (bit & taken) == 0 )); then
                printf '%d' "$bit"
                return 0
            fi
        done
        return 1
    fi
    [[ $raw =~ ^(0[xX][0-9a-fA-F]{1,8}|[1-9][0-9]{0,9})$ ]] || return 1
    printf '%d' $((raw))
}

# _guard_norm <var> <selectors> - set <var> to an ip rule's selectors as they compare: split at
# tabs and runs of spaces, ends trimmed (iproute2 4.4 ends each rule with a space), "/32" dropped
# after from and to (the kernel keeps a host, and iproute2 prints it without a length), a prefix
# of length 0 written as the kernel lists it ("from x/0" as "from all", "to x/0" not at all), a
# lookup table as rt_table_label prints it (table 111 reads back as "lookup wgc1" on
# Asuswrt-Merlin). Only a number goes to rt_table_label: it prints a name as given, and each
# lookup is a process. The locals carry a _gn_ prefix, so none of them can stand in for the
# caller's <var>.
_guard_norm() {
    local -a _gn_words=()
    local _gn_w _gn_prev="" _gn_out=""
    read -ra _gn_words <<< "$2"
    for _gn_w in ${_gn_words[@]+"${_gn_words[@]}"}; do
        case "$_gn_prev" in
            from|to)
                _gn_w=${_gn_w%/32}
                if [[ $_gn_w == */0 && $_gn_prev == from ]]; then
                    _gn_w=all
                elif [[ $_gn_w == */0 ]]; then
                    # "to" is written already: it goes, and the address with it.
                    _gn_out=${_gn_out%to}
                    _gn_out=${_gn_out% }
                    _gn_prev=""
                    continue
                fi
                ;;
            lookup)
                [[ ! $_gn_w =~ ^[0-9]+$ ]] || _gn_w=$(rt_table_label "$_gn_w")
                ;;
        esac
        _gn_out+="${_gn_out:+ }$_gn_w"
        _gn_prev=$_gn_w
    done
    printf -v "$1" '%s' "$_gn_out"
}

# _guard_is <shape> <selectors> - is the rule of <shape>, one of the two this layer owns? direct:
# "from all fwmark <m>/<m> lookup main", value and mask equal, the shape of the direct rule. guard:
# a rule with iif and without fwmark. main's, default's and local's rules carry no iif, so
# "from all lookup main" is no guard, while the "from all iif br0 ..." of a /0 client is one.
# <selectors> as listed or as _guard_norm writes them: neither shape rests on what it changes.
_guard_is() {
    local rule direct_re='^from all fwmark (0x[0-9a-f]+)/(0x[0-9a-f]+) lookup main$'
    local -a words=()
    read -ra words <<< "$2"
    [[ ${#words[@]} -gt 0 ]] || return 1
    rule="${words[*]}"
    case "$1" in
        direct)
            [[ $rule =~ $direct_re && ${BASH_REMATCH[1]} == "${BASH_REMATCH[2]}" ]]
            ;;
        guard)
            [[ " $rule " == *" iif "* && " $rule " != *" fwmark "* ]]
            ;;
        *)
            return 1
            ;;
    esac
}

# _guard_client <var> <rule> - set <var> to the client a guard (as _guard_norm writes it) is for:
# its from and its iif, as "<from> <iif>". The locals carry a _gc_ prefix, as _guard_norm's do.
_guard_client() {
    local -a _gc_words=()
    local _gc_w _gc_prev="" _gc_from="" _gc_iif=""
    read -ra _gc_words <<< "$2"
    for _gc_w in ${_gc_words[@]+"${_gc_words[@]}"}; do
        case "$_gc_prev" in
            from) _gc_from=$_gc_w ;;
            iif)  _gc_iif=$_gc_w ;;
        esac
        _gc_prev=$_gc_w
    done
    printf -v "$1" '%s %s' "$_gc_from" "$_gc_iif"
}

# _guard_listed <var> <pref> <rules> <shape> - set <var> to the rules of <shape> (_guard_is) the
# listing <rules> holds at <pref>, each as _guard_norm writes it, one per line in the order listed:
# the order the kernel tries them in. Empty when there are none, and at a <pref> from 32766 on.
_guard_listed() {
    local _gl_line _gl_norm _gl_out=""
    if (( $2 < 32766 )); then
        while IFS= read -r _gl_line; do
            [[ $_gl_line == "$2:"* ]] || continue
            _guard_is "$4" "${_gl_line#*:}" || continue
            _guard_norm _gl_norm "${_gl_line#*:}"
            _gl_out+="$_gl_norm"$'\n'
        done <<< "$3"
    fi
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

# _guard_pref_clear <pref> <rules> <shape> [<keep>] - delete every rule of <shape> the listing
# <rules> holds at <pref> but those <keep> holds (rules one per line, as _guard_norm writes them),
# one by one, each by its full selector as listed: a delete takes the rule it names or none, where
# "ip rule del pref <pref>" alone takes whichever rule is first there by then. One that does not
# go is logged and costs nothing else. A rule of another shape stays, and nothing goes at a <pref>
# from 32766 on.
_guard_pref_clear() {
    local line norm keep=$'\n'"${4:-}"$'\n'
    local -a sel=()
    (( $1 < 32766 )) || return 0
    while IFS= read -r line; do
        [[ $line == "$1:"* ]] || continue
        _guard_is "$3" "${line#*:}" || continue
        if [[ -n ${4:-} ]]; then
            _guard_norm norm "${line#*:}"
            [[ $keep != *$'\n'"$norm"$'\n'* ]] || continue
        fi
        read -ra sel <<< "${line#*:}"
        if ! ip rule del pref "$1" "${sel[@]}" 2>/dev/null; then
            log -l WARN "The ip rule at pref $1 did not go: ${sel[*]}"
        fi
    done <<< "$2"
    return 0
}

# _guard_record <kind> - print the preferences the record of <kind> (direct, tunnel, xray) names,
# on one line: where its rules last went in. Nothing when there is no record, and nothing for one
# that is no regular file, a symlink, or another user's: the record decides at which preferences
# root deletes rules of the guard shapes, and only one of this user's own may. A word that is no
# preference is left out.
_guard_record() {
    local file="${GUARD_STATE_DIR:-/tmp/vpn-director/guards}/$1" line="" w out=""
    local -a words=()
    [[ -f $file && ! -L $file && -O $file ]] || return 0
    { IFS= read -r line < "$file"; } 2>/dev/null || true
    read -ra words <<< "$line"
    for w in ${words[@]+"${words[@]}"}; do
        [[ ! $w =~ ^[1-9][0-9]{0,9}$ ]] || out+="${out:+ }$w"
    done
    [[ -z $out ]] || printf '%s\n' "$out"
}

# _guard_record_set <kind> <prefs> - record that the rules of <kind> went in at <prefs>: through a
# file beside the record, written under umask 077, and a rename, and only when that changes it.
# The directory and its parent have to be this user's alone (ensure_owned_dir): a missing one is
# made 0755 whatever the umask, one that group or others can write to loses that write -
# /tmp/vpn-director is also watchd's runtime directory - and a symlink, no directory or another
# user's records nothing. A refusal or a write that fails is a WARN and nothing else: the next
# change of pref_base then finds no former preferences to release.
_guard_record_set() {
    local dir="${GUARD_STATE_DIR:-/tmp/vpn-director/guards}" file
    file="$dir/$1"
    [[ $(_guard_record "$1") != "$2" ]] || return 0
    if ! { ensure_owned_dir "${dir%/*}" && ensure_owned_dir "$dir"; }; then
        log -l WARN "Guards: cannot record preferences $2 in $file; after a change of pref_base their rules stay"
        return 0
    fi
    if ! { ( umask 077; printf '%s\n' "$2" > "$file.new" ) && mv -f "$file.new" "$file"; } 2>/dev/null; then
        rm -f "$file.new" 2>/dev/null || true
        log -l WARN "Guards: cannot record preferences $2 in $file; after a change of pref_base their rules stay"
    fi
    return 0
}

# _guard_record_drop <kind> - remove the record of <kind>, its rules gone.
_guard_record_drop() {
    local file="${GUARD_STATE_DIR:-/tmp/vpn-director/guards}/$1"
    [[ ! -e $file ]] || rm -f "$file" 2>/dev/null || true
}

# _guard_release_former <kind> <shape> <rules> - delete the rules of <shape> (_guard_pref_clear)
# the listing <rules> holds at each preference the record of <kind> names, but where a rule of that
# shape can be live: a guard at an owner's current preference, Tunnel Director's two or Xray's two,
# a direct rule at the direct rule's current one. The kind's own current ones it reconciles as
# such. A pref_base moved by less than five puts a former preference of one kind on a current one
# of another. Released at another owner's, a guard would go with that owner's live guards - "stop
# xray" taking Tunnel Director's - so that owner's sync takes it instead, as one of its own. A guard
# at the direct rule's preference and a direct rule at a guard's are never live there, and go with
# their former owner, on stop as on an apply.
_guard_release_former() {
    local live p
    local -a recorded=()
    read -ra recorded <<< "$(_guard_record "$1")"
    if [[ $2 == guard ]]; then
        live=" $(guard_owner_prefs tunnel) $(guard_owner_prefs xray) "
    else
        live=" $(guard_direct_pref) "
    fi
    for p in ${recorded[@]+"${recorded[@]}"}; do
        [[ $live == *" $p "* ]] || _guard_pref_clear "$p" "$3" "$2"
    done
    return 0
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
# Xray stays in its tunnel while the chains are gone - but for what Xray lets through between
# tproxy_apply and tunnel_apply of a full apply that finds both chains gone, which the direct rule
# sends to main (packet-flow.md, "Left open"). rc 1 for any other owner.
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
# Lowercase hex: direct_mark, or without it the bit _guard_direct_mark picks, 0x200 unless a mark
# mask holds that bit. rc 1 for a value that is no number, and without direct_mark when no bit is
# free (guard_layout_check refuses both).
# -------------------------------------------------------------------------------------------------
guard_direct_mark_hex() {
    local mark
    mark=$(_guard_direct_mark) || return 1
    printf '0x%x\n' "$mark"
}

# -------------------------------------------------------------------------------------------------
# guard_layout_check - can the direct mark, the guard preferences and Xray's rule_pref work?
# -------------------------------------------------------------------------------------------------
# The direct mark is one bit of the 32-bit fwmark, outside Tunnel Director's mark field and
# outside Xray's fwmark_mask: inside either, a packet marked direct would read as one marked for a
# tunnel or for Xray. Xray's mask is looked at only when it is set - Tunnel Director's tests run
# without it. A direct_mark that is set is the user's, and refused there, and in the firmware's
# mark byte (0xff) too: the mark layout gives the firmware the low byte. Without it the mark is
# the bit _guard_direct_mark picks outside both, and one that is not 0x200 moves whenever the masks
# do: a WARN names it, the mask that holds 0x200 and the setting that pins it, once per value. No
# bit left to pick is refused. The direct rule and the guards stay ahead of main's rule at 32766,
# as tunnel_apply keeps the block rules there; their preferences follow the block range, so they
# never overlap it. Xray's rule_pref, when it is set, stays below the direct rule: a packet TPROXY
# takes carries the direct mark too - --tproxy-mark changes the bits of fwmark_mask alone - and one
# that meets the direct rule first goes to main to be forwarded, which drops a packet TPROXY took,
# while the ready marker says Xray is ready. Returns 1 with an ERROR naming the setting or the
# preferences, for a caller that refuses the apply before it touches a rule.
# -------------------------------------------------------------------------------------------------
guard_layout_check() {
    local raw="${TUN_DIR_DIRECT_MARK:-}" td_mask="${TUN_DIR_MARK_MASK:-0x00ff0000}" mark hex direct
    local masks held="" warning
    if [[ -z $raw ]]; then
        if ! mark=$(_guard_direct_mark); then
            masks="Tunnel Director's mark field (mark_mask $td_mask)"
            [[ -z ${XRAY_FWMARK_MASK:-} ]] || masks+=" or Xray's fwmark_mask ($XRAY_FWMARK_MASK)"
            log -l ERROR "advanced.tunnel_director.direct_mark is not set, and every bit from 0x200 to 0x80000000 is taken by $masks; rules not applied"
            return 1
        fi
        if (( mark != 0x200 )); then
            printf -v hex '0x%x' "$mark"
            if (( 0x200 & td_mask )); then
                held="lies in Tunnel Director's mark field (mark_mask $td_mask)"
            fi
            if [[ -n ${XRAY_FWMARK_MASK:-} ]] && (( 0x200 & XRAY_FWMARK_MASK )); then
                held+="${held:+ and }is a bit of Xray's fwmark_mask ($XRAY_FWMARK_MASK)"
            fi
            warning="advanced.tunnel_director.direct_mark is not set, and 0x200 $held: the direct mark is $hex; set advanced.tunnel_director.direct_mark to \"$hex\" to pin it"
            if [[ $warning != "$_guard_direct_warned" ]]; then
                log -l WARN "$warning"
                _guard_direct_warned=$warning
            fi
        fi
    else
        mark=$(_guard_direct_mark) || mark=0
        if (( mark == 0 || mark > 0xffffffff || (mark & (mark - 1)) != 0 )); then
            log -l ERROR "advanced.tunnel_director.direct_mark '$raw' is not a single bit of the fwmark (such as 0x200); rules not applied"
            return 1
        fi
        if (( mark & 0xff )); then
            log -l ERROR "advanced.tunnel_director.direct_mark $(guard_direct_mark_hex) lies in the firmware's mark byte (0xff); rules not applied"
            return 1
        fi
        if (( mark & td_mask )); then
            log -l ERROR "advanced.tunnel_director.direct_mark $(guard_direct_mark_hex) lies in Tunnel Director's mark field (mark_mask $td_mask); rules not applied"
            return 1
        fi
        if [[ -n ${XRAY_FWMARK_MASK:-} ]] && (( mark & XRAY_FWMARK_MASK )); then
            log -l ERROR "advanced.tunnel_director.direct_mark $(guard_direct_mark_hex) is a bit of Xray's fwmark_mask ($XRAY_FWMARK_MASK); rules not applied"
            return 1
        fi
    fi
    direct=$(guard_direct_pref)
    if (( direct + 4 >= 32766 )); then
        log -l ERROR "Guards: the direct rule and the guards would take preferences $direct-$((direct + 4)) and reach main's rule (32766) with pref_base ${TUN_DIR_PREF_BASE:-16384}; rules not applied"
        return 1
    fi
    if [[ ${XRAY_RULE_PREF:-} =~ ^[0-9]{1,10}$ ]] && (( 10#$XRAY_RULE_PREF >= direct )); then
        log -l ERROR "advanced.xray.rule_pref $XRAY_RULE_PREF must be below $direct, the direct rule's preference: what TPROXY takes carries the direct mark too, and would be routed to main and dropped instead of reaching Xray; rules not applied"
        return 1
    fi
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_layout_moved - has the direct rule's preference moved since the last apply, with Tunnel
# Director's chain in mangle?
# -------------------------------------------------------------------------------------------------
# rc 0 when the direct rule's record names a preference other than guard_direct_pref - pref_base,
# mark_mask or mark_shift changed since the last apply - while TUN_DIR_CHAIN is in mangle. Tunnel
# Director's lookup and block rules then sit at the former base until a tunnel_apply moves them,
# and an apply of Xray alone, which runs none, would move the direct rule without them: after a
# lower base it lands ahead of the former lookup range, and sends a packet that carries a tunnel's
# mark beside the direct bit - what Xray lets through of a client that is on a tunnel too - to
# main. vpn-director.sh makes such an apply a full one. rc 1 without a record, with the same
# preference, or without Tunnel Director's chain: nothing of Tunnel Director's to move then. Prints
# nothing.
# -------------------------------------------------------------------------------------------------
guard_layout_moved() {
    local recorded
    recorded=$(_guard_record direct)
    [[ -n $recorded && $recorded != "$(guard_direct_pref)" ]] || return 1
    fw_chain_exists mangle "${TUN_DIR_CHAIN:-TUN_DIR}"
}

# -------------------------------------------------------------------------------------------------
# guard_direct_ensure - the direct rule, alone at its preference
# -------------------------------------------------------------------------------------------------
# "<pref>: from all fwmark <mark>/<mark> lookup main", put in when it is not listed and left alone
# when it is: deleting and re-adding it is a window in which what the chains mark direct meets the
# guards - an Xray client's excluded traffic refused, a tunnel client's taken into its tunnel. Once
# it is in, every other rule of its shape at its preference goes: the rule of an earlier
# direct_mark would otherwise route a mark nothing sets any more for good, and outlive stop. So
# does every guard there: none belongs there, so any is a former layout's - a pref_base raised by
# one or two puts the direct rule on Tunnel Director's former guards - and, tried ahead of the
# rule, it would take what the chains mark direct. Then the rules of its shape go at the
# preference the record names, when that is another one (_guard_release_former): after a raise of
# pref_base the former rule sits ahead of the new lookup range, and sends a packet that carries a
# tunnel's mark beside the direct bit to main. They go even where that preference is a guard
# preference now: no live rule there has their shape. Each goes by its full selector, a rule of
# another shape stays, and the record then names the current preference. Whether a rule is listed
# is read from the whole listing; one that fails releases nothing. Returns 1 when the rule did not
# go in - an ERROR, or a WARN for a preference from main's rule (32766) on - with nothing deleted
# and the record as it was.
# -------------------------------------------------------------------------------------------------
guard_direct_ensure() {
    local pref mark rules listed want why listed_ok=1
    pref=$(guard_direct_pref)
    if ! mark=$(guard_direct_mark_hex); then
        why="advanced.tunnel_director.direct_mark '${TUN_DIR_DIRECT_MARK:-}' is no mark"
        [[ -n ${TUN_DIR_DIRECT_MARK:-} ]] || why="no bit outside the mark masks is left for the direct mark"
        log -l ERROR "Guards: the direct rule did not go in (pref $pref): $why"
        return 1
    fi
    if (( pref >= 32766 )); then
        log -l WARN "Guards: the direct rule not put in: pref $pref reaches main's rule (32766)"
        return 1
    fi
    want="from all fwmark $mark/$mark lookup main"
    rules=$(ip rule show 2>/dev/null) || { rules=""; listed_ok=0; }
    _guard_listed listed "$pref" "$rules" direct
    if [[ $'\n'$listed != *$'\n'"$want"$'\n'* ]]; then
        if ! ip rule add pref "$pref" fwmark "$mark/$mark" lookup main 2>/dev/null; then
            log -l ERROR "Guards: the direct rule did not go in (pref $pref, $want); what the chains mark direct meets the guards instead"
            return 1
        fi
    fi
    [[ $listed_ok -eq 1 ]] || return 0
    _guard_pref_clear "$pref" "$rules" direct "$want"
    _guard_pref_clear "$pref" "$rules" guard
    _guard_release_former direct direct "$rules"
    _guard_record_set direct "$pref"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_direct_release - remove the direct rule once neither module needs it
# -------------------------------------------------------------------------------------------------
# Only when neither TUN_DIR_CHAIN nor XRAY_CHAIN is in mangle - either could still mark a packet
# direct - and no guard is listed at either owner's preferences, current or recorded, which would
# take such a packet instead. Then every rule of its shape goes, at its preference and at the one
# its record names, a guard preference now included (_guard_release_former) - the rule of an
# earlier direct_mark too, and that of a former pref_base - and the record with them. Each
# module's stop calls it once its own chain and guards are gone, so the one that stops last
# removes it. A listing that fails says nothing about the guards, and the rule stays. Returns 0.
# -------------------------------------------------------------------------------------------------
guard_direct_release() {
    local rules owners p listed
    local -a prefs=()
    fw_chain_exists mangle "${TUN_DIR_CHAIN:-TUN_DIR}" && return 0
    if [[ -n ${XRAY_CHAIN:-} ]] && fw_chain_exists mangle "$XRAY_CHAIN"; then
        return 0
    fi
    rules=$(ip rule show 2>/dev/null) || return 0
    owners="$(guard_owner_prefs tunnel) $(guard_owner_prefs xray)"
    read -ra prefs <<< "$owners $(_guard_record tunnel) $(_guard_record xray)"
    for p in "${prefs[@]}"; do
        _guard_listed listed "$p" "$rules" guard
        [[ -z $listed ]] || return 0
    done
    _guard_pref_clear "$(guard_direct_pref)" "$rules" direct
    _guard_release_former direct direct "$rules"
    _guard_record_drop direct
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_sync <owner> <lines> - make an owner's guards exactly <lines>, make before break
# -------------------------------------------------------------------------------------------------
# <lines>: ip rule selectors, one per line, in the order they are to match - such as
# "from 192.168.1.5 iif br0 lookup wgc1". Afterwards one of the owner's two preferences holds them
# in that order and the other holds no guard. The kernel tries the rules of one preference in the
# order they went in, and puts a new one after them, so the list is written at one preference while
# the other goes on guarding. Which one is decided by the guards each holds - every guard there,
# those another kind's former layout left included (_guard_release_former):
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
# Only once every line is in does the other preference go, and with it the guards at the
# preferences the record names (_guard_release_former): after a change of pref_base they would
# route every packet of their clients whatever its mark. The record then names the current two.
# A refused line leaves both, and the record, with an ERROR and rc 1, and so does every later sync
# of that list while the refusal lasts - each finds its own partial list by 2 or 3 - so a client
# whose new guard is refused keeps its old one. A client <lines> no longer names - no line with
# its from and iif - loses its guards at the other preference all the same: nothing protects it
# any more, and they would go on routing it while the refusal lasts. A preference from main's rule
# (32766) on takes no line: a WARN and rc 1.
#
# The listing is read once, whole (shell-conventions.md: never "ip rule show pref N", never piped
# into "grep -q"); one that fails changes nothing and returns 1. Lines compare as _guard_norm
# writes them, so a host written x/32, a prefix of length 0, or a table given by its number, reads
# as the kernel lists it. A line given twice goes in once, where it came first: the kernel refuses
# an identical rule (EEXIST), and an older kernel keeps it as a second copy.
# -------------------------------------------------------------------------------------------------
guard_sync() {
    local owner="${1:-}" lines="${2:-}" prefs p0 p1 rules want="" clients="" norm key line
    local l0 l1 lo n0 n1 t o keep=""
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
        _guard_client key "$norm"
        clients+="$key"$'\n'
        add+=("$line")
    done <<< "$lines"
    _guard_listed l0 "$p0" "$rules" guard
    _guard_listed l1 "$p1" "$rules" guard

    if [[ $l0 == "$want" ]]; then
        # 1. In place already: the first preference...
        t=$p0 o=$p1 lo=$l1 i=${#add[@]}
    elif [[ $l1 == "$want" ]]; then
        # ...or the second.
        t=$p1 o=$p0 lo=$l0 i=${#add[@]}
    else
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
            t=$p0 o=$p1 lo=$l1 i=$n0
        elif (( pre1 )); then
            # ...or the second.
            t=$p1 o=$p0 lo=$l0 i=$n1
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
                t=$p0 o=$p1 lo=$l1
            else
                t=$p1 o=$p0 lo=$l0
            fi
            _guard_pref_clear "$t" "$rules" guard
        fi
    fi

    if (( i < ${#add[@]} && t >= 32766 )); then
        log -l WARN "Guards of $owner not installed: pref $t reaches main's rule (32766)"
        rc=1
    else
        for (( ; i < ${#add[@]}; i++ )); do
            read -ra sel <<< "${add[i]}"
            if ! ip rule add pref "$t" "${sel[@]}" 2>/dev/null; then
                log -l ERROR "Guard of $owner not installed at pref $t: ${sel[*]}"
                rc=1
            fi
        done
    fi
    if [[ $rc -ne 0 ]]; then
        while IFS= read -r norm; do
            [[ -n $norm ]] || continue
            _guard_client key "$norm"
            [[ $'\n'$clients != *$'\n'"$key"$'\n'* ]] || keep+="$norm"$'\n'
        done <<< "$lo"
        _guard_pref_clear "$o" "$rules" guard "$keep"
        return 1
    fi
    _guard_pref_clear "$o" "$rules" guard
    _guard_release_former "$owner" guard "$rules"
    _guard_record_set "$owner" "$p0 $p1"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_clear <owner> - remove an owner's guards
# -------------------------------------------------------------------------------------------------
# Every guard at the owner's two preferences and at those its record names (_guard_release_former),
# each by its full selector (_guard_pref_clear), then the record. A rule of another shape stays, and
# so does every rule at any other preference, the direct rule too (guard_direct_release). A listing
# that fails removes nothing, the record neither. Returns 0.
# -------------------------------------------------------------------------------------------------
guard_clear() {
    local prefs p0 p1 rules
    prefs=$(guard_owner_prefs "${1:-}") || return 1
    read -r p0 p1 <<< "$prefs"
    if ! rules=$(ip rule show 2>/dev/null); then
        log -l WARN "Guards of $1: cannot list the ip rules; none removed"
        return 0
    fi
    _guard_pref_clear "$p0" "$rules" guard
    _guard_pref_clear "$p1" "$rules" guard
    _guard_release_former "$1" guard "$rules"
    _guard_record_drop "$1"
    return 0
}

# -------------------------------------------------------------------------------------------------
# guard_list <owner> - print an owner's guards as "ip rule show" lists them
# -------------------------------------------------------------------------------------------------
# The guards at the owner's two preferences, in the order listed, for a status: a rule of another
# shape there is none, and nothing from 32766 on is listed.
# -------------------------------------------------------------------------------------------------
guard_list() {
    local prefs p0 p1 rules line
    prefs=$(guard_owner_prefs "${1:-}") || return 1
    read -r p0 p1 <<< "$prefs"
    rules=$(ip rule show 2>/dev/null) || return 0
    while IFS= read -r line; do
        [[ $line == "$p0:"* || $line == "$p1:"* ]] || continue
        (( ${line%%:*} < 32766 )) || continue
        _guard_is guard "${line#*:}" || continue
        printf '%s\n' "$line"
    done <<< "$rules"
    return 0
}
