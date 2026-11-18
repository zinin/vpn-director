#!/usr/bin/env bash

###################################################################################################
# platform/merlin.sh - Asuswrt-Merlin implementation of the platform contract
# -------------------------------------------------------------------------------------------------
# See lib/platform.sh for the contract. Facts come from nvram, the firmware's
# named routing tables (wgcN, ovpncN in /etc/iproute2/rt_tables) and cru.
###################################################################################################

platform_name() {
    printf 'merlin\n'
}

# The firmware flags one WAN slot as primary; its interface is the active one.
platform_wan_if() {
    local idx name
    for idx in 0 1 2; do
        if [[ "$(nvram get "wan${idx}_primary" 2>/dev/null || true)" == "1" ]]; then
            name="$(nvram get "wan${idx}_ifname" 2>/dev/null || true)"
            [[ -n $name ]] || return 1
            printf '%s\n' "$name"
            return 0
        fi
    done
    name="$(nvram get wan0_ifname 2>/dev/null || true)"
    [[ -n $name ]] || return 1
    printf '%s\n' "$name"
}

platform_ipv6_enabled() {
    local s
    s="$(nvram get ipv6_service 2>/dev/null || true)"
    if [[ -n $s ]] && [[ $s != "disabled" ]]; then
        printf '1\n'
    else
        printf '0\n'
    fi
}

platform_lan_ifaces() {
    printf 'br0\n'
}

# The "Redirect Internet traffic through tunnel" setting of OpenVPN client N
# (ovpncN): nvram vpn_clientN_rgw, "" when it is not set. 0 "No", 1 "Yes (all)",
# 2 "VPN Director (policy rules)"; the firmware reads an empty value as 0
# (nvram_pf_get_int).
_merlin_ovpn_rgw() {
    nvram get "vpn_client${1#ovpnc}_rgw" 2>/dev/null || true
}

# Whether Tunnel Director may route through <id>. In "No" and "Yes (all)" the
# firmware adds "from all lookup ovpncN" at priority 10000+N while the client
# runs (amvpn_set_routing_rules in libovpn/amvpn_routing.c, Merlin 388): every
# packet of the router reads that table ahead of Tunnel Director's fwmark rule,
# and a default there takes them all into the tunnel - the direct clients, the
# router itself, Xray's upstream and the other tunnels' clients. Only VPN
# Director mode leaves the table to the rules that name their sources. A
# WireGuard client has no such setting - the firmware routes every wgcN by VPN
# Director rules alone (it sets rgw = OVPN_RGW_POLICY for WireGuard) - and main
# is no tunnel. Read on every call: the mode can be switched at any time.
_merlin_tunnel_routable() {
    [[ ${1:-} =~ ^ovpnc[0-9]+$ ]] || return 0
    [[ "$(_merlin_ovpn_rgw "$1")" == 2 ]]
}

# wgcN first, ovpncN next, main always last. RT_TABLES_FILE overrides the path
# for tests. An OpenVPN client is listed only in VPN Director mode
# (_merlin_tunnel_routable). A TAP client never is: its page offers no such
# mode, and Tunnel Director, which routes through tun1N, could not use it.
platform_tunnels() {
    local rt_tables="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}" wgc ovpn id
    # Keep main available to local callers, but report an incomplete inventory
    # so cmd_platform cannot mistake it for a valid empty Merlin tunnel list.
    if [[ ! -f $rt_tables || ! -r $rt_tables ]]; then
        printf '%s\n' main
        return 1
    fi
    wgc="$(awk '$0!~/^#/ && $2 ~ /^wgc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null)" || {
        printf '%s\n' main
        return 1
    }
    ovpn="$(awk '$0!~/^#/ && $2 ~ /^ovpnc[0-9]+$/ { print $2 }' "$rt_tables" 2>/dev/null)" || {
        printf '%s\n' main
        return 1
    }
    if [[ -n $wgc ]]; then
        wgc="$(printf '%s\n' "$wgc" | sort)" || {
            printf '%s\n' main
            return 1
        }
        printf '%s\n' "$wgc"
    fi
    if [[ -n $ovpn ]]; then
        ovpn="$(printf '%s\n' "$ovpn" | sort)" || {
            printf '%s\n' main
            return 1
        }
    fi
    while IFS= read -r id; do
        if [[ -n $id ]] && _merlin_tunnel_routable "$id"; then
            printf '%s\n' "$id"
        fi
    done <<< "$ovpn"
    printf '%s\n' main
}

# Why an OpenVPN client rt_tables names is not listed: its redirect mode, for
# the warning tunnel.sh logs where it would otherwise call the tunnel unknown.
# Nothing and rc 1 for a tunnel platform_tunnels lists, a WireGuard client,
# main, and an id rt_tables does not name - a typo has no mode.
platform_tunnel_unlisted_reason() {
    local id="${1:-}" rt_tables="${RT_TABLES_FILE:-/etc/iproute2/rt_tables}" rgw setting
    [[ $id =~ ^ovpnc[0-9]+$ ]] || return 1
    awk -v id="$id" '$0 !~ /^#/ && $2 == id { found = 1 } END { exit !found }' "$rt_tables" 2>/dev/null ||
        return 1
    if _merlin_tunnel_routable "$id"; then
        return 1
    fi
    rgw="$(_merlin_ovpn_rgw "$id")"
    case "$rgw" in
        ''|0) setting="No" ;;
        1)    setting="Yes (all)" ;;
        *)    setting="$rgw" ;;
    esac
    printf 'OpenVPN client %s is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "%s")\n' \
        "${id#ovpnc}" "$setting"
}

# OpenVPN client N runs on tun1N; WireGuard clients are named after their table.
platform_tunnel_iface() {
    case "${1:-}" in
        wgc[0-9]*)   printf '%s\n' "$1" ;;
        ovpnc[0-9]*) printf 'tun1%s\n' "${1#ovpnc}" ;;
        *)           return 1 ;;
    esac
}

platform_tunnel_info() {
    local id="${1:-}" type desc="" iface connected=0
    case "$id" in
        wgc[0-9]*)
            type=wireguard
            desc="$(nvram get "${id}_desc" 2>/dev/null || true)"
            ;;
        ovpnc[0-9]*)
            type=openvpn
            desc="$(nvram get "vpn_client${id#ovpnc}_desc" 2>/dev/null || true)"
            ;;
        *) return 1 ;;
    esac
    iface="$(platform_tunnel_iface "$id")"
    # "<POINTOPOINT,NOARP,UP,LOWER_UP>": UP as a whole flag, not the tail of LOWER_UP.
    # No \b: busybox grep on the routers has no word boundaries.
    if ip -o link show "$iface" 2>/dev/null | grep -qE '<([^>]*,)?UP[,>]'; then
        connected=1
    fi
    printf '%s\n%s\n%s\n' "$type" "$connected" "$desc"
}

# The firmware keeps a routing table per tunnel under the tunnel's own name, so
# the mapping is the identity for every id platform_tunnels lists - "main"
# included, which is why this cannot just defer to platform_tunnel_iface (that
# one has no interface to name for "main"). Anything that is not a tunnel id at
# all gets the contract's general answer: nothing on stdout, rc 1.
platform_tunnel_table() {
    case "${1:-}" in
        wgc[0-9]*|ovpnc[0-9]*|main) printf '%s\n' "$1" ;;
        *)                          return 1 ;;
    esac
}

# First host of an IPv4 CIDR (10.74.150.53/24 → 10.74.150.1). Same arithmetic
# as Keenetic's _keenetic_first_host: a /31 or /32 is answered wrongly rather
# than not at all, and tunnel_director.tunnels.<id>.gateway is what overrides
# it. Prefix and octets are range-checked so a shift cannot fold 300 into a
# neighbouring octet.
_merlin_first_host() {
    local cidr="${1:-}" a1 a2 a3 a4 pfx o net first left
    local -a mask=()
    [[ $cidr =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})/([0-9]{1,2})$ ]] || return 1
    a1="${BASH_REMATCH[1]}" a2="${BASH_REMATCH[2]}" a3="${BASH_REMATCH[3]}" a4="${BASH_REMATCH[4]}"
    pfx="${BASH_REMATCH[5]}"
    for o in "$a1" "$a2" "$a3" "$a4"; do
        [[ $((10#$o)) -le 255 ]] || return 1
    done
    (( 10#$pfx <= 32 )) || return 1
    left=$((10#$pfx))
    for o in 0 1 2 3; do
        if (( left >= 8 )); then
            mask+=("255")
            left=$((left - 8))
        elif (( left > 0 )); then
            mask+=("$(( 256 - (1 << (8 - left)) ))")
            left=0
        else
            mask+=("0")
        fi
    done
    net=$(( ((10#$a1 << 24) | (10#$a2 << 16) | (10#$a3 << 8) | 10#$a4) &
           ((10#${mask[0]} << 24) | (10#${mask[1]} << 16) | (10#${mask[2]} << 8) | 10#${mask[3]}) ))
    first=$(( net + 1 ))
    printf '%d.%d.%d.%d\n' $(( (first >> 24) & 255 )) $(( (first >> 16) & 255 )) \
        $(( (first >> 8) & 255 )) $(( first & 255 ))
}

# OpenVPN in policy mode (rgw=2) copies WAN into ovpncN when the server does
# not push redirect-gateway. Tunnel Director then marks into a table whose
# default is still the WAN, so the spec is that default: via the configured
# gateway, else the first host of tun1N. Wireguard is a p2p device, so a
# configured gateway has nothing to apply to. Only the OpenVPN path without a
# gateway needs the interface to carry an address.
platform_tunnel_route() {
    local id="${1:-}" gateway="${2:-}" iface cidr
    iface="$(platform_tunnel_iface "$id")" || return 1
    case "$id" in
        wgc[0-9]*)
            printf 'default dev %s\n' "$iface"
            ;;
        ovpnc[0-9]*)
            if [[ -z $gateway ]]; then
                cidr="$(ip -4 -o addr show "$iface" 2>/dev/null |
                    awk '{ for (i = 1; i <= NF; i++) if ($i == "inet") { print $(i + 1); exit } }')" || true
                [[ -n $cidr ]] || return 1
                gateway="$(_merlin_first_host "$cidr")" || return 1
            fi
            printf 'default via %s dev %s\n' "$gateway" "$iface"
            ;;
        *) return 1 ;;
    esac
}

# Take the default out of the table of an OpenVPN client in "No" mode
# (vpn_clientN_rgw 0 or unset). Whenever the client comes up in that mode the
# firmware deletes its own ("ip route del default table ovpncN" in
# libovpn/openvpn_control.c), so a default found there is one an earlier
# Tunnel Director apply installed - and the firmware's "from all" rule sends
# every packet of the router to it. The dev selector keeps the delete to a
# default through the tunnel, the only kind Tunnel Director installs. In "Yes
# (all)" the default is the firmware's and stays. Prints nothing; rc 0.
_merlin_drop_default() {
    local id="${1:-}" rgw iface
    [[ $id =~ ^ovpnc[0-9]+$ ]] || return 0
    rgw="$(_merlin_ovpn_rgw "$id")"
    [[ -z $rgw || $rgw == 0 ]] || return 0
    iface="$(platform_tunnel_iface "$id")" || return 0
    ip route del default dev "$iface" table "$id" 2>/dev/null || true
}

# "ip route replace" is idempotent, which tunnel.sh relies on: it calls this
# on every apply, including those that change nothing, so the default follows
# the interface across OpenVPN flaps once wan-event fires an apply. A tunnel
# _merlin_tunnel_routable refuses gets none: the configuration can still name
# it, and TUN_DIR_TABLES can still hold it after its mode was switched, but
# its table is the one the firmware routes the whole router through. In "No"
# mode a default an earlier apply left there goes as well.
platform_tunnel_route_ensure() {
    local id="${1:-}" idx="${2:-}" gateway="${3:-}" table spec
    table="$(platform_tunnel_table "$id" "$idx")" || return 1
    [[ $table != main ]] || return 0
    if ! _merlin_tunnel_routable "$id"; then
        _merlin_drop_default "$id"
        return 1
    fi
    spec="$(platform_tunnel_route "$id" "$gateway")" || return 1
    # shellcheck disable=SC2086
    ip route replace $spec table "$table" 2>/dev/null
}

# The rest of ovpncN/wgcN is firmware's (LAN routes, the tunnel prefix, DNS).
# Flushing it would drop those. tunnel_stop only needs the ip rule gone; in VPN
# Director mode the default we installed is inert without it. In "No" mode the
# firmware's own "from all" rule reads the table, so that default goes.
platform_tunnel_table_release() {
    _merlin_drop_default "${1:-}"
    return 0
}

# Nothing sits between a forwarded packet and mangle on this firmware, so a
# marked flow keeps reaching TUN_DIR for its whole life and there is no
# acceleration to opt out of. Printing a target name here would make tunnel.sh
# add a rule naming a target this kernel does not have, and every apply would
# die on it.
platform_tunnel_offload_target() {
    return 1
}

platform_vpn_endpoints() {
    local slot addr
    for slot in 1 2 3 4 5; do
        addr="$(nvram get "vpn_client${slot}_addr" 2>/dev/null || true)"
        [[ -n $addr ]] || continue
        printf '%s\n' "$addr"
    done
}

platform_load_module() {
    local name="${1:-}"
    [[ -n $name ]] || return 1
    # The module name is the first column of every lsmod format (a miss costs a redundant modprobe).
    if lsmod 2>/dev/null | grep -q "^${name}[[:space:]]"; then
        return 0
    fi
    modprobe "$name" 2>/dev/null
}

platform_cron_add() {
    local name="$1" schedule="$2" cmd="$3"
    cru a "$name" "$schedule $cmd"
}

platform_cron_del() {
    cru d "$1"
}

# cru is the firmware's own and is always there, so a cron that refuses the job
# on Merlin is not something the user can install their way out of: nothing to
# print, rc 1.
platform_cron_requirements() {
    return 1
}

# ------------------------------------------------------------------ Shared hooks
#
# The firmware runs /jffs/scripts/<hook> for its events, one file per event,
# shared with the owner and with other add-ons (Skynet, Diversion, amtm, ...).
# VPN Director's logic lives in files of its own, /opt/vpn-director/hooks/<hook>,
# which an update replaces whole; each shared hook gets one line of ours,
# ending in " # vpn-director", and nothing else in it is ours. Releases up to
# v0.22.0 wrote the whole shared file: their bodies, kept byte for byte under
# hooks/legacy/<hook>.<version>, are cut out where one is found whole.
# VPD_JFFS_SCRIPTS_DIR and VPD_HOOKS_DIR move both directories for the tests.

_MERLIN_HOOKS="firewall-start wan-event nat-start"
_MERLIN_HOOK_MARK=" # vpn-director"

# _merlin_hook_line <hook> - our line for the shared <hook>: our hook with the
# firmware's arguments, skipped while Entware is not mounted. It names the shell
# running the shared hook (VPD_HOOK_SHELL), whose other commands our detached
# apply waits for (hooks/<hook>).
_merlin_hook_line() {
    local dir="${VPD_HOOKS_DIR:-/opt/vpn-director/hooks}"
    printf '[ -x %s/%s ] && VPD_HOOK_SHELL=$$ %s/%s "$@"%s\n' "$dir" "$1" "$dir" "$1" "$_MERLIN_HOOK_MARK"
}

# _merlin_hook_foreign <line> <lines...> - does any of <lines>, ours aside, run
# something: a line that is neither blank nor a comment.
_merlin_hook_foreign() {
    local line="$1" l text
    shift
    for l in "$@"; do
        [[ $l != "$line" ]] || continue
        text=${l#"${l%%[![:space:]]*}"}
        [[ -n $text && $text != '#'* ]] && return 0
    done
    return 1
}

# _merlin_hook_stray_call <line> <lines...> - does any of <lines>, ours aside,
# call vpn-director.sh: an earlier body someone edited, so it was not cut out,
# applying a second time on every event.
_merlin_hook_stray_call() {
    local line="$1" l text
    shift
    for l in "$@"; do
        [[ $l != "$line" ]] || continue
        text=${l#"${l%%[![:space:]]*}"}
        [[ $text != '#'* && $text == *vpn-director.sh* ]] && return 0
    done
    return 1
}

# _merlin_hook_ensure <hook> - our line in the shared <hook>, everything else in
# it kept; the lines and the status of platform_hooks_install for one hook. A
# rewrite goes through a file beside the hook, renamed over it, and only when
# something changes; a file renamed over a symlink would put a file in place of
# the owner's link, so a symlinked hook is rewritten by hand.
_merlin_hook_ensure() {
    local hook="$1" dir="${VPD_JFFS_SCRIPTS_DIR:-/jffs/scripts}"
    local file tmp line body version i j n found first=-1 replaced=0 dups=0 joined make_x=0 off=0
    local -a lines=() old=() kept=() changes=()
    file="$dir/$hook"
    tmp="$dir/.$hook.vpd-new"
    line=$(_merlin_hook_line "$hook")

    # No hook, or an empty one: the firmware runs nothing there yet.
    if [[ ! -s $file ]]; then
        if { mkdir -p "$dir" && printf '#!/bin/sh\n%s\n' "$line" > "$tmp" && chmod 755 "$tmp" &&
            mv -f "$tmp" "$file"; } 2>/dev/null; then
            printf 'ok: %s: created\n' "$file"
            return 0
        fi
        rm -f "$tmp" 2>/dev/null || true
        printf 'warn: %s: cannot write it; the hook is as it was\n' "$file"
        return 1
    fi

    mapfile -t lines < "$file"
    # Without "#!" the firmware cannot run the file; giving it one would start
    # running commands that do not run now, which is the owner's call.
    if [[ ${lines[0]:-} != '#!'* ]]; then
        printf 'warn: %s: left alone: no #! line, so the firmware does not run it; add #!/bin/sh as its first line, then run vpn-director.sh hooks\n' "$file"
        return 1
    fi

    # An earlier release's body, found whole after the file's own "#!" line, is
    # ours to remove; a single line of it never is - a "fi" of ours reads like
    # a "fi" of theirs.
    for body in "${VPD_HOOKS_DIR:-/opt/vpn-director/hooks}/legacy/$hook".*; do
        [[ -f $body ]] || continue
        version=${body##*/"$hook".}
        mapfile -t old < "$body"
        old=("${old[@]:1}")
        n=${#old[@]}
        if (( n == 0 )); then
            continue
        fi
        found=-1
        for (( i = 1; i + n <= ${#lines[@]}; i++ )); do
            for (( j = 0; j < n; j++ )); do
                [[ ${lines[i + j]} == "${old[j]}" ]] || break
            done
            if (( j == n )); then
                found=$i
                break
            fi
        done
        if (( found >= 0 )); then
            lines=("${lines[@]:0:found}" "${lines[@]:found + n}")
            changes+=("$version body removed")
        fi
    done

    # Our line: the first marked one keeps its place with the current text, any
    # other marked one goes, and with none ours goes in after the "#!" line.
    for i in "${!lines[@]}"; do
        if [[ ${lines[i]} == *"$_MERLIN_HOOK_MARK" ]]; then
            if (( first < 0 )); then
                first=${#kept[@]}
                if [[ ${lines[i]} != "$line" ]]; then
                    replaced=1
                fi
                kept+=("$line")
            else
                dups=1
            fi
        else
            kept+=("${lines[i]}")
        fi
    done
    if (( first < 0 )); then
        kept=("${kept[0]}" "$line" "${kept[@]:1}")
        changes+=("line added")
    elif (( replaced )); then
        changes+=("line replaced")
    fi
    if (( dups )); then
        changes+=("duplicate lines removed")
    fi
    # The firmware runs no hook that is not executable. One that holds nothing
    # but ours is made so; one that also runs someone else's commands is off,
    # and switching it on is not ours to do: it gets the line and keeps its mode.
    if [[ ! -x $file ]]; then
        if _merlin_hook_foreign "$line" "${kept[@]:1}"; then
            off=1
        else
            make_x=1
            changes+=("made executable")
        fi
    fi

    if (( ${#changes[@]} == 0 )); then
        if (( ! off )); then
            printf 'ok: %s: unchanged\n' "$file"
        fi
    elif [[ ${changes[*]} == "made executable" ]]; then
        if ! chmod +x "$file" 2>/dev/null; then
            printf 'warn: %s: cannot write it; the hook is as it was\n' "$file"
            return 1
        fi
        printf 'ok: %s: updated (made executable)\n' "$file"
    elif [[ -L $file ]]; then
        printf 'warn: %s: left alone: it is a symlink, which a rewrite would replace with a file; put this line after the #! line of the file it points to: %s\n' "$file" "$line"
        return 1
    else
        # The new text goes into a copy of the hook, which keeps the hook's own
        # mode; only one made executable above gains the execute bits.
        if ! { cp -p "$file" "$tmp" && printf '%s\n' "${kept[@]}" > "$tmp" &&
            { (( ! make_x )) || chmod +x "$tmp"; } && mv -f "$tmp" "$file"; } 2>/dev/null; then
            rm -f "$tmp" 2>/dev/null || true
            printf 'warn: %s: cannot write it; the hook is as it was\n' "$file"
            return 1
        fi
        printf -v joined '%s, ' "${changes[@]}"
        printf 'ok: %s: updated (%s)\n' "$file" "${joined%, }"
    fi

    if (( off )); then
        printf 'warn: %s: not executable, so the firmware runs nothing in it; chmod +x it if its commands should run, then run vpn-director.sh hooks\n' "$file"
    fi
    if _merlin_hook_stray_call "$line" "${kept[@]:1}"; then
        printf 'warn: %s: still calls vpn-director.sh outside the vpn-director line; remove that call by hand\n' "$file"
    fi
    return "$off"
}

# One line per shared hook, "ok: <file>: <what was done>" or "warn: <file>:
# <what the owner has to do>", for the caller to log; rc 1 when a hook is left
# without our line or cannot run it.
platform_hooks_install() {
    local hook rc=0
    for hook in $_MERLIN_HOOKS; do
        _merlin_hook_ensure "$hook" || rc=1
    done
    return "$rc"
}

# What the owner can act on, per hook: the line missing, a hook the firmware
# does not run, or a call of vpn-director.sh outside our line - which an
# update reports in its own log only, and a successful update deletes that.
platform_hooks_status() {
    local hook file line
    local -a lines=()
    for hook in $_MERLIN_HOOKS; do
        file="${VPD_JFFS_SCRIPTS_DIR:-/jffs/scripts}/$hook"
        line=$(_merlin_hook_line "$hook")
        if [[ -f $file ]] && grep -qxF -- "$line" "$file"; then
            mapfile -t lines < "$file"
            if [[ ! -x $file ]] && _merlin_hook_foreign "$line" "${lines[@]:1}"; then
                printf '%s: not executable, so the firmware runs nothing in it; chmod +x it if its commands should run\n' "$file"
            elif [[ ! -x $file ]]; then
                printf '%s: not executable; run vpn-director.sh hooks\n' "$file"
            elif _merlin_hook_stray_call "$line" "${lines[@]:1}"; then
                printf '%s: vpn-director line in place; still calls vpn-director.sh outside it - remove that call by hand\n' "$file"
            else
                printf '%s: vpn-director line in place\n' "$file"
            fi
        else
            printf '%s: no vpn-director line; run vpn-director.sh hooks\n' "$file"
        fi
    done
}

# Nothing to add outside our own chain on Merlin. The verb is still checked:
# accepting anything would let a typo in a future call site pass as a clean
# no-op on this platform and only surface on the one that acts on it.
platform_tproxy_extra_rules() {
    case "${1:-}" in
        apply|stop) return 0 ;;
        *)          return 1 ;;
    esac
}

# Position right after the firmware's own iface-mark rules (-i wgcN / -i tunN
# -j MARK --set-...), so Tunnel Director never precedes them.
#
# The chain is read into a variable first, and an iptables that cannot answer
# fails the whole function. Piping iptables straight into awk would let awk's
# END print a position for a chain nobody read: rc 0 and "1" without pipefail,
# so the caller would insert TUN_DIR ahead of the firmware's own rules.
platform_prerouting_base_pos() {
    local raw
    raw="$(iptables -t mangle -S PREROUTING 2>/dev/null)" || return 1
    printf '%s\n' "$raw" |
    awk '
        $1 == "-A" {
          i++
          if ( ($0 ~ /-i wgc[0-9]+/ || $0 ~ /-i tun[0-9]+/) &&
               $0 ~ /-j MARK/ && $0 ~ /--set-/ ) {
            last = i
          }
        }
        END { print (last ? last + 1 : 1) }
    '
}

platform_password_file() {
    printf '/etc/shadow\n'
}

platform_lan_ip() {
    local ip
    ip="$(nvram get lan_ipaddr 2>/dev/null || true)"
    [[ -n $ip ]] || return 1
    printf '%s\n' "$ip"
}

platform_hostname() {
    local name
    name="$(nvram get lan_hostname 2>/dev/null || true)"
    [[ -n $name ]] || return 1
    printf '%s\n' "$name"
}

platform_model() {
    local model
    model="$(nvram get model 2>/dev/null || true)"
    [[ -n $model ]] || return 1
    printf '%s\n' "$model"
}

# amtm email is a Merlin feature; whether it is configured stays send-email.sh's check.
platform_email_supported() {
    printf '1\n'
}
