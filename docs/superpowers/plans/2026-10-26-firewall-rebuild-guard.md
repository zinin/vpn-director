# Firewall Rebuild Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep every protected client off the WAN while the firmware's firewall rebuild has deleted
`TUN_DIR` and `XRAY_TPROXY`, and shorten that window with a snapshot the hooks restore.

**Architecture:** A new shared layer (`lib/guard.sh`) owns per-client `ip rule` guards (by source
address and LAN interface) after the fwmark rules, and a "direct" fwmark rule for exclusions. Both
chains set a direct-mark bit on what they let go direct and, on KeeneticOS, keep every protected flow
off the fast path. Every successful apply snapshots our rules (`lib/fwsnap.sh`); the firmware hooks
restore the snapshot synchronously (`lib/fwsnap-restore.sh`, POSIX sh) before queuing the full apply.

**Tech Stack:** bash 4 (sourced libraries), POSIX sh (hooks, restore script), iptables 1.4.21,
iproute2 4.4 (`ip-full`), ipset, Bats with bats-assert.

**Spec:** `docs/superpowers/specs/2026-10-26-firewall-rebuild-guard-design.md`

## Global Constraints

- Normal operation (chains in place) routes every client and destination exactly as today (spec 5.1).
- Rule preferences, with `field_max = TUN_DIR_MARK_MASK >> TUN_DIR_MARK_SHIFT` (255 by default):
  direct rule at `TUN_DIR_PREF_BASE + 2*field_max` (16894), Tunnel Director guards at the next two
  (16895, 16896), Xray guards at the two after (16897, 16898); all below 32766.
- Direct mark: `advanced.tunnel_director.direct_mark`, default `0x200`; exactly one bit, outside
  `TUN_DIR_MARK_MASK` and outside `XRAY_FWMARK_MASK`. Direct rule: `fwmark 0x200/0x200 lookup main`.
- A guard carries `from <client>` and `iif <lan>` for each of `platform_lan_ifaces`; `/32` is written
  without the suffix, as iproute2 prints it.
- Xray guard private ranges, in this order: `10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16
  224.0.0.0/4`.
- Where `platform_tunnel_offload_target` names a target (KeeneticOS: `PPE`), it applies to every flow of
  every Tunnel Director client (`main`'s included) and every Xray client's packet that `XRAY_TPROXY` lets
  through.
- Make before break: no moment without a guard for a protected client; no flush of a live chain
  (`$BATS_IPT_DIR/live_flushes` stays empty in apply tests).
- Shell conventions (`.claude/rules/shell-conventions.md`): libraries start `#!/usr/bin/env bash` and
  `set -euo pipefail`; `[[ ]]`; `log -l LEVEL`; no jq regex builtins; `ip rule show` read whole, never
  piped into `grep -q`, never `ip rule show pref N`; hooks and `lib/fwsnap-restore.sh` are POSIX sh with
  no `command` builtin (Merlin's BusyBox).
- Core modules never test the platform name.
- Tests run under `VPD_PLATFORM=merlin` by default; Keenetic tests export `VPD_PLATFORM=keenetic`.
- Commits: conventional English subjects as in `git log` (`feat(tunnel): ...`, `test: ...`,
  `docs: ...`), no trailer.
- Full suite: `bats router/test/*.bats router/test/unit router/test/integration`.

## Review Focus

- A client CIDR that holds the router's own LAN address (`192.168.1.0/24` with the router at
  `192.168.1.1`): the router's own traffic and traffic to the router must never meet a guard. Pinned in
  Task 3 ("every guard names a LAN interface").
- `exclude` entries that are IPv6 or that iptables would misread (`2001:db8::1`, `203.0.113.010`): no
  guard for them, and the apply still succeeds. Pinned in Task 3.
- Paused clients (`paused_clients`) get no guard of either kind. Pinned in Tasks 3 and 5.
- NDM calls the `netfilter.d` hook for `mangle` several times in one rebuild (6 in one interface cycle
  on 5.1.5): the restore must leave exactly one jump per chain and interface. Pinned in Task 7.
- A router without `iptables-restore`, or one whose restore fails: the hook still queues the apply and
  exits 0. Pinned in Task 7.

---

### Task 1: The guard layer and the direct-mark setting
✅ Done — see commit(s): `ffd2cca`, `e86c296`

### Task 2: `TUN_DIR` opts every flow out first and marks exclusions direct
✅ Done — see commit(s): `3d6d03d`

### Task 3: Tunnel Director guards
✅ Done — see commit(s): `9dbdd13`, `b8e0dc0`

### Task 3a: The guards and the direct rule move with pref_base (added during execution, owner decision)
✅ Done — see commit(s): `a96e786`, `41bcaf1`

### Task 4: `XRAY_TPROXY` marks what it lets through direct and keeps it off the fast path
✅ Done — see commit(s): `e99cdf9`

### Task 5: Xray guards
✅ Done — see commit(s): `b97a9ab`, `a75e149`, `2cea5a4`

### Task 6: A snapshot of our rules after every apply
✅ Done — see commit(s): `2635629`

### Task 7: The hooks restore the snapshot before they queue the apply
✅ Done — see commit(s): `3aa16de`

### Task 8: Documentation
✅ Done — see commit(s): `f205e67`

### Final review fixes (added during execution, owner decisions)
✅ Done — see commit(s): `20e333a`, `b0f71e5`, `861cee0`, `2bafea7`, `9d20d35`, `464ece7`, `5d5d911`, `c275e68`, `6627f52`, `3aa2d95`

### Task 9: Acceptance on the owner's KeeneticOS router

Run by the controller in the main session, never by a subagent, and only after the owner explicitly
allows each router change. Traffic of the owner's own sessions crosses this router.

- [ ] **Step 1: Ask the owner for permission**, naming the files to copy and the rebuild to trigger.
- [ ] **Step 2: Back up and deploy.** On `root@192.168.1.1 -p 222`: `tar czf
  /opt/vpn-director-backup-guard.tgz` of `/opt/vpn-director/lib`, `/opt/vpn-director/vpn-director.sh`,
  `/opt/vpn-director/vpn-director.json` and `/opt/etc/ndm/netfilter.d/50-vpn-director.sh`; copy the
  branch's versions of `vpn-director.sh`, `lib/*.sh` and the hook; add `"direct_mark": "0x200"` only if
  the owner wants it explicit (the default applies without it).
- [ ] **Step 3: Apply and inspect.** `vpn-director.sh apply` logs "rebuilding in place"; `ip rule show`
  holds 16894 and the `192.168.1.10` pair at 16895 (`lookup 2000`, `prohibit`); `iptables -t mangle -S
  TUN_DIR` opens with `-j PPE`; `/tmp/vpn-director/fwsnap/mangle.rules` exists.
- [ ] **Step 4: Repeat the spec's section 4 probes across a real NDM rebuild** (the trigger chosen in
  Step 1), with `tcpdump` on `mitino`'s `enp1s0` for the test addresses. Expected: 0 packets to them
  around the tunnel; keep-alive connections and the slow download survive; `mirror.yandex.ru` downloads
  directly at full rate in normal operation; the hook's `Restored the mangle rules` line, and the time
  from the rebuild to it, recorded.
- [ ] **Step 5: Asuswrt-Merlin, if the owner has a router at hand:** the same steps with
  `service restart_firewall` as the rebuild, checking in addition that its acceleration keeps an excluded
  flow direct and that bit `0x200` carries no firmware mark (`iptables-save -t mangle | grep 0x200`).
- [ ] **Step 6: Report** the measurements to the owner. On any failure, restore the backup and run
  `vpn-director.sh apply`.
