#!/usr/bin/env bats

load '../test_helper'

load_platform() {
    export VPD_PLATFORM=merlin
    source "$LIB_DIR/platform.sh"
}

# with_mock <name> <script body> puts a one-off mock first in PATH.
with_mock() {
    mkdir -p "$BATS_TEST_TMPDIR/mock"
    printf '#!/bin/bash\n%s\n' "$2" > "$BATS_TEST_TMPDIR/mock/$1"
    chmod +x "$BATS_TEST_TMPDIR/mock/$1"
    export PATH="$BATS_TEST_TMPDIR/mock:$PATH"
}

@test "platform_name: merlin" {
    load_platform
    run platform_name
    assert_output "merlin"
}

# ---------------------------------------------------------------- WAN / IPv6

@test "platform_wan_if: interface of the primary WAN" {
    load_platform
    run platform_wan_if
    assert_success
    assert_output "eth0"
}

@test "platform_wan_if: falls back to wan0_ifname when no WAN is primary" {
    load_platform
    with_mock nvram 'case "$*" in "get wan0_ifname") echo eth9 ;; *) echo "" ;; esac'
    run platform_wan_if
    assert_success
    assert_output "eth9"
}

@test "platform_wan_if: fails and prints nothing when nvram knows no WAN" {
    load_platform
    with_mock nvram 'echo ""'
    run platform_wan_if
    assert_failure
    refute_output
}

@test "platform_ipv6_enabled: 1 when ipv6_service is set" {
    load_platform
    run platform_ipv6_enabled
    assert_output "1"
}

@test "platform_ipv6_enabled: 0 when ipv6_service is disabled or empty" {
    load_platform
    with_mock nvram 'case "$*" in "get ipv6_service") echo disabled ;; *) echo "" ;; esac'
    run platform_ipv6_enabled
    assert_output "0"
    with_mock nvram 'echo ""'
    run platform_ipv6_enabled
    assert_output "0"
}

@test "platform_lan_ifaces: br0" {
    load_platform
    run platform_lan_ifaces
    assert_output "br0"
}

# ------------------------------------------------------------------ tunnels

@test "platform_tunnels: wgc, then ovpnc from rt_tables, then main" {
    load_platform
    run platform_tunnels
    assert_success
    assert_line --index 0 "wgc1"
    assert_line --index 1 "wgc2"
    assert_line --index 2 "ovpnc1"
    assert_line --index 3 "ovpnc2"
    assert_line --index 4 "main"
}

@test "platform_tunnels: reports a missing rt_tables while keeping main for local callers" {
    load_platform
    RT_TABLES_FILE="$BATS_TEST_TMPDIR/absent" run platform_tunnels
    assert_failure
    assert_output "main"
}

@test "platform_tunnels: reports a routing table read error" {
    load_platform
    mkdir "$BATS_TEST_TMPDIR/rt_tables_directory"
    RT_TABLES_FILE="$BATS_TEST_TMPDIR/rt_tables_directory" run platform_tunnels
    assert_failure
    assert_output "main"
}

# "Redirect Internet traffic through tunnel" is nvram vpn_clientN_rgw: 0 "No",
# 1 "Yes (all)", 2 "VPN Director (policy rules)"; the firmware reads an empty
# value as 0. In "No" and "Yes (all)" it adds "from all lookup ovpncN" at
# priority 10000+N while the client runs, so every packet of the router reads
# the client's table first, and a default Tunnel Director put there would take
# them all into the tunnel.

# with_ovpn_modes <client 1> <client 2> - an nvram that answers the redirect
# mode of OpenVPN clients 1 and 2 as given ("" for unset) and nothing for any
# other key.
with_ovpn_modes() {
    with_mock nvram "case \"\$*\" in
    \"get vpn_client1_rgw\") echo '$1' ;;
    \"get vpn_client2_rgw\") echo '$2' ;;
    *) echo '' ;;
esac"
}

@test "platform_tunnels: an OpenVPN client is listed only in VPN Director mode" {
    load_platform
    with_ovpn_modes 0 2
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 ovpnc2 main)"
    with_ovpn_modes 2 1
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 ovpnc1 main)"
    with_ovpn_modes "" ""
    run platform_tunnels
    assert_success
    assert_output "$(printf '%s\n' wgc1 wgc2 main)"
}

@test "platform_tunnel_unlisted_reason: names the redirect mode of an OpenVPN client it leaves out" {
    load_platform
    with_ovpn_modes 0 1
    run platform_tunnel_unlisted_reason ovpnc1
    assert_success
    assert_output 'OpenVPN client 1 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No")'
    run platform_tunnel_unlisted_reason ovpnc2
    assert_success
    assert_output 'OpenVPN client 2 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "Yes (all)")'
    with_ovpn_modes "" 3
    run platform_tunnel_unlisted_reason ovpnc1
    assert_success
    assert_output 'OpenVPN client 1 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "No")'
    run platform_tunnel_unlisted_reason ovpnc2
    assert_success
    assert_output 'OpenVPN client 2 is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "3")'
}

# A typo in the tunnel id has no mode: ovpnc9 is not in the fixture rt_tables,
# so its warning stays the generic "not a tunnel this platform knows".
@test "platform_tunnel_unlisted_reason: nothing for a listed tunnel, WireGuard, main or an id rt_tables does not name" {
    load_platform
    with_ovpn_modes 0 2
    local id
    for id in ovpnc2 wgc1 main ovpnc9 eth0 ""; do
        run platform_tunnel_unlisted_reason "$id"
        assert_failure
        refute_output
    done
}

@test "_merlin_tunnel_routable: only complete ovpncN ids consult the current mode" {
    load_platform
    with_ovpn_modes "" ""
    for id in ovpnc2junk ovpnc ovpncx2 xovpnc2 wgc1 main eth0 ""; do
        run _merlin_tunnel_routable "$id"
        assert_success
        refute_output
    done
    run _merlin_tunnel_routable ovpnc1
    assert_failure
    refute_output

    with_ovpn_modes 2 1
    run _merlin_tunnel_routable ovpnc1
    assert_success
    refute_output
    run _merlin_tunnel_routable ovpnc2
    assert_failure
    refute_output

    with_ovpn_modes 0 2
    run _merlin_tunnel_routable ovpnc1
    assert_failure
    refute_output
    run _merlin_tunnel_routable ovpnc2
    assert_success
    refute_output
}

@test "platform_tunnel_iface: wgcN is its own interface, ovpncN is tun1N" {
    load_platform
    run platform_tunnel_iface wgc1
    assert_output "wgc1"
    run platform_tunnel_iface ovpnc3
    assert_output "tun13"
}

@test "platform_tunnel_iface: unknown ids fail" {
    load_platform
    run platform_tunnel_iface main
    assert_failure
    refute_output
    run platform_tunnel_iface eth0
    assert_failure
}

@test "platform_tunnel_info: type, connected and description on three lines" {
    load_platform
    run platform_tunnel_info wgc1
    assert_success
    assert_line --index 0 "wireguard"
    assert_line --index 1 "0"
    assert_line --index 2 "Office WG"
    run platform_tunnel_info ovpnc1
    assert_line --index 0 "openvpn"
    assert_line --index 2 "Office OVPN"
}

@test "platform_tunnel_info: connected is 1 when the interface carries UP" {
    load_platform
    with_mock ip 'echo "12: wgc1: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420 qdisc noqueue state UNKNOWN"'
    run platform_tunnel_info wgc1
    assert_line --index 1 "1"
}

@test "platform_tunnel_info: unknown id fails" {
    load_platform
    run platform_tunnel_info eth0
    assert_failure
    refute_output
}

@test "platform_tunnel_table: the id itself; main stays main" {
    load_platform
    run platform_tunnel_table wgc1 0
    assert_output "wgc1"
    run platform_tunnel_table main 3
    assert_output "main"
}

@test "platform_tunnel_table: an id that is not a tunnel fails" {
    load_platform
    run platform_tunnel_table eth0 0
    assert_failure
    refute_output
    run platform_tunnel_table
    assert_failure
    refute_output
}

# Firmware OpenVPN in policy mode (rgw=2) copies WAN into ovpncN when the
# server does not push redirect-gateway. Tunnel Director then marks packets
# into a table whose default is still the WAN. The spec is the default that
# replace puts in that table. In that mode release stays a no-op because the
# rest of the table is firmware's.

with_tun_addr() {
    printf '%s\n' "$@" > "$BATS_TEST_TMPDIR/addrs"
    export BATS_IP_ADDRS_FILE="$BATS_TEST_TMPDIR/addrs"
}

@test "platform_tunnel_route: OpenVPN via the subnet's first host, Wireguard by device" {
    load_platform
    with_tun_addr "tun12 10.74.150.53/24"
    run platform_tunnel_route ovpnc2
    assert_success
    assert_output "default via 10.74.150.1 dev tun12"
    run platform_tunnel_route wgc1
    assert_success
    assert_output "default dev wgc1"
}

@test "platform_tunnel_route: a configured gateway replaces the computed one" {
    load_platform
    run platform_tunnel_route ovpnc2 10.74.150.254
    assert_success
    assert_output "default via 10.74.150.254 dev tun12"
}

@test "platform_tunnel_route: nothing while the OpenVPN tunnel has no address" {
    load_platform
    with_tun_addr "tun11 10.73.149.53/24"
    run platform_tunnel_route ovpnc2
    assert_failure
    refute_output
    run platform_tunnel_route ovpnc1
    assert_success
    assert_output "default via 10.73.149.1 dev tun11"
}

@test "platform_tunnel_route_ensure: replaces the default in the tunnel's table, idempotently" {
    load_platform
    with_tun_addr "tun12 10.74.150.53/24"
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure ovpnc2 1
    assert_success
    run platform_tunnel_route_ensure ovpnc2 1
    assert_success
    assert_equal "$(grep -c 'ip route replace default via 10.74.150.1 dev tun12 table ovpnc2' /tmp/bats_ip_calls.log)" 2
    run platform_tunnel_route_ensure wgc1 0 ""
    assert_success
    grep -q 'ip route replace default dev wgc1 table wgc1' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_route_ensure: passes the gateway on and fails for a down OpenVPN tunnel" {
    load_platform
    with_tun_addr "tun12 10.74.150.53/24"
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure ovpnc2 1 10.74.150.254
    assert_success
    grep -q 'ip route replace default via 10.74.150.254 dev tun12 table ovpnc2' /tmp/bats_ip_calls.log
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure ovpnc1 0
    assert_failure
    refute grep -q 'table ovpnc1' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_route_ensure and _table_release: main touches no table" {
    load_platform
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure main 4
    assert_success
    run platform_tunnel_table_release main 4
    assert_success
    [ ! -s /tmp/bats_ip_calls.log ]
}

# Unlike Keenetic, Merlin must not flush ovpncN/wgcN: firmware keeps LAN
# routes, the tunnel prefix and DNS there. Stopping Tunnel Director only
# drops the ip rule; the table stays.
@test "platform_tunnel_table_release: does not flush a firmware table" {
    load_platform
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_table_release ovpnc2 1
    assert_success
    run platform_tunnel_table_release wgc1 0
    assert_success
    [ ! -s /tmp/bats_ip_calls.log ]
}

# Out of VPN Director mode the client's table is the one the firmware routes the
# whole router through, so no default goes in. In "No" the firmware deletes its
# own default whenever the client comes up, so one found there is Tunnel
# Director's and goes; in "Yes (all)" the default is the firmware's and stays.
@test "platform_tunnel_route_ensure: installs nothing for an OpenVPN client out of VPN Director mode" {
    load_platform
    with_tun_addr "tun11 10.73.149.53/24"
    local mode
    for mode in 0 1 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_route_ensure ovpnc1 0
        assert_failure
        refute_output
        refute grep -q 'ip route replace' /tmp/bats_ip_calls.log
    done
}

@test "platform_tunnel_route_ensure: takes the default out of the table of a client in No mode" {
    load_platform
    local mode
    for mode in 0 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_route_ensure ovpnc1 0
        assert_failure
        grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    done
    with_ovpn_modes 1 2
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure ovpnc1 0
    assert_failure
    refute grep -q 'ip route del' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_table_release: takes the default out of a table in No mode only" {
    load_platform
    local mode
    for mode in 0 ""; do
        with_ovpn_modes "$mode" 2
        : > /tmp/bats_ip_calls.log
        run platform_tunnel_table_release ovpnc1 0
        assert_success
        grep -qx 'ip route del default dev tun11 table ovpnc1' /tmp/bats_ip_calls.log
    done
    with_ovpn_modes 1 2
    : > /tmp/bats_ip_calls.log
    run platform_tunnel_table_release ovpnc1 0
    assert_success
    run platform_tunnel_table_release ovpnc2 1
    assert_success
    run platform_tunnel_table_release wgc1 0
    assert_success
    [[ ! -s /tmp/bats_ip_calls.log ]]
}

@test "_merlin_drop_default: only complete ovpncN ids can have a default removed" {
    load_platform
    with_ovpn_modes 0 0
    : > /tmp/bats_ip_calls.log
    local id
    for id in ovpnc2junk ovpnc ovpncx2 xovpnc2 wgc1 main ""; do
        run _merlin_drop_default "$id"
        assert_success
        refute_output
    done
    [[ ! -s /tmp/bats_ip_calls.log ]]
}

# Merlin's firmware puts nothing between a forwarded packet and mangle, so
# there is no acceleration to opt out of and nothing for tunnel.sh to insert.
# refute_output is the load-bearing half: a target name leaking out of this
# platform would add a rule naming a target the Merlin kernel does not have,
# and every apply would die on it.
@test "platform_tunnel_offload_target: merlin has no fast path to opt out of" {
    load_platform
    run platform_tunnel_offload_target
    assert_failure
    refute_output
}

@test "platform_vpn_endpoints: non-empty vpn_clientN_addr values, one per line" {
    load_platform
    run platform_vpn_endpoints
    assert_success
    assert_line --index 0 "openvpn1.example.com"
    assert_line --index 1 "example.com"
    [ "${#lines[@]}" -eq 2 ]
}

# ------------------------------------------------------------ modules / cron

@test "platform_load_module: already loaded module needs no modprobe" {
    load_platform
    : > /tmp/bats_modprobe_calls.log
    run platform_load_module xt_TPROXY
    assert_success
    [ ! -s /tmp/bats_modprobe_calls.log ]
}

@test "platform_load_module: modprobes a module lsmod does not list" {
    load_platform
    : > /tmp/bats_modprobe_calls.log
    run platform_load_module xt_socket
    assert_success
    grep -q "modprobe xt_socket" /tmp/bats_modprobe_calls.log
}

@test "platform_load_module: fails when modprobe fails" {
    load_platform
    with_mock modprobe 'exit 1'
    run platform_load_module xt_socket
    assert_failure
}

# cru is the firmware's own, always there: nothing for the user to install,
# so nothing is printed and no Entware advice can reach a Merlin router.
@test "platform_cron_requirements: merlin has nothing to require" {
    load_platform
    run platform_cron_requirements
    assert_failure
    refute_output
}

@test "platform_cron_add / platform_cron_del: cru a and cru d" {
    load_platform
    : > /tmp/bats_cru_calls.log
    run platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    assert_success
    run platform_cron_del vpn_director_update
    assert_success
    grep -qF 'cru a vpn_director_update 0 3 * * * /opt/vpn-director/vpn-director.sh update' /tmp/bats_cru_calls.log
    grep -qF 'cru d vpn_director_update' /tmp/bats_cru_calls.log
}

# ------------------------------------------------------------- firewall bits

@test "platform_tproxy_extra_rules: nothing to do on Merlin" {
    load_platform
    : > /tmp/bats_iptables_calls.log
    run platform_tproxy_extra_rules apply
    assert_success
    run platform_tproxy_extra_rules stop
    assert_success
    [ ! -s /tmp/bats_iptables_calls.log ]
}

# The contract documents the verb as apply|stop. Accepting anything would let a
# typo in a future call site read as a clean no-op here and only break on the
# platform that acts on the verb.
@test "platform_tproxy_extra_rules: rejects a verb that is not apply or stop" {
    load_platform
    run platform_tproxy_extra_rules
    assert_failure
    refute_output
    run platform_tproxy_extra_rules aply
    assert_failure
    refute_output
}

@test "platform_prerouting_base_pos: 1 when PREROUTING has no firmware mark rules" {
    load_platform
    run platform_prerouting_base_pos
    assert_output "1"
}

@test "platform_prerouting_base_pos: right after the last firmware iface-mark rule" {
    load_platform
    with_mock iptables 'cat <<EOF
-P PREROUTING ACCEPT
-A PREROUTING -i wgc1 -j MARK --set-xmark 0x1/0x7
-A PREROUTING -i tun11 -j MARK --set-xmark 0x2/0x7
-A PREROUTING -i br0 -j SOMETHING_ELSE
EOF'
    run platform_prerouting_base_pos
    assert_output "3"
}

# An iptables that cannot read the chain must not be turned into a position:
# awk's END would print 1, and the caller would insert TUN_DIR at the top of
# PREROUTING, ahead of the firmware's iface-mark rules.
@test "platform_prerouting_base_pos: fails and prints nothing when iptables fails" {
    load_platform
    with_mock iptables 'exit 3'
    run platform_prerouting_base_pos
    assert_failure
    refute_output
}

# ------------------------------------------------------------- system facts

@test "platform_password_file: /etc/shadow" {
    load_platform
    run platform_password_file
    assert_output "/etc/shadow"
}

@test "platform_lan_ip, platform_hostname, platform_model come from nvram" {
    load_platform
    run platform_lan_ip
    assert_output "192.168.50.1"
    run platform_hostname
    assert_output "RT-AX88U-1234"
    run platform_model
    assert_output "RT-AX88U"
}

@test "platform_lan_ip: fails when nvram has no lan_ipaddr" {
    load_platform
    NVRAM_NO_LAN_IP=1 run platform_lan_ip
    assert_failure
    refute_output
}

@test "platform_email_supported: 1" {
    load_platform
    run platform_email_supported
    assert_output "1"
}

# ---------------------------------------------------------------- Shared hooks
#
# The firmware runs /jffs/scripts/<hook>, one file per event, shared with the
# owner and other add-ons. platform_hooks_install puts one line of ours into
# each (it calls /opt/vpn-director/hooks/<hook>) and keeps everything else, an
# earlier release's whole body aside, which it cuts out when it finds it whole.
# VPD_JFFS_SCRIPTS_DIR and VPD_HOOKS_DIR come from test_helper's setup().

# assert_hook <hook> <text> - the shared hook holds exactly <text> and a newline.
assert_hook() {
    printf '%s\n' "$2" | cmp - "$VPD_JFFS_SCRIPTS_DIR/$1"
}

# shared_hook <hook> - the shared hook as stdin has it, mode 755.
shared_hook() {
    mkdir -p "$VPD_JFFS_SCRIPTS_DIR"
    cat > "$VPD_JFFS_SCRIPTS_DIR/$1"
    chmod 755 "$VPD_JFFS_SCRIPTS_DIR/$1"
}

@test "platform_hooks_install: the RT-AX86U's firewall-start keeps guest-lan-port" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    { cat "$VPD_HOOKS_DIR/legacy/firewall-start.v0.14.0"; echo '/jffs/scripts/guest-lan-port &'; } | shared_hook firewall-start
    run _merlin_hook_ensure firewall-start
    assert_success
    assert_output "ok: $f: updated (v0.14.0 body removed, line added)"
    assert_hook firewall-start "$(printf '#!/bin/sh\n%s\n/jffs/scripts/guest-lan-port &' "$(_merlin_hook_line firewall-start)")"
}

@test "platform_hooks_install: creates the missing hooks and an empty one: #!/bin/sh and the line, mode 755" {
    load_platform
    : | shared_hook wan-event
    run platform_hooks_install
    assert_success
    [ "${#lines[@]}" -eq 3 ]
    local h
    for h in firewall-start wan-event nat-start; do
        assert_line "ok: $VPD_JFFS_SCRIPTS_DIR/$h: created"
        [ -x "$VPD_JFFS_SCRIPTS_DIR/$h" ]
        assert_hook "$h" "$(printf '#!/bin/sh\n%s' "$(_merlin_hook_line "$h")")"
    done
}

@test "platform_hooks_install: a hook left without the line fails the run, the others are still done" {
    load_platform
    echo 'echo hi' | shared_hook wan-event
    run platform_hooks_install
    assert_failure
    assert_line "ok: $VPD_JFFS_SCRIPTS_DIR/firewall-start: created"
    assert_line "ok: $VPD_JFFS_SCRIPTS_DIR/nat-start: created"
    assert_line --partial "warn: $VPD_JFFS_SCRIPTS_DIR/wan-event: left alone"
}

@test "_merlin_hook_line: calls our hook with the firmware's arguments and its shell, marked" {
    load_platform
    run _merlin_hook_line nat-start
    assert_output "[ -x $VPD_HOOKS_DIR/nat-start ] && VPD_HOOK_SHELL=\$\$ $VPD_HOOKS_DIR/nat-start \"\$@\" # vpn-director"
}

@test "_merlin_hook_ensure: every earlier body of every hook is recognized" {
    load_platform
    local body name h v n=0
    for body in "$VPD_HOOKS_DIR"/legacy/*; do
        name=${body##*/}
        h=${name%%.v*}
        v=${name#"$h".}
        { cat "$body"; echo 'echo mine'; } | shared_hook "$h"
        run _merlin_hook_ensure "$h"
        assert_success
        assert_output "ok: $VPD_JFFS_SCRIPTS_DIR/$h: updated ($v body removed, line added)"
        assert_hook "$h" "$(printf '#!/bin/sh\n%s\necho mine' "$(_merlin_hook_line "$h")")"
        n=$((n + 1))
    done
    [ "$n" -eq 9 ]
}

@test "_merlin_hook_ensure: other add-ons' lines before and after an earlier body stay in place" {
    load_platform
    local skynet='sh /jffs/scripts/firewall start skynetloc=/tmp/mnt/sda1/skynet # Skynet Firewall Addition'
    local other='/jffs/addons/x/x.sh firewall # Added by X'
    { echo '#!/bin/sh'; echo "$skynet"; tail -n +2 "$VPD_HOOKS_DIR/legacy/firewall-start.v0.3.0"; echo "$other"; } |
        shared_hook firewall-start
    run _merlin_hook_ensure firewall-start
    assert_success
    assert_output "ok: $VPD_JFFS_SCRIPTS_DIR/firewall-start: updated (v0.3.0 body removed, line added)"
    assert_hook firewall-start "$(printf '#!/bin/sh\n%s\n%s\n%s' "$(_merlin_hook_line firewall-start)" "$skynet" "$other")"
}

@test "_merlin_hook_ensure: an edited earlier body stays, with a warning" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start" edited
    sed 's/--wait apply/apply/' "$VPD_HOOKS_DIR/legacy/firewall-start.v0.14.0" | shared_hook firewall-start
    edited=$(tail -n +2 "$f")
    run _merlin_hook_ensure firewall-start
    assert_success
    assert_line --index 0 "ok: $f: updated (line added)"
    assert_line --index 1 "warn: $f: still calls vpn-director.sh outside the vpn-director line; remove that call by hand"
    assert_hook firewall-start "$(printf '#!/bin/sh\n%s\n%s' "$(_merlin_hook_line firewall-start)" "$edited")"
}

@test "_merlin_hook_ensure: CRLF line ends leave an earlier body unrecognized" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/wan-event"
    sed 's/$/\r/' "$VPD_HOOKS_DIR/legacy/wan-event.v0.14.0" | shared_hook wan-event
    run _merlin_hook_ensure wan-event
    assert_success
    assert_line --index 0 "ok: $f: updated (line added)"
    assert_line --index 1 "warn: $f: still calls vpn-director.sh outside the vpn-director line; remove that call by hand"
    # The body is all there, line by line.
    run grep -c $'\r$' "$f"
    assert_output "$(wc -l < "$VPD_HOOKS_DIR/legacy/wan-event.v0.14.0")"
}

@test "_merlin_hook_ensure: a second run writes nothing" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/nat-start" before
    run _merlin_hook_ensure nat-start
    assert_success
    touch -d '2000-01-01 00:00' "$f"
    before=$(stat -c '%i %Y' "$f")
    run _merlin_hook_ensure nat-start
    assert_success
    assert_output "ok: $f: unchanged"
    [ "$(stat -c '%i %Y' "$f")" = "$before" ]
}

@test "_merlin_hook_ensure: replaces an old form of the line and drops duplicates" {
    load_platform
    local line
    line=$(_merlin_hook_line firewall-start)
    printf '#!/bin/sh\n%s\necho x\n%s\n' '/old/path "$@" # vpn-director' "$line" | shared_hook firewall-start
    run _merlin_hook_ensure firewall-start
    assert_success
    assert_output "ok: $VPD_JFFS_SCRIPTS_DIR/firewall-start: updated (line replaced, duplicate lines removed)"
    assert_hook firewall-start "$(printf '#!/bin/sh\n%s\necho x' "$line")"
}

@test "_merlin_hook_ensure: makes a hook with the line executable" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/wan-event"
    printf '#!/bin/sh\n%s\n' "$(_merlin_hook_line wan-event)" | shared_hook wan-event
    chmod 644 "$f"
    run _merlin_hook_ensure wan-event
    assert_success
    assert_output "ok: $f: updated (made executable)"
    [ -x "$f" ]
}

# Merlin runs no hook that is not executable: one that holds someone else's
# commands is off, and turning it on is not ours to do.
@test "_merlin_hook_ensure: a non-executable hook with someone else's commands is not made executable" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    printf '#!/bin/sh\n/jffs/scripts/disabled-thing start\n' | shared_hook firewall-start
    chmod 644 "$f"
    run _merlin_hook_ensure firewall-start
    assert_failure
    assert_line --index 0 "ok: $f: updated (line added)"
    assert_line --index 1 "warn: $f: not executable, so the firmware runs nothing in it; chmod +x it if its commands should run, then run vpn-director.sh hooks"
    [ "$(stat -c %a "$f")" = 644 ]
    assert_hook firewall-start "$(printf '#!/bin/sh\n%s\n/jffs/scripts/disabled-thing start' "$(_merlin_hook_line firewall-start)")"
}

@test "_merlin_hook_ensure: a rewrite keeps the hook's own mode" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    { cat "$VPD_HOOKS_DIR/legacy/firewall-start.v0.14.0"; echo '/jffs/scripts/guest-lan-port &'; } | shared_hook firewall-start
    chmod 700 "$f"
    run _merlin_hook_ensure firewall-start
    assert_success
    [ "$(stat -c %a "$f")" = 700 ]
}

@test "_merlin_hook_ensure: leaves a file without #! alone" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    echo 'echo hi' | shared_hook firewall-start
    run _merlin_hook_ensure firewall-start
    assert_failure
    assert_output "warn: $f: left alone: no #! line, so the firmware does not run it; add #!/bin/sh as its first line, then run vpn-director.sh hooks"
    assert_hook firewall-start 'echo hi'
}

@test "_merlin_hook_ensure: a hook it cannot write stays as it was" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    printf '#!/bin/sh\necho mine\n' | shared_hook firewall-start
    mkdir "$VPD_JFFS_SCRIPTS_DIR/.firewall-start.vpd-new"
    run _merlin_hook_ensure firewall-start
    assert_failure
    assert_output "warn: $f: cannot write it; the hook is as it was"
    assert_hook firewall-start "$(printf '#!/bin/sh\necho mine')"
}

# A rewrite renames a new file over the hook: over a symlink, that would put a
# file in place of the owner's link.
@test "_merlin_hook_ensure: a symlinked hook that needs a rewrite is left alone" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start" target="$BATS_TEST_TMPDIR/my-firewall-start"
    mkdir -p "$VPD_JFFS_SCRIPTS_DIR"
    printf '#!/bin/sh\necho mine\n' > "$target"
    chmod 755 "$target"
    ln -s "$target" "$f"
    run _merlin_hook_ensure firewall-start
    assert_failure
    assert_output "warn: $f: left alone: it is a symlink, which a rewrite would replace with a file; put this line after the #! line of the file it points to: $(_merlin_hook_line firewall-start)"
    [ -L "$f" ]
    printf '#!/bin/sh\necho mine\n' | cmp - "$target"
}

@test "platform_hooks_install: the earlier bodies are the released ones" {
    local entry want=(
        "firewall-start.v0.1.0 210fe9580a0635b872c1328ce63ec7e5"
        "firewall-start.v0.2.0 93b96f381f17389b9ca03c587b43016d"
        "firewall-start.v0.3.0 6aab1db5bcc5370420986edafc50a308"
        "firewall-start.v0.14.0 ff06ffddeaf5fdde38e7c8d2debd6186"
        "wan-event.v0.1.0 2bf722445bc34ddb5863b189e81d6510"
        "wan-event.v0.2.0 fa8ab8ee1cdfbf10a51892e2cf071f57"
        "wan-event.v0.3.0 b05abaa4abd76ee32745e4ed8afcd297"
        "wan-event.v0.14.0 a8379d20a893911e99af5522b938bd85"
        "nat-start.v0.22.0 35fc4470a4f7edc1eb34ab5daac2e069"
    )
    for entry in "${want[@]}"; do
        [ "$(md5sum < "$VPD_HOOKS_DIR/legacy/${entry% *}" | cut -d' ' -f1)" = "${entry#* }" ]
    done
    [ "$(ls "$VPD_HOOKS_DIR/legacy" | wc -l)" -eq 9 ]
}

# An update's log goes with the update: the status is where an earlier body
# the update could not cut out stays visible.
@test "platform_hooks_status: a vpn-director.sh call left beside the line is reported" {
    load_platform
    { echo '#!/bin/sh'; _merlin_hook_line firewall-start
      tail -n +2 "$VPD_HOOKS_DIR/legacy/firewall-start.v0.14.0" | sed 's/--wait apply/apply/'; } | shared_hook firewall-start
    run platform_hooks_status
    assert_success
    assert_line "$VPD_JFFS_SCRIPTS_DIR/firewall-start: vpn-director line in place; still calls vpn-director.sh outside it - remove that call by hand"
}

@test "platform_hooks_status: a non-executable hook with someone else's commands asks for chmod" {
    load_platform
    printf '#!/bin/sh\n%s\necho mine\n' "$(_merlin_hook_line nat-start)" | shared_hook nat-start
    chmod 644 "$VPD_JFFS_SCRIPTS_DIR/nat-start"
    run platform_hooks_status
    assert_success
    assert_line "$VPD_JFFS_SCRIPTS_DIR/nat-start: not executable, so the firmware runs nothing in it; chmod +x it if its commands should run"
}

@test "platform_hooks_status: in place, not executable, no line" {
    load_platform
    printf '#!/bin/sh\n%s\n' "$(_merlin_hook_line firewall-start)" | shared_hook firewall-start
    printf '#!/bin/sh\n%s\n' "$(_merlin_hook_line wan-event)" | shared_hook wan-event
    chmod 644 "$VPD_JFFS_SCRIPTS_DIR/wan-event"
    run platform_hooks_status
    assert_success
    assert_output "$VPD_JFFS_SCRIPTS_DIR/firewall-start: vpn-director line in place
$VPD_JFFS_SCRIPTS_DIR/wan-event: not executable; run vpn-director.sh hooks
$VPD_JFFS_SCRIPTS_DIR/nat-start: no vpn-director line; run vpn-director.sh hooks"
}
