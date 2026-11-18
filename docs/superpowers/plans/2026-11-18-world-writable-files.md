# World-Writable Files on Asuswrt-Merlin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Whatever umask its caller brings, nothing VPN Director creates is writable by group or
others. `telegram-bot.json` is 0600 from the moment it is written, and install and update repair
what earlier releases left.

**Architecture:** Every script a router executes gets `umask 022` as its first command, ahead of
the bash hand-over block. `setup_telegram_bot.sh` writes its config with `mktemp`, then
`chmod 600`, then `mv`. `install.sh` and the update script each run `repair_modes` once: a
`chmod go-w` per path of ours, and `chmod 600` on the bot config.

**Tech Stack:** POSIX sh and bash 5; BusyBox 1.25 `chmod` on Merlin; Bats (`router/test`); Go
tests of the update script template (`server/internal/updater`).

**Spec:** `docs/superpowers/specs/2026-11-18-world-writable-files-design.md`

## Global Constraints

- **`umask 022` is the first command of every script a router executes.** The first command is the
  first line that is not the shebang, a comment or blank.
  - The scripts:
    - `router/opt/vpn-director/vpn-director.sh`, `configure.sh`, `import_server_list.sh`,
      `setup_telegram_bot.sh`, `lib/send-email.sh`, and `install.sh`;
    - `router/opt/etc/init.d/S98telegram-bot`, `S98vpn-director-webui`,
      `S98vpn-director-watchd`, `S99vpn-director`;
    - `router/opt/vpn-director/hooks/firewall-start`, `wan-event`, `nat-start`;
    - `router/opt/etc/ndm/{netfilter.d,wan.d,iflayerchanged.d}/50-vpn-director.sh`;
    - `server/internal/updater/update_script.sh.tmpl`.
  - In a script with the hand-over block, the line goes before
    `if [ -z "${BASH_VERSION:-}" ]`. `bash script` and `curl … | bash` skip the block, so a line
    inside it would never run there.
- **Unchanged:** `lib/fwsnap-restore.sh` keeps its `umask 077`, and `hooks/legacy/*` keep their
  bytes, which `platform_merlin.bats` pins by md5. The Go daemons do not change.
- **Secrets are 0600 from the moment they are written.** `telegram-bot.json` goes through `mktemp`
  in its own directory, then `chmod 600`, then `mv -f`.
- **`repair_modes`** exists twice, in `install.sh` and in the update script. Each path is handled
  on its own, and a missing path is skipped:
  - `chmod -R go-w` on `/opt/vpn-director`, the whole tree as the owner chose, their own
    `backup-*`, `saved/`, `saved-config.json` and `myvpn-config` included;
  - `chmod -R go-w` on `/tmp/tunnel_director` and `/tmp/xray_tproxy`;
  - `chmod go-w` on `/tmp/vpn-director.log` and `/var/lock/vpn-director.lock`;
  - `chmod 600` on `/opt/vpn-director/telegram-bot.json`.

  After trying every path, it returns 1 if one failed. The caller warns and goes on: a repair never
  fails an install or an update.
- **Ruling, made after the design:** `/tmp/xray_tproxy` is 0777 on the RT-AX86U, and its `ready`
  file - the marker watchd waits for before a failover - is 0666. `/var/lock/vpn-director.lock` is
  0666 too. Both are runtime paths of ours, like `/tmp/tunnel_director` and the log, so they join
  the repair. Cost if wrong: two harmless `chmod`s.
- **Tests run only through `build-forge:build-runner`; never two bats runs at once.**
  - The full bats suite is the CI command:
    `bats router/test/*.bats router/test/unit router/test/integration`.
  - Go: `cd server && go test ./internal/updater -count=1`.
- **Commit messages** follow the repository's `fix(<scope>): …`.

## Review Focus

1. **A script started as `bash script.sh` or `curl … | bash` skips the hand-over block.** A
   `umask 022` inside the block never runs there. Task 1's probe runs each entry script's head
   under bash as well as under sh.
2. **A `telegram-bot.json` already there at 0666, as on the RT-AX86U.** A write in place keeps the
   old mode. Task 2's test starts from such a file.
3. **A symlink in `/opt/vpn-director` that points outside, such as an owner's link.** The repair
   must leave the target's mode alone. Task 3's test plants one. On the router, BusyBox's
   `chmod -R` skips a symlink it meets during recursion; acceptance checks it on the RT-AX86U.
4. **A `chmod` that fails under the update script's `set -e`,** say on a path that vanished or a
   read-only mount. The update must not end. Task 4 checks that the call sits in `if !`, and Task 3
   that a failing `chmod` leaves the install going.
5. **A fresh install,** with nothing of ours in `/tmp` yet and no `telegram-bot.json`. The repair
   succeeds without a warning; Task 3's test covers it.

---

### Task 1: `umask 022` first in every executed script

**Files:**
- Modify: the six entry scripts, the four init scripts, the three Merlin hooks and the three NDM
  hooks listed in Global Constraints.
- Modify: `CLAUDE.md` ("Shell Conventions"); `.claude/rules/shell-conventions.md` ("Script
  Structure", and a new Known Pitfall: "Asuswrt-Merlin runs everything under umask 0").
- Test: `router/test/unit/entrypoints.bats`

**Interfaces:**
- Produces: `executed_scripts()` in `entrypoints.bats`: `entry_scripts`, then the init scripts and
  the hooks.

- [ ] **Step 1: Write the failing tests** in `entrypoints.bats`

```bash
# Every script a router executes: the entry scripts, the init scripts that start the daemons,
# and the firmware hooks.
executed_scripts() {
    entry_scripts
    printf '%s\n' "$PROJECT_ROOT"/opt/etc/init.d/S9* \
        "$SCRIPTS_DIR/hooks/firewall-start" "$SCRIPTS_DIR/hooks/wan-event" "$SCRIPTS_DIR/hooks/nat-start" \
        "$PROJECT_ROOT"/opt/etc/ndm/*/50-vpn-director.sh
}

@test "every executed script sets umask 022 before anything else" {
    local script first
    while IFS= read -r script; do
        first=$(awk 'NR > 1 && !/^[[:space:]]*(#|$)/ { print; exit }' "$script")
        [[ $first == 'umask 022' ]] || fail "$script starts with '$first', not 'umask 022'"
    done < <(executed_scripts)
}

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
```

The probe pins the spec's "the CLI, called by a 0-umask caller, creates its log, a dump and the
Tunnel Director state without group or other write" at the source. `vpn-director.sh` is an entry
script, and what it creates takes the process's umask, which the probe reads.

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/entrypoints.bats`
Expected: both new tests FAIL. The first command is the `if` of the block or `PATH=`, and the probe
prints `0000`.

- [ ] **Step 3: Add `umask 022`** to the 16 scripts, as the first command.
  - In the six entry scripts, it goes after the comment above the hand-over block, before its `if`.
  - Everywhere else, it goes before `PATH=`.
  - Each gets a one-line comment: Asuswrt-Merlin runs everything under umask 0.

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/entrypoints.bats router/test/unit/hooks.bats router/test/unit/install.bats`
Expected: PASS.

- [ ] **Step 5: Docs**
  - `CLAUDE.md`, "Shell Conventions": every executed script starts with `umask 022`, ahead of the
    hand-over; secrets are written with `mktemp`, `chmod 600` and `mv`.
  - `shell-conventions.md`, new pitfall:
    - the measured modes on the RT-AX86U, as in the spec's "Why";
    - why it matters: root runs from those directories, and `nobody` processes exist;
    - the rule above;
    - `fwsnap-restore.sh`'s `umask 077`;
    - `repair_modes` in install and update.

- [ ] **Step 6: Run the full bats suite**
Expected: everything passes.

- [ ] **Step 7: Commit**

```bash
git add install.sh router/opt router/test/unit/entrypoints.bats CLAUDE.md .claude/rules/shell-conventions.md
git commit -m "fix(shell): every script a router executes starts with umask 022"
```

### Task 2: `telegram-bot.json` written 0600

**Files:**
- Modify: `router/opt/vpn-director/setup_telegram_bot.sh`
  - the interactive flow becomes `main()`;
  - add `bot_config_write`;
  - add a `--source-only` guard, the one `install.sh` ends with.
- Test: Create `router/test/unit/setup_telegram_bot.bats`. Add it to the test tree in
  `.claude/rules/testing.md`.

**Interfaces:**
- Produces: `bot_config_write <file> <token> <users_json>`.
  - It writes `{bot_token, allowed_users, log_level: "info", update_check_interval: "24h"}` - the
    keys and values the script writes today.
  - The write goes through `mktemp "<file>.XXXXXX"`, `chmod 600` and `mv -f`.
  - On a failure it returns 1, removes its temp file and leaves `<file>` untouched.

- [ ] **Step 1: Write the failing tests**

```bash
#!/usr/bin/env bats

load '../test_helper'

setup() {
    source "$SCRIPTS_DIR/setup_telegram_bot.sh" --source-only
    umask 0    # Asuswrt-Merlin's
    CONFIG="$BATS_TEST_TMPDIR/telegram-bot.json"
}

@test "bot_config_write: the config is 0600 under a caller's umask 0" {
    bot_config_write "$CONFIG" "123:abc" '["alice"]'
    assert_equal "$(stat -c %a "$CONFIG")" 600
    assert_equal "$(jq -c . "$CONFIG")" '{"bot_token":"123:abc","allowed_users":["alice"],"log_level":"info","update_check_interval":"24h"}'
}

@test "bot_config_write: a 0666 config from before is replaced by a 0600 one" {
    printf '{}\n' > "$CONFIG"
    chmod 666 "$CONFIG"
    bot_config_write "$CONFIG" "123:abc" '["alice"]'
    assert_equal "$(stat -c %a "$CONFIG")" 600
}

@test "bot_config_write: a failed write keeps the old config and leaves no temp file" {
    printf '{"old":1}\n' > "$CONFIG"
    run bot_config_write "$CONFIG" "123:abc" 'not json'
    assert_failure
    assert_equal "$(cat "$CONFIG")" '{"old":1}'
    run compgen -G "$CONFIG.*"
    assert_failure
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/setup_telegram_bot.bats`
Expected: FAIL. Without the guard, sourcing runs the wizard, whose `read < /dev/tty` fails; or
`bot_config_write: command not found`.

- [ ] **Step 3: Implement** `bot_config_write`, `main` and the guard.
  - `main` calls `bot_config_write "$CONFIG_FILE" "$BOT_TOKEN" "$USERS_JSON"`.
  - If the write fails, `main` prints `Error: could not write $CONFIG_FILE` and exits 1.

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/setup_telegram_bot.bats router/test/unit/entrypoints.bats`
Expected: PASS. The script is still an entry script: its block is unchanged and `umask 022` is
still its first command.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/setup_telegram_bot.sh router/test/unit/setup_telegram_bot.bats .claude/rules/testing.md
git commit -m "fix(bot): setup_telegram_bot.sh writes telegram-bot.json 0600"
```

### Task 3: `install.sh` repairs what an earlier install left

**Files:**
- Modify: `install.sh`. Add `repair_modes()`. `main` runs it right after `install_hooks` as
  `repair_modes || print_info "Warning: …"`; the warning names the manual fix,
  `chmod -R go-w $VPD_DIR`.
- Test: `router/test/unit/install.bats`
  - the `repair_modes` tests below;
  - `repair_modes` added after `install_hooks` to the stubbed steps and the expected calls of
    `"main: downloads the optional monitor and starts it only after the config exists"`.

**Interfaces:**
- Produces: `repair_modes` in `install.sh`. It takes no arguments.
  - Paths: `$VPD_DIR`, `${INSTALL_ROOT}/tmp/tunnel_director`, `${INSTALL_ROOT}/tmp/xray_tproxy`,
    `${INSTALL_ROOT}/tmp/vpn-director.log`, `${INSTALL_ROOT}/var/lock/vpn-director.lock` and
    `$VPD_DIR/telegram-bot.json`, with the Global Constraints' semantics.
  - For each failure it prints
    `Warning: could not remove write access for others from <path>` with `print_info`.
  - It returns 1 when one failed and 0 otherwise.

- [ ] **Step 1: Write the failing tests** in `install.bats`. Every test calls `load_installer`,
  then sets `INSTALL_ROOT="$BATS_TEST_TMPDIR/root"`.

```bash
@test "repair_modes: takes group and other write away from what an earlier install left" {
    # Given: 0777 directories and 0666 files at every repaired path - lib, data/ipsets/ru.dump,
    # an owner's backup-v0.10.0/vpn-director.sh (0777), tmp/tunnel_director/tun_dir_tables,
    # tmp/xray_tproxy/ready, tmp/vpn-director.log, var/lock/vpn-director.lock, telegram-bot.json.
    # chmod only those paths, never $INSTALL_ROOT or its tmp and var.
    run repair_modes
    assert_success
    run find "$VPD_DIR" "$INSTALL_ROOT/tmp/tunnel_director" "$INSTALL_ROOT/tmp/xray_tproxy" \
        "$INSTALL_ROOT/tmp/vpn-director.log" "$INSTALL_ROOT/var/lock/vpn-director.lock" ! -type l -perm /022
    assert_output ""
    assert_equal "$(stat -c %a "$VPD_DIR/telegram-bot.json")" 600
    assert_equal "$(stat -c %a "$VPD_DIR/lib")" 755
    assert_equal "$(stat -c %a "$VPD_DIR/data/ipsets/ru.dump")" 644
    assert_equal "$(stat -c %a "$VPD_DIR/backup-v0.10.0/vpn-director.sh")" 755
}

@test "repair_modes: a symlink in the tree leaves its target's mode alone" {
    # $BATS_TEST_TMPDIR/outside at 0666, linked as $VPD_DIR/link
    run repair_modes
    assert_success
    assert_equal "$(stat -c %a "$BATS_TEST_TMPDIR/outside")" 666
}

@test "repair_modes: a fresh install has nothing to repair and no warning" {
    run repair_modes
    assert_success
    refute_output --partial "Warning"
}

@test "repair_modes: a chmod that fails is a warning, and every other path is still repaired" {
    # telegram-bot.json at 0666; a chmod() shell function fails "-R go-w $VPD_DIR" and runs
    # "command chmod" for everything else
    run repair_modes
    assert_failure
    assert_output --partial "Warning: could not remove write access for others from $VPD_DIR"
    assert_equal "$(stat -c %a "$VPD_DIR/telegram-bot.json")" 600
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `bats router/test/unit/install.bats`
Expected: the four new tests FAIL with `repair_modes: command not found`, and the main order test
FAILs because `repair_modes` is not in the calls.

- [ ] **Step 3: Implement** `repair_modes` and its call in `main`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `bats router/test/unit/install.bats`
Expected: PASS.

- [ ] **Step 5: Run the full bats suite**
Expected: everything passes.

- [ ] **Step 6: Commit**

```bash
git add install.sh router/test/unit/install.bats
git commit -m "fix(install): the installer takes write for others away from what an earlier install left"
```

### Task 4: The update script runs under `umask 022` and repairs the modes

**Files:**
- Modify: `server/internal/updater/update_script.sh.tmpl`
  - `umask 022` becomes the first command, before `set -e`;
  - a `repair_modes()` function, the twin of `install.sh`'s, with the paths fixed: `/opt/...`,
    `/tmp/...`, `/var/lock/...`;
  - a step `5c.` right after 5b:

    ```sh
    log "Removing write access for others"
    if ! repair_modes >> "$LOG_FILE" 2>&1; then
        log "WARN: …"
    fi
    ```

    The WARN names `chmod -R go-w /opt/vpn-director`.
- Modify: `server/internal/updater/testdata/update_script.golden.sh`, regenerated.
- Modify: `README.md`. Add an upgrade note before "Upgrading from v0.22.0 or earlier": "Upgrading
  from v0.23.0 or earlier on Asuswrt-Merlin". It says:
  - the firmware runs everything under umask 0;
  - earlier releases left `/opt/vpn-director` writable by every process, `telegram-bot.json` and
    the bot token included;
  - the update takes that write away from the whole tree, the owner's own files included, and from
    our files in `/tmp` and `/var/lock`;
  - it makes `telegram-bot.json` 0600.

  `README.ru.md` gets the same note beside its v0.22.0 note.
- Test: `server/internal/updater/script_test.go`

- [ ] **Step 1: Write the failing tests** in `script_test.go`, using the existing helpers
  `writeTestManifest`, `validOpts` and `afterOnExit`.

```go
func TestGenerateScript_SetsUmask022BeforeAnything(t *testing.T) {
	// render as TestGenerateScript_SetsTheHooksOnceTheFilesAreInPlace does; then the first line
	// after the shebang that is neither blank nor a comment must be exactly "umask 022"
}

func TestGenerateScript_RepairsModesOnceTheFilesAreInPlace(t *testing.T) {
	// body := afterOnExit(t, script)
	// call := strings.Index(body, "if ! repair_modes"): present, after the
	// "/opt/vpn-director/vpn-director.sh hooks" call, before "# 6. Start everyone"
	// and the whole script contains each of:
	//   "chmod -R go-w /opt/vpn-director", "/tmp/tunnel_director", "/tmp/xray_tproxy",
	//   "/tmp/vpn-director.log", "/var/lock/vpn-director.lock",
	//   "chmod 600 /opt/vpn-director/telegram-bot.json"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./internal/updater -run 'TestGenerateScript_(SetsUmask022BeforeAnything|RepairsModesOnceTheFilesAreInPlace)' -count=1`
Expected: FAIL. The first command is `set -e`, and `repair_modes` is missing.

- [ ] **Step 3: Implement** the template changes.

- [ ] **Step 4: Regenerate the golden file, then run the package**

Run: `cd server && UPDATE_GOLDEN=1 go test ./internal/updater -run TestGenerateScript_Golden -count=1`,
then `cd server && go test ./internal/updater -count=1`.
Expected: PASS. The syntax test (`sh -n`) passes on the new template.

- [ ] **Step 5: Write the README notes.**

- [ ] **Step 6: Commit**

```bash
git add server/internal/updater README.md README.ru.md
git commit -m "fix(updater): the update runs under umask 022 and takes write for others away from an earlier install"
```

## After the tasks

- Final review of the whole branch, `259c094..HEAD`.
- Then, with the owner's go-ahead:
  1. run `git rm -r docs/superpowers/` before the PR, and open the PR;
  2. release;
  3. update the RT-AX86U through the updater;
  4. check there that nothing under `/opt/vpn-director`, `/tmp/tunnel_director` or
     `/tmp/xray_tproxy` is group- or other-writable, that `telegram-bot.json` is 0600, and that a
     symlink planted in the tree kept its target's mode.
