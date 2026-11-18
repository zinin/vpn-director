# Merlin Hook Lines Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install and update stop overwriting Asuswrt-Merlin's shared hooks: our logic moves into files
of our own, and `/jffs/scripts/<hook>` gets one marked line that calls it, the rest of the file left as
it is.

**Architecture:** `router/jffs/scripts/*` move to `router/opt/vpn-director/hooks/` and detach their
apply. A new pair of platform-contract functions puts the line into the shared hook (Merlin) or does
nothing (KeeneticOS), cutting out a body an earlier release wrote. `vpn-director.sh hooks` runs it;
`install.sh` and the update script call that command; the full `status` shows the lines.

**Tech Stack:** bash 4 (the CLI and the platform library), POSIX sh (the hooks), Go (the updater's
script template), Bats with bats-assert, Go tests.

**Spec:** `docs/superpowers/specs/2026-11-18-merlin-hook-lines-design.md`

## Global Constraints

- The three Merlin hooks: `firewall-start`, `wan-event`, `nat-start`. KeeneticOS changes nothing.
- The line for hook `<h>`, with `D` = `${VPD_HOOKS_DIR:-/opt/vpn-director/hooks}`:
  `[ -x D/<h> ] && D/<h> "$@" # vpn-director`. Ours is any line ending in ` # vpn-director`.
- The shared hooks live in `${VPD_JFFS_SCRIPTS_DIR:-/jffs/scripts}`.
- Earlier bodies: `D/legacy/<h>.<version>`, byte-exact copies of the released files (md5 below).
- A write goes through `<dir>/.<h>.vpd-new` and `mv -f`, mode 755, only when the text or the mode has
  to change. Nothing else in a shared hook is changed, removed or moved.
- An apply never writes to `/jffs`. The hooks keep `--wait apply` without `--unless-stopped`.
- The hooks are POSIX sh for Merlin's BusyBox: no `command` builtin.
- Commits: conventional English subjects as in `git log`, no trailer.
- Full suite: `bats router/test/*.bats router/test/unit router/test/integration`; Go:
  `cd server && go test ./internal/updater/...` (`make -C server test` needs `make web-embed` first).

## Review Focus

- The RT-AX86U's `firewall-start` (the v0.14.0 body, then `/jffs/scripts/guest-lan-port &`): the body
  goes, the owner's line stays, ours follows the `#!` line. Pinned in Task 2.
- A WAN event other than `connected`: the owner's lines after ours in `wan-event` still run - the
  `exit` is inside our script now. Pinned in Task 2.
- A second `hooks` run, and every update after the first: nothing written to `/jffs`, the file's
  inode and mtime unchanged. Pinned in Task 2.
- A hook without `#!`, or one that cannot be written: left as it was, a warning, exit 1 - and install
  and update carry on. Pinned in Tasks 2 and 4.
- An add-on's line after ours in `firewall-start` runs before our apply ends (the apply is detached).
  Pinned in Tasks 1 and 2.

---

### Task 1: Our hook scripts move to `/opt/vpn-director/hooks` and detach the apply

**Files:**
- Move: `router/jffs/scripts/{firewall-start,wan-event,nat-start}` → `router/opt/vpn-director/hooks/`
  (`git mv`; `router/jffs/` is then empty and goes)
- Modify: `router/files.manifest` (the three `merlin router/jffs/scripts/*` lines become
  `merlin router/opt/vpn-director/hooks/<h>`)
- Modify: `router/test/unit/hooks.bats`, `server/internal/updater/manifest_test.go`
- Modify: `CLAUDE.md` (the three architecture rows), `.claude/rules/shell-conventions.md` (the
  paragraph "Either way the apply after the last firewall start has to run")

**Interfaces:**
- Produces: `/opt/vpn-director/hooks/<h>` - same argv as the firmware's hook, same environment seams
  (`VPD_SCRIPT`, `VPD_RESTORE`); returns once the restore is done, the apply still running.

- [ ] **Step 1: Point the Merlin tests at the new tree and make them wait for a detached apply**

`MERLIN_HOOKS="$PROJECT_ROOT/opt/vpn-director/hooks"`. A helper `wait_for_merlin_applies <n>` polls
`$BATS_TEST_TMPDIR/apply.log` (0.2 s steps, 3 s at most) until it holds `<n>` lines `--wait apply`;
`assert_no_merlin_apply` sleeps 0.5 s and checks there is none. Run the Merlin hooks with `3>&-`, so a
detached apply never holds bats' output. Every existing Merlin test waits before it reads
`apply.log`; their assertions stay as they are (`$'restore mangle\n--wait apply'`, ...).
`nat-start: ships to Asuswrt-Merlin only` reads `router/opt/vpn-director/hooks/nat-start`.

New tests:

```bash
@test "Merlin hooks: return before the apply ends" {
    setup_merlin_hook_env
    printf '#!/bin/sh\nsleep 1\necho "$*" >> "%s/apply.log"\n' "$BATS_TEST_TMPDIR" > "$VPD_SCRIPT"
    run_hook "$MERLIN_HOOKS/firewall-start" eth0 3>&-
    assert_success
    run cat "$BATS_TEST_TMPDIR/apply.log"
    assert_output "restore mangle"          # the restore ran, the apply not yet
    wait_for_merlin_applies 1
}

@test "Merlin hooks: are POSIX sh and executable in the tree" {
    for hook in firewall-start wan-event nat-start; do
        [ -x "$MERLIN_HOOKS/$hook" ]
        head -n1 "$MERLIN_HOOKS/$hook" | grep -qx '#!/bin/sh'
        sh -n "$MERLIN_HOOKS/$hook"
    done
}
```

(The old `nat-start: is POSIX sh and executable in the tree` goes into the second one.)

- [ ] **Step 2: Run them - the moved paths and the new test fail**

Run: `bats router/test/unit/hooks.bats`
Expected: FAIL (no hooks under `opt/vpn-director/hooks`).

- [ ] **Step 3: Move the hooks, detach the apply, update the manifest**

In each hook the apply line becomes `"$VPD_SCRIPT" --wait apply </dev/null >/dev/null 2>&1 &`; the
restore stays synchronous and first. Header comments: the firmware runs the shared
`/jffs/scripts/<h>`, whose one line (`vpn-director.sh hooks`) calls this; the apply is detached so the
lines after ours there run at once. `wan-event` keeps `[ "$2" = "connected" ] || exit 0`.

- [ ] **Step 4: Pin the manifest in Go**

`TestRepoManifest_ShipsUpdateCriticalFiles`: `router/opt/vpn-director/hooks/firewall-start` tagged
`merlin`, and no entry starts with `router/jffs/` ("nothing copies a file into /jffs").

- [ ] **Step 5: Run the tests**

Run: `bats router/test/unit/hooks.bats` and `cd server && go test ./internal/updater/...`
Expected: PASS.

- [ ] **Step 6: Docs** - `CLAUDE.md` rows name `router/opt/vpn-director/hooks/<h>` ("called from the
shared `/jffs/scripts/<h>` by one line"); `shell-conventions.md`: the hooks queue a detached
`--wait apply`, so a firewall start's other commands do not wait for it.

- [ ] **Step 7: Commit**

```bash
git add -A router/jffs router/opt/vpn-director/hooks router/files.manifest router/test/unit/hooks.bats \
    server/internal/updater/manifest_test.go CLAUDE.md .claude/rules/shell-conventions.md
git commit -m "refactor(hooks): Merlin's hooks move to /opt/vpn-director/hooks and detach the apply"
```

---

### Task 2: `platform_hooks_install` puts the line into the shared hook

**Files:**
- Create: `router/opt/vpn-director/hooks/legacy/` - nine files from git:
  `git show v0.1.0:jffs/scripts/firewall-start > .../legacy/firewall-start.v0.1.0`, likewise
  `v0.2.0` (same path), `v0.3.0` and `v0.14.0` (`router/jffs/scripts/firewall-start`); the same four
  for `wan-event`; `nat-start.v0.22.0` (`router/jffs/scripts/nat-start`)
- Modify: `router/files.manifest` (nine `merlin` lines), `router/opt/vpn-director/lib/platform.sh`
  (contract text), `lib/platform/merlin.sh`, `lib/platform/keenetic.sh`, `router/test/test_helper.bash`
  (`setup()` exports `VPD_JFFS_SCRIPTS_DIR="$BATS_TEST_TMPDIR/jffs-scripts"` and
  `VPD_HOOKS_DIR="$PROJECT_ROOT/opt/vpn-director/hooks"`), `.claude/rules/testing.md` (both seams)
- Test: `router/test/unit/platform_merlin.bats`, `platform_keenetic.bats`, `hooks.bats`

**Interfaces:**
- Produces `platform_hooks_install` (no arguments). Merlin: one or two stdout lines per hook, each
  `ok: <file>: <text>` or `warn: <file>: <text>`; rc 0 when every hook holds our line and is
  executable, 1 otherwise. KeeneticOS: nothing, rc 0. Texts, `<file>` the full path:
  - `ok: <file>: created` - no file, or an empty one; also makes the directory
  - `ok: <file>: unchanged`
  - `ok: <file>: updated (<changes>)` - `<changes>` joined by `, `, in this order: `<version> body
    removed` for each earlier body cut; `line added` or `line replaced`; `duplicate lines removed`;
    `made executable`
  - `warn: <file>: still calls vpn-director.sh outside the vpn-director line; remove that call by hand`
    - after the `ok:` line, when a line that is not ours and not a comment contains `vpn-director.sh`
  - `warn: <file>: left alone: no #! line, so the firmware does not run it; add #!/bin/sh as its first line, then run vpn-director.sh hooks` (rc 1)
  - `warn: <file>: cannot write it; the hook is as it was` (rc 1)
- Produces `platform_hooks_status`. Merlin: per hook `<file>: vpn-director line in place`,
  `<file>: not executable; run vpn-director.sh hooks` or `<file>: no vpn-director line; run
  vpn-director.sh hooks`; rc 0. KeeneticOS: nothing, rc 1.
- Merlin internals tests may call: `_merlin_hook_line <h>` (prints the line),
  `_merlin_hook_ensure <h>` (one hook; its lines and rc).

The algorithm of `_merlin_hook_ensure`, after the missing/empty and no-`#!` cases: read the file into
an array; for each `legacy/<h>.*` (glob order) take its lines without its first (`#!`) line and, when
they occur as a contiguous run anywhere after the file's first line, remove that run; then the lines
ending in ` # vpn-director`: none - insert ours after the first line; the first one - set it to ours
(`line replaced` when it differed); any other - removed. Compare the result with the file (text and
the executable bit) before writing anything.

- [ ] **Step 1: Write the failing tests** (`platform_merlin.bats`, a "Shared hooks" section; each test
`load_platform` and works in `$VPD_JFFS_SCRIPTS_DIR`)

```bash
@test "platform_hooks_install: the RT-AX86U's firewall-start keeps guest-lan-port" {
    load_platform
    local f="$VPD_JFFS_SCRIPTS_DIR/firewall-start"
    mkdir -p "$VPD_JFFS_SCRIPTS_DIR"
    { cat "$VPD_HOOKS_DIR/legacy/firewall-start.v0.14.0"; echo '/jffs/scripts/guest-lan-port &'; } > "$f"
    chmod 755 "$f"
    run _merlin_hook_ensure firewall-start
    assert_success
    assert_output "ok: $f: updated (v0.14.0 body removed, line added)"
    [ "$(cat "$f")" = "$(printf '#!/bin/sh\n%s\n/jffs/scripts/guest-lan-port &' "$(_merlin_hook_line firewall-start)")" ]
}
```

The others, by name and what they assert:

```bash
@test "platform_hooks_install: creates the missing hooks: #!/bin/sh and the line, mode 755"
    # all three files == $'#!/bin/sh\n'"$(_merlin_hook_line <h>)"$'\n', -x; three "ok: ...: created"; rc 0
@test "_merlin_hook_ensure: every earlier body of every hook is recognized"
    # for each legacy/<h>.<v>: file = that body + "echo mine" -> #! line, our line, "echo mine"
@test "_merlin_hook_ensure: other add-ons' lines before and after an earlier body stay in place"
    # #!/bin/sh, a Skynet line (`sh /jffs/scripts/firewall start skynetloc=/tmp/mnt/sda1/skynet # Skynet Firewall Addition`),
    # the v0.3.0 body without its #!, `/jffs/addons/x/x.sh firewall # Added by X`
    # -> #!/bin/sh, ours, the Skynet line, the X line
@test "_merlin_hook_ensure: an edited earlier body stays, with a warning"
    # v0.14.0 body with "--wait apply" -> "apply": body untouched, our line after #!,
    # "warn: $f: still calls vpn-director.sh outside the vpn-director line; remove that call by hand"; rc 0
@test "_merlin_hook_ensure: CRLF line ends leave an earlier body unrecognized"   # same warning
@test "_merlin_hook_ensure: a second run writes nothing"
    # touch -d '2000-01-01 00:00' after the first run; second run: "ok: $f: unchanged",
    # same inode and mtime (stat -c '%i %Y')
@test "_merlin_hook_ensure: replaces an old form of the line and drops duplicates"
    # #!/bin/sh, `/old/path "$@" # vpn-director`, `echo x`, ours -> #!/bin/sh, ours, `echo x`;
    # "updated (line replaced, duplicate lines removed)"
@test "_merlin_hook_ensure: makes a hook with the line executable"           # mode 644 -> "updated (made executable)"
@test "_merlin_hook_ensure: leaves a file without #! alone"                  # `echo hi` kept; the warn text; rc 1
@test "_merlin_hook_ensure: a hook it cannot write stays as it was"
    # mkdir "$VPD_JFFS_SCRIPTS_DIR/.firewall-start.vpd-new" before the run: content unchanged,
    # "warn: $f: cannot write it; the hook is as it was"; rc 1
@test "platform_hooks_install: the earlier bodies are the released ones"
    # md5sum of the nine files:
    # firewall-start v0.1.0 210fe9580a0635b872c1328ce63ec7e5, v0.2.0 93b96f381f17389b9ca03c587b43016d,
    #   v0.3.0 6aab1db5bcc5370420986edafc50a308, v0.14.0 ff06ffddeaf5fdde38e7c8d2debd6186
    # wan-event v0.1.0 2bf722445bc34ddb5863b189e81d6510, v0.2.0 fa8ab8ee1cdfbf10a51892e2cf071f57,
    #   v0.3.0 b05abaa4abd76ee32745e4ed8afcd297, v0.14.0 a8379d20a893911e99af5522b938bd85
    # nat-start v0.22.0 35fc4470a4f7edc1eb34ab5daac2e069
@test "platform_hooks_status: in place, not executable, no line"
```

`platform_keenetic.bats`: `platform_hooks_install` prints nothing, rc 0, and creates nothing under
`$VPD_JFFS_SCRIPTS_DIR`; `platform_hooks_status` prints nothing, rc 1.

`hooks.bats`, through the real line (`platform_hooks_install` writes it into a shared file that holds
an owner's line, then `sh "$VPD_JFFS_SCRIPTS_DIR/<h>" <argv> 3>&-` with the Merlin fakes):

```bash
@test "shared wan-event: the owner's line after ours runs on every WAN event"
    # owner line `echo "owner $2" >> "$BATS_TEST_TMPDIR/owner.log"`; argv 0 disconnected:
    # owner.log == "owner disconnected", assert_no_merlin_apply
@test "shared firewall-start: the owner's line after ours runs before the apply ends"
    # the fake CLI sleeps 1 s, then logs "apply" to order.log; the owner line logs "owner" there:
    # right after the run order.log == "owner"; then "owner" followed by "apply"
```

- [ ] **Step 2: Run them**

Run: `bats router/test/unit/platform_merlin.bats router/test/unit/platform_keenetic.bats router/test/unit/hooks.bats`
Expected: FAIL (`platform_hooks_install: command not found`, no legacy files).

- [ ] **Step 3: Copy the nine bodies, list them in the manifest, add the contract text and both
functions** (`platform.sh`: the two signatures, and that a platform whose hooks are files of its own
prints nothing; Merlin in `merlin.sh` beside the cron functions; KeeneticOS one-liners)

- [ ] **Step 4: Run the tests**

Run: the command of Step 2, then `cd server && go test ./internal/updater/...` (the manifest lists
every new file)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/hooks/legacy router/files.manifest router/opt/vpn-director/lib/platform.sh \
    router/opt/vpn-director/lib/platform/merlin.sh router/opt/vpn-director/lib/platform/keenetic.sh \
    router/test/test_helper.bash router/test/unit/platform_merlin.bats router/test/unit/platform_keenetic.bats \
    router/test/unit/hooks.bats .claude/rules/testing.md
git commit -m "feat(platform): one marked line in Merlin's shared hooks, an earlier release's body cut out"
```

---

### Task 3: `vpn-director.sh hooks`, and the hook lines in the full status

**Files:**
- Modify: `router/opt/vpn-director/vpn-director.sh` (usage header, `show_help`, `cmd_hooks`, the
  dispatch, `cmd_status`)
- Modify: `router/test/integration/vpn_director.bats` (its own `setup()` exports
  `VPD_JFFS_SCRIPTS_DIR` and `VPD_HOOKS_DIR` as `test_helper.bash` does)
- Modify: `CLAUDE.md` (Commands block), `README.md`, `README.ru.md` (their command lists)

**Interfaces:**
- Consumes: `platform_hooks_install`, `platform_hooks_status` (Task 2).
- Produces: `vpn-director.sh hooks` - loads common.sh only (no config, no lock), logs each
  `ok: X` line as `log "X"` and each `warn: X` as `log -l WARN "X"`, exits with
  `platform_hooks_install`'s rc. Help line: `hooks                       Put the call of VPN
  Director's hooks into the firmware's shared hook scripts (Asuswrt-Merlin)`.

- [ ] **Step 1: Write the failing tests**

```bash
@test "vpn-director: hooks puts the line into each Merlin hook and says so"
    # run vpn-director.sh hooks; success; three files under $VPD_JFFS_SCRIPTS_DIR;
    # output --partial "$VPD_JFFS_SCRIPTS_DIR/firewall-start: created"
@test "vpn-director: hooks exits 1 when a hook is left without the line"
    # $VPD_JFFS_SCRIPTS_DIR/wan-event holds "echo hi": failure, output --partial "left alone"
@test "vpn-director: the full status lists the firmware hooks"
    # after `hooks`: status shows "=== Firmware Hooks ===" and
    # "$VPD_JFFS_SCRIPTS_DIR/nat-start: vpn-director line in place"
@test "vpn-director: apply leaves the firmware hooks alone"
    # apply --dry-run and a real apply (as the existing apply tests run it): $VPD_JFFS_SCRIPTS_DIR absent
```

KeeneticOS (`VPD_PLATFORM=keenetic`, as the file's Keenetic tests set it): `hooks` succeeds silently and
the status has no "Firmware Hooks" section.

- [ ] **Step 2: Run them**

Run: `bats router/test/integration/vpn_director.bats`
Expected: FAIL (`Unknown command: hooks`).

- [ ] **Step 3: Implement `cmd_hooks` and the status section** (full status only: after Xray's
section, `echo ""`, `=== Firmware Hooks ===`, `platform_hooks_status`'s lines - nothing when it
returns 1)

- [ ] **Step 4: Run the tests**

Run: `bats router/test/integration/vpn_director.bats`
Expected: PASS.

- [ ] **Step 5: Docs** - the command in `CLAUDE.md`'s Commands block and both READMEs' command lists.

- [ ] **Step 6: Commit**

```bash
git add router/opt/vpn-director/vpn-director.sh router/test/integration/vpn_director.bats CLAUDE.md README.md README.ru.md
git commit -m "feat(cli): vpn-director.sh hooks, and the hook lines in the full status"
```

---

### Task 4: Install and update put the lines in place

**Files:**
- Modify: `install.sh` (`install_hooks`, called in `main` right after `load_platform_lib`)
- Modify: `server/internal/updater/update_script.sh.tmpl` (step 5b, right after the top-level
  `release_apply_lock` call, before step 6), `testdata/update_script.golden.sh`, `script_test.go`
- Test: `router/test/unit/install.bats`
- Modify: `README.md`, `README.ru.md` (Startup Scripts rows and the paragraph "The installer, and every
  update after it, writes these `/jffs/scripts` hooks in full..."; the upgrade notes of this release),
  `.claude/rules/telegram-bot.md` (Self-Update step 6)

**Interfaces:**
- Consumes: `vpn-director.sh hooks` (Task 3).
- Produces: `install_hooks` - runs `VPD_PLATFORM="$PLATFORM" "$VPD_DIR/vpn-director.sh" hooks`;
  success: `print_success "Firmware hooks set"`; failure: `print_info "Warning: a firmware hook does
  not call VPN Director (see above); fix it, then run $VPD_DIR/vpn-director.sh hooks"` and the
  installer goes on.
- The update script, step 5b: `log "Setting the firmware hooks"`, then
  `if ! /opt/vpn-director/vpn-director.sh hooks >> "$LOG_FILE" 2>&1; then log "WARN: a firmware hook does not call VPN Director; see the lines above, then run /opt/vpn-director/vpn-director.sh hooks"; fi`.

- [ ] **Step 1: Write the failing tests**

`install.bats`:

```bash
@test "install_hooks: runs vpn-director.sh hooks for the detected platform"
    # a stub $VPD_DIR/vpn-director.sh records "$VPD_PLATFORM $*": "merlin hooks"; "Firmware hooks set"
@test "install_hooks: a failure is a warning, not the end of the install"
    # the stub exits 1: success; output --partial "vpn-director.sh hooks"
```

and `main`'s expected call order gains `install_hooks` after `load_platform_lib`.

`script_test.go`:

```go
func TestGenerateScript_SetsTheHooksOnceTheFilesAreInPlace(t *testing.T)
    // call := strings.Index(script, "/opt/vpn-director/vpn-director.sh hooks")
    // call > strings.Index(script, "# 4. Copy files")
    // call > strings.Index(script, "\nrelease_apply_lock\n")   // the top-level call
    // call < strings.Index(script, "# 6. Start everyone")
    // strings.Contains(script, "if ! /opt/vpn-director/vpn-director.sh hooks")  // never fatal
```

- [ ] **Step 2: Run them**

Run: `bats router/test/unit/install.bats`; `cd server && go test ./internal/updater/...`
Expected: FAIL.

- [ ] **Step 3: Implement `install_hooks` and step 5b; regenerate the golden script**

Run: `cd server && UPDATE_GOLDEN=1 go test ./internal/updater -run TestGenerateScript_Golden -count=1`,
then review the golden diff: only step 5b.

- [ ] **Step 4: Run the tests**

Run: the commands of Step 2
Expected: PASS.

- [ ] **Step 5: Docs** - README.md and README.ru.md: each `/jffs/scripts/<h>` row says the file holds
one line of ours, marked `# vpn-director`, calling `/opt/vpn-director/hooks/<h>`; the paragraph about
merging by hand becomes: install and every update put that line in and leave the rest of the file
alone, the first update after this release cuts out the hook an earlier release wrote and keeps the
other lines (an edited one stays, with a warning in the update log), `vpn-director.sh hooks` puts a
lost line back, deleting the line takes VPN Director out of the hook. The release's upgrade note
says the same in one sentence. `telegram-bot.md` step 6: once the payload is copied the script runs
`vpn-director.sh hooks`, whose failure does not fail the update.

- [ ] **Step 6: Commit**

```bash
git add install.sh router/test/unit/install.bats server/internal/updater/update_script.sh.tmpl \
    server/internal/updater/testdata/update_script.golden.sh server/internal/updater/script_test.go \
    README.md README.ru.md .claude/rules/telegram-bot.md
git commit -m "feat(install): install and update put the hook lines in place instead of copying the hooks"
```

---

### Final verification

- [ ] Full Bats suite and `cd server && go test ./internal/updater/...` (the controller runs both
  through `build-forge:build-runner`); the counts and a clean output recorded.
- [ ] `grep -rn "jffs/scripts" router/opt install.sh server/internal/updater/*.go` shows no file copied
  into `/jffs` (the hooks' comments and the platform code name the directory, nothing ships there).
