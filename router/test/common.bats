#!/usr/bin/env bats

load 'test_helper'

# ============================================================================
# uuid4
# ============================================================================

@test "uuid4: returns valid UUID format" {
    load_common
    run uuid4
    assert_success
    # UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
    assert_output --regexp '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
}

@test "uuid4: generates unique values" {
    load_common
    uuid1=$(uuid4)
    uuid2=$(uuid4)
    [ "$uuid1" != "$uuid2" ]
}

# ============================================================================
# compute_hash
# ============================================================================

@test "compute_hash: hashes string from stdin" {
    load_common
    result=$(echo -n "test" | compute_hash)
    # SHA-256 of "test" is known
    [ "$result" = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" ]
}

@test "compute_hash: hashes file" {
    load_common
    echo -n "test" > /tmp/bats_hash_test
    run compute_hash /tmp/bats_hash_test
    assert_success
    assert_output "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
    rm /tmp/bats_hash_test
}

# ============================================================================
# is_lan_ip
# ============================================================================

@test "is_lan_ip: 192.168.x.x is private" {
    load_common
    run is_lan_ip 192.168.1.100
    assert_success
}

@test "is_lan_ip: 10.x.x.x is private" {
    load_common
    run is_lan_ip 10.0.0.1
    assert_success
}

@test "is_lan_ip: 172.16.x.x is private" {
    load_common
    run is_lan_ip 172.16.0.1
    assert_success
}

@test "is_lan_ip: 172.31.x.x is private" {
    load_common
    run is_lan_ip 172.31.255.255
    assert_success
}

@test "is_lan_ip: 172.15.x.x is NOT private" {
    load_common
    run is_lan_ip 172.15.0.1
    assert_failure
}

@test "is_lan_ip: 8.8.8.8 is NOT private" {
    load_common
    run is_lan_ip 8.8.8.8
    assert_failure
}

@test "is_lan_ip: IPv6 ULA fd00:: is private" {
    load_common
    run is_lan_ip -6 "fd00::1"
    assert_success
}

@test "is_lan_ip: IPv6 link-local fe80:: is private" {
    load_common
    run is_lan_ip -6 "fe80::1"
    assert_success
}

@test "is_lan_ip: IPv6 global 2001:: is NOT private" {
    load_common
    run is_lan_ip -6 "2001:4860::1"
    assert_failure
}

# ============================================================================
# is_ipv4_net
# ============================================================================

@test "is_ipv4_net: accepts an IPv4 address or an IPv4 CIDR" {
    load_common
    local good
    for good in 192.168.1.5 10.0.0.0/8 172.16.0.0/12 192.168.50.10/24 0.0.0.0/0 255.255.255.255/32 192.168.1.0/9; do
        run is_ipv4_net "$good"
        if [[ $status -ne 0 ]]; then
            echo "rejected $good"
            return 1
        fi
    done
}

# is_lan_ip looks at the prefix only, so every one of these passed it and then
# took an apply down: iptables refuses 192.168.1.1000 outright and reads an octet
# with a leading zero as octal (08 is no number at all, 010 is 8), and ipset takes
# none of them.
@test "is_ipv4_net: rejects what iptables or ipset would refuse or misread" {
    load_common
    local bad
    for bad in 192.168.1.1000 192.168.1.08 192.168.01.1 010.0.0.1 256.1.1.1 1.2.3 1.2.3.4.5 \
        192.168.1.0/33 192.168.1.0/ 192.168.1.0/024 /24 fd00::10 "" " 192.168.1.5" "192.168.1.5 " 192.168.1.5/8/8; do
        run is_ipv4_net "$bad"
        if [[ $status -eq 0 ]]; then
            echo "accepted '$bad'"
            return 1
        fi
    done
}

# ============================================================================
# resolve_ip
# ============================================================================

@test "resolve_ip: returns literal IPv4" {
    load_common
    run resolve_ip 192.168.1.1
    assert_success
    assert_output "192.168.1.1"
}

@test "resolve_ip: resolves from /etc/hosts" {
    load_common
    # Uses fixture hosts file via HOSTS_FILE env
    run resolve_ip mypc
    assert_success
    assert_output "192.168.1.100"
}

@test "resolve_ip: -q suppresses error on failure" {
    load_common
    run resolve_ip -q nonexistent.invalid
    assert_failure
    assert_output ""
}

@test "resolve_ip: nslookup runs with the resolver's wait bounded" {
    load_common
    export NSLOOKUP_ENV_LOG="$BATS_TEST_TMPDIR/nslookup_env"
    run resolve_ip example.com
    assert_success
    assert_output "93.184.216.34"
    run cat "$NSLOOKUP_ENV_LOG"
    assert_output "RES_OPTIONS=timeout:1 attempts:2"
}

@test "resolve_ip -a: nslookup runs with the resolver's wait bounded" {
    load_common
    export NSLOOKUP_ENV_LOG="$BATS_TEST_TMPDIR/nslookup_env"
    run resolve_ip -a example.com
    assert_success
    assert_output "93.184.216.34"
    run cat "$NSLOOKUP_ENV_LOG"
    assert_output "RES_OPTIONS=timeout:1 attempts:2"
}

# ============================================================================
# log
# ============================================================================

@test "log: writes to LOG_FILE" {
    load_common
    log "test message"
    run cat "$LOG_FILE"
    assert_success
    assert_output --partial "INFO"
    assert_output --partial "test message"
}

@test "log: supports -l ERROR level" {
    load_common
    log -l ERROR "error message"
    run cat "$LOG_FILE"
    assert_output --partial "ERROR"
    assert_output --partial "error message"
}

@test "log: supports -l WARN level" {
    load_common
    log -l WARN "warning message"
    run cat "$LOG_FILE"
    assert_output --partial "WARN"
}

@test "log_error_trace: includes stack trace" {
    load_common

    # Define nested function to test stack trace
    inner_func() { log_error_trace "inner error"; }
    outer_func() { inner_func; }

    outer_func

    run cat "$LOG_FILE"
    assert_output --partial "inner error"
    assert_output --partial "at"
}

# ============================================================================
# strip_comments
# ============================================================================

@test "strip_comments: removes # comments" {
    load_common
    input=$'line1\n# comment\nline2'
    run strip_comments "$input"
    assert_success
    assert_line -n 0 "line1"
    assert_line -n 1 "line2"
}

@test "strip_comments: removes inline comments" {
    load_common
    run strip_comments "value # comment"
    assert_success
    assert_output "value"
}

@test "strip_comments: trims whitespace" {
    load_common
    run strip_comments "  spaced  "
    assert_success
    assert_output "spaced"
}

# ============================================================================
# download_file
# ============================================================================

@test "download_file: backward compatible with 2 args" {
    load_common

    # Test that function accepts 2 args (backward compatibility)
    # Uses real curl with a stable test URL
    run download_file "http://www.msftconnecttest.com/connecttest.txt" "/tmp/bats_test_dl"
    assert_success

    rm -f /tmp/bats_test_dl
}

@test "download_file: accepts custom timeout parameter" {
    load_common

    # Test with invalid URL to trigger failure path
    # This verifies the function signature works with 3 args
    run download_file "http://invalid.localhost.test/file" "/tmp/bats_test_dl2" 1
    assert_failure

    rm -f /tmp/bats_test_dl2
}

# Keenetic's busybox wget has no TLS and segfaults on https URLs; curl is there.
@test "download_file: falls back to curl when wget fails" {
    load_common
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/bash\nexit 139\n' > "$BATS_TEST_TMPDIR/bin/wget"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<'EOF'
#!/bin/bash
out=""
while [[ $# -gt 0 ]]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
printf 'payload\n' > "$out"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/wget" "$BATS_TEST_TMPDIR/bin/curl"
    PATH="$BATS_TEST_TMPDIR/bin:$PATH" run download_file "https://example.invalid/file" "$BATS_TEST_TMPDIR/out"
    assert_success
    run cat "$BATS_TEST_TMPDIR/out"
    assert_output "payload"
}

@test "download_file: fails and leaves no file when wget and curl both fail" {
    load_common
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/bash\nexit 139\n' > "$BATS_TEST_TMPDIR/bin/wget"
    printf '#!/bin/bash\nexit 22\n' > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/wget" "$BATS_TEST_TMPDIR/bin/curl"
    PATH="$BATS_TEST_TMPDIR/bin:$PATH" run download_file "https://example.invalid/file" "$BATS_TEST_TMPDIR/out"
    assert_failure
    assert_output --partial "Failed to download"
    [ ! -e "$BATS_TEST_TMPDIR/out" ]
}

# Keenetic has no working wget, so curl is the whole download path there and
# its flags decide what lands on disk. Without -f curl writes the server's
# error page to $dest and exits 0 - a 404 from a mirror would be stored as an
# ipset; without -L it stops at a redirect that wget would have followed. This
# mock answers the way curl does with and without those flags.
@test "download_file: an HTTP error from curl fails and leaves no file" {
    load_common
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/bash\nexit 139\n' > "$BATS_TEST_TMPDIR/bin/wget"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<'EOF'
#!/bin/bash
out=""; fail=0; follow=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o)         out="$2"; shift 2 ;;
        --fail)     fail=1; shift ;;
        --location) follow=1; shift ;;
        -[!-]*)     [[ $1 == *f* ]] && fail=1
                    [[ $1 == *L* ]] && follow=1
                    shift ;;
        *)          shift ;;
    esac
done
printf 'fail=%s follow=%s\n' "$fail" "$follow" > "$(dirname "$0")/../curl.flags"
: > "$out"   # curl creates the output file before it knows the status
[[ $fail -eq 1 ]] && exit 22                        # -f: HTTP 404 is an error
printf '<html>404 Not Found</html>\n' > "$out"      # no -f: the body is "the download"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/wget" "$BATS_TEST_TMPDIR/bin/curl"
    PATH="$BATS_TEST_TMPDIR/bin:$PATH" run download_file "https://example.invalid/file" "$BATS_TEST_TMPDIR/out"
    assert_failure
    assert_output --partial "Failed to download"
    [ ! -e "$BATS_TEST_TMPDIR/out" ]
    run cat "$BATS_TEST_TMPDIR/curl.flags"
    assert_output "fail=1 follow=1"
}

# The DEBUG line after a failed wget used to promise a curl attempt on a
# router that has no curl either, which is the one place the log has to be
# read to find out why nothing was downloaded.
@test "download_file: does not claim a curl attempt when curl is not installed" {
    load_common
    mkdir -p "$BATS_TEST_TMPDIR/nocurl"
    local tool
    for tool in date wc mv rm logger; do
        ln -sf "$(command -v "$tool")" "$BATS_TEST_TMPDIR/nocurl/$tool"
    done
    printf '#!/bin/bash\nexit 139\n' > "$BATS_TEST_TMPDIR/nocurl/wget"
    chmod +x "$BATS_TEST_TMPDIR/nocurl/wget"
    PATH="$BATS_TEST_TMPDIR/nocurl" run download_file "https://example.invalid/file" "$BATS_TEST_TMPDIR/out"
    assert_failure
    refute_output --partial "trying curl"
    assert_output --partial "curl is not installed"
}

# ============================================================================
# Platform contract is available through common.sh
# ============================================================================

@test "common.sh: sourcing it loads the platform contract" {
    load_common
    run platform_name
    assert_success
    assert_output "merlin"
}

# A router that got this common.sh without platform.sh - the shape of an update
# delivered by an updater that predates the platform layer - used to die on the
# shell's own "No such file or directory", naming no way out. It must say what
# is missing and how to get it, and still fail: without the contract every
# platform_* call below would be a command-not-found mid-apply.
@test "common.sh: names the missing platform.sh and still fails" {
    local lib="$BATS_TEST_TMPDIR/lib"
    mkdir -p "$lib"
    cp "$LIB_DIR/common.sh" "$lib/common.sh"
    run bash -c "set -e; source '$lib/common.sh'; echo reached"
    assert_failure
    assert_output --partial "$lib/platform.sh"
    assert_output --partial "install.sh"
    refute_output --partial "reached"
}

@test "get_active_wan_if: wraps platform_wan_if" {
    load_common
    run get_active_wan_if
    assert_success
    assert_output "eth0"
}

@test "get_active_wan_if: prints nothing and still succeeds when the platform has no answer" {
    load_common
    platform_wan_if() { return 1; }
    run get_active_wan_if
    assert_success
    refute_output
}

@test "get_ipv6_enabled: wraps platform_ipv6_enabled" {
    load_common
    run get_ipv6_enabled
    assert_output "1"
    platform_ipv6_enabled() { printf '0\n'; }
    run get_ipv6_enabled
    assert_output "0"
}
