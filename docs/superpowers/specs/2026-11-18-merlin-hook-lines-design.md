# Merlin hooks that keep other commands: design

Date: 2026-11-18
Branch: feature/firewall-rebuild-guard (the owner's choice: before the v0.23.0 release)
Status: approved by the owner

## Intent

`install.sh` (`curl -o`) and every update (`cp -f` in `server/internal/updater/update_script.sh.tmpl`)
write Asuswrt-Merlin's shared hooks `/jffs/scripts/firewall-start`, `wan-event` and `nat-start` in
full. Whatever the owner or another add-on put there goes: the owner's `/jffs/scripts/guest-lan-port &`
on the RT-AX86U, Skynet's, Diversion's lines. README.md calls it a manual merge that no update keeps.
The owner wants install and update to leave every command that is not ours where it is.

Today's hooks also get in the way while they run: `wan-event`'s `[ "$2" = "connected" ] || exit 0`
stops every line after ours on any other event, and `firewall-start` runs `--wait apply` in the
foreground, so a line after it waits for the whole apply.

## Goals

1. Install and update never change, remove or reorder a line of `/jffs/scripts/<hook>` that is not
   ours.
2. Ours is one marked line calling our own script; the logic lives in files of ours under
   `/opt/vpn-director/hooks/`, which an update replaces freely.
3. The update to this release migrates a router: a recognized body of an earlier release is cut out
   of the shared file, the rest kept.
4. Our call neither blocks nor cuts off the lines after it.

## Non-goals

- KeeneticOS: its hooks are files of ours already (`/opt/etc/ndm/*/50-vpn-director.sh`).
- An uninstaller.
- What the hooks do: the restore from the snapshot first, then `--wait apply`, still without
  `--unless-stopped` (ruling R28 of the firewall rebuild guard).
- Merlin hooks vpn-director does not use (`services-start`, `service-event`, ...).

## Anti-goals

- Deleting or moving a line we did not recognize, or changing whether someone else's file runs.
- A write to `/jffs` when nothing changes, or from an apply: a line the owner removed on purpose stays
  removed until install, an update or `vpn-director.sh hooks`.
- A slower or later restore (decision 6 of the firewall rebuild guard: every millisecond is part of
  the window).

## Approaches

| | Approach | Verdict |
|---|---|---|
| A | One marked line in the shared hook calling a script of ours (how amtm, Skynet and xrayui do it, `docs/research-merlin-web-ui-addons.md`) | **Chosen** |
| B | Our logic inside the shared file between markers, the block replaced on update | Every update still rewrites the owner's file, and the logic in it still affects the lines after it |
| C | A `firewall-start.local` for the owner's commands, the shared file still ours | Other add-ons do not know it and keep writing to the shared file, which we would still overwrite |
| D | Leave a modified hook alone | Such a hook never gets new logic, this release's restore included |

## Design

1. **Our hook scripts.** `router/jffs/scripts/{firewall-start,wan-event,nat-start}` move to
   `router/opt/vpn-director/hooks/` (manifest tag `merlin`, executable), installed as
   `/opt/vpn-director/hooks/<hook>`. They keep today's logic and order, with one change: the apply
   runs detached (its descriptors closed, as the KeeneticOS hooks do it), so the call returns once the
   restore is done. `wan-event` keeps its `connected` filter inside our script. POSIX sh for Merlin's
   BusyBox, no `command` builtin.
2. **Our line**, for hook `<h>`, right after the file's `#!` line:

   ```
   [ -x /opt/vpn-director/hooks/<h> ] && /opt/vpn-director/hooks/<h> "$@" # vpn-director
   ```

   The `-x` test keeps an Entware that is not mounted yet from failing the line.
3. **`vpn-director.sh hooks`** runs `platform_hooks_install` (a new function of the platform
   contract; KeeneticOS does nothing, rc 0). For each of the three hooks, on Merlin:
   - **Ours** are the lines ending in ` # vpn-director`. One with the canonical text: nothing to do.
     One with another text: replaced in place. Several: the first keeps its place with the canonical
     text, the others go. None: the line goes in right after the first line, `#!...`.
   - **Bodies of earlier releases** are recognized exactly: the release ships a copy of each of the 9
     bodies released so far (`firewall-start` of v0.1.0, v0.2.0, v0.3.0, v0.14.0; `wan-event` of
     the same four; `nat-start` of v0.22.0; v0.1.x-v0.2.x kept them at `jffs/scripts/` of the
     repository). A body found in the file as a contiguous run of identical lines, its `#!` line
     aside, is cut out. A body that was edited is not cut: our line goes in, and a WARN says that the
     file still calls `vpn-director.sh` outside our line and the owner has to remove that call. No
     single line is ever deleted alone: a `fi` of ours looks like a `fi` of theirs.
   - **The write**: the new text into a temporary file in the same directory, executable, renamed
     over the hook with `mv`; only when the text changed.
   - **Missing file or directory, or an empty file**: created as `#!/bin/sh` and our line, mode 755.
     **A non-empty file without `#!`**: left alone with a WARN - the firmware does not run it now, and
     adding the shebang would start running someone else's commands. CRLF line ends leave an earlier
     body unrecognized, which is the WARN case above.
4. **Callers**: `install.sh` once the files are downloaded; the update script of this release once
   the files are copied (the self-update contract, `server/internal/updater/selfupdate.go`: a release
   installs itself with its own script, so the update to this release already migrates); the owner by
   hand. An apply never runs it. `status` shows whether each hook holds our line.
5. **The manifest** lists no `router/jffs/scripts/*` any more: nothing copies a file into `/jffs`.

## Left to the builder

- Where the copies of the earlier bodies live (files under `router/opt/vpn-director/hooks/`, as the
  builder names them, or embedded) and the test seam for `/jffs/scripts` (e.g.
  `VPD_JFFS_SCRIPTS_DIR`).
- My call: a `hooks` run that fails during an update does not fail the update - the files are in
  place already; the update log says so, and the WARN names `vpn-director.sh hooks`.
- My call: the full `status` prints a line per hook (our line in place or not); any other place for
  it is the builder's.
- Tests: Bats fixtures for a missing file, the RT-AX86U's file (the v0.14.0 body followed by
  `/jffs/scripts/guest-lan-port &`), Skynet-style lines, an edited earlier body, duplicates of our
  line, a file without `#!`, CRLF, an unchanged file not rewritten; `hooks.bats` and `install.bats`
  on the new paths; the Go manifest, downloader and script tests and the golden update script.
- Docs: README.md and README.ru.md (startup scripts, and the paragraph about merging by hand),
  CLAUDE.md (architecture rows), `.claude/rules/` wherever `router/jffs/scripts` or the hooks'
  behaviour is described, `testing.md`.
