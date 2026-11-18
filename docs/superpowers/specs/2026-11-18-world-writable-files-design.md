# Files and directories writable by everyone on Asuswrt-Merlin: design

Approved by the owner on 2026-11-18. Branch `fix/world-writable-files`, cut from master `259c094`
(v0.23.0).

## Why

Asuswrt-Merlin runs everything under umask 0: the firmware's services, cron, the hooks, even an
ssh session (`umask` prints `0000`). Our shell code creates directories and files without a mode
and takes whatever the caller's umask gives. Measured on the RT-AX86U (Merlin 388.13, v0.23.0):

- all 14 directories of `/opt/vpn-director` are 0777: `lib`, `lib/platform`, `certs`, `data`,
  `data/ipsets`, v0.23.0's `hooks` and `hooks/legacy`, and the owner's own directories in the tree;
- `telegram-bot.json`, which holds the bot token and `allowed_users`, is 0666;
- `certs/server.crt` and `data/ipsets/{ru,by,kz}.dump` are 0666. Every apply restores the country
  sets from those dumps;
- `/tmp/vpn-director.log` and the files in `/tmp/tunnel_director` are 0666.

Root runs the scripts, the hooks and the daemon binaries in those directories, and in a 0777
directory any process can replace a root-owned 0755 file. The router runs non-root processes:
`dnsmasq`, nginx workers and `lldpd`, all as `nobody`. Code execution in one of them becomes root
at the next apply, hook or daemon start. Any of them can also:

- read the bot token;
- rewrite `allowed_users`, and the bot manages the router as root;
- rewrite a dump so that a tunnel client's traffic goes direct.

KeeneticOS sessions run under umask 0022, and the KN-4521's install is 0755, 0644 and 0600 as it
should be. The fix is platform-neutral all the same.

Some of the code already sets modes itself because of Merlin's umask 0:

- the guard work: `fwsnap-restore.sh` runs under `umask 077`, `ensure_owned_dir` makes 0755
  directories, and the guard records and watchd's runtime directory are root's alone;
- `configure.sh`, `substore.sh` and `xrayconf.sh` write their secrets with `mktemp`, then
  `chmod 600`, then `mv`;
- the Go daemons give every file and directory an explicit mode, and the updater publishes what it
  downloads as 0644.

What still takes the caller's umask:

- the installer;
- the directories the update script creates;
- the CLI's other writes: the log, the dumps, the Tunnel Director state;
- `setup_telegram_bot.sh`.

## Goals

1. Nothing VPN Director creates is writable by group or others, on any platform, whatever umask
   its caller brings.
2. Secrets are 0600 from the moment they are written. `telegram-bot.json` is the one secret still
   written without a mode.
3. An existing install is repaired by `install.sh` and by the update:
   - the whole `/opt/vpn-director` tree, `/tmp/tunnel_director` and `/tmp/vpn-director.log` lose
     group and other write;
   - `telegram-bot.json` becomes 0600.

## Non-goals

- The disk's mount root (`/tmp/mnt/entware` is 0777 on the RT-AX86U) and other Entware
  directories: the firmware's or amtm's, not ours.
- `/opt/etc/xray` beyond our own files. `config.json` is already 0600; `config.json.old` on the
  RT-AX86U is not ours.
- Tightening read access beyond secrets: scripts and binaries stay 0755, plain files 0644.
- A lasting mode check in `status`.

## Anti-goals

- Breaking anything. Removing group and other write never stops root, and everything of ours runs
  as root.
- Following a symlink out of `/opt/vpn-director` during the repair. `chmod -R` does not follow a
  symlink it meets during recursion, in BusyBox and in coreutils alike; a test pins it.
- Touching the bytes of `hooks/legacy/*`. The hook migration recognizes them byte for byte.

## Approaches

- **(A) Picked.** `umask 022` at the start of every script we execute, so whatever it creates is
  0755 or 0644. Secrets get 0600 explicitly, and install and update run a one-time repair.
- **(B) Rejected.** An explicit mode at every creation site (`mkdir -m`, `chmod` after every
  write). That is dozens of sites, and new code forgets.
- **(C) Rejected.** The repair alone. What the runtime creates later - the log, the dumps, the
  state, a directory a later release adds - would be 0666 or 0777 again.

**Repair scope.** The owner picked the whole `/opt/vpn-director` tree, including their own
`backup-*`, `saved/`, `saved-config.json` and `myvpn-config`. The alternative was only what we ship
and create.

## Design

1. **`umask 022` in every executed script:**
   - The hand-over block shared by `vpn-director.sh`, `configure.sh`, `import_server_list.sh`,
     `setup_telegram_bot.sh`, `lib/send-email.sh` and `install.sh` sets `umask 022` before the
     hand-over, so the sh part and the bash part both run under it. `entrypoints.bats` asserts it
     with the rest of the block.
   - `umask 022` goes before the first command that starts or creates anything in:
     - the init scripts `S98telegram-bot`, `S98vpn-director-webui`, `S98vpn-director-watchd` and
       `S99vpn-director`. The daemons, and everything they start (the CLI, the update script),
       inherit it;
     - the Merlin hooks (`hooks/firewall-start`, `wan-event`, `nat-start`);
     - the NDM hooks (`netfilter.d`, `wan.d`, `iflayerchanged.d`);
     - the update script template.
   - `fwsnap-restore.sh` keeps its `umask 077`.
   - The Go daemons need no change. My call: they already pass explicit modes.
2. **`setup_telegram_bot.sh` writes `telegram-bot.json`** with `mktemp`, then `chmod 600`, then
   `mv`, the way `configure.sh` and `substore.sh` write theirs. The `mv` gives the file a new inode,
   so an old 0666 file does not keep its mode.
3. **The repair.** Both `install.sh` and the update script run it. In the update script it runs
   after the files are in place, beside the hooks step:
   - `chmod -R go-w` on `/opt/vpn-director` and on `/tmp/tunnel_director` when present;
   - `chmod go-w` on `/tmp/vpn-director.log` when present;
   - `chmod 600` on `telegram-bot.json` when present.

   A failure is logged as a WARN and never fails the install or the update.

   My call: a few inline lines in each place, no new CLI command, since it is a one-time repair.
   The next update runs it again at no cost.
4. **Tests.** Unless noted, each runs under `umask 0`:
   - `entrypoints.bats`: the hand-over block sets `umask 022` before the hand-over.
   - The init scripts and the Merlin and NDM hooks set `umask 022` before they start anything.
   - `install.sh`, through its `INSTALL_ROOT` seam, leaves no group- or other-writable path.
   - `setup_telegram_bot.sh` writes 0600, also over an existing 0666 file.
   - The CLI, called by a 0-umask caller, creates its log, a dump and the Tunnel Director state
     without group or other write.
   - The repair: a tree of 0777 directories and 0666 files comes out with no group or other write,
     and a symlink inside it keeps its target's mode. `telegram-bot.json` becomes 0600.
   - Go, `script_test.go`: the update script sets `umask 022` before anything else and runs the
     repair after the copy. The golden file is regenerated.
5. **Docs:**
   - CLAUDE.md "Shell Conventions" and `.claude/rules/shell-conventions.md` (known pitfalls):
     Merlin's umask 0, `umask 022` in every executed script, secrets with `mktemp` and
     `chmod 600`;
   - README.md and README.ru.md: an upgrade note saying that the update removes write for others
     from `/opt/vpn-director` and makes `telegram-bot.json` 0600.

## Left to the builder

- Where exactly `umask 022` goes in each script, within the rule above.
- The wording of the WARN lines, and whether the repair names what it changed.
- The release number. My guess is a patch release (v0.23.1), but the owner decides at release
  time.
