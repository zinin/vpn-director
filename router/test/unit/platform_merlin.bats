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

@test "platform_tunnels: only main when rt_tables is missing" {
    load_platform
    RT_TABLES_FILE="$BATS_TEST_TMPDIR/absent" run platform_tunnels
    assert_success
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
