# Bounded returns: design

Date: 2026-10-13
Branch: `fix/bounded-returns`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Returns to a flaky preferred server must end after a bounded number of failures, and no return may
skip `ReturnAlive` because the monitor is re-checking. Both changes are in `vpn-director-watchd`'s
subscription watch (`server/internal/subwatch`) and ship as v0.21.1:

1. The count of failed returns measures the user's choice alone. The death of a server the watch
   switched to itself no longer starts the returns over.
2. While the monitor is on and OK but has no evidence for the servers yet, the return waits for it
   instead of taking the TCP look.

Raising `monitor.concurrency` on both routers, the third change of this round, is router
configuration and needs no design.

## 2. Where it stands

v0.21.0 has run on both routers since 2026-10-13, about 11:40 MSK.

**The KN-4521 never stops returning** (times UTC, names pseudonymized). The user's choice `S1/N1`
is flaky: returns at 14:32, 15:14, 15:58 and 17:24 each died 6 to 27 minutes later.
`returnAfterDeath` (`return.go`) counts a death that starts within `ReturnHold` of a return, on the
record that return wrote, as a failed return; any other death resets `returnFails`, `returnRetry`
and `returnNotBefore`. At 15:36:56 the replacement `S2/N2` died and reset the count, so the failed
return at 16:06:22 backed off 10 minutes instead of 30, and the next one 20. With two flaky
providers the count never reaches `ReturnFailsMax`: return, death, replacement, return, each cycle
two Telegram messages and several Xray restarts.

**The RT-AX86U returns without `ReturnAlive`** (times MSK). A list publication that changes the
monitor's endpoint set - new servers, or new addresses, since keys are per address - makes the
monitor merge the set (`merge`) and start a new prober (`ensureSession`). Both invalidate the
evidence (`invalidateEvidence`), and for 20 to 30 seconds `Evidence()` holds no result.
`walkEvidence` (`best.go`) then finds no key and reports no fresh evidence, and `maybeReturn` takes
the TCP look. At 17:36:16 `S1` published `added=50 removed=50`; at 17:36:45 the watch returned to
`S1/N5`; at 17:36:46 the monitor saw `N5` alive for the first time. The same happened at 18:52.
Those returns held, but `S2` publishes new addresses every 5 minutes, so the windows recur.

## 3. Decisions

| Question | Decision |
|---|---|
| The death of a record no return wrote | Changes nothing: the count, the backoff and the wait stay |
| What starts the returns over | A return that held `ReturnHold`, a selection, a watchd restart |
| How the watch recognizes a selection | The first tick that reads a record the watch did not write (`seq` other than `ownSeq`), once per record |
| Xray back on the user's choice without a return, with failed returns counted | Counts as a return: a death within `ReturnHold` is a failed return; holding `ReturnHold` starts the returns over |
| After `ReturnFailsMax` | The returns stay stopped until a selection, a watchd restart, or Xray holding on the choice (row above); nothing else resumes them |
| `ReturnAlive` | Stays 5 minutes |
| The monitor on and OK without evidence for the servers | No look; the next tick looks again |
| The TCP look | Only with monitoring off, without `Health`, or with a monitor that is not OK |

## 4. Design

### 4.1 What the count belongs to

`returnFails`, `returnRetry` and `returnNotBefore` belong to the user's choice: the preferred server
and its group. These events change them, and no others:

| Event | Effect |
|---|---|
| A return whose probe fails (`tryReturn`) | A failed return: the next waits 10, 20, then 30 minutes; after `ReturnFailsMax` the returns stop (unchanged) |
| The record a return wrote (`failSeq == returnSeq`) dies, starting within `ReturnHold` of the return | A failed return (unchanged) |
| That record dies, starting at `ReturnHold` or later | The return held: the count, the backoff and the wait end (unchanged) |
| A return keeps working for `ReturnHold` (`maybeReturn`) | The count and the backoff end (unchanged) |
| **Any other record dies**: a replacement's, the fast path's, the walk's, a follow's | **Nothing changes.** `lastReturn` is cleared, as today |
| **A selection** (4.2) | **The returns start over**: the count, the backoff, the wait and `lastReturn` |
| **The watch's own switch puts Xray back on the user's choice while the count is not zero** (4.3) | It counts as a return from the first look that finds Xray there |
| A watchd restart | The returns start over: the state lives in memory |

At an intervening death the wait stays too. Whatever puts Xray back on a working server - the fast
path, the keeper's switch, a restore - runs `settled()`, so the next look comes once the backoff has
passed and at least `ReturnCheck` after that.

### 4.2 Selections

`Tick` notes a selection once it has read the config, before it acts in either mode. The record's
write counter is `ActiveSeq(cfg.Xray.ActiveServer)`. It is a selection when it is neither the
watch's last own write (`ownSeq`, which `generate` sets) nor the selection last noted (a new field,
`selectedSeq`). A selection starts the returns over and records its counter in `selectedSeq`.

Every writer of the record moves its counter (`RecordActiveServer`, and `configure.sh` the same
way); a rename does not. So the note catches the Web UI, `/xray`, the wizard and `configure.sh`, a
second selection of the same server, and a selected server that never passes a probe, whose death
would otherwise hand the old count to the new choice. The first tick of a new process (`ownSeq`
zero) notes the record it reads as well, which only repeats what the restart did. `missed` already
tells the user's records from the watch's by `ownSeq`; the returns use the same test.

### 4.3 Xray back on the choice without a return

The fast path, the keeper's replacement and the walk rank the alive servers of the user's group and
subscription first, so they can switch Xray to the user's choice itself. `RecordWalkedServer` then
clears `preferred_server`, and today the next look of `maybeReturn` resets the count, since nothing
is left to return to. A flaky choice that the watch keeps landing on would cycle forever.

`maybeReturn`'s branch for an absent `preferred_server` changes. When `active_server` names a
server, no return waits to hold (`lastReturn` zero) and the count is not zero, the look takes the
record Xray runs for a return: `lastReturn` becomes now, `returnSeq` the record's counter and
`returnGroup` empty. Its death within `ReturnHold` counts as a failed return; once it has held
`ReturnHold`, the existing check ends the count. With a zero count nothing happens, and a selection
arrives here with the count already zero (4.2).

This also ends a stop: a choice Xray has run for `ReturnHold` starts the returns over, as a held
return does.

### 4.4 The look while the monitor re-checks

`walkEvidence(servers, cfg)` returns a third value, `waiting`: monitoring on, `Health` present and
`Evidence().State` OK, but no key of `servers` in the evidence, or `ValidateEvidence` refused it (a
check completed between the snapshot and the validation). `bestWalkOrder` ignores it.

With `waiting`, `maybeReturn` makes no look: no TCP dial, no attempt, no return to the group.
`returnNotBefore` goes back to the value it had before the look, so the next tick looks again, and
a DEBUG line says why. A window lasts about 30 seconds; a skip that waited `ReturnCheck` could keep
landing in the RT-AX86U's 5-minute publications.

The TCP look remains for monitoring off, a missing `Health`, and a monitor in any state but OK
(`wan_down`, `prober_error`, `no_xray`, `stopped`, `disabled`, `not_running`). That is what section
4.6 of the v0.21.0 design meant by "without evidence".

### 4.5 Log

- The stop: `Returns to the preferred server stopped after failed attempts; a selection or a watchd
  restart starts them again`, and the same change in the group variant.
- The death of a record Xray got back to without a return logs the existing line
  (`The preferred server died soon after the return; counted as a failed return`).
- The skipped look: DEBUG `Return to the preferred server put off; the monitor has no evidence yet`.
- No new Telegram message.

## 5. Compatibility

No config, file or IPC change: the state stays in watchd's memory. Only `vpn-director-watchd`
changes.

## 6. Known limits

- A replacement after a death still ranks the user's subscription first, stopped returns or not,
  and can pick another server of the flaky subscription. That is the deferred "replacements try many
  dead candidates".
- In the monitor's re-check windows the walk and the replacement lose the alive-first order
  (`bestWalkOrder` without evidence). Deferred.
- A stop sends nothing to Telegram; the INFO line is all.
- A record whose save failed after its file was written reads as a selection at the next tick, and
  the returns start over: the harmless direction.
- A deleted subscription takes `preferred_server` with it; a count left from that choice then counts
  for the server Xray runs (4.3).

## 7. Testing

Go, every test written first and seen failing:

- Task 1:
  - the KN-4521 cycle: a return dies within `ReturnHold`, then the replacement dies; the count and
    the wait stay, and the next failed return backs off 20 minutes;
  - returns stopped at `ReturnFailsMax` stay stopped when the server the watch runs dies
    (`TestTick_ADeathStartsStoppedReturnsOver` changes);
  - another server's death keeps the backoff (`TestTick_ADeathStartsTheReturnsOver` changes);
  - a selection starts the returns over at once: within `ReturnHold` after a return, for a selected
    server that never passes a probe, and for the same server selected again;
    `TestTick_ASelectionStartsStoppedReturnsOver` selects with a moved counter, as a real selection
    does;
  - Xray back on the choice through the fast path, a replacement or the walk with a count: a death
    within `ReturnHold` is a failed return, a hold starts the returns over;
    `TestTick_NoPreferredServerStartsTheReturnsOver` becomes that hold;
  - keeper: in `TestKeeper_AnActualityReplacementSettlesTheReturns`, "no return within ReturnHold"
    keeps the count (its record the watch's own), and a variant with a selected record starts over;
  - fixtures that preset the count set `ownSeq` to their record's counter, or the first tick takes
    the record for a selection.
- Task 2:
  - the monitor OK without evidence: no switch and no TCP dial of the preferred server; the next tick
    looks again; the return comes once the monitor has held the server alive for `ReturnAlive`;
  - the monitor OK, the validation refused: the same;
  - monitoring off, `Health` missing, a monitor not OK: the TCP look as before (a table).
- `make -C server test`; `go test -race` for `subwatch` and `cmd/watchd`; builds for arm64, arm and
  mipsle.

A test that changes, changes because its rule did.

## 8. Documentation

- `.claude/rules/watchd.md`: the returns in "Subscription watch and failover" and in "Keeping Xray on
  a working server".
- `.claude/rules/telegram-bot.md`: the return paragraph of "Subscription watch (owned by watchd)".
- `README.md`, `README.ru.md`: "without valid monitor evidence, once it accepts connections again"
  becomes monitoring off or not working.
- Code comments: `ReturnHold`, `ReturnFailsMax`, `maybeReturn`, `returnAfterDeath`,
  `backOffReturn`, `walkEvidence`, the new field.

## 9. Release and verification

A PR without the planning documents, Codex's review, the merge, and the annotated tag `v0.21.1` on
the merge commit, which carries `1dadee6` as well. Release notes follow the v0.21.0 style. The owner
updates both routers with the Update button. Then, read-only:

- KN-4521: `counted as a failed return` and a backoff that grows across a replacement's death; the
  stop line once four returns have failed;
- RT-AX86U: after `Subscription list published` with new keys, no `Returning to the preferred
  server` before the monitor has held that server alive for 5 minutes;
- both: no new watchd WARN, and Xray's SOCKS probe answers 204.
