# Xray Watch: Fast Death, Return to the Preferred Server, Bounded Shell DNS — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Declare an unreachable Xray server dead after a minute, return to the user's server once it answers again, and bound the shell resolver's wait.

**Architecture:** Two additions to the Telegram bot's subscription watch (`server/internal/subwatch`). A TCP check of the active server's addresses shortens the 3-minute death rule to 60 s when nothing accepts. A healthy tick switches Xray back to `xray.preferred_server` — TCP-gated, probed, rolled back on failure, backed off. The bot wires `LoadServers` and a `tcp4` dialer. A shell helper bounds BusyBox `nslookup` through `RES_OPTIONS`.

**Tech Stack:** Go 1.25 (stdlib `net`, `log/slog`, `sync`), Bats, bash.

**Spec:** `docs/superpowers/specs/2026-09-19-xray-watch-fast-death-and-return-design.md`

## Global Constraints

- `FastDeadAfter` = 60 s; `ReachTimeout` = 3 s; `ReturnCheck` = 5 minutes; `ReturnRetry` = 10 minutes, doubling to `ReturnRetryMax` = 30 minutes.
- `DeadAfter` stays 3 minutes for every failure other than an unreachable server.
- TCP checks dial `tcp4` only. An address the bot does not dial (not IPv4, private, loopback, reserved — `ssrf.IsPrivateOrReserved`) counts as reachable without a dial.
- The return is TCP-gated, writes through `Generate` with the walk's guard (`walkGuard`), restarts with `w.restartXray()` (`restart xray-process --unless-stopped`), waits `SettleAfterRestart`, probes through SOCKS. Success sends `Xray back on the preferred server %s`; a failed attempt sends nothing.
- Shell: both `nslookup` calls in `_resolve_ip_impl` run with `RES_OPTIONS="timeout:1 attempts:2"`.
- English for code, comments, commits, docs and Telegram text; Russian only to the owner.
- Commits by theme: subject plus prose paragraphs, no trailers. Stage files by name, never `git add -A`. `.claude/settings.local.json` belongs to the owner and is never staged.
- Go commands run only through the `claude-forge:build-runner` agent (model opus); it refuses `>` redirection. Bats and shellcheck run in the main session, never while a `-race` run is going.
- TDD: watch each new test fail for the right reason before the implementation. A test that passes on the old code is a guard; say so in the task report.
- Fields of `subwatch.Watch` take end-of-line comments. A comment line between fields splits gofmt's alignment section, and a new field with a longer type realigns its whole run — the placements below avoid both.
- gofmt baseline: `internal/ssrf/ssrf_test.go`, `internal/wizard/handler.go` only. shellcheck baseline for `lib/common.sh`: SC1091 only.
- `docs/superpowers/` never reaches the PR diff: Task 6 removes it in its own commit, on the owner's go-ahead.

## File Structure

| File | Responsibility |
|------|----------------|
| `server/internal/subwatch/reach.go` (new) | Which address a server copy is dialed at; the concurrent TCP look; whether the active server is down; the unreachable streak |
| `server/internal/subwatch/return.go` (new) | Return constants; the return look, attempt, rollback and backoff |
| `server/internal/subwatch/watch.go` | Constants, fields, the death rule, the hooks into `Tick`, `settled`, the walk's pick |
| `server/internal/subwatch/reach_test.go` (new) | Unit tests of `reach.go`; fast-death tests |
| `server/internal/subwatch/return_test.go` (new) | Return tests |
| `server/internal/bot/reach.go` (new) | `reachTCP4`: the production `Reachable` |
| `server/internal/bot/reach_test.go` (new) | Tests of `reachTCP4` |
| `server/internal/bot/bot.go` | Wire `LoadServers` and `Reachable` into the watch |
| `router/opt/vpn-director/lib/common.sh` | `_resolve_nslookup`; `_resolve_ip_impl` calls it |
| `router/test/mocks/nslookup` | Records `RES_OPTIONS` when a test asks |
| `router/test/common.bats` | Tests of the bounded resolver |
| `.claude/rules/telegram-bot.md` | Architecture tree; "Subscription watch" |
| `.claude/rules/shell-conventions.md` | The DNS pitfall |

---

### Task 1: Fast death in the watch
✅ Done — see commit(s): `30163ec`

### Task 2: The bot's reachability check and the wiring
✅ Done — see commit(s): `9599155`

### Task 3: Return to the preferred server
✅ Done — see commit(s): `a20fd58`

### Task 4: Bounded shell DNS
✅ Done — see commit(s): `cbbcf28`

### Task 5: Full verification
✅ Done — no commit (verification only): `go test ./...` 24 packages, `-race` 7, bats 593, shellcheck SC1091 only

---

### Task 6: Prepare the PR (owner's go-ahead only)

**Files:**
- Delete: `docs/superpowers/specs/2026-09-19-xray-watch-fast-death-and-return-design.md`
- Delete: `docs/superpowers/plans/2026-09-19-xray-watch-fast-death-and-return.md`

- [ ] **Step 1: Drop the spec and the plan from the branch**

```bash
git rm docs/superpowers/specs/2026-09-19-xray-watch-fast-death-and-return-design.md docs/superpowers/plans/2026-09-19-xray-watch-fast-death-and-return.md
git commit -m "docs: drop superpowers spec and plan from the PR"
git ls-files docs/superpowers
```

Expected: `git ls-files docs/superpowers` prints nothing.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin feature/xray-watch-fast-death-and-return
gh pr create --base master --head feature/xray-watch-fast-death-and-return --title "feat(bot): fast death, return to the preferred server, bounded shell DNS" --body-file - <<'EOF'
The first night of the subscription watch on the RT-AX86U brought four real failovers: the provider's foreign endpoints stopped answering at the IP level for about ten minutes each. The watch handled them, at three costs this PR removes.

- **Fast death.** A failed probe also dials the active server's addresses (tcp4, 3 s, all at once). When none accepted at any look since the first miss, the outbound is dead after 1 minute instead of 3. Local failures still wait the full time.
- **Return to the preferred server.** While healthy and away from the user's server, the watch looks every 5 minutes whether it accepts TCP, switches Xray to it with the walk's guard, and keeps it on a live probe — one Telegram message. A dead probe switches back and waits 10, 20, then 30 minutes.
- **Bounded shell DNS.** `resolve_ip` runs BusyBox `nslookup` with `RES_OPTIONS="timeout:1 attempts:2"`: a lost AAAA answer cost up to 20 s inside `tproxy_apply` on Asuswrt-Merlin.

Verified: go test (24 packages), -race on 7, bats 593, shellcheck baseline. Device check pending.
EOF
```

Expected: the PR URL. Report it to the owner in Russian.
