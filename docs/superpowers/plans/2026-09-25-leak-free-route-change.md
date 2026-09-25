# Leak-Free Route Changes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move a LAN client between Xray and a tunnel in one action from the Web UI and the bot, and make every apply, restart and server switch make-before-break, so that no packet that belongs in a VPN leaves through the WAN while routing changes.

**Architecture:** The shell stops tearing routing down to rebuild it. A new helper, `swap_fw_chain`, builds a chain beside the live one and moves the PREROUTING jumps over; sets are swapped instead of flushed; Tunnel Director keeps each tunnel's slot (mark, preference, table) across applies, so a rebuild installs routing first, swaps the chain and releases the dropped slots last. A full apply runs Xray make (add clients), Tunnel Director, then Xray prune (let go of the clients that left). On top of that, `vpnconfig.MoveClient` changes a client's route in one config update, reached through `POST /api/clients/route`, a select in the Web UI's Route column and a 🔀 button in the bot's `/clients`.

**Tech Stack:** bash with iptables and ipset (BusyBox, Entware jq without regex builtins), bats; Go 1.25 (standard library and the existing telegram-bot-api); Vue 3 + TypeScript (Vite, vue-tsc).

**Spec:** `docs/superpowers/specs/2026-09-25-leak-free-route-change-design.md`. Read it before any task; "spec 8.2" below means its section 8.2.

## Global Constraints

- An apply never flushes a chain or a set that a rule currently uses: no `iptables -F` of a live chain, no `ipset flush` of a live set. Only `tproxy_stop` and `tunnel_stop` tear down, and they run for `stop` and for Tunnel Director's no-tunnels branch alone.
- The shadow of a chain or a set is its name plus `_NEW`: `XRAY_TPROXY_NEW`, `TUN_DIR_NEW`, `XRAY_CLIENTS_NEW`, `TPROXY_BYPASS_NEW`. A chain name may have 24 characters at most (iptables allows 28), a set name 27 (ipset allows 31).
- `swap_fw_chain` returns 0 (swapped, every rule in), 1 (swapped, `build_fn` reported a rule missing), 2 (the live chain untouched), 3 (the cutover unfinished: an interface still on the old chain, or the rename failed).
- A full apply (`apply`, `update`, `restart`) runs `tproxy_apply` → `tunnel_apply` → `tproxy_prune`. `apply xray` and `restart xray` run `tproxy_apply` → `tproxy_prune`; `apply tunnel` runs `tunnel_apply` alone. `TUN_DIR_FORCE_REBUILD=1` makes `tunnel_apply` rebuild even when it reads up to date (`restart`, `restart tunnel`).
- `TUN_DIR_TABLES` keeps its format, one `<idx> <id>` line per tunnel; `mark = (idx + 1) << TUN_DIR_MARK_SHIFT`, `pref = TUN_DIR_PREF_BASE + idx`, Keenetic table `2000 + idx`.
- `POST /api/clients/route` takes `{"ip": "...", "route": "..."}` and answers as the other client routes do.
- Bot callbacks: `clients:move:<ip>`, `clients:to:<route>:<ip>`, `clients:toyes:<route>:<ip>`; « Back and Cancel reuse `clients:rm_no`. `/clients` answers in English.
- Copy: the Web UI asks "`<route>` is down: until it is up, this client's traffic goes out through the WAN. Move anyway?" (the Add form: "… Add anyway?"); the bot asks "`<route>` is down: until it is up, `<ip>`'s traffic goes out through the WAN. Move anyway?".
- Shell: a sourced library starts with `#!/usr/bin/env bash`; use `[[ ]]` and `log -l`; no jq regex builtins (`test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub`). A function that runs under `||` or `if !` checks its commands itself: errexit is off there. The callbacks of `swap_fw_chain` run in the caller's dynamic scope; its own locals carry a `_sw_` prefix.
- No new Go or npm dependency.
- The owner's working rules: run Go and npm build and test commands through the `claude-forge:build-runner` agent; run bats and shellcheck directly; the full bats suite takes minutes — run it in the background and read the counts from its log. Never write to the local memory directory. Commit each task on `feature/leak-free-route-change`; do not push without asking.

## Review Focus

1. **Merlin's firewall start empties every mangle chain** (`iptables -t mangle -F`), the PREROUTING jumps with them, and the hook applies again. Both chains must come back whole, with their jumps, and no flush may count as a live one. Tests: Task 2 (`tproxy_apply` after `mangle -F`), Task 3 (`tunnel_apply` after `mangle -F`).
2. **An NDM rebuild deletes our chains while the ip rules, the routes and `TUN_DIR_TABLES` survive.** The rebuild must keep every slot and touch no ip rule of a kept tunnel. Test: Task 3.
3. **Tunnel Director fails after Xray's make half.** The prune must not run: a client that left Xray stays proxied instead of leaking. Test: Task 2 (the CLI).
4. **A tunnel loses its last client** (moved away, paused). Its slot is released after the swap, and every other tunnel keeps its mark, its rule and its table untouched. Test: Task 3.
5. **A paused client stored as `/32`, moved while a committed failover holds it.** One entry lands on the new route, it stays paused, and the failover record lets it go. Test: Task 5.

## File Structure

| Path | Responsibility |
|---|---|
| `router/opt/vpn-director/lib/firewall.sh` | `swap_fw_chain`, `_fw_chain_cutover`: rebuild a chain beside the live one and move the jumps over |
| `router/opt/vpn-director/lib/tproxy.sh` | `_tproxy_build_chain` and the swapped `XRAY_TPROXY`; `TPROXY_BYPASS` by swap; `XRAY_CLIENTS` add-only; `tproxy_prune`; shadows removed on stop |
| `router/opt/vpn-director/lib/tunnel.sh` | Stable slots (`_tunnel_collect_applied`), `_tunnel_build_chain`, `_tunnel_slot_release`, the rebuild in place; shadow removed on stop |
| `router/opt/vpn-director/vpn-director.sh` | The apply order, `restart` without a stop, the help text |
| `router/test/mocks/stateful/iptables` (new) | An iptables mock that remembers chains and rules and records a flush of a live chain |
| `router/test/test_helper.bash` | `use_stateful_iptables` |
| `router/test/firewall.bats`, `router/test/unit/tproxy.bats`, `router/test/unit/tunnel.bats`, `router/test/integration/vpn_director.bats` | Tests |
| `server/internal/vpnconfig/clients.go` (new) | `ClientRoutes`, `MoveClient` |
| `server/internal/webapi/handler_clients.go`, `router.go` | `checkClientRoute`, `handleMoveClient`, the route |
| `server/internal/handler/clients.go` | The 🔀 button, the route keyboard, the move |
| `server/internal/service/vpndirector.go` | The doc comments of the restart calls |
| `web/src/api.ts`, `web/src/components/ClientsTab.vue` | `moveClient`, the Route select, the confirmation for a tunnel that is down |
| `.claude/rules/*.md`, `README.md`, `README.ru.md`, `CLAUDE.md` | Documentation |

Every commit keeps the suites green. Tasks 1–4 change the shell: the helper first, then Xray together with the new apply order, then Tunnel Director, then `restart`. Tasks 5–8 add the move on top of it, and Task 9 documents it.

---

### Task 1: `swap_fw_chain` and a stateful iptables mock

**Files:**
- Create: `router/test/mocks/stateful/iptables`
- Modify: `router/test/test_helper.bash` (add `use_stateful_iptables` right after `load_firewall`)
- Modify: `router/opt/vpn-director/lib/firewall.sh` (the header list; `swap_fw_chain` and `_fw_chain_cutover` right after `sync_fw_rule`)
- Test: `router/test/firewall.bats`

**Interfaces:**
- Consumes: `fw_chain_exists`, `create_fw_chain`, `delete_fw_chain`, `find_fw_rules`, `purge_fw_rules`, `log` (existing).
- Produces:
  - `swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...` → 0, 1, 2 or 3 (Global Constraints). `build_fn` is called as `build_fn <chain>_NEW` and returns 0, 1 or 2; `pos_fn` prints a 1-based PREROUTING position.
  - `use_stateful_iptables` (test helper): exports `BATS_IPT_DIR` and puts `mocks/stateful` first on PATH. A flush of a chain a rule still jumps to is appended to `$BATS_IPT_DIR/live_flushes` as `<table> <chain>`.

- [ ] **Step 1: Write the stateful mock**

Create `router/test/mocks/stateful/iptables`:

```bash
#!/bin/bash
# An iptables that remembers: every chain of a table is a file under
# $BATS_IPT_DIR/<table>/<chain> holding one "-A <chain> <spec>" line per rule,
# the way "iptables -S" prints them. use_stateful_iptables (test_helper.bash)
# puts it ahead of the stateless mocks/iptables for one test. Every call is
# logged to /tmp/bats_iptables_calls.log, as the stateless mock logs it.
#
# It does one thing iptables does not: a flush of a chain that a rule still
# jumps to - a live chain, whose packets meet no rule until it is filled again -
# is appended to $BATS_IPT_DIR/live_flushes as "<table> <chain>". An apply must
# never write there. A whole-table "-F" (Merlin's firewall start) is not
# recorded.

echo "iptables $*" >> /tmp/bats_iptables_calls.log

dir="${BATS_IPT_DIR:?use_stateful_iptables sets BATS_IPT_DIR}"
table=filter
if [[ ${1:-} == -t ]]; then
    table="$2"
    shift 2
fi
tdir="$dir/$table"
mkdir -p "$tdir"

cmd="${1:-}"
chain="${2:-}"
file="$tdir/$chain"

die() {
    echo "iptables: $1" >&2
    exit "${2:-1}"
}

is_builtin() {
    case "$1" in
        PREROUTING|INPUT|FORWARD|OUTPUT|POSTROUTING) return 0 ;;
    esac
    return 1
}

exists() {
    is_builtin "$1" || [[ -f $tdir/$1 ]]
}

# jumped_to <chain> - does a rule of the table jump to <chain>?
jumped_to() {
    local f
    for f in "$tdir"/*; do
        [[ -f $f ]] || continue
        grep -qE -- "-j $1( |\$)" "$f" && return 0
    done
    return 1
}

# target_known <rule args...> - is the -j target one iptables has, or a chain?
target_known() {
    local prev="" arg
    for arg in "$@"; do
        if [[ $prev == -j ]]; then
            case "$arg" in
                RETURN|ACCEPT|DROP|REJECT|MARK|CONNMARK|TPROXY|PPE|LOG) ;;
                *) exists "$arg" || return 1 ;;
            esac
        fi
        prev="$arg"
    done
    return 0
}

case "$cmd" in
    -S)
        if [[ -z $chain ]]; then
            for c in PREROUTING INPUT FORWARD OUTPUT POSTROUTING; do
                printf -- '-P %s ACCEPT\n' "$c"
            done
            for f in "$tdir"/*; do
                [[ -f $f ]] || continue
                is_builtin "${f##*/}" || printf -- '-N %s\n' "${f##*/}"
            done
            cat "$tdir"/* 2>/dev/null || true
            exit 0
        fi
        exists "$chain" || die "No chain/target/match by that name."
        if is_builtin "$chain"; then
            printf -- '-P %s ACCEPT\n' "$chain"
        else
            printf -- '-N %s\n' "$chain"
        fi
        cat "$file" 2>/dev/null || true
        ;;
    -N)
        exists "$chain" && die "Chain already exists."
        : > "$file"
        ;;
    -F)
        if [[ -z $chain ]]; then
            for f in "$tdir"/*; do
                [[ -f $f ]] && : > "$f"
            done
            exit 0
        fi
        exists "$chain" || die "No chain/target/match by that name."
        if jumped_to "$chain"; then
            printf '%s %s\n' "$table" "$chain" >> "$dir/live_flushes"
        fi
        : > "$file"
        ;;
    -X)
        [[ -f $file ]] || die "No chain/target/match by that name."
        jumped_to "$chain" && die "Too many links."
        [[ -s $file ]] && die "Directory not empty."
        rm -f "$file"
        ;;
    -E)
        new="${3:-}"
        [[ -f $file ]] || die "No chain/target/match by that name."
        exists "$new" && die "File exists."
        sed "s/^-A $chain /-A $new /" "$file" > "$tdir/$new"
        rm -f "$file"
        for f in "$tdir"/*; do
            [[ -f $f ]] || continue
            sed -i -E "s/-j $chain( |\$)/-j $new\\1/" "$f"
        done
        ;;
    -A|-I|-D|-C)
        exists "$chain" || die "No chain/target/match by that name."
        shift 2
        pos=1
        if [[ $cmd == -I && ${1:-} =~ ^[0-9]+$ ]]; then
            pos="$1"
            shift
        fi
        line="-A $chain $*"
        case "$cmd" in
            -A)
                target_known "$@" || die "Couldn't load target." 2
                printf '%s\n' "$line" >> "$file"
                ;;
            -I)
                target_known "$@" || die "Couldn't load target." 2
                touch "$file"
                n=$(wc -l < "$file")
                (( pos >= 1 && pos <= n + 1 )) || die "Index of insertion too big."
                awk -v p="$pos" -v l="$line" 'NR == p { print l } { print } END { if (p == NR + 1) print l }' \
                    "$file" > "$file.tmp" && mv "$file.tmp" "$file"
                ;;
            -D)
                grep -qxF -- "$line" "$file" 2>/dev/null ||
                    die "Bad rule (does a matching rule exist in that chain?)."
                awk -v l="$line" '!done && $0 == l { done = 1; next } { print }' \
                    "$file" > "$file.tmp" && mv "$file.tmp" "$file"
                ;;
            -C)
                grep -qxF -- "$line" "$file" 2>/dev/null || exit 1
                ;;
        esac
        ;;
esac
exit 0
```

Make it executable: `chmod +x router/test/mocks/stateful/iptables`.

In `router/test/test_helper.bash`, right after `load_firewall()`, add:

```bash
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
```

- [ ] **Step 2: Write the failing tests**

Append to `router/test/firewall.bats`:

```bash
# ============================================================================
# swap_fw_chain - rebuild a chain beside the live one, then move the jumps
# ============================================================================

# The callbacks the tests hand swap_fw_chain.
build_new() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; }
build_refused() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; return 2; }
build_incomplete() { ensure_fw_rule -q mangle "$1" -d 172.16.0.0/12 -j RETURN; return 1; }
pos_one() { printf '1\n'; }
pos_two() { printf '2\n'; }

# live_chain <chain> <jump_match...> - a chain holding one old rule and a jump
# to it for every match, the way an earlier apply left them.
live_chain() {
    local chain="$1" match
    shift
    iptables -t mangle -N "$chain"
    iptables -t mangle -A "$chain" -d 10.0.0.0/8 -j RETURN
    for match in "$@"; do
        # shellcheck disable=SC2086
        iptables -t mangle -A PREROUTING $match -j "$chain"
    done
}

# Flushing the live chain and refilling it one rule per call left every packet
# that crossed it meanwhile unrouted - out through the WAN, on every apply.
@test "swap_fw_chain: builds beside the live chain and moves its jump over" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    : > /tmp/bats_iptables_calls.log

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
    # The new jump went in before the old one went, and the old chain went last.
    run grep -E -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW$|-D PREROUTING -i br0 -j XRAY_TPROXY$|-X XRAY_TPROXY$|-E XRAY_TPROXY_NEW XRAY_TPROXY$' \
        /tmp/bats_iptables_calls.log
    assert_line --index 0 --partial '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW'
    assert_line --index 1 --partial '-D PREROUTING -i br0 -j XRAY_TPROXY'
    assert_line --index 2 --partial '-X XRAY_TPROXY'
    assert_line --index 3 --partial '-E XRAY_TPROXY_NEW XRAY_TPROXY'
}

@test "swap_fw_chain: a chain nothing jumps to yet gets its jump where pos_fn says" {
    load_firewall
    use_stateful_iptables
    iptables -t mangle -A PREROUTING -p icmp -j ACCEPT
    iptables -t mangle -A PREROUTING -p igmp -j ACCEPT

    run swap_fw_chain mangle TUN_DIR build_new pos_two "-i br0 -m mark --mark 0x0/0xff0000"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output "$(printf '%s\n' '-P PREROUTING ACCEPT' '-A PREROUTING -p icmp -j ACCEPT' \
        '-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR' '-A PREROUTING -p igmp -j ACCEPT')"
}

# Merlin's firewall start empties every mangle chain, deletes none, and takes
# the jumps with it. The next apply swaps a full chain in over the empty one.
@test "swap_fw_chain: after a whole-table flush the chain comes back with its jump" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -F

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A rule that bounds what the chain takes did not go in: the chain that works
# stays, rather than one that would take too much.
@test "swap_fw_chain: a build that refuses leaves the live chain and its jump alone" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"

    run swap_fw_chain mangle XRAY_TPROXY build_refused pos_one "-i br0"
    assert_failure 2

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 10.0.0.0/8 -j RETURN'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
}

@test "swap_fw_chain: a build that misses a rule still goes in, and says so" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"

    run swap_fw_chain mangle XRAY_TPROXY build_incomplete pos_one "-i br0"
    assert_failure 1

    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
}

# An interface whose new jump the kernel refused keeps the old chain, and
# nothing is deleted under it. The next swap finishes the cutover first.
@test "swap_fw_chain: a jump that does not go in keeps that interface on the old chain" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0" "-i br1"
    iptables() {
        [[ $* == *"-I PREROUTING "*"-i br1 -j XRAY_TPROXY_NEW" ]] && return 1
        command iptables "$@"
    }

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0" "-i br1"
    assert_failure 3
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY_NEW\n-A PREROUTING -i br1 -j XRAY_TPROXY'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]

    unset -f iptables
    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0" "-i br1"
    assert_success
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY\n-A PREROUTING -i br1 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY_NEW
    assert_failure
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A swap that stopped after its new jump went in left two chains, the new one
# complete: the jumps go in only after the build.
@test "swap_fw_chain: finishes a swap that stopped after its jump went in" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN
    iptables -t mangle -I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success
    assert_output --partial "Finishing an interrupted swap of XRAY_TPROXY"

    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -j XRAY_TPROXY'
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

@test "swap_fw_chain: deletes a shadow chain nothing jumps to" {
    load_firewall
    use_stateful_iptables
    live_chain XRAY_TPROXY "-i br0"
    iptables -t mangle -N XRAY_TPROXY_NEW
    iptables -t mangle -A XRAY_TPROXY_NEW -d 192.168.0.0/16 -j RETURN

    run swap_fw_chain mangle XRAY_TPROXY build_new pos_one "-i br0"
    assert_success
    run iptables -t mangle -S XRAY_TPROXY
    assert_output $'-N XRAY_TPROXY\n-A XRAY_TPROXY -d 172.16.0.0/12 -j RETURN'
}

@test "swap_fw_chain: refuses a chain name that leaves no room for _NEW" {
    load_firewall
    use_stateful_iptables
    : > /tmp/bats_iptables_calls.log
    run swap_fw_chain mangle ABCDEFGHIJKLMNOPQRSTUVWXY build_new pos_one "-i br0"
    assert_failure 2
    assert_output --partial "leaves no room for the _NEW suffix"
    [ ! -s /tmp/bats_iptables_calls.log ]
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `bats router/test/firewall.bats`
Expected: the new `swap_fw_chain` tests FAIL with `swap_fw_chain: command not found`; the older tests pass.

- [ ] **Step 4: Write the helper**

In `router/opt/vpn-director/lib/firewall.sh`, add to the Public API list in the header, right after the `sync_fw_rule` entry:

```bash
#   swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...
#       Rebuild a chain that PREROUTING jumps to as <chain>_NEW, move the jumps over and give
#       it the chain's name: no moment without a whole chain. Returns 0 done, 1 done with a
#       rule missing, 2 live chain untouched, 3 cutover unfinished (the next swap finishes it).
#
```

Then insert, right after the closing `}` of `sync_fw_rule` and before the `block_wan_for_host` header block:

```bash
###################################################################################################
# swap_fw_chain - rebuild a chain PREROUTING jumps to, with no moment in which it is not whole
# -------------------------------------------------------------------------------------------------
# Usage:
#   swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...
#
# Args:
#   <table>       : iptables table (the jumps live in its PREROUTING)
#   <chain>       : the chain to rebuild; 24 characters at most, to leave room for "_NEW"
#   <build_fn>    : called as "<build_fn> <chain>_NEW" on an empty chain; returns 0 when every
#                   rule went in, 1 when the chain may go in although a rule did not, and 2 when
#                   it must not go in at all
#   <pos_fn>      : prints the PREROUTING position of the first jump, read from the listing as
#                   it stands; jump N goes to that position + N - 1
#   <jump_match>  : the match of one jump per LAN interface ("-i br0",
#                   "-i br0 -m mark --mark 0x0/0xff0000")
#
# Behavior:
#   * Flushing the live chain and filling it again one rule per call left every packet that
#     crossed it meanwhile unrouted - out through the WAN, on every apply. The rules go into
#     <chain>_NEW instead, and only a finished chain takes the jumps over: a jump to <chain>_NEW
#     goes in ahead of each old jump, the old jumps go, <chain> - nothing jumps to it any more -
#     is emptied and deleted, and <chain>_NEW takes its name ("iptables -E"; the jumps follow the
#     rename). A packet crosses a whole chain at every step. While both jumps stand the new chain
#     comes first, and the old one can only take up what the new one returned.
#   * A <chain>_NEW a jump leads to is what a swap left when it stopped half-way, and it is
#     complete: the jumps go in only after the build. Its cutover is finished first. One that
#     nothing jumps to is deleted.
#   * build_fn runs in the caller's dynamic scope, with errexit off (it runs under "||"), and
#     checks its rules itself. The locals here carry a _sw_ prefix so that none of them can
#     shadow a variable the callback sets.
#   * Returns 0 when the new chain carries every interface; 1 when it does, but build_fn
#     reported a rule missing; 2 when the live chain is untouched (build_fn returned 2, the name
#     is too long, <chain>_NEW could not be made); 3 when the cutover did not finish - an
#     interface whose new jump did not go in stays on the old chain, or the rename failed - and
#     the next swap finishes it.
###################################################################################################
swap_fw_chain() {
    local _sw_table="${1-}" _sw_chain="${2-}" _sw_build="${3-}" _sw_pos="${4-}"
    local _sw_shadow _sw_build_rc=0

    if [[ -z $_sw_table || -z $_sw_chain || -z $_sw_build || -z $_sw_pos || $# -lt 5 ]]; then
        log -l ERROR "swap_fw_chain: usage: swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>..."
        return 2
    fi
    shift 4
    _sw_shadow="${_sw_chain}_NEW"
    # iptables takes chain names of up to 28 characters.
    if (( ${#_sw_shadow} > 28 )); then
        log -l ERROR "Chain name '$_sw_chain' leaves no room for the _NEW suffix (24 characters at most)"
        return 2
    fi

    if fw_chain_exists "$_sw_table" "$_sw_shadow"; then
        if [[ -n $(find_fw_rules "$_sw_table PREROUTING" "-j ${_sw_shadow}\$") ]]; then
            log -l WARN "Finishing an interrupted swap of $_sw_chain"
            _fw_chain_cutover "$_sw_table" "$_sw_chain" "$_sw_pos" "$@" || return 3
        elif ! delete_fw_chain -q "$_sw_table" "$_sw_shadow"; then
            return 2
        fi
    fi

    create_fw_chain -q -f "$_sw_table" "$_sw_shadow" || return 2

    "$_sw_build" "$_sw_shadow" || _sw_build_rc=$?
    if [[ $_sw_build_rc -ge 2 ]]; then
        delete_fw_chain -q "$_sw_table" "$_sw_shadow" || true
        return 2
    fi

    _fw_chain_cutover "$_sw_table" "$_sw_chain" "$_sw_pos" "$@" || return 3
    return "$_sw_build_rc"
}

# _fw_chain_cutover <table> <chain> <pos_fn> <jump_match>... - move the PREROUTING jumps from
# <chain> to <chain>_NEW, then retire <chain> and give <chain>_NEW its name. Returns 1 when a
# jump or the rename did not go in; <chain> then keeps every jump that did not move.
_fw_chain_cutover() {
    local _sw_table="$1" _sw_chain="$2" _sw_pos_fn="$3"
    shift 3
    local _sw_shadow="${_sw_chain}_NEW" _sw_pos _sw_match _sw_i=0 _sw_rc=0
    local -a _sw_moved=()

    _sw_pos="$("$_sw_pos_fn")" || _sw_pos=""
    if [[ ! $_sw_pos =~ ^[1-9][0-9]*$ ]]; then
        log -l ERROR "Cannot determine the PREROUTING position for $_sw_shadow; $_sw_chain stays in place"
        return 1
    fi

    for _sw_match in "$@"; do
        # The match is word-split on purpose: "-i br0 -m mark --mark 0x0/0xff0000".
        # shellcheck disable=SC2086
        if iptables -t "$_sw_table" -C PREROUTING $_sw_match -j "$_sw_shadow" 2>/dev/null \
            || iptables -t "$_sw_table" -I PREROUTING "$((_sw_pos + _sw_i))" $_sw_match -j "$_sw_shadow" 2>/dev/null; then
            _sw_moved+=("$_sw_match")
        else
            log -l ERROR "Failed to insert the PREROUTING jump to $_sw_shadow ($_sw_match); that interface stays on $_sw_chain"
            _sw_rc=1
        fi
        _sw_i=$((_sw_i + 1))
    done

    if [[ $_sw_rc -ne 0 ]]; then
        # Only the interfaces now on the new chain lose their old jump.
        if [[ ${#_sw_moved[@]} -gt 0 ]]; then
            for _sw_match in "${_sw_moved[@]}"; do
                purge_fw_rules -q "$_sw_table PREROUTING" "^-A PREROUTING ${_sw_match} -j ${_sw_chain}\$"
            done
        fi
        return 1
    fi

    # Every interface is on the new chain: every jump to the old one goes, a copy an older
    # version left and an interface no longer listed included.
    purge_fw_rules -q "$_sw_table PREROUTING" "-j ${_sw_chain}\$"
    if [[ -n $(find_fw_rules "$_sw_table PREROUTING" "-j ${_sw_chain}\$") ]]; then
        log -l ERROR "A PREROUTING jump to $_sw_chain did not go; the next apply finishes the swap"
        return 1
    fi
    if fw_chain_exists "$_sw_table" "$_sw_chain"; then
        delete_fw_chain -q "$_sw_table" "$_sw_chain" || return 1
    fi
    if ! iptables -t "$_sw_table" -E "$_sw_shadow" "$_sw_chain" 2>/dev/null; then
        log -l ERROR "Failed to rename $_sw_shadow to $_sw_chain; it carries the traffic until the next apply renames it"
        return 1
    fi
    return 0
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `bats router/test/firewall.bats`
Expected: PASS, every test.

- [ ] **Step 6: shellcheck**

Run: `shellcheck router/opt/vpn-director/lib/firewall.sh`
Expected: nothing the file did not report on `master`.

- [ ] **Step 7: Commit**

```bash
git add router/test/mocks/stateful/iptables router/test/test_helper.bash \
        router/opt/vpn-director/lib/firewall.sh router/test/firewall.bats
git commit -m "feat(shell): swap_fw_chain rebuilds a chain beside the live one"
```

---

### Task 2: Xray — the chain and the sets swapped, clients pruned after Tunnel Director

**Files:**
- Modify: `router/opt/vpn-director/lib/tproxy.sh`
- Modify: `router/opt/vpn-director/vpn-director.sh` (`cmd_apply`, `cmd_update`)
- Test: `router/test/unit/tproxy.bats`, `router/test/integration/vpn_director.bats`

**Interfaces:**
- Consumes: `swap_fw_chain`, `use_stateful_iptables` (Task 1); `_ipset_exists` (`lib/ipset.sh`); `is_ipv4_net` (`lib/common.sh`).
- Produces:
  - `tproxy_prune` — public; swaps `XRAY_CLIENTS` to exactly the effective `xray.clients`; always returns 0.
  - `tproxy_apply` adds to `XRAY_CLIENTS` and never removes.
  - `_tproxy_build_chain <chain>` → 0, 1 (a narrowing RETURN missing) or 2 (a bounding RETURN or a target missing); `_tproxy_jump_pos` prints `1`; `_tproxy_shadow_set <name>`; `_tproxy_swap_set <name>`.
  - `cmd_apply` (all) and `cmd_update` run `tproxy_apply`, `tunnel_apply`, `tproxy_prune`; `cmd_apply xray` runs `tproxy_apply`, `tproxy_prune`.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/tproxy.bats`:

```bash
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
```

In the test `tproxy.sh: exports expected functions`, add after `declare -f tproxy_apply >/dev/null`:

```bash
    declare -f tproxy_prune >/dev/null
```

In `router/test/integration/vpn_director.bats`, replace the whole `run_stubbed_cli` function with:

```bash
# run_stubbed_cli <arguments...> runs the command the CLI parses from its
# arguments with the lock, the boot wait and the ipsets stubbed. Each module call
# that would change routing appends its name to $BATS_TEST_TMPDIR/calls instead;
# tunnel_apply appends " forced" under TUN_DIR_FORCE_REBUILD and returns
# $TUNNEL_APPLY_RC.
run_stubbed_cli() {
    run bash -c '
        script=$1 calls=$2
        shift 2
        source "$script" --source-only "$@"
        _load_modules
        acquire_lock() { :; }
        _ipset_boot_wait() { :; }
        _ensure_ipsets() { :; }
        tproxy_restart_process() { echo tproxy_restart_process >> "$calls"; }
        tproxy_stop() { echo tproxy_stop >> "$calls"; }
        tunnel_stop() { echo tunnel_stop >> "$calls"; }
        tproxy_apply() { echo tproxy_apply >> "$calls"; }
        tunnel_apply() {
            echo "tunnel_apply${TUN_DIR_FORCE_REBUILD:+ forced}" >> "$calls"
            return "${TUNNEL_APPLY_RC:-0}"
        }
        tproxy_prune() { echo tproxy_prune >> "$calls"; }
        "cmd_$COMMAND"
    ' -- "$SCRIPTS_DIR/vpn-director.sh" "$BATS_TEST_TMPDIR/calls" "$@"
}
```

and append after the test `restart --unless-stopped re-applies a running router`:

```bash
# Make before break: a client moving from Xray to a tunnel stays proxied until
# Tunnel Director carries it, so the prune that lets it go runs last.
@test "vpn-director: a full apply adds for Xray, applies Tunnel Director, then prunes" {
    run_stubbed_cli apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune'
}

@test "vpn-director: update applies in the same order" {
    run_stubbed_cli update
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply\ntproxy_prune'
}

@test "vpn-director: apply xray adds and prunes, apply tunnel applies Tunnel Director alone" {
    run_stubbed_cli apply xray
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntproxy_prune'
    rm -f "$BATS_TEST_TMPDIR/calls"
    run_stubbed_cli apply tunnel
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output 'tunnel_apply'
}

# tunnel_apply hard-fails under errexit. The run ends there, before the prune:
# the clients that left Xray stay proxied rather than leave through the WAN.
@test "vpn-director: a Tunnel Director failure ends the apply before the prune" {
    export TUNNEL_APPLY_RC=1
    run_stubbed_cli apply
    assert_failure
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_apply\ntunnel_apply'
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `bats router/test/unit/tproxy.bats router/test/integration/vpn_director.bats`
Expected: the new tests FAIL — `tproxy_prune: command not found`, `ipset flush XRAY_CLIENTS` still called, `XRAY_TPROXY` flushed while live, the CLI calls lacking `tproxy_prune`.

- [ ] **Step 3: Rewrite the sets in `tproxy.sh`**

In the header of `router/opt/vpn-director/lib/tproxy.sh` replace the Dependencies and Public API blocks with:

```bash
# Dependencies:
#   - common.sh (log, tmp_file, is_ipv4_net, rt_table_label) and, through it, the platform contract
#     (platform_load_module, platform_vpn_endpoints, platform_tproxy_extra_rules,
#      platform_lan_ifaces)
#   - firewall.sh (delete_fw_chain, ensure_fw_rule, purge_fw_rules, swap_fw_chain)
#   - config.sh (XRAY_* variables)
#   - ipset.sh (_is_valid_country_code, _ipset_exists)
#
# Public API:
#   tproxy_status()              - show XRAY_TPROXY chain, routing, xray process
#   tproxy_apply()               - the make half of an apply: routing, sets, chain swapped in;
#                                  clients added, never removed; soft-fail if unavailable
#   tproxy_prune()               - the break half: XRAY_CLIENTS becomes exactly xray.clients
#   tproxy_stop()                - remove chain and routing
#   tproxy_restart_process()     - restart Xray process via Entware init script
#   tproxy_get_required_ipsets() - return list of valid exclude ipsets (unknown codes dropped with a WARN)
```

and in the list of internal functions delete the lines of `_tproxy_setup_clients_ipset`, `_tproxy_setup_bypass_ipset` and `_tproxy_setup_iptables` (keep `_tproxy_validate_ipv4_cidr` and the rest) and put in their place, where `_tproxy_setup_clients_ipset` was:

```bash
#   _tproxy_setup_clients_ipset()   - create the clients ipset and add every client (never removes)
#   _tproxy_shadow_set()            - an empty <set>_NEW to fill and swap in
#   _tproxy_swap_set()              - put <set>_NEW in place of <set> in one step
#   _tproxy_setup_bypass_ipset()    - build the bypass ipset beside the live one (3-source assembly)
#   _tproxy_build_chain()           - fill a fresh chain with the TPROXY rules (swap_fw_chain's build_fn)
#   _tproxy_jump_pos()              - where the first XRAY_TPROXY jump goes: position 1
#   _tproxy_setup_iptables()        - platform rules, then the chain swapped in with its jumps
```

Replace the whole `_tproxy_setup_clients_ipset` function, with its comment block, with:

```bash
# -------------------------------------------------------------------------------------------------
# _tproxy_setup_clients_ipset - create the clients ipset and add every effective client to it
# -------------------------------------------------------------------------------------------------
# Always creates the ipset (even if empty) so iptables rules can reference it.
#
# Added to, never flushed: a client on its way from Xray to a tunnel stays in the set - and
# intercepted - until tproxy_prune, which a full apply runs once Tunnel Director has taken it. A
# flush emptied the set until it was filled again, and every Xray client went out through the
# WAN in between.
#
# Returns 1 when any effective client is missing from the set. The chain RETURNs
# every source it does not list, so a missing client is not proxied at all, and
# the watch drops its fallback tunnel on the ready marker tproxy_apply writes.
# An entry that is no IPv4 address or CIDR is skipped with a WARN and does not
# count: an IPv6 address an older Web UI saved, a typo like 192.168.1.1000. No
# kernel set takes it and no tunnel carries it, and waiting for it withheld the
# marker for the whole LAN - every restore held on the fallback tunnel for good.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_clients_ipset() {
    local ip
    local rc=0
    local -a clients_array=()

    # Create ipset if not exists
    if ! ipset list "$XRAY_CLIENTS_IPSET" >/dev/null 2>&1; then
        if ipset create "$XRAY_CLIENTS_IPSET" hash:net; then
            log "Created ipset: $XRAY_CLIENTS_IPSET"
        else
            log -l ERROR "Failed to create $XRAY_CLIENTS_IPSET"
            return 1
        fi
    fi

    # Handle empty XRAY_CLIENTS gracefully
    if [[ -n ${XRAY_CLIENTS:-} ]]; then
        read -ra clients_array <<< "$XRAY_CLIENTS"
        for ip in "${clients_array[@]}"; do
            [[ -n $ip ]] || continue
            if ! is_ipv4_net "$ip"; then
                log -l WARN "Xray client '$ip' is not an IPv4 address or CIDR; skipping"
                continue
            fi
            # -exist: the client is usually in the set already, and xray.clients
            # is never validated - a repeated address is the one failure that
            # means nothing.
            ipset add -exist "$XRAY_CLIENTS_IPSET" "$ip" 2>/dev/null || {
                log -l WARN "Failed to add $ip to $XRAY_CLIENTS_IPSET"
                rc=1
            }
        done
    fi

    log "Added the Xray clients to $XRAY_CLIENTS_IPSET (${#clients_array[@]} entries)"
    return $rc
}

# -------------------------------------------------------------------------------------------------
# _tproxy_shadow_set <name> - an empty <name>_NEW to fill and then swap in for <name>
# -------------------------------------------------------------------------------------------------
# ipset takes names of up to 31 characters, so <name> may have 27 at most. No rule names
# <name>_NEW, so emptying one an interrupted apply left opens no window.
# -------------------------------------------------------------------------------------------------
_tproxy_shadow_set() {
    local shadow="${1}_NEW"
    if (( ${#shadow} > 31 )); then
        log -l ERROR "ipset name '$1' leaves no room for the _NEW suffix (27 characters at most)"
        return 1
    fi
    if _ipset_exists "$shadow"; then
        ipset flush "$shadow" 2>/dev/null
    else
        ipset create "$shadow" hash:net 2>/dev/null
    fi
}

# -------------------------------------------------------------------------------------------------
# _tproxy_swap_set <name> - put <name>_NEW in place of <name> in one step, then drop the old entries
# -------------------------------------------------------------------------------------------------
# "ipset swap" exchanges the two sets under the rules that name them atomically: a packet meets
# the old entries or the new ones, never an empty set.
# -------------------------------------------------------------------------------------------------
_tproxy_swap_set() {
    local name="$1" shadow="${1}_NEW"
    if ! _ipset_exists "$name"; then
        ipset create "$name" hash:net 2>/dev/null || return 1
    fi
    ipset swap "$shadow" "$name" 2>/dev/null || return 1
    ipset destroy "$shadow" 2>/dev/null || true
}
```

Replace the whole `_tproxy_setup_bypass_ipset` function, with its comment block, with:

```bash
# -------------------------------------------------------------------------------------------------
# _tproxy_setup_bypass_ipset - setup bypass ipset (3-source assembly)
# -------------------------------------------------------------------------------------------------
# Merges three sources into the bypass ipset:
#   1. Xray server IPs from config (xray.servers)
#   2. User-defined exclude IPs from config (xray.exclude_ips)
#   3. Firmware VPN client endpoints from platform_vpn_endpoints (resolved on the fly)
# The entries go into TPROXY_BYPASS_NEW, which is then swapped in whole: a flush emptied the live
# set until it was filled again, and a subscription server, an excluded address or a firmware VPN
# endpoint was proxied in between. Returns 1, the live set left as it was, when the new set
# cannot be made or swapped in.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_bypass_ipset() {
    local ip addr resolved
    local -a servers_array=()
    local -a exclude_ips_array=()
    local xray_count=0 user_count=0 ovpn_count=0
    local shadow="${XRAY_BYPASS_IPSET}_NEW"

    if ! _tproxy_shadow_set "$XRAY_BYPASS_IPSET"; then
        log -l ERROR "Cannot prepare $shadow; $XRAY_BYPASS_IPSET keeps the entries it has"
        return 1
    fi

    # Source 1: Xray server IPs from config
    if [[ -n ${XRAY_SERVERS:-} ]]; then
        read -ra servers_array <<< "$XRAY_SERVERS"
        for ip in "${servers_array[@]}"; do
            [[ -n $ip ]] || continue
            ipset add "$shadow" "$ip" 2>/dev/null && xray_count=$((xray_count + 1)) || {
                log -l WARN "Failed to add xray server $ip to $XRAY_BYPASS_IPSET"
            }
        done
    fi

    # Source 2: User-defined exclude IPs from config (validated)
    if [[ -n ${XRAY_EXCLUDE_IPS:-} ]]; then
        read -ra exclude_ips_array <<< "$XRAY_EXCLUDE_IPS"
        for ip in "${exclude_ips_array[@]}"; do
            [[ -n $ip ]] || continue
            # Validate IPv4 or IPv4 CIDR before adding
            if ! _tproxy_validate_ipv4_cidr "$ip"; then
                log -l WARN "Invalid exclude_ips entry '$ip', skipping"
                continue
            fi
            ipset add "$shadow" "$ip" 2>/dev/null && user_count=$((user_count + 1)) || {
                log -l WARN "Failed to add user exclude IP $ip to $XRAY_BYPASS_IPSET"
            }
        done
    fi

    # Source 3: endpoints of the firmware's own VPN clients, so their traffic
    # never enters the proxy (resolved on the fly)
    while IFS= read -r addr; do
        [[ -n $addr ]] || continue

        resolved=$(resolve_ip -a -q "$addr" 2>/dev/null) || {
            log -l WARN "Cannot resolve VPN endpoint $addr"
            continue
        }

        while IFS= read -r ip; do
            [[ -n $ip ]] || continue
            ipset add "$shadow" "$ip" 2>/dev/null && ovpn_count=$((ovpn_count + 1)) || true
        done <<< "$resolved"
    done < <(platform_vpn_endpoints || true)

    if ! _tproxy_swap_set "$XRAY_BYPASS_IPSET"; then
        log -l ERROR "Cannot swap $shadow into $XRAY_BYPASS_IPSET; it keeps the entries it has"
        return 1
    fi

    local total=$((xray_count + user_count + ovpn_count))
    log "Populated $XRAY_BYPASS_IPSET ipset: $xray_count xray, $user_count user, $ovpn_count openvpn = $total total"
}
```

- [ ] **Step 4: Swap the chain in `tproxy.sh`**

Replace the whole `_tproxy_setup_iptables` function, with its comment block, with:

```bash
# -------------------------------------------------------------------------------------------------
# _tproxy_jump_pos - where the first XRAY_TPROXY jump goes: position 1, ahead of Tunnel Director
# -------------------------------------------------------------------------------------------------
_tproxy_jump_pos() {
    printf '1\n'
}

# -------------------------------------------------------------------------------------------------
# _tproxy_build_chain <chain> - fill a fresh chain with the TPROXY rules (swap_fw_chain's build_fn)
# -------------------------------------------------------------------------------------------------
# The rules ahead of the targets are of two kinds. Rule 1 and the private ranges of rule 4 bound
# what the targets take - without rule 1 every LAN client, without rule 4 traffic to the router
# and the rest of the LAN - so a failure there, or of a target itself, returns 2: the chain must
# not go in, and the chain already in place keeps working. The others only decide what a client
# reaches directly instead of through the proxy: a failure there is logged and returns 1, and the
# chain goes in without the rule, the ready marker withheld. Keeping the chain out for those as
# well left every Xray client on the WAN over one busy xtables lock.
# -------------------------------------------------------------------------------------------------
_tproxy_build_chain() {
    local chain="$1" exclude_set resolved_set narrow_rc=0
    local -a exclude_sets_array

    read -ra exclude_sets_array <<< "$(_tproxy_exclude_sets -q)"

    # Rule 1: Skip if source is not in our clients ipset
    ensure_fw_rule -q mangle "$chain" \
        -m set ! --match-set "$XRAY_CLIENTS_IPSET" src -j RETURN || return 2

    # Rule 2: Skip traffic to bypass destinations (Xray servers, user excludes, OpenVPN endpoints)
    ensure_fw_rule -q mangle "$chain" \
        -m set --match-set "$XRAY_BYPASS_IPSET" dst -j RETURN || narrow_rc=1

    # Rule 3: Skip local destinations (loopback)
    ensure_fw_rule -q mangle "$chain" \
        -d 127.0.0.0/8 -j RETURN || narrow_rc=1

    # Rule 4: Skip private network destinations (RFC1918)
    ensure_fw_rule -q mangle "$chain" \
        -d 10.0.0.0/8 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 172.16.0.0/12 -j RETURN || return 2
    ensure_fw_rule -q mangle "$chain" \
        -d 192.168.0.0/16 -j RETURN || return 2

    # Rule 5: Skip link-local
    ensure_fw_rule -q mangle "$chain" \
        -d 169.254.0.0/16 -j RETURN || narrow_rc=1

    # Rule 6: Skip multicast
    ensure_fw_rule -q mangle "$chain" \
        -d 224.0.0.0/4 -j RETURN || narrow_rc=1

    # Rule 7: Skip broadcast
    ensure_fw_rule -q mangle "$chain" \
        -d 255.255.255.255/32 -j RETURN || narrow_rc=1

    # Rule 8: Skip excluded country/custom ipsets
    for exclude_set in "${exclude_sets_array[@]}"; do
        [[ -n $exclude_set ]] || continue
        if ! resolved_set="$(_tproxy_resolve_exclude_set "$exclude_set")"; then
            # _tproxy_check_required_ipsets has just seen it; the set went away since.
            log -l ERROR "Exclusion ipset '$exclude_set' not found; its destinations are proxied"
            narrow_rc=1
            continue
        fi
        if ensure_fw_rule -q mangle "$chain" \
            -m set --match-set "$resolved_set" dst -j RETURN; then
            log "Added exclusion for ipset: $resolved_set"
        else
            narrow_rc=1
        fi
    done
    if [[ $narrow_rc -ne 0 ]]; then
        log -l WARN "A TPROXY exclusion did not go in; its destinations are proxied until the next apply"
    fi

    # Rule 9: Apply TPROXY for remaining traffic.
    ensure_fw_rule -q mangle "$chain" \
        -p tcp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2
    ensure_fw_rule -q mangle "$chain" \
        -p udp -j TPROXY --on-port "$XRAY_TPROXY_PORT" \
        --tproxy-mark "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || return 2

    return "$narrow_rc"
}

# -------------------------------------------------------------------------------------------------
# _tproxy_setup_iptables - the platform's rules, then XRAY_TPROXY swapped in with its jumps
# -------------------------------------------------------------------------------------------------
# Returns 1 whenever the ready marker has to wait: the chain did not go in (the one in place
# stays), a narrowing RETURN or a jump is missing, or the platform's own rules are.
# -------------------------------------------------------------------------------------------------
_tproxy_setup_iptables() {
    local lan_ifaces lan_if
    local -a jump_matches=()

    # The PREROUTING jumps are what make this chain matter, so ask for the
    # LAN interfaces before touching any firewall state. A platform that cannot
    # name them would otherwise leave a fully populated chain with nothing
    # jumping to it: every packet the proxy exists to carry goes direct, and the
    # function still ends in "Applied TPROXY iptables rules".
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Cannot determine the LAN interfaces; TPROXY rules not applied"
        return 1
    fi
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        jump_matches+=("-i $lan_if")
    done <<< "$lan_ifaces"

    # Rules the platform needs outside our chain (Keenetic: mangle INPUT accept),
    # in place before the new chain takes the jumps. The status is ours to
    # report: this function runs under "if !", which turns errexit off for its
    # whole body. A failure here fails the function only after the swap, which
    # still happens: a router without the subscription watch keeps its
    # interception, and what changes is the ready marker tproxy_apply writes -
    # the watch waits for it before Xray clients leave the fallback tunnel.
    local extra_rc=0
    if ! platform_tproxy_extra_rules apply "$XRAY_FWMARK/$XRAY_FWMARK_MASK"; then
        log -l WARN "Failed to apply platform TPROXY rules; proxied traffic may be dropped"
        extra_rc=1
    fi

    # The chain is built beside the live one and swapped in: flushing the live
    # chain and filling it one rule per call left every Xray client unproxied -
    # out through the WAN - for the length of the refill, on every apply. Each
    # LAN interface gets its own jump, from position 1: ahead of Tunnel
    # Director, and none displacing another.
    local swap_rc=0
    swap_fw_chain mangle "$XRAY_CHAIN" _tproxy_build_chain _tproxy_jump_pos "${jump_matches[@]}" || swap_rc=$?
    if [[ $swap_rc -eq 2 ]]; then
        log -l ERROR "TPROXY rules not rebuilt; the rules already in place stay"
    fi
    [[ $swap_rc -eq 0 && $extra_rc -eq 0 ]] || return 1

    log "Applied TPROXY iptables rules"
}
```

Replace the whole `_tproxy_teardown_iptables` function with:

```bash
_tproxy_teardown_iptables() {
    # Best effort: tproxy_stop calls this bare under errexit, so a platform whose
    # cleanup fails must not abort the teardown before the jump and the chain go.
    platform_tproxy_extra_rules stop "$XRAY_FWMARK/$XRAY_FWMARK_MASK" || true
    # The jumps to a shadow an interrupted swap left go too.
    purge_fw_rules -q "mangle PREROUTING" "-j ${XRAY_CHAIN}(_NEW)?\$"
    delete_fw_chain -q mangle "$XRAY_CHAIN"
    delete_fw_chain -q mangle "${XRAY_CHAIN}_NEW" || true

    # Remove ipsets, and the shadows an interrupted apply left
    ipset destroy "$XRAY_CLIENTS_IPSET" 2>/dev/null || true
    ipset destroy "$XRAY_BYPASS_IPSET" 2>/dev/null || true
    ipset destroy "${XRAY_CLIENTS_IPSET}_NEW" 2>/dev/null || true
    ipset destroy "${XRAY_BYPASS_IPSET}_NEW" 2>/dev/null || true

    log "Removed TPROXY iptables rules and ipsets"
}
```

- [ ] **Step 5: `tproxy_apply` and `tproxy_prune`**

In `tproxy_apply`, replace its comment block with:

```bash
# -------------------------------------------------------------------------------------------------
# tproxy_apply - the make half of an apply: TPROXY routing, sets and chain (idempotent)
# -------------------------------------------------------------------------------------------------
# Adds every effective client to XRAY_CLIENTS and removes none: tproxy_prune does that after
# Tunnel Director has taken the clients that left Xray (vpn-director.sh cmd_apply). Soft-fails if
# xt_TPROXY is unavailable or ipsets are missing, returning 0 so caller scripts can continue.
# -------------------------------------------------------------------------------------------------
```

and replace, inside it, the lines from `_tproxy_setup_bypass_ipset` down to the soft-fail block with:

```bash
    # A bypass set that could not be swapped keeps its entries and has said so.
    _tproxy_setup_bypass_ipset || true

    # Soft-fail if iptables setup fails
    if ! _tproxy_setup_iptables; then
        log -l WARN "TPROXY rules are not complete; readiness withheld"
        rm -f "$XRAY_TPROXY_READY"
        return 0
    fi
```

Then add, right after `tproxy_apply`:

```bash
# -------------------------------------------------------------------------------------------------
# tproxy_prune - let go of the clients xray.clients no longer names
# -------------------------------------------------------------------------------------------------
# The break half of a full apply (vpn-director.sh): tproxy_apply only adds to XRAY_CLIENTS, and a
# client that left Xray stays intercepted until Tunnel Director has taken it. The exact set is
# built as XRAY_CLIENTS_NEW and swapped in. A client the new set does not take would lose its
# interception with the swap, so the prune stops there instead: the clients that left stay
# proxied until the next apply, which is no leak. Always returns 0.
# -------------------------------------------------------------------------------------------------
tproxy_prune() {
    _tproxy_init

    local ip shadow="${XRAY_CLIENTS_IPSET}_NEW" count=0
    local -a clients_array=()

    # tproxy_apply soft-failed before it made the set: there is nothing to prune.
    _ipset_exists "$XRAY_CLIENTS_IPSET" || return 0

    if ! _tproxy_shadow_set "$XRAY_CLIENTS_IPSET"; then
        log -l WARN "Cannot prepare $shadow; clients that left Xray stay proxied until the next apply"
        return 0
    fi
    if [[ -n ${XRAY_CLIENTS:-} ]]; then
        read -ra clients_array <<< "$XRAY_CLIENTS"
        for ip in "${clients_array[@]}"; do
            [[ -n $ip ]] || continue
            # Skipped by tproxy_apply too, with a WARN: no set takes it.
            is_ipv4_net "$ip" || continue
            if ! ipset add -exist "$shadow" "$ip" 2>/dev/null; then
                log -l WARN "Cannot add $ip to $shadow; clients that left Xray stay proxied until the next apply"
                ipset destroy "$shadow" 2>/dev/null || true
                return 0
            fi
            count=$((count + 1))
        done
    fi
    if ! _tproxy_swap_set "$XRAY_CLIENTS_IPSET"; then
        log -l WARN "Cannot swap $shadow into $XRAY_CLIENTS_IPSET; clients that left Xray stay proxied until the next apply"
        return 0
    fi
    log "Pruned $XRAY_CLIENTS_IPSET to the $count clients xray.clients names"
    return 0
}
```

- [ ] **Step 6: The apply order in `vpn-director.sh`**

In `cmd_apply`, replace the `""|all)` branch's comment and its two calls (from `# Xray first.` through `tunnel_apply`) with:

```bash
            # Make before break, whichever way a client moves. tproxy_apply adds every
            # client xray.clients names to XRAY_CLIENTS and removes none; tunnel_apply puts
            # Tunnel Director's rules in place; only then does tproxy_prune let go of the
            # clients that left Xray. XRAY_TPROXY runs ahead of TUN_DIR, so a client moving
            # from Xray to a tunnel stays proxied until TUN_DIR marks it, and one moving the
            # other way is proxied before TUN_DIR lets it go. Applied Xray-then-Tunnel
            # Director in one step, a client moving to a tunnel was on neither for the length
            # of tunnel_apply and left through the WAN.
            #
            # tproxy_apply soft-fails (a WARN and rc 0) while tunnel_apply hard-fails under
            # errexit: a Tunnel Director failure - a malformed tunnels object, say - ends the
            # run before the prune, and the clients that left Xray stay proxied rather than
            # leak. The PREROUTING positions: XRAY_TPROXY always goes in at 1; TUN_DIR at the
            # platform's base position, never ahead of the XRAY_TPROXY jumps - tunnel_apply
            # counts them - so the order is [XRAY_TPROXY, TUN_DIR] on both platforms. No call
            # is wrapped in `||` or `if !`: that would run its whole body with errexit off
            # and let an unguarded failure inside pass as success.
            tproxy_apply
            tunnel_apply
            tproxy_prune
```

In the `xray|tproxy)` branch of `cmd_apply`, replace the single `tproxy_apply` line with:

```bash
            # A component apply does not wait for Tunnel Director: a client moving
            # between the two needs a full apply.
            tproxy_apply
            tproxy_prune
```

In `cmd_update`, replace

```bash
    # Xray first, for the reason spelled out in cmd_apply.
    tproxy_apply
    tunnel_apply
```

with

```bash
    # Make before break, for the reason spelled out in cmd_apply.
    tproxy_apply
    tunnel_apply
    tproxy_prune
```

- [ ] **Step 7: Update the tests that name the live chain or set**

In `router/test/unit/tproxy.bats`:

In `_tproxy_setup_bypass_ipset: adds every platform VPN endpoint`, replace the two greps with:

```bash
    grep -q "ipset add TPROXY_BYPASS_NEW 203.0.113.7" /tmp/bats_ipset_calls.log
    grep -q "ipset add TPROXY_BYPASS_NEW 198.51.100.9" /tmp/bats_ipset_calls.log
```

In `_tproxy_setup_iptables: applies platform extra rules with the configured mark and one jump per LAN interface`, replace the two jump greps with:

```bash
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
    grep -q -- '-I PREROUTING 2 -i br1 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
```

In `_tproxy_setup_iptables: fails after installing the jumps when the platform cannot apply its extra rules`, replace the jump grep with:

```bash
    grep -q -- '-I PREROUTING 1 -i br0 -j XRAY_TPROXY_NEW' /tmp/bats_iptables_calls.log
```

Replace the whole test `_tproxy_setup_iptables: fails before the TPROXY targets when a rule that bounds them fails`, comment included, with:

```bash
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
        "-F XRAY_TPROXY_NEW" \
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
```

(The stateless mock reports every chain as present, so the shadow is "created" with `-F XRAY_TPROXY_NEW`: failing that is failing to make the shadow.)

Replace the whole test `_tproxy_setup_iptables: a RETURN that only narrows the proxy keeps the TPROXY targets`, keeping its comment, with:

```bash
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
```

- [ ] **Step 8: Run the tests to see them pass**

Run: `bats router/test/unit/tproxy.bats router/test/integration/vpn_director.bats router/test/firewall.bats`
Expected: PASS, every test.

- [ ] **Step 9: shellcheck**

Run: `shellcheck router/opt/vpn-director/lib/tproxy.sh router/opt/vpn-director/vpn-director.sh`
Expected: nothing the files did not report on `master`.

- [ ] **Step 10: Commit**

```bash
git add router/opt/vpn-director/lib/tproxy.sh router/opt/vpn-director/vpn-director.sh \
        router/test/unit/tproxy.bats router/test/integration/vpn_director.bats
git commit -m "feat(shell): Xray's chain and sets are swapped in, and a full apply prunes after Tunnel Director"
```

---

### Task 3: Tunnel Director — stable slots and the rebuild in place

**Files:**
- Modify: `router/opt/vpn-director/lib/tunnel.sh`
- Test: `router/test/unit/tunnel.bats`

**Interfaces:**
- Consumes: `swap_fw_chain`, `use_stateful_iptables` (Task 1).
- Produces:
  - `_tunnel_collect_applied <prev>` — prints `<idx> <tunnel>` per tunnel of the new layout, in JSON order; a tunnel in `<prev>` keeps its idx, a new one takes the lowest idx that neither layout holds.
  - `_tunnel_emit_client <chain> <client> <tunnel> <mark_hex> <excludes>` (the chain is now the first argument).
  - `_tunnel_build_chain <chain>` — `swap_fw_chain`'s build_fn; reads `slots_tmp`, sets `fo_carried` (dynamic scope); returns 0.
  - `_tunnel_slot_release <idx> <tunnel>`.
  - `tunnel_apply` honours `TUN_DIR_FORCE_REBUILD=1`; `_tunnel_sync_jumps` is gone.

- [ ] **Step 1: Write the failing tests**

Append to `router/test/unit/tunnel.bats` (after `load_tunnel_module_with`, which the new tests use — the end of the file is fine):

```bash
# ============================================================================
# The rebuild in place: stable slots, make before break
# ============================================================================

@test "_tunnel_collect_applied: without a record the slots follow the JSON order from 0" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"ovpnc2":{"clients":["192.168.1.6"]}}'
    _tunnel_init
    _tunnel_collect_applied /dev/null > "$BATS_TEST_TMPDIR/slots" 2>/dev/null
    run cat "$BATS_TEST_TMPDIR/slots"
    assert_output $'0 wgc1\n1 ovpnc2'
}

# A slot is a mark, a preference and on Keenetic a table. A tunnel that keeps its
# clients keeps its slot; a new one takes a slot neither layout holds, since the
# slot this apply frees still carries marks until the swap.
@test "_tunnel_collect_applied: a recorded tunnel keeps its idx, a new one takes the lowest idx neither layout holds" {
    load_tunnel_module_with '{"ovpnc1":{"clients":["192.168.1.7"]},"ovpnc2":{"clients":["192.168.1.6"]}}'
    _tunnel_init
    printf '0 wgc1\n1 ovpnc2\n' > "$BATS_TEST_TMPDIR/prev"
    _tunnel_collect_applied "$BATS_TEST_TMPDIR/prev" > "$BATS_TEST_TMPDIR/slots" 2>/dev/null
    run cat "$BATS_TEST_TMPDIR/slots"
    assert_output $'2 ovpnc1\n1 ovpnc2'
}

@test "_tunnel_collect_applied: a tunnel that lost its clients gives up its slot" {
    load_tunnel_module_with '{"wgc1":{"clients":[]},"ovpnc2":{"clients":["192.168.1.6"]}}'
    _tunnel_init
    printf '0 wgc1\n1 ovpnc2\n' > "$BATS_TEST_TMPDIR/prev"
    _tunnel_collect_applied "$BATS_TEST_TMPDIR/prev" > "$BATS_TEST_TMPDIR/slots" 2>/dev/null
    run cat "$BATS_TEST_TMPDIR/slots"
    assert_output '1 ovpnc2'
}

# A rebuild used to start with tunnel_stop: every Tunnel Director client left
# through the WAN until the last rule was back - on every change of any client.
@test "tunnel_apply: a rebuild moves the jump to the rebuilt chain and never flushes the live one" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    use_stateful_iptables
    run tunnel_apply
    assert_success
    TUN_DIR_TUNNELS_JSON='{"wgc1":{"clients":["192.168.1.5","192.168.1.6"]}}'

    run tunnel_apply
    assert_success
    refute_output --partial "Stopping Tunnel Director"
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.6 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
    run iptables -t mangle -S TUN_DIR_NEW
    assert_failure
}

# The move of a client off its tunnel (Review Focus 4): the tunnel that loses
# its last client gives up its slot after the swap; the other keeps its mark,
# its rule and its table, touched by nothing.
@test "tunnel_apply: a tunnel that keeps its clients keeps its slot, its ip rule and its table" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]},"ovpnc2":{"clients":["192.168.1.6"]}}'
    use_stateful_iptables
    run tunnel_apply
    assert_success
    run cat "$TUN_DIR_TABLES"
    assert_output $'0 wgc1\n1 ovpnc2'

    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/release.log"; }
    : > /tmp/bats_ip_calls.log
    TUN_DIR_TUNNELS_JSON='{"wgc1":{"clients":[]},"ovpnc2":{"clients":["192.168.1.6"]}}'
    run tunnel_apply
    assert_success

    run cat "$TUN_DIR_TABLES"
    assert_output '1 ovpnc2'
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.6 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x20000/0xff0000'
    refute grep -q 'pref 16385' /tmp/bats_ip_calls.log
    grep -q 'ip rule del pref 16384 fwmark 0x10000/0xff0000 lookup wgc1' /tmp/bats_ip_calls.log
    run cat "$BATS_TEST_TMPDIR/release.log"
    assert_output 'release wgc1 0'
    run ip rule show
    assert_line $'16385:\tfrom all fwmark 0x20000/0xff0000 lookup ovpnc2'
    refute_line --partial '16384:'
}

@test "tunnel_apply: a new slot's ip rule goes in before the swap, a released one's goes after it" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success
    ip() {
        if [[ ${1:-} == rule && ( ${2:-} == add || ${2:-} == del ) ]]; then
            echo "ip $*" >> "$BATS_TEST_TMPDIR/order.log"
        fi
        command ip "$@"
    }
    iptables() {
        [[ $* == *" -E "* ]] && echo "iptables $*" >> "$BATS_TEST_TMPDIR/order.log"
        command iptables "$@"
    }
    TUN_DIR_TUNNELS_JSON='{"ovpnc2":{"clients":["192.168.1.6"]}}'

    run tunnel_apply
    assert_success
    run cat "$BATS_TEST_TMPDIR/order.log"
    assert_line --index 0 'ip rule del pref 16385'
    assert_line --index 1 'ip rule add pref 16385 fwmark 0x20000/0xff0000 lookup ovpnc2'
    assert_line --index 2 'iptables -t mangle -E TUN_DIR_NEW TUN_DIR'
    assert_line --index 3 'ip rule del pref 16384 fwmark 0x10000/0xff0000 lookup wgc1'
}

# Review Focus 2: an NDM rebuild deletes our chain and jumps; the ip rules,
# the routes and TUN_DIR_TABLES survive it. The rebuild puts the chain back and
# leaves every rule of a kept tunnel as it is.
@test "tunnel_apply: after NDM deleted the chain, the rebuild touches no ip rule of a kept tunnel" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    use_stateful_iptables
    run tunnel_apply
    assert_success
    rm -rf "$BATS_IPT_DIR/mangle"
    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/release.log"; }
    : > /tmp/bats_ip_calls.log

    run tunnel_apply
    assert_success
    assert_output --partial "rebuilding"
    refute grep -qE 'ip rule (add|del)' /tmp/bats_ip_calls.log
    [ ! -e "$BATS_TEST_TMPDIR/release.log" ]
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    [ -f "$TUN_DIR_HASH" ]
}

# Review Focus 1: Merlin's firewall start empties TUN_DIR, deletes nothing and
# takes the jump; firewall-start applies again.
@test "tunnel_apply: after Merlin emptied mangle, the rebuild marks every client again" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    use_stateful_iptables
    run tunnel_apply
    assert_success
    iptables -t mangle -F

    run tunnel_apply
    assert_success
    run iptables -t mangle -S TUN_DIR
    assert_line '-A TUN_DIR -s 192.168.1.5 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
    run iptables -t mangle -S PREROUTING
    assert_output $'-P PREROUTING ACCEPT\n-A PREROUTING -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR'
    [ ! -s "$BATS_IPT_DIR/live_flushes" ]
}

# A swap that did not take over every interface leaves the old chain marking with
# the old slots: they are not released, and the hash is not recorded, so the
# next apply rebuilds, finishes the swap and releases them.
@test "tunnel_apply: a rebuild whose swap did not finish records no hash and releases no slot" {
    load_tunnel_module_with '{"wgc1":{"clients":["192.168.1.5"]}}'
    run tunnel_apply
    assert_success

    eval "real_$(declare -f swap_fw_chain)"
    swap_fw_chain() { return 3; }
    platform_tunnel_table_release() { echo "release $1 $2" >> "$BATS_TEST_TMPDIR/release.log"; }
    TUN_DIR_TUNNELS_JSON='{"ovpnc2":{"clients":["192.168.1.6"]}}'
    run tunnel_apply
    assert_success
    [ ! -f "$TUN_DIR_HASH" ]
    run cat "$TUN_DIR_TABLES"
    assert_output $'0 wgc1\n1 ovpnc2'
    refute grep -q "release wgc1" "$BATS_TEST_TMPDIR/release.log"

    swap_fw_chain() { real_swap_fw_chain "$@"; }
    run tunnel_apply
    assert_success
    [ -f "$TUN_DIR_HASH" ]
    run cat "$TUN_DIR_TABLES"
    assert_output '1 ovpnc2'
    grep -q "release wgc1 0" "$BATS_TEST_TMPDIR/release.log"
}

@test "tunnel_apply: TUN_DIR_FORCE_REBUILD rebuilds a chain that reads up to date" {
    load_tunnel_module
    run tunnel_apply
    assert_success
    fw_chain_exists() { return 0; }
    marks_in_place
    export TUN_DIR_FORCE_REBUILD=1

    run tunnel_apply
    assert_success
    assert_output --partial "Rebuilding Tunnel Director in place"
    refute_output --partial "up-to-date"
}

@test "tunnel_stop: removes a shadow chain an interrupted swap left" {
    load_tunnel_module
    : > /tmp/bats_iptables_calls.log
    run tunnel_stop
    assert_success
    grep -q -- '-t mangle -X TUN_DIR_NEW' /tmp/bats_iptables_calls.log
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `bats router/test/unit/tunnel.bats`
Expected: the new tests FAIL (`_tunnel_collect_applied` ignores the record; a rebuild runs `tunnel_stop`; no `TUN_DIR_NEW`; no forced rebuild).

- [ ] **Step 3: The header of `tunnel.sh`**

In `router/opt/vpn-director/lib/tunnel.sh`, replace the firewall.sh line of the Dependencies block with:

```bash
#   - firewall.sh (delete_fw_chain, ensure_fw_rule, sync_fw_rule, swap_fw_chain,
#                  purge_fw_rules, find_fw_rules, fw_chain_exists)
```

replace the `tunnel_apply()` line of the Public API block with:

```bash
#   tunnel_apply()               - apply rules from config (idempotent; a rebuild happens in place)
```

and add to the internal functions list, after `_tunnel_marks_present()`:

```bash
#   _tunnel_collect_applied()    - the slot of every tunnel: kept from TUN_DIR_TABLES, or the lowest free
#   _tunnel_build_chain()        - fill a fresh chain with every client's rules (swap_fw_chain's build_fn)
#   _tunnel_slot_release()       - drop the ip rule and the table of a slot the new layout dropped
```

- [ ] **Step 4: Stable slots**

Replace the whole `_tunnel_collect_applied` function, with its comment, with:

```bash
# _tunnel_collect_applied <prev> - print "idx tunnel" for every tunnel that gets a
# slot, in JSON order. A tunnel <prev> (the previous TUN_DIR_TABLES) records keeps
# its idx - its mark, preference and table - so a rebuild puts its chain in place
# over the rules it already has. A tunnel new to the layout takes the lowest idx
# that neither layout holds: a slot this apply frees still carries marks until the
# swap, and its ip rule goes only after it. warnings and skipped_unknown are
# tunnel_apply's locals (bash dynamic scope). One pass, so the failover MARK slots
# cannot drift from the rules.
_tunnel_collect_applied() {
    local prev="${1:-/dev/null}"
    local tunnel tunnel_type clients_type clients idx used tunnels
    tunnels=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r 'keys_unsorted[]')
    used=" $(awk '{ printf "%s ", $1 }' "$prev" 2>/dev/null) "
    while IFS= read -r tunnel; do
        [[ -n $tunnel ]] || continue
        if ! _tunnel_table_allowed "$tunnel"; then
            log -l WARN "Tunnel '$tunnel' is not a tunnel this platform knows; skipping"
            warnings=1
            skipped_unknown=1
            continue
        fi
        tunnel_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t] | type')
        if [[ $tunnel_type != "object" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid config (expected object, got $tunnel_type); skipping"
            warnings=1
            continue
        fi
        clients_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients | type')
        if [[ $clients_type != "array" ]] && [[ $clients_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid clients (expected array, got $clients_type); skipping"
            warnings=1
            continue
        fi
        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        if [[ -z $clients ]]; then
            log -l WARN "Tunnel '$tunnel' has no clients; skipping"
            warnings=1
            continue
        fi
        idx=$(awk -v id="$tunnel" '$2 == id { print $1; exit }' "$prev" 2>/dev/null)
        [[ $idx =~ ^[0-9]+$ ]] || idx=""
        if [[ -z $idx ]]; then
            idx=0
            while [[ $used == *" $idx "* ]]; do
                idx=$((idx + 1))
            done
        fi
        if [[ $((idx + 1)) -gt $_tunnel_mark_field_max ]]; then
            log -l WARN "Too many tunnels (max $_tunnel_mark_field_max); skipping '$tunnel'"
            warnings=1
            continue
        fi
        used+="$idx "
        printf '%s %s\n' "$idx" "$tunnel"
    done <<< "$tunnels"
}
```

- [ ] **Step 5: The chain's rules move into `_tunnel_build_chain`**

Replace the whole `_tunnel_emit_client` function, with its comment, with:

```bash
# One client's RETURN / offload / MARK in <chain>. Offload sits immediately
# before MARK with the same match so excluded destinations keep acceleration.
# warnings, changes, incomplete and offload_target are tunnel_apply's locals
# (bash dynamic scope); incomplete says a rule did not go in, so the rebuild is
# not recorded.
#
# Returns 0 when the client is marked, 1 when it is skipped as no IPv4 address
# or CIDR at all - nothing TPROXY can take either - 2 when it is skipped as
# outside RFC1918, and 3 when its MARK rule did not go in. The callers run it
# under "||", which turns errexit off here, so every rule is checked by hand:
# under errexit one refused rule used to end the whole apply half-way.
_tunnel_emit_client() {
    local chain="$1" client="$2" tunnel="$3" mark_hex="$4" excludes="$5"
    local client_ip="${client%%/*}"
    # is_lan_ip looks at the prefix only: 192.168.1.1000 passes it and then
    # makes "iptables -s" fail, 192.168.1.010 would be read as .8.
    if ! is_ipv4_net "$client"; then
        log -l WARN "Client '$client' is not an IPv4 address or CIDR; skipping"
        warnings=1
        return 1
    fi
    if ! is_lan_ip "$client_ip"; then
        log -l WARN "Client '$client' is not RFC1918; skipping"
        warnings=1
        return 2
    fi

    local excl excl_set
    while IFS= read -r excl; do
        [[ -n $excl ]] || continue
        excl_set=$(printf '%s' "$excl" | tr 'A-Z' 'a-z')
        if ! _ipset_exists "$excl_set"; then
            log -l WARN "Exclude ipset '$excl_set' not found; skipping exclusion"
            warnings=1
            continue
        fi
        if ! ensure_fw_rule -q mangle "$chain" \
            -s "$client" -m set --match-set "$excl_set" dst -j RETURN; then
            warnings=1
            incomplete=1
        fi
    done <<< "$excludes"

    if [[ -n $offload_target ]]; then
        if ! ensure_fw_rule -q mangle "$chain" \
            -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
            -j "$offload_target"; then
            warnings=1
            incomplete=1
        fi
    fi

    if ! ensure_fw_rule -q mangle "$chain" \
        -s "$client" -m mark --mark "0x0/$_tunnel_mark_mask_hex" \
        -j MARK --set-xmark "$mark_hex/$_tunnel_mark_mask_hex"; then
        log -l ERROR "Client '$client' is not marked for tunnel '$tunnel'; its traffic falls through to main"
        warnings=1
        incomplete=1
        return 3
    fi

    log "Added: client=$client tunnel=$tunnel mark=$mark_hex"
    changes=1
}
```

Delete the whole `_tunnel_sync_jumps` function together with its comment block (`# _tunnel_sync_jumps <lan_ifaces> <pos> - ...`); nothing calls it after this task.

Then add, right after the closing `}` of `_tunnel_jumps_ensure`:

```bash
# _tunnel_build_chain <chain> - fill a fresh chain with every client's rules
# The build_fn of swap_fw_chain. It reads the new layout from slots_tmp and sets
# warnings, incomplete, changes and fo_carried - tunnel_apply's locals (bash
# dynamic scope; swap_fw_chain's own locals carry a _sw_ prefix). The failover
# snapshot clients come first, under the failover tunnel's mark, so a covering
# earlier rule (often main) does not send them to the WAN; the other tunnels
# follow in JSON order. Returns 0: a refused rule costs its client only
# (_tunnel_emit_client says which), and the chain always goes in.
_tunnel_build_chain() {
    local chain="$1"
    local tunnel_idx tunnel mark_hex exclude_type clients excludes client
    local fo_idx fo_mark="" fo_on_tunnel="" fo_excl_type fo_excludes="" fo_client fo_rc
    local fo_have fo_still fo_skip fo_c

    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
        fo_idx=$(awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { print $1; exit }' "$slots_tmp")
        if [[ -n $fo_idx ]]; then
            fo_mark=$(_tunnel_mark_hex "$fo_idx")
            fo_on_tunnel=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].clients // [] | .[]')
        fi
    fi
    if [[ -n $fo_mark ]]; then
        fo_excl_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude | type')
        if [[ $fo_excl_type == array ]]; then
            fo_excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$XRAY_FAILOVER_TUNNEL" '.[$t].exclude // [] | .[]')
        fi
        for fo_client in $XRAY_FAILOVER_CLIENTS; do
            # DELETE /api/clients drops the address from the tunnel but leaves
            # xray.failover. An override for an IP no longer on this tunnel
            # would first-match it onto the old fallback.
            fo_still=0
            while IFS= read -r fo_have; do
                if [[ $fo_have == "$fo_client" ]]; then
                    fo_still=1
                    break
                fi
            done <<< "$fo_on_tunnel"
            [[ $fo_still -eq 1 ]] || continue
            fo_rc=0
            _tunnel_emit_client "$chain" "$fo_client" "$XRAY_FAILOVER_TUNNEL" "$fo_mark" "$fo_excludes" || fo_rc=$?
            # 1 is no address at all, which TPROXY cannot take either. 2 and 3
            # are clients TPROXY takes and TUN_DIR does not mark: dropped from
            # Xray on failover_ready, they would leave through the WAN.
            [[ $fo_rc -lt 2 ]] || fo_carried=0
        done
    fi

    while read -r tunnel_idx tunnel; do
        [[ -n $tunnel ]] || continue

        # Validate exclude is an array (if present)
        exclude_type=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude | type')
        if [[ $exclude_type != "array" ]] && [[ $exclude_type != "null" ]]; then
            log -l WARN "Tunnel '$tunnel' has invalid exclude (expected array, got $exclude_type); skipping exclusions"
            warnings=1
            exclude_type="null"  # Skip excludes but continue with clients
        fi

        clients=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].clients // [] | .[]')
        if [[ $exclude_type == "array" ]]; then
            excludes=$(printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -r --arg t "$tunnel" '.[$t].exclude // [] | .[]')
        else
            excludes=""
        fi
        mark_hex=$(_tunnel_mark_hex "$tunnel_idx")

        # Add rules for each client. Snapshot IPs were already emitted first.
        while IFS= read -r client; do
            [[ -n $client ]] || continue
            if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" && -n ${XRAY_FAILOVER_CLIENTS:-} ]]; then
                fo_skip=0
                for fo_c in $XRAY_FAILOVER_CLIENTS; do
                    if [[ $client == "$fo_c" ]]; then
                        fo_skip=1
                        break
                    fi
                done
                [[ $fo_skip -eq 0 ]] || continue
            fi
            # A client that is skipped or not marked has said so; the rest of
            # the tunnel, and every other tunnel, still goes in.
            _tunnel_emit_client "$chain" "$client" "$tunnel" "$mark_hex" "$excludes" || true
        done <<< "$clients"
    done < "$slots_tmp"
    return 0
}

# _tunnel_slot_release <idx> <tunnel> - drop the ip rule and the table of a slot
# the new layout no longer holds. Called after the swap, when no packet carries
# the slot's mark any more. Only this module's rule goes (_tunnel_rule_listed):
# another owner's rule on the preference stays, as the up-to-date path leaves it.
_tunnel_slot_release() {
    local idx="$1" tunnel="$2" pref mark_hex table
    pref=$((TUN_DIR_PREF_BASE + idx))
    mark_hex=$(_tunnel_mark_hex "$idx")
    if table="$(platform_tunnel_table "$tunnel" "$idx")" && _tunnel_rule_listed "$pref" "$mark_hex" "$table"; then
        if ! ip rule del pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
            log -l WARN "Tunnel '$tunnel': the ip rule at pref $pref did not go; it routes a mark nothing sets any more"
        fi
    fi
    platform_tunnel_table_release "$tunnel" "$idx" || true
    log "Tunnel '$tunnel': released slot $idx"
}
```

- [ ] **Step 6: `tunnel_stop` removes the shadow too**

In `tunnel_stop`, replace

```bash
    # Remove PREROUTING jump
    purge_fw_rules -q "mangle PREROUTING" "-j ${TUN_DIR_CHAIN}\$"

    # Delete chain if exists
    if fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        delete_fw_chain -q mangle "$TUN_DIR_CHAIN"
        log "Removed chain: $TUN_DIR_CHAIN"
    fi
```

with

```bash
    # Remove the PREROUTING jumps, those to a shadow an interrupted swap left included
    purge_fw_rules -q "mangle PREROUTING" "-j ${TUN_DIR_CHAIN}(_NEW)?\$"

    # Delete chain if exists
    if fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        delete_fw_chain -q mangle "$TUN_DIR_CHAIN"
        log "Removed chain: $TUN_DIR_CHAIN"
    fi
    if fw_chain_exists mangle "${TUN_DIR_CHAIN}_NEW"; then
        delete_fw_chain -q mangle "${TUN_DIR_CHAIN}_NEW" || true
    fi
```

- [ ] **Step 7: `tunnel_apply` rebuilds in place**

Replace the whole `tunnel_apply` function, with its comment block, with:

```bash
# -------------------------------------------------------------------------------------------------
# tunnel_apply - apply rules from config (idempotent)
# -------------------------------------------------------------------------------------------------
# Applies TUN_DIR_TUNNELS_JSON configuration. Single chain, exclusion-based routing.
#
# A rebuild happens in place, make before break. The routes and ip rules of the new layout go in
# first, the chain is built beside the live one and swapped in (swap_fw_chain), and only then do
# the slots the new layout dropped lose their rules and tables. A tunnel keeps its slot - its
# mark, preference and table - for as long as it has clients (_tunnel_collect_applied), so a swap
# changes the chain alone. The rebuild used to start with tunnel_stop: every Tunnel Director
# client left through the WAN until the last rule was back, on every change of any client.
# TUN_DIR_FORCE_REBUILD=1 rebuilds even when the state reads up to date ("restart").
# -------------------------------------------------------------------------------------------------
tunnel_apply() {
    _tunnel_init

    local changes=0
    local warnings=0
    local skipped_unknown=0
    local incomplete=0

    # Check if tunnels config is empty. A leftover chain/jump/ip rule would
    # still force previously matched clients into the tunnel, so tear down.
    if [[ -z $TUN_DIR_TUNNELS_JSON ]] || [[ $TUN_DIR_TUNNELS_JSON == "{}" ]]; then
        log "No tunnels configured"
        # Only when there is something to tear down: a state file, or a chain
        # that outlived its state files in /tmp. Every Xray-only install (the
        # template's default) takes this branch on every hook and cron run,
        # and an unconditional tunnel_stop is 255 `ip rule del` spawns, a
        # PREROUTING purge and two syslog lines each time.
        if [[ -f $TUN_DIR_HASH || -f $TUN_DIR_TABLES ]] || fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
            tunnel_stop
        fi
        return 0
    fi

    # Validate JSON before modifying firewall state
    if ! printf '%s\n' "$TUN_DIR_TUNNELS_JSON" | jq -e 'type == "object"' >/dev/null 2>&1; then
        log -l ERROR "Invalid tunnels JSON configuration"
        return 1
    fi

    # Compute config hash for change detection. Failover snapshot clients are
    # extra MARK rules, not a key reorder, so they belong in the hash. With no
    # failover, hash only the tunnels JSON so an upgrade does not rebuild TUN_DIR.
    local new_hash old_hash empty_hash
    if [[ -n ${XRAY_FAILOVER_TUNNEL:-} ]]; then
        new_hash=$(printf '%s\n%s\n%s' "$TUN_DIR_TUNNELS_JSON" "$XRAY_FAILOVER_TUNNEL" "${XRAY_FAILOVER_CLIENTS:-}" | compute_hash)
    else
        new_hash=$(printf '%s' "$TUN_DIR_TUNNELS_JSON" | compute_hash)
    fi
    empty_hash=$(printf '' | compute_hash)
    old_hash=$(cat "$TUN_DIR_HASH" 2>/dev/null || printf '%s' "$empty_hash")

    # Check if rebuild needed
    local rebuild=0
    if [[ ${TUN_DIR_FORCE_REBUILD:-0} == 1 ]]; then
        rebuild=1
    elif [[ $new_hash != "$old_hash" ]]; then
        rebuild=1
    elif ! fw_chain_exists mangle "$TUN_DIR_CHAIN"; then
        rebuild=1
    elif [[ ! -f $TUN_DIR_TABLES ]]; then
        rebuild=1
    elif ! _tunnel_marks_present; then
        rebuild=1
    fi

    if [[ $rebuild -eq 0 ]]; then
        # Always re-install recorded routes first. Gating that on the failover
        # row being in TUN_DIR_TABLES skipped Keenetic route repair for every
        # other tunnel after an interface flap.
        local route_rc=0 jumps_rc=0
        _tunnel_ensure_routes || route_rc=$?
        if ! _tunnel_jumps_ensure; then
            log -l ERROR "Tunnel Director: a PREROUTING jump to $TUN_DIR_CHAIN is missing and did not go back in"
            jumps_rc=1
        fi
        if _tunnel_failover_needed; then
            if ! awk -v id="$XRAY_FAILOVER_TUNNEL" '$2 == id { found = 1 } END { exit !found }' "$TUN_DIR_TABLES" \
                || [[ $route_rc -ne 0 ]] \
                || [[ $jumps_rc -ne 0 ]] \
                || ! _tunnel_failover_carried \
                || ! _tunnel_failover_rule_present; then
                rm -f "$TUN_DIR_FAILOVER_READY"
                log -l WARN "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
            else
                printf '%s\n' "$XRAY_FAILOVER_TUNNEL" > "$TUN_DIR_FAILOVER_READY"
            fi
        else
            rm -f "$TUN_DIR_FAILOVER_READY"
        fi
        log "Rules are applied and up-to-date"
        return 0
    fi

    # The PREROUTING jumps below are what make the chain matter, so ask for the
    # LAN interfaces before touching any firewall state. A platform that cannot
    # name them would otherwise leave a fully populated chain with nothing
    # jumping to it - every client routed direct instead of through its tunnel -
    # and tunnel_apply would still return 0. Asking first leaves the rules that
    # are already installed alone.
    local lan_ifaces
    lan_ifaces="$(platform_lan_ifaces)" || lan_ifaces=""
    if [[ -z $lan_ifaces ]]; then
        log -l ERROR "Cannot determine the LAN interfaces; Tunnel Director rules not applied"
        return 1
    fi

    # Where the jumps go is asked first for the same reason: an rc 1 here used to
    # trip errexit mid-rebuild and end the whole CLI run without a log line.
    # swap_fw_chain asks again for the insert, on the listing as it is then.
    if ! _tunnel_prerouting_pos >/dev/null; then
        log -l ERROR "Cannot determine the PREROUTING insert position; Tunnel Director rules not applied"
        return 1
    fi

    if [[ ${TUN_DIR_FORCE_REBUILD:-0} == 1 ]]; then
        log "Rebuilding Tunnel Director in place..."
    elif [[ $new_hash != "$old_hash" ]]; then
        log "Configuration changed; rebuilding in place..."
    else
        # A rebuild also fires with the hash unchanged - a missing chain, a
        # missing TUN_DIR_TABLES, a MARK rule gone - and that is no change.
        log "Applied rules are incomplete; rebuilding..."
    fi
    # Written back only by a rebuild that completes. One that dies part-way
    # leaves the next apply a rebuild as well, and that one finishes the swap
    # the dead one left and releases the slots it left on record.
    rm -f "$TUN_DIR_HASH"
    changes=1

    # A platform whose firmware accelerates established forwarded flows past
    # mangle names the target that opts a flow out of it (KeeneticOS: PPE);
    # one that needs none prints nothing and returns 1. Asked once rather than
    # per client: one answer keeps every client's block consistent, and the
    # platform is not made to answer the same question N times.
    local offload_target
    offload_target="$(platform_tunnel_offload_target)" || offload_target=""

    local fo_applied=0 fo_route_ok=1 fo_rule_ok=1 fo_carried=1
    local prev_tmp slots_tmp
    prev_tmp="$(tmp_file)"
    slots_tmp="$(tmp_file)"
    if [[ -f $TUN_DIR_TABLES ]]; then
        cp -f "$TUN_DIR_TABLES" "$prev_tmp"
    else
        : > "$prev_tmp"
    fi
    _tunnel_collect_applied "$prev_tmp" > "$slots_tmp"

    # 1. Routing, before any packet carries a new mark. TUN_DIR_TABLES holds
    # both layouts until the release below: an apply that dies in between
    # leaves every slot in use on record, and the next one releases the extras.
    mkdir -p "$(dirname "$TUN_DIR_TABLES")"
    awk '!seen[$0]++' "$prev_tmp" "$slots_tmp" > "$TUN_DIR_TABLES"

    local tunnel_idx tunnel table pref mark_hex route_ok rule_ok
    while read -r tunnel_idx tunnel; do
        [[ -n $tunnel ]] || continue
        route_ok=1
        rule_ok=1
        if grep -qxF "$tunnel_idx $tunnel" "$prev_tmp"; then
            # A kept slot is carrying its clients now: its route and its rule
            # are ensured, never released - an empty table is a window in which
            # they fall through to main.
            if ! platform_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
                warnings=1
                route_ok=0
            fi
            if ! _tunnel_rule_ensure "$tunnel_idx" "$tunnel"; then
                warnings=1
                rule_ok=0
            fi
        else
            # A new slot starts from an empty table. An apply that died after
            # ensuring a route but before recording its slot left the route
            # with no record, and a route_ensure that fails now (interface
            # down) would leave the previous owner's route behind the ip rule
            # installed below. No-op on Merlin.
            platform_tunnel_table_release "$tunnel" "$tunnel_idx" || true
            if ! platform_tunnel_route_ensure "$tunnel" "$tunnel_idx" "$(_tunnel_gateway "$tunnel")"; then
                log -l WARN "Tunnel '$tunnel': route not installed (interface down or not mapped?); traffic falls through to main"
                warnings=1
                route_ok=0
            fi
            # The ip rule goes in even when the route does not: a lookup in an
            # empty table falls through to main.
            table="$(platform_tunnel_table "$tunnel" "$tunnel_idx")"
            pref=$((TUN_DIR_PREF_BASE + tunnel_idx))
            mark_hex=$(_tunnel_mark_hex "$tunnel_idx")
            ip rule del pref "$pref" 2>/dev/null || true
            if ! ip rule add pref "$pref" fwmark "$mark_hex/$_tunnel_mark_mask_hex" lookup "$table" 2>/dev/null; then
                log -l ERROR "Failed to add ip rule: pref=$pref fwmark=$mark_hex lookup=$table"
                warnings=1
                rule_ok=0
            fi
        fi
        if [[ $tunnel == "${XRAY_FAILOVER_TUNNEL:-}" ]]; then
            fo_applied=1
            [[ $route_ok -eq 1 ]] || fo_route_ok=0
            [[ $rule_ok -eq 1 ]] || fo_rule_ok=0
        fi
    done < "$slots_tmp"

    # 2. The chain, built beside the live one and swapped in, one jump per LAN
    # interface. The mark test on the jump makes the first match win: a packet an
    # earlier rule marked skips this chain.
    local lan_if swap_rc=0 jumps_ok=1
    local -a jump_matches=()
    while IFS= read -r lan_if; do
        [[ -n $lan_if ]] || continue
        jump_matches+=("-i $lan_if -m mark --mark 0x0/$_tunnel_mark_mask_hex")
    done <<< "$lan_ifaces"
    swap_fw_chain mangle "$TUN_DIR_CHAIN" _tunnel_build_chain _tunnel_prerouting_pos "${jump_matches[@]}" || swap_rc=$?
    if [[ $swap_rc -ge 2 ]]; then
        log -l ERROR "Tunnel Director: the rebuilt chain did not take over every LAN interface; the next apply finishes it"
        warnings=1
        jumps_ok=0
    fi

    # 3. Release the slots the new layout dropped - only once the new chain
    # carries every interface, since until then the old chain marks with them.
    if [[ $jumps_ok -eq 1 ]]; then
        local old_idx old_tunnel
        while read -r old_idx old_tunnel; do
            [[ -n $old_tunnel ]] || continue
            grep -qxF "$old_idx $old_tunnel" "$slots_tmp" && continue
            _tunnel_slot_release "$old_idx" "$old_tunnel"
        done < "$prev_tmp"
        cp -f "$slots_tmp" "$TUN_DIR_TABLES"
    fi

    # 4. The hash is what makes the next apply take the up-to-date branch, so it
    # is recorded only when every configured tunnel was applied, every client
    # rule went in and the chain took over. A tunnel the platform does not list
    # is not a configuration state: on Keenetic platform_tunnels answers only
    # "main" while RCI does not reply, and the netfilter.d hook fires exactly
    # during an NDM rebuild, when it may well not. Recording the hash there would
    # send every later apply down the up-to-date branch and never restore the
    # routing until the next rebuild - a silent fail-open. On Merlin the only
    # case is a typo in the tunnel id, which then warns on every apply instead
    # of once. A refused chain rule is the same: the up-to-date branch looks for
    # the MARK rules alone, so a refused exclusion or offload rule would stay
    # missing, and only a rebuild retries it.
    if [[ $skipped_unknown -eq 0 && $incomplete -eq 0 && $jumps_ok -eq 1 ]]; then
        printf '%s\n' "$new_hash" > "$TUN_DIR_HASH"
    elif [[ $skipped_unknown -ne 0 ]]; then
        log -l WARN "Tunnel Director: a configured tunnel is unknown to the platform (RCI down, or a typo in the id); this apply is not recorded as up-to-date and the next apply retries"
    elif [[ $jumps_ok -eq 0 ]]; then
        log -l WARN "Tunnel Director: the rebuild did not finish; this apply is not recorded as up-to-date and the next apply finishes it"
    else
        log -l WARN "Tunnel Director: a client rule did not go in; this apply is not recorded as up-to-date and the next apply rebuilds"
    fi

    # A failover tunnel that is not ready does not fail the apply: S99 start,
    # hooks and Web UI Apply would then skip cron and report failure while the
    # fallback interface is still coming up. The watch reads
    # TUN_DIR_FAILOVER_READY instead of the apply exit status, and drops Xray
    # membership on it: every failover client has to be marked, and the jump
    # that sends LAN traffic to those marks has to be there.
    if _tunnel_failover_needed && [[ $fo_applied -eq 1 && $fo_route_ok -eq 1 && $fo_rule_ok -eq 1 \
        && $fo_carried -eq 1 && $jumps_ok -eq 1 ]]; then
        printf '%s\n' "$XRAY_FAILOVER_TUNNEL" > "$TUN_DIR_FAILOVER_READY"
    else
        rm -f "$TUN_DIR_FAILOVER_READY"
        if _tunnel_failover_needed; then
            log -l WARN "Failover tunnel '${XRAY_FAILOVER_TUNNEL}' is not carrying traffic; Xray membership stays"
        fi
    fi

    if [[ $changes -eq 0 ]]; then
        log "No changes applied"
    elif [[ $warnings -eq 0 ]]; then
        log "All rules applied successfully"
    else
        log -l WARN "Completed with warnings"
    fi

    return 0
}
```

- [ ] **Step 8: Update the tests that name the live chain in a rebuild**

A rebuild now appends its rules to `TUN_DIR_NEW` and inserts jumps to `TUN_DIR_NEW`; the stateless mock logs those names. In `router/test/unit/tunnel.bats`:

- `tunnel_apply: inserts the PREROUTING jump at platform_prerouting_base_pos` — the grep becomes:

  ```bash
  grep -q -- '-t mangle -I PREROUTING 4 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: the jump goes behind the XRAY_TPROXY jumps already in PREROUTING` — the grep becomes:

  ```bash
  grep -q -- '-I PREROUTING 2 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: one PREROUTING jump per platform LAN interface` — the two greps become:

  ```bash
  grep -q -- '-I PREROUTING 4 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  grep -q -- '-I PREROUTING 5 -i br1 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: the platform's offload target goes immediately before MARK, same match` — the body after `assert_success` becomes:

  ```bash
  run grep -- '-A TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  assert_line --index 0 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN'
  assert_line --index 1 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j PPE'
  assert_line --index 2 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
  ```

- `tunnel_apply: no offload rule when the platform names no target` — the body after `assert_success` becomes:

  ```bash
  run grep -- '-A TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  assert_line --index 0 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m set --match-set ru dst -j RETURN'
  assert_line --index 1 'iptables -t mangle -A TUN_DIR_NEW -s 192.168.50.0/24 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000'
  assert_equal "${#lines[@]}" 2
  ```

- `tunnel_apply: a client address iptables would refuse is skipped, and the rest applies` — the three greps after `tunnel_apply` become:

  ```bash
  grep -q -- '-A TUN_DIR_NEW -s 192.168.1.5 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000' /tmp/bats_iptables_calls.log
  grep -q -- '-I PREROUTING 1 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  refute grep -q -- '-A TUN_DIR_NEW -s 192.168.1.1000' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: a MARK rule that does not go in costs that client only` — the override's pattern and the two greps become:

  ```bash
      iptables() {
          if [[ $* == *"-A TUN_DIR_NEW -s 192.168.1.5 "*"-j MARK"* ]]; then
              echo "iptables $*" >> /tmp/bats_iptables_calls.log
              return 4
          fi
          command iptables "$@"
      }
  ```

  ```bash
  grep -q -- '-A TUN_DIR_NEW -s 192.168.1.6 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000' /tmp/bats_iptables_calls.log
  grep -q -- '-I PREROUTING 1 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR_NEW' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: no failover_ready when a failover client's MARK rule did not go in` — the override's pattern becomes `*"-A TUN_DIR_NEW -s 192.168.1.8 "*"-j MARK"*`.

- `tunnel_apply: a failover client whose MARK rule is gone is marked again before failover_ready` — the grep becomes:

  ```bash
  grep -q -- '-A TUN_DIR_NEW -s 192.168.1.8 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x10000/0xff0000' /tmp/bats_iptables_calls.log
  ```

- `tunnel_apply: a chain that lost a client's MARK rule is rebuilt` — the grep becomes:

  ```bash
  grep -q -- '-A TUN_DIR_NEW -s 192.168.1.6 -m mark --mark 0x0/0xff0000 -j MARK --set-xmark 0x20000/0xff0000' /tmp/bats_iptables_calls.log
  ```

- Replace the whole test `tunnel_apply: the up-to-date path puts a missing PREROUTING jump back`, comment included, with:

  ```bash
  # A jump that goes missing after a recorded rebuild is put back by the
  # up-to-date path, which leaves a jump that is there where it is. (A rebuild
  # whose own jumps did not go in records no hash: the next apply rebuilds.)
  @test "tunnel_apply: the up-to-date path puts a missing PREROUTING jump back" {
      load_tunnel_module_with '{"ovpnc2":{"clients":["192.168.1.8"]}}'
      export XRAY_FAILOVER_TUNNEL=ovpnc2 XRAY_FAILOVER_CLIENTS=192.168.1.8
      platform_tunnel_route_ensure() { return 0; }
      run tunnel_apply
      assert_success
      [ -f "$TUN_DIR_HASH" ]

      # The stateless mock lists no jump: the one the rebuild put in is gone.
      fw_chain_exists() { return 0; }
      marks_in_place
      : > /tmp/bats_iptables_calls.log
      run tunnel_apply
      assert_success
      assert_output --partial "up-to-date"
      grep -q -- '-I PREROUTING 1 -i br0 -m mark --mark 0x0/0xff0000 -j TUN_DIR$' /tmp/bats_iptables_calls.log
      [ -f "$TUN_DIR_FAILOVER_READY" ]
  }
  ```

- In `tunnel_apply: a PREROUTING jump that did not go in withholds failover_ready`, add after the existing assertions:

  ```bash
  [ ! -f "$TUN_DIR_HASH" ]
  ```

- [ ] **Step 9: Run the tests to see them pass**

Run: `bats router/test/unit/tunnel.bats router/test/unit/tproxy.bats router/test/firewall.bats router/test/integration/vpn_director.bats`
Expected: PASS, every test.

- [ ] **Step 10: shellcheck**

Run: `shellcheck router/opt/vpn-director/lib/tunnel.sh`
Expected: nothing the file did not report on `master`.

- [ ] **Step 11: Commit**

```bash
git add router/opt/vpn-director/lib/tunnel.sh router/test/unit/tunnel.bats
git commit -m "feat(shell): Tunnel Director rebuilds in place and keeps each tunnel's slot"
```

---

### Task 4: `restart` rebuilds in place and stops nothing

**Files:**
- Modify: `router/opt/vpn-director/vpn-director.sh` (the usage header, `show_help`, `cmd_restart`)
- Modify: `server/internal/service/vpndirector.go` (doc comments only)
- Test: `router/test/integration/vpn_director.bats`

**Interfaces:**
- Consumes: `TUN_DIR_FORCE_REBUILD` (Task 3), `tproxy_prune` (Task 2), `run_stubbed_cli` as Task 2 left it.
- Produces: `restart` = process restart + `TUN_DIR_FORCE_REBUILD=1 cmd_apply`; `restart tunnel` = `TUN_DIR_FORCE_REBUILD=1 COMPONENT=tunnel cmd_apply`; `restart xray` = process restart + `COMPONENT=xray cmd_apply`; `restart xray-process` unchanged.

- [ ] **Step 1: Write the failing tests**

In `router/test/integration/vpn_director.bats`, replace the test `restart --unless-stopped re-applies a running router`, comment included, with:

```bash
# A restart stops nothing, so it leaves no marker of its own: the apply it runs
# removes the one a stop left, as an apply does.
@test "vpn-director: restart --unless-stopped re-applies a running router" {
    export VPD_STOPPED_FILE="$BATS_TEST_TMPDIR/stopped"
    run_stubbed_cli --unless-stopped restart
    assert_success
    grep -qx tproxy_apply "$BATS_TEST_TMPDIR/calls"
    grep -qx "tunnel_apply forced" "$BATS_TEST_TMPDIR/calls"
    [ ! -e "$VPD_STOPPED_FILE" ]
}
```

and append:

```bash
# A restart stops nothing: every apply swaps its chains and sets in whole, where
# a stop took the routing away until the apply put it back.
@test "vpn-director: restart rebuilds in place and stops nothing" {
    run_stubbed_cli restart
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_restart_process\ntproxy_apply\ntunnel_apply forced\ntproxy_prune'
}

# A server switch (the Web UI, /xray, the wizard) runs "restart xray". The TPROXY
# rules stay in place while the process restarts: its clients wait for it
# instead of leaving through the WAN.
@test "vpn-director: restart xray restarts the process and re-applies Xray without a stop" {
    run_stubbed_cli restart xray
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output $'tproxy_restart_process\ntproxy_apply\ntproxy_prune'
}

@test "vpn-director: restart tunnel rebuilds Tunnel Director in place" {
    run_stubbed_cli restart tunnel
    assert_success
    run cat "$BATS_TEST_TMPDIR/calls"
    assert_output 'tunnel_apply forced'
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `bats router/test/integration/vpn_director.bats`
Expected: the four tests FAIL — the calls show `tproxy_stop` / `tunnel_stop`, and `tunnel_apply` without ` forced`.

- [ ] **Step 3: `cmd_restart`, the usage header and the help text**

In `router/opt/vpn-director/vpn-director.sh`, in the usage block at the top, replace

```bash
#   vpn-director restart [tunnel|xray]       - Restart components
```

with

```bash
#   vpn-director restart [tunnel|xray]       - Rebuild in place, nothing stopped (all and xray restart Xray)
```

In `show_help`, replace the lines

```
  apply [tunnel|xray]         Apply configuration
  stop [tunnel|xray]          Stop components
  restart [tunnel|xray]       Restart (stop + apply)
```

with

```
  apply [tunnel|xray]         Apply configuration (moving a client between them takes a full apply)
  stop [tunnel|xray]          Stop components
  restart [tunnel|xray]       Rebuild in place, nothing stopped (all and xray restart the Xray process)
```

Replace the whole `cmd_restart` function with:

```bash
cmd_restart() {
    # Lock first for the same reason: cmd_apply below would find the modules
    # already loaded and reuse the config read before the wait.
    _load_common
    acquire_lock "vpn-director"
    _load_modules
    if _skip_when_stopped restart; then
        return 0
    fi
    # Checked once, above, under the lock this restart holds throughout.
    UNLESS_STOPPED=0
    # Nothing is stopped first. An apply replaces its chains and sets whole, with
    # no moment without them (swap_fw_chain, ipset swap); a stop took the routing
    # away until the apply put it back, and every client left through the WAN in
    # between. TUN_DIR_FORCE_REBUILD rebuilds Tunnel Director even when its state
    # reads up to date: what a restart is asked for.
    case "$COMPONENT" in
        ""|all)
            tproxy_restart_process
            TUN_DIR_FORCE_REBUILD=1 cmd_apply
            ;;
        tunnel)
            TUN_DIR_FORCE_REBUILD=1 COMPONENT=tunnel cmd_apply
            ;;
        xray|tproxy)
            # A server switch (the Web UI, /xray, the wizard): the rules stay in
            # place while the process restarts, so its clients wait for it
            # instead of leaving through the WAN, and the apply swaps in
            # TPROXY_BYPASS, whose xray.servers the switch may have recomputed.
            tproxy_restart_process
            COMPONENT=xray cmd_apply
            ;;
        xray-process)
            # config.json changed and nothing else: the Telegram bot's
            # subscription watch writes one per server it tries.
            tproxy_restart_process
            ;;
        *)
            echo "Unknown component: $COMPONENT" >&2
            exit 1
            ;;
    esac
}
```

- [ ] **Step 4: The Go doc comments**

In `server/internal/service/vpndirector.go`, replace the comment above `RestartXray` with:

```go
// RestartXray restarts the Xray process and applies the TPROXY rules again in
// place: nothing is stopped, so the Xray clients wait for the process instead
// of leaving through the WAN. A server switch calls it; the switch may also
// have recomputed xray.servers, which the apply swaps into TPROXY_BYPASS.
```

and the comment above `RestartXrayProcessUnlessStopped` with:

```go
// RestartXrayProcessUnlessStopped restarts the Xray process for the same
// caller, skipped the same way, and leaves the TPROXY rules alone. The walk
// writes one config.json per server it tries and changes nothing else; "restart
// xray" would apply the TPROXY rules again after each one for nothing.
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `bats router/test/integration/vpn_director.bats`
Expected: PASS, every test.

Run (through `claude-forge:build-runner`): `cd server && go vet ./internal/service/ && gofmt -l internal/service/`
Expected: no output.

- [ ] **Step 6: shellcheck**

Run: `shellcheck router/opt/vpn-director/vpn-director.sh`
Expected: nothing the file did not report on `master`.

- [ ] **Step 7: Commit**

```bash
git add router/opt/vpn-director/vpn-director.sh router/test/integration/vpn_director.bats \
        server/internal/service/vpndirector.go
git commit -m "feat(shell): restart rebuilds in place and stops nothing"
```

---

### Task 5: `vpnconfig.MoveClient`

**Files:**
- Create: `server/internal/vpnconfig/clients.go`
- Test: `server/internal/vpnconfig/clients_test.go`

**Interfaces:**
- Consumes: `CollectClients`, `RepointPausedClients` (`vpnconfig.go`); `DetachFailoverClient`, `withoutAddr`, `addrKey`, `contains` (`failover.go`).
- Produces:
  - `type MoveResult int` with `ClientMoved`, `ClientAlreadyThere`, `ClientNotFound`.
  - `func ClientRoutes(cfg *VPNDirectorConfig, addr string) []string`
  - `func MoveClient(cfg *VPNDirectorConfig, addr, route string) MoveResult`

- [ ] **Step 1: Write the failing tests**

Create `server/internal/vpnconfig/clients_test.go`:

```go
package vpnconfig

import (
	"reflect"
	"strings"
	"testing"
)

// movable is 192.168.50.10 on Xray, which excludes ru, and 192.168.50.30 on wgc1.
func movable() *VPNDirectorConfig {
	return &VPNDirectorConfig{
		Xray: XrayConfig{Clients: []string{"192.168.50.10"}, ExcludeSets: []string{"ru"}},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
		}},
	}
}

func TestClientRoutes(t *testing.T) {
	cfg := movable()
	// A staged failover: the address sits on Xray and on the fallback tunnel.
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.30", "192.168.50.10/32"}}
	if got := ClientRoutes(cfg, "192.168.50.10"); !reflect.DeepEqual(got, []string{"xray", "wgc1"}) {
		t.Errorf("ClientRoutes = %v, want [xray wgc1]", got)
	}
	if got := ClientRoutes(cfg, "192.168.50.99"); len(got) != 0 {
		t.Errorf("ClientRoutes of an unknown address = %v", got)
	}
}

func TestMoveClient_XrayToTunnel(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
}

func TestMoveClient_TunnelToXray(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.30", "xray"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if strings.Join(cfg.Xray.Clients, ",") != "192.168.50.10,192.168.50.30" {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	// A tunnel whose last client leaves stays, empty, as after a delete.
	tun, ok := cfg.TunnelDirector.Tunnels["wgc1"]
	if !ok || len(tun.Clients) != 0 || strings.Join(tun.Exclude, ",") != "ru" {
		t.Errorf("wgc1 = %+v, %v", tun, ok)
	}
}

func TestMoveClient_TakesEverySpellingOffEveryRoute(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.10", "192.168.50.30"}}
	if got := MoveClient(cfg, "192.168.50.10", "ovpnc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 || strings.Join(cfg.TunnelDirector.Tunnels["wgc1"].Clients, ",") != "192.168.50.30" {
		t.Errorf("xray %v, wgc1 %v: a spelling is left behind", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["wgc1"].Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["ovpnc1"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("ovpnc1 = %v", got)
	}
}

// An empty exclude would carry the client's local-country traffic through the
// tunnel too, so a new tunnel starts from Xray's exclusions - a copy of them.
func TestMoveClient_NewTunnelInheritsTheXrayExclusions(t *testing.T) {
	cfg := movable()
	MoveClient(cfg, "192.168.50.10", "OpenVPN0")
	tun := cfg.TunnelDirector.Tunnels["OpenVPN0"]
	if strings.Join(tun.Clients, ",") != "192.168.50.10" || strings.Join(tun.Exclude, ",") != "ru" {
		t.Fatalf("OpenVPN0 = %+v", tun)
	}
	cfg.Xray.ExcludeSets[0] = "de"
	if cfg.TunnelDirector.Tunnels["OpenVPN0"].Exclude[0] != "ru" {
		t.Error("the tunnel shares its exclude slice with xray.exclude_sets")
	}
}

// The shell subtracts paused_clients by exact string: the paused entry follows
// the address to its new spelling, or the client resumes on its own.
func TestMoveClient_PausedClientMovesPaused(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10/32"}
	MoveClient(cfg, "192.168.50.10", "wgc1")
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.10"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
}

// Review Focus 5: a paused /32 on the fallback tunnel of a committed failover.
func TestMoveClient_PausedSlash32DuringAFailover(t *testing.T) {
	cfg := &VPNDirectorConfig{
		PausedClients: []string{"192.168.50.8/32"},
		Xray: XrayConfig{
			ExcludeSets: []string{"ru"},
			Failover: &XrayFailover{
				Tunnel: "wgc1", Clients: []string{"192.168.50.8/32"}, Added: []string{"192.168.50.8/32"}, Committed: true,
			},
		},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.3", "192.168.50.8/32"}, Exclude: []string{"ru"}},
		}},
	}
	if got := MoveClient(cfg, "192.168.50.8", "ovpnc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if got := cfg.TunnelDirector.Tunnels["ovpnc1"].Clients; strings.Join(got, ",") != "192.168.50.8" {
		t.Errorf("ovpnc1 = %v", got)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.3" {
		t.Errorf("wgc1 = %v", got)
	}
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.8"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
	if fo := cfg.Xray.Failover; len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Errorf("failover %+v: the restore would take the address back to Xray", fo)
	}
}

func TestMoveClient_AlreadyThereChangesNothing(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.10", "xray"); got != ClientAlreadyThere {
		t.Fatalf("MoveClient = %v, want ClientAlreadyThere", got)
	}
	if !reflect.DeepEqual(cfg, movable()) {
		t.Errorf("cfg changed: %+v", cfg)
	}
}

func TestMoveClient_NotFoundChangesNothing(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.99", "wgc1"); got != ClientNotFound {
		t.Fatalf("MoveClient = %v, want ClientNotFound", got)
	}
	if !reflect.DeepEqual(cfg, movable()) {
		t.Errorf("cfg changed: %+v", cfg)
	}
}

// During a staged failover the address sits on Xray and on the fallback tunnel.
// A move to that tunnel is a real move: it leaves the address there once.
func TestMoveClient_StagedFailoverEndsOnOneRoute(t *testing.T) {
	cfg := movable()
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.10", "192.168.50.30"}}
	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/vpnconfig/ -run 'TestClientRoutes|TestMoveClient' -count=1`
Expected: FAIL — `undefined: ClientRoutes`, `undefined: MoveClient`.

- [ ] **Step 3: Write `clients.go`**

Create `server/internal/vpnconfig/clients.go`:

```go
package vpnconfig

// MoveResult says what MoveClient did.
type MoveResult int

const (
	// ClientMoved means the address now sits on the target route alone.
	ClientMoved MoveResult = iota
	// ClientAlreadyThere means the target route was already the only route
	// holding the address; nothing changed.
	ClientAlreadyThere
	// ClientNotFound means no route holds the address; nothing changed.
	ClientNotFound
)

// ClientRoutes lists the routes that hold addr, in any stored spelling, in the
// order CollectClients reports them: "xray" first, then the tunnels by name.
// An address normally sits on one route; during a staged failover it sits on
// Xray and on the fallback tunnel at once.
func ClientRoutes(cfg *VPNDirectorConfig, addr string) []string {
	key := addrKey(addr)
	var routes []string
	for _, c := range CollectClients(cfg) {
		if addrKey(c.IP) == key && !contains(routes, c.Route) {
			routes = append(routes, c.Route)
		}
	}
	return routes
}

// MoveClient puts addr on route - "xray" or a tunnel id the caller has
// checked - and takes it off every other route, in one change of cfg. The
// caller writes the change under the config lock and applies once, and that
// apply moves the client make-before-break: lib/tproxy.sh keeps a client that
// leaves Xray intercepted until lib/tunnel.sh has taken it, so none of its
// packets leaves through the WAN in between. A delete and an add left it there
// for as long as the user took between the two.
//
// The address goes on in its normalized spelling. A tunnel new to the config
// inherits xray.exclude_sets, as an added client's does: an empty exclude
// would carry the client's local-country traffic through the tunnel too.
// paused_clients follows the new spelling, since the shell subtracts it by
// exact string: a paused client moves paused. The address leaves the failover
// record, so a restore cannot undo the user's choice; one moved to Xray while
// Xray is down joins the failover afresh, as an added one does. A tunnel whose
// last client leaves stays in the config with an empty list, as after a
// delete. Only the routes that hold the address are rewritten.
func MoveClient(cfg *VPNDirectorConfig, addr, route string) MoveResult {
	on := ClientRoutes(cfg, addr)
	switch {
	case len(on) == 0:
		return ClientNotFound
	case len(on) == 1 && on[0] == route:
		return ClientAlreadyThere
	}

	for _, r := range on {
		if r == "xray" {
			cfg.Xray.Clients = withoutAddr(cfg.Xray.Clients, addr)
			continue
		}
		t := cfg.TunnelDirector.Tunnels[r]
		t.Clients = withoutAddr(t.Clients, addr)
		cfg.TunnelDirector.Tunnels[r] = t
	}

	key := addrKey(addr)
	if route == "xray" {
		cfg.Xray.Clients = append(cfg.Xray.Clients, key)
	} else {
		if cfg.TunnelDirector.Tunnels == nil {
			cfg.TunnelDirector.Tunnels = make(map[string]TunnelConfig)
		}
		t, ok := cfg.TunnelDirector.Tunnels[route]
		if !ok {
			t = TunnelConfig{Clients: []string{}, Exclude: append([]string{}, cfg.Xray.ExcludeSets...)}
		}
		t.Clients = append(t.Clients, key)
		cfg.TunnelDirector.Tunnels[route] = t
	}
	cfg.PausedClients = RepointPausedClients(cfg.PausedClients, CollectClients(cfg))
	DetachFailoverClient(cfg, addr)
	return ClientMoved
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/vpnconfig/ -count=1 && go vet ./internal/vpnconfig/ && gofmt -l internal/vpnconfig/`
Expected: PASS, no vet or gofmt output.

- [ ] **Step 5: Commit**

```bash
git add server/internal/vpnconfig/clients.go server/internal/vpnconfig/clients_test.go
git commit -m "feat(vpnconfig): MoveClient puts a client on another route in one change"
```

---

### Task 6: `POST /api/clients/route`

**Files:**
- Modify: `server/internal/webapi/handler_clients.go`
- Modify: `server/internal/webapi/router.go`
- Test: `server/internal/webapi/handler_clients_test.go`, `server/internal/webapi/deadline_test.go`

**Interfaces:**
- Consumes: `vpnconfig.MoveClient`, `vpnconfig.MoveResult` (Task 5); `updateAndApply`, `writeSaveApplyResult`, `lockLongOp`, `decodeJSON`, `jsonError`, `jsonOK`, `extendWriteDeadline`, `httpError` (existing).
- Produces: `checkClientRoute(w, deps, route) bool`; `handleMoveClient(deps) http.HandlerFunc`; the route `POST /api/clients/route`.

- [ ] **Step 1: Write the failing tests**

Append to `server/internal/webapi/handler_clients_test.go`:

```go
// movableClients is 192.168.50.10 on Xray, which excludes ru, and
// 192.168.50.30 on wgc1.
func movableClients() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}, ExcludeSets: []string{"ru"}},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
			},
		},
	}
}

func moveRequest(body string) *http.Request {
	return httptest.NewRequest("POST", "/api/clients/route", strings.NewReader(body))
}

func TestHandleMoveClient_XrayToTunnel(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"wgc1"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mc.savedCfg.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v, want the address gone", mc.savedCfg.Xray.Clients)
	}
	if got := mc.savedCfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("expected one apply, got %d", vpn.applyCalls)
	}
}

func TestHandleMoveClient_TunnelToXray(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.30","route":"xray"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Join(mc.savedCfg.Xray.Clients, ",") != "192.168.50.10,192.168.50.30" {
		t.Errorf("xray.clients = %v", mc.savedCfg.Xray.Clients)
	}
	if got := mc.savedCfg.TunnelDirector.Tunnels["wgc1"].Clients; len(got) != 0 {
		t.Errorf("wgc1 = %v", got)
	}
}

// newTestDeps's platform lists wgc1 and ovpnc1; ovpnc1 is not in the config.
func TestHandleMoveClient_NewTunnelInheritsTheXrayExclusions(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"ovpnc1"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	tun := mc.savedCfg.TunnelDirector.Tunnels["ovpnc1"]
	if strings.Join(tun.Clients, ",") != "192.168.50.10" || strings.Join(tun.Exclude, ",") != "ru" {
		t.Errorf("ovpnc1 = %+v", tun)
	}
}

func TestHandleMoveClient_ToTheRouteItIsOnChangesNothing(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"xray"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg != nil {
		t.Error("a move that changes nothing must write nothing")
	}
	if vpn.applyCalls != 0 {
		t.Errorf("a move that changes nothing must not apply, got %d applies", vpn.applyCalls)
	}
}

func TestHandleMoveClient_NotFound(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.99","route":"wgc1"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", mc.savedCfg, vpn.applyCalls)
	}
}

func TestHandleMoveClient_RejectsBadInput(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"route":"xray"}`,
		`{"ip":"fd00::1","route":"xray"}`,
		`{"ip":"192.168.50.10"}`,
	} {
		deps := newTestDeps(t)
		deps.Config = &mockConfig{cfg: movableClients()}
		rec := httptest.NewRecorder()
		handleMoveClient(deps).ServeHTTP(rec, moveRequest(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleMoveClient_RouteMustBeATunnelThePlatformLists(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: movableClients()}
	deps.VPN = &mockVPN{platform: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "OpenVPN0"}}}}

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"wgc2"}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid route") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMoveClient_PlatformUnavailableIs503(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{platformErr: errors.New("down")}

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"OpenVPN0"}`))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "platform info unavailable") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg != nil {
		t.Error("nothing may be saved when the route could not be validated")
	}
}

func TestHandleMoveClient_SavedButApplyFailed(t *testing.T) {
	mc := &mockConfig{cfg: movableClients()}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{err: errors.New("apply failed (exit 1):\n[ERROR] iptables missing")}

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"wgc1"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["saved"] != true {
		t.Errorf("expected saved: true, got %v", resp["saved"])
	}
}

func TestHandleMoveClient_KeepsThePausedState(t *testing.T) {
	cfg := movableClients()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10/32"}
	mc := &mockConfig{cfg: cfg}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.10","route":"wgc1"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Join(mc.savedCfg.PausedClients, ",") != "192.168.50.10" {
		t.Errorf("paused_clients = %v", mc.savedCfg.PausedClients)
	}
}

// Moving an address during a failover is where the user wants it; left in the
// failover record, the restore would take it back to Xray.
func TestHandleMoveClient_DetachesTheAddressFromTheFailover(t *testing.T) {
	mc := &mockConfig{cfg: failedOverClients()}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleMoveClient(deps).ServeHTTP(rec, moveRequest(`{"ip":"192.168.50.8","route":"xray"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fo := mc.savedCfg.Xray.Failover; fo == nil || len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Fatalf("failover %+v", fo)
	}
	if strings.Join(mc.savedCfg.Xray.Clients, ",") != "192.168.50.8" {
		t.Errorf("xray.clients = %v", mc.savedCfg.Xray.Clients)
	}
}
```

In `server/internal/webapi/deadline_test.go`, in `TestLongOpHandlers_ExtendWriteDeadline`, add after the `delete client` row:

```go
		{"move client", "POST", "/api/clients/route", `{"ip":"192.168.50.10","route":"xray"}`, handleMoveClient, applyDeadline},
```

- [ ] **Step 2: Run the tests to see them fail**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/webapi/ -run 'TestHandleMoveClient|TestLongOpHandlers' -count=1`
Expected: FAIL — `undefined: handleMoveClient`.

- [ ] **Step 3: The handler**

In `server/internal/webapi/handler_clients.go`, add `"errors"` to the imports. In `handleAddClient`, replace the whole block from the comment `// A route is xray, a tunnel already in the config, or a tunnel this` through the closing `}` of `if req.Route != "xray" { ... }` with:

```go
		if !checkClientRoute(w, deps, req.Route) {
			return
		}
```

Add, right after `handleAddClient`:

```go
// checkClientRoute answers whether route may take a client, for an add and a
// move alike: xray, a tunnel already in the config, or a tunnel this router
// has, asked of the platform now - the list is the firmware's (Merlin
// wgcN/ovpncN, Keenetic OpenVPN0, Wireguard1, ...) and a tunnel can appear or
// go at any time. A configured tunnel is accepted without the platform, as the
// bot and the wizard accept it: ClientsTab offers exactly those when the
// platform cannot be asked, and a 503 here made that fallback unusable. An
// empty list is not an answer either: on Keenetic `vpn-director.sh platform`
// prints "tunnels": [] and exits 0 while RCI does not reply (spec 13). It
// writes the error response itself and returns false when the handler must
// stop.
func checkClientRoute(w http.ResponseWriter, deps *Deps, route string) bool {
	if route == "xray" {
		return true
	}
	cfg, err := deps.Config.LoadVPNConfig()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load configuration")
		return false
	}
	if _, configured := cfg.TunnelDirector.Tunnels[route]; configured {
		return true
	}
	extendWriteDeadline(w, statusDeadline)
	info, err := deps.VPN.Platform()
	if err != nil || len(info.Tunnels) == 0 {
		jsonError(w, http.StatusServiceUnavailable, "platform info unavailable")
		return false
	}
	if !info.HasTunnel(route) {
		jsonError(w, http.StatusBadRequest, "invalid route: must be xray or a tunnel this router has")
		return false
	}
	return true
}

// moveClientRequest is the expected JSON body for POST /api/clients/route.
type moveClientRequest struct {
	IP    string `json:"ip"`
	Route string `json:"route"`
}

// errClientUnchanged ends a move whose route already holds the address alone:
// the update writes nothing, and there is nothing to apply.
var errClientUnchanged = errors.New("client already on that route")

// handleMoveClient returns a handler that moves a client to another route in
// one config change and one apply (vpnconfig.MoveClient). The apply moves it
// make-before-break: the client stays on its old route until the new one
// carries it, so none of its packets leaves through the WAN meanwhile - a
// delete and an add left it there for as long as the user took between them.
func handleMoveClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req moveClientRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.IP == "" {
			jsonError(w, http.StatusBadRequest, "ip is required")
			return
		}
		ip, err := vpnconfig.NormalizeClientAddr(req.IP)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Route == "" {
			jsonError(w, http.StatusBadRequest, "route is required")
			return
		}
		if !checkClientRoute(w, deps, req.Route) {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		err = updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			switch vpnconfig.MoveClient(cfg, ip, req.Route) {
			case vpnconfig.ClientNotFound:
				return &httpError{status: http.StatusNotFound, msg: "client not found"}
			case vpnconfig.ClientAlreadyThere:
				return errClientUnchanged
			}
			return nil
		})
		if errors.Is(err, errClientUnchanged) {
			jsonOK(w, map[string]bool{"ok": true})
			return
		}
		writeSaveApplyResult(w, err)
	}
}
```

In `server/internal/webapi/router.go`, add after `mux.HandleFunc("POST /api/clients/resume", handleResumeClient(deps))`:

```go
	mux.HandleFunc("POST /api/clients/route", handleMoveClient(deps))
```

- [ ] **Step 4: Run the tests to see them pass**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/webapi/ -count=1 && go vet ./internal/webapi/ && gofmt -l internal/webapi/`
Expected: PASS, the existing `TestHandleAddClient_*` tests included; no vet or gofmt output.

- [ ] **Step 5: Commit**

```bash
git add server/internal/webapi/handler_clients.go server/internal/webapi/router.go \
        server/internal/webapi/handler_clients_test.go server/internal/webapi/deadline_test.go
git commit -m "feat(webapi): POST /api/clients/route moves a client in one apply"
```

---

### Task 7: Web UI — the Route column moves a client

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/components/ClientsTab.vue`

**Interfaces:**
- Consumes: `POST /api/clients/route` (Task 6).
- Produces: `api.moveClient(ip, route)`.

- [ ] **Step 1: The API call**

In `web/src/api.ts`, add after `resumeClient`:

```ts
  // One change and one apply: the router takes the client off its old route
  // only once the new one carries it.
  moveClient: (ip: string, route: string) =>
    api.post<OkResponse>('/api/clients/route', { ip, route }),
```

- [ ] **Step 2: The tab**

Replace the whole content of `web/src/components/ClientsTab.vue` with:

```vue
<script setup lang="ts">
import { ref, onMounted } from 'vue'
import api from '../api'
import type { ClientInfo, PlatformTunnel } from '../types'

const clients = ref<ClientInfo[]>([])
const loading = ref(false)
const actionLoading = ref('')
const error = ref('')

const newIp = ref('')
const newRoute = ref('xray')
const addLoading = ref(false)

interface RouteOption {
  value: string
  label: string
}

// xray first, then every tunnel the router has. These options feed the Add
// form's select and every row's Route select, and POST /api/clients and
// /api/clients/route reject a route the platform does not list, so a listed
// client's unlisted route is offered only when the platform itself could not
// be asked - the fallback the form's own message describes. A row always
// offers its own route as well (rowRouteOptions), so nothing is hidden.
// Rebuilt whenever the platform or the clients are reloaded.
const routeOptions = ref<RouteOption[]>([{ value: 'xray', label: 'xray' }])
const platformTunnels = ref<PlatformTunnel[]>([])
const platformError = ref('')

function buildRouteOptions() {
  const opts: RouteOption[] = [{ value: 'xray', label: 'xray' }]
  for (const t of platformTunnels.value) {
    const desc = t.description ? ` — ${t.description}` : ''
    opts.push({ value: t.id, label: `${t.id}${desc}${t.connected ? '' : ' (down)'}` })
  }
  if (platformError.value !== '') {
    for (const c of clients.value) {
      if (!opts.some((o) => o.value === c.route)) {
        opts.push({ value: c.route, label: c.route })
      }
    }
  }
  routeOptions.value = opts
  if (!opts.some((o) => o.value === newRoute.value)) {
    newRoute.value = 'xray'
  }
}

// A row's options are the shared ones plus the row's own route, so its select
// never shows blank for a tunnel the platform no longer lists.
function rowRouteOptions(client: ClientInfo): RouteOption[] {
  if (routeOptions.value.some((o) => o.value === client.route)) {
    return routeOptions.value
  }
  return [...routeOptions.value, { value: client.route, label: client.route }]
}

// A tunnel the platform reports down has no route in its table, and a client
// put on it goes out through the WAN until it comes up.
function isDown(route: string): boolean {
  return platformTunnels.value.some((t) => t.id === route && !t.connected)
}

function confirmDown(route: string, verb: string): boolean {
  return confirm(
    `${route} is down: until it is up, this client's traffic goes out through the WAN. ${verb} anyway?`,
  )
}

async function loadPlatform() {
  try {
    const resp = await api.getPlatform()
    platformTunnels.value = resp.data.tunnels ?? []
    platformError.value = ''
  } catch (e: any) {
    platformTunnels.value = []
    platformError.value = e.response?.data?.error || e.message
  }
  buildRouteOptions()
}

// Shows the server error; when the change was saved but apply failed
// (response carries saved: true) the list is refreshed so the saved
// change is visible and the Status tab's Apply can retry. A 404 is
// refreshed too: the server says the row does not exist, so the displayed
// list is stale and is exactly what must be reloaded.
async function reportError(e: any) {
  alert('Error: ' + (e.response?.data?.error || e.message))
  if (e.response?.data?.saved || e.response?.status === 404) {
    await loadClients()
  }
}

async function loadClients() {
  loading.value = true
  error.value = ''
  try {
    const resp = await api.getClients()
    clients.value = resp.data.clients ?? []
    buildRouteOptions()
  } catch (e: any) {
    error.value = e.response?.data?.error || e.message
  } finally {
    loading.value = false
  }
}

async function addClient() {
  if (!newIp.value.trim()) return
  if (isDown(newRoute.value) && !confirmDown(newRoute.value, 'Add')) return
  addLoading.value = true
  try {
    await api.addClient(newIp.value.trim(), newRoute.value)
    newIp.value = ''
    newRoute.value = 'xray'
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    addLoading.value = false
  }
}

// One request moves the client: the router writes the new route and applies
// once, and the apply keeps the client on its old route until the new one
// carries it. A select whose move did not happen goes back to the route the
// client is on.
async function moveClient(client: ClientInfo, event: Event) {
  const select = event.target as HTMLSelectElement
  const route = select.value
  if (route === client.route) return
  if (isDown(route) && !confirmDown(route, 'Move')) {
    select.value = client.route
    return
  }
  actionLoading.value = 'move:' + client.ip
  try {
    await api.moveClient(client.ip, route)
    await loadClients()
  } catch (e: any) {
    select.value = client.route
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

async function pauseClient(ip: string) {
  actionLoading.value = 'pause:' + ip
  try {
    await api.pauseClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

async function resumeClient(ip: string) {
  actionLoading.value = 'resume:' + ip
  try {
    await api.resumeClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

async function removeClient(ip: string) {
  if (!confirm('Remove client ' + ip + '?')) return
  actionLoading.value = 'remove:' + ip
  try {
    await api.deleteClient(ip)
    await loadClients()
  } catch (e: any) {
    await reportError(e)
  } finally {
    actionLoading.value = ''
  }
}

onMounted(async () => {
  await Promise.all([loadClients(), loadPlatform()])
})
</script>

<template>
  <div class="card">
    <div class="card-title">Add Client</div>
    <div style="display: flex; gap: 0.5rem; align-items: flex-end; flex-wrap: wrap;">
      <div class="form-group" style="flex: 1; min-width: 180px; margin-bottom: 0;">
        <label>IP / CIDR</label>
        <input
          v-model="newIp"
          placeholder="192.168.50.10 or 192.168.50.0/24"
          @keyup.enter="addClient"
        />
      </div>
      <div class="form-group" style="width: 260px; margin-bottom: 0;">
        <label>Route</label>
        <select v-model="newRoute">
          <option v-for="r in routeOptions" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
      </div>
      <button class="btn btn-primary" :disabled="addLoading || !newIp.trim()" @click="addClient">
        {{ addLoading ? '...' : '+ Add' }}
      </button>
    </div>
    <p v-if="platformError" class="error-msg" style="margin-top: 0.5rem;">
      Tunnel list unavailable ({{ platformError }}); only xray and the routes in use are offered.
    </p>
  </div>

  <div class="card">
    <div class="card-title">Clients</div>

    <div class="actions">
      <button class="btn btn-blue" :disabled="loading" @click="loadClients">
        {{ loading ? '...' : '⟳ Refresh' }}
      </button>
    </div>

    <p v-if="error" class="error-msg">{{ error }}</p>

    <table v-if="clients.length > 0">
      <thead>
        <tr>
          <th>IP</th>
          <th>Route</th>
          <th>Status</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        <!-- ip|route: during a staged failover one address sits on two routes. -->
        <tr v-for="client in clients" :key="client.ip + '|' + client.route">
          <td>{{ client.ip }}</td>
          <td>
            <select
              :value="client.route"
              :disabled="!!actionLoading"
              style="min-width: 160px;"
              @change="moveClient(client, $event)"
            >
              <option v-for="r in rowRouteOptions(client)" :key="r.value" :value="r.value">
                {{ r.label }}
              </option>
            </select>
          </td>
          <td>
            <span v-if="!client.paused" class="badge badge-green">Active</span>
            <span v-else class="badge badge-grey">Paused</span>
          </td>
          <td style="display: flex; gap: 0.35rem;">
            <button
              v-if="!client.paused"
              class="btn btn-yellow"
              :disabled="!!actionLoading"
              @click="pauseClient(client.ip)"
            >
              {{ actionLoading === 'pause:' + client.ip ? '...' : 'Pause' }}
            </button>
            <button
              v-else
              class="btn btn-green"
              :disabled="!!actionLoading"
              @click="resumeClient(client.ip)"
            >
              {{ actionLoading === 'resume:' + client.ip ? '...' : 'Resume' }}
            </button>
            <button
              class="btn btn-red"
              :disabled="!!actionLoading"
              @click="removeClient(client.ip)"
            >
              {{ actionLoading === 'remove:' + client.ip ? '...' : 'Remove' }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-else-if="!loading" style="color: #999; font-size: 0.875rem;">
      No clients configured.
    </p>
  </div>
</template>
```

- [ ] **Step 3: Type-check and build**

Run (through `claude-forge:build-runner`): `cd web && npm run build`
Expected: `vue-tsc -b` reports nothing, and `vite build` writes `web/dist`.

- [ ] **Step 4: Look at it in dev mode**

Run `make web-embed` from the repository root (through `claude-forge:build-runner`: it runs npm), then `cd server && go run ./cmd/webui --dev`; log in as `admin`/`admin` and open Clients. `server/testdata/dev/platform.json` lists `wgc1` (connected) and `ovpnc1` (down).
- Add `192.168.50.10` on xray; its row shows a select on `xray`.
- Pick `wgc1`: the row reloads on `wgc1`.
- Pick `ovpnc1 — Office OVPN (down)`: a confirmation names `ovpnc1`; Cancel puts the select back on `wgc1`, OK moves the client.
- Choosing `ovpnc1` in the Add form asks "… Add anyway?".
Stop the server.

- [ ] **Step 5: Commit**

```bash
git add web/src/api.ts web/src/components/ClientsTab.vue
git commit -m "feat(web): the Route column moves a client"
```

---

### Task 8: Bot — `/clients` moves a client

**Files:**
- Modify: `server/internal/handler/clients.go`
- Test: `server/internal/handler/clients_test.go`

**Interfaces:**
- Consumes: `vpnconfig.MoveClient`, `vpnconfig.ClientRoutes`, `vpnconfig.MoveResult` (Task 5); `vpnconfig.NormalizeClientAddr`, `PlatformInfo.HasTunnel`, `configUpdateError`, `telegram.NewKeyboard`, `telegram.EscapeMarkdownV2` (existing).
- Produces: callbacks `clients:move:<ip>`, `clients:to:<route>:<ip>`, `clients:toyes:<route>:<ip>`; `(*ClientsHandler).routeChoices(cfg) ([]routeChoice, bool)`, which `showRouteSelection` uses too.

- [ ] **Step 1: Write the failing tests**

In `server/internal/handler/clients_test.go`, give `mockVPNClients` an apply counter — replace its struct and its `Apply` with:

```go
type mockVPNClients struct {
	applyErr    error
	applyCalls  int
	platform    vpnconfig.PlatformInfo
	platformErr error
}
```

```go
func (m *mockVPNClients) Apply() error { m.applyCalls++; return m.applyErr }
```

Append:

```go
// moveCfg is 192.168.50.10 on Xray, which excludes ru, and 192.168.50.30 on wgc1.
func moveCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}, ExcludeSets: []string{"ru"}},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
		}},
	}
}

// movePlatform lists OpenVPN0 up and Wireguard1 down.
func movePlatform() vpnconfig.PlatformInfo {
	return vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "OpenVPN0", Connected: true, Description: "office"},
		{ID: "Wireguard1", Connected: false},
	}}
}

func moveCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{
		Data:    data,
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
}

func TestClientsHandler_ListOffersAMoveButtonPerClient(t *testing.T) {
	sender := &mockSenderClients{}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: moveCfg()}})
	h.HandleClients(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}})

	row := sender.lastKeyboard.InlineKeyboard[0]
	if len(row) != 3 || *row[1].CallbackData != "clients:move:192.168.50.10" || row[1].Text != "\U0001f500 192.168.50.10" {
		t.Fatalf("row 0 = %+v, want pause, move, remove", row)
	}
}

// An entry an older build saved as IPv6 goes on no route: it can be paused or
// removed, not moved.
func TestClientsHandler_NoMoveButtonForAnAddressNoRouteTakes(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"fd00::10"}}}
	sender := &mockSenderClients{}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: cfg}})
	h.HandleClients(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}})

	if row := sender.lastKeyboard.InlineKeyboard[0]; len(row) != 2 {
		t.Fatalf("row 0 = %+v, want pause and remove only", row)
	}
}

func TestClientsHandler_MoveKeyboardMarksTheCurrentRoute(t *testing.T) {
	sender := &mockSenderClients{}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: moveCfg()}, VPN: vpn})

	h.HandleCallback(moveCallback("clients:move:192.168.50.10"))

	rows := sender.editKeyboard.InlineKeyboard
	want := []struct{ text, data string }{
		{"✓ xray", "clients:to:xray:192.168.50.10"},
		{"OpenVPN0 office", "clients:to:OpenVPN0:192.168.50.10"},
		{"Wireguard1 (down)", "clients:to:Wireguard1:192.168.50.10"},
		{"wgc1 (unknown)", "clients:to:wgc1:192.168.50.10"},
		{"« Back", "clients:rm_no"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i, w := range want {
		if rows[i][0].Text != w.text || *rows[i][0].CallbackData != w.data {
			t.Errorf("row %d = %q %q, want %q %q", i, rows[i][0].Text, *rows[i][0].CallbackData, w.text, w.data)
		}
	}
	if sender.editMsgID != 42 {
		t.Errorf("editMsgID = %d, want the list replaced", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToATunnel(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.10"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", config.savedConfig.Xray.Clients)
	}
	tun := config.savedConfig.TunnelDirector.Tunnels["OpenVPN0"]
	if strings.Join(tun.Clients, ",") != "192.168.50.10" || strings.Join(tun.Exclude, ",") != "ru" {
		t.Errorf("OpenVPN0 = %+v", tun)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("applies = %d, want 1", vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToADownTunnelAsksFirst(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:Wireguard1:192.168.50.10"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Fatal("a move to a tunnel that is down happens only once confirmed")
	}
	if !strings.Contains(sender.editText, "Wireguard1 is down") {
		t.Errorf("edit text = %q", sender.editText)
	}
	row := sender.editKeyboard.InlineKeyboard[0]
	if len(row) != 2 || *row[0].CallbackData != "clients:toyes:Wireguard1:192.168.50.10" || *row[1].CallbackData != "clients:rm_no" {
		t.Errorf("confirmation row = %+v", row)
	}
}

func TestClientsHandler_MoveConfirmedToADownTunnel(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:toyes:Wireguard1:192.168.50.10"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if got := config.savedConfig.TunnelDirector.Tunnels["Wireguard1"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("Wireguard1 = %v", got)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("applies = %d, want 1", vpn.applyCalls)
	}
}

func TestClientsHandler_MoveToTheRouteItIsOnChangesNothing(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:xray:192.168.50.10"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveOfAClientThatIsGoneRedrawsTheList(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.99"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToARouteNoLongerAvailable(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:Wireguard9:192.168.50.10"))

	if config.savedConfig != nil {
		t.Error("a route the router does not have must not be saved")
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "route Wireguard9 is no longer available") {
		t.Errorf("plain texts = %v", sender.plainTexts)
	}
}

// The buttons carry the stored spelling; the paused entry follows the address
// to the spelling it is stored under now.
func TestClientsHandler_MoveKeepsThePausedState(t *testing.T) {
	cfg := moveCfg()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10/32"}
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.10/32"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if got := config.savedConfig.TunnelDirector.Tunnels["OpenVPN0"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("OpenVPN0 = %v", got)
	}
	if strings.Join(config.savedConfig.PausedClients, ",") != "192.168.50.10" {
		t.Errorf("paused_clients = %v", config.savedConfig.PausedClients)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/handler/ -run 'TestClientsHandler' -count=1`
Expected: the new tests FAIL (no move button; the `move:`, `to:` and `toyes:` callbacks do nothing).

- [ ] **Step 3: The route choices, shared with the Add flow**

In `server/internal/handler/clients.go`, add `"errors"` and `"slices"` to the imports. Replace the whole `showRouteSelection` function, with its comment, with:

```go
// routeChoice is one route the keyboards offer a client.
type routeChoice struct {
	id    string
	label string
}

// routeChoices lists xray, every tunnel the platform lists (with its
// description, and "(down)" when it is not connected), and the tunnels already
// in the config the platform does not list, so an existing route stays
// reachable - marked "(unknown)" the way the Web UI marks them when the
// platform answered, bare when it could not be asked: the tunnel is not
// unknown then, the platform is. answered is false in that last case.
func (h *ClientsHandler) routeChoices(cfg *vpnconfig.VPNDirectorConfig) (choices []routeChoice, answered bool) {
	choices = append(choices, routeChoice{id: "xray", label: "xray"})
	listed := map[string]bool{}
	info, err := h.deps.VPN.Platform()
	answered = err == nil
	if answered {
		for _, t := range info.Tunnels {
			label := t.ID
			if t.Description != "" {
				label += " " + t.Description
			}
			if !t.Connected {
				label += " (down)"
			}
			choices = append(choices, routeChoice{id: t.ID, label: label})
			listed[t.ID] = true
		}
	}

	names := make([]string, 0, len(cfg.TunnelDirector.Tunnels))
	for name := range cfg.TunnelDirector.Tunnels {
		if !listed[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		label := name
		if answered {
			label += " (unknown)"
		}
		choices = append(choices, routeChoice{id: name, label: label})
	}
	return choices, answered
}

// showRouteSelection offers the routes a new client can go on.
func (h *ClientsHandler) showRouteSelection(chatID int64, ip string, cfg *vpnconfig.VPNDirectorConfig) {
	choices, answered := h.routeChoices(cfg)
	if !answered {
		h.deps.Sender.SendPlain(chatID, "platform info unavailable; offering the configured tunnels only")
	}
	kb := telegram.NewKeyboard()
	for _, c := range choices {
		kb.Button(c.label, fmt.Sprintf("clients:route:%s", c.id)).Row()
	}
	kb.Button("Cancel", "clients:route:cancel").Row()

	text := telegram.EscapeMarkdownV2(fmt.Sprintf("Select route for %s:", ip))
	h.deps.Sender.SendWithKeyboard(chatID, text, kb.Build())
}
```

- [ ] **Step 4: The move button, the keyboard and the move**

In `buildClientList`, replace

```go
			kb.Button(fmt.Sprintf("\U0001f5d1 %s", c.IP), fmt.Sprintf("clients:remove:%s", c.IP))
			kb.Row()
```

with

```go
			// A move needs an address a route can take; an entry an older build
			// saved in another form can only be paused or removed.
			if _, err := vpnconfig.NormalizeClientAddr(c.IP); err == nil {
				kb.Button(fmt.Sprintf("\U0001f500 %s", c.IP), fmt.Sprintf("clients:move:%s", c.IP))
			}
			kb.Button(fmt.Sprintf("\U0001f5d1 %s", c.IP), fmt.Sprintf("clients:remove:%s", c.IP))
			kb.Row()
```

In `HandleCallback`, add these cases before `case strings.HasPrefix(action, "route:"):`:

```go
	case strings.HasPrefix(action, "move:"):
		h.handleMoveStart(chatID, msgID, strings.TrimPrefix(action, "move:"))
	case strings.HasPrefix(action, "to:"):
		h.handleMoveTo(chatID, msgID, strings.TrimPrefix(action, "to:"), false)
	case strings.HasPrefix(action, "toyes:"):
		h.handleMoveTo(chatID, msgID, strings.TrimPrefix(action, "toyes:"), true)
```

Add at the end of the file:

```go
// errNothingToMove ends a move whose client is gone or already on that route
// alone: the update writes nothing, and there is nothing to apply.
var errNothingToMove = errors.New("nothing to move")

// handleMoveStart replaces the list with the routes a client can move to, its
// current one marked ✓ (two during a staged failover).
func (h *ClientsHandler) handleMoveStart(chatID int64, msgID int, ip string) {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}
	current := vpnconfig.ClientRoutes(cfg, ip)
	if len(current) == 0 {
		// Removed since the list was sent.
		text, kb := h.buildClientList(cfg)
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	}

	choices, answered := h.routeChoices(cfg)
	if !answered {
		h.deps.Sender.SendPlain(chatID, "platform info unavailable; offering the configured tunnels only")
	}
	kb := telegram.NewKeyboard()
	for _, c := range choices {
		label := c.label
		if slices.Contains(current, c.id) {
			label = "✓ " + label
		}
		kb.Button(label, fmt.Sprintf("clients:to:%s:%s", c.id, ip)).Row()
	}
	kb.Button("« Back", "clients:rm_no").Row()

	text := telegram.EscapeMarkdownV2(fmt.Sprintf("Move %s (now on %s) to:", ip, strings.Join(current, ", ")))
	h.deps.Sender.EditMessage(chatID, msgID, text, kb.Build())
}

// handleMoveTo moves a client to a route in one config change and one apply;
// the apply keeps it on its old route until the new one carries it. data is
// "<route>:<ip>". A tunnel the platform reports down asks first, unless
// confirmed: until it is up, the client's traffic goes out through the WAN.
func (h *ClientsHandler) handleMoveTo(chatID int64, msgID int, data string, confirmed bool) {
	route, ip, ok := strings.Cut(data, ":")
	if !ok || route == "" || ip == "" {
		return
	}

	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}
	if len(vpnconfig.ClientRoutes(cfg, ip)) == 0 {
		// Removed since the keyboard was sent.
		text, kb := h.buildClientList(cfg)
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	}

	if route != "xray" {
		_, configured := cfg.TunnelDirector.Tunnels[route]
		info, perr := h.deps.VPN.Platform()
		if !configured {
			// Not configured yet: fine when the router has the tunnel (the
			// keyboard listed it from the platform), stale otherwise.
			if perr != nil {
				h.deps.Sender.SendPlain(chatID, "platform info unavailable, try again")
				text, kb := h.buildClientList(cfg)
				h.deps.Sender.EditMessage(chatID, msgID, text, kb)
				return
			}
			if !info.HasTunnel(route) {
				h.deps.Sender.SendPlain(chatID, fmt.Sprintf("route %s is no longer available", route))
				text, kb := h.buildClientList(cfg)
				h.deps.Sender.EditMessage(chatID, msgID, text, kb)
				return
			}
		}
		if !confirmed && perr == nil && tunnelDown(info, route) {
			text := telegram.EscapeMarkdownV2(fmt.Sprintf(
				"%s is down: until it is up, %s's traffic goes out through the WAN. Move anyway?", route, ip))
			kb := telegram.NewKeyboard()
			kb.Button("Move anyway", fmt.Sprintf("clients:toyes:%s:%s", route, ip))
			kb.Button("Cancel", "clients:rm_no")
			kb.Row()
			h.deps.Sender.EditMessage(chatID, msgID, text, kb.Build())
			return
		}
	}

	err = h.deps.Config.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
		if vpnconfig.MoveClient(c, ip, route) != vpnconfig.ClientMoved {
			return errNothingToMove
		}
		cfg = c // render the list from what was actually saved
		return nil
	})
	if errors.Is(err, errNothingToMove) {
		h.handleRefreshList(chatID, msgID)
		return
	}
	if err != nil {
		h.deps.Sender.SendPlain(chatID, configUpdateError(err))
		return
	}

	if err := h.deps.VPN.Apply(); err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Apply error: %v", err))
		return
	}

	text, kb := h.buildClientList(cfg)
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

// tunnelDown reports whether the platform lists route as a tunnel that is not
// connected.
func tunnelDown(info vpnconfig.PlatformInfo, route string) bool {
	for _, t := range info.Tunnels {
		if t.ID == route {
			return !t.Connected
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run (through `claude-forge:build-runner`): `cd server && go test ./internal/handler/ ./internal/bot/ -count=1 && go vet ./internal/handler/ && gofmt -l internal/handler/`
Expected: PASS — the existing `/clients`, Add-flow and route keyboard tests included; no vet or gofmt output.

- [ ] **Step 6: Commit**

```bash
git add server/internal/handler/clients.go server/internal/handler/clients_test.go
git commit -m "feat(bot): /clients moves a client with one button"
```

---

### Task 9: Documentation

**Files:**
- Modify: `.claude/rules/packet-flow.md`, `.claude/rules/tunnel-director.md`, `.claude/rules/xray-tproxy.md`, `.claude/rules/shell-conventions.md`, `.claude/rules/testing.md`, `.claude/rules/webui.md`, `.claude/rules/telegram-bot.md`
- Modify: `README.md`, `README.ru.md`, `CLAUDE.md`

**Interfaces:** none.

- [ ] **Step 1: `packet-flow.md`**

Insert before `## Fwmark Bit Layout`:

```markdown
## The apply: make before break

A full apply (`apply`, `update`, `restart`) moves every client make-before-break:

1. `tproxy_apply` adds every client `xray.clients` names to `XRAY_CLIENTS` and removes none.
2. `tunnel_apply` puts Tunnel Director's rules in place.
3. `tproxy_prune` swaps in the exact `XRAY_CLIENTS`.

Xray wins over TUN_DIR (above). A client moving from Xray to a tunnel stays proxied until step 3
lets it go, when TUN_DIR already marks it; a client moving the other way is proxied from step 1,
before TUN_DIR lets it go in step 2. A Tunnel Director failure ends the run before step 3: the
clients that left Xray stay proxied rather than leak.

No step flushes a chain or a set a packet is crossing. `XRAY_TPROXY` and a rebuilt `TUN_DIR` are
built as `<chain>_NEW` and swapped in by `swap_fw_chain` (`lib/firewall.sh`): the new jump goes in
ahead of the old one, the old jump and chain go, the new chain takes the name. The sets are built
as `<set>_NEW` and swapped in (`ipset swap`). A Tunnel Director rebuild keeps every tunnel's slot
and installs the routing of a new slot before the swap and releases a dropped one after it
(`tunnel-director.md`). `restart` and `restart xray` stop nothing; while the Xray process
restarts, TPROXY drops what no socket takes.

Left open: the firmware flushing our chains (Merlin's `iptables -t mangle -F` on a firewall start,
an NDM rebuild on KeeneticOS) leaves the clients on the WAN until the hook's apply, and a tunnel
that is down sends its clients to `main`.
```

In `### Position Calculation`, append to the paragraph that starts `` `tunnel_apply` raises `base_pos` ``:

```markdown
A rebuild inserts its jumps to `TUN_DIR_NEW` at those positions, ahead of the old jumps, which go
after (`swap_fw_chain`).
```

In `### Temporary state`, replace the `tun_dir_tables` row with:

```markdown
| `/tmp/tunnel_director/tun_dir_tables` | Applied tunnels, `<idx> <id>` per line (`TUN_DIR_TABLES`); a tunnel keeps its idx while it has clients; during a rebuild, the union of both layouts |
```

- [ ] **Step 2: `tunnel-director.md`**

Replace the bullet `- Order of tunnels in JSON determines fwmark assignment` with:

```markdown
- Order of tunnels in JSON decides which rule a client meets first; a tunnel's fwmark comes from its slot, which it keeps while it has clients (see "Stable slots and the rebuild in place")
```

Insert before `## State Tracking`:

```markdown
## Stable slots and the rebuild in place

A tunnel's slot `idx` gives its mark (`(idx + 1) << mark_shift`), its ip rule preference
(`pref_base + idx`) and, on Keenetic, its table (`2000 + idx`). `_tunnel_collect_applied` reads the
previous layout from `TUN_DIR_TABLES`: a tunnel in it keeps its idx, and a tunnel new to the layout
takes the lowest idx that neither the previous nor the new layout holds, so an apply never reuses a
slot it frees. The chain keeps the JSON order (the failover clients first), so the first match still
wins between overlapping CIDRs. The slots used to follow the JSON order, and a tunnel that gained
its first client or lost its last shifted the mark, preference and table of every tunnel after it.

A rebuild no longer starts with `tunnel_stop`:

1. Routing first, for every tunnel of the new layout. A new slot's table is released, its route
   ensured and its ip rule replaced; a kept slot's route and rule are only ensured
   (`_tunnel_rule_ensure` leaves a correct rule alone), and its table is never released.
2. `TUN_DIR_NEW` is built (`_tunnel_build_chain`) and swapped in by `swap_fw_chain`.
3. Every slot the new layout dropped loses its ip rule (only when it is ours) and its table
   (`_tunnel_slot_release`) — after the swap, when no packet carries its mark.
4. The hash and `failover_ready` are recorded. `TUN_DIR_TABLES` holds the union of both layouts
   from step 1 to step 3, and the new layout after.

The hash is removed when a rebuild starts and written back only by one that completes: a rebuild
that dies part-way leaves the next apply a rebuild, which finishes the interrupted swap (step 0 of
`swap_fw_chain`) and releases the slots left on record. A rebuild whose swap did not take over every
interface (`swap_fw_chain` returned 2 or 3) records no hash and releases nothing.
```

In `## State Tracking`:

- In the paragraph that begins `The two can be out of step in one direction`, replace its last two sentences (from `The cleanup that precedes a rebuild` to the end of the paragraph) with:

  ```markdown
  A rebuild reads its previous layout from `TUN_DIR_TABLES` whatever the hash says, so the tunnels on
  record keep their slots and the ones gone from the layout are released.
  ```

- Delete the sentence `Deleting the hash forced the next apply through `tunnel_stop`.`
- Replace `because under errexit one refused MARK used to end the apply after `tunnel_stop` had purged the jumps and the ip rules of every tunnel.` with `because under errexit one refused MARK used to end the apply half-way.`
- Replace the sentence `The PREROUTING jumps are different: a rebuild whose jump did not go in keeps its hash, and the up-to-date branch puts a missing jump back (`_tunnel_jumps_ensure`), leaving one that is there where it is.` with:

  ```markdown
  The PREROUTING jumps: a rebuild whose new jumps did not all go in records no hash, and the next
  apply finishes the swap. A jump that goes missing after a recorded rebuild is put back by the
  up-to-date branch (`_tunnel_jumps_ensure`), which leaves one that is there where it is.
  ```

- Replace the paragraph that begins `The rebuild loop also releases each tunnel's table` with:

  ```markdown
  A rebuild releases the table of a new slot right before it ensures the route
  (`platform_tunnel_table_release`, then `platform_tunnel_route_ensure`): an apply that dies after
  ensuring a route but before recording its slot leaves that route with no record at all, and the
  next apply, handing the index to a tunnel whose route cannot be installed, would otherwise send its
  clients through the previous owner's tunnel. A kept slot's table is never released: it is carrying
  traffic.
  ```

- Add to **Rebuild triggers**: `- `TUN_DIR_FORCE_REBUILD=1` (`restart`, `restart tunnel`)`.

In `## Key Functions`, replace the `tunnel_apply()` row with `| `tunnel_apply()` | Apply rules from config (idempotent; a rebuild happens in place) |` and add to the internal functions table:

```markdown
| `_tunnel_collect_applied(prev)` | The slot of every tunnel of the new layout: kept from `prev`, or the lowest idx neither layout holds |
| `_tunnel_build_chain(chain)` | `swap_fw_chain`'s build_fn: every client's rules, the failover clients first |
| `_tunnel_slot_release(idx, tunnel)` | Drop the ip rule (ours only) and the table of a slot the layout dropped |
```

In `## Dependencies`, replace the `lib/firewall.sh` line with:

```markdown
From `lib/firewall.sh`: `swap_fw_chain`, `delete_fw_chain`, `ensure_fw_rule`, `sync_fw_rule`, `purge_fw_rules`, `find_fw_rules`, `fw_chain_exists`
```

- [ ] **Step 3: `xray-tproxy.md`**

Replace the `## Usage via CLI` block and the paragraph under it with:

````markdown
## Usage via CLI

```bash
vpn-director.sh status xray       # Show Xray TPROXY status
vpn-director.sh restart xray      # Restart the Xray process and apply TPROXY again in place
vpn-director.sh restart xray-process  # Restart the Xray process only, TPROXY rules kept
vpn-director.sh apply             # Apply all (including TPROXY)
```

`restart xray` restarts the process, then applies the TPROXY rules in place (`tproxy_apply`,
`tproxy_prune`): nothing is stopped, so a client meets a restarting Xray and waits — TPROXY drops
what no socket takes. The Web UI server switch, `/xray` and the wizard use it; its apply also swaps
in `TPROXY_BYPASS`, whose `xray.servers` a switch may have recomputed. `restart xray-process`
restarts the process alone, for a caller whose only change is `config.json` — the subscription
watch trying one server after another.
````

Insert before `## Outbound Generation`:

```markdown
## Applying without a window

`tproxy_apply` flushes nothing a packet is crossing:

- `XRAY_TPROXY` is built as `XRAY_TPROXY_NEW` on every apply (`_tproxy_build_chain`) and swapped in
  by `swap_fw_chain`. Flushing the live chain and refilling it one rule per call left every Xray
  client unproxied — out through the WAN — for the length of the refill, on every apply.
- `TPROXY_BYPASS` is built as `TPROXY_BYPASS_NEW` and swapped in (`ipset swap`).
- `XRAY_CLIENTS` is only added to. `tproxy_prune`, which a full apply runs after `tunnel_apply`,
  swaps in the exact set: a client moving to a tunnel stays proxied until Tunnel Director carries
  it (`packet-flow.md`, "The apply: make before break").
- A name from `advanced.xray.chain` leaves room for `_NEW` (24 characters at most), and so does
  one from `advanced.xray.clients_ipset` or `bypass_ipset` (27).
```

In the Public API table, replace the `tproxy_apply()` and `tproxy_stop()` rows with:

```markdown
| `tproxy_apply()` | The make half of an apply: routing, sets, chain swapped in; clients added, never removed; soft-fail if unavailable |
| `tproxy_prune()` | The break half: `XRAY_CLIENTS` swapped to exactly `xray.clients`; always returns 0 |
| `tproxy_stop()` | Remove chain and routing (an interrupted swap's shadows included) |
```

In the internal functions table, replace the rows of `_tproxy_setup_clients_ipset()`, `_tproxy_setup_bypass_ipset()` and `_tproxy_setup_iptables()` with:

```markdown
| `_tproxy_setup_clients_ipset()` | Create the client ipset and add every effective client (never removes) |
| `_tproxy_shadow_set(name)` | An empty `<name>_NEW` to fill and swap in |
| `_tproxy_swap_set(name)` | `ipset swap` of `<name>_NEW` into `<name>`, then destroy the shadow |
| `_tproxy_setup_bypass_ipset()` | Build `TPROXY_BYPASS_NEW` from the three sources and swap it in |
| `_tproxy_build_chain(chain)` | Fill a fresh chain with the TPROXY rules: 0, 1 (a narrowing RETURN missing), 2 (a bounding RETURN or a target missing) |
| `_tproxy_jump_pos()` | Position of the first jump: 1 |
| `_tproxy_setup_iptables()` | Platform rules, then `XRAY_TPROXY` swapped in with its jumps |
```

In the **Ready marker** paragraph, replace the sentence that begins `The chain flush, rule 1 (`! XRAY_CLIENTS`) or a private-range RETURN that does not go in ends it before the TPROXY targets instead` (through `the rest of the LAN.`) with:

```markdown
Rule 1 (`! XRAY_CLIENTS`), a private-range RETURN or a TPROXY target that does not go in keeps the
rebuilt chain out (`_tproxy_build_chain` returns 2): the chain already in place keeps working and
the marker is withheld. Without those rules the targets would take every LAN client, or traffic to
the router and the rest of the LAN.
```

and replace `A PREROUTING jump that does not go in withholds the marker as well: `sync_fw_rule` returns 1 for an insert the kernel refused.` with:

```markdown
A PREROUTING jump that does not go in withholds the marker as well: `swap_fw_chain` returns 3, and
that interface stays on the old chain until the next apply finishes the swap.
```

- [ ] **Step 4: `shell-conventions.md` and `testing.md`**

In `shell-conventions.md`, add to the `## Firewall Utilities (firewall.sh)` table, after the `sync_fw_rule` row:

```markdown
| `swap_fw_chain <table> <chain> <build_fn> <pos_fn> <jump_match>...` | Rebuild a chain PREROUTING jumps to as `<chain>_NEW` and swap it in; 0 done, 1 done with a rule missing, 2 live chain untouched, 3 cutover unfinished |
```

In the Merlin firewall-start pitfall, replace `refill the chain on every apply (`_tproxy_setup_iptables` flushes and rebuilds `XRAY_TPROXY`)` with `rebuild the chain on every apply (`_tproxy_setup_iptables` builds `XRAY_TPROXY_NEW` and swaps it in)`. Then add a pitfall after that section:

```markdown
### An apply never flushes a live chain or set

**Problem**: `create_fw_chain -f` on a chain the PREROUTING jumps lead to, `ipset flush` on a set a
rule matches, and `tunnel_stop` ahead of a rebuild each opened a window in which packets crossed an
empty chain or set. `XRAY_TPROXY` returned every Xray client to the WAN for the length of its
refill, on every apply; a Tunnel Director rebuild sent every tunnel client there for seconds.

**Solution**: build beside the live object and swap it in. `swap_fw_chain` (`lib/firewall.sh`)
fills `<chain>_NEW` through a callback, inserts its jumps ahead of the old ones, deletes the old
jumps and chain and renames the new chain; `ipset swap` replaces a set's contents in one step. Order
does the rest: add before remove (`tproxy_apply` only adds clients, `tproxy_prune` removes them
after `tunnel_apply`). Only `tproxy_stop` and `tunnel_stop` tear down.

The stateful iptables mock (`use_stateful_iptables`) appends every flush of a chain a rule still
jumps to to `$BATS_IPT_DIR/live_flushes`; a test of an apply asserts that file stays empty.
```

In `testing.md`, add to the Test Helpers table, after the `load_tproxy_module` row:

```markdown
| `use_stateful_iptables` | For the rest of the test, `mocks/stateful/iptables` ahead of the stateless mock: chains and rules kept under `$BATS_IPT_DIR`, a flush of a live chain appended to `$BATS_IPT_DIR/live_flushes`; call it after the `load_*` helper |
```

and to the mocks list, after `- **iptables/ip6tables**: Tracks rule operations`:

```markdown
- **stateful/iptables**: Remembers chains and rules per table under `$BATS_IPT_DIR` (`-S`, `-N`, `-F`, `-X`, `-E`, `-A`, `-I`, `-D`, `-C`); records a flush of a chain a rule still jumps to. Turned on per test by `use_stateful_iptables`
```

- [ ] **Step 5: `webui.md` and `telegram-bot.md`**

In `webui.md`, add to the API table, after the `/api/clients/pause`, `/api/clients/resume` row:

```markdown
| POST | `/api/clients/route` | Move a client: `{ip, route}`, the route checked as for an add; one config update (`vpnconfig.MoveClient`) and one apply; 404 when no route holds the address, 200 with no write and no apply when the route already holds it alone |
```

and append to the paragraph that begins `Client and exclusion mutations go through `updateAndApply``:

```markdown
A move is one such change: its apply takes the client off its old route only once the new one
carries it (`packet-flow.md`), where a delete and an add left it on the WAN in between.
```

In `telegram-bot.md`, add to the Commands table, after the `/cancel` row:

```markdown
| `/clients` | `ClientsHandler.HandleClients` | LAN clients: pause, resume, move (🔀) and remove, and add |
```

Insert before `## Server switch (`/xray`)`:

```markdown
## Clients (`/clients`)

Each client gets ⏸/▶, 🔀 (`clients:move:<ip>`) and 🗑; an address no route can take (an IPv6 entry
an older build saved) gets no 🔀. 🔀 replaces the list with the routes (`routeChoices`, which the
Add flow's keyboard uses too): xray, the platform's tunnels with their description and "(down)",
the config's tunnels the platform does not list ("(unknown)" when the platform answered); ✓ on the
current route; « Back (`clients:rm_no`). A route button (`clients:to:<route>:<ip>`) runs
`vpnconfig.MoveClient` under the config lock and one `Apply`, and the message shows the list again.
A tunnel that is down asks first — "Move anyway" (`clients:toyes:<route>:<ip>`) or Cancel. A button
whose client is gone, or whose route the router no longer has, redraws the list.
```

In the watch paragraph, replace `it still records the config hash so the next apply does not `tunnel_stop` (which would take TUN_DIR down for every client).` with `it still records the config hash, so the next apply does not rebuild for nothing.` and replace `The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries, and a full `restart xray` would take the TPROXY jump away and put it back each time, with the Xray clients leaving through the WAN in between.` with `The walk restarts only the Xray process (`restart xray-process`): it writes a config.json per server it tries and changes nothing else, and `restart xray` would apply the TPROXY rules again each time for nothing.`

- [ ] **Step 6: `README.md`, `README.ru.md`, `CLAUDE.md`**

`README.md`:
- In `## Commands`: `restart             # Restart all` → `restart             # Rebuild all in place (nothing stopped)`; `restart xray        # Restart Xray TPROXY only` → `restart xray        # Restart Xray, TPROXY applied again in place`.
- In the Web UI features table, the Clients row: `| **Clients** | LAN client routing: add, change the route in place, pause/resume, delete |`.
- In the bot commands table: `| `/clients` | Manage VPN clients: move between routes, pause, remove |`.
- Insert before `### Country IPSets`:

  ```markdown
  ### Changing a client's route

  Pick another route for a client in the Web UI (the Route column) or in the bot (`/clients`, 🔀).
  It is one change and one apply, and the apply moves the client make-before-break: the client stays
  on its old route until the new one carries it, so none of its traffic leaves through the WAN in
  between. Every apply works that way — chains and sets are built beside the live ones and swapped
  in — and so does a server switch, which restarts Xray with its rules in place.

  What remains: the firmware's own firewall rebuilds (a firewall restart on Merlin, an NDM rebuild on
  KeeneticOS) empty the chains until the hook applies them again, and a tunnel that is down sends its
  clients through the WAN (on Merlin, the VPN client's killswitch prevents that). The Web UI and the
  bot ask before they move a client to a tunnel that is down.
  ```

`README.ru.md`:
- `restart             # Перезапустить всё` → `restart             # Пересобрать всё на месте, без остановки`; `restart xray        # Перезапустить только Xray TPROXY` → `restart xray        # Перезапустить Xray, правила TPROXY применяются на месте`.
- The Clients row: `| **Clients** | Маршруты LAN-клиентов: добавление, смена маршрута на месте, пауза/возобновление, удаление |`.
- `/clients`: `| `/clients` | Управление VPN-клиентами: смена маршрута, пауза, удаление |`.
- Insert before `### Country IPSets`:

  ```markdown
  ### Смена маршрута клиента

  Маршрут клиента меняется в Web UI (колонка Route) или в боте (`/clients`, кнопка 🔀). Это одно
  изменение и один apply, и apply переносит клиента по принципу make-before-break: клиент остаётся на
  старом маршруте, пока его не подхватит новый, поэтому в промежутке ни один его пакет не уходит в WAN.
  Так работает любой apply — цепочки и наборы собираются рядом с действующими и подменяются, — и так же
  переключается сервер: Xray перезапускается, а его правила остаются на месте.

  Что остаётся: собственные пересборки firewall прошивки (рестарт firewall на Merlin, пересборка NDM
  на KeeneticOS) очищают цепочки, пока хук не применит их заново, а упавший туннель отправляет своих
  клиентов в WAN (на Merlin этого не будет, если у VPN-клиента прошивки включён killswitch). Web UI и
  бот спрашивают подтверждение, прежде чем перенести клиента на упавший туннель.
  ```

`CLAUDE.md`, in `## Commands`: `restart             # Restart all` → `restart             # Rebuild all in place, nothing stopped`; `restart xray        # Restart Xray TPROXY only` → `restart xray        # Restart the Xray process, TPROXY applied again in place`.

- [ ] **Step 7: Check the docs**

Run: `grep -rn "Order of tunnels in JSON determines\|flushes and rebuilds .XRAY_TPROXY\|Restart (stop + apply)" .claude/rules/ README.md README.ru.md CLAUDE.md router/opt/`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add .claude/rules/ README.md README.ru.md CLAUDE.md
git commit -m "docs: route changes and applies without a window"
```

---

### Task 10: Final verification and the device check

**Files:** none new; `docs/superpowers/` leaves the branch in Step 6.

- [ ] **Step 1: The Go suite, vet and format**

Run (through `claude-forge:build-runner`): `cd server && go vet ./... && go test ./... -count=1 && go test -race ./internal/webapi/ ./internal/handler/ ./internal/vpnconfig/ -count=1 && gofmt -l .`
Expected: PASS; `gofmt -l` lists no file this branch touched.

- [ ] **Step 2: Leftovers**

Run:

```bash
grep -rn "_tunnel_sync_jumps\|create_fw_chain -q -f mangle \"\$TUN_DIR_CHAIN\"\|ipset flush \"\$XRAY_CLIENTS_IPSET\"" router/opt/
grep -rn "create_fw_chain -f mangle \"\$XRAY_CHAIN\"" router/opt/
```

Expected: no output.

- [ ] **Step 3: The shell suites and shellcheck**

Run in the background and read the counts from the log: `cd router/test && bats -r . > "$TMPDIR/bats.log" 2>&1; tail -5 "$TMPDIR/bats.log"` (with `TMPDIR` the session's scratchpad directory).
Expected: `0 failures`.
Run: `shellcheck router/opt/vpn-director/*.sh router/opt/vpn-director/lib/*.sh`
Expected: nothing new against `master`.

- [ ] **Step 4: Builds**

Run (through `claude-forge:build-runner`): `cd web && npm run build`, then from the repository root `make build-webui && make build-all`.
Expected: both daemons build for every target; the SPA is embedded.

- [ ] **Step 5: The Web UI in dev mode**

Repeat Task 7 Step 4 against the embedded build (`cd server && go run ./cmd/webui --dev`). Stop the server.

- [ ] **Step 6: Remove the plan documents from the branch**

The owner's rule: `docs/superpowers/` must not be in the pull request's diff; the documents stay in the branch history.

```bash
git rm -r docs/superpowers/specs/2026-09-25-leak-free-route-change-design.md \
          docs/superpowers/plans/2026-09-25-leak-free-route-change.md
git commit -m "chore: remove superpowers docs from feature branch"
```

(Only these two files are tracked under `docs/superpowers/` on this branch; the other files there are the owner's untracked notes and stay untouched.)

- [ ] **Step 7: The device check — only with the owner's explicit permission**

Ask the owner first. With a yes, on the RT-AX86U (Merlin) and on the KN-4521 (Keenetic), spec 12:
1. Install the build.
2. Check the one new iptables operation on the firmware's iptables: `iptables -t mangle -N VPD_T; iptables -t mangle -I PREROUTING 1 -i br0 -p icmp -j VPD_T; iptables -t mangle -E VPD_T VPD_U; iptables -t mangle -S PREROUTING | grep VPD_U` prints the jump under the new name; then remove it: `iptables -t mangle -D PREROUTING -i br0 -p icmp -j VPD_U; iptables -t mangle -X VPD_U`.
3. Pick a LAN test client `<client>` and put a counter without a target at the top of `FORWARD`: `iptables -I FORWARD 1 -s <client> -d 1.1.1.1 -o <wan>` (`<wan>` from `vpn-director.sh platform`; add it again after a firmware rebuild of the table). On the client, loop `curl -m 2 https://1.1.1.1` and a DNS query to `1.1.1.1` every 100 ms — TCP and UDP, since ICMP from Xray clients goes direct by design.
4. Move the client from xray to a tunnel, back to xray, and between two tunnels; switch the server in the Web UI; run `restart`, `restart xray` and an `apply` that changes nothing. After each: `iptables -L FORWARD 1 -v -n` shows 0 packets on the counter; `iptables -t mangle -S PREROUTING` lists the Xray jumps, then the `TUN_DIR` jumps (behind the firmware's iface-mark rules on Merlin); `iptables -t mangle -S | grep _NEW` and `ipset list -n | grep _NEW` print nothing; `cat /tmp/tunnel_director/tun_dir_tables` keeps the idx of every tunnel that kept clients, and `ip rule` shows their rules untouched.
5. On a router still running the release before this change, the same moves and switch make the counter grow — the check catches a leak.
6. Remove the counter: `iptables -D FORWARD -s <client> -d 1.1.1.1 -o <wan>`.

- [ ] **Step 8: Ask before pushing**

Report the results and ask the owner whether to push `feature/leak-free-route-change` and open the pull request.
