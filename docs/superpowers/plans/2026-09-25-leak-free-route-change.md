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

✅ Done — see commit(s): `720751b`, `2681984`

---

### Task 2: Xray — the chain and the sets swapped, clients pruned after Tunnel Director

✅ Done — see commit(s): `6fc1b00`

---

### Task 3: Tunnel Director — stable slots and the rebuild in place

✅ Done — see commit(s): `ea6daed`, `6242428`

---

### Task 4: `restart` rebuilds in place and stops nothing

✅ Done — see commit(s): `ab485cf`

---

### Task 5: `vpnconfig.MoveClient`

✅ Done — see commit(s): `3a64c0a`

---

### Task 6: `POST /api/clients/route`

✅ Done — see commit(s): `6004f70`

---

### Task 7: Web UI — the Route column moves a client

✅ Done — see commit(s): `9e5d42c`

---

### Task 8: Bot — `/clients` moves a client

✅ Done — see commit(s): `ae5d0d7`

---

### Task 9: Documentation

✅ Done — see commit(s): `bc3e272`, `633ba5e`, `e8e1681`

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
