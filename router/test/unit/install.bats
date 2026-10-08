#!/usr/bin/env bats

load '../test_helper'

# install.sh is sourced with --source-only, so main() never runs. The paths it
# writes to are redirected into BATS_TEST_TMPDIR by overriding the globals it
# declares at the top of the file.
load_installer() {
    source "$PROJECT_ROOT/../install.sh" --source-only
    VPD_DIR="$BATS_TEST_TMPDIR/vpn-director"
    INIT_DIR="$BATS_TEST_TMPDIR/init.d"
    # create_directories also mkdirs XRAY_CONFIG_DIR; /opt/etc is not writable here.
    XRAY_CONFIG_DIR="$BATS_TEST_TMPDIR/xray"
    mkdir -p "$VPD_DIR" "$INIT_DIR"
    WEBUI_INIT="$INIT_DIR/S98vpn-director-webui"
    # What download_scripts installs before the LAN facts are needed.
    cp -r "$SCRIPTS_DIR/lib" "$VPD_DIR/lib"
    PLATFORM=merlin
    load_platform_lib
}

# fake_init writes a stand-in init script that records its arguments and exits
# with the given code. $code is expanded now; \$1 is left for the inner script.
fake_init() {
    local code="${1:-0}"
    cat > "$WEBUI_INIT" <<EOF
#!/bin/sh
echo "\$1" >> "$BATS_TEST_TMPDIR/init.calls"
exit $code
EOF
    chmod +x "$WEBUI_INIT"
}

@test "start_webui: does nothing when the webui binary was not installed" {
    load_installer
    fake_init 0

    run start_webui

    assert_success
    [[ ! -f "$BATS_TEST_TMPDIR/init.calls" ]]
}

@test "start_webui: starts the daemon and reports the LAN address" {
    load_installer
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 0

    run start_webui

    assert_success
    assert_output --partial "https://192.168.50.1:8444"
    run cat "$BATS_TEST_TMPDIR/init.calls"
    assert_output "start"
}

@test "start_webui: a failed start is reported without aborting the installer" {
    load_installer
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 1

    run start_webui

    assert_success
    assert_output --partial "Failed to start Web UI"
    # Without the early return the installer would report the failure and then
    # go on to announce a URL for a daemon that is not running.
    refute_output --partial "Web UI started"
}

@test "start_webui: falls back to a default address when nvram has no lan_ipaddr" {
    load_installer
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 0
    # The mock answers "" for anything it does not know; an empty address in the
    # printed URL would be worse than a wrong-but-plausible default.
    NVRAM_NO_LAN_IP=1 run start_webui

    assert_success
    assert_output --partial "https://192.168.1.1:8444"
}

@test "start_webui: uses webui.port from the config when present" {
    load_installer
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 0
    printf '%s\n' '{"webui":{"port":9444}}' > "$VPD_DIR/vpn-director.json"

    run start_webui

    assert_success
    assert_output --partial "https://192.168.50.1:9444"
}

@test "start_webui: records the LAN URL in WEBUI_URL for print_next_steps" {
    load_installer
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 0

    # Deliberately not `run`: it forks a subshell, so an assignment to the
    # global would be lost and print_next_steps would take the wrong branch.
    start_webui

    assert_equal "$WEBUI_URL" "https://192.168.50.1:8444"
}

@test "setup_webui_config: copies the template when vpn-director.json is missing" {
    load_installer
    printf '%s\n' '{"webui":{"port":8444}}' > "$VPD_DIR/vpn-director.json.template"

    run setup_webui_config

    assert_success
    assert_output --partial "Created"
    [ -f "$VPD_DIR/vpn-director.json" ]
    run stat -c %a "$VPD_DIR/vpn-director.json"
    assert_output "600"
}

@test "setup_webui_config: does nothing when neither json nor template exist" {
    load_installer

    run setup_webui_config

    assert_success
    [ ! -f "$VPD_DIR/vpn-director.json" ]
}

# ============================================================================
# files.manifest parsing
# ============================================================================

write_manifest() {
    cat > "$BATS_TEST_TMPDIR/files.manifest" <<'EOF'
# comment line
common   router/opt/vpn-director/vpn-director.sh

merlin   router/jffs/scripts/firewall-start
keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh
common   router/opt/etc/xray/config.json.template
EOF
}

@test "manifest_files: prints common and platform paths in manifest order" {
    load_installer
    write_manifest
    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" merlin
    assert_success
    assert_line --index 0 "router/opt/vpn-director/vpn-director.sh"
    assert_line --index 1 "router/jffs/scripts/firewall-start"
    assert_line --index 2 "router/opt/etc/xray/config.json.template"
    refute_output --partial "netfilter.d"
}

@test "manifest_files: selects the other platform's files with its tag" {
    load_installer
    write_manifest
    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" keenetic
    assert_success
    assert_line --index 1 "router/opt/etc/ndm/netfilter.d/50-vpn-director.sh"
    refute_output --partial "firewall-start"
}

@test "manifest_is_executable: scripts and init files are executable" {
    load_installer
    run manifest_is_executable "router/opt/vpn-director/lib/common.sh"
    assert_success
    run manifest_is_executable "router/opt/etc/init.d/S99vpn-director"
    assert_success
    run manifest_is_executable "router/jffs/scripts/wan-event"
    assert_success
}

@test "manifest_is_executable: templates, json and the manifest are data" {
    load_installer
    run manifest_is_executable "router/opt/vpn-director/vpn-director.json.template"
    assert_failure
    run manifest_is_executable "router/opt/etc/xray/config.json.template"
    assert_failure
    run manifest_is_executable "router/opt/vpn-director/data/servers.json"
    assert_failure
    run manifest_is_executable "router/files.manifest"
    assert_failure
}

# fake_curl serves "$REPO_URL/<path>" from $BATS_TEST_TMPDIR/repo/<path>.
# install.sh calls: curl -fsSL "$url" -o "$target"
fake_curl() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<EOF
#!/bin/bash
out=""; url=""
while [[ \$# -gt 0 ]]; do
    case "\$1" in
        -o) out="\$2"; shift 2 ;;
        -*) shift ;;
        *)  url="\$1"; shift ;;
    esac
done
src="$BATS_TEST_TMPDIR/repo/\${url#*/refs/tags/v1.0.0/}"
[[ -f "\$src" ]] || exit 22
cp "\$src" "\$out"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

@test "download_scripts: installs the manifest's common and platform files under INSTALL_ROOT" {
    load_installer
    fake_curl
    local repo="$BATS_TEST_TMPDIR/repo/router"
    mkdir -p "$repo/opt/vpn-director/lib" "$repo/jffs/scripts" "$repo/opt/etc/ndm/netfilter.d"
    cat > "$BATS_TEST_TMPDIR/repo/router/files.manifest" <<'EOF'
common   router/opt/vpn-director/lib/common.sh
common   router/opt/vpn-director/vpn-director.json.template
merlin   router/jffs/scripts/firewall-start
keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh
EOF
    echo "lib" > "$repo/opt/vpn-director/lib/common.sh"
    echo "{}" > "$repo/opt/vpn-director/vpn-director.json.template"
    echo "hook" > "$repo/jffs/scripts/firewall-start"
    echo "ndm" > "$repo/opt/etc/ndm/netfilter.d/50-vpn-director.sh"

    REPO_URL="https://raw.example/zinin/vpn-director/refs/tags/v1.0.0"
    PLATFORM="merlin"
    INSTALL_ROOT="$BATS_TEST_TMPDIR/root"

    run download_scripts
    assert_success
    [ -x "$INSTALL_ROOT/opt/vpn-director/lib/common.sh" ]
    [ -f "$INSTALL_ROOT/opt/vpn-director/vpn-director.json.template" ]
    [ ! -x "$INSTALL_ROOT/opt/vpn-director/vpn-director.json.template" ]
    [ -x "$INSTALL_ROOT/jffs/scripts/firewall-start" ]
    [ ! -e "$INSTALL_ROOT/opt/etc/ndm/netfilter.d/50-vpn-director.sh" ]
}

@test "download_scripts: fails when the manifest cannot be downloaded" {
    load_installer
    fake_curl
    mkdir -p "$BATS_TEST_TMPDIR/repo/router"
    REPO_URL="https://raw.example/zinin/vpn-director/refs/tags/v1.0.0"
    PLATFORM="merlin"
    INSTALL_ROOT="$BATS_TEST_TMPDIR/root"
    run download_scripts
    assert_failure
    assert_output --partial "files.manifest"
}

@test "manifest_files: keeps a last line that has no trailing newline" {
    load_installer
    # The manifest arrives over the network, and the Go parser reads an
    # unterminated last line; a `read` loop that drops it would make the two
    # disagree about what a release ships.
    printf 'common   router/opt/vpn-director/lib/common.sh\nmerlin   router/jffs/scripts/wan-event' \
        > "$BATS_TEST_TMPDIR/files.manifest"

    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" merlin

    assert_success
    assert_line --index 0 "router/opt/vpn-director/lib/common.sh"
    # Not --index 1: bats-assert reads the array unguarded, so a missing line
    # crashes it under `set -u` instead of reporting the diff. Ordering is
    # covered by "prints common and platform paths in manifest order".
    assert_line "router/jffs/scripts/wan-event"
}

@test "manifest_files: an unknown tag fails and names the offending line" {
    load_installer
    cat > "$BATS_TEST_TMPDIR/files.manifest" <<'EOF'
common   router/opt/vpn-director/lib/common.sh
comon    router/opt/vpn-director/lib/tunnel.sh
EOF

    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" merlin

    # Silently skipping the typo would drop tunnel.sh from every install, and
    # no "entry exists in the repo" check can see it: the path is fine.
    assert_failure
    assert_output --partial "line 2"
    assert_output --partial "comon"
    assert_output --partial "router/opt/vpn-director/lib/tunnel.sh"
}

@test "manifest_files: an invalid path fails and names the offending line" {
    load_installer
    cat > "$BATS_TEST_TMPDIR/files.manifest" <<'EOF'
common   router/opt/vpn-director/lib/common.sh
common   router/opt/../../etc/passwd
EOF

    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" merlin

    # The path is pasted into "${INSTALL_ROOT}/${file#router/}", so a traversal
    # would install outside the root. The Go parser refuses the same shapes.
    assert_failure
    assert_output --partial "line 2"
    assert_output --partial "router/opt/../../etc/passwd"

    printf 'common   /etc/passwd\n' > "$BATS_TEST_TMPDIR/files.manifest"

    run manifest_files "$BATS_TEST_TMPDIR/files.manifest" merlin

    assert_failure
    assert_output --partial "/etc/passwd"
}

@test "download_scripts: a misspelled tag aborts before installing anything" {
    load_installer
    fake_curl
    local repo="$BATS_TEST_TMPDIR/repo/router"
    mkdir -p "$repo/opt/vpn-director/lib"
    cat > "$repo/files.manifest" <<'EOF'
common   router/opt/vpn-director/lib/common.sh
comon    router/opt/vpn-director/lib/tunnel.sh
EOF
    echo "lib" > "$repo/opt/vpn-director/lib/common.sh"
    echo "tunnel" > "$repo/opt/vpn-director/lib/tunnel.sh"

    REPO_URL="https://raw.example/zinin/vpn-director/refs/tags/v1.0.0"
    PLATFORM="merlin"
    INSTALL_ROOT="$BATS_TEST_TMPDIR/root"

    run download_scripts

    assert_failure
    assert_output --partial "comon"
    # The good entry is listed first, so a parser that gave up mid-loop would
    # have written it already and left a half-installed router.
    [ ! -e "$INSTALL_ROOT/opt/vpn-director/lib/common.sh" ]
}

@test "download_scripts: fails when the manifest selects no file for the platform" {
    load_installer
    fake_curl
    mkdir -p "$BATS_TEST_TMPDIR/repo/router"
    cat > "$BATS_TEST_TMPDIR/repo/router/files.manifest" <<'EOF'
# a manifest with nothing for this platform
keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh
EOF
    REPO_URL="https://raw.example/zinin/vpn-director/refs/tags/v1.0.0"
    PLATFORM="merlin"
    INSTALL_ROOT="$BATS_TEST_TMPDIR/root"

    run download_scripts

    # Reporting success after installing nothing would send the installer on to
    # setup_webui_config and start_webui on top of absent scripts.
    assert_failure
    assert_output --partial "no file for platform merlin"
    refute_output --partial "Installed"
}

# ============================================================================
# Platform detection and the Keenetic prerequisites
# ============================================================================

@test "detect_platform: keenetic, merlin, or a refusal" {
    load_installer
    local root="$BATS_TEST_TMPDIR/root"
    mkdir -p "$root/opt/etc/ndm" "$root/bin"
    printf '#!/bin/sh\n' > "$root/bin/ndmc"
    chmod +x "$root/bin/ndmc"
    VPD_PROBE_ROOT="$root" detect_platform
    assert_equal "$PLATFORM" keenetic

    root="$BATS_TEST_TMPDIR/root2"
    mkdir -p "$root/jffs" "$root/bin"
    printf '#!/bin/sh\n' > "$root/bin/nvram"
    chmod +x "$root/bin/nvram"
    VPD_PROBE_ROOT="$root" detect_platform
    assert_equal "$PLATFORM" merlin

    mkdir -p "$BATS_TEST_TMPDIR/empty"
    VPD_PROBE_ROOT="$BATS_TEST_TMPDIR/empty" run detect_platform
    assert_failure
    assert_output --partial "Unsupported platform"
}

# fake_opkg <installed...> answers "opkg status <pkg>" for the named packages
# and records every "opkg install".
fake_opkg() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/opkg" <<EOF
#!/bin/bash
installed=" $* "
case "\$1" in
    status)  [[ "\$installed" == *" \$2 "* ]] && echo "Status: install user installed"; exit 0 ;;
    update)  exit 0 ;;
    install) shift; echo "install \$*" >> "$BATS_TEST_TMPDIR/opkg.log"; exit 0 ;;
esac
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/opkg"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

@test "check_keenetic_prerequisites: refuses without the TPROXY kernel module and names the component" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    run check_keenetic_prerequisites
    assert_failure
    assert_output --partial "xt_TPROXY.ko not found"
    assert_output --partial "Kernel modules for Netfilter"
}

@test "check_keenetic_prerequisites: passes when the module and every package are there" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    : > "$VPD_MODULES_DIR/xt_TPROXY.ko"
    fake_opkg $KEENETIC_PACKAGES
    run check_keenetic_prerequisites
    assert_success
    assert_output --partial "Required Entware packages are installed"
}

@test "check_keenetic_prerequisites: without a terminal it prints the opkg command and exits 1" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    : > "$VPD_MODULES_DIR/xt_TPROXY.ko"
    fake_opkg bash curl jq iptables ipset ip-full
    INSTALL_TTY="$BATS_TEST_TMPDIR/no-tty" run check_keenetic_prerequisites
    assert_failure
    assert_output --partial "opkg update && opkg install flock coreutils-nohup"
    assert_output --partial " xray"
    [ ! -e "$BATS_TEST_TMPDIR/opkg.log" ]
}

@test "check_keenetic_prerequisites: a terminal that yields no answer prints the opkg command and exits 1" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    : > "$VPD_MODULES_DIR/xt_TPROXY.ko"
    fake_opkg bash curl jq iptables ipset ip-full
    : > "$BATS_TEST_TMPDIR/silent-tty"       # a read that yields nothing must refuse, not default to yes
    INSTALL_TTY="$BATS_TEST_TMPDIR/silent-tty" run check_keenetic_prerequisites
    assert_failure
    assert_output --partial "opkg update && opkg install"
    [ ! -e "$BATS_TEST_TMPDIR/opkg.log" ]
}

@test "check_keenetic_prerequisites: on a terminal installs the missing packages after a yes" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    : > "$VPD_MODULES_DIR/xt_TPROXY.ko"
    fake_opkg bash curl jq iptables ipset ip-full flock coreutils-nohup coreutils-base64 coreutils-sha256sum gawk procps-ng-pgrep procps-ng-pkill procps-ng-ps openssl-util
    printf '\n' > "$BATS_TEST_TMPDIR/tty"   # Enter = the default, yes
    INSTALL_TTY="$BATS_TEST_TMPDIR/tty" run check_keenetic_prerequisites
    assert_success
    run cat "$BATS_TEST_TMPDIR/opkg.log"
    assert_output "install cron xray"
}

@test "check_keenetic_prerequisites: a no on the terminal prints the command and exits 1" {
    load_installer
    export VPD_MODULES_DIR="$BATS_TEST_TMPDIR/modules"
    mkdir -p "$VPD_MODULES_DIR"
    : > "$VPD_MODULES_DIR/xt_TPROXY.ko"
    fake_opkg bash
    printf 'n\n' > "$BATS_TEST_TMPDIR/tty"
    INSTALL_TTY="$BATS_TEST_TMPDIR/tty" run check_keenetic_prerequisites
    assert_failure
    assert_output --partial "opkg update && opkg install"
    [ ! -e "$BATS_TEST_TMPDIR/opkg.log" ]
}

@test "release_arch: aarch64, armv7l and mips map to release asset suffixes" {
    load_installer
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    for m in aarch64 armv7l mips x86_64; do
        printf '#!/bin/bash\necho %s\n' "$m" > "$BATS_TEST_TMPDIR/bin/uname"
        chmod +x "$BATS_TEST_TMPDIR/bin/uname"
        PATH="$BATS_TEST_TMPDIR/bin:$PATH" run release_arch
        case "$m" in
            aarch64) assert_output arm64 ;;
            armv7l)  assert_output arm ;;
            mips)    assert_output mipsle ;;
            x86_64)  assert_failure; refute_output ;;
        esac
    done
}

@test "create_directories: hook directories per platform, under INSTALL_ROOT" {
    load_installer
    INSTALL_ROOT="$BATS_TEST_TMPDIR/root"
    PLATFORM=merlin run create_directories
    assert_success
    [ -d "$INSTALL_ROOT/jffs/scripts" ]
    [ ! -d "$INSTALL_ROOT/opt/etc/ndm" ]

    INSTALL_ROOT="$BATS_TEST_TMPDIR/root2"
    PLATFORM=keenetic run create_directories
    assert_success
    [ -d "$INSTALL_ROOT/opt/etc/ndm/netfilter.d" ]
    [ -d "$INSTALL_ROOT/opt/etc/ndm/wan.d" ]
    [ -d "$INSTALL_ROOT/opt/etc/ndm/iflayerchanged.d" ]
    [ ! -d "$INSTALL_ROOT/opt/etc/ndm/ifstatechanged.d" ]
    [ -d "$INSTALL_ROOT/opt/etc/cron.d" ]
    [ ! -d "$INSTALL_ROOT/jffs" ]
}

@test "lan_ip and lan_hostname: from the installed platform library, with defaults" {
    load_installer
    PLATFORM=merlin
    load_platform_lib
    run lan_ip
    assert_output "192.168.50.1"
    run lan_hostname
    assert_output "RT-AX88U-1234"
    NVRAM_NO_LAN_IP=1 run lan_ip
    assert_output "192.168.1.1"
}

@test "print_next_steps: names the login per platform" {
    load_installer
    RELEASE_TAG=v1.0.0
    WEBUI_URL="https://192.168.1.1:8444"
    PLATFORM=keenetic run print_next_steps
    assert_output --partial "root"
    assert_output --partial "Entware password"
    PLATFORM=merlin run print_next_steps
    assert_output --partial "router admin username and password"
}

# fake_watchd_init writes a stand-in for the monitor's init script that records
# its arguments and exits with the given code.
fake_watchd_init() {
    local code="${1:-0}"
    cat > "$INIT_DIR/S98vpn-director-watchd" <<EOF2
#!/bin/sh
echo "\$1" >> "$BATS_TEST_TMPDIR/watchd.calls"
exit $code
EOF2
    chmod +x "$INIT_DIR/S98vpn-director-watchd"
}

@test "start_watchd: does nothing when the monitor binary was not installed" {
    load_installer
    fake_watchd_init 0

    run start_watchd

    assert_success
    [[ ! -f "$BATS_TEST_TMPDIR/watchd.calls" ]]
}

@test "start_watchd: starts the monitor" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"
    fake_watchd_init 0

    run start_watchd

    assert_success
    assert_output --partial "Server monitor started"
    run cat "$BATS_TEST_TMPDIR/watchd.calls"
    assert_output "start"
}

@test "start_watchd: a failed start is reported without aborting the installer" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"
    fake_watchd_init 1

    run start_watchd

    assert_success
    assert_output --partial "Failed to start the server monitor"
}

@test "start_watchd: skips a missing or non-executable init script" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"

    run start_watchd
    assert_success
    assert_output --partial "Server monitor init script not found, skipping start"
    refute_output --partial "Server monitor started"

    fake_watchd_init 0
    chmod -x "$INIT_DIR/S98vpn-director-watchd"
    run start_watchd
    assert_success
    assert_output --partial "Server monitor init script not found, skipping start"
    [[ ! -f "$BATS_TEST_TMPDIR/watchd.calls" ]]
}

# All download and process commands are sandbox stubs, including the fallback
# killall; the liveness marker never represents a real process.
watchd_download_sandbox() {
    RELEASE_ASSET_URL="https://releases.example/v1.0.0"
    export WATCHD_ARCH=aarch64 WATCHD_DOWNLOAD_EXIT=0 WATCHD_STOP_EXIT=0 WATCHD_STOP_SURVIVES=0
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<'EOF'
#!/bin/bash
out=""; url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
[[ $url == https://releases.example/v1.0.0/vpn-director-watchd-* ]] || exit 99
[[ $out == "$BATS_TEST_TMPDIR/vpn-director/vpn-director-watchd.tmp" ]] || exit 99
printf '%s\n' "$url" > "$BATS_TEST_TMPDIR/watchd.url"
printf 'curl\n' >> "$BATS_TEST_TMPDIR/watchd.events"
printf 'new monitor\n' > "$out"
exit "$WATCHD_DOWNLOAD_EXIT"
EOF
    cat > "$BATS_TEST_TMPDIR/bin/uname" <<'EOF'
#!/bin/sh
[ "$1" = -m ] || exit 99
printf '%s\n' "$WATCHD_ARCH"
EOF
    cat > "$BATS_TEST_TMPDIR/bin/pidof" <<'EOF'
#!/bin/sh
[ "$#" = 1 ] && [ "$1" = vpn-director-watchd ] || exit 99
if [ -f "$BATS_TEST_TMPDIR/watchd.running" ]; then
    printf 'pidof:alive\n' >> "$BATS_TEST_TMPDIR/watchd.events"
    printf '12345\n'
    exit 0
fi
printf 'pidof:gone\n' >> "$BATS_TEST_TMPDIR/watchd.events"
exit 1
EOF
    cat > "$BATS_TEST_TMPDIR/bin/killall" <<'EOF'
#!/bin/sh
[ "$#" = 1 ] && [ "$1" = vpn-director-watchd ] || exit 99
printf 'stop:killall\n' >> "$BATS_TEST_TMPDIR/watchd.events"
if [ "$WATCHD_STOP_SURVIVES" = 0 ]; then
    rm -f "$BATS_TEST_TMPDIR/watchd.running"
fi
exit "$WATCHD_STOP_EXIT"
EOF
    cat > "$BATS_TEST_TMPDIR/bin/sleep" <<'EOF'
#!/bin/sh
[ "$#" = 1 ] && [ "$1" = 1 ] || exit 99
printf 'sleep:1\n' >> "$BATS_TEST_TMPDIR/watchd.events"
EOF
    chmod +x "$BATS_TEST_TMPDIR/bin/"{curl,uname,pidof,killall,sleep}
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
    hash -r
    local file
    for file in telegram-bot webui vpn-director.json watchd.unrelated.tmp; do
        printf 'unrelated %s\n' "$file" > "$VPD_DIR/$file"
    done
}

fake_watchd_stop_init() {
    cat > "$INIT_DIR/S98vpn-director-watchd" <<'EOF'
#!/bin/sh
[ "$#" = 1 ] && [ "$1" = stop ] || exit 99
printf 'stop:init\n' >> "$BATS_TEST_TMPDIR/watchd.events"
if [ "$WATCHD_STOP_SURVIVES" = 0 ]; then
    rm -f "$BATS_TEST_TMPDIR/watchd.running"
fi
exit "$WATCHD_STOP_EXIT"
EOF
    chmod +x "$INIT_DIR/S98vpn-director-watchd"
}

old_watchd_binary() {
    printf 'old monitor\n' > "$VPD_DIR/vpn-director-watchd"
    chmod 711 "$VPD_DIR/vpn-director-watchd"
    touch "$BATS_TEST_TMPDIR/watchd.running"
}

assert_watchd_preserved() {
    assert_success
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "old monitor"
    assert_output --partial "Warning: Failed to stop the server monitor"
    refute_output --partial "Installed the server monitor"
    [[ -f "$BATS_TEST_TMPDIR/watchd.running" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    assert_equal "$(stat -c '%d:%i:%s:%a:%Y' "$VPD_DIR/vpn-director-watchd")" "$old_identity"
    local file
    for file in telegram-bot webui vpn-director.json watchd.unrelated.tmp; do
        assert_equal "$(cat "$VPD_DIR/$file")" "unrelated $file"
    done
}

@test "download_watchd: installs the correct optional asset for every supported architecture" {
    load_installer
    watchd_download_sandbox
    local spec arch suffix
    for spec in "aarch64 arm64" "armv7l arm" "mips mipsle"; do
        read -r arch suffix <<< "$spec"
        export WATCHD_ARCH="$arch"
        run download_watchd
        assert_success
        assert_output --partial "Installed the server monitor"
        assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.url")" "$RELEASE_ASSET_URL/vpn-director-watchd-$suffix"
        assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "new monitor"
        [[ -x "$VPD_DIR/vpn-director-watchd" ]]
        [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    done
    run cat "$BATS_TEST_TMPDIR/watchd.events"
    refute_output --partial "stop:"
}

@test "download_watchd: an unsupported architecture skips download and preserves a running monitor" {
    load_installer
    watchd_download_sandbox
    old_watchd_binary
    export WATCHD_ARCH=x86_64

    run download_watchd

    assert_success
    assert_output --partial "Architecture x86_64 not supported for the server monitor (optional component)"
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "old monitor"
    [[ -f "$BATS_TEST_TMPDIR/watchd.running" ]]
    [[ ! -e "$BATS_TEST_TMPDIR/watchd.events" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
}

@test "download_watchd: a partial failed download is optional and never stops an old monitor" {
    load_installer
    watchd_download_sandbox
    export WATCHD_DOWNLOAD_EXIT=22

    run download_watchd
    assert_success
    assert_output --partial "Warning: Failed to download the server monitor (optional component)"
    [[ ! -e "$VPD_DIR/vpn-director-watchd" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]

    old_watchd_binary
    fake_watchd_stop_init
    run download_watchd
    assert_success
    assert_output --partial "Warning: Failed to download the server monitor (optional component)"
    refute_output --partial "Installed the server monitor"
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "old monitor"
    [[ -f "$BATS_TEST_TMPDIR/watchd.running" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.events")" $'curl\ncurl'
}

@test "download_watchd: stops a running monitor through its init script before replacement" {
    load_installer
    watchd_download_sandbox
    old_watchd_binary
    fake_watchd_stop_init

    run download_watchd

    assert_success
    assert_output --partial "Installed the server monitor"
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "new monitor"
    [[ ! -e "$BATS_TEST_TMPDIR/watchd.running" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    run cat "$BATS_TEST_TMPDIR/watchd.events"
    assert_output --partial $'curl\npidof:alive\nstop:init\nsleep:1'
    refute_output --partial "stop:killall"
}

@test "download_watchd: a failed init stop may replace the binary when the process is gone" {
    load_installer
    watchd_download_sandbox
    old_watchd_binary
    fake_watchd_stop_init
    export WATCHD_STOP_EXIT=1

    run download_watchd

    assert_success
    assert_output --partial "Installed the server monitor"
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "new monitor"
    [[ -x "$VPD_DIR/vpn-director-watchd" ]]
    [[ ! -e "$BATS_TEST_TMPDIR/watchd.running" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
}

@test "download_watchd: preserves the old binary while the monitor survives either init stop result" {
    load_installer
    watchd_download_sandbox
    old_watchd_binary
    fake_watchd_stop_init
    export WATCHD_STOP_SURVIVES=1
    local old_identity code
    old_identity="$(stat -c '%d:%i:%s:%a:%Y' "$VPD_DIR/vpn-director-watchd")"
    for code in 1 0; do
        export WATCHD_STOP_EXIT="$code"
        run download_watchd
        assert_watchd_preserved
        run cat "$BATS_TEST_TMPDIR/watchd.events"
        assert_output --partial "stop:init"
        refute_output --partial "stop:killall"
    done
}

@test "download_watchd: uses killall when init is missing or non-executable and accepts a gone process" {
    load_installer
    watchd_download_sandbox
    local code
    for code in 0 1; do
        old_watchd_binary
        export WATCHD_STOP_EXIT="$code"
        if [[ $code == 1 ]]; then
            fake_watchd_stop_init
            chmod -x "$INIT_DIR/S98vpn-director-watchd"
        fi
        run download_watchd
        assert_success
        assert_output --partial "Installed the server monitor"
        assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "new monitor"
        [[ ! -e "$BATS_TEST_TMPDIR/watchd.running" ]]
        [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    done
    run cat "$BATS_TEST_TMPDIR/watchd.events"
    assert_output --partial $'curl\npidof:alive\nstop:killall\nsleep:1'
    refute_output --partial "stop:init"
}

@test "download_watchd: preserves the old binary while the monitor survives either killall result" {
    load_installer
    watchd_download_sandbox
    old_watchd_binary
    export WATCHD_STOP_SURVIVES=1
    local old_identity code
    old_identity="$(stat -c '%d:%i:%s:%a:%Y' "$VPD_DIR/vpn-director-watchd")"
    for code in 1 0; do
        export WATCHD_STOP_EXIT="$code"
        run download_watchd
        assert_watchd_preserved
        run cat "$BATS_TEST_TMPDIR/watchd.events"
        assert_output --partial "stop:killall"
        refute_output --partial "stop:init"
    done
}

# Bats run suppresses errexit in shell functions; a fresh Bash keeps it active.
watchd_install_failure_case() {
    local failed_operation="$1" invocation="$2" old_identity protected_identity expected_events
    watchd_download_sandbox
    old_watchd_binary
    old_identity="$(stat -c '%d:%i:%s:%a:%Y' "$VPD_DIR/vpn-director-watchd")"
    cat > "$INIT_DIR/S98vpn-director-watchd" <<'EOF'
#!/bin/sh
[ "$#" = 1 ] || exit 99
printf 'init:%s\n' "$1" >> "$BATS_TEST_TMPDIR/watchd.events"
case "$1" in
    stop) rm -f "$BATS_TEST_TMPDIR/watchd.running" ;;
    start|restart) touch "$BATS_TEST_TMPDIR/watchd.running" ;;
    *) exit 99 ;;
esac
EOF
    chmod +x "$INIT_DIR/S98vpn-director-watchd"
    protected_identity="$(stat -c '%d:%i:%s:%a:%Y' "$INIT_DIR/S98vpn-director-watchd" \
        "$VPD_DIR/"{telegram-bot,webui,vpn-director.json,watchd.unrelated.tmp})"

    run bash -e -c '
        source "$1" --source-only
        VPD_DIR="$2"
        INIT_DIR="$3"
        RELEASE_ASSET_URL="$4"
        failed_operation="$5"
        mv() {
            printf "%s\n" "$@" > "$BATS_TEST_TMPDIR/watchd.mv.calls"
            printf "mv\n" >> "$BATS_TEST_TMPDIR/watchd.events"
            [[ $# == 2 && $1 == "$VPD_DIR/vpn-director-watchd.tmp" &&
                $2 == "$VPD_DIR/vpn-director-watchd" ]] || return 99
            [[ $failed_operation != mv ]] || return 1
            command mv "$@"
        }
        chmod() {
            printf "%s\n" "$@" > "$BATS_TEST_TMPDIR/watchd.chmod.calls"
            printf "chmod\n" >> "$BATS_TEST_TMPDIR/watchd.events"
            [[ $# == 2 && $1 == +x && $2 == "$VPD_DIR/vpn-director-watchd" ]] || return 99
            [[ $failed_operation != chmod ]] || return 1
            command chmod "$@"
        }
        rm() {
            local path
            for path in "$@"; do
                case "$path" in -*) continue ;; esac
                printf "%s\n" "$path" >> "$BATS_TEST_TMPDIR/watchd.rm.calls"
                [[ $path == "$BATS_TEST_TMPDIR/"* && $path != *"/../"* ]] || return 99
            done
            command rm "$@"
        }
        case "$6" in
            errexit) download_watchd ;;
            conditional)
                if download_watchd; then
                    :
                else
                    exit 90
                fi
                ;;
            *) exit 99 ;;
        esac
        printf "installer continued\n"
    ' bash "$PROJECT_ROOT/../install.sh" "$VPD_DIR" "$INIT_DIR" "$RELEASE_ASSET_URL" \
        "$failed_operation" "$invocation"

    assert_success
    assert_output --partial "installer continued"
    case "$failed_operation" in
        mv)
            assert_output --partial "Warning: Failed to move the server monitor binary (optional component)"
            assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "old monitor"
            assert_equal "$(stat -c '%d:%i:%s:%a:%Y' "$VPD_DIR/vpn-director-watchd")" "$old_identity"
            [[ ! -e "$BATS_TEST_TMPDIR/watchd.chmod.calls" ]]
            assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.rm.calls")" "$VPD_DIR/vpn-director-watchd.tmp"
            ;;
        chmod)
            assert_output --partial "Warning: Failed to make the server monitor executable (optional component)"
            assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "new monitor"
            [[ ! -x "$VPD_DIR/vpn-director-watchd" ]]
            assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.chmod.calls")" $'+x\n'"$VPD_DIR/vpn-director-watchd"
            [[ ! -e "$BATS_TEST_TMPDIR/watchd.rm.calls" ]]
            ;;
    esac
    assert_watchd_repair_hint
    refute_output --partial "Installed the server monitor"
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    [[ ! -f "$BATS_TEST_TMPDIR/watchd.running" ]]
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.mv.calls")" \
        "$VPD_DIR/vpn-director-watchd.tmp"$'\n'"$VPD_DIR/vpn-director-watchd"
    assert_equal "$(stat -c '%d:%i:%s:%a:%Y' "$INIT_DIR/S98vpn-director-watchd" \
        "$VPD_DIR/"{telegram-bot,webui,vpn-director.json,watchd.unrelated.tmp})" "$protected_identity"
    local file
    for file in telegram-bot webui vpn-director.json watchd.unrelated.tmp; do
        assert_equal "$(cat "$VPD_DIR/$file")" "unrelated $file"
    done
    expected_events=$'curl\npidof:alive\ninit:stop\nsleep:1\npidof:gone\nmv'
    if [[ $failed_operation == chmod ]]; then
        expected_events+=$'\nchmod'
    fi
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.events")" "$expected_events"
}

@test "download_watchd: a failed mv is optional under real errexit and preserves the installed binary" {
    load_installer
    watchd_install_failure_case mv errexit
}

@test "download_watchd: a failed mv is optional for a conditional caller and preserves the installed binary" {
    load_installer
    watchd_install_failure_case mv conditional
}

@test "download_watchd: a failed chmod is optional under real errexit and keeps the moved binary" {
    load_installer
    watchd_install_failure_case chmod errexit
}

@test "download_watchd: a failed chmod is optional for a conditional caller and keeps the moved binary" {
    load_installer
    watchd_install_failure_case chmod conditional
}

# Missing automation must name both the lost function and a recovery action.
assert_watchd_repair_hint() {
    assert_output --partial "automatic failover"
    assert_output --regexp '[Uu]navailable'
    assert_output --regexp '[Rr]einstall|[Rr]epair|[Rr]estore'
}

# Installed files do not establish whether an older daemon is still running.
assert_watchd_partial_start_context() {
    assert_output --partial "automatic failover"
    assert_output --regexp '[Cc]annot start|[Uu]nable to start'
    assert_output --regexp '[Rr]untime state is unconfirmed|[Ww]atchd is (already |still )?running'
    assert_output --regexp '[Rr]einstall|[Rr]epair|[Rr]estore'
    refute_output --regexp '[Mm]onitoring and automatic failover are unavailable'
}

watchd_start_alive_sandbox() {
    watchd_download_sandbox
    old_watchd_binary
    printf 'first-owner-12345\n' > "$BATS_TEST_TMPDIR/watchd.running"
    fake_watchd_init 0
    printf '%s\n' '{"xray":{"clients":["192.168.1.8"]},"monitor":{"enabled":true}}' > "$VPD_DIR/vpn-director.json"
    cat > "$VPD_DIR/vpn-director-watchd" <<'EOF'
#!/bin/sh
printf 'daemon spawn\n' >> "$BATS_TEST_TMPDIR/watchd-start.effects"
exit 99
EOF
    chmod 711 "$VPD_DIR/vpn-director-watchd"
    local file command
    for file in xray.running xray.ready stopped failover_ready routes.state; do
        printf 'preserve %s\n' "$file" > "$BATS_TEST_TMPDIR/$file"
        chmod 600 "$BATS_TEST_TMPDIR/$file"
    done
    for command in nohup pkill ip iptables ip6tables ipset; do
        cat > "$BATS_TEST_TMPDIR/bin/$command" <<'EOF'
#!/bin/sh
printf 'unexpected %s %s\n' "$0" "$*" >> "$BATS_TEST_TMPDIR/watchd-start.effects"
exit 99
EOF
        chmod +x "$BATS_TEST_TMPDIR/bin/$command"
    done
}

watchd_start_alive_case() {
    local component="$1" state="$2" failed_path
    watchd_start_alive_sandbox
    case "$component" in
        binary) failed_path="$VPD_DIR/vpn-director-watchd" ;;
        init) failed_path="$INIT_DIR/S98vpn-director-watchd" ;;
        *) return 99 ;;
    esac
    case "$state" in
        missing) rm "$failed_path" ;;
        non-executable) chmod 600 "$failed_path" ;;
        *) return 99 ;;
    esac
    local -a protected=("$BATS_TEST_TMPDIR/watchd.running" "$VPD_DIR/vpn-director.json"
        "$VPD_DIR/telegram-bot" "$VPD_DIR/webui" "$VPD_DIR/watchd.unrelated.tmp"
        "$BATS_TEST_TMPDIR/xray.running" "$BATS_TEST_TMPDIR/xray.ready"
        "$BATS_TEST_TMPDIR/stopped" "$BATS_TEST_TMPDIR/failover_ready" "$BATS_TEST_TMPDIR/routes.state")
    [[ ! -e "$VPD_DIR/vpn-director-watchd" ]] || protected+=("$VPD_DIR/vpn-director-watchd")
    [[ ! -e "$INIT_DIR/S98vpn-director-watchd" ]] || protected+=("$INIT_DIR/S98vpn-director-watchd")
    local identity contents
    identity="$(stat -c '%d:%i:%s:%a:%Y' "${protected[@]}")"
    contents="$(cat "${protected[@]}")"

    run bash -c '
        source "$1" --source-only
        VPD_DIR="$2" INIT_DIR="$3"
        kill() {
            printf "unexpected kill %s\n" "$*" >> "$BATS_TEST_TMPDIR/watchd-start.effects"
            return 99
        }
        start_watchd
    ' bash "$PROJECT_ROOT/../install.sh" "$VPD_DIR" "$INIT_DIR"

    assert_success
    assert_equal "$(stat -c '%d:%i:%s:%a:%Y' "${protected[@]}")" "$identity"
    assert_equal "$(cat "${protected[@]}")" "$contents"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.running")" "first-owner-12345"
    [[ ! -e "$BATS_TEST_TMPDIR/watchd.calls" ]]
    [[ ! -e "$BATS_TEST_TMPDIR/watchd-start.effects" ]]
    if [[ -e "$BATS_TEST_TMPDIR/watchd.events" ]]; then
        assert_equal "$(sed '/^pidof:alive$/d' "$BATS_TEST_TMPDIR/watchd.events")" ""
    fi
    case "$state" in
        missing) [[ ! -e "$failed_path" ]] ;;
        non-executable) [[ ! -x "$failed_path" ]] ;;
    esac
    assert_watchd_partial_start_context
    assert_output --partial "$failed_path"
    assert_output --partial "$INIT_DIR/S98vpn-director-watchd start"
    refute_output --partial "Server monitor started"
}

@test "watchd install: missing binary preserves a synthetic live owner and reports start-only failure" {
    load_installer
    watchd_start_alive_case binary missing
}

@test "watchd install: non-executable binary preserves a synthetic live owner and reports start-only failure" {
    load_installer
    watchd_start_alive_case binary non-executable
}

@test "watchd install: missing init preserves a synthetic live owner and reports start-only failure" {
    load_installer
    watchd_start_alive_case init missing
}

@test "watchd install: non-executable init preserves a synthetic live owner and reports start-only failure" {
    load_installer
    watchd_start_alive_case init non-executable
}

@test "watchd install: independent startup and visible partial installation" {
    load_installer
    fake_watchd_init 0
    printf '{}\n' > "$VPD_DIR/vpn-director.json"
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"

    run start_watchd

    assert_success
    assert_output --partial "Server monitor started"
    assert_output --partial "automatic failover"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.calls")" "start"
    [[ ! -e "$VPD_DIR/telegram-bot" ]]
    [[ ! -e "$VPD_DIR/telegram-bot.json" ]]

    printf '%s\n' '{"bot_token":"","allowed_users":[],"log_level":"info","update_check_interval":"1h"}' > "$VPD_DIR/telegram-bot.json"
    run start_watchd

    assert_success
    assert_output --partial "Server monitor started"
    assert_output --partial "automatic failover"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.calls")" $'start\nstart'
    assert_equal "$(cat "$VPD_DIR/telegram-bot.json")" '{"bot_token":"","allowed_users":[],"log_level":"info","update_check_interval":"1h"}'
}

@test "watchd install: missing or non-executable binary explains start failure and runtime uncertainty" {
    load_installer
    fake_watchd_init 0
    local state
    for state in missing non-executable; do
        if [[ $state == non-executable ]]; then
            printf 'partial monitor\n' > "$VPD_DIR/vpn-director-watchd"
            chmod 600 "$VPD_DIR/vpn-director-watchd"
        fi

        run start_watchd

        assert_success
        assert_watchd_partial_start_context
        assert_output --partial "$VPD_DIR/vpn-director-watchd"
        assert_output --partial "$INIT_DIR/S98vpn-director-watchd start"
        refute_output --partial "Server monitor started"
        [[ ! -e "$BATS_TEST_TMPDIR/watchd.calls" ]]
    done
    assert_equal "$(cat "$VPD_DIR/vpn-director-watchd")" "partial monitor"
    [[ ! -x "$VPD_DIR/vpn-director-watchd" ]]
}

@test "watchd install: missing or non-executable init explains start failure and runtime uncertainty" {
    load_installer
    touch "$VPD_DIR/vpn-director-watchd"
    chmod +x "$VPD_DIR/vpn-director-watchd"
    local state
    for state in missing non-executable; do
        if [[ $state == non-executable ]]; then
            fake_watchd_init 0
            chmod -x "$INIT_DIR/S98vpn-director-watchd"
        fi

        run start_watchd

        assert_success
        assert_output --partial "Server monitor init script not found, skipping start"
        assert_watchd_partial_start_context
        assert_output --partial "$INIT_DIR/S98vpn-director-watchd start"
        refute_output --partial "Server monitor started"
        [[ ! -e "$BATS_TEST_TMPDIR/watchd.calls" ]]
    done
}

@test "watchd install: failed start explains unavailable automation without stopping Web UI" {
    load_installer
    touch "$VPD_DIR/"{webui,vpn-director-watchd}
    chmod +x "$VPD_DIR/"{webui,vpn-director-watchd}
    fake_init 0
    fake_watchd_init 1

    run bash -c '
        source "$1" --source-only
        VPD_DIR="$2" INIT_DIR="$3"
        lan_ip() { printf "192.0.2.1\n"; }
        start_watchd
        start_webui
        printf "installer continued\n"
    ' bash "$PROJECT_ROOT/../install.sh" "$VPD_DIR" "$INIT_DIR"

    assert_success
    assert_output --partial "Failed to start the server monitor"
    assert_watchd_repair_hint
    assert_output --partial "/tmp/vpn-director-watchd.log"
    assert_output --partial "Web UI started: https://192.0.2.1:8444"
    assert_output --partial "installer continued"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.calls")" "start"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/init.calls")" "start"
}

@test "watchd install: failed first download is visible and main still starts Web UI" {
    load_installer
    watchd_download_sandbox
    export WATCHD_DOWNLOAD_EXIT=22
    touch "$VPD_DIR/webui"
    chmod +x "$VPD_DIR/webui"
    fake_init 0
    fake_watchd_init 0
    local step
    for step in print_header detect_platform check_environment resolve_release_tag create_directories \
        download_scripts load_platform_lib download_telegram_bot download_webui generate_tls_cert print_next_steps; do
        eval "$step() { printf '%s\\n' '$step' >> \"\$BATS_TEST_TMPDIR/main.calls\"; }"
    done
    setup_webui_config() {
        printf '{}\n' > "$VPD_DIR/vpn-director.json"
    }

    run main

    assert_success
    assert_output --partial "Warning: Failed to download the server monitor (optional component)"
    assert_watchd_repair_hint
    assert_output --partial "Web UI started"
    refute_output --partial "Server monitor started"
    assert_equal "$(cat "$BATS_TEST_TMPDIR/init.calls")" "start"
    [[ ! -e "$BATS_TEST_TMPDIR/watchd.calls" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd" ]]
    [[ ! -e "$VPD_DIR/vpn-director-watchd.tmp" ]]
    assert_equal "$(cat "$BATS_TEST_TMPDIR/watchd.events")" "curl"
}

@test "main: downloads the optional monitor and starts it only after the config exists" {
    load_installer
    local step
    for step in print_header detect_platform check_environment resolve_release_tag create_directories \
        download_scripts load_platform_lib download_telegram_bot download_webui download_watchd \
        generate_tls_cert start_webui print_next_steps; do
        eval "$step() { printf '%s\\n' '$step' >> \"\$BATS_TEST_TMPDIR/main.calls\"; }"
    done
    setup_webui_config() {
        printf '{}\n' > "$VPD_DIR/vpn-director.json"
        printf 'setup_webui_config\n' >> "$BATS_TEST_TMPDIR/main.calls"
    }
    start_watchd() {
        [[ -f "$VPD_DIR/vpn-director.json" ]] || return 1
        printf 'start_watchd\n' >> "$BATS_TEST_TMPDIR/main.calls"
    }

    run main

    assert_success
    run cat "$BATS_TEST_TMPDIR/main.calls"
    assert_output $'print_header\ndetect_platform\ncheck_environment\nresolve_release_tag\ncreate_directories\ndownload_scripts\nload_platform_lib\ndownload_telegram_bot\ndownload_webui\ndownload_watchd\ngenerate_tls_cert\nsetup_webui_config\nstart_webui\nstart_watchd\nprint_next_steps'
}
