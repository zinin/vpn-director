#!/bin/sh
# KeeneticOS runs this after it rebuilt one netfilter table, once per table
# and family ($table: filter|nat|mangle, $type: iptables|ip6tables). Every
# rebuild deletes our chains, so re-apply. Detached: NDM runs its hooks one
# at a time under a 24-second timeout, and an apply can take longer. --wait
# queues the apply behind a running one, so a burst of rebuilds converges on
# the final state; an apply that finds nothing changed is cheap.
#
# The rules of the last apply go back first, from the snapshot it left
# (fwsnap-restore.sh), and the hook waits for that: until then the guards
# refuse the Xray clients' connections and send a tunnel client's excluded
# countries through its tunnel. It leaves a table that still holds our rules
# alone, so the several calls of one rebuild add no second jump.
#
# Argv is "start" on 5.1.5, but nothing here reads it: a future NDM verb must
# re-apply too, and $type/$table already say whether the rebuild was ours.

# $type and $table are NDM's, out of the hook environment; nothing here assigns them.
# shellcheck disable=SC2154
# Asuswrt-Merlin runs everything under umask 0; nothing this creates may be writable by others.
umask 022
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
VPD_SCRIPT="${VPD_SCRIPT:-/opt/vpn-director/vpn-director.sh}"
VPD_RESTORE="${VPD_RESTORE:-/opt/vpn-director/lib/fwsnap-restore.sh}"

[ "$type" = "iptables" ] || exit 0
case "$table" in
    mangle|nat|filter) ;;
    *) exit 0 ;;
esac

if [ ! -x "$VPD_SCRIPT" ]; then
    logger -t "vpn-director-hook" "vpn-director not found at $VPD_SCRIPT"
    exit 0
fi
if [ -x "$VPD_RESTORE" ]; then
    "$VPD_RESTORE" "$table" >/dev/null 2>&1
fi
if ! command -v nohup >/dev/null 2>&1; then
    logger -t "vpn-director-hook" "nohup not found: opkg install coreutils-nohup"
    exit 0
fi
nohup "$VPD_SCRIPT" --wait apply >/dev/null 2>&1 &
exit 0
