#!/usr/bin/env bash

###################################################################################################
# fwsnap.sh - the snapshot of our firewall rules that the firmware hooks restore
# -------------------------------------------------------------------------------------------------
# Purpose:
#   The firmware's firewall rebuilds take XRAY_TPROXY and TUN_DIR away: KeeneticOS's NDM deletes
#   them with their jumps, Asuswrt-Merlin's "iptables -t mangle -F" on every firewall start empties
#   every mangle chain. The guards (guard.sh) keep the protected clients off the WAN until the
#   apply the hook queues puts the chains back, but that window still costs: an Xray client's new
#   connections are refused, and a tunnel client's traffic to an excluded country goes through its
#   tunnel. To shorten it, every apply that gets to its end - in full, of one component, the
#   update, the restart - snapshots our rules here, and the hooks restore them before they queue
#   the apply (fwsnap-restore.sh).
#
#   A snapshot is iptables-restore --noflush input, a file per table:
#     mangle.rules  XRAY_TPROXY and TUN_DIR, each that exists: declared, then filled with its rules
#                   as "iptables -S" lists them; their PREROUTING jumps, inserted from the top,
#                   Xray's first; the mangle INPUT accept of Xray's mark (KeeneticOS,
#                   platform_tproxy_extra_rules) and the MSS rules of the interface tunnels
#                   (iftunnel.sh), each at the head of its chain. None while neither chain exists.
#     nat.rules     "-o <iface> -j MASQUERADE" for each interface tunnel whose rule nat POSTROUTING
#                   lists, a line each for the restore to check and insert. None without one.
#   iptables-restore --noflush flushes a declared chain that exists and fills it in the same
#   commit, which is what Merlin's emptied chains need, and creates one NDM deleted. The
#   <chain>_NEW of an interrupted swap is not ours to bring back: the next apply finishes that
#   swap or deletes the chain (swap_fw_chain).
#
#   An apply that fails part-way ends before the snapshot, and the one of an earlier apply stays:
#   the apply the hooks queue after restoring it reconciles whatever it holds that is stale. A
#   full stop removes the snapshot. A component's stop removes it before its teardown too, then
#   writes one of what keeps running: the stopped component's chain is gone by then.
#
# Dependencies:
#   - common.sh (log, ensure_owned_dir) and, through it, iftunnel.sh (IFTUNNEL_STATE_FILE)
#   - config.sh (XRAY_CHAIN, XRAY_FWMARK, XRAY_FWMARK_MASK, TUN_DIR_CHAIN). Read when a function
#     runs, never when this file is sourced. An XRAY_* one that is unset or empty is no Xray to
#     snapshot; TUN_DIR_CHAIN defaults to TUN_DIR, as in guard.sh.
#   - FWSNAP_DIR: the directory of the snapshot, /tmp/vpn-director/fwsnap unless set. /tmp goes at
#     a reboot, as the rules do. It and its parent are held to watchd's OwnedDir policy
#     (ensure_owned_dir): root's alone, as fwsnap-restore.sh checks before it restores anything -
#     another user able to write there could hand root rules of their own.
#
# Public API:
#   fwsnap_save    - snapshot the rules in place; rc 0 always
#   fwsnap_remove  - remove the snapshot
#
# Internal functions (for testing):
#   _fwsnap_ifaces - the interface tunnels the last apply recorded
#   _fwsnap_mangle - mangle.rules for a listing of the mangle table
#   _fwsnap_nat    - nat.rules for a listing of nat POSTROUTING
#   _fwsnap_put    - write a file of the snapshot, or remove it
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

FWSNAP_DIR="${FWSNAP_DIR:-/tmp/vpn-director/fwsnap}"

###################################################################################################
# Internal helper functions
###################################################################################################

# _fwsnap_ifaces - the interface tunnels the last apply recorded (IFTUNNEL_STATE_FILE), one per
# line; nothing without one.
_fwsnap_ifaces() {
    local iface table
    [[ -s $IFTUNNEL_STATE_FILE ]] || return 0
    while read -r iface table _; do
        [[ -z $iface || -z $table ]] || printf '%s\n' "$iface"
    done < "$IFTUNNEL_STATE_FILE"
}

# _fwsnap_mangle <listing> <ifaces> - mangle.rules for <listing>, what "iptables -t mangle -S"
# printed, and <ifaces>, the interface tunnels one per line. Prints nothing while neither chain
# exists. Every declaration comes ahead of every rule: a jump names a chain, and a chain is there
# only once declared. The jumps are numbered in listing order, Xray's first; on Merlin that puts
# TUN_DIR's ahead of the firmware's iface-mark rules, which match other interfaces, until the
# rebuild that follows the restore puts it back behind them (platform_prerouting_base_pos). The
# MSS rules are the two iftunnel.sh puts in for an interface tunnel, exactly as "iptables -S" lists
# them - "-o <iface> ... -j TCPMSS --clamp-mss-to-pmtu" and "-i <iface> ... -j TCPMSS --set-mss
# <digits>", the name compared as written (it may hold dots) - and no other: another package's MSS
# rule for that interface is not ours to put back at the head of FORWARD. They keep their order
# there, where iftunnel.sh puts them. The listing is read into an array once: bash reads a
# here-string one byte per read call, and this walks the listing up to five times.
_fwsnap_mangle() {
    local chain line spec iface accept mss_out mss_in mss n=0
    local -a lines=() ifaces=() chains=()
    mapfile -t lines <<< "$1"
    mapfile -t ifaces <<< "$2"
    for chain in "${XRAY_CHAIN:-}" "${TUN_DIR_CHAIN:-TUN_DIR}"; do
        if [[ -n $chain && $'\n'$1$'\n' == *$'\n'"-N $chain"$'\n'* ]]; then
            chains+=("$chain")
        fi
    done
    [[ ${#chains[@]} -gt 0 ]] || return 0

    printf '%s\n' '*mangle'
    for chain in "${chains[@]}"; do
        printf ':%s - [0:0]\n' "$chain"
    done
    for chain in "${chains[@]}"; do
        for line in "${lines[@]}"; do
            [[ $line != "-A $chain "* ]] || printf '%s\n' "$line"
        done
    done

    for chain in "${chains[@]}"; do
        for line in "${lines[@]}"; do
            [[ $line == "-A PREROUTING "* ]] || continue
            spec="${line#"-A PREROUTING "}"
            [[ " $spec" == *" -j $chain" ]] || continue
            n=$((n + 1))
            printf -- '-I PREROUTING %s %s\n' "$n" "$spec"
        done
    done

    accept="-m mark --mark ${XRAY_FWMARK:-}/${XRAY_FWMARK_MASK:-} -j ACCEPT"
    if [[ $'\n'$1$'\n' == *$'\n'"-A INPUT $accept"$'\n'* ]]; then
        printf -- '-I INPUT 1 %s\n' "$accept"
    fi

    n=0
    for line in "${lines[@]}"; do
        [[ $line == "-A FORWARD "* && "$line " == *" -j TCPMSS "* ]] || continue
        for iface in "${ifaces[@]}"; do
            [[ -n $iface ]] || continue
            mss_out="-A FORWARD -o $iface -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu"
            mss_in="-A FORWARD -i $iface -p tcp -m tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss "
            mss="${line#"$mss_in"}"
            if [[ $line == "$mss_out" || ( $line == "$mss_in"* && $mss =~ ^[0-9]+$ ) ]]; then
                n=$((n + 1))
                printf -- '-I FORWARD %s %s\n' "$n" "${line#"-A FORWARD "}"
                break
            fi
        done
    done

    printf '%s\n' COMMIT
}

# _fwsnap_nat <listing> <ifaces> - nat.rules for <listing>, what "iptables -t nat -S POSTROUTING"
# printed: the MASQUERADE of each interface tunnel of <ifaces> it lists, in their order. Prints
# nothing without one.
_fwsnap_nat() {
    local iface
    while IFS= read -r iface; do
        [[ -n $iface ]] || continue
        if [[ $'\n'$1$'\n' == *$'\n'"-A POSTROUTING -o $iface -j MASQUERADE"$'\n'* ]]; then
            printf -- '-o %s -j MASQUERADE\n' "$iface"
        fi
    done <<< "$2"
}

# _fwsnap_put <name> <content> - make $FWSNAP_DIR/<name> hold <content>, through a file beside it
# and a rename; no content removes it. The hooks hand the file to iptables-restore as root, so the
# directory and its parent have to be root's alone (ensure_owned_dir): a missing one is made 0755
# whatever the umask, one that group or others can write to loses that write - /tmp/vpn-director
# is also watchd's runtime directory, which an older release's stop left 0777 under Merlin's umask
# 0 - and a symlink, no directory or another user's is refused with a WARN naming it, nothing
# written and nothing removed there: the restore refuses such a directory anyway, and nothing is
# deleted through a symlink. The file is written under umask 077, then made 0644 and renamed into
# place. A write that fails there is a WARN, and takes the file an earlier apply wrote away with
# the one it could not finish: left in place, that stale file would put back, after the next
# rebuild of the firewall, rules the configuration has dropped since.
_fwsnap_put() {
    local file="$FWSNAP_DIR/$1" dir
    if [[ -z $2 ]]; then
        [[ ! -e $file ]] || rm -f "$file" 2>/dev/null || true
        return 0
    fi
    for dir in "${FWSNAP_DIR%/*}" "$FWSNAP_DIR"; do
        if ! ensure_owned_dir "$dir"; then
            log -l WARN "Firewall snapshot: cannot write $file: $dir is a symlink, no directory or not this user's alone; nothing there is touched, and a firewall rebuild restores nothing from it"
            return 0
        fi
    done
    if ! { ( umask 077; printf '%s\n' "$2" > "$file.new" ) && chmod 644 "$file.new" &&
        mv -f "$file.new" "$file"; } 2>/dev/null; then
        rm -f "$file.new" "$file" 2>/dev/null || true
        log -l WARN "Firewall snapshot: cannot write $file; the one of an earlier apply removed, so a firewall rebuild restores nothing from it until an apply writes one"
    fi
    return 0
}

###################################################################################################
# Public API
###################################################################################################

# -------------------------------------------------------------------------------------------------
# fwsnap_save - snapshot the rules in place, for the firmware hooks to restore
# -------------------------------------------------------------------------------------------------
# vpn-director.sh runs it at the end of every apply that gets there, so the snapshot holds the
# rules as that apply left them. One listing of the mangle table answers for both chains, their
# jumps, the INPUT accept and the MSS rules, all as they stood at one moment; nat is listed only
# while there are interface tunnels. Returns 0 always: the snapshot only shortens a window the
# guards keep off the WAN already, and an apply never fails over it. A listing or a write that
# fails is a WARN, and removes what of an earlier apply's snapshot this run cannot bring up to
# date: a failed listing of mangle both files (the run ends there), one of nat POSTROUTING
# nat.rules alone (mangle.rules is this run's), a failed write its own file (_fwsnap_put). Left
# in place, a stale snapshot would put back, after the next rebuild of the firewall, rules the
# configuration has dropped since - an address taken out of a tunnel's exclude, marked direct
# and sent to main ahead of the new guards - until the hook's apply rebuilds.
# -------------------------------------------------------------------------------------------------
fwsnap_save() {
    local listing ifaces nat=""
    ifaces="$(_fwsnap_ifaces 2>/dev/null)" || ifaces=""

    if ! listing="$(iptables -t mangle -S 2>/dev/null)"; then
        fwsnap_remove
        log -l WARN "Firewall snapshot: cannot list mangle; the snapshot of an earlier apply removed, so a firewall rebuild restores nothing until an apply writes one"
        return 0
    fi
    _fwsnap_put mangle.rules "$(_fwsnap_mangle "$listing" "$ifaces")"

    if [[ -n $ifaces ]]; then
        if ! listing="$(iptables -t nat -S POSTROUTING 2>/dev/null)"; then
            rm -f "$FWSNAP_DIR/nat.rules" 2>/dev/null || true
            log -l WARN "Firewall snapshot: cannot list nat POSTROUTING; the nat snapshot of an earlier apply removed, so a rebuild of nat restores nothing until an apply writes one"
            return 0
        fi
        nat="$(_fwsnap_nat "$listing" "$ifaces")"
    fi
    _fwsnap_put nat.rules "$nat"
    return 0
}

# -------------------------------------------------------------------------------------------------
# fwsnap_remove - remove the snapshot
# -------------------------------------------------------------------------------------------------
# stop runs it, whichever component it stops, before it tears anything down: a firewall rebuild
# during the teardown has nothing to bring back. After a full stop none is left until the next
# apply writes one; a component's stop writes one of what keeps running once its teardown is done
# (fwsnap_save).
# -------------------------------------------------------------------------------------------------
fwsnap_remove() {
    rm -f "$FWSNAP_DIR/mangle.rules" "$FWSNAP_DIR/nat.rules" 2>/dev/null || true
}
