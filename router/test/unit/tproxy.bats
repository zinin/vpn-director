#!/usr/bin/env bats

load '../test_helper'

# Note: load_tproxy_module is provided by test_helper.bash
# It loads: common.sh, config.sh, ipset.sh, firewall.sh, tproxy.sh

# ============================================================================
# _tproxy_check_module - check xt_TPROXY kernel module
# ============================================================================

@test "_tproxy_check_module: returns success when module loaded" {
    load_tproxy_module
    run _tproxy_check_module
    assert_success
}

@test "_tproxy_check_module: attempts modprobe if not loaded" {
    load_tproxy_module
    # Clean call log first
    : > /tmp/bats_modprobe_calls.log 2>/dev/null || true
    run _tproxy_check_module
    assert_success
}

# ============================================================================
# _tproxy_resolve_exclude_set - resolve exclusion ipset name
# ============================================================================

@test "_tproxy_resolve_exclude_set: returns set name for existing ipset" {
    load_tproxy_module
    result=$(_tproxy_resolve_exclude_set "ru")
    [ "$result" = "ru" ]
}

@test "_tproxy_resolve_exclude_set: prefers _ext variant if exists" {
    load_tproxy_module
    # Mock ipset returns ru_ext as existing
    result=$(_tproxy_resolve_exclude_set "ru")
    # Should return either ru or ru_ext depending on mock
    [ -n "$result" ]
}

@test "_tproxy_resolve_exclude_set: fails for non-existing set" {
    load_tproxy_module
    run _tproxy_resolve_exclude_set "nonexistent_xyz"
    assert_failure
}

# ============================================================================
# _tproxy_check_required_ipsets - fail-safe check
# ============================================================================

@test "_tproxy_check_required_ipsets: returns success when all sets exist" {
    load_tproxy_module
    run _tproxy_check_required_ipsets
    assert_success
}

# ============================================================================
# tproxy_get_required_ipsets - return list of exclude ipsets
# ============================================================================

@test "tproxy_get_required_ipsets: returns exclude sets" {
    load_tproxy_module
    run tproxy_get_required_ipsets
    assert_success
    # From fixture: XRAY_EXCLUDE_SETS should contain "ru"
    assert_output --partial "ru"
}

@test "tproxy_get_required_ipsets: handles empty exclude sets" {
    # This test uses a subshell to override XRAY_EXCLUDE_SETS
    load_common
    source "$LIB_DIR/firewall.sh"
    # Manually set XRAY_EXCLUDE_SETS before loading config
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    # Source tproxy with override
    export XRAY_EXCLUDE_SETS=""
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tproxy.sh" --source-only

    result=$(tproxy_get_required_ipsets)
    [ -z "$result" ]
}

@test "tproxy_get_required_ipsets: drops an unknown country code with a WARN" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    source "$LIB_DIR/ipset.sh" --source-only
    export XRAY_EXCLUDE_SETS="ru xx US"
    source "$LIB_DIR/tproxy.sh" --source-only

    result=$(tproxy_get_required_ipsets 2>/dev/null)
    [ "$result" = $'ru\nus' ]
    grep -q "WARN.*Ignoring invalid country code 'xx' in xray.exclude_sets" "$LOG_FILE"
}

@test "_tproxy_check_required_ipsets: ignores an unknown country code" {
    load_common
    source "$LIB_DIR/firewall.sh"
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    source "$LIB_DIR/ipset.sh" --source-only
    export XRAY_EXCLUDE_SETS="ru xx"
    source "$LIB_DIR/tproxy.sh" --source-only

    # The ipset mock knows ru but not xx; before the filter this returned 1.
    run _tproxy_check_required_ipsets
    assert_success
}

@test "tproxy_apply: applies rules when xray.exclude_sets holds an unknown code" {
    # config.sh marks XRAY_EXCLUDE_SETS readonly, so feed the bad code through a
    # copy of the fixture instead of overriding the variable afterwards.
    load_common
    jq '.xray.exclude_sets = ["ru", "xx"]' "$TEST_ROOT/fixtures/vpn-director.json" \
        > "$BATS_TEST_TMPDIR/vpn-director.json"
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tproxy.sh" --source-only

    run tproxy_apply
    assert_success
    assert_output --partial "Added exclusion for ipset: ru"
    assert_output --partial "Xray TPROXY routing applied successfully"
    refute_output --partial "aborting"
}

# ============================================================================
# tproxy_status - display status information
# ============================================================================

@test "tproxy_status: outputs status header" {
    load_tproxy_module
    run tproxy_status
    assert_success
    assert_output --partial "Xray TPROXY Status"
}

@test "tproxy_status: shows kernel module section" {
    load_tproxy_module
    run tproxy_status
    assert_success
    assert_output --partial "Kernel Module"
}

@test "tproxy_status: shows routing section" {
    load_tproxy_module
    run tproxy_status
    assert_success
    assert_output --partial "Routing"
}

@test "tproxy_status: shows chain section" {
    load_tproxy_module
    run tproxy_status
    assert_success
    assert_output --partial "Iptables Chain"
}

@test "tproxy_status: shows xray process section" {
    load_tproxy_module
    run tproxy_status
    assert_success
    assert_output --partial "Xray Process"
}

# ============================================================================
# tproxy_stop - remove chain and routing
# ============================================================================

@test "tproxy_stop: returns success when no chain exists" {
    load_tproxy_module
    run tproxy_stop
    assert_success
}

@test "tproxy_stop: logs cleanup message" {
    load_tproxy_module
    run tproxy_stop
    assert_success
    assert_output --partial "TPROXY"
}

# ============================================================================
# tproxy_apply - apply TPROXY rules (idempotent)
# ============================================================================

@test "tproxy_apply: returns success" {
    load_tproxy_module
    run tproxy_apply
    assert_success
}

@test "tproxy_apply: does not record ready when TPROXY targets cannot be installed" {
    load_tproxy_module
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    printf 'stale\n' > "$XRAY_TPROXY_READY"
    # The targets only: every rule of the chain names XRAY_TPROXY, and matching
    # on that stopped the setup at rule 1 before a target was ever tried.
    iptables() {
        if [[ $* == *"-j TPROXY"* && $* == *" -A "* ]]; then
            echo "iptables $*" >> /tmp/bats_iptables_calls.log
            return 1
        fi
        command iptables "$@"
    }
    run tproxy_apply
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

# On Keenetic the platform's mangle INPUT accept is what lets proxied HTTPS past
# _NDM_HTTP_INPUT_TLS_. The subscription watch reads ready as "Xray clients can
# leave the fallback tunnel", so an apply without that rule must not publish it.
@test "tproxy_apply: does not record ready when the platform cannot apply its extra rules" {
    load_tproxy_module
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    printf 'stale\n' > "$XRAY_TPROXY_READY"
    platform_tproxy_extra_rules() { return 1; }
    run tproxy_apply
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

# The watch reads the marker as "every Xray client is intercepted" and drops
# their fallback-tunnel membership on it. A client the ipset did not take is not
# intercepted, and with the tunnel gone its traffic leaves through the WAN.
@test "tproxy_apply: does not record ready when a client cannot be added to the ipset" {
    load_tproxy_module
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    printf 'stale\n' > "$XRAY_TPROXY_READY"
    ipset() {
        if [[ $1 == add && $* == *"$XRAY_CLIENTS_IPSET"* ]]; then
            return 1
        fi
        command ipset "$@"
    }
    run tproxy_apply
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

# xray.clients is not validated anywhere, and a repeated address is the one add
# failure that means nothing: the client is already in the set. The adds ask for
# -exist, so a duplicate does not cost the whole LAN its marker.
@test "tproxy_apply: records ready when a client is already in the ipset" {
    load_tproxy_module
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    ipset() {
        if [[ $1 == add && $* != *-exist* ]]; then
            return 1
        fi
        command ipset "$@"
    }
    run tproxy_apply
    assert_success
    [ -f "$XRAY_TPROXY_READY" ]
}

# Nothing validates xray.clients: an IPv6 address from an older Web UI, a typo
# like 192.168.1.1000. The set never takes such an entry, no router client can
# carry it, and withholding the marker for it held every restore of the LAN on
# the fallback tunnel for good.
@test "tproxy_apply: an xray.clients entry that is not an IPv4 address does not withhold ready" {
    load_common
    jq '.xray.clients = ["192.168.1.5", "fd00::10", "192.168.1.1000"]' "$TEST_ROOT/fixtures/vpn-director.json" \
        > "$BATS_TEST_TMPDIR/vpn-director.json"
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tproxy.sh" --source-only
    # The kernel takes neither: hash:net is IPv4, and 1000 is no octet.
    ipset() {
        if [[ $1 == add && ( $* == *fd00::10* || $* == *192.168.1.1000* ) ]]; then
            return 1
        fi
        command ipset "$@"
    }
    run tproxy_apply
    assert_success
    [ -f "$XRAY_TPROXY_READY" ]
    grep -q "WARN.*fd00::10" "$LOG_FILE"
    grep -q "WARN.*192.168.1.1000" "$LOG_FILE"
}

# sync_fw_rule used to report an insert the kernel refused as success: the
# marker then went out for a chain nothing jumped to.
@test "tproxy_apply: a PREROUTING jump that did not go in withholds ready" {
    load_tproxy_module
    iptables() {
        [[ $* == *" -I PREROUTING "* ]] && return 4
        command iptables "$@"
    }
    run tproxy_apply
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

@test "tproxy_apply: records ready when rules are installed" {
    load_tproxy_module
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    run tproxy_apply
    assert_success
    [ -f "$XRAY_TPROXY_READY" ]
}

@test "tproxy_apply: logs application message" {
    load_tproxy_module
    run tproxy_apply
    assert_success
}

@test "tproxy_apply: soft-fails when module unavailable (returns 0)" {
    # Override lsmod/modprobe to simulate module unavailable
    load_common
    source "$LIB_DIR/firewall.sh"
    load_config
    source "$LIB_DIR/ipset.sh" --source-only

    # Create temp mock that always fails
    mkdir -p /tmp/bats_mock_fail
    cat > /tmp/bats_mock_fail/lsmod << 'EOF'
#!/bin/bash
echo ""
EOF
    cat > /tmp/bats_mock_fail/modprobe << 'EOF'
#!/bin/bash
exit 1
EOF
    chmod +x /tmp/bats_mock_fail/lsmod /tmp/bats_mock_fail/modprobe
    export PATH="/tmp/bats_mock_fail:$PATH"

    source "$LIB_DIR/tproxy.sh" --source-only
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/tproxy_ready"
    printf 'stale\n' > "$XRAY_TPROXY_READY"
    run tproxy_apply
    # Should soft-fail (return 0, not fail)
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]

    rm -rf /tmp/bats_mock_fail
}

@test "tproxy_apply: soft-fails when required ipsets missing (returns 0)" {
    load_common
    source "$LIB_DIR/firewall.sh"
    load_config
    source "$LIB_DIR/ipset.sh" --source-only

    # Create temp mock that returns nothing for ipset list
    mkdir -p /tmp/bats_mock_no_ipset
    cat > /tmp/bats_mock_no_ipset/ipset << 'EOF'
#!/bin/bash
exit 1
EOF
    chmod +x /tmp/bats_mock_no_ipset/ipset
    export PATH="/tmp/bats_mock_no_ipset:$PATH"

    source "$LIB_DIR/tproxy.sh" --source-only
    run tproxy_apply
    # Should soft-fail (return 0, not fail)
    assert_success

    rm -rf /tmp/bats_mock_no_ipset
}

# ============================================================================
# _tproxy_init - initialization function
# ============================================================================

@test "_tproxy_init: sets initialized flag" {
    load_tproxy_module
    _tproxy_init
    [ "$_tproxy_initialized" -eq 1 ]
}

@test "_tproxy_init: is idempotent" {
    load_tproxy_module
    _tproxy_init
    local first_call=$_tproxy_initialized
    _tproxy_init
    [ "$_tproxy_initialized" -eq "$first_call" ]
}

# ============================================================================
# Module loading
# ============================================================================

@test "tproxy.sh: can be sourced with --source-only" {
    load_common
    load_config
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/tproxy.sh" --source-only
    # If we get here without error, the test passes
    [ $? -eq 0 ]
}

@test "tproxy.sh: exports expected functions" {
    load_tproxy_module
    # Check that public API functions exist
    declare -f tproxy_status >/dev/null
    declare -f tproxy_apply >/dev/null
    declare -f tproxy_stop >/dev/null
    declare -f tproxy_get_required_ipsets >/dev/null
}

# ============================================================================
# _tproxy_validate_ipv4_cidr - IP/CIDR validation
# ============================================================================

@test "_tproxy_validate_ipv4_cidr: accepts valid IPv4" {
    load_tproxy_module
    run _tproxy_validate_ipv4_cidr "1.2.3.4"
    assert_success
}

@test "_tproxy_validate_ipv4_cidr: accepts valid CIDR" {
    load_tproxy_module
    run _tproxy_validate_ipv4_cidr "10.0.0.0/8"
    assert_success
}

@test "_tproxy_validate_ipv4_cidr: rejects invalid IP" {
    load_tproxy_module
    run _tproxy_validate_ipv4_cidr "256.1.1.1"
    assert_failure
}

@test "_tproxy_validate_ipv4_cidr: rejects invalid CIDR mask" {
    load_tproxy_module
    run _tproxy_validate_ipv4_cidr "10.0.0.0/33"
    assert_failure
}

@test "_tproxy_validate_ipv4_cidr: rejects non-IP string" {
    load_tproxy_module
    run _tproxy_validate_ipv4_cidr "not-an-ip"
    assert_failure
}

# ============================================================================
# _tproxy_setup_bypass_ipset - 3-source ipset assembly
# ============================================================================

@test "_tproxy_setup_bypass_ipset: logs counts per source" {
    load_tproxy_module
    run _tproxy_setup_bypass_ipset
    assert_success
    assert_output --partial "xray"
    assert_output --partial "user"
    assert_output --partial "openvpn"
}

@test "_tproxy_setup_bypass_ipset: skips empty nvram entries" {
    load_tproxy_module
    # vpn_client2_addr is empty in mock — should not cause error
    run _tproxy_setup_bypass_ipset
    assert_success
}

# ============================================================================
# _tproxy_setup_routing / _tproxy_teardown_routing - fwmark rule for TPROXY
# ============================================================================

# The routers' iproute2 takes no selectors on "ip rule show" (mocks/ip refuses
# them the way the tool does), so a preference is filtered by the caller.
rules_at_pref() {
    ip rule show | grep "^$1:" || true
}

# The kernel refuses an exact duplicate through "ip rule add", but an older
# version of this module left duplicates behind all the same - it added its
# rule under a different table label, which the kernel accepted, and the copies
# accumulated. Reproduce that state the way the kernel would hold it.
duplicate_last_rule() {
    local f="${BATS_IP_RULES_FILE:-/tmp/bats_test_ip_rules}" n="${1:-1}" line
    line="$(tail -1 "$f")"
    while (( n-- > 0 )); do
        printf '%s\n' "$line" >> "$f"
    done
}

# The kernel prints a routing table by its rt_tables name, so table 100 comes
# back as "lookup wan0" on Asuswrt-Merlin. A check that looks for the number
# never recognises its own rule and re-adds it on every apply.
@test "_tproxy_setup_routing: adds the rule once when the kernel names the table" {
    load_tproxy_module

    _tproxy_setup_routing
    _tproxy_setup_routing

    run rules_at_pref 200
    assert_success
    assert_equal "$(grep -c 'fwmark 0x100/0x100' <<< "$output")" 1
}

@test "_tproxy_setup_routing: collapses duplicates left behind by an older version" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0x100/0x100 table 100
    duplicate_last_rule 2

    _tproxy_setup_routing

    run rules_at_pref 200
    assert_success
    assert_equal "$(grep -c 'fwmark 0x100/0x100' <<< "$output")" 1
}

@test "_tproxy_teardown_routing: removes every copy of the rule" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0x100/0x100 table 100
    duplicate_last_rule 1

    _tproxy_teardown_routing

    run rules_at_pref 200
    assert_success
    refute_output
}

# A rule created with an earlier advanced.xray.route_table / fwmark_mask sits at
# the same reserved preference. Deleting only the exact configured tuple would
# leave it in place, and counting it as "our rule" would skip installing the one
# the config now asks for - the configuration change would be ignored.
@test "_tproxy_setup_routing: replaces a rule left by a different route_table" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0x100/0x100 table 111

    _tproxy_setup_routing

    run rules_at_pref 200
    assert_success
    assert_output --partial "lookup wan0"
    refute_output --partial "lookup wgc1"
}

@test "_tproxy_setup_routing: replaces a rule left by a different fwmark mask" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0x100/0x1ff table 100

    _tproxy_setup_routing

    run rules_at_pref 200
    assert_success
    assert_output --partial "fwmark 0x100/0x100"
    refute_output --partial "0x1ff"
}

@test "_tproxy_teardown_routing: removes a rule left by earlier settings" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0x100/0x1ff table 111

    _tproxy_teardown_routing

    run rules_at_pref 200
    assert_success
    refute_output
}

# Another owner may park a rule at our pref (a connection policy on some
# KeeneticOS versions; 5.1.5 measured none at 200 - its policies sit at prefs
# 102/103, table 4097). Sharing a preference is not owning it: only a rule with
# our mark value or our table is ours to remove.
@test "_tproxy_setup_routing: leaves another owner's rule at the same pref alone" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0xffffd00 table 42

    run _tproxy_setup_routing
    assert_success
    refute_output --partial "Removed stale ip rule"

    run rules_at_pref 200
    assert_output --partial "fwmark 0xffffd00 lookup 42"
    assert_output --partial "fwmark 0x100/0x100 lookup wan0"
}

@test "_tproxy_teardown_routing: removes only our rules from the shared pref" {
    load_tproxy_module
    ip rule add pref 200 fwmark 0xffffd00 table 42
    ip rule add pref 200 fwmark 0x100/0x100 table 100
    ip rule add pref 200 fwmark 0x100/0x1ff table 111

    _tproxy_teardown_routing

    run rules_at_pref 200
    assert_output "200:	from all fwmark 0xffffd00 lookup 42"
}

@test "_tproxy_rule_is_ours: our mark value under any mask, or our table under any mark" {
    load_tproxy_module
    run _tproxy_rule_is_ours 0x100/0x100 wan0 wan0
    assert_success
    run _tproxy_rule_is_ours 0x100/0x1ff 111 wan0
    assert_success
    run _tproxy_rule_is_ours 0xabc wan0 wan0
    assert_success
    run _tproxy_rule_is_ours 0xffffd00 42 wan0
    assert_failure
    run _tproxy_rule_is_ours "" 42 wan0
    assert_failure
    run _tproxy_rule_is_ours garbage 42 wan0
    assert_failure
}

# ============================================================================
# Platform contract in tproxy.sh
# ============================================================================

@test "_tproxy_check_module: loads xt_TPROXY through platform_load_module" {
    load_tproxy_module
    platform_load_module() { echo "load $1" >> "$BATS_TEST_TMPDIR/load.log"; return 0; }
    run _tproxy_check_module
    assert_success
    grep -q "load xt_TPROXY" "$BATS_TEST_TMPDIR/load.log"
}

@test "_tproxy_check_module: fails with an ERROR when the platform cannot load the module" {
    load_tproxy_module
    platform_load_module() { return 1; }
    run _tproxy_check_module
    assert_failure
    assert_output --partial "xt_TPROXY module not available"
}

@test "_tproxy_setup_bypass_ipset: adds every platform VPN endpoint" {
    load_tproxy_module
    platform_vpn_endpoints() { printf '%s\n' 203.0.113.7 198.51.100.9; }
    : > /tmp/bats_ipset_calls.log
    run _tproxy_setup_bypass_ipset
    assert_success
    grep -q "ipset add TPROXY_BYPASS 203.0.113.7" /tmp/bats_ipset_calls.log
    grep -q "ipset add TPROXY_BYPASS 198.51.100.9" /tmp/bats_ipset_calls.log
}

@test "_tproxy_setup_iptables: applies platform extra rules with the configured mark and one jump per LAN interface" {
    load_tproxy_module
    platform_tproxy_extra_rules() { echo "extra $1 $2" >> "$BATS_TEST_TMPDIR/extra.log"; }
    platform_lan_ifaces() { printf 'br0\nbr1\n'; }
    : > /tmp/bats_iptables_calls.log
    run _tproxy_setup_iptables
    assert_success
    grep -qF "extra apply 0x100/0x100" "$BATS_TEST_TMPDIR/extra.log"
    # Each interface asks for its own position, so no jump displaces another and
    # the next apply finds both already in place instead of rewriting them.
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY' /tmp/bats_iptables_calls.log
    grep -q -- '-I PREROUTING 2 -i br1 -j XRAY_TPROXY' /tmp/bats_iptables_calls.log
}

# The jump is the whole point of the chain. A platform that cannot name its LAN
# interfaces used to run the loop zero times, leaving a fully populated chain
# with nothing jumping to it and logging "Applied TPROXY iptables rules" - every
# packet the proxy exists to carry silently going direct. Unreachable on Merlin,
# where platform_lan_ifaces is a constant.
@test "_tproxy_setup_iptables: fails and builds no chain when the platform names no LAN interface" {
    load_tproxy_module
    platform_lan_ifaces() { return 1; }
    : > /tmp/bats_iptables_calls.log
    run _tproxy_setup_iptables
    assert_failure
    assert_output --partial "Cannot determine the LAN interfaces"
    refute grep -q -- "-j XRAY_TPROXY" /tmp/bats_iptables_calls.log
    refute grep -q -- "-N XRAY_TPROXY" /tmp/bats_iptables_calls.log
}

# An empty answer with rc 0 is the same failure: zero jumps installed.
@test "_tproxy_setup_iptables: fails when the platform prints no LAN interface" {
    load_tproxy_module
    platform_lan_ifaces() { return 0; }
    : > /tmp/bats_iptables_calls.log
    run _tproxy_setup_iptables
    assert_failure
    assert_output --partial "Cannot determine the LAN interfaces"
    refute grep -q -- "-j XRAY_TPROXY" /tmp/bats_iptables_calls.log
}

# _tproxy_setup_iptables runs as "if ! _tproxy_setup_iptables", which turns
# errexit off for its whole body, and its last command is a log - so a platform
# that cannot install its own rules would otherwise be reported as a clean apply.
# The jumps still go in: a router without the subscription watch keeps the
# interception it had, and only the status - and so the ready marker - changes.
@test "_tproxy_setup_iptables: fails after installing the jumps when the platform cannot apply its extra rules" {
    load_tproxy_module
    platform_tproxy_extra_rules() { return 1; }
    : > /tmp/bats_iptables_calls.log
    run _tproxy_setup_iptables
    assert_failure
    assert_output --partial "Failed to apply platform TPROXY rules"
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY' /tmp/bats_iptables_calls.log
}

# Two rules decide who the targets take at all: without the "! XRAY_CLIENTS"
# return TPROXY takes every LAN client, without the private ranges traffic to
# the router and the rest of the LAN. errexit is off under "if !", so a failed
# one has to stop the setup itself - before the targets, and without a status
# that lets tproxy_apply publish ready.
@test "_tproxy_setup_iptables: fails before the TPROXY targets when a rule that bounds them fails" {
    load_tproxy_module
    local failing
    iptables() {
        [[ $* == *"$FAILING"* ]] && return 1
        command iptables "$@"
    }
    for failing in \
        "-F XRAY_TPROXY" \
        "-A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN" \
        "-A XRAY_TPROXY -d 10.0.0.0/8 -j RETURN" \
        "-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN" \
        "-A XRAY_TPROXY -d 192.168.0.0/16 -j RETURN"
    do
        FAILING="$failing"
        : > /tmp/bats_iptables_calls.log
        run _tproxy_setup_iptables
        if [[ $status -eq 0 ]]; then
            echo "succeeded although \"$failing\" failed"
            return 1
        fi
        if grep -q -- "-j TPROXY" /tmp/bats_iptables_calls.log; then
            echo "reached the TPROXY targets although \"$failing\" failed"
            return 1
        fi
    done
}

# The other RETURNs only decide what a client reaches directly instead of
# through the proxy: a server of the subscription, the loopback and link-local
# ranges, multicast, broadcast, an excluded country. Leaving the setup at a
# missing one - a busy xtables lock was enough - left a flushed chain that took
# nothing while the jumps stayed, and every Xray client went out through the
# WAN. The targets go in; only the marker waits for a clean apply.
@test "_tproxy_setup_iptables: a RETURN that only narrows the proxy keeps the TPROXY targets" {
    load_tproxy_module
    local failing
    iptables() {
        [[ $* == *"$FAILING"* ]] && return 4
        command iptables "$@"
    }
    for failing in \
        "-A XRAY_TPROXY -m set --match-set TPROXY_BYPASS dst -j RETURN" \
        "-A XRAY_TPROXY -d 127.0.0.0/8 -j RETURN" \
        "-A XRAY_TPROXY -d 169.254.0.0/16 -j RETURN" \
        "-A XRAY_TPROXY -d 224.0.0.0/4 -j RETURN" \
        "-A XRAY_TPROXY -d 255.255.255.255/32 -j RETURN" \
        "-A XRAY_TPROXY -m set --match-set ru dst -j RETURN"
    do
        FAILING="$failing"
        : > /tmp/bats_iptables_calls.log
        run _tproxy_setup_iptables
        if [[ $status -eq 0 ]]; then
            echo "reported success although \"$failing\" failed"
            return 1
        fi
        if ! grep -q -- "-A XRAY_TPROXY -p tcp -j TPROXY" /tmp/bats_iptables_calls.log ||
            ! grep -q -- "-A XRAY_TPROXY -p udp -j TPROXY" /tmp/bats_iptables_calls.log; then
            echo "no TPROXY targets after \"$failing\" failed"
            return 1
        fi
        if ! grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY' /tmp/bats_iptables_calls.log; then
            echo "no PREROUTING jump after \"$failing\" failed"
            return 1
        fi
    done
}

@test "_tproxy_teardown_iptables: removes platform extra rules with the configured mark" {
    load_tproxy_module
    platform_tproxy_extra_rules() { echo "extra $1 $2" >> "$BATS_TEST_TMPDIR/extra.log"; }
    run _tproxy_teardown_iptables
    assert_success
    grep -qF "extra stop 0x100/0x100" "$BATS_TEST_TMPDIR/extra.log"
}

# tproxy_stop calls this bare under set -euo pipefail, so a platform whose
# cleanup fails must not abort the teardown at its first line: the PREROUTING
# purge, the chain deletion and the ipsets matter more than the platform's own
# rules. Called without "run" on purpose - "run" turns errexit off, and errexit
# is exactly what the real caller has on.
@test "_tproxy_teardown_iptables: finishes the teardown when the platform stop fails" {
    load_tproxy_module
    platform_tproxy_extra_rules() { return 1; }
    : > /tmp/bats_iptables_calls.log

    _tproxy_teardown_iptables

    grep -q -- '-t mangle -S PREROUTING' /tmp/bats_iptables_calls.log
    grep -q -- '-t mangle -X XRAY_TPROXY' /tmp/bats_iptables_calls.log
}
