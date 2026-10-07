#!/usr/bin/env bats

load '../test_helper'

# The Keenetic column of the contract. RCI answers come from the curl mock in
# mocks/keenetic (fixtures/keenetic/rci/<path>.json), kernel facts from the ip,
# insmod and iptables mocks, module state from a /proc/modules fixture.
load_platform() {
    export VPD_PLATFORM=keenetic
    export PATH="$TEST_ROOT/mocks/keenetic:$PATH"
    export VPD_PROC_MODULES="$TEST_ROOT/fixtures/keenetic/proc_modules"
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    export VPD_CRON_D="$BATS_TEST_TMPDIR/cron.d"
    export VPD_CRON_INIT="$BATS_TEST_TMPDIR/S10cron"
    mkdir -p "$VPD_MODULES_DIR"
    : > /tmp/bats_curl_calls.log
    : > /tmp/bats_ip_calls.log
    : > /tmp/bats_insmod_calls.log
    # Standalone, without common.sh: that is a supported way to load the
    # contract (configure.sh does it), and it is what keeps a platform
    # function that logs from ever passing this file - log is not defined here.
    source "$LIB_DIR/platform.sh"
}

# with_mock <name> <script body> puts a one-off mock first in PATH.
with_mock() {
    mkdir -p "$BATS_TEST_TMPDIR/mock"
    printf '#!/bin/bash\n%s\n' "$2" > "$BATS_TEST_TMPDIR/mock/$1"
    chmod +x "$BATS_TEST_TMPDIR/mock/$1"
    export PATH="$BATS_TEST_TMPDIR/mock:$PATH"
}

@test "platform_name: keenetic" {
    load_platform
    run platform_name
    assert_output "keenetic"
}

# ------------------------------------------------------------------- RCI

@test "_rci_get: prints the body of an RCI path" {
    load_platform
    run _rci_get show/system
    assert_success
    assert_output --partial '"hostname":"Keenetic-4521"'
    grep -q 'http://localhost:79/rci/show/system' /tmp/bats_curl_calls.log
}

@test "_rci_get: fails and prints nothing for a path NDM does not have" {
    load_platform
    run _rci_get show/interface/Wireguard9
    assert_failure
    refute_output
}

@test "_rci_get: fails when NDM does not answer" {
    load_platform
    BATS_RCI_DOWN=1 run _rci_get show/system
    assert_failure
    refute_output
}

# ---------------------------------------------------------------- WAN / IPv6

@test "platform_wan_if: the device of the IPv4 default route" {
    load_platform
    run platform_wan_if
    assert_success
    assert_output "eth2.4"
}

@test "platform_wan_if: fails and prints nothing without a default route" {
    load_platform
    BATS_IP_DEFAULT_ROUTE4="" run platform_wan_if
    assert_failure
    refute_output
}

@test "platform_ipv6_enabled: 0 without an IPv6 default route, 1 with one" {
    load_platform
    run platform_ipv6_enabled
    assert_output "0"
    BATS_IP_DEFAULT_ROUTE6="default via fe80::1 dev eth2.4 metric 1024" run platform_ipv6_enabled
    assert_output "1"
}

@test "platform_lan_ifaces: br0" {
    load_platform
    run platform_lan_ifaces
    assert_output "br0"
}

# ------------------------------------------------------------- system facts

@test "platform_lan_ip: the first IPv4 address of br0" {
    load_platform
    run platform_lan_ip
    assert_success
    assert_output "192.168.1.1"
}

@test "platform_lan_ip: fails when br0 has no address" {
    load_platform
    : > "$BATS_TEST_TMPDIR/no-addrs"
    BATS_IP_ADDRS_FILE="$BATS_TEST_TMPDIR/no-addrs" run platform_lan_ip
    assert_failure
    refute_output
}

@test "platform_hostname and platform_model come from RCI" {
    load_platform
    run platform_hostname
    assert_success
    assert_output "Keenetic-4521"
    run platform_model
    assert_success
    assert_output "Ultra (NC-1812)"
}

@test "platform_hostname: fails when NDM does not answer" {
    load_platform
    BATS_RCI_DOWN=1 run platform_hostname
    assert_failure
    refute_output
}

@test "platform_password_file: /opt/etc/passwd" {
    load_platform
    run platform_password_file
    assert_output "/opt/etc/passwd"
}

@test "platform_email_supported: 0" {
    load_platform
    run platform_email_supported
    assert_output "0"
}

# ------------------------------------------------------------------ tunnels

@test "platform_tunnels: every OpenVPN and Wireguard interface, then main" {
    load_platform
    run platform_tunnels
    assert_success
    assert_line --index 0 "OpenVPN0"
    assert_line --index 1 "OpenVPN2"
    assert_line --index 2 "Wireguard1"
    assert_line --index 3 "main"
    [ "${#lines[@]}" -eq 4 ]
}

@test "platform_tunnels: only main when NDM does not answer" {
    load_platform
    BATS_RCI_DOWN=1 run platform_tunnels
    assert_success
    assert_output "main"
}

# Every OpenVPN and WireGuard interface RCI lists is one Tunnel Director may
# route through, and its tables are Tunnel Director's own: nothing to explain.
@test "platform_tunnel_unlisted_reason: Keenetic has no reason to give" {
    load_platform
    run platform_tunnel_unlisted_reason OpenVPN0
    assert_failure
    refute_output
    run platform_tunnel_unlisted_reason ovpnc1
    assert_failure
    refute_output
}

@test "platform_tunnel_iface: OpenVPNN is ovpn_brN, WireguardN is nwgN" {
    load_platform
    run platform_tunnel_iface OpenVPN0
    assert_success
    assert_output "ovpn_br0"
    run platform_tunnel_iface Wireguard1
    assert_output "nwg1"
    # Down: nothing to check against, the convention is the answer.
    run platform_tunnel_iface OpenVPN2
    assert_output "ovpn_br2"
}

@test "platform_tunnel_iface: fails when the interface does not carry the address RCI reports" {
    load_platform
    printf 'ovpn_br0 10.9.9.9/24\n' > "$BATS_TEST_TMPDIR/addrs"
    BATS_IP_ADDRS_FILE="$BATS_TEST_TMPDIR/addrs" run platform_tunnel_iface OpenVPN0
    assert_failure
    refute_output
}

@test "platform_tunnel_iface: the convention stands when NDM does not answer" {
    load_platform
    BATS_RCI_DOWN=1 run platform_tunnel_iface OpenVPN0
    assert_success
    assert_output "ovpn_br0"
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
    run platform_tunnel_info OpenVPN0
    assert_success
    assert_line --index 0 "openvpn"
    assert_line --index 1 "1"
    assert_line --index 2 "office-ovpn"
    run platform_tunnel_info Wireguard1
    assert_line --index 0 "wireguard"
    assert_line --index 1 "1"
    assert_line --index 2 "wg-home"
    run platform_tunnel_info OpenVPN2
    assert_line --index 1 "0"
    assert_line --index 2 "spare-ovpn"
}

@test "platform_tunnel_info: fails for an unknown id and when NDM does not answer" {
    load_platform
    run platform_tunnel_info OpenVPN7
    assert_failure
    refute_output
    run platform_tunnel_info eth0
    assert_failure
    BATS_RCI_DOWN=1 run platform_tunnel_info OpenVPN0
    assert_failure
    refute_output
}

@test "platform_tunnel_table: 2000 + idx; main stays main" {
    load_platform
    run platform_tunnel_table OpenVPN0 0
    assert_output "2000"
    run platform_tunnel_table Wireguard1 3
    assert_output "2003"
    run platform_tunnel_table main 5
    assert_output "main"
}

@test "platform_tunnel_table: an id that is not a tunnel or an idx that is not a number fails" {
    load_platform
    run platform_tunnel_table eth0 0
    assert_failure
    refute_output
    run platform_tunnel_table OpenVPN0 x
    assert_failure
    refute_output
    run platform_tunnel_table
    assert_failure
}

@test "_keenetic_first_host: the first host of the tunnel subnet" {
    load_platform
    run _keenetic_first_host 10.73.149.113 255.255.255.0
    assert_output "10.73.149.1"
    run _keenetic_first_host 10.8.0.6 255.255.255.252
    assert_output "10.8.0.5"
    run _keenetic_first_host 10.73.149.113 garbage
    assert_failure
    refute_output
}

# Digits alone are not an octet. Without the range check the shift arithmetic
# would happily fold 300 into a neighbouring octet and print a "first host".
@test "_keenetic_first_host: rejects an octet above 255" {
    load_platform
    run _keenetic_first_host 10.0.0.1 255.255.300.0
    assert_failure
    refute_output
    run _keenetic_first_host 10.0.300.1 255.255.255.0
    assert_failure
    refute_output
}

@test "platform_tunnel_route: OpenVPN via the subnet's first host, Wireguard by device" {
    load_platform
    run platform_tunnel_route OpenVPN0
    assert_success
    assert_output "default via 10.73.149.1 dev ovpn_br0"
    run platform_tunnel_route Wireguard1
    assert_output "default dev nwg1"
}

@test "platform_tunnel_route: a configured gateway replaces the computed one" {
    load_platform
    run platform_tunnel_route OpenVPN0 10.73.149.254
    assert_output "default via 10.73.149.254 dev ovpn_br0"
}

@test "platform_tunnel_route: nothing while the tunnel is down" {
    load_platform
    run platform_tunnel_route OpenVPN2
    assert_failure
    refute_output
}

@test "platform_tunnel_route_ensure: replaces the route in the tunnel's table, idempotently" {
    load_platform
    run platform_tunnel_route_ensure OpenVPN0 0
    assert_success
    run platform_tunnel_route_ensure OpenVPN0 0
    assert_success
    assert_equal "$(grep -c 'ip route replace default via 10.73.149.1 dev ovpn_br0 table 2000' /tmp/bats_ip_calls.log)" 2
    run platform_tunnel_route_ensure Wireguard1 1 ""
    assert_success
    grep -q 'ip route replace default dev nwg1 table 2001' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_route_ensure: passes the gateway on and fails for a down tunnel" {
    load_platform
    run platform_tunnel_route_ensure OpenVPN0 0 10.73.149.254
    assert_success
    grep -q 'ip route replace default via 10.73.149.254 dev ovpn_br0 table 2000' /tmp/bats_ip_calls.log
    run platform_tunnel_route_ensure OpenVPN2 2
    assert_failure
    refute grep -q 'table 2002' /tmp/bats_ip_calls.log
}

@test "platform_tunnel_route_ensure and _table_release: main touches no table" {
    load_platform
    run platform_tunnel_route_ensure main 4
    assert_success
    run platform_tunnel_table_release main 4
    assert_success
    [ ! -s /tmp/bats_ip_calls.log ]
}

@test "platform_tunnel_table_release: flushes the table, also one that was never ensured" {
    load_platform
    run platform_tunnel_table_release OpenVPN0 0
    assert_success
    grep -q 'ip route flush table 2000' /tmp/bats_ip_calls.log
    run platform_tunnel_table_release Wireguard1 7
    assert_success
    grep -q 'ip route flush table 2007' /tmp/bats_ip_calls.log
}

# KeeneticOS takes an established forwarded flow into a fast path that runs
# before mangle and never returns to it, so Tunnel Director's MARK is never
# applied and the traffic leaves through the WAN. `-j PPE` sets ct->fast_ext,
# which is what both the fastnat and the fastroute entry tests check.
@test "platform_tunnel_offload_target: PPE, the target that opts a flow out of the fast path" {
    load_platform
    run platform_tunnel_offload_target
    assert_success
    assert_output "PPE"
}

@test "platform_vpn_endpoints: the OpenVPN server and every Wireguard peer, one per line" {
    load_platform
    run platform_vpn_endpoints
    assert_success
    assert_line --index 0 "203.0.113.7"
    assert_line --index 1 "198.51.100.9"
    [ "${#lines[@]}" -eq 2 ]
}

@test "platform_vpn_endpoints: preserves every Wireguard peer in a multi-peer array" {
    load_platform
    mkdir -p "$BATS_TEST_TMPDIR/rci/show"
    jq '.Wireguard1.wireguard.peer[0] as $peer |
        .Wireguard1.wireguard.peer = [
            ($peer + {"public-key":"PEER_ONE", "remote":"wg-one.example.test"}),
            ($peer + {"public-key":"PEER_TWO", "remote":"wg-two.example.test"}),
            ($peer + {"public-key":"PEER_THREE", "remote":"wg-three.example.test"})
        ]' "$TEST_ROOT/fixtures/keenetic/rci/show/interface.json" \
        > "$BATS_TEST_TMPDIR/rci/show/interface.json"

    BATS_RCI_FIXTURES="$BATS_TEST_TMPDIR/rci" run platform_vpn_endpoints

    assert_success
    assert_output $'203.0.113.7\nwg-one.example.test\nwg-two.example.test\nwg-three.example.test'
    assert_equal "${#lines[@]}" 4
}

# RCI hands a single Wireguard peer back as an object where several come as an
# array; the fixture has the array, this is the other shape.
@test "platform_vpn_endpoints: a lone Wireguard peer arrives as an object" {
    load_platform
    mkdir -p "$BATS_TEST_TMPDIR/rci/show"
    jq '.Wireguard1.wireguard.peer = .Wireguard1.wireguard.peer[0]' \
        "$TEST_ROOT/fixtures/keenetic/rci/show/interface.json" \
        > "$BATS_TEST_TMPDIR/rci/show/interface.json"
    BATS_RCI_FIXTURES="$BATS_TEST_TMPDIR/rci" run platform_vpn_endpoints
    assert_success
    assert_line --index 0 "203.0.113.7"
    assert_line --index 1 "198.51.100.9"
    [ "${#lines[@]}" -eq 2 ]
}

@test "platform_vpn_endpoints: fails when NDM does not answer" {
    load_platform
    BATS_RCI_DOWN=1 run platform_vpn_endpoints
    assert_failure
    refute_output
}

# ------------------------------------------------------------ modules / cron

@test "platform_load_module: a module /proc/modules lists needs no insmod" {
    load_platform
    run platform_load_module xt_TPROXY
    assert_success
    [ ! -s /tmp/bats_insmod_calls.log ]
}

@test "platform_load_module: insmods the .ko of a module that is not loaded" {
    load_platform
    : > "$VPD_MODULES_DIR/xt_comment.ko"
    run platform_load_module xt_comment
    assert_success
    grep -qF "insmod $VPD_MODULES_DIR/xt_comment.ko" /tmp/bats_insmod_calls.log
}

@test "platform_load_module: fails without insmod when the .ko is missing" {
    load_platform
    run platform_load_module xt_comment
    assert_failure
    refute_output
    [ ! -s /tmp/bats_insmod_calls.log ]
}

# fake_cron_init <check rc> writes a stand-in S10cron that records its verb.
fake_cron_init() {
    cat > "$VPD_CRON_INIT" <<EOF
#!/bin/sh
echo "\$1" >> "$BATS_TEST_TMPDIR/cron-init.calls"
[ "\$1" = check ] && exit $1
exit 0
EOF
    chmod +x "$VPD_CRON_INIT"
}

@test "platform_cron_add: writes a cron.d job for root and starts a cron that is not running" {
    load_platform
    fake_cron_init 1
    run platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    assert_success
    run cat "$VPD_CRON_D/vpn_director_update"
    assert_line --index 0 "PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    assert_line --index 1 "0 3 * * * root /opt/vpn-director/vpn-director.sh update"
    run cat "$BATS_TEST_TMPDIR/cron-init.calls"
    assert_line --index 0 "check"
    assert_line --index 1 "start"
}

# Vixie cron refuses a crontab that is not 0600: it logs "(*system*) BAD FILE
# MODE" to syslog at startup and ignores the file, so the job never runs and
# nothing anywhere says why. /opt/etc/crontab, which the package ships and
# which does run, is 0600 for the same reason. Measured on KeeneticOS 5.1.5.
@test "platform_cron_add: writes the job 0600, the only mode Vixie cron accepts" {
    load_platform
    fake_cron_init 0
    platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    run stat -c '%a' "$VPD_CRON_D/vpn_director_update"
    assert_success
    assert_output "600"
}

# A job started from cron.d inherits cron's own PATH, /usr/bin:/bin, where none
# of Entware's tools are: the update would die on its first jq or ipset.
@test "platform_cron_add: the job carries the PATH Entware's tools need" {
    load_platform
    fake_cron_init 0
    platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    run cat "$VPD_CRON_D/vpn_director_update"
    assert_success
    assert_line --index 0 --partial "/opt/sbin"
    assert_line --index 0 --partial "/opt/bin"
}

@test "platform_cron_add: leaves a running cron alone" {
    load_platform
    fake_cron_init 0
    run platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    assert_success
    run cat "$BATS_TEST_TMPDIR/cron-init.calls"
    assert_output "check"
}

@test "platform_cron_add: fails when the cron package is not installed, the job file written" {
    load_platform
    run platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    assert_failure
    [ -f "$VPD_CRON_D/vpn_director_update" ]
    # Contract (lib/platform.sh): a function that cannot answer prints nothing
    # and returns 1, and none of them logs. What to do about it is
    # platform_cron_requirements' answer and cmd_cron's line.
    refute_output
}

@test "platform_cron_requirements: names the package that would run the job" {
    load_platform
    run platform_cron_requirements
    assert_success
    assert_output --partial "opkg install cron"
    # The job file is written whether or not cron is there, so it starts
    # running by itself once the package is installed - say where it is.
    assert_output --partial "$VPD_CRON_D"
}

@test "platform_cron_requirements: nothing to say once cron is installed" {
    load_platform
    fake_cron_init 0
    run platform_cron_requirements
    assert_failure
    refute_output
}

@test "platform_cron_del: removes the job; a job that is not there is fine" {
    load_platform
    fake_cron_init 0
    platform_cron_add vpn_director_update "0 3 * * *" "/opt/vpn-director/vpn-director.sh update"
    run platform_cron_del vpn_director_update
    assert_success
    [ ! -e "$VPD_CRON_D/vpn_director_update" ]
    run platform_cron_del update_ipsets
    assert_success
}

# ------------------------------------------------------------- firewall bits

@test "platform_tproxy_extra_rules apply: the accept rule at position 1 of mangle INPUT" {
    load_platform
    : > /tmp/bats_iptables_calls.log
    run platform_tproxy_extra_rules apply
    assert_success
    grep -qF -- '-t mangle -C INPUT -m mark --mark 0x100/0x100 -j ACCEPT' /tmp/bats_iptables_calls.log
    grep -qF -- '-t mangle -I INPUT 1 -m mark --mark 0x100/0x100 -j ACCEPT' /tmp/bats_iptables_calls.log
}

@test "platform_tproxy_extra_rules: the configured mark replaces the default" {
    load_platform
    : > /tmp/bats_iptables_calls.log
    run platform_tproxy_extra_rules apply 0x200/0x200
    assert_success
    grep -qF -- '-I INPUT 1 -m mark --mark 0x200/0x200 -j ACCEPT' /tmp/bats_iptables_calls.log
}

@test "platform_tproxy_extra_rules stop: deletes the accept rule and succeeds" {
    load_platform
    : > /tmp/bats_iptables_calls.log
    run platform_tproxy_extra_rules stop
    assert_success
    grep -qF -- '-t mangle -D INPUT -m mark --mark 0x100/0x100 -j ACCEPT' /tmp/bats_iptables_calls.log
}

@test "platform_tproxy_extra_rules: rejects a verb that is not apply or stop" {
    load_platform
    run platform_tproxy_extra_rules
    assert_failure
    refute_output
    run platform_tproxy_extra_rules aply
    assert_failure
}

@test "platform_prerouting_base_pos: 2, right after XRAY_TPROXY and ahead of every _NDM_ jump" {
    load_platform
    run platform_prerouting_base_pos
    assert_output "2"
}
