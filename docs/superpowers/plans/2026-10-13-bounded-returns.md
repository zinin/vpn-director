# Bounded Returns Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Returns to a flaky preferred server end after a bounded number of failures, and no return skips `ReturnAlive` while the monitor re-checks a new endpoint set.

**Architecture:** Everything changes in `vpn-director-watchd`'s subscription watch, `server/internal/subwatch`. The count of failed returns becomes the preferred server's alone: `returnAfterDeath` no longer resets it at deaths no return wrote (Task 1), the new `noteSelection` resets it at the first tick that reads a record the watch did not write (Task 2), and `maybeReturn` takes Xray back on the user's choice by the watch's own switch for a return (Task 3). `walkEvidence` reports a monitor that is OK but holds no evidence yet (`waiting`), and `maybeReturn` then waits for the next tick instead of taking the TCP look (Task 4). Documentation follows (Task 5), then whole-branch verification (Task 6) and the release (Task 7).

**Tech Stack:** Go 1.25 (module `github.com/zinin/vpn-director/server`), standard `testing`, `log/slog`; Markdown docs.

**Spec:** `docs/superpowers/specs/2026-10-13-bounded-returns-design.md`

Out of scope: raising `monitor.concurrency` on the routers (router configuration, done separately with the owner's permission) and every item the spec lists under "Known limits".

## Global Constraints

- Only `server/internal/subwatch/{return.go,watch.go,best.go}`, their tests and four documents change. No config, file, IPC or shell change; the bot and the Web UI do not change.
- Constants keep their values: `ReturnCheck` 5m, `ReturnRetry` 10m, `ReturnRetryMax` 30m, `ReturnHold` 30m, `ReturnFailsMax` 4, `ReturnAlive` 5m.
- No new Telegram message. Log lines exactly as spec 4.5 has them: the stop line ends `; a selection or a watchd restart starts them again` (both variants), the skipped look logs DEBUG `Return to the preferred server put off; the monitor has no evidence yet`, and `The preferred server died soon after the return; counted as a failed return` stays as it is.
- English for code, comments, tests, commit messages and docs. Comments follow the voice of `return.go`: full sentences that say why.
- Commits: `type(scope): summary`, a body that explains why, no trailers. Stage files by name only. Never stage `.claude/settings.local.json`, `docs/session-transfer-*.md`, `docs/superpowers/2026-10-09-*.md`, `docs/superpowers/plans/*-prompt.md`, `review.diff`, `test_exit.sh`.
- TDD: every new test is written first and seen failing for the reason its step names. A test marked **guard** pins behaviour that must not change and passes before and after.
- An existing test changes only where this plan says so, and only because the rule it pinned changed. Any other test that fails is a stop-and-ask, not a fix.
- `gofmt -l ./internal/subwatch/` prints nothing before each commit; `go vet ./internal/subwatch/` is clean.
- Commands run from `server/`, the Go module root. In the main session, builds and tests go through the `build-forge:build-runner` agent, model `opus`; subagents are `opus` too.

## Review Focus

1. **Keeper mode, the KN-4521's own path.** The keeper replaces a dead replacement the legacy way (`replaceDead`); the count of failed returns to the preferred server must survive it. Test: `TestKeeper_AReplacementsDeathKeepsTheCountOfFailedReturns` (Task 1).
2. **A selection while the clients are on the fallback tunnel.** `noteSelection` must run before the failover branch of `Tick`, or a selection made during a failover would not start the returns over. Test: `TestTick_ASelectionDuringAFailoverStartsTheReturnsOver` (Task 2).
3. **Keeper mode, the fast path lands on the user's choice.** With failed returns counted, Xray back on the choice must count as a return through the real fast path, not only through a direct write. Test: `TestKeeper_AFastSwitchBackToTheChoiceCountsAsAReturn` (Task 3).
4. **Keeper mode waits for the monitor too.** Both routers run the keeper; its return must wait in a re-check window and look again at the next tick. Test: `TestMaybeReturn_WaitsWhileTheMonitorRechecks` (Task 4).
5. **A re-keyed preferred server while the monitor knows other keys** (the RT-AX86U's publication). The evidence is fresh, the preferred server is not in it: no TCP look, no return until its new key has been alive for `ReturnAlive`. Test (guard): `TestTick_AReKeyedPreferredServerWaitsForReturnAlive` (Task 4).

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `server/internal/subwatch/return.go` | `ReturnHold`/`ReturnFailsMax` comments, `maybeReturn`, `returnAfterDeath`, new `noteSelection`, `backOffReturn` log | 1, 2, 3, 4 |
| `server/internal/subwatch/watch.go` | field `selectedSeq`; `Tick` calls `noteSelection` | 2 |
| `server/internal/subwatch/best.go` | `walkEvidence` returns `waiting`; `bestWalkOrder` ignores it | 4 |
| `server/internal/subwatch/return_test.go` | rig helpers `ownWrite`, `osloLooks`; return tests | 1, 2, 3, 4 |
| `server/internal/subwatch/keeper_test.go` | keeper and `maybeReturn` tests | 1, 2, 3, 4 |
| `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md`, `README.md`, `README.ru.md` | the returns' rules | 5 |

Test helpers already in the package that the tests below use: `newReturnRig`, `returnServers`, `osloIP`, `madridIP`, `failReturns`, `selectServer`, `(*returnRig).tick`, `(*returnRig).attempts` (`return_test.go`); `newKeeperFixture`, `newReturnKeeper`, `newGroupReturnKeeper`, `groupReturnStates`, `aliveSince`, `writtenNames`, `(*fastFixture).missOnce`, `setStates`, `setHealthOf` (`keeper_test.go`); `fastHealth`, `fastServer` (`fast_test.go`); `tickUntilDead`, `tickFor`, `connected`, `errProbe` (`watch_test.go`). `returnRig` has no `Fetch`, so a death there moves the clients to the tunnel and no wave walks: a test writes the walk's record itself, through `ownWrite`.

---

### Task 1: A death no return wrote leaves the count of failed returns

**Files:**
- Modify: `server/internal/subwatch/return.go` (`ReturnFailsMax` comment, `returnAfterDeath`, the stop lines of `backOffReturn`)
- Test: `server/internal/subwatch/return_test.go`, `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: test helpers on `*returnRig`, used by Tasks 2-4:
  - `func (r *returnRig) ownWrite(t *testing.T, s vpnconfig.Server)` - writes `s` through `r.w.generate`, so `ownSeq` moves to its record, and Xray (`r.running`) runs its first address.
  - `func (r *returnRig) osloLooks() *int` - wraps `r.w.Reachable` and counts the TCP looks at `osloIP` from then on.

- [ ] **Step 1: Add the rig helpers to `return_test.go`**

Insert right after `func (r *returnRig) attempts() int { ... }` (line ~97-105):

```go
// ownWrite has the watch write s as the running server the way its walk, fast
// path or replacement does - through generate - so the record is the watch's
// own, and Xray runs s's first address.
func (r *returnRig) ownWrite(t *testing.T, s vpnconfig.Server) {
	t.Helper()
	if generated, _, err := r.w.generate(s, nil); !generated || err != nil {
		t.Fatalf("writing %s: generated %v, error %v", s.Name, generated, err)
	}
}

// osloLooks counts the TCP looks at Oslo's address from now on.
func (r *returnRig) osloLooks() *int {
	looks := 0
	reachable := r.w.Reachable
	r.w.Reachable = func(ctx context.Context, ip string, port int) bool {
		if ip == osloIP {
			looks++
		}
		return reachable(ctx, ip, port)
	}
	return &looks
}
```

- [ ] **Step 2: Replace `TestTick_ADeathStartsStoppedReturnsOver` (`return_test.go` ~237-250) with:**

```go
// Stopped returns stay stopped when the server the watch runs dies: Madrid,
// which the failed returns went back to, says nothing about Oslo. Once Madrid
// works again, the watch does not look at Oslo.
func TestTick_StoppedReturnsStayStoppedThroughADeath(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)
	r.live[madridIP] = false // Madrid dies
	r.f.plat = connected("ovpnc2")
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnFails != ReturnFailsMax || r.w.returnRetry != ReturnRetryMax {
		t.Fatalf("returnFails %d, returnRetry %v; Madrid's death leaves the returns stopped", r.w.returnFails, r.w.returnRetry)
	}
	r.live[madridIP] = true // Madrid works again, and the clients come back to Xray
	looks := r.osloLooks()
	tickFor(r.w, r.f, time.Hour)
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the clients must be back on Xray")
	}
	if *looks != 0 || r.attempts() != ReturnFailsMax {
		t.Fatalf("%d looks at Oslo, %d attempts; the returns have stopped", *looks, r.attempts())
	}
}
```

- [ ] **Step 3: Replace `TestTick_ADeathStartsTheReturnsOver` (`return_test.go` ~592-610) with:**

```go
// The returns belong to the preferred server. Madrid, the server a failed
// return went back to, dies: a death no return wrote, which says nothing about
// Oslo, and the count, the backoff and the wait stay.
func TestTick_ADeathOfAnotherServerKeepsTheBackoff(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick() // a failed return: the next waits ReturnRetry
	if r.w.returnFails != 1 || r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; want a failed return", r.w.returnFails, r.w.returnRetry)
	}
	next := r.w.returnNotBefore
	r.live[madridIP] = false // Madrid dies
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnFails != 1 || r.w.returnRetry != ReturnRetry || !r.w.returnNotBefore.Equal(next) {
		t.Fatalf("returnFails %d, returnRetry %v, next return %v; Madrid's death keeps the count and the wait of %v",
			r.w.returnFails, r.w.returnRetry, r.w.returnNotBefore, next)
	}
}
```

- [ ] **Step 4: Add after `TestTick_ADeathSoonAfterAReturnCountsTowardTheCap` (`return_test.go` ~787-820):**

```go
// The KN-4521 on 2026-10-13: every return to a flaky preferred server died
// within minutes, and the death of the replacement in between started the count
// over, so the returns never stopped. The count is Oslo's: Madrid's death leaves
// it, and the next failed return waits twice as long.
func TestTick_AReplacementsDeathKeepsTheCountOfFailedReturns(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP], r.live[osloIP] = true, true
	r.f.plat = connected("ovpnc2")
	r.tick()                                    // the return: Oslo runs again
	r.up[osloIP], r.live[osloIP] = false, false // and dies again
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.w.returnFails != 1 {
		t.Fatalf("returnFails %d; Oslo's death soon after the return is a failed return", r.w.returnFails)
	}
	// A walk puts Madrid in Oslo's place, and the clients come back to Xray.
	r.ownWrite(t, returnServers()[1])
	r.live[madridIP] = true
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the clients must be back on Xray")
	}
	// Madrid dies in its turn: a death no return wrote.
	r.live[madridIP] = false
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.w.returnFails != 1 || r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; Madrid's death says nothing about Oslo", r.w.returnFails, r.w.returnRetry)
	}
	// Madrid comes back, and Oslo accepts TCP but refuses the proxy: the next
	// return fails, and the one after it waits twice as long.
	r.live[madridIP] = true
	r.up[osloIP] = true
	attempts := r.attempts()
	for end := r.f.now.Add(time.Hour); r.attempts() == attempts && r.f.now.Before(end); r.f.now = r.f.now.Add(ProbeInterval) {
		r.tick()
	}
	if r.attempts() != attempts+1 || r.w.returnFails != 2 || r.w.returnRetry != 2*ReturnRetry {
		t.Fatalf("%d attempts, returnFails %d, returnRetry %v; want the second failed return, backed off %v",
			r.attempts()-attempts, r.w.returnFails, r.w.returnRetry, 2*ReturnRetry)
	}
}
```

- [ ] **Step 5: In `keeper_test.go`, replace the doc comment and the subtest `"no return within ReturnHold"` of `TestKeeper_AnActualityReplacementSettlesTheReturns` (~575-615)**

New doc comment above `func TestKeeper_AnActualityReplacementSettlesTheReturns`:

```go
// The actuality check's replacements settle the returns once per death, as
// replaceDead and failOutbound do: as far as Xray goes, a server that left its
// subscription, or whose followed entry does not answer either, died. Within
// ReturnHold of a return, on the record that return wrote, the death counts as
// a failed return and backs the next one off. The death of a record the watch
// wrote itself leaves the count as it was, stopped returns included.
```

The subtest `"no return within ReturnHold"` becomes (the subtest `"a return within ReturnHold"` stays as it is in this task):

```go
			t.Run("a record the watch wrote", func(t *testing.T) {
				s := newKeeperFixture(t)
				kind.change(s)
				// The watch put Xray on Oslo itself, and the returns had stopped.
				s.w.ownSeq = vpnconfig.ActiveSeq(s.f.cfg.Xray.ActiveServer)
				s.w.returnFails, s.w.returnRetry = ReturnFailsMax, ReturnRetryMax
				s.missOnce(t)
				s.w.Tick(context.Background())
				if got := writtenNames(s.written); !reflect.DeepEqual(got, kind.written) {
					t.Fatalf("written %v, want %v", got, kind.written)
				}
				if s.w.returnFails != ReturnFailsMax || s.w.returnRetry != ReturnRetryMax {
					t.Fatalf("returnFails %d, returnRetry %v; the death of a server the watch switched to says nothing about the preferred server",
						s.w.returnFails, s.w.returnRetry)
				}
			})
```

- [ ] **Step 6: Add after `TestKeeper_AnActualityReplacementSettlesTheReturns` in `keeper_test.go` (Review Focus 1)**

```go
// The KN-4521 runs the keeper. Backup, which the watch put in Oslo's place
// after returns to Oslo had failed, dies, and the keeper replaces it the legacy
// way - Oslo first, still dead, then Spare: a death no return wrote, and the
// count of failed returns to Oslo stays.
func TestKeeper_AReplacementsDeathKeepsTheCountOfFailedReturns(t *testing.T) {
	s := newReturnKeeper(t) // Xray on Backup, Oslo preferred
	s.f.subs[0].Servers = append(s.f.subs[0].Servers, fastServer("Spare", "spare.example", "198.51.100.30"))
	s.w.Health = nil
	s.w.ownSeq = vpnconfig.ActiveSeq(s.f.cfg.Xray.ActiveServer) // the watch put Xray on Backup
	s.w.returnFails, s.w.returnRetry = 2, 2*ReturnRetry
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if len(s.written) > 0 && s.written[len(s.written)-1].Name == "Spare" {
			return nil
		}
		return errProbe // Backup has died, and Oslo is still dead
	}
	tickUntilDead(s.w, s.f)
	names := writtenNames(s.written)
	if len(names) == 0 || names[0] != "Oslo@oslo.example" || names[len(names)-1] != "Spare@spare.example" {
		t.Fatalf("written %v, want Oslo tried first and Spare last", names)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept", p)
	}
	if s.w.returnFails != 2 || s.w.returnRetry != 2*ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; Backup's death says nothing about Oslo", s.w.returnFails, s.w.returnRetry)
	}
}
```

- [ ] **Step 7: Run the new tests and watch them fail**

Run: `go test ./internal/subwatch/ -count=1 -run '^(TestTick_StoppedReturnsStayStoppedThroughADeath|TestTick_ADeathOfAnotherServerKeepsTheBackoff|TestTick_AReplacementsDeathKeepsTheCountOfFailedReturns|TestKeeper_AnActualityReplacementSettlesTheReturns|TestKeeper_AReplacementsDeathKeepsTheCountOfFailedReturns)$'`

Expected: FAIL, each because the current `returnAfterDeath` resets the count at any death:
- `TestTick_StoppedReturnsStayStoppedThroughADeath`: `returnFails 0, returnRetry 0s; Madrid's death leaves the returns stopped`
- `TestTick_ADeathOfAnotherServerKeepsTheBackoff`: `returnFails 0, returnRetry 0s, next return 0001-01-01 00:00:00 +0000 UTC; Madrid's death keeps the count and the wait of ...`
- `TestTick_AReplacementsDeathKeepsTheCountOfFailedReturns`: `returnFails 0, returnRetry 0s; Madrid's death says nothing about Oslo`
- `TestKeeper_AnActualityReplacementSettlesTheReturns/*/a_record_the_watch_wrote` (both kinds): `returnFails 0, returnRetry 0s; the death of a server the watch switched to says nothing about the preferred server`
- `TestKeeper_AReplacementsDeathKeepsTheCountOfFailedReturns`: `returnFails 0, returnRetry 0s; Backup's death says nothing about Oslo`

The subtests `a_return_within_ReturnHold` pass.

- [ ] **Step 8: Implement in `return.go`**

`ReturnFailsMax`'s comment (lines ~31-37) becomes:

```go
	// ReturnFailsMax is how many returns in a row may fail - a death within
	// ReturnHold of one included - before the watch stops trying. An address
	// that accepts TCP and keeps refusing the proxy, a blocked endpoint, would
	// otherwise cost every Xray client a few dead seconds every ReturnRetryMax
	// for as long as the block lasts. The count is the preferred server's: the
	// death of a server the watch switched to itself leaves it. A selection, a
	// held return or a restart of watchd starts the returns over.
	ReturnFailsMax = 4
```

`returnAfterDeath` (lines ~412-434) becomes:

```go
// returnAfterDeath settles the returns at a death. The count is the preferred
// server's: a death that starts within ReturnHold of a return, on the record
// that return wrote, is a failed return - the server it went back to, the
// preferred server or a server of its group, has failed again - so a server
// that flaps is not returned to every few minutes; the same record dying later
// held, and the returns start over. The death of any other record - one the
// watch switched to itself: a replacement's, the fast path's, the walk's, a
// follow's - says nothing about the preferred server and leaves the count and
// the wait as they are. A selection since the return wrote another record, so
// its death is not the return's either.
func (w *Watch) returnAfterDeath() {
	switch {
	case w.lastReturn.IsZero() || w.failSeq != w.returnSeq:
		// Not the record a return wrote: nothing about the preferred server.
	case w.failSince.Sub(w.lastReturn) < ReturnHold:
		if w.returnGroup == "" {
			slog.Info("The preferred server died soon after the return; counted as a failed return", "after", w.failSince.Sub(w.lastReturn))
		} else {
			slog.Info("The server of the preferred group died soon after the return; counted as a failed return", "group", w.returnGroup, "after", w.failSince.Sub(w.lastReturn))
		}
		w.backOffReturn(w.returnGroup)
	default:
		w.returnRetry = 0
		w.returnFails = 0
		w.returnNotBefore = time.Time{}
	}
	w.lastReturn = time.Time{}
}
```

In `backOffReturn`, the two stop lines become:

```go
			slog.Info("Returns to the preferred server stopped after failed attempts; a selection or a watchd restart starts them again", "failed", w.returnFails)
```

```go
			slog.Info("Returns to the preferred server and its group stopped after failed attempts; a selection or a watchd restart starts them again", "group", group, "failed", w.returnFails)
```

- [ ] **Step 9: Run the package and see it pass**

Run: `gofmt -l ./internal/subwatch/ && go vet ./internal/subwatch/ && go test ./internal/subwatch/ -count=1`
Expected: no gofmt output, no vet output, `ok`. Any other failing test: stop and ask.

- [ ] **Step 10: Commit**

```bash
git add server/internal/subwatch/return.go server/internal/subwatch/return_test.go server/internal/subwatch/keeper_test.go
git commit -F - <<'EOF'
fix(subwatch): a death no return wrote leaves the count of failed returns

The KN-4521 kept returning to a flaky preferred server: the death of the
replacement between two returns reset returnFails, so the backoff never
grew past 10-20 minutes and ReturnFailsMax was never reached. The count
is the preferred server's: only the death of the record a return wrote
counts against it, and the death of a server the watch switched to
itself leaves the count, the backoff and the wait as they are.
EOF
```

---

### Task 2: A selection starts the returns over at the first tick that reads it

**Files:**
- Modify: `server/internal/subwatch/watch.go` (field after `returnDeath` ~146; `Tick` after the `cfg == nil` check ~230-233)
- Modify: `server/internal/subwatch/return.go` (new `noteSelection` between `returnAfterDeath` and `backOffReturn`)
- Test: `server/internal/subwatch/return_test.go`, `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Consumes: `ownWrite`, `osloLooks` (Task 1).
- Produces: `func (w *Watch) noteSelection(cfg *vpnconfig.VPNDirectorConfig)` and the field `selectedSeq int`; `Tick` calls `noteSelection` once the config is read, before any action.

- [ ] **Step 1: Replace `TestTick_ASelectionStartsStoppedReturnsOver` (`return_test.go` ~212-235) with**

The old test modelled a selection by clearing `preferred_server` without moving the record's counter, and expected the backoff's wait to survive it. A selection moves the counter (`selectServer`), and the spec starts the wait over with the count.

```go
// A selection starts stopped returns over, the wait included: once Xray runs
// another server than the user's again, the next look comes at once.
func TestTick_ASelectionStartsStoppedReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)
	selectServer(r.f, returnServers()[1]) // the user selects Madrid, which runs
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.w.returnFails != 0 || r.w.returnRetry != 0 || !r.w.returnNotBefore.IsZero() {
		t.Fatalf("returnFails %d, returnRetry %v, next return %v; a selection starts the returns over",
			r.w.returnFails, r.w.returnRetry, r.w.returnNotBefore)
	}
	r.f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if n := r.attempts(); n != ReturnFailsMax+1 {
		t.Fatalf("attempts %d, want %d: the next look comes at once", n, ReturnFailsMax+1)
	}
}
```

- [ ] **Step 2: Add after `TestTick_ADeathSoonAfterASelectionThatFollowedAReturnIsNoFailedReturn` (`return_test.go` ~672-700)**

```go
// A selection is the user's new choice: what the returns to the old one counted
// is done with at the first tick that reads it - within ReturnHold of a return
// too, and for the server already running, selected again.
func TestTick_ASelectionStartsTheReturnsOverAtOnce(t *testing.T) {
	for _, choice := range []string{"another server", "the same server"} {
		t.Run(choice, func(t *testing.T) {
			r := newReturnRig(returnServers())
			r.up[osloIP] = true
			r.live[madridIP] = true
			r.tick() // a failed return: the next waits ReturnRetry
			r.live[osloIP] = true
			r.f.now = r.f.now.Add(ReturnRetry)
			r.tick() // the next attempt: Oslo works now
			if r.w.returnFails != 1 || r.w.lastReturn.IsZero() {
				t.Fatalf("returnFails %d, lastReturn %v; want the return waiting to hold and the count kept", r.w.returnFails, r.w.lastReturn)
			}
			selected := returnServers()[1]
			if choice == "the same server" {
				selected = returnServers()[0]
			}
			selectServer(r.f, selected)
			r.running = dialIP(selected)
			r.f.now = r.f.now.Add(ProbeInterval)
			r.tick()
			if r.w.returnFails != 0 || r.w.returnRetry != 0 || !r.w.returnNotBefore.IsZero() || !r.w.lastReturn.IsZero() {
				t.Fatalf("returnFails %d, returnRetry %v, next return %v, lastReturn %v; a selection starts the returns over",
					r.w.returnFails, r.w.returnRetry, r.w.returnNotBefore, r.w.lastReturn)
			}
		})
	}
}

// A selected server that never passes a probe dies before any look: the
// returns start over all the same, or its death would hand the count of the old
// choice on to the new one.
func TestTick_ASelectedServerThatNeverWorksStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)                     // the returns to Oslo have stopped
	selectServer(r.f, returnServers()[0]) // the user selects Oslo, which refuses the proxy
	r.running = osloIP
	r.f.plat = connected("ovpnc2")
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnFails != 0 || r.w.returnRetry != 0 {
		t.Fatalf("returnFails %d, returnRetry %v; the selection started the returns over", r.w.returnFails, r.w.returnRetry)
	}
}

// A selection the user makes while the clients are on the fallback tunnel
// starts the returns over at the next tick, before the failover goes on: the
// tick notes it before it acts.
func TestTick_ASelectionDuringAFailoverStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)        // the returns to Oslo have stopped
	r.live[madridIP] = false // Madrid dies: the clients go to the tunnel
	r.f.plat = connected("ovpnc2")
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil || r.w.returnFails != ReturnFailsMax {
		t.Fatalf("failover %+v, returnFails %d; want the clients on the tunnel and the returns stopped", r.f.cfg.Xray.Failover, r.w.returnFails)
	}
	selectServer(r.f, returnServers()[0]) // the user selects Oslo meanwhile
	r.running = osloIP
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("Oslo still refuses the proxy: the clients stay on the tunnel")
	}
	if r.w.returnFails != 0 || r.w.returnRetry != 0 {
		t.Fatalf("returnFails %d, returnRetry %v; the selection started the returns over", r.w.returnFails, r.w.returnRetry)
	}
}
```

- [ ] **Step 3: Make the fixtures say who wrote their records**

Once `noteSelection` exists, the first tick takes a record the watch did not write for a selection and starts the returns over. Three fixtures model records the watch wrote but did not write them through `generate`; they now say so.

(a) `TestTick_ADeathSoonAfterAReturnCountsTowardTheCap` (`return_test.go`): replace

```go
	// A walk puts Madrid in Oslo's place, and the clients come back to Xray.
	vpnconfig.RecordWalkedServer(r.f.cfg, returnServers()[1])
	r.running = madridIP
	r.live[madridIP] = true
	looks := 0
	reachable := r.w.Reachable
	r.w.Reachable = func(ctx context.Context, ip string, port int) bool {
		if ip == osloIP {
			looks++
		}
		return reachable(ctx, ip, port)
	}
	tickFor(r.w, r.f, time.Hour)
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the clients must be back on Xray")
	}
	if looks != 0 {
		t.Fatalf("%d looks at Oslo; the returns have stopped", looks)
	}
```

with

```go
	// A walk puts Madrid in Oslo's place, and the clients come back to Xray.
	r.ownWrite(t, returnServers()[1])
	r.live[madridIP] = true
	looks := r.osloLooks()
	tickFor(r.w, r.f, time.Hour)
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the clients must be back on Xray")
	}
	if *looks != 0 {
		t.Fatalf("%d looks at Oslo; the returns have stopped", *looks)
	}
```

(b) In `TestKeeper_AnActualityReplacementSettlesTheReturns`, the subtest `"a return within ReturnHold"`: the return wrote the record through `generate`, so add right after the line that sets `s.w.lastReturn, s.w.returnSeq`:

```go
				s.w.ownSeq = s.w.returnSeq
```

(c) `TestMaybeReturn_AHeldGroupReturnEndsTheBackoff` (`keeper_test.go` ~1678): the watch put Xray on Shadow, and the preset count must stand until the return to the group has held, or the test no longer pins what it says. Replace its first lines

```go
	s := newGroupReturnKeeper(t)
	s.w.returnFails, s.w.returnRetry = ReturnFailsMax-1, ReturnRetryMax
	s.w.Tick(context.Background()) // back on Backup, of Oslo's group
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
```

with

```go
	s := newGroupReturnKeeper(t)
	s.w.ownSeq = vpnconfig.ActiveSeq(s.f.cfg.Xray.ActiveServer) // the watch put Xray on Shadow
	s.w.returnFails, s.w.returnRetry = ReturnFailsMax-1, ReturnRetryMax
	s.w.Tick(context.Background()) // back on Backup, of Oslo's group
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if s.w.returnFails != ReturnFailsMax-1 {
		t.Fatalf("returnFails %d; the count stands until the return has held", s.w.returnFails)
	}
```

- [ ] **Step 4: Add the subtest `"a selected record"` to `TestKeeper_AnActualityReplacementSettlesTheReturns`**

Between the subtests `"a record the watch wrote"` and `"a return within ReturnHold"`:

```go
			t.Run("a selected record", func(t *testing.T) {
				s := newKeeperFixture(t)
				kind.change(s)
				// The user selected Oslo after returns to an earlier choice had stopped.
				s.w.returnFails, s.w.returnRetry = ReturnFailsMax, ReturnRetryMax
				s.missOnce(t)
				s.w.Tick(context.Background())
				if got := writtenNames(s.written); !reflect.DeepEqual(got, kind.written) {
					t.Fatalf("written %v, want %v", got, kind.written)
				}
				if s.w.returnFails != 0 || s.w.returnRetry != 0 {
					t.Fatalf("returnFails %d, returnRetry %v; the selection started the returns over", s.w.returnFails, s.w.returnRetry)
				}
			})
```

and append to the test's doc comment: `A record the user selected started the returns over when the watch first read it.`

- [ ] **Step 5: Run the new tests and watch them fail**

Run: `go test ./internal/subwatch/ -count=1 -run '^(TestTick_ASelectionStartsStoppedReturnsOver|TestTick_ASelectionStartsTheReturnsOverAtOnce|TestTick_ASelectedServerThatNeverWorksStartsTheReturnsOver|TestTick_ASelectionDuringAFailoverStartsTheReturnsOver|TestKeeper_AnActualityReplacementSettlesTheReturns|TestTick_ADeathSoonAfterAReturnCountsTowardTheCap|TestMaybeReturn_AHeldGroupReturnEndsTheBackoff)$'`

Expected: FAIL, each because nothing starts the returns over at a selection:
- `TestTick_ASelectionStartsStoppedReturnsOver`: `returnFails 0, returnRetry 0s, next return ...; a selection starts the returns over` (the wait survives)
- `TestTick_ASelectionStartsTheReturnsOverAtOnce/another_server` and `/the_same_server`: `returnFails 1, returnRetry 10m0s, next return ..., lastReturn ...; a selection starts the returns over`
- `TestTick_ASelectedServerThatNeverWorksStartsTheReturnsOver`: `returnFails 4, returnRetry 30m0s; the selection started the returns over`
- `TestTick_ASelectionDuringAFailoverStartsTheReturnsOver`: `returnFails 4, returnRetry 30m0s; the selection started the returns over`
- `TestKeeper_AnActualityReplacementSettlesTheReturns/*/a_selected_record` (both kinds): `returnFails 4, returnRetry 30m0s; the selection started the returns over`

The fixtures of Step 3 pass already: they only say who wrote the record.

- [ ] **Step 6: Implement**

`watch.go`, after the field `returnDeath`:

```go
	selectedSeq       int                                  // active_server's write counter of the last selection the returns started over at: a record the watch read and had not written
```

`watch.go`, in `Tick`, right after

```go
	if cfg == nil {
		w.resetFail()
		return
	}
```

add

```go
	w.noteSelection(cfg)
```

so that `w.pendingRestore = cfg.Xray.PendingRestore` follows it.

`return.go`, between `returnAfterDeath` and `backOffReturn`:

```go
// noteSelection starts the returns over at a record the watch did not write: a
// selection in the Web UI, /xray, the wizard or configure.sh - the same server
// selected again too, since every write moves the counter - or the record a
// previous process left, which the restart has started over anyway. What the
// returns counted is the old choice's, and the user's new choice starts with
// none: a selected server that never passes a probe would otherwise hand the
// old count on at its death. Once per record (selectedSeq), at the first tick
// that reads it, before the tick acts.
func (w *Watch) noteSelection(cfg *vpnconfig.VPNDirectorConfig) {
	seq := vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
	if seq == w.ownSeq || seq == w.selectedSeq {
		return
	}
	w.selectedSeq = seq
	w.returnFails, w.returnRetry = 0, 0
	w.returnNotBefore, w.lastReturn = time.Time{}, time.Time{}
}
```

Run `gofmt -w ./internal/subwatch/watch.go`: the new field shifts the alignment of the fields around it.

- [ ] **Step 7: Run the package and see it pass**

Run: `gofmt -l ./internal/subwatch/ && go vet ./internal/subwatch/ && go test ./internal/subwatch/ -count=1`
Expected: no gofmt output, no vet output, `ok`. Any other failing test: stop and ask.

- [ ] **Step 8: Commit**

```bash
git add server/internal/subwatch/watch.go server/internal/subwatch/return.go server/internal/subwatch/return_test.go server/internal/subwatch/keeper_test.go
git commit -F - <<'EOF'
fix(subwatch): a selection starts the returns over at the first tick that reads it

Until now any death started the returns over, which also ended a count
left from the user's previous choice. Deaths no longer do, so the watch
notes a record it did not write - active_server.seq against ownSeq, as
missed tells them apart - and starts the returns over there: within
ReturnHold of a return, for the same server selected again, for a
selected server that never passes a probe, and during a failover.
EOF
```

---

### Task 3: Xray back on the choice by the watch's own switch counts as a return

**Files:**
- Modify: `server/internal/subwatch/return.go` (`ReturnHold` comment; `maybeReturn`'s doc and its branch for an absent `preferred_server`)
- Test: `server/internal/subwatch/return_test.go`, `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Consumes: `ownWrite` (Task 1), `noteSelection` (Task 2: a selection reaches the branch with the count already zero).
- Produces: no new names.

- [ ] **Step 1: Replace `TestTick_NoPreferredServerStartsTheReturnsOver` (`return_test.go` ~822-838) with**

```go
// With no preferred server left - its subscription was deleted - what the
// failed returns counted ends once Xray has held on the server it runs for
// ReturnHold: nothing tells that server from a choice Xray got back to.
func TestTick_NoPreferredServerEndsTheCountOnceXrayHolds(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.tick()                           // a failed return: the next waits ReturnRetry
	r.f.cfg.Xray.PreferredServer = nil // its subscription was deleted
	r.f.now = r.f.now.Add(ProbeInterval)
	start := r.f.now
	r.tick()
	if r.w.returnFails != 1 || r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; the count waits for Xray to hold", r.w.returnFails, r.w.returnRetry)
	}
	r.f.now = start.Add(ReturnHold)
	r.tick()
	if r.w.returnFails != 0 || r.w.returnRetry != 0 {
		t.Fatalf("returnFails %d, returnRetry %v; Xray has held for %v", r.w.returnFails, r.w.returnRetry, ReturnHold)
	}
}
```

- [ ] **Step 2: Add after `TestTick_AReturnEndsTheBackoffOnceItHasHeld` (`return_test.go` ~842-867)**

```go
// The watch's own switch can put Xray back on the preferred server - the fast
// path or a replacement takes it as the best server alive - while returns to it
// have failed. That counts as a return: Oslo's death soon after is one more
// failed return.
func TestTick_XrayBackOnTheChoiceWithoutAReturnCountsAsOne(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick()                          // a failed return: the next waits ReturnRetry
	r.ownWrite(t, returnServers()[0]) // the fast path takes Oslo, alive again
	r.live[osloIP] = true
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if r.f.cfg.Xray.PreferredServer != nil || r.w.returnFails != 1 {
		t.Fatalf("preferred %+v, returnFails %d; want Oslo running and the count kept", r.f.cfg.Xray.PreferredServer, r.w.returnFails)
	}
	r.up[osloIP], r.live[osloIP] = false, false // Oslo dies again
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.w.returnFails != 2 || r.w.returnRetry != 2*ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; Oslo's death soon after Xray went back to it is a failed return",
			r.w.returnFails, r.w.returnRetry)
	}
}

// Once Xray has held on the preferred server for ReturnHold, the count ends, as
// after a return that held - stopped returns included.
func TestTick_XrayHeldOnTheChoiceEndsTheCount(t *testing.T) {
	r := newReturnRig(returnServers())
	failReturns(t, r)                 // the returns to Oslo have stopped
	r.ownWrite(t, returnServers()[0]) // the fast path takes Oslo, alive again
	r.live[osloIP] = true
	r.f.now = r.f.now.Add(ProbeInterval)
	start := r.f.now
	r.tick()
	r.f.now = start.Add(ReturnHold - ProbeInterval)
	r.tick()
	if r.w.returnFails != ReturnFailsMax {
		t.Fatalf("returnFails %d before Oslo has held %v", r.w.returnFails, ReturnHold)
	}
	r.f.now = start.Add(ReturnHold)
	r.tick()
	if r.w.returnFails != 0 || r.w.returnRetry != 0 {
		t.Fatalf("returnFails %d, returnRetry %v; Oslo has held %v", r.w.returnFails, r.w.returnRetry, ReturnHold)
	}
}
```

- [ ] **Step 3: Add after `TestMaybeReturn_AHeldGroupReturnEndsTheBackoff` at the end of `keeper_test.go` (Review Focus 3)**

```go
// The KN-4521 again: with returns to Oslo failed, Backup - the server the watch
// runs - dies, and the fast path takes Oslo, alive again. Xray is back on the
// user's choice without a return; Oslo dies within ReturnHold, and that counts
// as one more failed return.
func TestKeeper_AFastSwitchBackToTheChoiceCountsAsAReturn(t *testing.T) {
	s := newReturnKeeper(t) // Xray on Backup, Oslo preferred
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	s.w.ownSeq = vpnconfig.ActiveSeq(s.f.cfg.Xray.ActiveServer) // the watch put Xray on Backup
	s.w.returnFails, s.w.returnRetry = 1, ReturnRetry
	dead := "Backup"
	running := func() string {
		if len(s.written) == 0 {
			return "Backup"
		}
		return s.written[len(s.written)-1].Name
	}
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if running() == dead {
			return errProbe
		}
		return nil
	}
	s.setHealthOf([]vpnconfig.Server{backup}, []vpnconfig.Server{oslo})
	s.w.Tick(context.Background()) // Backup's probe fails; the fast path switches to Oslo
	if got := writtenNames(s.written); len(got) == 0 || got[len(got)-1] != "Oslo@oslo.example" || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("written %v, preferred %+v; want the fast path on Oslo", got, s.f.cfg.Xray.PreferredServer)
	}
	s.f.now = s.f.now.Add(ProbeInterval)
	s.w.Tick(context.Background()) // Oslo works: the first look takes it for a return
	if s.w.lastReturn.IsZero() || s.w.returnFails != 1 {
		t.Fatalf("lastReturn %v, returnFails %d; Xray back on the choice counts as a return", s.w.lastReturn, s.w.returnFails)
	}
	// Oslo dies again a few minutes later; the keeper confirms it the legacy way.
	dead = "Oslo"
	s.w.Health = nil
	s.f.now = s.f.now.Add(2 * time.Minute)
	tickUntilDead(s.w, s.f)
	if s.w.returnFails != 2 || s.w.returnRetry != 2*ReturnRetry {
		t.Fatalf("returnFails %d, returnRetry %v; Oslo's death soon after Xray got back to it is a failed return",
			s.w.returnFails, s.w.returnRetry)
	}
}
```

- [ ] **Step 4: Run the new tests and watch them fail**

Run: `go test ./internal/subwatch/ -count=1 -run '^(TestTick_NoPreferredServerEndsTheCountOnceXrayHolds|TestTick_XrayBackOnTheChoiceWithoutAReturnCountsAsOne|TestTick_XrayHeldOnTheChoiceEndsTheCount|TestKeeper_AFastSwitchBackToTheChoiceCountsAsAReturn)$'`

Expected: FAIL, each because `maybeReturn` resets the count at once when `preferred_server` is absent:
- `TestTick_NoPreferredServerEndsTheCountOnceXrayHolds`: `returnFails 0, returnRetry 0s; the count waits for Xray to hold`
- `TestTick_XrayBackOnTheChoiceWithoutAReturnCountsAsOne`: `preferred <nil>, returnFails 0; want Oslo running and the count kept`
- `TestTick_XrayHeldOnTheChoiceEndsTheCount`: `returnFails 0 before Oslo has held 30m0s`
- `TestKeeper_AFastSwitchBackToTheChoiceCountsAsAReturn`: `lastReturn 0001-01-01 00:00:00 +0000 UTC, returnFails 0; Xray back on the choice counts as a return`

- [ ] **Step 5: Implement in `return.go`**

`ReturnHold`'s comment (lines ~26-30) becomes:

```go
	// ReturnHold is how long the server a return went back to - the preferred
	// server, or a server of its group - has to keep working for the return to
	// count as held: a death that starts within it counts as a failed return,
	// and only a held return ends the backoff. A switch of the watch's own that
	// puts Xray back on the preferred server while returns to it have failed
	// counts as a return here (maybeReturn).
	ReturnHold = 30 * time.Minute
```

`maybeReturn`'s doc comment gains a last sentence; the whole comment reads:

```go
// maybeReturn takes Xray back to the server the user chose once the watch has
// left another one running and the chosen one is back: with fresh monitor
// evidence, once the monitor has held it alive for ReturnAlive; without, once
// it accepts TCP again - or, for a server no TCP dial can see, once its turn
// comes round. While it is not back and Xray runs a server of another group,
// it takes Xray to the best server of the user's group the monitor has held
// alive as long, and preferred_server stays. It runs after a probe that
// passed, with no failover and no restore apply outstanding, and looks every
// ReturnCheck - after a failed return, ReturnRetry and longer. After
// ReturnFailsMax failed attempts in a row the returns stop; a return of either
// kind that has held for ReturnHold ends that count and its wait. Xray back on
// the user's choice by a switch of the watch's own counts as a return while
// failed returns are counted.
```

The branch for an absent `preferred_server` (lines ~67-75) becomes:

```go
	if cfg == nil || cfg.Xray.PreferredServer == nil {
		// Xray runs the user's choice, or nothing is left to return to. A switch
		// of the watch's own - the fast path, a replacement, the walk - can put
		// Xray back on the choice while returns to it have failed: that counts
		// as a return, so a choice that dies again soon after is one more failed
		// return, and one that holds for ReturnHold ends the count above. A
		// selection gets here with the count ended (noteSelection), and with no
		// count there is nothing to hold.
		if w.lastReturn.IsZero() && w.returnFails > 0 && cfg != nil && cfg.Xray.ActiveServer != nil {
			w.lastReturn = w.Now()
			w.returnSeq = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
			w.returnGroup = ""
		}
		return
	}
```

- [ ] **Step 6: Run the package and see it pass**

Run: `gofmt -l ./internal/subwatch/ && go vet ./internal/subwatch/ && go test ./internal/subwatch/ -count=1`
Expected: no gofmt output, no vet output, `ok`. Any other failing test: stop and ask.

- [ ] **Step 7: Commit**

```bash
git add server/internal/subwatch/return.go server/internal/subwatch/return_test.go server/internal/subwatch/keeper_test.go
git commit -F - <<'EOF'
fix(subwatch): Xray back on the choice by the watch's own switch counts as a return

The fast path, a replacement or the walk rank the user's group and
subscription first, so they can put Xray on the user's choice itself.
maybeReturn then found no preferred_server and reset the count at once,
and a flaky choice the watch kept landing on could cycle without the
returns ever stopping. With failed returns counted, the first look that
finds Xray there now takes it for a return: a death within ReturnHold is
one more failed return, and a hold ends the count.
EOF
```

---

### Task 4: The return waits for the monitor's evidence while it re-checks

**Files:**
- Modify: `server/internal/subwatch/best.go` (`walkEvidence`, `bestWalkOrder`)
- Modify: `server/internal/subwatch/return.go` (`maybeReturn`: doc, the look)
- Test: `server/internal/subwatch/return_test.go` (add the import `"github.com/zinin/vpn-director/server/internal/monitor"`), `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Consumes: `osloLooks` (Task 1).
- Produces: `func (w *Watch) walkEvidence(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) (ev monitor.Evidence, fresh, waiting bool)`. Its callers are `bestWalkOrder` (ignores `waiting`) and `maybeReturn`; nothing else calls it.

- [ ] **Step 1: Append to the end of `return_test.go`, and add the `monitor` import**

```go
// A list publication gave the monitor new endpoints, and while it checks them it
// is OK with no evidence for the servers: the return waits for it - no TCP look,
// no switch - and looks again at the next tick rather than ReturnCheck later.
// Once the monitor holds Oslo alive for ReturnAlive, Xray goes back.
func TestTick_TheReturnWaitsWhileTheMonitorRechecks(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	h := &fastHealth{cached: monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{}}, invalid: map[string]bool{}}
	r.w.Health = h
	looks := r.osloLooks()
	r.tick()
	if len(r.events) != 0 || *looks != 0 {
		t.Fatalf("events %v, %d TCP looks at Oslo; the monitor is re-checking", r.events, *looks)
	}
	h.cached.Endpoints[endpoint.Key(returnServers()[0])] = aliveSince(r.f.now.Add(-time.Hour), 40)
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) || *looks != 0 {
		t.Fatalf("events %v, %d TCP looks; want %v at the next tick, on the monitor's evidence", r.events, *looks, want)
	}
}

// A check that completed while the evidence was read fails its validation: the
// monitor is OK all the same, and the return waits for the next tick.
func TestTick_TheReturnWaitsForEvidenceThatValidates(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	key := endpoint.Key(returnServers()[0])
	h := &fastHealth{cached: monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		key: aliveSince(r.f.now.Add(-time.Hour), 40),
	}}, invalid: map[string]bool{key: true}}
	r.w.Health = h
	looks := r.osloLooks()
	r.tick()
	if len(r.events) != 0 || *looks != 0 {
		t.Fatalf("events %v, %d TCP looks at Oslo; the evidence did not validate", r.events, *looks)
	}
	delete(h.invalid, key)
	r.f.now = r.f.now.Add(ProbeInterval)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v at the next tick", r.events, want)
	}
}

// Guard. Without the monitor - monitoring off, or a monitor in any state but
// OK - the TCP look decides the return, as before.
func TestTick_TheReturnTakesTheTCPLookWithoutTheMonitor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    watchdapi.State
		disabled bool
	}{
		{name: "WAN down", state: watchdapi.StateWANDown},
		{name: "prober error", state: watchdapi.StateProberError},
		{name: "no Xray", state: watchdapi.StateNoXray},
		{name: "monitor stopped", state: watchdapi.StateStopped},
		{name: "monitor disabled", state: watchdapi.StateDisabled},
		{name: "monitor not running", state: watchdapi.StateNotRunning},
		{name: "monitoring off in the config", state: watchdapi.StateOK, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReturnRig(returnServers())
			r.up[osloIP] = true
			r.live[osloIP] = true
			r.w.Health = &fastHealth{cached: monitor.Evidence{State: tc.state, Endpoints: map[string]watchdapi.EndpointState{}}, invalid: map[string]bool{}}
			if tc.disabled {
				enabled := false
				r.f.cfg.Monitor = &vpnconfig.MonitorConfig{Enabled: &enabled}
			}
			r.tick()
			if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
				t.Fatalf("events %v, want %v: without the monitor the TCP look decides", r.events, want)
			}
		})
	}
}

// Guard (Review Focus 5). The RT-AX86U on 2026-10-13: a list publication gave
// Oslo a new key while the monitor already knows Madrid again. The evidence is
// fresh and Oslo is not in it: no TCP look and no return until the monitor has
// held Oslo's new key alive for ReturnAlive.
func TestTick_AReKeyedPreferredServerWaitsForReturnAlive(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	h := &fastHealth{cached: monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		endpoint.Key(returnServers()[1]): aliveSince(r.f.now.Add(-time.Hour), 17),
	}}, invalid: map[string]bool{}}
	r.w.Health = h
	looks := r.osloLooks()
	start := r.f.now
	r.tick()
	// The monitor checks Oslo's new key at the next tick: alive from then on.
	h.cached.Endpoints[endpoint.Key(returnServers()[0])] = aliveSince(start.Add(ProbeInterval), 40)
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if len(r.events) != 0 || *looks != 0 {
		t.Fatalf("events %v, %d TCP looks; Oslo has been alive less than %v", r.events, *looks, ReturnAlive)
	}
	r.f.now = start.Add(2 * ReturnCheck)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) || *looks != 0 {
		t.Fatalf("events %v, %d TCP looks; want %v once Oslo has been alive %v", r.events, *looks, want, ReturnAlive)
	}
}
```

- [ ] **Step 2: Add after `TestMaybeReturn_TwoMinutesAliveIsNotEnough` in `keeper_test.go` (Review Focus 4)**

```go
// Both routers run the keeper: its return waits for the monitor the same way,
// and looks again at the next tick.
func TestMaybeReturn_WaitsWhileTheMonitorRechecks(t *testing.T) {
	s := newReturnKeeper(t)
	s.setStates(map[string]watchdapi.EndpointState{}) // OK, nothing checked yet
	looks := 0
	s.w.Reachable = func(context.Context, string, int) bool { looks++; return true }
	s.w.Tick(context.Background())
	if len(s.written) != 0 || looks != 0 {
		t.Fatalf("written %v, %d TCP looks; the monitor is re-checking", writtenNames(s.written), looks)
	}
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo": aliveSince(s.f.now.Add(-6*time.Minute), 40), "Backup": aliveSince(s.f.now.Add(-time.Hour), 17),
	})
	s.f.now = s.f.now.Add(ProbeInterval)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo.example"}) || looks != 0 {
		t.Fatalf("written %v, %d TCP looks; want Oslo at the next tick, on the monitor's evidence", got, looks)
	}
}
```

- [ ] **Step 3: Run the new tests and watch them fail**

Run: `go test ./internal/subwatch/ -count=1 -run '^(TestTick_TheReturnWaitsWhileTheMonitorRechecks|TestTick_TheReturnWaitsForEvidenceThatValidates|TestTick_TheReturnTakesTheTCPLookWithoutTheMonitor|TestTick_AReKeyedPreferredServerWaitsForReturnAlive|TestMaybeReturn_WaitsWhileTheMonitorRechecks)$'`

Expected: FAIL for the three that need the change, because `maybeReturn` takes the TCP look whenever the evidence is not fresh:
- `TestTick_TheReturnWaitsWhileTheMonitorRechecks`: `events [Oslo@203.0.113.10 restart], 1 TCP looks at Oslo; the monitor is re-checking`
- `TestTick_TheReturnWaitsForEvidenceThatValidates`: `events [Oslo@203.0.113.10 restart], 1 TCP looks at Oslo; the evidence did not validate`
- `TestMaybeReturn_WaitsWhileTheMonitorRechecks`: `written [Oslo@oslo.example], 2 TCP looks; the monitor is re-checking`

The guards `TestTick_TheReturnTakesTheTCPLookWithoutTheMonitor` (every subtest) and `TestTick_AReKeyedPreferredServerWaitsForReturnAlive` pass.

- [ ] **Step 4: Implement**

`best.go`, `walkEvidence` and `bestWalkOrder` become:

```go
// walkEvidence is the monitor's evidence for ordering servers, and whether it
// may: monitoring on, its state OK and every key of servers it knows still
// valid. Missing keys stay unknown; an empty validation request would mean the
// whole monitor set. waiting is a monitor that is on and OK with nothing valid
// for servers yet - a new endpoint set or prober whose first checks have not
// come in, or a check that completed while the evidence was read - which a
// look that needs the monitor waits for rather than look without it.
func (w *Watch) walkEvidence(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig) (ev monitor.Evidence, fresh, waiting bool) {
	if len(servers) == 0 || w.Health == nil ||
		(cfg != nil && cfg.Monitor != nil && cfg.Monitor.Enabled != nil && !*cfg.Monitor.Enabled) {
		return monitor.Evidence{}, false, false
	}
	evidence := w.Health.Evidence()
	if evidence.State != watchdapi.StateOK {
		return monitor.Evidence{}, false, false
	}
	keys := make([]string, 0, len(servers))
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		key := endpoint.Key(s)
		if _, ok := evidence.Endpoints[key]; ok && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	if len(keys) == 0 || w.Health.ValidateEvidence(evidence, keys) != nil {
		return monitor.Evidence{}, false, true
	}
	return evidence, true, false
}

// bestWalkOrder is orderBest on the monitor's evidence for servers, when it may
// order them.
func (w *Watch) bestWalkOrder(servers []vpnconfig.Server, cfg *vpnconfig.VPNDirectorConfig, ref bestRef) []vpnconfig.Server {
	ev, ok, _ := w.walkEvidence(servers, cfg)
	return orderBest(servers, ref, ev, ok)
}
```

`return.go`, in `maybeReturn`, the look - from `now := w.Now()` through `ev, fresh := w.walkEvidence(servers, cfg)` - becomes:

```go
	now := w.Now()
	if now.Before(w.returnNotBefore) {
		return
	}
	notBefore := w.returnNotBefore
	w.returnNotBefore = now.Add(ReturnCheck)
	subs, err := w.loadSubscriptions()
	if w.mutationEnded(ctx) {
		return
	}
	if err != nil {
		slog.Warn("Failed to read the subscriptions for the return to the preferred server", watchErrorAttr(err))
		return
	}
	servers := vpnconfig.AllServers(subs)
	// With fresh evidence the monitor's look is the look: alive for
	// ReturnAlive. While the monitor is on and OK without evidence for these
	// servers yet - a list publication gave it new endpoints or a new prober,
	// whose checks come in within half a minute - the look waits, and the next
	// tick looks again: the TCP look would skip ReturnAlive. Without the
	// monitor, the TCP look as before - a server no TCP dial can see is
	// switched to on its turn, the attempt the only check there is.
	ev, fresh, waiting := w.walkEvidence(servers, cfg)
	if waiting {
		w.returnNotBefore = notBefore
		slog.Debug("Return to the preferred server put off; the monitor has no evidence yet")
		return
	}
```

and `maybeReturn`'s whole doc comment (as Task 3 left it) becomes:

```go
// maybeReturn takes Xray back to the server the user chose once the watch has
// left another one running and the chosen one is back: with fresh monitor
// evidence, once the monitor has held it alive for ReturnAlive; while the
// monitor is on and OK without evidence yet, at a later look; without the
// monitor, once it accepts TCP again - or, for a server no TCP dial can see,
// once its turn comes round. While it is not back and Xray runs a server of
// another group, it takes Xray to the best server of the user's group the
// monitor has held alive as long, and preferred_server stays. It runs after a
// probe that passed, with no failover and no restore apply outstanding, and
// looks every ReturnCheck - after a failed return, ReturnRetry and longer.
// After ReturnFailsMax failed attempts in a row the returns stop; a return of
// either kind that has held for ReturnHold ends that count and its wait. Xray
// back on the user's choice by a switch of the watch's own counts as a return
// while failed returns are counted.
```

- [ ] **Step 5: Run the package and see it pass**

Run: `gofmt -l ./internal/subwatch/ && go vet ./internal/subwatch/ && go test ./internal/subwatch/ -count=1`
Expected: no gofmt output, no vet output, `ok`. Any other failing test: stop and ask.

- [ ] **Step 6: Commit**

```bash
git add server/internal/subwatch/best.go server/internal/subwatch/return.go server/internal/subwatch/return_test.go server/internal/subwatch/keeper_test.go
git commit -F - <<'EOF'
fix(subwatch): the return waits for the monitor's evidence while it re-checks

A list publication that changes the monitor's endpoint set leaves the
monitor OK but without evidence for 20 to 30 seconds while a new prober
checks the new set. walkEvidence reported that as no evidence, and the
return took the TCP look: on the RT-AX86U at 17:36 and 18:52 on
2026-10-13 Xray went back to a re-keyed preferred server before the
monitor had seen it alive once. walkEvidence now reports the monitor
waiting, and the return looks again at the next tick; the TCP look stays
for monitoring off and a monitor that is not OK.
EOF
```

---

### Task 5: Documentation

**Files:**
- Modify: `.claude/rules/watchd.md` (lines ~146-150 and ~346-354)
- Modify: `.claude/rules/telegram-bot.md` (the paragraph starting `While Xray works and the watch has left another server running`, line ~325)
- Modify: `README.md`, `README.ru.md` (line ~248)

**Interfaces:** none.

- [ ] **Step 1: `.claude/rules/watchd.md`, section "Subscription watch and failover"**

Replace

```
old download. Preferred return looks every 5 minutes: with valid monitor evidence it goes back once
the monitor has held the preferred server alive for 5 minutes (`ReturnAlive`), otherwise by the
TCP look; while that server is not back and Xray runs another group than the user's, a server of
the user's group alive as long is taken instead. Returns of both kinds retry at 10–30 minutes, end
their backoff once one has held for 30 minutes and stop after four failed returns. Details of the
```

with

```
old download. Preferred return looks every 5 minutes: with valid monitor evidence it goes back once
the monitor has held the preferred server alive for 5 minutes (`ReturnAlive`); while the monitor is
on and OK but holds nothing valid for the servers yet, the look waits for the next tick; with
monitoring off or a monitor that is not OK, the TCP look decides. While that server is not back and
Xray runs another group than the user's, a server of the user's group alive as long is taken
instead. Returns of both kinds retry at 10–30 minutes, end their backoff once one has held for 30
minutes and stop after four failed returns. The count is the preferred server's: the death of a
server the watch switched to itself leaves it, and a selection or a watchd restart starts it over
(see "Keeping Xray on a working server"). Details of the
```

- [ ] **Step 2: `.claude/rules/watchd.md`, section "Keeping Xray on a working server"**

Replace

```
knows no kind and goes by subscription and name. The return runs in both modes and looks every
`ReturnCheck`: with valid evidence it goes back once the monitor has held the preferred server
alive for `ReturnAlive` (5 minutes); without evidence the TCP look applies (`telegram-bot.md`).
While the preferred server is not back and Xray runs another group than the user's, a look with
valid evidence takes the best server of the user's group instead - the one `orderBest` puts first
among those the monitor has held alive for `ReturnAlive`, through its addresses alive that long:
one server per attempt. It switches, rolls back and backs off like any return, and
`preferred_server` stays. A return of either kind that has held for `ReturnHold` (30 minutes) ends
the return backoff, and both kinds count toward `ReturnFailsMax`.
```

with

```
knows no kind and goes by subscription and name. The return runs in both modes and looks every
`ReturnCheck`: with valid evidence it goes back once the monitor has held the preferred server
alive for `ReturnAlive` (5 minutes). A list publication that changes the monitor's endpoint set
leaves the monitor OK but without evidence for 20 to 30 seconds while a new prober checks the new
set; a look then waits for the next tick (`walkEvidence` reports `waiting`), since the TCP look
would skip `ReturnAlive`. With monitoring off or a monitor that is not OK the TCP look applies
(`telegram-bot.md`). While the preferred server is not back and Xray runs another group than the
user's, a look with valid evidence takes the best server of the user's group instead - the one
`orderBest` puts first among those the monitor has held alive for `ReturnAlive`, through its
addresses alive that long: one server per attempt. It switches, rolls back and backs off like any
return, and `preferred_server` stays. A return of either kind that has held for `ReturnHold` (30
minutes) ends the return backoff, and both kinds count toward `ReturnFailsMax`.

The count of failed returns is the user's choice's. A death that starts within `ReturnHold` of a
return, on the record that return wrote, counts against it (`returnAfterDeath`); the death of any
other record - a replacement's, the fast path's, the walk's, a follow's - leaves the count, the
backoff and the wait as they are. The fast path, a replacement or the walk can put Xray back on
the user's choice itself, since they rank its group and subscription first: with failed returns
counted, the first look that finds Xray there takes that for a return, so a death within
`ReturnHold` is one more failed return and a hold ends the count. A selection starts the returns
over at the first tick that reads it (`noteSelection`: a record the watch did not write, told by
`active_server.seq` against `ownSeq` as `missed` tells it), and so does a watchd restart. Returns
stopped at `ReturnFailsMax` stay stopped until then, or until Xray has held on the choice for
`ReturnHold`.
```

- [ ] **Step 3: `.claude/rules/telegram-bot.md`, the return paragraph (one long line, ~325)**

Replace the sentence

```
Without valid evidence, a preferred server no dial can see — Hysteria2, mKCP, xhttp over HTTP/3 (`tcpChecked`) — is switched to on that same schedule without a look: the attempt is then the only check there is.
```

with

```
While the monitor is on and OK but holds no valid evidence for the servers yet — a list publication gave it new endpoints and a new prober — the look waits for the next tick instead of dialing. With monitoring off or a monitor that is not OK, a preferred server no dial can see — Hysteria2, mKCP, xhttp over HTTP/3 (`tcpChecked`) — is switched to on that same schedule without a look: the attempt is then the only check there is.
```

and replace the passage

```
A death starts that interval over, unless it starts within 30 minutes (`ReturnHold`) of a return, on the record that return wrote (the `active_server.seq` it left, read at the first miss): the server it went back to has failed again, the death counts as a failed return, and the restore after it does not shorten that wait. A selection since the return writes another record — the same server selected again too — so the death of the user's new choice starts the interval over like any other. A return of either kind that has held for 30 minutes ends the backoff. After the fourth failed return in a row (`ReturnFailsMax`; a death of the returned record within `ReturnHold` counts) the watch stops trying: an endpoint that accepts TCP and keeps refusing the proxy would otherwise cost every Xray client ten to twenty dead seconds every 30 minutes. A new death, a selection or a watchd restart starts the returns over.
```

with

```
The interval and the count of failed returns are the preferred server's. A death that starts within 30 minutes (`ReturnHold`) of a return, on the record that return wrote (the `active_server.seq` it left, read at the first miss), counts as a failed return: the server it went back to has failed again, and the restore after it does not shorten that wait. The death of any other record — a server the watch switched to itself: a replacement, the fast path's pick, the walk's, a follow — says nothing about the preferred server and leaves the count and the wait as they are. The fast path, a replacement or the walk can put Xray back on the preferred server itself; while returns to it have failed, that counts as a return, and its death within 30 minutes is one more failed one. A selection — a record the watch did not write, the same server selected again too — starts the returns over at the first tick that reads it. A return of either kind that has held for 30 minutes ends the backoff, and so does Xray holding on the preferred server for 30 minutes after a switch of the watch's own. After the fourth failed return in a row (`ReturnFailsMax`; a death of the returned record within `ReturnHold` counts) the watch stops trying: an endpoint that accepts TCP and keeps refusing the proxy would otherwise cost every Xray client ten to twenty dead seconds every 30 minutes. A selection or a watchd restart starts the returns over.
```

- [ ] **Step 4: READMEs**

`README.md` line ~248: replace `(without valid monitor evidence, once it accepts connections again)` with `(with monitoring off or not working, once it accepts connections again)`.

`README.ru.md` line ~248: replace `(без действительных данных монитора — когда сервер снова принимает соединения)` with `(при выключенном или неработающем мониторинге — когда сервер снова принимает соединения)`.

- [ ] **Step 5: Check the docs against the code**

Read each replaced passage next to `return.go`, `best.go` and `watch.go` as they are after Task 4: every name it cites (`walkEvidence`, `waiting`, `noteSelection`, `returnAfterDeath`, `ownSeq`, `ReturnHold`, `ReturnFailsMax`, `ReturnAlive`) exists, and every rule it states matches the code. Run `grep -n "A new death" .claude/rules/*.md README*.md server/internal/subwatch/*.go` and expect no hit.

- [ ] **Step 6: Commit**

```bash
git add .claude/rules/watchd.md .claude/rules/telegram-bot.md README.md README.ru.md
git commit -F - <<'EOF'
docs: the returns count the preferred server's failures and wait for the monitor

The rules described a death that starts the returns over and a TCP look
whenever the evidence was not fresh. A death of a server the watch
switched to now leaves the count, a selection or a watchd restart starts
it over, Xray back on the choice by the watch's own switch counts as a
return, and a look waits for the next tick while the monitor is OK but
re-checks a new endpoint set.
EOF
```

---

### Task 6: Whole-branch verification

**Files:** none changed, unless a check fails (then stop and ask).

- [ ] **Step 1: Full test suite, race and vet (through `build-forge:build-runner`, model `opus`)**

From `server/`:
- `go test ./... -count=1` - expected: every package `ok` (31 packages at `1dadee6`).
- `go test -race ./internal/subwatch/ ./cmd/watchd/ -count=1` - expected: `ok`, no `DATA RACE`.
- `go vet ./...` and `gofmt -l .` - expected: no output.

- [ ] **Step 2: Router builds of the changed daemon**

From `server/`: `GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/watchd/`, `GOOS=linux GOARCH=arm GOARM=7 go build -o /dev/null ./cmd/watchd/`, `GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -o /dev/null ./cmd/watchd/` - expected: all three succeed.

- [ ] **Step 3: Final review**

Request a whole-branch review (superpowers:requesting-code-review) from a fresh reviewer on `opus`, of `git diff 1dadee6..HEAD -- server .claude README.md README.ru.md`, against the spec. Fix what it finds that the spec supports; ask the owner about anything else.

---

### Task 7: Release v0.21.1

Every outward step waits for the owner's explicit go-ahead in the session: push, PR, `@codex review`, merge, tag, release edit, router access. If the auto-mode classifier blocks a needed step, stop and ask the owner to disable it, naming the action.

- [ ] **Step 1: Remove the planning documents from the branch**

```bash
git rm docs/superpowers/specs/2026-10-13-bounded-returns-design.md docs/superpowers/plans/2026-10-13-bounded-returns.md
git commit -m "docs: remove the design and plan documents before the PR"
```

Check with `git diff --stat master...HEAD` that no file under `docs/superpowers/` is in the diff.

- [ ] **Step 2: Push the branch and open the PR (owner's go-ahead)**

`git push -u origin fix/bounded-returns`, then `gh pr create --base master --title "fix(subwatch): bounded returns to a flaky preferred server" --body-file <file>`. The body follows PR #68's layout:
- **Summary**: the two changes of spec section 1.
- **Why**: the KN-4521's endless "return, death, replacement, return" and the RT-AX86U's return without `ReturnAlive` (spec section 2, names pseudonymized, no hosts or addresses).
- **What changed**: one bullet per Task 1-5, with the rules of spec 4.1-4.4.
- **Verification**: the results of Task 6, verbatim counts.
- **Not in this PR**: the spec's known limits, `monitor.concurrency` (router configuration), and the deferred items of the analysis (replacements trying many dead candidates, subscription-health hysteresis, mass re-keying).

- [ ] **Step 3: Codex review loop**

Codex reviews the PR when it opens. Read findings with `gh pr view <N> --comments` and `gh api repos/zinin/vpn-director/pulls/<N>/comments`. Each finding: verify against the code (superpowers:receiving-code-review), fix with a test where it holds, and answer it. A new round only after `@codex review`, posted with the owner's permission.

- [ ] **Step 4: Merge and tag (owner's go-ahead)**

Merge the PR, keep the branch. On the merge commit:

```bash
git tag -a v0.21.1 -m "vpn-director-watchd counts failed returns against the preferred server alone: the death of a server it switched to itself no longer starts the returns over, so returns to a flaky choice stop after four failures until a selection or a restart, and a return waits for the monitor's evidence while it re-checks a new endpoint set instead of going back without five minutes alive."
git push origin v0.21.1
```

The `Build` workflow (`.github/workflows/telegram-bot.yml`) builds 9 assets and creates the release with generated notes; then `gh release edit v0.21.1` with a title and notes in the v0.21.0 style. The notes also name `1dadee6` (the INFO line `Following the Xray server did not work; replacing it`).

- [ ] **Step 5: Routers (owner's go-ahead; read-only)**

The owner updates both routers with the Web UI Update button. Then, read-only:
- KN-4521: the watchd log shows `counted as a failed return` and backoffs that grow across a replacement's death (`Next return attempt backed off after=20m0s` after one); after four failed returns, the stop line.
- RT-AX86U: after `Subscription list published` with `added` or `readdressed` servers, no `Returning to the preferred server` before the monitor has held that server alive for 5 minutes.
- Both: no new watchd WARN; the SOCKS probe answers 204.

Record the results in the release notes' "Verified" section (owner's go-ahead for the edit).
