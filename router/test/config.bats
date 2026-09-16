#!/usr/bin/env bats

load 'test_helper'

# ============================================================================
# config.sh: loading
# ============================================================================

@test "config.sh: loads without error" {
    load_config
    # If we get here without error, the test passes
    [[ -n $VPD_CONFIG_FILE ]]
}

# ============================================================================
# config.sh: Tunnel Director variables
# ============================================================================

@test "config.sh: exports TUN_DIR_TUNNELS_JSON" {
    load_config
    [ -n "$TUN_DIR_TUNNELS_JSON" ]
}

@test "config.sh: TUN_DIR_TUNNELS_JSON contains tunnel data" {
    load_config
    echo "$TUN_DIR_TUNNELS_JSON" | jq -e '.wgc1.clients[0]' >/dev/null
}

@test "config.sh: exports TUN_DIR_CHAIN (not prefix)" {
    load_config
    [ "$TUN_DIR_CHAIN" = "TUN_DIR" ]
}

@test "config.sh: sets IPS_BDR_DIR" {
    load_config
    [[ -n $IPS_BDR_DIR ]]
}

# ============================================================================
# config.sh: Xray variables
# ============================================================================

@test "config.sh: sets XRAY_CLIENTS" {
    load_config
    # XRAY_CLIENTS can be empty or have values
    [[ -n $XRAY_CLIENTS ]] || [[ $XRAY_CLIENTS == "" ]]
}

@test "config.sh: sets XRAY_SERVERS" {
    load_config
    [[ -n $XRAY_SERVERS ]] || [[ $XRAY_SERVERS == "" ]]
}

@test "config.sh: sets XRAY_TPROXY_PORT" {
    load_config
    [[ $XRAY_TPROXY_PORT == "12345" ]]
}

@test "config.sh: sets XRAY_CHAIN" {
    load_config
    [[ $XRAY_CHAIN == "XRAY_TPROXY" ]]
}

@test "config: XRAY_EXCLUDE_IPS is loaded from config" {
    load_config
    [[ "$XRAY_EXCLUDE_IPS" == *"5.6.7.8"* ]]
    [[ "$XRAY_EXCLUDE_IPS" == *"10.20.0.0/16"* ]]
}

@test "config: XRAY_BYPASS_IPSET is loaded from config" {
    load_config
    [[ "$XRAY_BYPASS_IPSET" == "TPROXY_BYPASS" ]]
}

# ============================================================================
# config.sh: Advanced Tunnel Director variables
# ============================================================================

@test "config.sh: sets TUN_DIR_PREF_BASE" {
    load_config
    [[ $TUN_DIR_PREF_BASE == "16384" ]]
}

# ============================================================================
# config.sh: error handling
# ============================================================================

@test "config.sh: fails on missing config file" {
    export VPD_CONFIG_FILE="/nonexistent/config.json"
    run source "$LIB_DIR/config.sh"
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "Config not found"
}

@test "config.sh: fails on invalid JSON" {
    local tmp_invalid="/tmp/bats_invalid_config.json"
    echo "not valid json {" > "$tmp_invalid"
    export VPD_CONFIG_FILE="$tmp_invalid"
    run source "$LIB_DIR/config.sh"
    assert_failure
    assert_output --partial "ERROR"
    assert_output --partial "Invalid JSON"
    rm -f "$tmp_invalid"
}

# ============================================================================
# config.sh: paused_clients filtering
# ============================================================================

@test "config: paused_clients filters XRAY_CLIENTS" {
    local tmp_cfg="/tmp/bats_test_paused_config.json"
    jq '.paused_clients = ["192.168.1.100"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [[ "$XRAY_CLIENTS" != *"192.168.1.100"* ]]
    rm -f "$tmp_cfg"
}

@test "config: paused_clients filters TUN_DIR_TUNNELS_JSON clients" {
    local tmp_cfg="/tmp/bats_test_paused_td_config.json"
    jq '.paused_clients = ["192.168.50.0/24"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    local clients
    clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1.clients[]')
    [[ "$clients" != *"192.168.50.0/24"* ]]
    rm -f "$tmp_cfg"
}

@test "config: empty paused_clients changes nothing" {
    local tmp_cfg="/tmp/bats_test_empty_paused_config.json"
    jq '.paused_clients = []' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [[ "$XRAY_CLIENTS" == *"192.168.1.100"* ]]
    rm -f "$tmp_cfg"
}

@test "config: missing paused_clients changes nothing" {
    load_config
    [[ "$XRAY_CLIENTS" == *"192.168.1.100"* ]]
    local clients
    clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1.clients[]')
    [[ "$clients" == *"192.168.50.0/24"* ]]
}

@test "config: paused_clients filtering preserves tunnel exclude field" {
    local tmp_cfg="/tmp/bats_test_paused_exclude_config.json"
    jq '.paused_clients = ["192.168.50.0/24"]' "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    local exclude
    exclude=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1.exclude[]')
    [[ "$exclude" == *"ru"* ]]
    rm -f "$tmp_cfg"
}

# Spec 12: a tunnel gateway that is not a dotted IPv4 is dropped at load so
# it never reaches ip route. The rest of the tunnel (and the rest of the
# config) still loads; apply-time _tunnel_gateway is the belt.
#
# Entware jq has no Oniguruma: test/match/sub abort `source config.sh` under
# set -e as soon as any gateway string is present, valid IPv4 included.
# Host bats cannot catch that (this workstation's jq has regex). Pin the
# jq programs themselves.
@test "config.sh: jq programs do not call regex functions Keenetic jq lacks" {
    run grep -nE 'test\(|match\(|sub\(' "$LIB_DIR/config.sh"
    assert_failure
    refute_output
}

# The same pin over every shell file that ships to a router. Entware jq on
# KeeneticOS is built without Oniguruma, so test/match/capture/scan/splits abort
# any jq program under set -e on the device while a workstation's jq accepts
# them - which is how 0d5dc6f passed green here and failed on the router. This is
# a source pin, not a run under the device's jq. sub(/gsub( stay with the
# config.sh-scoped test above, because awk shares those names and common.sh uses
# awk's sub().
@test "shipped shell does not call jq regex functions Keenetic jq lacks" {
    run grep -rnE '(^|[^A-Za-z_])(test|match|capture|scan|splits)\(' \
        "$SCRIPTS_DIR" "$PROJECT_ROOT/../install.sh"
    assert_failure
    refute_output
}

@test "config.sh: drops a non-IPv4 tunnel gateway and keeps a valid one" {
    load_common
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-gateway.json"
    jq '.tunnel_director.tunnels.wgc1.gateway = "10.8.0.1" |
        .tunnel_director.tunnels.ovpnc1 = {"clients":["192.168.1.5"],"exclude":["ru"],"gateway":"not-an-ip"}' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1.gateway')" = "10.8.0.1" ]
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.ovpnc1 | has("gateway")')" = "false" ]
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.ovpnc1.clients[0]')" = "192.168.1.5" ]
    grep -q "WARN.*invalid gateway 'not-an-ip'" "$LOG_FILE"
}

@test "config.sh: failover tunnel is first in TUN_DIR_TUNNELS_JSON" {
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-failover-order.json"
    jq '.tunnel_director.tunnels = {
            "main": {"clients":["192.168.50.0/24"],"exclude":[]},
            "ovpnc2": {"clients":["192.168.1.3","192.168.1.8"],"exclude":[]}
        } |
        .xray.failover = {"tunnel":"ovpnc2","clients":["192.168.1.8"]}' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r 'keys_unsorted[0]')" = "ovpnc2" ]
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r 'keys_unsorted[1]')" = "main" ]
}

@test "config.sh: drops a gateway whose octet is above 255" {
    local tmp_cfg="$BATS_TEST_TMPDIR/vpn-director-gateway-octet.json"
    jq '.tunnel_director.tunnels.wgc1.gateway = "10.8.0.999"' \
        "$TEST_ROOT/fixtures/vpn-director.json" > "$tmp_cfg"
    export VPD_CONFIG_FILE="$tmp_cfg"
    source "$LIB_DIR/config.sh"
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1 | has("gateway")')" = "false" ]
    [ "$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r '.wgc1.clients[0]')" = "192.168.50.0/24" ]
}
