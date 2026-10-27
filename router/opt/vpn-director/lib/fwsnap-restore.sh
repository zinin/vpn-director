#!/bin/sh

###################################################################################################
# fwsnap-restore.sh - put our firewall rules back from the snapshot, ahead of the hook's apply
# -------------------------------------------------------------------------------------------------
# Usage: fwsnap-restore.sh mangle|nat
#
# The firmware hooks run it with the table the firmware has just rebuilt, wait for it, then queue
# the full apply as they did before: KeeneticOS's netfilter.d hook with its $table,
# Asuswrt-Merlin's firewall-start with mangle and nat-start with nat. Until our rules are back the
# guards keep the protected clients off the WAN, but an Xray client's new connections are refused
# and a tunnel client's traffic to an excluded country goes through its tunnel (lib/fwsnap.sh). A
# restore is a few commands; the apply takes seconds.
#
#   mangle  iptables-restore --noflush of $FWSNAP_DIR/mangle.rules, the snapshot fwsnap_save
#           wrote: in one commit it creates each chain the snapshot declares that NDM deleted,
#           flushes and fills each that Merlin's "iptables -t mangle -F" emptied, and puts the
#           jumps back at the top of PREROUTING. Only while PREROUTING jumps to none of those
#           chains: a hook for a table the rebuild left whole, or called again for the same
#           rebuild - NDM calls netfilter.d for mangle six times in one interface cycle on 5.1.5 -
#           must not add a second jump. The snapshot's INPUT and FORWARD rules (KeeneticOS's
#           accept of Xray's mark, the interface tunnels' MSS rules) go in only where
#           "iptables -C" does not find them: a rebuild that took the jumps and left those would
#           have them twice, and a doubled MSS rule outlives its tunnel's release. A restore that
#           went in removes Tunnel Director's hash and leaves the restored marker (tunnel.sh), so
#           the apply that follows rebuilds over whatever the snapshot held that is stale;
#           tproxy_apply rebuilds its chain on every apply anyway.
#   nat     each "-o <iface> -j MASQUERADE" of $FWSNAP_DIR/nat.rules that nat POSTROUTING does
#           not hold, inserted at its head.
#   Nothing for any other table, without the table's snapshot, or once stop has left its marker;
#   stop removes the snapshot as well.
#
# No lock: the restore is made to race an apply. A rebuild that runs meanwhile records no hash
# while the restored marker is there, and its swap deletes every jump to the chain it replaces
# (swap_fw_chain, firewall.sh), one the restore put in beside its own included. The hooks keep
# queuing "--wait apply" without --unless-stopped, as before: a firmware rebuild applies again
# after stop, and the restore changes nothing there.
#
# Fast and small: no common.sh, no jq, no config. NDM runs its hooks one after another under a
# 24-second timeout, and every millisecond here is part of the window. POSIX sh: Asuswrt-Merlin's
# /bin/sh is BusyBox's, which has no "command" builtin. Always exits 0: what this cannot do, the
# apply the hook queues next does.
#
# Environment, for tests and overrides: FWSNAP_DIR (/tmp/vpn-director/fwsnap, as in fwsnap.sh),
# VPD_STOPPED_FILE (/tmp/vpn-director/stopped), VPD_TUN_DIR_HASH and VPD_TUN_DIR_RESTORED (the
# hash and the marker in /tmp/tunnel_director, TUN_DIR_HASH and TUN_DIR_RESTORED of ipset.sh),
# VPD_RESTORE_PATH (the PATH it runs with; the router's directories without it).
###################################################################################################

PATH="${VPD_RESTORE_PATH:-/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin}"
FWSNAP_DIR="${FWSNAP_DIR:-/tmp/vpn-director/fwsnap}"
VPD_STOPPED_FILE="${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}"
VPD_TUN_DIR_HASH="${VPD_TUN_DIR_HASH:-/tmp/tunnel_director/tun_dir_rules.sha256}"
VPD_TUN_DIR_RESTORED="${VPD_TUN_DIR_RESTORED:-/tmp/tunnel_director/restored}"

# A rule is split into words for iptables and never expanded as a file name.
set -f

# restore_mangle - the snapshot's mangle rules, unless PREROUTING still jumps to a chain of it.
# shellcheck disable=SC2086  # a rule's $spec is split into its words on purpose
restore_mangle() {
    snap="$FWSNAP_DIR/mangle.rules"
    [ -f "$snap" ] || return 0
    type iptables-restore >/dev/null 2>&1 || return 0

    # Read whole, never piped into grep -q (shell-conventions.md). A listing that failed says
    # nothing of the jumps, and a restore over jumps that are there would double them.
    listing="$(iptables -t mangle -S PREROUTING 2>/dev/null)" || return 0

    # The copy iptables-restore reads, made in one pass. The snapshot's chains are its ":NAME"
    # lines, which come ahead of every rule: a jump to any of them is a table that still holds our
    # rules, and nothing is restored. An INPUT or FORWARD rule iptables -C finds is left out, and the
    # others of its chain are numbered from 1 again, so each goes in at the head of the chain, in
    # the snapshot's order, at a position the chain has.
    out="" input=0 forward=0
    while IFS= read -r line; do
        case "$line" in
            :*)
                chain="${line#:}"
                chain="${chain%% *}"
                case "$listing
" in
                    *" -j $chain
"*) return 0 ;;
                esac
                ;;
            "-I INPUT "*|"-I FORWARD "*)
                chain="${line#-I }"
                spec="${chain#* }"
                chain="${chain%% *}"
                spec="${spec#* }"
                if iptables -t mangle -C "$chain" $spec 2>/dev/null; then
                    continue
                fi
                if [ "$chain" = INPUT ]; then
                    input=$((input + 1))
                    line="-I INPUT $input $spec"
                else
                    forward=$((forward + 1))
                    line="-I FORWARD $forward $spec"
                fi
                ;;
        esac
        out="$out$line
"
    done < "$snap"
    # Nothing read: the snapshot went after the check above.
    [ -n "$out" ] || return 0

    # printf, not ":": a redirection that fails ends the shell when the command is a special
    # builtin.
    copy="$snap.$$"
    if { printf '%s' "$out" > "$copy"; } 2>/dev/null && iptables-restore --noflush < "$copy"; then
        { printf '' > "$VPD_TUN_DIR_RESTORED"; } 2>/dev/null
        rm -f "$copy" "$VPD_TUN_DIR_HASH"
        logger -t vpn-director "Restored the mangle rules from the snapshot"
    else
        rm -f "$copy"
    fi
}

# restore_nat - each MASQUERADE of the snapshot that nat POSTROUTING does not hold, at its head.
# shellcheck disable=SC2086  # as above
restore_nat() {
    snap="$FWSNAP_DIR/nat.rules"
    [ -f "$snap" ] || return 0
    while IFS= read -r spec; do
        [ -n "$spec" ] || continue
        iptables -t nat -C POSTROUTING $spec 2>/dev/null ||
            iptables -t nat -I POSTROUTING 1 $spec 2>/dev/null
    done < "$snap"
}

[ ! -e "$VPD_STOPPED_FILE" ] || exit 0
case "${1:-}" in
    mangle) restore_mangle ;;
    nat) restore_nat ;;
esac
exit 0
