---
paths: "**/*.sh, jffs/**/*"
---

# Shell Conventions

## Script Structure

- Shebang: `#!/usr/bin/env bash` for libraries, which are only ever sourced - except
  `lib/send-email.sh`, which `S99vpn-director` executes. A script a router executes starts
  `#!/bin/sh` and hands over to bash itself (see "No `/usr/bin/env`
  on KeeneticOS" below). Both forms then `set -euo pipefail`
- Debug mode: `DEBUG=1 ./script.sh` enables `set -x` with informative PS4
- shellcheck annotations for intentional expansions/externals (SC2086, SC2155, SC2034)

## Logging & Locking

- Logging: `log -l ERROR|WARN|INFO|DEBUG|TRACE "message"` (default: INFO)
- Locking: `acquire_lock [name]` prevents concurrent script execution; with `VPD_LOCK_WAIT=<sec>` (set by `vpn-director.sh --wait[=SEC]`) it waits for the lock instead of exiting 0
- Temp files: `tmp_file` / `tmp_dir` with auto-cleanup on exit

## Key Utilities (common.sh and the platform contract)

| Function | Description |
|----------|-------------|
| `uuid4` | Generate random UUIDv4 from kernel |
| `compute_hash [file\|-]` | SHA-256 digest of file or stdin |
| `get_script_path` | Absolute path to current script (resolves symlinks) |
| `get_script_dir` | Directory containing current script |
| `get_script_name [-n]` | Script filename; `-n` strips extension |
| `resolve_ip [-6] [-q] [-g] [-a] <host>` | DNS/hosts resolution |
| `resolve_lan_ip [-6] [-q] [-a] <host>` | Resolve only private/LAN addresses |
| `is_lan_ip [-6] <ip>` | Check if IP is in RFC1918/ULA range (prefix only) |
| `is_ipv4_net <addr>` | IPv4 address or CIDR that iptables and ipset read as written (no leading zeros) |
| `is_pos_int <value>` | Check if value is positive integer (>=1) |
| `rt_table_label <table>` | A routing table as `ip rule show` prints it: its rt_tables name, else as given |
| `strip_comments [text]` | Remove blank lines and # comments |
| `platform_wan_if` | Active WAN interface (platform contract; `get_active_wan_if` is a wrapper) |
| `platform_ipv6_enabled` | 1 when IPv6 is enabled (platform contract; `get_ipv6_enabled` is a wrapper) |
| `platform_tunnels`, `platform_tunnel_table`, `platform_load_module`, `platform_cron_add` | Other platform facts; see `lib/platform.sh` header for the whole contract |
| `download_file <url> <dest> [timeout]` | Download with retry (wget/curl fallback) |
| `log_error_trace <msg>` | Log error with bash stack trace |

**Logging**: `LOG_FILE=/tmp/vpn-director.log` with 200KB rotation

**Platform contract**: `common.sh` sources `lib/platform.sh` at its end, so every script that
sources `common.sh` can call `platform_*`. Firmware-specific facts belong in
`lib/platform/<name>.sh`; no core module tests `VPD_PLATFORM` itself.

## Firewall Utilities (firewall.sh)

| Function | Description |
|----------|-------------|
| `fw_chain_exists [-6] <table> <chain>` | Check if chain exists |
| `create_fw_chain [-6] [-q] [-f] <table> <chain>` | Create chain; `-f` flushes if exists |
| `delete_fw_chain [-6] [-q] <table> <chain>` | Flush and delete chain |
| `find_fw_rules [-6] "<table> <chain>" "<pattern>"` | Find rules matching regex |
| `purge_fw_rules [-6] [-q] [--count] "<table> <chain>" "<pattern>"` | Remove matching rules |
| `ensure_fw_rule [-6] [-q] [--count] <table> <chain> [-I [pos]\|-D] <rule>` | Idempotent rule add/delete |
| `sync_fw_rule [-6] [-q] [--count] <table> <chain> "<pattern>" "<desired>" [pos]` | Replace matching rules with one |
| `swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...` | Rebuild a chain PREROUTING jumps to as `<chain>_NEW` and swap it in; 0 done, 1 done with a rule missing, 2 live chain untouched, 3 cutover unfinished |
| `block_wan_for_host <host>` | Block host from WAN (IPv4/IPv6); WAN interface from `platform_wan_if` |
| `allow_wan_for_host <host>` | Unblock host from WAN |
| `chg <cmd>` | Returns true if command output is non-zero integer |
| `validate_port <N>` | Validate port 1-65535 |
| `validate_ports <spec>` | Validate port spec (any, N, N-M, N,N2) |
| `normalize_protos <spec>` | Normalize to tcp, udp, or tcp,udp |

## State Tracking

Hash files in `/tmp/` detect config changes; scripts only reapply if changed.

## Bash-specific Patterns

- Use `[[ ]]` instead of `[ ]` for conditionals
- Use `read -ra array <<< "$string"` for splitting strings into arrays
- Use `${array[@]}` for iterating arrays
- Use `[[ $var =~ regex ]]` for regex matching instead of grep
- Debug mode: `DEBUG=1` enables `set -x` with PS4 showing file:line:function

## Known Pitfalls

### `tr` with POSIX character classes breaks on router

**Problem**: `tr '[:upper:]' '[:lower:]'` corrupts certain characters on Asuswrt-Merlin routers with Entware.

Example: letter `u` (0x75) becomes `l` (0x6c):
```bash
echo "SETUP" | tr '[:upper:]' '[:lower:]'
# Expected: setup
# Actual:   setlp
```

This is a bug in glibc/busybox `tr` with certain locale settings (`LC_ALL=en_US.UTF-8`). Setting `LC_ALL=C` does not fix it.

**Solution**: Always use explicit ASCII ranges instead of POSIX character classes:
```bash
# BAD - breaks on router
tr '[:upper:]' '[:lower:]'

# GOOD - works everywhere
tr 'A-Z' 'a-z'
```

**Rule**: Never use `[:upper:]` / `[:lower:]` in shell scripts for this project. Always use `A-Z` / `a-z`.

**Alternative fix**: Install `opkg install coreutils-tr` which provides a working `/opt/bin/tr`. After installation and `hash -r`, the correct `tr` will be used. However, code should still use `A-Z` / `a-z` for compatibility with systems without coreutils.

### Uninitialized arrays with `set -u`

**Problem**: With `set -u` (nounset), accessing uninitialized array length fails:
```bash
local -a my_array
echo ${#my_array[@]}  # Error: my_array: unbound variable
```

**Solution**: Always initialize arrays:
```bash
local -a my_array=()
echo ${#my_array[@]}  # Works: outputs 0
```

### A `flock` outlives the script through inherited descriptors

**Problem**: `exec 9>lock; flock -n 9` holds the lock on the open file
description, and descriptors a shell opens are not close-on-exec. Any process
started while the descriptor is open inherits it — a daemon launched with
`nohup … &` then holds the lock for its whole lifetime, long after the script
that took it has exited. Every later `acquire_lock` sees the file locked: with
no `--wait` it exits 0 without doing anything, with `--wait` it times out.

**Solution**: release and close before starting anything, not at script exit:
```bash
flock -u 9
exec 9>&-
```
Both the success path and the recovery path need it; `update_script.sh.tmpl`
does this in `release_apply_lock`.

When the lock has to stay held, close the descriptor for the child instead:
`tproxy_restart_process` runs the Xray init script as `"$xray_init" restart
200>&-`, so the daemon rc.func backgrounds cannot inherit the lock
`vpn-director.sh` is holding on FD 200. For the same reason `acquire_lock`
returns early when this process already holds the lock: reopening FD 200 drops
it and takes it again, and another waiter can step into that window.

**Related**: POSIX allows a single digit in a redirection. `exec 201>` is a
bash/ksh extension that dash rejects, and generated scripts run under
`/bin/sh`.

### `ip rule show` prints the table by name, not by number

**Problem**: `ip rule add ... table 100` reads back as `lookup wan0`, because
`/etc/iproute2/rt_tables` on Asuswrt-Merlin maps `100 wan0` (and `111 ovpnc1`,
`116 wgc1`, …). An idempotency check that greps the output for the table
*number* never recognises its own rule:

```bash
# never matches on a router: the kernel prints "lookup wan0"
ip rule show | grep -c "fwmark 0x100.*lookup 100"
```

KeeneticOS has no `/etc/iproute2/rt_tables`; `ip rule show` prints table numbers,
and `rt_table_label` (common.sh) falls back to the number so both sides still agree.

`_tproxy_setup_routing` did exactly this and re-added its rule on every apply —
seven copies on a router with 23 days of uptime, and auto-apply from the Web UI
made each click add another.

**Solution**: the preference belongs to the module, so reconcile everything
sitting on it instead of looking for one tuple. Keep a rule that carries the
configured mark and table — comparing what the kernel *prints*, via
`rt_table_label` — and delete the rest, including rules an earlier
`route_table` or `fwmark_mask` left behind. Matching only the configured tuple
has the mirror-image failure: a stale rule counts as ours, and the new setting
never gets installed.

```bash
want_table=$(rt_table_label "$TABLE")          # 100 -> wan0
mark=$(printf '%s' "$line" | sed -n 's/.*fwmark \([^ ]*\).*/\1/p')
table=$(printf '%s' "$line" | sed -n 's/.*lookup \([^ ]*\).*/\1/p')
```

`ip rule del` removes one rule per call and fails when none is left, so a
teardown deletes by preference in a loop rather than once.

### `cmd | grep -q` under pipefail reports a match as a failure

**Problem**: `grep -q` exits at its first match. If the command on the left still
has output to write, that write gets SIGPIPE, the command dies with 141, and
`pipefail` makes the pipeline report the 141 instead of grep's 0. iproute2 writes
each rule as it prints it, so `ip rule show | grep -q "^16384:"` read an installed
rule as missing now and then - measured 35 of 500 applies on one CPU - and the
caller deleted and re-added a live rule, a window in which marked packets fell
through to `main`. It is timing-dependent, so it passes every test that does not
force it.

**Solution**: read the whole output first, then search it:

```bash
rules=$(ip rule show 2>/dev/null) || true
[[ $'\n'$rules == *$'\n'"$pref:"* ]]
```

`grep` without `-q` (or `grep -c`) reads its whole input and is not affected;
neither is `grep -q` on a here-string.

### Merlin's firewall start empties every mangle chain and deletes none

**Problem**: `mangle_setting()` in the firmware's `rc/firewall.c` runs
`iptables -t mangle -F` on every firewall start unless traditional QoS, the
bandwidth limiter or GeForce NOW QoS is on (those go through `add_iQosRules`),
and `del_iQosRules()` in `qos.c` does the same when QoS stops. `-F` without a
chain name empties every chain of the table, `TUN_DIR` and `XRAY_TPROXY`
included, and deletes none of them - mangle is not reloaded with
`iptables-restore`, which would have. `firewall-start` then runs
`vpn-director.sh --wait apply`, and a chain that exists no longer says its rules do:

```bash
# succeeds for an empty chain
iptables -t mangle -S TUN_DIR >/dev/null 2>&1
```

On the RT-AX86U (388.11, `qos_enable=0`, no `/tmp/mangle_rules`) that is every
firewall start. The up-to-date path of `tunnel_apply` took the empty chain for
applied: it put the PREROUTING jumps back into it and wrote `failover_ready`,
and every Tunnel Director client left through the WAN until the configuration
changed.

**Solution**: rebuild the chain on every apply (`_tproxy_setup_iptables` builds
`XRAY_TPROXY_NEW` and swaps it in), or look for the rules themselves before
calling it applied - `_tunnel_marks_present` asks `iptables -C` for every
client's MARK rule and rebuilds when one is gone. KeeneticOS deletes our chains
on every NDM rebuild, so there the missing chain is what sends the apply
through a rebuild.

Either way the apply after the last firewall start has to run. The firmware
starts `firewall-start` and `wan-event` without waiting for them
(`run_custom_script` with no timeout), a WAN coming up does both, and a plain
`apply` exits at once while another instance holds the lock - so the chains of
a flush that landed during a running apply stayed empty until the next event.
Both hooks pass `--wait`, as the KeeneticOS hooks do.

### An apply never flushes a live chain or set

**Problem**: `create_fw_chain -f` on a chain the PREROUTING jumps lead to,
`ipset flush` on a set a rule matches, and `tunnel_stop` ahead of a rebuild each
opened a window in which packets crossed an empty chain or set. `XRAY_TPROXY`
returned every Xray client to the WAN for the length of its refill, on every
apply; a Tunnel Director rebuild sent every tunnel client there for seconds.

**Solution**: build beside the live object and swap it in. `swap_fw_chain`
(`lib/firewall.sh`) creates `<chain>_NEW` with a bare `iptables -N`, fills it
through a callback, inserts its jumps ahead of the old ones, deletes the old
jumps and chain and renames the new chain; `ipset swap` replaces a set's
contents in one step. Order does the rest: add before remove (`tproxy_apply`
only adds clients, `tproxy_prune` removes them after `tunnel_apply`). Only
`tproxy_stop` and `tunnel_stop` tear down.

When an interrupted swap left a `<chain>_NEW` behind, `swap_fw_chain` first
finishes that swap if a jump leads to it and deletes the chain if none does.
Whether one does is read from a single PREROUTING listing whose status is
checked: a failed listing would read as no jump, and the delete starts with a
flush, so `swap_fw_chain` returns 2 there and changes nothing. `iptables -N`
refuses a chain that is there, and `swap_fw_chain` then returns 2 as well: a
live `<chain>_NEW` the existence check missed is never flushed either —
`create_fw_chain -f` would have emptied it for the whole build.

The stateful iptables mock (`use_stateful_iptables`) records every flush of a
chain a rule still jumps to in `$BATS_IPT_DIR/live_flushes`; a test of an apply
asserts that file stays empty.

### A dual-family DNS lookup on the router often never answers

**Problem**: `/etc/resolv.conf` points the router's own processes straight at
8.8.8.8/8.8.4.4, and anything resolving `AF_UNSPEC` - curl, wget, every
`getaddrinfo` caller - asks for A and AAAA together. The AAAA half goes
unanswered often enough to matter, and glibc waits its full `timeout:` of five
seconds before retrying. Interleaved on an RT-AX86U, same window, same host:

```
curl -s  --connect-timeout 5 --max-time 10 ifconfig.me    16 failures / 30
curl -4 -s --connect-timeout 5 --max-time 10 ifconfig.me   0 failures / 30
```

The failure is `curl: (6) Could not resolve host` arriving at exactly 5.011s:
`--connect-timeout` covers name resolution, so a timeout at or below the
resolver's retry boundary turns a slow lookup into a hard failure. It is not
the binary - Entware's curl fails 10/10 the same way - and not reachability:
`--connect-timeout 20` succeeds in about six seconds. `GetExternalIP` was the
only caller to show this because its 5 s was the only connect timeout in the
repository below the boundary; everything else uses 10 or 30.

**Solution**: ask only for the family the router can route. Everything here is
IPv4 - the ipsets, the TPROXY rules, the fwmark tables - so `-4` requests what
the code actually needs rather than papering over the resolver:

```sh
curl -4 -s --connect-timeout 5 --max-time 10 ifconfig.me
```

Where a wait is acceptable instead, keep the connect timeout above five seconds
so the resolver's second attempt can land.

The shell resolver cannot ask for one family: BusyBox 1.25's `nslookup` on Asuswrt-Merlin takes no
`-type`, and glibc 2.26 predates `options no-aaaa`. `_resolve_ip_impl` bounds the wait instead:
`_resolve_nslookup` runs `nslookup` with `RES_OPTIONS="timeout:1 attempts:2"`, which glibc reads, so
a lost AAAA answer costs about a second per try rather than five. On an RT-AX86U the five OpenVPN
endpoint lookups for `TPROXY_BYPASS` had added about 20 s to every second `tproxy_apply`, the
subscription watch's failover and restore included. KeeneticOS resolves through a local proxy with
`timeout:1` already.

### BusyBox `sh` has no `command` builtin

**Problem**: on Asuswrt-Merlin, `/bin/sh` is BusyBox and `command` is not there:

```
$ /bin/sh -c 'command -v pgrep'
/bin/sh: command: not found       # exit 127, even though /opt/bin/pgrep exists
```

Every `command -v X` in a `#!/bin/sh` script therefore reports "missing" for
tools that are installed. In `update_script.sh.tmpl` this turned the guard
`if ! command -v pgrep` into a refusal of every self-update on the router, and
made the two `command -v monit` gates skip silently, so monit was never
unmonitored during an update and was free to restart a daemon mid-copy.

**Solution**: probe with `type`, which BusyBox does have, and keep `which` as a
fallback for a shell that has neither:

```sh
have_cmd() {
    type "$1" >/dev/null 2>&1 || which "$1" >/dev/null 2>&1
}
```

Scripts that reach bash run under Entware's bash (`/opt/bin/bash`), where
`command -v` works; the rule above is for what stays in `#!/bin/sh`: the
generated update script, the init scripts and the NDM hooks. KeeneticOS's
`/bin/sh` does have `command` (measured on 5.1.5), Asuswrt-Merlin's does not,
so anything shipped to both platforms must assume it is missing.

**Related**: `/bin/bash` on the Merlin router is a symlink to busybox. A login
shell puts `/opt/bin` first in `PATH`, so `curl … | bash` resolves to the real
bash 5.x, but a non-interactive `ssh router 'bash script.sh'` does not — call
`/opt/bin/bash` explicitly there.

### No `/usr/bin/env` on KeeneticOS

**Problem**: KeeneticOS has neither `/usr/bin/env` nor `/bin/env`, and its root
filesystem is a read-only squashfs, so nothing can be added there. A script
shipped with `#!/usr/bin/env bash` dies on rc 126, `bad interpreter` — by hand,
from `S99vpn-director`, from an NDM hook, and from the Go daemons, which run
the CLI through `exec.CommandContext` on its own path.

**Solution**: every script a router executes starts as a POSIX shell and hands
over to bash by absolute path, Entware's first:

```sh
#!/bin/sh
if [ -z "${BASH_VERSION:-}" ]; then
    for _vpd_bash in /opt/bin/bash /usr/bin/bash /bin/bash; do
        # Asuswrt-Merlin's /bin/bash is busybox: a POSIX shell that never sets
        # BASH_VERSION, so exec-ing it would re-run this block forever. Only a
        # candidate that proves it is bash gets the script.
        [ -x "$_vpd_bash" ] && "$_vpd_bash" -c '[ -n "$BASH_VERSION" ]' 2>/dev/null &&
            exec "$_vpd_bash" "$0" "$@"
    done
    echo "$0: bash not found; install it (Entware package \"bash\")" >&2
    exit 1
fi
```

`PATH` is deliberately not consulted: Merlin's `/bin/sh` has no `command`
builtin and its `/bin/bash` is busybox, so both `command -v bash` and a bare
`exec bash` would pick the wrong interpreter or fail. Sourcing the script from
bash (`source install.sh --source-only` in the tests) skips the block, because
`BASH_VERSION` is already set. `router/test/unit/entrypoints.bats` pins the
block in every file that needs it.

### A deleted working directory breaks monit and every shell below it

**Problem**: a process whose cwd has been removed keeps running, but the dead
directory is inherited by everything it starts, and on the router that is not
cosmetic. monit refuses to run at all:

```sh
cd /tmp/gone && rm -rf /tmp/gone && monit status telegram-bot
# AssertException: Monit: Cannot read current directory -- No such file or directory
#  raised in init_env at src/env.c:111        (exit 1)
```

and every shell the affected daemons spawn prefixes its output with

```
shell-init: error retrieving current directory: getcwd: cannot access parent directories: No such file or directory
chdir: error retrieving current directory: getcwd: ...
```

which is what the Web UI's Status page then shows above the real output.

Under `set -e` the same deletion is fatal in another way: a `>>` redirect into
the removed directory fails, and the shell exits at that line.

**Seen as**: the self-update script ran from `/tmp/vpn-director-update`
(`cmd.Dir`), the bot deleted that directory the moment it reported success, and
the script's `monit monitor …  2>/dev/null || true` swallowed the exception —
`telegram-bot` stayed unmonitored, and all three daemons carried a dead cwd.

**Rule**: start long-lived processes from a directory nothing deletes (`/`), and
let a log helper survive the loss of its own file.

A daemon checking at startup whether its own directory still exists is **not** a
defence, and v0.11.4 shipped one that could never fire: the update script starts
the daemons while the directory is still there, and the bot removes it moments
later, once it has reported the update. The move has to be unconditional — with
any relative flag resolved before it, or `--config vpn-director.json` starts
naming `/vpn-director.json`. Dev mode is the exception: `DevPaths` are relative
to the source tree, so it keeps the directory it was started in.

```sh
log() {
    { echo "$1" >> "$LOG_FILE"; } 2>/dev/null || true
}
```

### busybox wget on KeeneticOS segfaults on https

**Problem**: Entware's busybox `wget` on KeeneticOS segfaults on https URLs
(no working TLS). A download that only called wget would never complete.

**Solution**: `download_file` prefers wget, then falls back to curl when wget
is missing or fails. Keenetic installs `curl` as a required package.

### BusyBox before 1.30 spells `timeout` as `-t SECS`

**Problem**: `timeout 15 cmd …` is the coreutils form, which BusyBox learned
only in 1.30. Asuswrt-Merlin ships BusyBox 1.25, whose applet is
`timeout [-t SECS] [-s SIG] PROG ARGS`: it takes the first free argument as the
program, so `timeout 15 xray run -test …` tries to execute `15` and exits 127.
Nothing installs Entware's `coreutils-timeout`, so a router may have only the
applet. In `xrayconf_validate` that turned every config into "xray rejected the
config", and `configure.sh` exits 1 rather than write one.

**Solution**: probe the form — both spell the command `timeout`, so only a run
tells them apart — and leave the command unbounded when neither answers:

```bash
local -a bound=()
if type -P timeout >/dev/null 2>&1; then
    if timeout 1 true >/dev/null 2>&1; then
        bound=(timeout 15)
    elif timeout -t 1 true >/dev/null 2>&1; then
        bound=(timeout -t 15)
    fi
fi
cmd=("${bound[@]}" "${cmd[@]}")
```

A killed process exits 124 under coreutils and 143 under BusyBox. Xray prints
its version banner before it loads the config, so `xrayconf_validate` reads
that exit code — not the output — to tell a timeout
(`xray config test timed out after 15s`) from a rejection.

### jq reads number literals that are not JSON

**Problem**: jq 1.8.1 takes number literals Go's `encoding/json` refuses:

```bash
printf '[nan, NaN, Infinity, .5, 1., +1, 0443]' | jq -c .
# [null,null,1.7976931348623157e+308,0.5,1,1,443]
```

An Xray JSON body, an xhttp `extra` or a v2rayN vmess object holding one
imports through `lib/subscription.sh` and is `invalid JSON subscription` (or an
`invalid` entry) in the Go decoder — the bot, the Web UI and the watch. Only
`nan` and `Infinity` can still be told apart after parsing (`isnan`,
`isinfinite`); the rest are ordinary numbers by then, so parity would take a
pass over the raw text before jq.

**Rule**: the subscription decoders keep this divergence (see the comment
above `_sub_xray_json`): no panel writes such a literal, and the value reaches
Xray as a number or a null. Any other jq that reads untrusted JSON on the
router inherits the same leniency.

### errexit is off inside a function on the left of `||` or `&&`

**Problem**: bash ignores `set -e` in a command on the left of `||` or `&&` — and
in an `if` or `while` condition, and after `!` — and a function called there
runs its whole body that way. A `set -e` inside a subshell there changes
nothing. A command that fails without a check of its own runs on, and the
status that comes back is the last command's:

```bash
set -e
f() { false; echo "ran on past false"; }
f || echo "f failed"                # ran on past false
( set -e; f ) || echo "f failed"    # ran on past false
```

An explicit `exit` still ends the function, so `abort` works anywhere; the stop
at an unchecked failure is what goes.

**Solution**: `import_server_list.sh` runs each menu action through
`run_action`, called as a plain command. The action runs in a subshell, so its
`abort` ends the action and not the menu:

```bash
run_action() {
    set +e
    ( set -e; "$@" )
    ACTION_RC=$?
    set -e
}

run_action menu_add
if (( ACTION_RC != 0 )); then
    exit "$ACTION_RC"
fi
```

`set +e` keeps a failed action from ending the menu, and the subshell turns
errexit back on for the action alone, which works only because nothing around
the call ignores it.

**Rule**: never `run_action … || …`, `run_action … && …`, `! run_action …` or
`if run_action …`: the action then runs with errexit off, and `ACTION_RC` reads
0 for an action whose `false` was followed by a successful command. Read
`ACTION_RC` after the call instead.

### Nothing that can reach 128 KiB goes to jq as an argument

**Problem**: `execve` takes no single argument of 128 KiB or more
(`MAX_ARG_STRLEN`, 32 pages, counts the terminating NUL): 131,071 bytes pass,
and at 131,072 the exec fails with `E2BIG`. jq never starts, bash prints
`<path to jq>: Argument list too long` (`/usr/bin/jq: …` here), and the status
is 126.

```bash
s=$(head -c 131072 /dev/zero | tr '\0' a)
jq -n --arg x "$s" '$x | length'    # /usr/bin/jq: Argument list too long (126)
printf '%s' "$s" | jq -R length     # 131072
```

A subscription list with its outbounds gets there inside the limit of ten
subscriptions: with 300-byte outbounds, about the median of the cases in
`testdata/subscription/`, some 370 servers pass it — ten subscriptions of 37.

**Solution**: hand such a value to jq on stdin (`<<< "$list"`) or from a file
(`--slurpfile`, as `import_server_list.sh` hands over the resolved servers). A
shell function's arguments never meet `execve`, so `lib/substore.sh` takes the
list as a function argument and pipes it in: `substore_name_taken`,
`substore_default_name` and `substore_ips` read it as `<<< "$1"`, and
`substore.bats` runs them and `substore_sync_config` on a list past the limit.
`substore_write` reads its one subscription the same way.

**Rule**: `--arg` and `--argjson` are for values with a small bound — an id, a
name, a time. `xray.servers`, the union of every address, still goes as
`--argjson` (`substore_sync_config`, the last write of `configure.sh`): at 18
bytes an address, it reaches the limit at about 7,300 addresses.
