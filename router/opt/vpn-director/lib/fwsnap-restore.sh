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
#           have them twice, and a doubled MSS rule outlives its tunnel's release. A -C that
#           answers neither way (a busy xtables lock) restores nothing. A restore that went in
#           removes Tunnel Director's hash and leaves the restored marker (tunnel.sh), so the
#           apply that follows rebuilds over whatever the snapshot held that is stale;
#           tproxy_apply rebuilds its chain on every apply anyway.
#   nat     each "-o <iface> -j MASQUERADE" of $FWSNAP_DIR/nat.rules that "iptables -C" does not
#           find in nat POSTROUTING, inserted at its head.
#   Nothing for any other table, without the table's snapshot, or once a full stop has left its
#   marker; a full stop removes the snapshot as well. A component's stop leaves no marker, and
#   leaves the snapshot of what keeps running.
#
# Root's alone: whoever can write the snapshot hands root the rules it restores. A snapshot file is
# read only when it is a regular file, no symlink, and it, its directory and that directory's
# parent are root's own, the two directories no symlinks, and none of the three is writable by
# group or others unless it has the sticky bit (trusted) - watchd's OwnedDir policy, which
# fwsnap.sh holds the directories to (ensure_owned_dir). Otherwise nothing is restored, with a line
# in syslog. What this writes is root's alone too (umask 077): the copy iptables-restore reads, in
# that checked directory, and the restored marker, removed first and created anew, never written
# through a path that is there - a symlink planted in /tmp/tunnel_director would have its target
# truncated as root.
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
# The copy and the marker are root's alone: the hooks bring the firmware's umask, 0 on Merlin.
umask 077
FWSNAP_DIR="${FWSNAP_DIR:-/tmp/vpn-director/fwsnap}"
VPD_STOPPED_FILE="${VPD_STOPPED_FILE:-/tmp/vpn-director/stopped}"
VPD_TUN_DIR_HASH="${VPD_TUN_DIR_HASH:-/tmp/tunnel_director/tun_dir_rules.sha256}"
VPD_TUN_DIR_RESTORED="${VPD_TUN_DIR_RESTORED:-/tmp/tunnel_director/restored}"

# A rule is split into words for iptables and never expanded as a file name.
set -f

# trusted <file> - may root restore from <file>? A regular file and no symlink; the file, its
# directory and that directory's parent each owned by the effective user (root on the router), the
# two directories no symlinks either; and none of the three writable by group or others unless it
# has the sticky bit. The modes come from one "ls -ldn" of the three, whose lines ls sorts by name,
# so each line is judged alone: in the mode field the 6th character "w" is group write, the 9th
# "w" other write, and a 10th "t" or "T" the sticky bit, the one [ -k ] tests. Any doubt - ls
# failing, a line missing - is "not trusted".
# shellcheck disable=SC3067  # -O is a builtin test of BusyBox ash and dash, the shells it runs under
trusted() {
    t_dir="${1%/*}"
    t_parent="${t_dir%/*}"
    [ -f "$1" ] && [ ! -L "$1" ] && [ -O "$1" ] &&
        [ ! -L "$t_dir" ] && [ -O "$t_dir" ] &&
        [ ! -L "$t_parent" ] && [ -O "$t_parent" ] || return 1
    t_modes="$(ls -ldn "$1" "$t_dir" "$t_parent" 2>/dev/null)" || return 1
    t_lines=0
    while read -r t_mode _; do
        t_lines=$((t_lines + 1))
        case "$t_mode" in
            ?????????[tT]*) ;;
            ?????w*|????????w*) return 1 ;;
        esac
    done <<EOF
$t_modes
EOF
    [ "$t_lines" -eq 3 ]
}

# untrusted <file> - the syslog line for a snapshot trusted refused.
untrusted() {
    logger -t vpn-director "Snapshot $1 not restored: the file or its directory is not root's alone"
}

# restore_mangle - the snapshot's mangle rules, unless PREROUTING still jumps to a chain of it.
# shellcheck disable=SC2086  # a rule's $spec is split into its words on purpose
restore_mangle() {
    snap="$FWSNAP_DIR/mangle.rules"
    [ -f "$snap" ] || return 0
    trusted "$snap" || { untrusted "$snap"; return 0; }
    type iptables-restore >/dev/null 2>&1 || return 0

    # Read whole, never piped into grep -q (shell-conventions.md). A listing that failed says
    # nothing of the jumps, and a restore over jumps that are there would double them.
    listing="$(iptables -t mangle -S PREROUTING 2>/dev/null)" || return 0

    # The copy iptables-restore reads, made in one pass. The snapshot's chains are its ":NAME"
    # lines, which come ahead of every rule: a jump to any of them is a table that still holds our
    # rules, and nothing is restored. An INPUT or FORWARD rule iptables -C finds (0) is left out,
    # and the others of its chain (1) are numbered from 1 again, so each goes in at the head of the
    # chain, in the snapshot's order, at a position the chain has. Any other status - 4 for a busy
    # xtables lock, an apply's among them - says nothing of the rule, and nothing is restored, as
    # for a listing that failed: one read as missing would go in a second time.
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
                iptables -t mangle -C "$chain" $spec 2>/dev/null
                case $? in
                    0) continue ;;
                    1) ;;
                    *) return 0 ;;
                esac
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
        # The marker anew, with noclobber: whatever stands at its path goes first, and a path that
        # is there again - a symlink planted meanwhile - is never written through.
        rm -f "$VPD_TUN_DIR_RESTORED"
        ( set -C; printf '' > "$VPD_TUN_DIR_RESTORED" ) 2>/dev/null
        rm -f "$copy" "$VPD_TUN_DIR_HASH"
        logger -t vpn-director "Restored the mangle rules from the snapshot"
    else
        rm -f "$copy"
    fi
}

# restore_nat - each MASQUERADE of the snapshot that nat POSTROUTING does not hold, at its head:
# only where iptables -C says it is not there (1). Any other status says nothing - a busy xtables
# lock, as above - and the rule is left to the apply the hook queues.
# shellcheck disable=SC2086  # as above
restore_nat() {
    snap="$FWSNAP_DIR/nat.rules"
    [ -f "$snap" ] || return 0
    trusted "$snap" || { untrusted "$snap"; return 0; }
    while IFS= read -r spec; do
        [ -n "$spec" ] || continue
        iptables -t nat -C POSTROUTING $spec 2>/dev/null
        if [ $? -eq 1 ]; then
            iptables -t nat -I POSTROUTING 1 $spec 2>/dev/null
        fi
    done < "$snap"
}

[ ! -e "$VPD_STOPPED_FILE" ] || exit 0
case "${1:-}" in
    mangle) restore_mangle ;;
    nat) restore_nat ;;
esac
exit 0
