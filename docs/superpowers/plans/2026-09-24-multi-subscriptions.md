# Multiple Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep up to ten subscriptions on the router, each in its own file; let the user run any server of any of them; and have the subscription watch look for a live server across all of them when the running one dies.

**Architecture:** A subscription is `<data_dir>/subscriptions/<id>.json`: its link, its status and its servers. `vpnconfig` owns the file format and five locked operations (add, refresh, record an error, rename, delete) written against a `ConfigUpdate` and a `SubscriptionFiles`, so the daemons, the watch and the tests share one implementation. `service` wraps them for the Web UI and the bot together with the one download path they share, and `lib/substore.sh` is their twin for the shell. The watch downloads every subscription at once and walks the servers in the hybrid order — the chosen server and two more of its subscription, then one server of each subscription in turn — skipping any server whose outbound is identical to one already tried.

**Tech Stack:** Go 1.25 (standard library only), Vue 3 + TypeScript (Vite, vue-tsc), bash + jq 1.8 without oniguruma + BusyBox, bats.

**Spec:** `docs/superpowers/specs/2026-09-24-multi-subscriptions-design.md`. Read it before any task; "spec 5.3" below means its section 5.3.

## Global Constraints

- Go module `github.com/zinin/vpn-director/server`. No new Go or npm dependency.
- A subscription file is `<data_dir>/subscriptions/<id>.json`, mode 0600, in a directory of mode 0700. An id is 8 lowercase hex digits; a file named anything else is not a subscription.
- At most 10 subscriptions (`vpnconfig.MaxSubscriptions`).
- A name is 1 to 32 code points once the spaces (U+0020 only) around it are trimmed, has no control character (C0, DEL, C1), and is unique under ASCII case folding. The default is the link's host name (a file's base name in the shell); a taken default becomes `-2`, `-3`, …, cut so that the suffix fits in 32.
- Times in the files are RFC 3339, UTC, whole seconds: `2026-09-24T18:00:00Z`.
- Every write to `subscriptions/` happens under the config lock `.vpn-director.json.lock`: in Go inside `UpdateVPNConfig` (or the watch's `update`), in the shell with flock on FD 9.
- `OwnFirst = 3`. The walk's rotation starts with the subscription after the chosen server's.
- No subscription link — it carries a token — goes into any message, log line, API answer, fixture, commit message or test output. Show the host at most. Fixtures are synthetic: `example.*` hosts and documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24).
- Shell: a sourced library starts with `#!/usr/bin/env bash`; use `[[ ]]`; Entware's jq has no regex builtins, so `test`, `match`, `capture`, `scan`, `split/2`, `splits`, `sub` and `gsub` never appear in `lib/*.sh`. `lib/substore.sh` stands alone — `configure.sh` does not source `lib/common.sh`, so the library must not call `log`.
- Bot texts keep the language each handler uses today: `/xray` answers in Russian, everything else in English.
- The owner's working rules: run Go and npm build and test commands through the `claude-forge:build-runner` agent; run bats and shellcheck directly; the full bats suite takes minutes — run it in the background and read the counts from its log. Never write to the local memory directory. Commit each task on `feature/multi-subscriptions`; do not push without asking.

## Review Focus

1. **One server name in two subscriptions.** Both providers can call a server `Germany-1`. Selecting it in the Web UI, `/xray` or the wizard must run the one in the chosen subscription; the Active badge and the `✓` mark only that one; the walk's fallback by name never crosses subscriptions. Tests: Task 1 (`RecordWalkedServer`), Task 5 (select), Task 8 (fingerprint), Task 9 (wizard pick), Task 10 (`chosenIndex`, `walkOrder`).
2. **A subscription file edited or broken by hand.** Invalid JSON, an `id` that is not the file's name, a stray temp or backup file: every reader skips it (the daemons with a WARN), and nobody fails because of it. Tests: Task 1, Task 13.
3. **State left by the previous release.** `xray.subscription_url` in the config, `servers.json` on disk, `active_server` without `subscription`: nothing crashes, no server shows as active, the watch stays unarmed until a subscription exists, the first subscription write removes `servers.json` (Go and shell), and the next daemon write of the config drops the old key. Tests: Task 3, Task 11, Task 12, Task 14.
4. **A subscription deleted while its refresh is downloading.** From the Web UI, the bot, the watch's wave or the shell, the publication is refused and the file is not recreated. Tests: Task 2, Task 11, Task 14.
5. **Names that differ only in case, and names outside ASCII.** `Alpha` and `alpha` collide in Go and in the shell; `Бета` and `бета` do not (ASCII folding only); a default host longer than 32 characters is cut with its suffix inside the limit — the same answers on both sides. Tests: Task 1 and Task 13, on the same inputs.

## File Structure

| Path | Responsibility |
|---|---|
| `server/internal/vpnconfig/substore.go` (new) | `Subscription`, the file store (load, save, delete), ids, names, flattening, the legacy `servers.json` |
| `server/internal/vpnconfig/subops.go` (new) | The locked operations: add, refresh, record an error, rename, delete |
| `server/internal/vpnconfig/vpnconfig.go` | `Server.Subscription`, `ActiveServer.Subscription`, `RecordWalkedServer`; later loses `SubscriptionURL` and the `servers.json` helpers |
| `server/internal/vpnconfig/failover.go` | `Armed(cfg, subscriptions)` |
| `server/internal/vpnconfig/publish.go` | Keeps only `ServersSaved`; the single-list publication goes |
| `server/internal/service/config.go`, `interfaces.go` | `ConfigStore` gains the subscription files; `LoadServers` flattens them |
| `server/internal/service/subscriptions.go` (new) | The daemons' download path and the operations they call |
| `server/internal/service/publish.go` | Deleted at the end |
| `server/internal/webapi/handler_subscriptions.go` (new) | `/api/subscriptions` routes |
| `server/internal/webapi/handler_servers.go` | Grouped list, selection by subscription; the import route goes |
| `server/internal/handler/subs.go` (new) | `/subs` with refresh, rename and delete |
| `server/internal/handler/import.go`, `xray.go`, `servers.go` | `/import`, the two-step `/xray`, `/servers` by subscription |
| `server/internal/wizard/server.go`, `state.go` | The two-step server choice of `/configure` |
| `server/internal/bot/router.go`, `bot.go` | `/subs`, `/cancel`, the text dispatch, the watch wiring |
| `server/internal/subwatch/order.go` (new) | The hybrid walk order and the dedupe key |
| `server/internal/subwatch/watch.go`, `return.go`, `reach.go` | The wave over all subscriptions, the guards, the messages |
| `web/src/types.ts`, `api.ts`, `components/ServersTab.vue`, `components/StatusTab.vue` | The Subscriptions card, servers grouped by subscription, the running server's label |
| `router/opt/vpn-director/lib/substore.sh` (new) | The shell twin of the store |
| `router/opt/vpn-director/import_server_list.sh` | The subscription menu |
| `router/opt/vpn-director/configure.sh` | The two-step server choice |
| `router/files.manifest` | Ships `lib/substore.sh` |
| `testdata/substore/*.json` (new) | Synthetic subscription files both test suites read |

Task order keeps every commit building and its tests green. Between Task 3 and Task 12 the Go readers already read the subscription files while some writers still write `servers.json`; that intermediate state never ships.

---

### Task 1: The subscription store in `vpnconfig`

✅ Done — see commit(s): `c0e8cfe`

---

### Task 2: The locked subscription operations

✅ Done — see commit(s): `d7ef560`

---

### Task 3: `ConfigStore` reads and writes the subscription files

✅ Done — see commit(s): `ac8d2f7`

---

### Task 4: The daemons' download path and subscription operations

✅ Done — see commit(s): `95b56b8`, `25a9ffd`

---

### Task 5: Web UI API — `/api/subscriptions`, servers by subscription

✅ Done — see commit(s): `d8cca7d`, `6e8be9b`, `1d06ca0`

---

### Task 6: Web UI pages — subscriptions and grouped servers

✅ Done — see commit(s): `f256b08`

---

### Task 7: Bot — `/import` for subscriptions and the new `/subs`

✅ Done — see commit(s): `5ef9f6a`

---

### Task 8: Bot — `/xray` in two steps, `/servers` by subscription

✅ Done — see commit(s): `ed73bfd`

---

### Task 9: Wizard step 1 — subscription, then server

✅ Done — see commit(s): `b6b7f0a`

---

### Task 10: The walk's order and the dedupe key

✅ Done — see commit(s): `0f602c6`

---

### Task 11: The watch over every subscription

✅ Done — see commit(s): `49c1668`, `932578c`

---

### Task 12: Remove the single link and `servers.json` from the Go code

✅ Done — see commit(s): `86d7331`

---

### Task 13: `lib/substore.sh` — the shell twin of the store

**Files:**
- Create: `router/opt/vpn-director/lib/substore.sh`
- Create: `router/test/unit/substore.bats`
- Modify: `router/files.manifest`

**Interfaces:**
- Consumes: `testdata/substore/*.json` (Task 1).
- Produces (all self-contained, no `lib/common.sh`):
  - `SUBSTORE_MAX=10`, `SUBSTORE_NAME_MAX=32`
  - `substore_dir <data_dir>`, `substore_valid_id <id>`, `substore_list <dir>` (a JSON array, ordered), `substore_new_id <dir>`
  - `substore_clean_name <name>` (prints the name or fails with the reason on stderr), `substore_name_taken <subs_json> <name> [except_id]`, `substore_default_name <subs_json> <base>`
  - `substore_ips <subs_json>`, `substore_write <dir> <subscription_json>`, `substore_delete <dir> <id>`
  - `substore_lock <config>`, `substore_unlock`, `substore_sync_config <config> <subs_json> [jq filter]`

- [ ] **Step 1: Write the failing tests**

Create `router/test/unit/substore.bats`:

```bash
#!/usr/bin/env bats
load '../test_helper'

# testdata/substore is read by the Go store too
# (server/internal/vpnconfig/substore_test.go): both list the same
# subscriptions in the same order, and apply the same name rules.
FIXTURES="$PROJECT_ROOT/../testdata/substore"

setup() {
    source "$LIB_DIR/substore.sh"
}

@test "substore_list: the shared fixtures, in the order the daemons list them" {
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$FIXTURES' | jq -c '[.[] | [.id, .name]]'"
    assert_success
    assert_output '[["1b2c3d4e","Beta"],["0a1b2c3d","Alpha"],["2c3d4e5f","Gamma"]]'
}

@test "substore_list: no directory is no subscription" {
    run substore_list "$BATS_TEST_TMPDIR/none"
    assert_success
    assert_output '[]'
}

# A temp file of an atomic write, a backup, a directory and a file whose id is
# not its name are no subscription; a broken file is skipped with a warning.
@test "substore_list: skips what is no subscription" {
    local dir="$BATS_TEST_TMPDIR/subscriptions" good
    mkdir -p "$dir/3d4e5f6a.json"
    good='{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    printf '%s' "$good" > "$dir/0a1b2c3d.json"
    printf '%s' "$good" > "$dir/.0a1b2c3d.json.tmp-123"
    printf '%s' "$good" > "$dir/0a1b2c3d.json.Ab12Cd"
    printf '%s' "$good" > "$dir/backup.json"
    printf '%s' "${good/0a1b2c3d/ffffffff}" > "$dir/2c3d4e5f.json"
    printf '{"id":"1b2c3d4e",' > "$dir/1b2c3d4e.json"

    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' 2>'$BATS_TEST_TMPDIR/err' | jq -c '[.[].id]'"

    assert_success
    assert_output '["0a1b2c3d"]'
    run cat "$BATS_TEST_TMPDIR/err"
    assert_output --partial "Skipping subscription file 1b2c3d4e.json"
    assert_output --partial "Skipping subscription file 2c3d4e5f.json"
}

@test "substore_valid_id: 8 lowercase hex digits" {
    substore_valid_id 0a1b2c3d
    ! substore_valid_id 0A1B2C3D
    ! substore_valid_id 0a1b2c3
    ! substore_valid_id 0a1b2c3d0
    ! substore_valid_id ../0a1b2
}

@test "substore_new_id: a valid id no file has" {
    local id
    id=$(substore_new_id "$BATS_TEST_TMPDIR")
    substore_valid_id "$id"
    [[ ! -e $BATS_TEST_TMPDIR/$id.json ]]
}

# The same table as TestCleanSubscriptionName in Go.
@test "substore_clean_name: the daemons' rules" {
    local long
    long=$(printf 'я%.0s' $(seq 1 32))
    [[ $(substore_clean_name '  Alpha  ') == "Alpha" ]]
    [[ $(substore_clean_name 'Бета VPN') == "Бета VPN" ]]
    [[ $(substore_clean_name "$long") == "$long" ]]
    run substore_clean_name "${long}я"
    assert_failure
    assert_output --partial "longer than 32"
    run substore_clean_name '   '
    assert_failure
    run substore_clean_name $'Al\tpha'
    assert_failure
    run substore_clean_name $'\tAlpha'
    assert_failure
    run substore_clean_name $'Al\xc2\x85pha'   # U+0085, a C1 control, as UTF-8 bytes
    assert_failure
    run substore_clean_name $'Al\x7fpha'
    assert_failure
}

# The same table as TestSubscriptionNameTaken_FoldsASCIIOnly in Go.
@test "substore_name_taken: ASCII case folding only" {
    local subs='[{"id":"0a1b2c3d","name":"Beta"},{"id":"1b2c3d4e","name":"Бета"}]'
    substore_name_taken "$subs" beta
    substore_name_taken "$subs" BETA
    ! substore_name_taken "$subs" beta 0a1b2c3d
    ! substore_name_taken "$subs" бета
    substore_name_taken "$subs" Бета
    ! substore_name_taken "$subs" Gamma
}

# The same table as TestDefaultSubscriptionName in Go.
@test "substore_default_name: the daemons' choice" {
    local host40 a31
    host40="$(printf 'a%.0s' $(seq 1 36)).com"
    a31=$(printf 'a%.0s' $(seq 1 31))
    [[ $(substore_default_name '[]' sub.example.com) == "sub.example.com" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"Sub.Example.com"}]' sub.example.com) == "sub.example.com-2" ]]
    [[ $(substore_default_name '[{"id":"00000000","name":"sub.example.com"},{"id":"00000001","name":"sub.example.com-2"}]' sub.example.com) == "sub.example.com-3" ]]
    [[ $(substore_default_name '[]' "$host40") == "${host40:0:32}" ]]
    [[ $(substore_default_name "[{\"id\":\"00000000\",\"name\":\"${host40:0:32}\"}]" "$host40") == "${host40:0:30}-2" ]]
    [[ $(substore_default_name '[]' "$a31 bc") == "$a31" ]]
    [[ $(substore_default_name '[]' $'  list\x07.txt ') == "list.txt" ]]
    [[ $(substore_default_name '[]' '') == "subscription" ]]
    [[ $(substore_default_name '[]' $' \x01 ') == "subscription" ]]
}

@test "substore_ips: every address of every server, sorted, once" {
    run substore_ips "$(substore_list "$FIXTURES")"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"]'
}

@test "substore_write: a file only its owner reads, in a directory only its owner opens" {
    local dir="$BATS_TEST_TMPDIR/data/subscriptions"
    substore_write "$dir" '{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}'
    run stat -c %a "$dir" "$dir/0a1b2c3d.json"
    assert_output $'700\n600'
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$dir' | jq -r '.[0].name'"
    assert_output "Alpha"
    run ls -A "$dir"
    assert_output "0a1b2c3d.json"
}

@test "substore_write: refuses an id the store would not read" {
    run substore_write "$BATS_TEST_TMPDIR" '{"id":"../x","name":"X"}'
    assert_failure
}

@test "substore_delete: removes the file of that id only" {
    local dir="$BATS_TEST_TMPDIR/s"
    mkdir -p "$dir"
    printf '{}' > "$dir/0a1b2c3d.json"
    printf '{}' > "$dir/1b2c3d4e.json"
    substore_delete "$dir" 0a1b2c3d
    run ls -A "$dir"
    assert_output "1b2c3d4e.json"
}

@test "substore_lock: waits for another holder, then gives up" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '{}' > "$config"
    flock "$BATS_TEST_TMPDIR/.vpn-director.json.lock" sleep 30 3>&- &
    local holder=$!
    sleep 0.5
    VPD_CONFIG_LOCK_WAIT=1 run substore_lock "$config"
    pkill -P "$holder" 2>/dev/null || true
    kill "$holder" 2>/dev/null || true
    assert_failure
}

@test "substore_sync_config: xray.servers in step, the old link gone, the filter applied" {
    local config="$BATS_TEST_TMPDIR/vpn-director.json"
    printf '%s' '{"webui":{"jwt_secret":"s"},"xray":{"clients":["192.168.1.8"],"servers":["9.9.9.9"],"subscription_url":"https://old.example/s/t","preferred_server":{"subscription":"0a1b2c3d","name":"Oslo"}}}' > "$config"

    substore_sync_config "$config" "$(substore_list "$FIXTURES")" 'if (.xray.preferred_server.subscription // "") == "0a1b2c3d" then del(.xray.preferred_server) else . end'

    run jq -c '[.xray.servers, (.xray | has("subscription_url")), (.xray | has("preferred_server")), .webui.jwt_secret, .xray.clients]' "$config"
    assert_output '[["192.0.2.10","192.0.2.11","198.51.100.20","203.0.113.30"],false,false,"s",["192.168.1.8"]]'
    run stat -c %a "$config"
    assert_output "600"
}

@test "substore_sync_config: no config yet is nothing to keep in step" {
    run substore_sync_config "$BATS_TEST_TMPDIR/none.json" '[]'
    assert_success
    [[ ! -e $BATS_TEST_TMPDIR/none.json ]]
}

# Entware's jq is built without oniguruma: a regex builtin works on a
# workstation and fails on the router.
@test "lib/substore.sh: its jq uses no regex builtin" {
    run grep -nE '(^|[^a-zA-Z_])(test|match|capture|scan|splits|sub|gsub)\(|split\([^)]*;' "$LIB_DIR/substore.sh"
    assert_failure
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats unit/substore.bats`
Expected: FAIL — `lib/substore.sh: No such file or directory`.

- [ ] **Step 3: Write the library**

Create `router/opt/vpn-director/lib/substore.sh`:

```bash
#!/usr/bin/env bash
# shellcheck shell=bash

###############################################################################
# substore.sh - the subscription files: <data_dir>/subscriptions/<id>.json, one
# per subscription, holding its link, its status and its servers. The Go
# daemons read and write the same files (server/internal/vpnconfig/substore.go)
# under the same lock, and apply the same rules to ids and names.
#
# Self-contained: configure.sh sources it without lib/common.sh, so nothing
# here calls log(); warnings go to stderr. Entware's jq is built without
# oniguruma, so nothing here uses a regex builtin.
###############################################################################

# The most subscriptions a router keeps, and the longest name in characters.
SUBSTORE_MAX=10
SUBSTORE_NAME_MAX=32

# substore_dir <data_dir> - where the data directory keeps the subscriptions.
substore_dir() {
    printf '%s/subscriptions\n' "$1"
}

# substore_valid_id <id> - an id of 8 lowercase hex digits, the only names the
# store reads or writes.
substore_valid_id() {
    [[ ${#1} -eq 8 && $1 != *[!0123456789abcdef]* ]]
}

# substore_list <dir> - every subscription as one JSON array, ordered by added,
# then by id, as the daemons order them. A file named anything but
# <8 hex digits>.json is no subscription - a temp file of an atomic write
# among them; one that does not parse, or whose id is not its name, is skipped
# with a warning.
substore_list() {
    local dir=$1 file name id sub
    local -a found=()
    if [[ -d $dir ]]; then
        for file in "$dir"/*.json; do
            [[ -f $file ]] || continue
            name=${file##*/}
            id=${name%.json}
            substore_valid_id "$id" || continue
            if ! sub=$(jq -ce --arg id "$id" 'select(type == "object" and .id == $id)' "$file" 2>/dev/null) || [[ -z $sub ]]; then
                printf 'Skipping subscription file %s: it does not parse, or its id is not its name\n' "$name" >&2
                continue
            fi
            found+=("$sub")
        done
    fi
    if [[ ${#found[@]} -eq 0 ]]; then
        printf '[]\n'
        return 0
    fi
    printf '%s\n' "${found[@]}" | jq -cs 'sort_by(.added, .id)'
}

# substore_new_id <dir> - a random id no file in <dir> has yet.
substore_new_id() {
    local id
    while :; do
        id=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
        if substore_valid_id "$id" && [[ ! -e $1/$id.json ]]; then
            break
        fi
    done
    printf '%s\n' "$id"
}

# substore_clean_name <name> - <name> without the spaces around it, when what is
# left is 1 to 32 characters and none of them a control character (C0, DEL,
# C1); otherwise it fails with the reason on stderr.
# vpnconfig.CleanSubscriptionName applies the same rules.
substore_clean_name() {
    local name=$1 verdict
    name=${name#"${name%%[! ]*}"}
    name=${name%"${name##*[! ]}"}
    verdict=$(jq -rn --arg n "$name" --argjson max "$SUBSTORE_NAME_MAX" '
        ($n | explode) as $c
        | if ($c | length) == 0 then "empty"
          elif ($c | length) > $max then "long"
          elif ($c | any(. < 32 or . == 127 or (. >= 128 and . <= 159))) then "control"
          else "ok" end')
    case $verdict in
        ok) printf '%s\n' "$name" ;;
        empty) printf 'The name is empty\n' >&2; return 1 ;;
        long) printf 'The name is longer than %d characters\n' "$SUBSTORE_NAME_MAX" >&2; return 1 ;;
        *) printf 'The name has a control character\n' >&2; return 1 ;;
    esac
}

# substore_name_taken <subs_json> <name> [except_id] - whether a subscription
# other than except_id has <name> under ASCII case folding: jq's
# ascii_downcase, the fold the daemons apply too.
substore_name_taken() {
    jq -e --arg n "$2" --arg except "${3:-}" \
        'any(.[]; .id != $except and ((.name | ascii_downcase) == ($n | ascii_downcase)))' <<< "$1" >/dev/null
}

# substore_default_name <subs_json> <base> - <base> (a link's host, a file's
# name) cut to 32 characters, or <base>-2, <base>-3 and so on, cut so the suffix
# fits, whichever no subscription has yet. Control characters and the spaces
# around <base> go first; an empty <base> is "subscription".
# vpnconfig.DefaultSubscriptionName makes the same choice.
substore_default_name() {
    jq -rn --argjson subs "$1" --arg base "$2" --argjson max "$SUBSTORE_NAME_MAX" '
        def trim_spaces: explode
            | until(length == 0 or .[0] != 32; .[1:])
            | until(length == 0 or .[-1] != 32; .[:-1])
            | implode;
        ($base | explode | map(select(. >= 32 and (. < 127 or . > 159))) | implode | trim_spaces) as $b0
        | (if $b0 == "" then "subscription" else $b0 end) as $b
        | [$subs[].name | ascii_downcase] as $taken
        | first(range(1; $max + 100) as $n
            | (if $n == 1 then "" else "-\($n)" end) as $suffix
            | (($b | .[0:($max - ($suffix | length))]) | trim_spaces) + $suffix
            | select(. as $cand | ($taken | any(. == ($cand | ascii_downcase))) | not))'
}

# substore_ips <subs_json> - xray.servers for these subscriptions: every address
# of every server, sorted, each once - the set TPROXY_BYPASS takes.
substore_ips() {
    jq -c '[.[].servers[]?.ips[]? | select(. != "")] | unique' <<< "$1"
}

# substore_write <dir> <subscription_json> - writes <dir>/<id>.json: a temp file
# beside it, mode 600 - it holds the link and every server's credentials - then
# a rename, so a reader sees the old file or the new one. The caller holds the
# config lock.
substore_write() {
    local dir=$1 sub=$2 id tmp
    id=$(jq -r '.id' <<< "$sub")
    if ! substore_valid_id "$id"; then
        printf 'Invalid subscription id: %s\n' "$id" >&2
        return 1
    fi
    mkdir -p "$dir"
    chmod 700 "$dir"
    tmp=$(mktemp "$dir/$id.json.XXXXXX")
    if ! jq . <<< "$sub" > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod 600 "$tmp"
    mv -f "$tmp" "$dir/$id.json"
}

# substore_delete <dir> <id> - removes <dir>/<id>.json. The caller holds the lock.
substore_delete() {
    substore_valid_id "$2" || return 1
    rm -f "$1/$2.json"
}

# substore_lock <config> - takes the lock the daemons and configure.sh take
# around vpn-director.json, on FD 9, waiting up to VPD_CONFIG_LOCK_WAIT seconds
# (30). BusyBox flock has no -w, hence the loop.
substore_lock() {
    local waited=0
    exec 9>"${1%/*}/.${1##*/}.lock"
    until flock -n 9; do
        if [[ $waited -ge ${VPD_CONFIG_LOCK_WAIT:-30} ]]; then
            exec 9>&-
            return 1
        fi
        [[ $waited -eq 0 ]] && printf 'Waiting for the config lock...\n' >&2
        sleep 1
        waited=$((waited + 1))
    done
}

# substore_unlock - releases what substore_lock took.
substore_unlock() {
    flock -u 9
    exec 9>&-
}

# substore_sync_config <config> <subs_json> [jq filter] - under the lock the
# caller holds: xray.servers for <subs_json>, the single link of earlier
# releases gone, and <filter> on top. With no config yet - before configure.sh
# has run - there is nothing to keep in step.
substore_sync_config() {
    local config=$1 subs=$2 filter=${3:-.} ips tmp
    [[ -f $config ]] || return 0
    ips=$(substore_ips "$subs")
    tmp=$(mktemp "$config.XXXXXX")
    if ! jq --argjson ips "$ips" ".xray.servers = \$ips | del(.xray.subscription_url) | $filter" "$config" > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod 600 "$tmp"
    mv -f "$tmp" "$config"
}
```

In `router/files.manifest`, after the `lib/subscription.sh` line, add:

```
common   router/opt/vpn-director/lib/substore.sh
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats unit/substore.bats && shellcheck ../opt/vpn-director/lib/substore.sh`
Expected: every test passes; shellcheck prints nothing.

Then the manifest test of the updater, which fails for a file under `router/` the manifest does not list:
Run: `cd server && go test ./internal/updater/ -run Manifest`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/lib/substore.sh router/test/unit/substore.bats router/files.manifest
git commit -m "feat(shell): lib/substore.sh, the subscription files for the shell"
```

---

### Task 14: `import_server_list.sh` — the subscription menu

**Files:**
- Modify: `router/opt/vpn-director/import_server_list.sh`
- Modify: `router/test/import_server_list.bats`

**Interfaces:**
- Consumes: Task 13's `substore_*`; the existing `lib/subscription.sh`, `resolve_ip`, `log`, `tmp_file`.
- Produces: the menu of spec 8.1. Functions the tests call: `step_get_subscription` (prompts, then `fetch_subscription`), `fetch_subscription <link or path>`, `step_parse_servers` (fills `$SERVERS_TMP`, reusing a path the caller set), `fail <message>`, `run_action <function> [args]` (sets `ACTION_RC`), `publish_add`, `refresh_one`, `publish_refresh`, `record_refresh_error`, `menu`.

The shell refreshes one subscription after another; the daemons refresh in parallel. A menu action runs in a subshell, so its `exit 1` ends the action and not the menu; a first run with no subscription ends the script with the action's status, as today's import does.

- [ ] **Step 1: Write the failing tests**

In `router/test/import_server_list.bats`, keep the `step_parse_servers` and `step_get_subscription` tests of the first section and the helpers `load_import_into`, `write_list_file`, `write_oversized_list`, `serve_list_file`, `hold_config_lock` and `release_config_lock`. Replace `run_import` with one that answers the prompts a line per argument:

```bash
# run_import runs import_server_list.sh the way a user does: each argument is
# the answer to one prompt, and the end of the input quits the menu.
run_import() {
    run env IMPORT_TEST_MODE=0 bash "$SCRIPTS_DIR/import_server_list.sh" < <(printf '%s\n' "$@")
}

# subs_dir is where the scratch router keeps its subscriptions.
subs_dir() {
    printf '%s/data/subscriptions' "$VPD_DIR"
}

# write_config writes a config the way configure.sh leaves one: a data
# directory, the Web UI's secret and one Xray client.
write_config() {
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data, webui: {jwt_secret: "secret"}, xray: {clients: ["192.168.50.10"]}}' > "$VPD_CONFIG"
}

# write_list_file_named <file> <link...> writes another list file.
write_list_file_named() {
    local file=$1
    shift
    printf '%s\n' "$@" > "$file"
}

# serve_failing_download puts a curl first on PATH that fails every download,
# as a host that answers 403 does.
serve_failing_download() {
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    printf '#!/bin/sh\nexit 22\n' > "$BATS_TEST_TMPDIR/bin/curl"
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}
```

Replace every test of the "Publishing" section with:

```bash
# A first run has no subscription: the menu opens on Add. The list becomes a
# subscription file, 600 in a 700 directory, and xray.servers follows it.
@test "import_server_list.sh: a first run adds a subscription and brings the config in step" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -c '[.name, has(\"url\"), [.servers[].name]]' '$(subs_dir)'/*.json"
    assert_output '["servers",false,["A","B","C"]]'
    run jq -c '[.xray.servers, .xray.clients, .webui.jwt_secret]' "$VPD_CONFIG"
    assert_output '[["1.2.3.4","5.6.7.8"],["192.168.50.10"],"secret"]'
    run bash -c "stat -c %a '$(subs_dir)' '$(subs_dir)'/*.json"
    assert_output $'700\n600'
}

@test "import_server_list.sh: an https link is saved and names the subscription after its host" {
    load_import_into
    write_config
    write_list_file
    serve_list_file

    run_import "https://cdn.example/s/b" ""

    assert_success
    run bash -c "jq -c '[.name, .url]' '$(subs_dir)'/*.json"
    assert_output '["cdn.example","https://cdn.example/s/b"]'
}

@test "import_server_list.sh: a name given is the subscription's name" {
    load_import_into
    write_config
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" "  My List  "

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "My List"
}

# The single list and link of the previous release go with the first
# subscription written; the rest of the config stays.
@test "import_server_list.sh: removes servers.json and xray.subscription_url of the previous release" {
    load_import_into
    write_imported_state
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    [[ ! -e $VPD_DIR/data/servers.json ]]
    run jq -c '[(.xray | has("subscription_url")), .xray.clients]' "$VPD_CONFIG"
    assert_output '[false,["192.168.50.10"]]'
}

@test "import_server_list.sh: Add puts a second subscription beside the first" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "Beta" q

    assert_success
    # Both were added within one second, and the random ids order them then.
    run bash -c "source '$LIB_DIR/substore.sh'; substore_list '$(subs_dir)' | jq -c '[.[].name] | sort'"
    assert_output '["Alpha","Beta"]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["1.2.3.4","5.6.7.8","9.9.9.9"]'
}

@test "import_server_list.sh: the same https link refreshes its subscription" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"

    run_import a "https://cdn.example/s/b" "" q

    assert_success
    assert_output --partial "saved already"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Alpha"
}

@test "import_server_list.sh: refuses a name another subscription has, whatever its case" {
    load_import_into
    write_config
    write_list_file
    write_list_file_named "$BATS_TEST_TMPDIR/other.txt" 'vless://uuid-d@9.9.9.9:443?type=tcp&security=tls#D'
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import a "$BATS_TEST_TMPDIR/other.txt" "ALPHA" q

    assert_output --partial "Another subscription is named ALPHA"
    run bash -c "ls '$(subs_dir)' | wc -l"
    assert_output "1"
}

# A refresh whose download fails keeps the list and says why in the file,
# as the daemons do.
@test "import_server_list.sh: Refresh records why a download failed and keeps the list" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    serve_failing_download

    run_import r 1 q

    run bash -c "jq -c '[.error, (.servers | length)]' '$(subs_dir)'/*.json"
    assert_output '["Failed to download the subscription",3]'
}

# Deleted while its download runs: the list is not published, and the file
# is not brought back.
@test "import_server_list.sh: a subscription deleted while it downloads stays deleted" {
    load_import_into
    write_config
    write_list_file
    serve_list_file
    run_import "https://cdn.example/s/b" "Alpha"
    printf '#!/bin/sh\nrm -f "%s"/*.json\ncat "%s"\n' "$(subs_dir)" "$BATS_TEST_TMPDIR/servers.txt" > "$BATS_TEST_TMPDIR/bin/curl"

    run_import r 1 q

    assert_output --partial "deleted or changed while it downloaded"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
}

@test "import_server_list.sh: Rename" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"

    run_import n 1 "Main" q

    assert_success
    run bash -c "jq -r '.name' '$(subs_dir)'/*.json"
    assert_output "Main"
}

# The choice the watch kept from the deleted subscription ends with it; the
# running Xray is left alone, and the user is told.
@test "import_server_list.sh: Delete ends the choice kept from it and warns about the running server" {
    load_import_into
    write_config
    write_list_file
    run_import "$BATS_TEST_TMPDIR/servers.txt" "Alpha"
    local id
    id=$(jq -r '.id' "$(subs_dir)"/*.json)
    jq --arg id "$id" '.xray.active_server = {subscription: $id, name: "A"} | .xray.preferred_server = {subscription: $id, name: "B"}' \
        "$VPD_CONFIG" > "$VPD_CONFIG.new" && mv "$VPD_CONFIG.new" "$VPD_CONFIG"

    run_import d 1 y q

    assert_success
    assert_output --partial "The running Xray server came from Alpha"
    run bash -c "ls -A '$(subs_dir)'"
    assert_output ""
    run jq -c '[(.xray | has("preferred_server")), .xray.active_server.name, .xray.servers]' "$VPD_CONFIG"
    assert_output '[false,"A",[]]'
}

# The Web UI, the bot and configure.sh write under one lock, and so does the
# watch. Nothing is written before the lock is held.
@test "import_server_list.sh: publishes nothing while another writer holds the config lock" {
    load_import_into
    write_config
    write_list_file
    hold_config_lock
    export VPD_CONFIG_LOCK_WAIT=1

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""
    release_config_lock

    assert_failure
    assert_output --partial "nothing was imported"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
    run jq -c '.xray | has("servers")' "$VPD_CONFIG"
    assert_output "false"
}

@test "import_server_list.sh: a list without a usable server adds nothing" {
    load_import_into
    write_config
    printf '%s\n' 'vless://uuid@5.6.7.8:99999?type=tcp#Bad' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# Before configure.sh has run there is no config, and none is created.
@test "import_server_list.sh: creates no config" {
    load_import_into
    mkdir -p "$VPD_DIR/data"
    jq -n --arg data "$VPD_DIR/data" '{data_dir: $data}' > "$VPD_DIR/vpn-director.json.template"
    write_list_file

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    run bash -c "jq -c '[.servers[].name]' '$(subs_dir)'/*.json"
    assert_output '["A","B","C"]'
    [[ ! -e $VPD_CONFIG ]]
}
```

The five tests left in that section become:

```bash
# The new provider serves no links at all: its subscription is an array of
# Xray configs. The proxy outbound of each is the server; a config with more
# than one proxy is skipped as composite.
@test "import_server_list.sh: imports an Xray JSON subscription" {
    load_import_into
    write_config
    cat > "$BATS_TEST_TMPDIR/servers.txt" <<'JSON'
[{"remarks": "Oslo", "outbounds": [{"tag": "proxy", "protocol": "trojan", "settings": {"servers": [{"address": "198.51.100.10", "port": 443, "password": "p"}]}, "streamSettings": {"network": "tcp", "security": "tls"}}, {"tag": "direct", "protocol": "freedom"}]},
 {"remarks": "Auto", "outbounds": [{"protocol": "vless", "settings": {"address": "198.51.100.11", "port": 443, "id": "u"}}, {"protocol": "vless", "settings": {"address": "198.51.100.12", "port": 443, "id": "u"}}]}]
JSON

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_success
    assert_output --partial "Skipping Auto: composite (2 proxy outbounds)"
    run bash -c "jq -c '[.servers[] | [.name, .outbound.protocol, .outbound.settings.servers[0].password, .ips]]' '$(subs_dir)'/*.json"
    assert_output '[["Oslo","trojan","p",["198.51.100.10"]]]'
    run jq -c '.xray.servers' "$VPD_CONFIG"
    assert_output '["198.51.100.10"]'
}

# An HTML page - what some panels answer a browser with - is no subscription.
@test "import_server_list.sh: refuses a body it cannot read" {
    load_import_into
    write_config
    printf '%s\n' '<!doctype html><html><body>Open this link in your VPN app</body></html>' > "$BATS_TEST_TMPDIR/servers.txt"

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure
    assert_output --partial "Cannot read the subscription: unrecognized subscription format"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# The daemons refuse a subscription over 1 MiB rather than cut it short, and
# so does the router.
@test "import_server_list.sh: refuses a subscription file over 1 MiB" {
    load_import_into
    write_config
    write_oversized_list

    run_import "$BATS_TEST_TMPDIR/servers.txt" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl --max-filesize stops a transfer it knows to be too large with exit 63:
# the subscription's size, not a download that failed.
@test "import_server_list.sh: refuses a download curl stops at 1 MiB" {
    load_import_into
    write_config
    mkdir -p "$BATS_TEST_TMPDIR/bin"
    cat > "$BATS_TEST_TMPDIR/bin/curl" <<MOCK
#!/bin/sh
printf '%s\n' "\$*" > "$BATS_TEST_TMPDIR/curl.args"
exit 63
MOCK
    chmod +x "$BATS_TEST_TMPDIR/bin/curl"
    export PATH="$BATS_TEST_TMPDIR/bin:$PATH"

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    run cat "$BATS_TEST_TMPDIR/curl.args"
    assert_output --partial -- "--max-filesize 1048576"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}

# curl before 8.4.0 does not stop a transfer whose size it did not know in
# advance, so what it downloaded is measured again.
@test "import_server_list.sh: refuses a download over 1 MiB that curl let through" {
    load_import_into
    write_config
    write_oversized_list
    serve_list_file

    run_import "https://cdn.example/s/big" ""

    assert_failure 1
    assert_output --partial "exceeds 1 MiB"
    [[ ! -e $(subs_dir) ]] || [[ -z $(ls -A "$(subs_dir)") ]]
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats import_server_list.bats`
Expected: FAIL — the script still writes `servers.json` and has no menu.

- [ ] **Step 3: Rewrite the script around the menu**

In `router/opt/vpn-director/import_server_list.sh`:

1. Source the store after `lib/subscription.sh`:

```bash
# shellcheck source=lib/substore.sh
. "$SCRIPT_DIR/lib/substore.sh"
```

2. `read_input` tolerates the end of the input, which quits the menu:

```bash
read_input() {
    printf "%s: " "$1" >&2
    read -r INPUT_RESULT || INPUT_RESULT=""
}
```

3. Add below `read_input`:

```bash
# fail <message> - logs <message> as an error, leaves it in $FAIL_REASON_FILE
# for a refresh to record, and ends the action it runs in.
fail() {
    log -l ERROR "$1"
    if [[ -n ${FAIL_REASON_FILE:-} ]]; then
        printf '%s' "$1" > "$FAIL_REASON_FILE"
    fi
    exit 1
}

# run_action <function> [args] - runs one action in a subshell with errexit
# on: its fail or exit ends the action and not the menu. ACTION_RC is its
# status. Never call it on the left of || or &&: bash turns errexit off inside.
run_action() {
    set +e
    ( set -e; "$@" )
    ACTION_RC=$?
    set -e
}
```

4. Split the first step: `step_get_subscription` keeps its prompt and hands the answer on; everything from the `if [[ -z "$SUB_INPUT" ]]` check down moves into `fetch_subscription`, with every `log -l ERROR "…"` followed by `exit 1` turned into `fail "…"`:

```bash
step_get_subscription() {
    log -l TRACE "Step 1: Subscription"

    printf "Enter a subscription URL or the path to a file:\n"
    printf "(Share links - vless, vmess, trojan, ss, hysteria2 - base64 or plain, or Xray JSON)\n\n"

    read_input "URL or path"
    fetch_subscription "$INPUT_RESULT"
}

# fetch_subscription <link or path> - downloads the link or reads the file into
# SUB_RESULT, the Result JSON of subscription_decode, and logs every entry it
# skipped. SUB_INPUT keeps <link or path>.
fetch_subscription() {
    SUB_INPUT=$1

    if [[ -z "$SUB_INPUT" ]]; then
        fail "No input provided"
    fi

    # The largest subscription taken, in bytes: 1 MiB, the cap of the daemons'
    # download (service.MaxSubscriptionBody). A larger one is refused, never
    # cut short: cut, a base64 list decodes to a shorter one.
    local -r max_bytes=1048576
    local content rc=0
    case "$SUB_INPUT" in
        http://*|https://*)
            log "Downloading from URL..."
            content=$(curl -fsSL --connect-timeout 10 --max-time 60 --max-filesize "$max_bytes" "$SUB_INPUT") || rc=$?
            if (( rc == 63 )); then
                # curl's "maximum file size exceeded"
                fail "Subscription exceeds 1 MiB; nothing was imported"
            elif (( rc != 0 )); then
                fail "Failed to download the subscription"
            fi
            # curl before 8.4.0 does not stop a transfer whose size it did not
            # know in advance.
            if (( $(printf '%s' "$content" | wc -c) > max_bytes )); then
                fail "Subscription exceeds 1 MiB; nothing was imported"
            fi
            ;;
        *)
            if [[ ! -f "$SUB_INPUT" ]]; then
                fail "File not found: $SUB_INPUT"
            fi
            if (( $(wc -c < "$SUB_INPUT") > max_bytes )); then
                fail "Subscription exceeds 1 MiB; nothing was imported"
            fi
            content=$(cat "$SUB_INPUT")
            ;;
    esac

    local err
    err=$(tmp_file)
    if ! SUB_RESULT=$(printf '%s' "$content" | subscription_decode 2>"$err"); then
        fail "Cannot read the subscription: $(cat "$err")"
    fi

    local skipped line
    skipped=$(printf '%s' "$SUB_RESULT" | jq -r "$JQ_PRINTABLE"'
        .skipped[] | "Skipping \(.name | printable): \(.reason) (\(.detail | printable))"')
    if [[ -n $skipped ]]; then
        while IFS= read -r line; do
            log -l WARN "$line"
        done <<< "$skipped"
    fi

    if [[ $(printf '%s' "$SUB_RESULT" | jq '.servers | length') -eq 0 ]]; then
        fail "No supported servers in subscription"
    fi
}
```

5. `step_parse_servers` reuses a `$SERVERS_TMP` the caller set, and no longer names `servers.json`: replace its first lines with

```bash
step_parse_servers() {
    log -l TRACE "Step 2: Resolving Servers"

    SERVERS_TMP=${SERVERS_TMP:-$(tmp_file)}
```

   (the `DATA_DIR=$(get_data_dir)` and `SERVERS_FILE=…` lines go), and its last check becomes `fail "No servers could be resolved"`.

6. Replace `step_publish_servers` and `main` with:

```bash
###############################################################################
# Publishing: every write under the config lock
###############################################################################

# now - the time as the subscription files keep it: UTC, whole seconds.
now() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

# store_and_sync <subscription_json> - under the lock the caller holds: writes
# the file, brings xray.servers in step, takes away what the previous release
# kept (servers.json, xray.subscription_url), and releases the lock.
store_and_sync() {
    if ! substore_write "$SUB_DIR" "$1"; then
        substore_unlock
        fail "Failed to write the subscription; nothing was saved"
    fi
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")"; then
        substore_unlock
        fail "The subscription is saved, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    substore_unlock
}

# publish_add <input> <name> <base> - saves $SERVERS_TMP as a subscription. An
# https link already saved makes it a refresh of that subscription, and a
# rename too when <name> is given; a file or a plain-http link is a static
# list, always a new one. <base> names a new subscription when <name> is empty.
publish_add() {
    local input=$1 name=$2 base=$3 url="" subs id sub stamp
    [[ $input == https://* ]] && url=$input
    stamp=$(now)
    if ! substore_lock "$VPD_CONFIG"; then
        fail "Config is locked by the Web UI or the bot; nothing was imported. Run the import again"
    fi
    subs=$(substore_list "$SUB_DIR")
    id=""
    if [[ -n $url ]]; then
        id=$(jq -r --arg u "$url" 'first(.[] | select(.url == $u) | .id) // ""' <<< "$subs")
    fi
    if [[ -n $id ]]; then
        sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$subs")
        if [[ -n $name && $name != "$(jq -r '.name' <<< "$sub")" ]]; then
            if substore_name_taken "$subs" "$name" "$id"; then
                substore_unlock
                fail "Another subscription is named $name; nothing was imported"
            fi
            sub=$(jq -c --arg n "$name" '.name = $n' <<< "$sub")
        fi
        log "The link is saved already as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub"); its list is refreshed"
    else
        if (( $(jq length <<< "$subs") >= SUBSTORE_MAX )); then
            substore_unlock
            fail "There are $SUBSTORE_MAX subscriptions already; delete one first"
        fi
        if [[ -z $name ]]; then
            name=$(substore_default_name "$subs" "$base")
        elif substore_name_taken "$subs" "$name"; then
            substore_unlock
            fail "Another subscription is named $name; nothing was imported"
        fi
        id=$(substore_new_id "$SUB_DIR")
        sub=$(jq -nc --arg id "$id" --arg n "$name" --arg u "$url" --arg now "$stamp" \
            '{id: $id, name: $n, added: $now} + (if $u == "" then {} else {url: $u} end)')
    fi
    sub=$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$stamp" \
        '.servers = $servers[0] | .refreshed = $now | del(.error)' <<< "$sub")
    store_and_sync "$sub"
    log "Saved $SERVER_COUNT servers as $(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")"
}

# publish_refresh <id> <link> <name> - saves $SERVERS_TMP as the list of
# subscription <id>, only while it still exists with <link>: one deleted, or
# deleted and added again, while its download ran is not brought back.
publish_refresh() {
    local id=$1 url=$2 name=$3 sub
    if ! substore_lock "$VPD_CONFIG"; then
        fail "Config is locked by the Web UI or the bot; $name was not refreshed"
    fi
    sub=$(jq -c --arg id "$id" --arg u "$url" 'first(.[] | select(.id == $id and .url == $u)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -z $sub ]]; then
        substore_unlock
        fail "$name was deleted or changed while it downloaded; nothing was written"
    fi
    store_and_sync "$(jq -c --slurpfile servers "$SERVERS_TMP" --arg now "$(now)" \
        '.servers = $servers[0] | .refreshed = $now | del(.error)' <<< "$sub")"
    log "Refreshed $name: $SERVER_COUNT servers"
}

# record_refresh_error <id> <link> <refreshed> <reason> - notes why a refresh
# failed; the list stays. Nothing is written when the subscription is gone,
# has another link, or was refreshed since <refreshed> by someone else.
record_refresh_error() {
    local id=$1 url=$2 seen=$3 reason=$4 sub
    substore_lock "$VPD_CONFIG" || return 0
    sub=$(jq -c --arg id "$id" --arg u "$url" --arg seen "$seen" \
        'first(.[] | select(.id == $id and .url == $u and (.refreshed // "") == $seen)) // empty' <<< "$(substore_list "$SUB_DIR")")
    if [[ -n $sub ]]; then
        substore_write "$SUB_DIR" "$(jq -c --arg r "$reason" '.error = $r' <<< "$sub")" || true
    fi
    substore_unlock
}

###############################################################################
# The menu
###############################################################################

# show_subscriptions <subs_json> - the subscriptions, numbered, a line each.
show_subscriptions() {
    printf '\nSubscriptions:\n'
    jq -r "$JQ_PRINTABLE"'
        def host: (split("/")[2] // "") | split("@") | last | split(":")[0];
        to_entries[]
        | .key as $i | .value
        | "  \($i + 1)) \(.name | printable)   \(if (.url // "") == "" then "static list" else (.url | host | printable) end)   \(.servers | length) servers   "
          + (if (.error // "") != "" then "error: \(.error | printable)"
             else "refreshed \((.refreshed // "") | .[0:10]) \((.refreshed // "") | .[11:16])" end)' <<< "$1"
}

# pick_subscription <subs_json> <prompt> - asks for a number of the list and
# prints the id at it; fails for an answer that is no number of it.
pick_subscription() {
    local count
    count=$(jq length <<< "$1")
    read_input "$2 [1-$count]"
    if [[ $INPUT_RESULT =~ ^[0-9]+$ ]] && (( INPUT_RESULT >= 1 && INPUT_RESULT <= count )); then
        jq -r ".[$((INPUT_RESULT - 1))].id" <<< "$1"
        return 0
    fi
    fail "There is no subscription number $INPUT_RESULT"
}

# menu_add - asks for a link or a file and a name, downloads and resolves the
# list, and saves it (publish_add).
menu_add() {
    local base name=""
    step_get_subscription
    case $SUB_INPUT in
        http://*|https://*) base=$(jq -Rr '(split("/")[2] // "") | split("@") | last | split(":")[0]' <<< "$SUB_INPUT") ;;
        *) base=${SUB_INPUT##*/}; base=${base%.*} ;;
    esac
    read_input "Name (Enter for $base)"
    if [[ -n $INPUT_RESULT ]]; then
        name=$(substore_clean_name "$INPUT_RESULT") || fail "The name was refused; nothing was imported"
    fi
    step_parse_servers
    publish_add "$SUB_INPUT" "$name" "$base"
}

# fetch_and_resolve <link> - one refresh's download and resolution.
fetch_and_resolve() {
    fetch_subscription "$1"
    step_parse_servers
}

# refresh_one <subscription_json> - downloads the subscription's link again
# and publishes the list. A download or a list that fails is recorded in the
# subscription, whose list stays, and ends the action.
refresh_one() {
    local sub=$1 id url seen name reason
    id=$(jq -r '.id' <<< "$sub")
    url=$(jq -r '.url' <<< "$sub")
    seen=$(jq -r '.refreshed // ""' <<< "$sub")
    name=$(jq -r "$JQ_PRINTABLE"' .name | printable' <<< "$sub")
    log "Refreshing $name"
    SERVERS_TMP=$(tmp_file)
    FAIL_REASON_FILE=$(tmp_file)
    run_action fetch_and_resolve "$url"
    if (( ACTION_RC != 0 )); then
        reason=$(cat "$FAIL_REASON_FILE")
        record_refresh_error "$id" "$url" "$seen" "${reason:-the refresh failed}"
        exit 1
    fi
    FAIL_REASON_FILE=""
    SERVER_COUNT=$(jq length "$SERVERS_TMP")
    publish_refresh "$id" "$url" "$name"
}

# linked_subscriptions <subs_json> - those that have a link to refresh.
linked_subscriptions() {
    jq -c 'map(select((.url // "") != ""))' <<< "$1"
}

menu_refresh() {
    local linked id
    linked=$(linked_subscriptions "$1")
    [[ $(jq length <<< "$linked") -gt 0 ]] || fail "No subscription has a link to refresh"
    show_subscriptions "$linked"
    id=$(pick_subscription "$linked" "Refresh subscription")
    refresh_one "$(jq -c --arg id "$id" 'first(.[] | select(.id == $id))' <<< "$linked")"
}

menu_refresh_all() {
    local sub total=0 failed=0
    while IFS= read -r sub; do
        total=$((total + 1))
        run_action refresh_one "$sub"
        if (( ACTION_RC != 0 )); then
            failed=$((failed + 1))
        fi
    done < <(jq -c '.[]' <<< "$(linked_subscriptions "$1")")
    log "Refreshed $((total - failed)) of $total subscriptions"
}

menu_rename() {
    local subs=$1 id sub name
    id=$(pick_subscription "$subs" "Rename subscription")
    read_input "New name"
    name=$(substore_clean_name "$INPUT_RESULT") || fail "The name was refused; nothing was renamed"
    substore_lock "$VPD_CONFIG" || fail "Config is locked by the Web UI or the bot; nothing was renamed"
    subs=$(substore_list "$SUB_DIR")
    sub=$(jq -c --arg id "$id" 'first(.[] | select(.id == $id)) // empty' <<< "$subs")
    if [[ -z $sub ]]; then
        substore_unlock
        fail "That subscription is gone"
    fi
    if substore_name_taken "$subs" "$name" "$id"; then
        substore_unlock
        fail "Another subscription is named $name"
    fi
    store_and_sync "$(jq -c --arg n "$name" '.name = $n' <<< "$sub")"
    log "Renamed to $name"
}

menu_delete() {
    local subs=$1 id name active=""
    id=$(pick_subscription "$subs" "Delete subscription")
    name=$(jq -r --arg id "$id" "$JQ_PRINTABLE"' first(.[] | select(.id == $id)) | .name | printable' <<< "$subs")
    read_input "Delete $name and its servers? [y/N]"
    if [[ $INPUT_RESULT != [yY]* ]]; then
        log "Nothing was deleted"
        return 0
    fi
    substore_lock "$VPD_CONFIG" || fail "Config is locked by the Web UI or the bot; nothing was deleted"
    substore_delete "$SUB_DIR" "$id"
    if ! substore_sync_config "$VPD_CONFIG" "$(substore_list "$SUB_DIR")" \
        "if (.xray.preferred_server.subscription // \"\") == \"$id\" then del(.xray.preferred_server) else . end"; then
        substore_unlock
        fail "$name is deleted, but $VPD_CONFIG was not updated"
    fi
    rm -f "$DATA_DIR/servers.json"
    if [[ -f $VPD_CONFIG ]]; then
        active=$(jq -r '.xray.active_server.subscription // ""' "$VPD_CONFIG")
    fi
    substore_unlock
    log "Deleted $name"
    if [[ $active == "$id" ]]; then
        log -l WARN "The running Xray server came from $name. Xray keeps running it until you select another (configure.sh, the Web UI or /xray)"
    fi
}

menu() {
    local subs
    while :; do
        subs=$(substore_list "$SUB_DIR")
        show_subscriptions "$subs"
        printf '\na) Add  r) Refresh  R) Refresh all  n) Rename  d) Delete  q) Quit\n'
        read_input "Choice"
        case $INPUT_RESULT in
            a) run_action menu_add ;;
            r) run_action menu_refresh "$subs" ;;
            R) run_action menu_refresh_all "$subs" ;;
            n) run_action menu_rename "$subs" ;;
            d) run_action menu_delete "$subs" ;;
            q|Q|"") return 0 ;;
            *) printf 'Unknown choice: %s\n' "$INPUT_RESULT" ;;
        esac
    done
}

###############################################################################
# Main
###############################################################################

main() {
    log -l TRACE "Import Server List"
    printf "Subscriptions: the server lists this router can run.\n"

    DATA_DIR=$(get_data_dir)
    SUB_DIR=$(substore_dir "$DATA_DIR")
    if [[ $(substore_list "$SUB_DIR" | jq length) -eq 0 ]]; then
        # A first run: there is nothing to choose from yet.
        run_action menu_add
        if (( ACTION_RC != 0 )); then
            exit "$ACTION_RC"
        fi
    fi
    menu

    log -l TRACE "Import Complete"
    printf "Run /opt/vpn-director/configure.sh to select a server and finish the setup.\n"
}
```

   Update the header comment of the script: it keeps the subscriptions of `lib/substore.sh` — adds, refreshes, renames and deletes them under the config lock — instead of publishing one list.

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats import_server_list.bats && shellcheck ../opt/vpn-director/import_server_list.sh`
Expected: every test passes; shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/import_server_list.sh router/test/import_server_list.bats
git commit -m "feat(shell): import_server_list.sh adds, refreshes, renames and deletes subscriptions"
```

---

### Task 15: `configure.sh` — subscription, then server

**Files:**
- Modify: `router/opt/vpn-director/configure.sh`
- Modify: `router/test/unit/configure.bats`

**Interfaces:**
- Consumes: Task 13's `substore_dir`, `substore_list`, `substore_ips`.
- Produces: `check_subscriptions` (sets `SUB_DIR` and `SUBS_JSON`, the subscriptions that have servers), `SELECTED_SUBSCRIPTION_ID`; `active_server` carries `subscription`; `xray.servers` is the union of every subscription's addresses.

- [ ] **Step 1: Write the failing tests**

In `router/test/unit/configure.bats`, `load_wizard` replaces its two `SERVERS_FILE` lines with:

```bash
    SUBS_JSON='[{"id":"0a1b2c3d","name":"Main","servers":[{"address":"1.2.3.4","ips":["1.2.3.4"]}]}]'
    SELECTED_SUBSCRIPTION_ID="0a1b2c3d"
```

The three `step_select_xray_server` tests hand their list over as one subscription instead of `$SERVERS_FILE`; their assertions stay:

```bash
@test "step_select_xray_server: lists each server with its protocol" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Legacy","address":"legacy.example.com","port":443,"ips":["1.2.3.4"],"security":"reality"},
 {"name":"Old TLS","address":"old.example.com","port":443,"ips":["1.2.3.5"]},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"Canada SS","address":"ss.example.com","port":2030,"ips":["1.2.3.7"],"outbound":{"protocol":"shadowsocks"}},
 {"name":"Gaming","address":"hy.example.com","port":8443,"ips":["1.2.3.8"],"outbound":{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Legacy [vless·reality]"
    assert_output --partial "2) Old TLS [vless·tls]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) Canada SS [ss]"
    assert_output --partial "5) Gaming [hysteria2]"
}

@test "step_select_xray_server: an outbound it cannot read is listed as ?" {
    load_wizard
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Broken","address":"broken.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":"tcp"}},
 {"name":"Null","address":"null.example.com","port":443,"ips":["1.2.3.5"],"outbound":null},
 {"name":"Oslo WS","address":"oslo.example.com","port":443,"ips":["1.2.3.6"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}},
 {"name":"No protocol","address":"noproto.example.com","port":443,"ips":["1.2.3.7"],"outbound":{"streamSettings":{"network":"ws","security":"tls"}}}]
JSON
)

    run step_select_xray_server <<< "3"

    assert_success
    assert_output --partial "1) Broken [?]"
    assert_output --partial "2) Null [?]"
    assert_output --partial "3) Oslo WS [vless·ws·tls]"
    assert_output --partial "4) No protocol [?]"
}

# A name and an outbound are subscription text: an escape sequence in either
# must not reach the terminal. The wizard's own colours are escape sequences
# too, so they are switched off: any ESC left in the output came from the list.
@test "step_select_xray_server: control characters from a subscription do not reach the terminal" {
    load_wizard
    RED='' GREEN='' YELLOW='' BLUE='' NC=''
    SUBS_JSON=$(jq -c '[{id: "0a1b2c3d", name: "Main", servers: .}]' <<'JSON'
[{"name":"Bad\u001b[31mName","address":"bad.example.com","port":443,"ips":["1.2.3.4"],"outbound":{"protocol":"vless","streamSettings":{"network":"ws","security":"\u001b]0;owned\u0007tls"}}}]
JSON
)

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial $'\x1b'
    assert_output --partial "1) Bad[31mName [vless·ws·]0;ownedtls]"
}
```

`step_generate_configs: records the server the Xray config was built from` also asserts the subscription:

```bash
    run jq -r '.xray.active_server | "\(.name)|\(.address)|\(.port)|\(.subscription)"' "$VPD_DIR/vpn-director.json"
    assert_output "Осло, Норвегия, Extra|1.2.3.4|443|0a1b2c3d"
```

Add:

```bash
two_subscriptions='[
  {"id":"0a1b2c3d","name":"Alpha","servers":[
    {"name":"Oslo","address":"a.example.com","port":443,"ips":["192.0.2.10"]},
    {"name":"Germany-1","address":"b.example.com","port":443,"ips":["192.0.2.11"]}]},
  {"id":"1b2c3d4e","name":"Beta","servers":[
    {"name":"Germany-1","address":"198.51.100.20","port":8443,"ips":["198.51.100.20"]}]}]'

@test "step_select_xray_server: with several subscriptions it asks for the subscription first" {
    load_wizard
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    run step_select_xray_server < <(printf '2\n1\n')

    assert_success
    assert_output --partial "1) Alpha (2 servers)"
    assert_output --partial "2) Beta (1 servers)"
    assert_output --partial "Selected: Germany-1 (198.51.100.20)"
}

@test "step_select_xray_server: one subscription goes straight to its servers" {
    load_wizard

    run step_select_xray_server <<< "1"

    assert_success
    refute_output --partial "Select subscription"
}

@test "step_generate_configs: xray.servers covers every subscription" {
    load_wizard
    write_daemon_config
    SUBS_JSON=$(jq -c . <<< "$two_subscriptions")

    run step_generate_configs

    assert_success
    run jq -c '.xray.servers' "$VPD_DIR/vpn-director.json"
    assert_output '["192.0.2.10","192.0.2.11","198.51.100.20"]'
}

@test "check_subscriptions: reads the shared fixtures" {
    load_wizard
    mkdir -p "$VPD_DIR/data/subscriptions"
    cp "$PROJECT_ROOT/../testdata/substore/"*.json "$VPD_DIR/data/subscriptions/"
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"

    check_subscriptions > /dev/null

    [[ $(jq -c '[.[].name]' <<< "$SUBS_JSON") == '["Beta","Alpha","Gamma"]' ]]
}

@test "check_subscriptions: no subscription sends the user to the import" {
    load_wizard
    jq --arg d "$VPD_DIR/data" '.data_dir = $d' "$VPD_DIR/vpn-director.json.template" > "$VPD_DIR/vpn-director.json"

    run check_subscriptions

    assert_failure
    assert_output --partial "Run import_server_list.sh first"
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd router/test && bats unit/configure.bats`
Expected: FAIL — `check_subscriptions: command not found`, and the selection tests still read `$SERVERS_FILE`.

- [ ] **Step 3: Write the two-step choice**

In `router/opt/vpn-director/configure.sh`:

1. Source the store after `lib/xrayconf.sh`:

```bash
# Library: the subscription files (self-contained, like xrayconf.sh)
. "$VPD_DIR/lib/substore.sh"
```

2. Add `SELECTED_SUBSCRIPTION_ID=""` to the "Temporary storage" variables.
3. Replace `check_servers_file` with:

```bash
# check_subscriptions - reads the subscriptions that have servers into
# SUBS_JSON, and stops the wizard when there is none.
check_subscriptions() {
    DATA_DIR=$(get_data_dir)
    SUB_DIR=$(substore_dir "$DATA_DIR")
    SUBS_JSON=$(substore_list "$SUB_DIR" | jq -c 'map(select((.servers // []) | length > 0))')

    local subs servers
    subs=$(jq length <<< "$SUBS_JSON")
    servers=$(jq '[.[].servers[]] | length' <<< "$SUBS_JSON")
    if [[ $subs -eq 0 ]]; then
        print_error "No subscription with servers in $SUB_DIR"
        print_info "Run import_server_list.sh first"
        exit 1
    fi

    print_success "Found $servers servers in $subs subscription(s)"
}
```

4. `step_select_xray_server` asks for the subscription first when there are several, then lists that subscription's servers exactly as today, reading `"$servers_json"` where it read `"$SERVERS_FILE"`:

```bash
step_select_xray_server() {
    print_header "Step 1: Select Xray Server"

    local count k=0 servers_json
    count=$(jq length <<< "$SUBS_JSON")
    if [[ $count -gt 1 ]]; then
        printf "Subscriptions:\n\n"
        i=1
        jq -r "$JQ_PRINTABLE"' .[] | "\(.name | printable)|\(.servers | length)"' <<< "$SUBS_JSON" | \
        while IFS='|' read -r name n; do
            printf "  %2d) %s (%s servers)\n" "$i" "$name" "$n"
            i=$((i + 1))
        done
        printf "\n"
        while true; do
            printf "Select subscription [1-%d]: " "$count"
            read -r choice
            if [[ $choice -ge 1 ]] 2>/dev/null && [[ $choice -le $count ]] 2>/dev/null; then
                break
            fi
            print_error "Invalid choice. Enter a number between 1 and $count"
        done
        k=$((choice - 1))
    fi
    SELECTED_SUBSCRIPTION_ID=$(jq -r ".[$k].id" <<< "$SUBS_JSON")
    servers_json=$(jq -c ".[$k].servers" <<< "$SUBS_JSON")

    printf "Available servers:\n\n"

    # Read servers from JSON and display. The label names the protocol, as the
    # Web UI and the bot do (vpnconfig.Server.Label): a record without an
    # outbound is a legacy VLESS one, generated as TLS when it names no
    # security.
    i=1
    jq -r "$JQ_PRINTABLE"'
        def protocol_label:
          # An outbound is stored as the subscription wrote it, so nothing says
          # its streamSettings is an object: indexing one that is not ends jq,
          # and with it the whole list. Server.Label in vpnconfig/outbound.go
          # answers "?" to an outbound it cannot read; so does this. Only a
          # record with no outbound key at all is the legacy one - jq reads a
          # null the way it reads a missing key, and a null outbound is no
          # outbound: the generators reject it, so it is a "?" too. So is an
          # outbound that names no protocol, as it is for Server.Label: there
          # is nothing to label, and "·ws·tls" is no label.
          try (
            (if (has("outbound") | not)
             then ["vless", .network, (if (.security // "") == "" then "tls" else .security end)]
             elif (.outbound | type) != "object" then error("not an outbound")
             else [.outbound.protocol, .outbound.streamSettings.network, .outbound.streamSettings.security] end)
            | map(if . == null then "" elif type == "string" then . else error("not a string") end) as [$p, $n, $s]
            | if $p == "" then "?"
              elif $p == "shadowsocks" then "ss" elif $p == "hysteria" then "hysteria2"
              else [$p] + (if $n == "" or $n == "tcp" or $n == "raw" then [] else [$n] end)
                        + (if $s == "" or $s == "none" then [] else [$s] end) | join("·") end
          ) catch "?";
        .[] | "\(.name | printable)|\(.address | printable)|\((.ips // []) | join(", "))|\(protocol_label | printable)"' <<< "$servers_json" | \
    while IFS='|' read -r name address ip label; do
        printf "  %2d) %s [%s]\n      %s -> %s\n\n" "$i" "$name" "$label" "$address" "$ip"
        i=$((i + 1))
    done

    total=$(jq length <<< "$servers_json")

    while true; do
        printf "Select server [1-%d]: " "$total"
        read -r choice

        if [[ $choice -ge 1 ]] 2>/dev/null && [[ $choice -le $total ]] 2>/dev/null; then
            break
        fi
        print_error "Invalid choice. Enter a number between 1 and $total"
    done

    # Get selected server data (jq uses 0-based index)
    idx=$((choice - 1))
    SELECTED_SERVER_ADDRESS=$(jq -r ".[$idx].address" <<< "$servers_json")
    SELECTED_SERVER_PORT=$(jq -r ".[$idx].port" <<< "$servers_json")
    SELECTED_SERVER_JSON=$(jq -c ".[$idx]" <<< "$servers_json")
    selected_name=$(jq -r "$JQ_PRINTABLE .[$idx].name | printable" <<< "$servers_json")

    print_success "Selected: $selected_name ($SELECTED_SERVER_ADDRESS)"
}
```

5. In `step_generate_configs`, `xray_servers_json` comes from every subscription, and the record names the subscription:

```bash
    # xray.servers: every address of every subscription (TPROXY_BYPASS).
    xray_servers_json=$(substore_ips "${SUBS_JSON:-[]}")
```

```bash
    xray_active_server_json=$(printf '%s' "$SELECTED_SERVER_JSON" \
        | jq -c --arg sub "${SELECTED_SUBSCRIPTION_ID:-}" \
            '{name: (.name // ""), address: (.address // ""), port: (.port // 0)}
             + (if $sub == "" then {} else {subscription: $sub} end)')
```

6. `main` calls `check_subscriptions` where it called `check_servers_file` (its comment: "Validate that a subscription exists").

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd router/test && bats unit/configure.bats && shellcheck ../opt/vpn-director/configure.sh`
Expected: every test passes; shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add router/opt/vpn-director/configure.sh router/test/unit/configure.bats
git commit -m "feat(shell): configure.sh picks a subscription, then its server"
```

---

### Task 16: Documentation

**Files:**
- Modify: `CLAUDE.md`, `README.md`
- Modify: `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md`, `.claude/rules/xray-tproxy.md`, `.claude/rules/testing.md`
- Modify: `router/opt/vpn-director/lib/xrayconf.sh` (header comment)

**Interfaces:** none; the docs describe what Tasks 1–15 built, and the spec is their source.

- [ ] **Step 1: `CLAUDE.md`**

- Commands: the block's last entry becomes

```bash
# Subscriptions: add, refresh, rename, delete (a menu)
/opt/vpn-director/import_server_list.sh
```

- Architecture table, two new rows beside `lib/subscription.sh` and `testdata/subscription/`:

```
| `router/opt/vpn-director/lib/substore.sh` | Subscription files (`<data_dir>/subscriptions/<id>.json`): order, ids, names, the write under the config lock; the twin of `vpnconfig/substore.go` |
| `testdata/substore/` | Synthetic subscription files both stores (shell and Go) must list alike |
```

- Data storage: "`data_dir` in vpn-director.json (default: `/opt/vpn-director/data`) — `subscriptions/<id>.json` (a file per subscription: its link, its status, its servers), ipset dumps".

- [ ] **Step 2: `README.md`**

- After installation, step 1 becomes: "Add the servers of your subscriptions (optional). The script is a menu: add, refresh, rename and delete subscriptions — up to ten, each a link or a file:" followed by the same command.
- The CLI block's `# Import servers` becomes `# Subscriptions: add, refresh, rename, delete`.
- Bot commands: `| /import <url> [name] | Add a subscription, or refresh the one saved with that link; /import alone refreshes them all |` and a new row `| /subs | Subscriptions: refresh, rename, delete |`.
- "How It Works → Xray TPROXY": after the paragraph on the formats, add: "Up to ten subscriptions live side by side; you pick the running server from any of them. When it dies, the bot's subscription watch moves the clients onto a Tunnel Director tunnel, refreshes every subscription at once and walks their servers — the chosen one and two more of its subscription, then one server of each subscription in turn — until one answers, and brings the clients back on it."

- [ ] **Step 3: `.claude/rules/telegram-bot.md`**

- The tree: `handler/subs.go  # /subs: refresh, rename, delete a subscription` and `subwatch/order.go  # The hybrid walk order and the dedupe key`.
- Commands table: `/import [url] [name]` — "Add a subscription, or refresh the one saved with that link; alone, refresh every subscription (a body over 1 MiB is refused)"; new rows `/subs` (`SubsHandler.HandleSubs`, "Subscriptions with refresh, rename and delete buttons; a rename takes the chat's next message") and `/cancel` (`SubsHandler.HandleCancel`, "Ends a rename that waits for its name").
- Configuration Wizard, step 1: "Server Selection — a subscription, then one of its servers (the first step is skipped with one subscription), 30 a page".
- Server switch (`/xray`): rewrite for two steps — `xray:sub:<id>:<page>`, `xray:subs`, `xray:select:<id>:<index>:<fingerprint>` with the fingerprint over `subscription|name|address|port`; a keyboard sent before subscriptions answers "server list changed".
- Subscription watch: bring every statement in line with spec 5. Replace these, and keep everything else of the section as it is:
  - Arming: "Armed when at least one subscription exists — a static list included — and there are effective Xray clients (after subtracting `paused_clients`). A `xray.failover` record arms it with or without a subscription, and so does a restore whose last apply has not succeeded: without a subscription the watch still restores the clients, follows the fallback tunnel and says so, but refreshes and walks nothing."
  - The look at the active server dials "every IPv4 address its subscription's entry lists (`chosenIndex`: its subscription, then its name, as the walk finds it)"; "`servers.json` unreadable" becomes "the subscriptions unreadable".
  - "It then refreshes the saved subscription (…)" becomes "It then runs a wave: every subscription with a link downloads at once, each within `FetchTimeout` (…)" — the WAN-then-tunnel path, the IPv4 resolution and the 3-minute deadline apply to each download as before.
  - The walk order: "the chosen server — its subscription's entry with its name, address and port, or else that subscription's first entry with its name — then the next servers of its subscription until `OwnFirst` (3) are placed, then one server of each subscription in turn, starting after the chosen server's subscription (`walkOrder`); a copy whose outbound, with its IPv4 in place, was already tried in the wave is skipped (`dialKey`)".
  - Publication: each arriving list is published with `xray.servers` in one config-lock update, only while its subscription still exists with the downloaded link (`vpnconfig.RefreshSubscription`); a failed download records its reason in the subscription's `error` (`vpnconfig.RecordSubscriptionError`, not over a newer `refreshed`) and keeps the list; the walk runs when at least one download arrived or no subscription has a link, a failed subscription is walked from its last list, and with every download failed there is no walk and "Subscription refresh failed: A, B" is sent.
  - Guards: the walk's guard refuses a server whose subscription is gone or has another link (`vpnconfig.ErrSubscriptionGone`), and the walk skips the rest of that subscription; the restore after a live probe does not look at the subscription; the return to the preferred server checks the preferred server's subscription.
  - Delete every sentence about `xray.subscription_url`, "a link saved while the walk runs", `vpnconfig.PublishServers`, `SubscriptionUnchanged`, and `import_server_list.sh` publishing `servers.json` with the saved link.
  - Messages: servers are named `<subscription> / <server>`; "No live server in any subscription"; a wave in which only some downloads failed sends nothing.
  - The returns: "the preferred server's entry in its subscription accepts TCP"; "while no subscription lists the server that runs".

- [ ] **Step 4: `.claude/rules/webui.md`**

- API table: the rows of spec 6.1 (`GET/POST /api/subscriptions`, `POST /api/subscriptions/refresh`, `POST /api/subscriptions/rename`, `DELETE /api/subscriptions`, the grouped `GET /api/servers`, `POST /api/servers/active` with `subscription`); `POST /api/servers/import` goes; `/api/config` "with `jwt_secret` blanked — subscription links live in their own files".
- The `active_server` paragraph: the record names its subscription too, since two subscriptions can name a server alike; a record from before subscriptions matches no server.
- The paragraph on the two server routes: the subscription routes write one subscription file and `xray.servers` in one config-lock update through `service.AddSubscription`, `RefreshSubscription`, `RefreshAllSubscriptions`, `RenameSubscription` and `DeleteSubscription`; a refresh publishes only while its subscription still exists with the link it downloaded; a failed download is a result (200) that says why, and the subscription records it. Drop the sentences about `PublishImport`, the saved link and `subscription_saved`.
- The paragraph on what an import says: "An add says the subscription is saved only when its file was written; a failure after that point carries `vpnconfig.ErrServersSaved`". The 1 MiB rule stays.
- `POST /api/servers/active` names the subscription as well as the index; a subscription that is gone is a 409 as a moved server is.
- Authentication: "`GET /api/config` blanks `jwt_secret`. Subscription links, whose paths carry tokens, are never in the config."

- [ ] **Step 5: `.claude/rules/xray-tproxy.md`, `.claude/rules/testing.md`, `lib/xrayconf.sh`**

- `xray-tproxy.md`: "An import stores each server's Xray outbound in its subscription's file (`<data_dir>/subscriptions/<id>.json`, `servers[].outbound`)" where it says `servers.json`.
- `testing.md`: `unit/substore.bats` in the tree, and after "Shared subscription cases" a paragraph: "`testdata/substore/` holds synthetic subscription files. `router/test/unit/substore.bats` and `server/internal/vpnconfig/substore_test.go` both read them and must list them alike — the same order, the same names — and both apply the same name rules to the same inputs. Entware's jq has no regex builtins; `substore.bats` fails when `lib/substore.sh` uses one."
- `lib/xrayconf.sh` header: "JSON object (as a subscription file stores it)".

- [ ] **Step 6: Check nothing stale is left**

Run:

```bash
grep -rn "servers\.json\|subscription_url\|PublishServers\|PublishImport\|SubscriptionUnchanged\|subscription_saved" CLAUDE.md README.md .claude/rules/ router/opt/vpn-director/
```

Expected: only the lines that say the old `servers.json` and `xray.subscription_url` are removed (the watch and store paragraphs, `import_server_list.sh`'s `rm -f "$DATA_DIR/servers.json"` and `del(.xray.subscription_url)` in `lib/substore.sh`).

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md README.md .claude/rules router/opt/vpn-director/lib/xrayconf.sh
git commit -m "docs: several subscriptions, the wave over them, and their files"
```

---

### Task 17: Final verification and the device check

**Files:** none new; `docs/superpowers/` leaves the branch in Step 6.

- [ ] **Step 1: The Go suite, vet and format**

Run (through `claude-forge:build-runner`): `cd server && go vet ./... && go test ./... -count=1 && go test -race ./internal/subwatch/ ./internal/service/ ./internal/webapi/ ./internal/handler/ && gofmt -l .`
Expected: PASS; `gofmt -l` lists at most `internal/ssrf/ssrf_test.go` and `internal/wizard/handler.go`.

- [ ] **Step 2: Leftovers**

Run:

```bash
grep -rn "SubscriptionURL\|SaveServers\|PublishServers\|PublishImport" server/ --include=*.go
grep -rn "servers\.json" server/ router/opt/ --include=*.go --include=*.sh
```

Expected: the first prints nothing; the second prints only the removal of the legacy file (`RemoveLegacyServers` and its callers, `rm -f "$DATA_DIR/servers.json"`).

- [ ] **Step 3: The shell suites and shellcheck**

Run in the background and read the counts from the log: `cd router/test && bats -r . > "$TMPDIR/bats.log" 2>&1; tail -5 "$TMPDIR/bats.log"` (with `TMPDIR` the session's scratchpad directory).
Expected: `0 failures`.
Run: `shellcheck router/opt/vpn-director/*.sh router/opt/vpn-director/lib/*.sh`
Expected: nothing new against `master`.

- [ ] **Step 4: Builds**

Run: `cd web && npm run build`, then from the repository root `make build-webui && make build-all`.
Expected: both daemons build for arm64 and arm; the SPA is embedded.

- [ ] **Step 5: The Web UI in dev mode**

Run `cd server && go run ./cmd/webui --dev`, log in as `admin`/`admin`, and open the Servers tab: the Subscriptions card says there is none yet, the Servers card says to add one, and `GET /api/subscriptions` answers `{"subscriptions":[]}`. Stop the server.

- [ ] **Step 6: Remove the plan documents from the branch**

The owner's rule: `docs/superpowers/` must not be in the pull request's diff; the documents stay in the branch history.

```bash
git rm -r docs/superpowers/
git commit -m "chore: remove superpowers docs from feature branch"
```

- [ ] **Step 7: The device check — only with the owner's explicit permission**

Ask the owner first. With a yes, on the owner's router:
1. Install the build.
2. Ask the owner for the two real subscription links at this point. Never write them — or a host, SNI, key or id from their bodies — into the repository, a commit, a log excerpt or a fixture.
3. Add one subscription in the Web UI and the other with `import_server_list.sh`; check both lists, their hosts, counts and statuses.
4. Select a server of each subscription in turn (Web UI, `/xray`, `configure.sh`); from a LAN client in `xray.clients`, check traffic through each.
5. Break the running server and watch the watch move the clients to the tunnel, refresh both subscriptions at once and walk into the other subscription; check the Telegram messages name `<subscription> / <server>`.
6. Rename and delete a subscription from the bot (`/subs`), and a delete of the running server's subscription (the warning, the running Xray left alone).
7. The bot's keyboards: two steps in `/xray` and `/configure`, pages of 30, `« Back`.

- [ ] **Step 8: Ask before pushing**

Report the results and ask the owner whether to push `feature/multi-subscriptions` and open the pull request.
