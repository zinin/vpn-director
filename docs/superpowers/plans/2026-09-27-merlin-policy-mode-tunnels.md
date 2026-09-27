# Merlin Policy-Mode Tunnels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On Asuswrt-Merlin, Tunnel Director neither offers nor routes through an OpenVPN client whose "Redirect Internet traffic through tunnel" is not "VPN Director (policy rules)", and it takes out the default route it left in such a client's table in "No" mode.

**Architecture:** Every firmware fact stays in `lib/platform/merlin.sh`. One predicate, `_merlin_tunnel_routable`, filters `platform_tunnels`, makes `platform_tunnel_route_ensure` refuse and gates the cleanup. A new contract function, `platform_tunnel_unlisted_reason`, names the reason, and `lib/tunnel.sh` prints it in its two warnings without testing the platform. KeeneticOS gets the new function as a stub that returns 1. Nothing under `server/` or `web/` changes.

**Tech Stack:** Bash 5 (Entware's on the router), bats-core with bats-support and bats-assert, shellcheck 0.11, the `nvram`, `ip` and `iptables` mocks under `router/test/mocks`.

**Spec:** `docs/superpowers/specs/2026-09-27-merlin-policy-mode-tunnels-design.md` (read it first: section 2 explains the firmware behaviour every task relies on).

## Global Constraints

- A routable OpenVPN client has `vpn_clientN_rgw` = `2`. `0`, `1` and an empty value are refused. "Mode 0" means `0` or an empty value (the firmware reads an empty value as 0).
- `wgcN`, `main` and KeeneticOS behave as they do today. Nothing under `server/` or `web/` changes.
- Only `_merlin_tunnel_routable` states the rule: no other code compares `vpn_clientN_rgw` with `2`.
- The cleanup command is exactly `ip route del default dev tun1N table ovpncN`, its failure ignored, run only for an `ovpncN` in mode 0 and never in mode 1.
- The reason line is exactly `OpenVPN client N is not in VPN Director mode ("Redirect Internet traffic through tunnel" is "<setting>")`, where `<setting>` is `No` for `0` or an empty value, `Yes (all)` for `1` and the raw value otherwise.
- The warnings are exactly `Tunnel '<id>' is skipped: <reason>` and `Tunnel '<id>': route not installed: <reason>; Tunnel Director does not route through it`. Without a reason, today's texts stay word for word.
- Platform functions never call `log`: `configure.sh` loads the contract without `common.sh`. The core never tests `VPD_PLATFORM`.
- Shell style (`.claude/rules/shell-conventions.md`): `[[ ]]`, `local` for every function variable, no `tr '[:upper:]'`, never `cmd | grep -q` on a live listing.
- shellcheck stays at 53 findings: `shellcheck -f gcc $(git ls-files 'router/*.sh') | wc -l` prints `53`.
- Run single bats files directly. Run the whole suite (`bats router/test/*.bats router/test/unit router/test/integration`) in the background, its output in a log file, and read the counts from the log.
- Stage files by name. Never touch or stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, the other files under `docs/superpowers/plans/`, `review.diff` or `test_exit.sh`: they are the owner's.
- Commit messages follow the house style: `type(scope): subject`, a blank line, one plain paragraph, no trailers.

## Review Focus

1. A client switched from VPN Director to "No" while the config stays, followed by `restart` or `restart tunnel` (`TUN_DIR_FORCE_REBUILD=1`): the rebuild must drop the recorded slot, its ip rule and its default. Pinned in Task 4.
2. `vpn-director.sh stop` with a recorded `ovpncN` in "No" mode: `tunnel_stop` must take the default out. Pinned in Task 2.
3. A failover tunnel whose client is in "No" mode: no slot and no `failover_ready`, so the watch keeps Xray membership. Pinned in Task 4.
4. An nvram that answers nothing for any key: `vpn-director.sh platform` lists the WireGuard tables and no OpenVPN client, and exits 0. Pinned in Task 1.
5. A recorded client in "Yes (all)" on the up-to-date branch: the firmware's default is neither replaced nor deleted. Pinned in Task 2.

## File Structure

| File | Change | Task |
|---|---|---|
| `router/opt/vpn-director/lib/platform/merlin.sh` | `_merlin_ovpn_rgw`, `_merlin_tunnel_routable`, a filtered `platform_tunnels` (1); `_merlin_drop_default`, a refusing `platform_tunnel_route_ensure`, a cleaning `platform_tunnel_table_release` (2); `platform_tunnel_unlisted_reason` (3) | 1, 2, 3 |
| `router/opt/vpn-director/lib/platform/keenetic.sh` | `platform_tunnel_unlisted_reason`, returning 1 | 3 |
| `router/opt/vpn-director/lib/platform.sh` | the contract header | 3 |
| `router/opt/vpn-director/lib/tunnel.sh` | the reason in two warnings; comments; the header | 4 |
| `router/test/mocks/nvram` | `vpn_client1_rgw` and `vpn_client2_rgw` answer `2` | 1 |
| `router/test/unit/platform_merlin.bats` | the `with_ovpn_modes` helper and tests | 1, 2, 3 |
| `router/test/unit/platform_keenetic.bats` | one test | 3 |
| `router/test/unit/tunnel.bats` | the `ovpn_mode` and `tun11_addressed` helpers and end-to-end tests | 2, 4 |
| `router/test/integration/vpn_director.bats` | two `platform` tests | 1 |
| `CLAUDE.md`, `.claude/rules/tunnel-director.md`, `README.md`, `README.ru.md` | documentation | 5 |

How the tests see the firmware: `test_helper.bash` sets `VPD_PLATFORM=merlin`, puts `router/test/mocks` first in `PATH` and points `RT_TABLES_FILE` at `router/test/fixtures/rt_tables`, which names `wgc1`, `wgc2`, `ovpnc1` and `ovpnc2`. The `ip` mock appends every call, as `ip <args>`, to `/tmp/bats_ip_calls.log`, keeps the ip rules it was given in `/tmp/bats_test_ip_rules`, and answers `ip -4 -o addr show <iface>` from `$BATS_IP_ADDRS_FILE` (`<iface> <cidr>` per line) when that is set. `log` prints to stderr, which `run` captures, and appends to `$LOG_FILE`.

---

### Task 1: Merlin lists an OpenVPN client only in VPN Director mode

✅ Done — see commit(s): `a705698`, `5983d56`

---

### Task 2: No default into the table of such a client, and the stray one taken out

✅ Done — see commit(s): `4804374`

---

### Task 3: The contract's `platform_tunnel_unlisted_reason`

✅ Done — see commit(s): `d61e562`

---

### Task 4: Tunnel Director says why

✅ Done — see commit(s): `93677f1`

---

### Task 5: Documentation and the final check

✅ Done — see commit(s): `263fd31`

---

## Finishing (with the owner, after Task 5)

- Ask the owner before any push and before opening a PR; the PR follows the house style of #54-#61.
- Before the PR, remove this plan and its spec from the branch (they stay in its history):

  ```bash
  git rm docs/superpowers/specs/2026-09-27-merlin-policy-mode-tunnels-design.md \
      docs/superpowers/plans/2026-09-27-merlin-policy-mode-tunnels.md
  git commit -m "docs: remove the design and plan documents before the PR"
  ```

- The PR description, and the v0.18.1 release notes if the owner releases, carry the one-time
  cleanup for a tunnel Tunnel Director no longer has on record: restart that OpenVPN client in the
  router UI, or run `ip route del default table ovpncN`.
- Post `@codex review` only with the owner's permission.
- After a release, read-only on the RT-AX86U (`ssh -o BatchMode=yes -p 2222 admin@192.168.1.1`):
  `/opt/vpn-director/vpn-director.sh platform` lists neither `ovpnc1` nor `ovpnc5`.
