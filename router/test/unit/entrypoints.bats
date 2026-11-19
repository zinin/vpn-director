#!/usr/bin/env bats

load '../test_helper'

# The scripts a router executes directly - by hand, from S99vpn-director, from
# an NDM hook, or from the Go daemons (exec.CommandContext on the CLI path).
# KeeneticOS has no /usr/bin/env and its root filesystem is a read-only
# squashfs, so "#!/usr/bin/env bash" cannot be repaired on the device: every
# one of these has to start as a POSIX shell and hand over to bash itself.
# Library files are excluded on purpose - they are only ever sourced, except
# lib/send-email.sh, which S99vpn-director executes.
entry_scripts() {
    printf '%s\n' \
        "$SCRIPTS_DIR/vpn-director.sh" \
        "$SCRIPTS_DIR/configure.sh" \
        "$SCRIPTS_DIR/import_server_list.sh" \
        "$SCRIPTS_DIR/setup_telegram_bot.sh" \
        "$SCRIPTS_DIR/lib/send-email.sh" \
        "$PROJECT_ROOT/../install.sh"
}

# Every script a router executes: the entry scripts, the init scripts that start the daemons,
# and the firmware hooks - every file of their directories, so a new init script or hook joins
# the check on its own. hooks/legacy/ holds the earlier releases' hook bodies, which the hook
# migration matches byte for byte and nothing executes.
executed_scripts() {
    entry_scripts
    printf '%s\n' "$PROJECT_ROOT"/opt/etc/init.d/* "$PROJECT_ROOT"/opt/etc/ndm/*/*
    find "$SCRIPTS_DIR/hooks" -maxdepth 1 -type f | sort
}

@test "executed scripts do not depend on /usr/bin/env" {
    local script hit
    while IFS= read -r script; do
        [[ -f "$script" ]] || fail "missing entry script: $script"
        assert_equal "$(head -1 "$script")" '#!/bin/sh'
        # Naming it in a comment is fine; a shebang or a command is not.
        while IFS= read -r hit; do
            [[ -z "$hit" || "${hit#*:}" == \#* ]] ||
                fail "$script still reaches for /usr/bin/env, which KeeneticOS lacks: $hit"
        done < <(grep -n '/usr/bin/env' "$script" || true)
    done < <(entry_scripts)
}

@test "executed scripts carry the same hand-over block" {
    # By absolute path, Entware's bash first. PATH is not usable: Merlin's
    # /bin/sh has no "command" builtin and its /bin/bash is busybox.
    local script
    while IFS= read -r script; do
        run grep -c 'for _vpd_bash in /opt/bin/bash /usr/bin/bash /bin/bash' "$script"
        assert_success
        assert_output "1"
        run grep -c 'exec "\$_vpd_bash" "\$0" "\$@"' "$script"
        assert_success
        assert_output "1"
        # A candidate is exec-ed only after it proves it really is bash.
        run grep -c '"\$_vpd_bash" -c '"'"'\[ -n "\$BASH_VERSION" \]'"'" "$script"
        assert_success
        assert_output "1"
    done < <(entry_scripts)
}

# The block lifted out of the CLI, its candidate list replaced by the caller's,
# given a body that reports which interpreter it ended up in.
make_probe_with_candidates() {
    local probe="$1"; shift
    sed -n '1,/^fi$/p' "$SCRIPTS_DIR/vpn-director.sh" |
        sed "s|/opt/bin/bash /usr/bin/bash /bin/bash|$*|" > "$probe"
    printf 'printf "BASH=%%s\\n" "${BASH_VERSION:-none}"\n' >> "$probe"
    chmod +x "$probe"
}

# On Asuswrt-Merlin /bin/bash is a busybox symlink: a POSIX shell that never
# sets BASH_VERSION. Exec-ing it re-runs the block, which execs it again,
# forever. A /bin/sh symlink named bash reproduces that here.
@test "the hand-over block skips a bash that is really a POSIX shell" {
    local fake="$BATS_TEST_TMPDIR/busybox/bash" probe="$BATS_TEST_TMPDIR/probe.sh"
    mkdir -p "${fake%/*}"
    ln -s /bin/sh "$fake"
    make_probe_with_candidates "$probe" "$fake" "$(command -v bash)"

    run timeout 5 sh "$probe"
    assert_success
    assert_output --partial "BASH="
    refute_output --partial "BASH=none"
}

@test "the hand-over block gives up instead of looping when no candidate is bash" {
    local fake="$BATS_TEST_TMPDIR/busybox/bash" probe="$BATS_TEST_TMPDIR/probe.sh"
    mkdir -p "${fake%/*}"
    ln -s /bin/sh "$fake"
    make_probe_with_candidates "$probe" "$fake"

    run timeout 5 sh "$probe"
    assert_failure 1
    assert_output --partial "bash not found"
}

@test "the hand-over block runs the body under real bash" {
    # The block itself, lifted out of the CLI and given a body that reports
    # which interpreter it ended up in.
    local probe="$BATS_TEST_TMPDIR/probe.sh"
    sed -n '1,/^fi$/p' "$SCRIPTS_DIR/vpn-director.sh" > "$probe"
    printf 'printf "BASH=%%s\\n" "${BASH_VERSION:-none}"\n' >> "$probe"
    chmod +x "$probe"

    # Started by a POSIX shell, the way KeeneticOS starts our hooks' children.
    run sh "$probe"
    assert_success
    refute_output --partial "BASH=none"

    # And started through its own shebang, the way a router runs the CLI.
    run "$probe"
    assert_success
    refute_output --partial "BASH=none"
}

@test "vpn-director.sh started by /bin/sh runs its bash body" {
    run sh "$SCRIPTS_DIR/vpn-director.sh" --help
    assert_success
    assert_output --partial "Usage:"
}

# Asuswrt-Merlin runs everything under umask 0 - the firmware's services, cron, the hooks, an
# ssh session - and what a script creates without a mode takes its caller's umask: directories
# 0777, files 0666, which every process on the router can write.
@test "every executed script sets umask 022 before anything else" {
    local script first
    while IFS= read -r script; do
        first=$(awk 'NR > 1 && !/^[[:space:]]*(#|$)/ { print; exit }' "$script")
        [[ $first == 'umask 022' ]] || fail "$script starts with '$first', not 'umask 022'"
    done < <(executed_scripts)
}

# Ahead of the hand-over block, not inside it: "bash script" and "curl ... | bash" skip the block.
@test "an entry script runs under umask 022 whatever its caller's umask, from sh or from bash" {
    local script probe="$BATS_TEST_TMPDIR/probe"
    while IFS= read -r script; do
        sed -n '1,/^fi$/p' "$script" > "$probe"
        printf 'umask\n' >> "$probe"
        chmod +x "$probe"
        run sh -c 'umask 0; exec "$1"' _ "$probe"
        assert_success
        assert_output "0022"
        run bash -c 'umask 0; bash "$1"' _ "$probe"
        assert_success
        assert_output "0022"
    done < <(entry_scripts)
}
