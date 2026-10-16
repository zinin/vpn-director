#!/bin/sh
# KeeneticOS runs this with "hook" for every layer change of every NDM
# interface: $id (OpenVPN0, Wireguard1, Bridge0, ...), $system_name (its
# Linux name), $layer (conf|link|ctrl|ipv4|ipv6), $level (running|pending|
# disabled). Only the IPv4 layer of a VPN client interface matters: up, its
# tunnel table route can be installed; down, apply logs the WARN and the
# slot's block rule refuses marked traffic until the next change. Detached
# for the reason netfilter.d gives.
#
# Not ifstatechanged.d: on 5.1.5 both directories fire, but one OpenVPN0
# down/up cycle gives this one exactly one "layer=ipv4 level=disabled" and
# one "layer=ipv4 level=running", while ifstatechanged.d's change=connected
# came only on the way up - the down edge would be missed.

# $id and $layer are NDM's, out of the hook environment; nothing here assigns them.
# shellcheck disable=SC2154
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
VPD_SCRIPT="${VPD_SCRIPT:-/opt/vpn-director/vpn-director.sh}"

[ "$1" = "hook" ] || exit 0
case "$id" in
    OpenVPN*|Wireguard*) ;;
    *) exit 0 ;;
esac
[ "$layer" = "ipv4" ] || exit 0

if [ ! -x "$VPD_SCRIPT" ]; then
    logger -t "vpn-director-hook" "vpn-director not found at $VPD_SCRIPT"
    exit 0
fi
if ! command -v nohup >/dev/null 2>&1; then
    logger -t "vpn-director-hook" "nohup not found: opkg install coreutils-nohup"
    exit 0
fi
nohup "$VPD_SCRIPT" --wait apply >/dev/null 2>&1 &
exit 0
