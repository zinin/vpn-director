#!/usr/bin/env bash

###################################################################################################
# platform.sh - platform detection and loader for the platform contract
# -------------------------------------------------------------------------------------------------
# VPN Director runs on Asuswrt-Merlin and on KeeneticOS. Everything the core
# modules need from the firmware goes through the platform_* functions defined
# in lib/platform/<name>.sh; the core never tests the platform name itself.
#
# Sourcing this file:
#   1. Uses VPD_PLATFORM when the environment already set it (tests, overrides).
#   2. Otherwise detects the platform from the file system (platform_detect).
#   3. Sources lib/platform/<name>.sh, which defines the contract functions.
# A platform that is neither known nor implemented aborts the sourcing script
# with "unsupported platform".
#
# Contract (every function prints its answer on stdout and returns 0; when it
# cannot answer it prints nothing and returns 1; none of them logs an ERROR):
#   platform_name                          merlin | keenetic
#   platform_wan_if                        active WAN interface name
#   platform_ipv6_enabled                  1 | 0
#   platform_lan_ifaces                    LAN interface names, one per line
#   platform_tunnels                       the tunnels Tunnel Director may route through,
#                                          one id per line, "main" last; on a failed
#                                          inventory it may print only "main" for
#                                          local callers, but returns 1
#   platform_tunnel_unlisted_reason <id>   why a tunnel the firmware has is not listed,
#                                          one line for a log message; nothing and rc 1
#                                          when there is no such reason
#   platform_tunnel_iface <id>             Linux interface of a tunnel
#   platform_tunnel_info <id>              three lines: type, connected (1|0), description
#   platform_tunnel_table <id> <idx>       routing table for "ip rule ... lookup"
#   platform_tunnel_route <id> [gateway]   route spec for the tunnel table (Keenetic)
#   platform_tunnel_route_ensure <id> <idx> [gateway] install the tunnel table route
#   platform_tunnel_table_release <id> <idx> drop the tunnel table route
#   platform_tunnel_offload_target         mangle target that takes a flow out of the
#                                          platform's NAT/route acceleration; nothing
#                                          and rc 1 when none is needed
#   platform_vpn_endpoints                 firmware VPN server hosts, one per line
#   platform_load_module <name>            load a kernel module
#   platform_cron_add <name> <schedule> <cmd> / platform_cron_del <name>
#   platform_cron_requirements            what the user must install before cron runs the
#                                         job; nothing and rc 1 when there is nothing owed
#   platform_tproxy_extra_rules apply|stop [mark/mask] platform-only firewall rules for TPROXY
#   platform_prerouting_base_pos           insert position for TUN_DIR in mangle PREROUTING
#   platform_password_file                 file the Web UI verifies passwords against
#   platform_lan_ip, platform_hostname, platform_model
#   platform_email_supported               1 | 0
#
# What the core relies on beyond those signatures:
#   * platform_tunnel_route has no caller in the core. It is there for the
#     implementation's own use, as the source of the spec that
#     platform_tunnel_route_ensure applies. Both platforms print a default
#     for the tunnel table (OpenVPN via a gateway, Wireguard by device).
#   * platform_tunnel_route_ensure must be idempotent: tunnel.sh calls it for
#     every recorded tunnel on every apply that finds the configuration already
#     up to date, not only when something changed.
#   * platform_tunnel_route_ensure may refuse a tunnel platform_tunnels does
#     not list. TUN_DIR_TABLES can still hold one whose mode changed after it
#     was applied, and on Merlin its table is then the one the firmware routes
#     the whole router through: a default there would take every packet along.
#   * platform_tunnel_unlisted_reason is read for messages only; nothing in
#     the core decides on it.
#   * platform_tunnel_table_release may be called for an index that was never
#     ensured. tunnel_stop walks the recorded state file unconditionally, and
#     tunnel_apply records an index even when its route_ensure failed.
#   * The optional trailing arguments are how a platform learns what only the
#     config knows without reading config globals: tunnel.sh passes
#     tunnel_director.tunnels.<id>.gateway (validated, or empty) and tproxy.sh
#     passes "$XRAY_FWMARK/$XRAY_FWMARK_MASK". Wireguard ignores the gateway
#     on both platforms; Merlin ignores the TPROXY mark (it has no extra rule).
#   * platform_tunnel_offload_target names a target, not a rule: tunnel.sh owns
#     where it goes. It builds the rule with the same match as the MARK rule of
#     the client it belongs to and places it immediately before that rule, so a
#     destination the exclusions return on keeps the platform's acceleration.
#     A platform that needs no opt-out must print nothing - a target name the
#     kernel does not have fails the append and takes the apply down with it.
#   * Keenetic's tunnel tables are KEENETIC_TABLE_BASE + idx (keenetic.sh).
#
# Testing: VPD_PROBE_ROOT prefixes every path platform_detect looks at.
###################################################################################################

# -------------------------------------------------------------------------------------------------
# platform_detect - print the platform this system is, or fail
# -------------------------------------------------------------------------------------------------
platform_detect() {
    local root="${VPD_PROBE_ROOT:-}"
    if [[ -d "$root/opt/etc/ndm" ]] && [[ -x "$root/bin/ndmc" ]]; then
        printf 'keenetic\n'
        return 0
    fi
    if [[ -d "$root/jffs" ]] && [[ -x "$root/bin/nvram" ]]; then
        printf 'merlin\n'
        return 0
    fi
    return 1
}

# -------------------------------------------------------------------------------------------------
# Resolve the platform and load its implementation
# -------------------------------------------------------------------------------------------------
if [[ -z ${VPD_PLATFORM:-} ]]; then
    if ! VPD_PLATFORM="$(platform_detect)"; then
        printf 'unsupported platform: neither Asuswrt-Merlin nor Keenetic detected\n' >&2
        # shellcheck disable=SC2317
        return 1 2>/dev/null || exit 1
    fi
fi
export VPD_PLATFORM

case "$VPD_PLATFORM" in
    merlin|keenetic) ;;
    *)
        printf 'unsupported platform: %s\n' "$VPD_PLATFORM" >&2
        # shellcheck disable=SC2317
        return 1 2>/dev/null || exit 1
        ;;
esac

_platform_impl="${BASH_SOURCE[0]%/*}/platform/${VPD_PLATFORM}.sh"
if [[ ! -f $_platform_impl ]]; then
    printf 'platform implementation not found: %s\n' "$_platform_impl" >&2
    # shellcheck disable=SC2317
    return 1 2>/dev/null || exit 1
fi
# shellcheck disable=SC1090
. "$_platform_impl"
unset _platform_impl
