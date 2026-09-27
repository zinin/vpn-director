#!/usr/bin/env bash

###################################################################################################
# platform/keenetic.sh - KeeneticOS implementation of the platform contract
# -------------------------------------------------------------------------------------------------
# See lib/platform.sh for the contract. Facts come from NDM's RCI over HTTP
# (http://localhost:79/rci/<path>, JSON), from ip-full and from the Entware
# cron. Tunnel routing tables are this module's own (KEENETIC_TABLE_BASE + idx):
# the firmware keeps none for its VPN client interfaces.
#
# Test seams: VPD_MODULES_DIR, VPD_PROC_MODULES, VPD_CRON_D, VPD_CRON_INIT.
###################################################################################################

platform_name() {
    printf 'keenetic\n'
}

# -------------------------------------------------------------------------------------------------
# _rci_get <path> - print the JSON body of http://localhost:79/rci/<path>
# -------------------------------------------------------------------------------------------------
# NDM answers an unknown path with 404 and an absent object with an empty
# body; both are "no answer" here (rc 1, nothing printed). Callers parse the
# body with jq -r and never use jq's regex functions (test, match, sub): the
# Entware jq on KeeneticOS lacks them.
_rci_get() {
    local body
    body="$(curl -sf --max-time 5 "http://localhost:79/rci/${1:-}" 2>/dev/null)" || return 1
    [[ -n $body ]] || return 1
    printf '%s\n' "$body"
}

# The WAN is whatever carries the IPv4 default route; the firmware has no one
# variable for it. "ip -4 route show default" lists the routes in metric
# order, so the first line is the active one.
platform_wan_if() {
    local dev
    dev="$(ip -4 route show default 2>/dev/null |
        awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }' || true)"
    [[ -n $dev ]] || return 1
    printf '%s\n' "$dev"
}

platform_ipv6_enabled() {
    if ip -6 route show default 2>/dev/null | grep -q .; then
        printf '1\n'
    else
        printf '0\n'
    fi
}

# Bridge0 (Home). Bridge1 (Guest) and other segments are out of scope (spec 16).
platform_lan_ifaces() {
    printf 'br0\n'
}

platform_lan_ip() {
    local ip
    ip="$(ip -4 -o addr show br0 2>/dev/null | awk '{ print $4; exit }' || true)"
    ip="${ip%%/*}"
    [[ -n $ip ]] || return 1
    printf '%s\n' "$ip"
}

platform_hostname() {
    local json name
    json="$(_rci_get show/system)" || return 1
    name="$(printf '%s' "$json" | jq -r '.hostname // empty' 2>/dev/null)"
    [[ -n $name ]] || return 1
    printf '%s\n' "$name"
}

platform_model() {
    local json model
    json="$(_rci_get show/version)" || return 1
    model="$(printf '%s' "$json" | jq -r '.model // empty' 2>/dev/null)"
    [[ -n $model ]] || return 1
    printf '%s\n' "$model"
}

# No /etc/shadow on KeeneticOS. Entware's root password is field 2 of
# /opt/etc/passwd (MD5-crypt), which auth.ShadowAuth reads the same way.
platform_password_file() {
    printf '/opt/etc/passwd\n'
}

# amtm email is a Merlin feature.
platform_email_supported() {
    printf '0\n'
}

# -------------------------------------------------------------------------------------------------
# Tunnels: NDM's OpenVPN and Wireguard client interfaces
# -------------------------------------------------------------------------------------------------
# First routing table Tunnel Director owns: table = KEENETIC_TABLE_BASE + idx.
# Clear of the firmware's tables (42+ for connection policies, 4096, 16386).
KEENETIC_TABLE_BASE=2000

# Ids are NDM interface ids (OpenVPN0, Wireguard1) - the keys of "show
# interface" - and main is always last, as on Merlin. NDM not answering means
# only main (spec 13): every configured tunnel is then skipped with a WARN.
platform_tunnels() {
    local json
    if json="$(_rci_get show/interface)"; then
        printf '%s' "$json" | jq -r '
            to_entries[] | select(.value.type == "OpenVPN" or .value.type == "Wireguard") | .key' 2>/dev/null || true
    fi
    printf '%s\n' main
}

# Every OpenVPN and WireGuard interface RCI lists is one Tunnel Director may
# route through: its tables are Tunnel Director's own (KEENETIC_TABLE_BASE +
# idx), and no firmware rule reads them. There is never a reason to give.
platform_tunnel_unlisted_reason() {
    return 1
}

# The Linux name is a convention (OpenVPNN -> ovpn_brN, WireguardN -> nwgN):
# RCI does not expose it. While the tunnel is up the interface must carry the
# address RCI reports, or the convention does not hold on this router and the
# caller is told so (rc 1) rather than routing into the wrong device. Down, or
# with NDM not answering, the convention is the best answer there is.
platform_tunnel_iface() {
    local id="${1:-}" iface json rci_addr link_addr
    case "$id" in
        OpenVPN[0-9]*)   iface="ovpn_br${id#OpenVPN}" ;;
        Wireguard[0-9]*) iface="nwg${id#Wireguard}" ;;
        *)               return 1 ;;
    esac
    if json="$(_rci_get "show/interface/$id")" &&
       [[ "$(printf '%s' "$json" | jq -r '.connected // empty' 2>/dev/null)" == yes ]]; then
        rci_addr="$(printf '%s' "$json" | jq -r '.address // empty' 2>/dev/null)"
        link_addr="$(ip -4 -o addr show "$iface" 2>/dev/null | awk '{ print $4; exit }' || true)"
        link_addr="${link_addr%%/*}"
        [[ -n $rci_addr && $rci_addr == "$link_addr" ]] || return 1
    fi
    printf '%s\n' "$iface"
}

platform_tunnel_info() {
    local id="${1:-}" json type connected=0 desc
    case "$id" in
        OpenVPN[0-9]*|Wireguard[0-9]*) ;;
        *) return 1 ;;
    esac
    json="$(_rci_get "show/interface/$id")" || return 1
    case "$(printf '%s' "$json" | jq -r '.type // empty' 2>/dev/null)" in
        OpenVPN)   type=openvpn ;;
        Wireguard) type=wireguard ;;
        *)         return 1 ;;
    esac
    if [[ "$(printf '%s' "$json" | jq -r '.connected // empty' 2>/dev/null)" == yes ]]; then
        connected=1
    fi
    desc="$(printf '%s' "$json" | jq -r '.description // empty' 2>/dev/null)"
    printf '%s\n%s\n%s\n' "$type" "$connected" "$desc"
}

# One table per configured tunnel, idx being its 0-based position in
# tunnel_director.tunnels; numbers are used directly, there is no rt_tables.
# A pure mapping: it must answer while the tunnel is down, because the ip
# rule is installed regardless (an empty table falls through to main).
platform_tunnel_table() {
    local id="${1:-}" idx="${2:-}"
    case "$id" in
        main) printf 'main\n' ;;
        OpenVPN[0-9]*|Wireguard[0-9]*)
            [[ $idx =~ ^[0-9]+$ ]] || return 1
            printf '%d\n' $((KEENETIC_TABLE_BASE + 10#$idx))
            ;;
        *) return 1 ;;
    esac
}

# _keenetic_first_host <address> <mask> - the first host of the subnet, dotted
# -------------------------------------------------------------------------------------------------
# 10.73.149.113 255.255.255.0 -> 10.73.149.1: the server side of an OpenVPN
# "subnet" topology, which is the gateway the route needs when the config
# names none.
#
# Only the octets are checked, not that the mask is a mask: a /31 or /32 (an
# OpenVPN "topology p2p", where the peer is not the subnet's first host) gets
# an answer that is wrong rather than none, and the caller installs it. That
# is what tunnel_director.tunnels.<id>.gateway is for.
_keenetic_first_host() {
    local a1 a2 a3 a4 m1 m2 m3 m4 o net first
    IFS=. read -r a1 a2 a3 a4 <<< "${1:-}"
    IFS=. read -r m1 m2 m3 m4 <<< "${2:-}"
    for o in "${a1:-x}" "${a2:-x}" "${a3:-x}" "${a4:-x}" "${m1:-x}" "${m2:-x}" "${m3:-x}" "${m4:-x}"; do
        [[ $o =~ ^[0-9]{1,3}$ ]] && [[ $((10#$o)) -le 255 ]] || return 1
    done
    net=$(( ((10#$a1 << 24) | (10#$a2 << 16) | (10#$a3 << 8) | 10#$a4) &
           ((10#$m1 << 24) | (10#$m2 << 16) | (10#$m3 << 8) | 10#$m4) ))
    first=$(( net + 1 ))
    printf '%d.%d.%d.%d\n' $(( (first >> 24) & 255 )) $(( (first >> 16) & 255 )) \
        $(( (first >> 8) & 255 )) $(( first & 255 ))
}

# platform_tunnel_route <id> [gateway] - the route the tunnel's table needs
# -------------------------------------------------------------------------------------------------
# Wireguard: "default dev nwgN" - a peer-to-peer device the kernel routes
# through without a next hop, so a configured gateway has nothing to apply to
# and is ignored (silently: a platform function may not log).
#
# OpenVPN: "default via <gateway> dev ovpn_brN", the gateway being
# tunnel_director.tunnels.<id>.gateway when the caller passes one, else the
# first host of the tunnel subnet RCI reports. Only that second path needs the
# tunnel up - RCI reports no address for a down tunnel, so it prints nothing;
# with a gateway in hand the spec is printed either way and the failure moves
# to "ip route replace", which needs the interface.
platform_tunnel_route() {
    local id="${1:-}" gateway="${2:-}" iface json addr mask
    iface="$(platform_tunnel_iface "$id")" || return 1
    case "$id" in
        Wireguard[0-9]*)
            printf 'default dev %s\n' "$iface"
            ;;
        OpenVPN[0-9]*)
            if [[ -z $gateway ]]; then
                json="$(_rci_get "show/interface/$id")" || return 1
                [[ "$(printf '%s' "$json" | jq -r '.connected // empty' 2>/dev/null)" == yes ]] || return 1
                addr="$(printf '%s' "$json" | jq -r '.address // empty' 2>/dev/null)"
                mask="$(printf '%s' "$json" | jq -r '.mask // empty' 2>/dev/null)"
                gateway="$(_keenetic_first_host "$addr" "$mask")" || return 1
            fi
            printf 'default via %s dev %s\n' "$gateway" "$iface"
            ;;
        *) return 1 ;;
    esac
}

# "ip route replace" is idempotent, which tunnel.sh relies on: it calls this
# on every apply, the ones that change nothing included, so the route follows
# the interface across flaps once the interface hook fires an apply.
platform_tunnel_route_ensure() {
    local id="${1:-}" idx="${2:-}" gateway="${3:-}" table spec
    table="$(platform_tunnel_table "$id" "$idx")" || return 1
    [[ $table != main ]] || return 0
    spec="$(platform_tunnel_route "$id" "$gateway")" || return 1
    # shellcheck disable=SC2086
    ip route replace $spec table "$table" 2>/dev/null
}

# Flushing a table that was never filled is not an error: tunnel_stop calls
# this for every recorded index, ensured or not.
platform_tunnel_table_release() {
    local table
    table="$(platform_tunnel_table "${1:-}" "${2:-}")" || return 1
    [[ $table != main ]] || return 0
    ip route flush table "$table" 2>/dev/null || true
    return 0
}

# KeeneticOS binds an established *forwarded* flow to a fast path that runs
# before mangle and never returns to it: the conntrack hook sits at priority
# -200 and mangle at -150, so once a flow is bound its packets never reach
# TUN_DIR at all. Tunnel Director's MARK is then applied to the first few
# packets only, the rest miss the ip rule and leave through the WAN. Measured
# on a KN-4521: conntrack counted 24 packets of a flow, TUN_DIR counted 6, and
# a 1 MB download stalled at one TCP window.
#
# `-j PPE` (mangle only, Keenetic's 4.9-ndm kernel) sets ct->fast_ext on the
# conntrack entry. `!ct->fast_ext` is a condition of both the fastnat and the
# fastroute entry test, and the same target sets FOE_ALG_SKIP, which closes the
# hardware path too. The flag lives as long as the conntrack entry.
#
# Xray needs none of this: TPROXY terminates the connection in a local socket,
# so there is no forwarded flow left to accelerate.
platform_tunnel_offload_target() {
    printf 'PPE\n'
}

# Hosts the firmware's own tunnels talk to, so TPROXY never captures them:
# each OpenVPN server (remote-endpoint-address) and each Wireguard peer's
# current endpoint (wireguard.peer[].remote, C3). RCI hands one peer back as
# an object and several as an array, hence the type test.
platform_vpn_endpoints() {
    local json
    json="$(_rci_get show/interface)" || return 1
    printf '%s' "$json" | jq -r '
        to_entries[] | .value |
        if .type == "OpenVPN" then (.["remote-endpoint-address"] // empty)
        elif .type == "Wireguard" then
            ((.wireguard.peer // []) | if type == "array" then .[] else . end | .remote // empty)
        else empty end' 2>/dev/null
}

# -------------------------------------------------------------------------------------------------
# Kernel modules, cron, TPROXY, PREROUTING position
# -------------------------------------------------------------------------------------------------
# No modprobe on KeeneticOS. The modules of the "Kernel modules for Netfilter"
# firmware component (opkg-kmod-netfilter) sit under /lib/modules/<release>/
# and nothing autoloads them; a missing .ko means the component is not
# installed, which the caller reports.
platform_load_module() {
    local name="${1:-}" dir="${VPD_MODULES_DIR:-/lib/modules/$(uname -r)}"
    [[ -n $name ]] || return 1
    if grep -q "^${name} " "${VPD_PROC_MODULES:-/proc/modules}" 2>/dev/null; then
        return 0
    fi
    [[ -f "$dir/$name.ko" ]] || return 1
    insmod "$dir/$name.ko" 2>/dev/null
}

# Entware's cron (Vixie, package "cron") reads /opt/etc/cron.d: one file per
# job, "<schedule> root <command>", picked up within a minute. The daemon is
# started when the init script's check says it is not running; its process is
# named cron, not crond, so the script is asked rather than pidof. The job
# file is written before the daemon is checked, so a router that gets the cron
# package later already has its job.
platform_cron_add() {
    local name="${1:-}" schedule="${2:-}" cmd="${3:-}"
    local dir="${VPD_CRON_D:-/opt/etc/cron.d}" init="${VPD_CRON_INIT:-/opt/etc/init.d/S10cron}"
    [[ -n $name && -n $schedule && -n $cmd ]] || return 1
    mkdir -p "$dir" 2>/dev/null || return 1
    # Vixie cron refuses a crontab that is not 0600: it logs "(*system*) BAD
    # FILE MODE" at startup and ignores the file, so the job never runs and the
    # only trace is one syslog line. /opt/etc/crontab, which the package ships
    # and which does run, is 0600 for the same reason. Set the mode on an empty
    # file, before the content goes in.
    : > "$dir/$name" || return 1
    chmod 600 "$dir/$name" || return 1
    # A job started from cron.d inherits cron's own PATH - /usr/bin:/bin - and
    # none of Entware's tools live there, so the update would die on its first
    # jq. This is the PATH the NDM hooks and S99vpn-director use.
    {
        printf 'PATH=%s\n' '/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin'
        printf '%s root %s\n' "$schedule" "$cmd"
    } >> "$dir/$name" || return 1
    # No init script is the cron package not being installed. What to do about
    # it is platform_cron_requirements' answer: a contract function prints its
    # answer or fails silently, and never logs - lib/platform.sh sources
    # without common.sh, where log does not exist.
    [[ -x $init ]] || return 1
    "$init" check >/dev/null 2>&1 || "$init" start >/dev/null 2>&1
}

platform_cron_del() {
    local name="${1:-}" dir="${VPD_CRON_D:-/opt/etc/cron.d}"
    [[ -n $name ]] || return 1
    rm -f "$dir/$name"
}

# The one thing a Keenetic needs before the job file above can run. Printed,
# not logged, and only when it is missing: the caller decides how to say it.
platform_cron_requirements() {
    local dir="${VPD_CRON_D:-/opt/etc/cron.d}" init="${VPD_CRON_INIT:-/opt/etc/init.d/S10cron}"
    [[ -x $init ]] && return 1
    printf "Entware's cron is not installed (%s is missing): opkg install cron - the job file in %s is already written and starts running once the package is there\n" \
        "$init" "$dir"
}

# mangle INPUT jumps port 443 to _NDM_HTTP_INPUT_TLS_, which drops every TLS
# ClientHello whose SNI is not the router's own; TPROXY keeps the original
# port, so proxied HTTPS would die there. ACCEPT in mangle INPUT ends that
# chain's traversal for packets carrying our mark. Position 1, ahead of NDM's
# jump. NDM deletes the rule on every rebuild; the netfilter.d hook re-applies.
# The mark is the caller's (tproxy.sh passes "$XRAY_FWMARK/$XRAY_FWMARK_MASK").
platform_tproxy_extra_rules() {
    local verb="${1:-}" mark="${2:-0x100/0x100}" i=0
    case "$verb" in
        apply)
            iptables -t mangle -C INPUT -m mark --mark "$mark" -j ACCEPT 2>/dev/null ||
                iptables -t mangle -I INPUT 1 -m mark --mark "$mark" -j ACCEPT
            ;;
        stop)
            # Every copy; bounded, for an iptables whose -D always succeeds.
            while [[ $i -lt 16 ]] && iptables -t mangle -D INPUT -m mark --mark "$mark" -j ACCEPT 2>/dev/null; do
                i=$((i + 1))
            done
            return 0
            ;;
        *) return 1 ;;
    esac
}

# XRAY_TPROXY takes position 1 when Xray is applied and TUN_DIR goes right after
# it, ahead of every _NDM_* jump: NDM guards its DNS routing chain with
# "-m mark --mark 0x0", so our marks must be set before it runs. Without Xray the
# constant leaves TUN_DIR behind the first _NDM_* jump (_NDM_PREROUTING_MC on
# 5.1.5, multicast - harmless).
platform_prerouting_base_pos() {
    printf '2\n'
}
