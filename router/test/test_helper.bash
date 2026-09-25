# test/test_helper.bash

# Load bats helpers
load '/usr/lib/bats/bats-support/load.bash'
load '/usr/lib/bats/bats-assert/load.bash'

# Project paths - find test/ directory regardless of nesting depth
_find_test_root() {
    local dir="$BATS_TEST_DIRNAME"
    while [[ "$dir" != "/" ]]; do
        if [[ -f "$dir/test_helper.bash" ]]; then
            printf '%s' "$dir"
            return
        fi
        dir="$(dirname "$dir")"
    done
    # Fallback to BATS_TEST_DIRNAME if not found
    printf '%s' "$BATS_TEST_DIRNAME"
}
export TEST_ROOT="$(_find_test_root)"
export PROJECT_ROOT="$TEST_ROOT/.."
export SCRIPTS_DIR="$PROJECT_ROOT/opt/vpn-director"
export LIB_DIR="$SCRIPTS_DIR/lib"

# Test mode - disables syslog, uses fixtures
export TEST_MODE=1
export LOG_FILE="/tmp/bats_test_vpn_director.log"

# Platform under test. lib/platform.sh skips detection when this is set, so
# the suite never depends on /jffs or /opt/etc/ndm of the machine running it.
export VPD_PLATFORM="${VPD_PLATFORM:-merlin}"

# Override system paths for mocks
setup() {
    export PATH="$TEST_ROOT/mocks:$PATH"
    export HOSTS_FILE="$TEST_ROOT/fixtures/hosts"
    export RT_TABLES_FILE="$TEST_ROOT/fixtures/rt_tables"

    # tproxy_apply writes its ready marker to /tmp/xray_tproxy/ready unless told
    # otherwise - the file the bot on the machine running the suite would read.
    export XRAY_TPROXY_READY="$BATS_TEST_TMPDIR/xray_tproxy_ready"

    # Clean log file
    : > "$LOG_FILE"

    # Create a mock /etc/iproute2/rt_tables symlink for tests
    mkdir -p /tmp/bats_etc_iproute2
    ln -sf "$TEST_ROOT/fixtures/rt_tables" /tmp/bats_etc_iproute2/rt_tables
}

teardown() {
    # load_common gives bats its EXIT trap back, so common.sh's temp cleanup
    # has to be called here. Under bats that trap never ran reliably anyway —
    # /tmp had collected thousands of empty *.tmp_files lists.
    if declare -F _cleanup_tmp >/dev/null 2>&1; then
        _cleanup_tmp
    fi

    # Cleanup temp files if any
    rm -rf /tmp/bats_test_*
    # Cleanup tunnel director state files
    rm -rf /tmp/tunnel_director/*
}

# Helper to source common.sh with mocks
load_common() {
    # Set $0 to a fake script path for get_script_* functions
    export BASH_SOURCE_OVERRIDE="$SCRIPTS_DIR/test_script.sh"

    # common.sh installs "trap _cleanup_tmp EXIT INT TERM", which replaces the
    # EXIT trap bats installs to report the result of the test. Without the
    # trap bats never hears about a failing assertion and prints "Executed N
    # instead of expected M" instead of naming the test, so save it and put it
    # back. Temp files are cleaned by teardown() below instead.
    local bats_exit_trap
    bats_exit_trap="$(trap -p EXIT)"
    source "$LIB_DIR/common.sh"
    eval "${bats_exit_trap:-trap - EXIT}"
}

# Helper to source firewall.sh (requires common.sh first)
load_firewall() {
    load_common
    source "$LIB_DIR/firewall.sh"
}

# use_stateful_iptables - for the rest of the test, an iptables that remembers
# its chains and rules under $BATS_IPT_DIR (mocks/stateful/iptables): a listing
# shows what the code under test wrote, "-C" finds it, "-X" refuses a chain a
# rule still jumps to. A flush of such a chain - a live one - is appended to
# $BATS_IPT_DIR/live_flushes. Call it after the load_* helper.
use_stateful_iptables() {
    export BATS_IPT_DIR="$BATS_TEST_TMPDIR/iptables"
    mkdir -p "$BATS_IPT_DIR"
    export PATH="$TEST_ROOT/mocks/stateful:$PATH"
    hash -r
}

# Helper to source config.sh
load_config() {
    export VPD_CONFIG_FILE="$TEST_ROOT/fixtures/vpn-director.json"
    source "$LIB_DIR/config.sh"
}

# Helper to source ipset.sh module
load_ipset_module() {
    load_common
    load_config
    source "$LIB_DIR/ipset.sh" --source-only
}

# Helper to source tunnel.sh module
load_tunnel_module() {
    load_common
    load_config
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tunnel.sh" --source-only
}

# Helper to source tproxy.sh module
load_tproxy_module() {
    load_common
    load_config
    source "$LIB_DIR/ipset.sh" --source-only
    source "$LIB_DIR/firewall.sh"
    source "$LIB_DIR/tproxy.sh" --source-only
}

# Helper to source import_server_list.sh without running main
load_import_server_list() {
    load_common
    export IMPORT_TEST_MODE=1
    # import_server_list.sh sources common.sh itself, which re-installs the
    # EXIT trap load_common just put back - and a failing assertion in this
    # file would vanish into "Executed N instead of expected M" with no name.
    # Same guard as load_common.
    local bats_exit_trap
    bats_exit_trap="$(trap -p EXIT)"
    source "$SCRIPTS_DIR/import_server_list.sh"
    eval "${bats_exit_trap:-trap - EXIT}"
}
