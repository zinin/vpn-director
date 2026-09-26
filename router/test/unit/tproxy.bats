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
    declare -f tproxy_prune >/dev/null
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
    grep -q "ipset add TPROXY_BYPASS_NEW 203.0.113.7" /tmp/bats_ipset_calls.log
    grep -q "ipset add TPROXY_BYPASS_NEW 198.51.100.9" /tmp/bats_ipset_calls.log
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
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
    grep -q -- '-I PREROUTING 2 -i br1 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
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
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
}

# Two rules decide who the targets take at all: without the "! XRAY_CLIENTS"
# return TPROXY takes every LAN client, without the private ranges traffic to
# the router and the rest of the LAN. errexit is off under "if !", so a failed
# one has to stop the build itself - before the targets - and the chain built so
# far must never take the jumps: the chain already in place stays.
@test "_tproxy_setup_iptables: fails before the TPROXY targets when a rule that bounds them fails" {
    load_tproxy_module
    local failing
    iptables() {
        [[ $* == *"$FAILING"* ]] && return 1
        command iptables "$@"
    }
    for failing in \
        "-N XRAY_TPROXY_NEW" \
        "-A XRAY_TPROXY_NEW -m set ! --match-set XRAY_CLIENTS src -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 10.0.0.0/8 -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 172.16.0.0/12 -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN"
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
        if grep -qE -- "-I PREROUTING .*-j XRAY_TPROXY_NEW|-E XRAY_TPROXY_NEW" /tmp/bats_iptables_calls.log; then
            echo "swapped the chain in although \"$failing\" failed"
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
        "-A XRAY_TPROXY_NEW -m set --match-set TPROXY_BYPASS dst -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 127.0.0.0/8 -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 169.254.0.0/16 -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 224.0.0.0/4 -j RETURN" \
        "-A XRAY_TPROXY_NEW -d 255.255.255.255/32 -j RETURN" \
        "-A XRAY_TPROXY_NEW -m set --match-set ru dst -j RETURN"
    do
        FAILING="$failing"
        : > /tmp/bats_iptables_calls.log
        run _tproxy_setup_iptables
        if [[ $status -eq 0 ]]; then
            echo "reported success although \"$failing\" failed"
            return 1
        fi
        if ! grep -q -- "-A XRAY_TPROXY_NEW -p tcp -j TPROXY" /tmp/bats_iptables_calls.log ||
            ! grep -q -- "-A XRAY_TPROXY_NEW -p udp -j TPROXY" /tmp/bats_iptables_calls.log; then
            echo "no TPROXY targets after \"$failing\" failed"
            return 1
        fi
        if ! grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log ||
            ! grep -q -- '-E XRAY_TPROXY_NEW XRAY_TPROXY' /tmp/bats_iptables_calls.log; then
            echo "the chain was not swapped in after \"$failing\" failed"
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

# ============================================================================
# Make before break: the chain and the sets are swapped, never flushed
# ============================================================================

# tproxy_apply used to flush XRAY_TPROXY and fill it one rule per call on every
# apply - the hooks and the daily update included - and every Xray client left
# through the WAN until the TPROXY targets were back.
@test "tproxy_apply: rebuilds XRAY_TPROXY beside the live chain and never flushes it" {
    load_tproxy_module
    use_stateful_iptables
    run tproxy_apply
    assert_success
    run tproxy_apply
    assert_success

    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_line '-A XRAY_TPROXY -m set ! --match-set XRAY_CLIENTS src -j RETURN'
    assert_line '-A XRAY_TPROXY -p tcp -j TPROXY --on-port 12345 --tproxy-mark 0x100/0x100'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
    [ -f "$XRAY_TPROXY_READY" ]
}

# Merlin's firewall start runs "iptables -t mangle -F": every chain emptied, the
# jumps with them, and firewall-start applies again.
@test "tproxy_apply: puts the chain and its jump back after the firmware emptied mangle" {
    load_tproxy_module
    use_stateful_iptables
    run tproxy_apply
    assert_success
    iptables -t mangle -F

    run tproxy_apply
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_line '-A XRAY_TPROXY -p udp -j TPROXY --on-port 12345 --tproxy-mark 0x100/0x100'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# Rule 1 and the private ranges bound what the targets take. One that does not
# go in keeps the new chain out; the chain already in place goes on working.
@test "tproxy_apply: a rule that bounds the targets and fails leaves the live chain in place" {
    load_tproxy_module
    use_stateful_iptables
    run tproxy_apply
    assert_success
    iptables() {
        [[ $* == *"-A XRAY_TPROXY_NEW -d 10.0.0.0/8 -j RETURN"* ]] && return 1
        command iptables "$@"
    }

    run tproxy_apply
    assert_success
    assert_output --partial "TPROXY rules not rebuilt"
    [ ! -e "$XRAY_TPROXY_READY" ]
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_line '-A XRAY_TPROXY -p tcp -j TPROXY --on-port 12345 --tproxy-mark 0x100/0x100'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
}

# XRAY_CLIENTS only grows in tproxy_apply: a client on its way from Xray to a
# tunnel stays intercepted until Tunnel Director has it, and tproxy_prune lets
# it go after. The bypass set is built beside the live one and swapped in.
@test "tproxy_apply: adds to XRAY_CLIENTS and flushes neither set" {
    load_tproxy_module
    : > /tmp/bats_ipset_calls.log
    run tproxy_apply
    assert_success
    grep -qx 'ipset add -exist XRAY_CLIENTS 192.168.1.100' /tmp/bats_ipset_calls.log
    refute grep -qx 'ipset flush XRAY_CLIENTS' /tmp/bats_ipset_calls.log
    refute grep -qx 'ipset flush TPROXY_BYPASS' /tmp/bats_ipset_calls.log
    refute grep -q 'swap XRAY_CLIENTS' /tmp/bats_ipset_calls.log
    grep -qx 'ipset add TPROXY_BYPASS_NEW 1.2.3.4' /tmp/bats_ipset_calls.log
    grep -qx 'ipset swap TPROXY_BYPASS_NEW TPROXY_BYPASS' /tmp/bats_ipset_calls.log
    grep -qx 'ipset destroy TPROXY_BYPASS_NEW' /tmp/bats_ipset_calls.log
}

@test "tproxy_prune: swaps in exactly the clients xray.clients names" {
    load_tproxy_module
    : > /tmp/bats_ipset_calls.log
    run tproxy_prune
    assert_success
    run cat /tmp/bats_ipset_calls.log
    assert_line 'ipset create XRAY_CLIENTS_NEW hash:net'
    assert_line 'ipset add -exist XRAY_CLIENTS_NEW 192.168.1.100'
    assert_line 'ipset swap XRAY_CLIENTS_NEW XRAY_CLIENTS'
    assert_line 'ipset destroy XRAY_CLIENTS_NEW'
    refute_line 'ipset flush XRAY_CLIENTS'
}

# The swap would drop a client from the live set the moment the new set lacked
# it. Stopping there leaves the clients that left Xray proxied until the next
# apply: no leak.
@test "tproxy_prune: keeps the live set when a client does not go into the new one" {
    load_tproxy_module
    ipset() {
        if [[ $1 == add && $* == *XRAY_CLIENTS_NEW* ]]; then
            echo "ipset $*" >> /tmp/bats_ipset_calls.log
            return 1
        fi
        command ipset "$@"
    }
    : > /tmp/bats_ipset_calls.log
    run tproxy_prune
    assert_success
    assert_output --partial "stay proxied until the next apply"
    refute grep -q 'ipset swap' /tmp/bats_ipset_calls.log
    grep -qx 'ipset destroy XRAY_CLIENTS_NEW' /tmp/bats_ipset_calls.log
}

@test "tproxy_prune: does nothing when tproxy_apply never made the set" {
    load_tproxy_module
    _ipset_exists() { return 1; }
    : > /tmp/bats_ipset_calls.log
    run tproxy_prune
    assert_success
    [ ! -s /tmp/bats_ipset_calls.log ]
}

@test "_tproxy_teardown_iptables: removes what an interrupted apply left" {
    load_tproxy_module
    : > /tmp/bats_iptables_calls.log
    : > /tmp/bats_ipset_calls.log
    run _tproxy_teardown_iptables
    assert_success
    grep -q -- '-t mangle -X XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
    grep -qx 'ipset destroy XRAY_CLIENTS_NEW' /tmp/bats_ipset_calls.log
    grep -qx 'ipset destroy TPROXY_BYPASS_NEW' /tmp/bats_ipset_calls.log
}

# ============================================================================
# The keep-list: a client Tunnel Director did not take stays proxied
# ============================================================================

# live_xray_clients <member>... - a live XRAY_CLIENTS holding <member>..., on the
# stateful ipset. The fixture's xray.clients is 192.168.1.100.
live_xray_clients() {
    use_stateful_ipset
    ipset create XRAY_CLIENTS hash:net
    local m
    for m in "$@"; do
        ipset add XRAY_CLIENTS "$m"
    done
    : > /tmp/bats_ipset_calls.log
}

@test "tproxy_prune: keeps a named address the live set holds, and says so" {
    load_tproxy_module
    live_xray_clients 192.168.1.100 192.168.1.7 192.168.1.8
    run tproxy_prune 192.168.1.7
    assert_success
    assert_output --partial "Kept in XRAY_CLIENTS: 192.168.1.7 - they stay proxied"
    run ipset test XRAY_CLIENTS 192.168.1.7
    assert_success
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
    # Left Xray for direct: let go.
    run ipset test XRAY_CLIENTS 192.168.1.8
    assert_failure
}

# A client that was not proxied - one moving between two tunnels, say - is not
# made one now.
@test "tproxy_prune: a named address the live set does not hold is not added" {
    load_tproxy_module
    live_xray_clients 192.168.1.100
    run tproxy_prune 192.168.1.9
    assert_success
    refute_output --partial "Kept in"
    run ipset test XRAY_CLIENTS 192.168.1.9
    assert_failure
}

@test "tproxy_prune: an Xray client named as well is not reported as kept" {
    load_tproxy_module
    live_xray_clients 192.168.1.100
    run tproxy_prune 192.168.1.100
    assert_success
    refute_output --partial "Kept in"
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
}

# The swap would drop a client the new set lacks: a kept address the set refuses
# ends the prune as an Xray client the set refuses does, and every client stays.
@test "tproxy_prune: a kept address the new set refuses leaves the live set as it is" {
    load_tproxy_module
    live_xray_clients 192.168.1.100 192.168.1.7 192.168.1.8
    ipset() {
        if [[ $1 == add && $* == *"XRAY_CLIENTS_NEW 192.168.1.7" ]]; then
            return 1
        fi
        command ipset "$@"
    }
    run tproxy_prune 192.168.1.7
    assert_success
    assert_output --partial "stay proxied until the next apply"
    run ipset test XRAY_CLIENTS 192.168.1.8
    assert_success
    run ipset list -n XRAY_CLIENTS_NEW
    assert_failure
}

# load_tproxy_module_with_clients <json array> - load_tproxy_module on a copy of
# the fixture whose xray.clients is <json array>: config.sh marks XRAY_CLIENTS
# readonly, so a test cannot set it afterwards.
load_tproxy_module_with_clients() {
    load_common
    jq --argjson c "$1" '.xray.clients = $c' "$TEST_ROOT/fixtures/vpn-director.json" \
        > "$BATS_TEST_TMPDIR/vpn-director.json"
    export VPD_CONFIG_FILE="$BATS_TEST_TMPDIR/vpn-director.json"
    source "$LIB_DIR/config.sh"
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tproxy.sh" --source-only
}

# The overlap packet-flow.md documents ("Example 3"): the network on Xray, a host
# of it on a tunnel. The kernel's "ipset test" finds the host through the
# network, and "apply xray", which names every tunnel client, added it as an
# element of its own - with a WARN - on every run, though Tunnel Director never
# takes it while the network is on Xray.
@test "tproxy_prune: a named host inside an effective network is not kept on its own" {
    load_tproxy_module_with_clients '["192.168.50.0/24"]'
    live_xray_clients 192.168.50.0/24
    run tproxy_prune 192.168.50.10
    assert_success
    refute_output --partial "Kept in"
    run ipset list XRAY_CLIENTS
    assert_output $'Name: XRAY_CLIENTS\nType: hash:net\nNumber of entries: 1\nMembers:\n192.168.50.0/24'
}

@test "tproxy_prune: a named x/32 of an effective x is not kept on its own" {
    load_tproxy_module_with_clients '["192.168.1.7"]'
    live_xray_clients 192.168.1.7
    run tproxy_prune 192.168.1.7/32
    assert_success
    refute_output --partial "Kept in"
    run ipset list XRAY_CLIENTS
    assert_output $'Name: XRAY_CLIENTS\nType: hash:net\nNumber of entries: 1\nMembers:\n192.168.1.7'
}

@test "tproxy_prune: an address named twice, in either spelling, is kept once" {
    load_tproxy_module
    live_xray_clients 192.168.1.100 192.168.1.7
    run tproxy_prune 192.168.1.7 192.168.1.7/32
    assert_success
    assert_output --partial "Kept in XRAY_CLIENTS: 192.168.1.7 - they stay proxied"
}

# The network has left Xray while the live set still holds it: the host was
# proxied through it and Tunnel Director has not taken it, so it is kept, as an
# element of its own, and the rest of the network is let go.
@test "tproxy_prune: a named host is kept on its own once its network has left Xray" {
    load_tproxy_module
    live_xray_clients 192.168.1.100 192.168.50.0/24
    run tproxy_prune 192.168.50.10
    assert_success
    assert_output --partial "Kept in XRAY_CLIENTS: 192.168.50.10 - they stay proxied"
    run ipset list XRAY_CLIENTS
    assert_output $'Name: XRAY_CLIENTS\nType: hash:net\nNumber of entries: 2\nMembers:\n192.168.1.100\n192.168.50.10'
    run ipset test XRAY_CLIENTS 192.168.50.11
    assert_failure
}

# M1: both soft-fails returned before any client was added. A client moving
# from a tunnel to Xray then entered XRAY_CLIENTS only at the prune - after the
# TUN_DIR swap had already taken its mark.
@test "tproxy_apply: a soft-fail for missing ipsets still adds the clients to a live XRAY_CLIENTS" {
    load_tproxy_module
    live_xray_clients 192.168.1.7
    _tproxy_check_required_ipsets() { return 1; }
    run tproxy_apply
    assert_success
    assert_output --partial "Required ipsets not ready"
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
    run ipset test XRAY_CLIENTS 192.168.1.7
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

@test "tproxy_apply: a soft-fail for a missing xt_TPROXY still adds the clients to a live XRAY_CLIENTS" {
    load_tproxy_module
    live_xray_clients
    _tproxy_check_module() { return 1; }
    run tproxy_apply
    assert_success
    run ipset test XRAY_CLIENTS 192.168.1.100
    assert_success
    [ ! -e "$XRAY_TPROXY_READY" ]
}

@test "tproxy_apply: a soft-fail makes no XRAY_CLIENTS where there was none" {
    load_tproxy_module
    use_stateful_ipset
    _tproxy_check_required_ipsets() { return 1; }
    run tproxy_apply
    assert_success
    run ipset list -n XRAY_CLIENTS
    assert_failure
}
