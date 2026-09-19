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

**Files:**
- Create: `server/internal/subwatch/reach.go`
- Create: `server/internal/subwatch/reach_test.go`
- Modify: `server/internal/subwatch/watch.go` (constants ~17-34, `Watch` ~60-95, `Tick` ~133-297, `settled` ~1462)
- Modify: `.claude/rules/telegram-bot.md` (tree line under `subwatch/`, "Subscription watch" second paragraph)

**Interfaces:**
- Consumes: `chosenIndex(servers []vpnconfig.Server, a *vpnconfig.ActiveServer) int`, `perAddress(servers []vpnconfig.Server) []vpnconfig.Server`, test helpers `fake`, `baseCfg()`, `connected(ids ...string)`, `errProbe` (all existing).
- Produces (later tasks rely on these exact names):
  - `Watch.LoadServers func() ([]vpnconfig.Server, error)`
  - `Watch.Reachable func(ctx context.Context, ip string, port int) bool`
  - `const FastDeadAfter = time.Minute`, `const ReachTimeout = 3 * time.Second`
  - `func dialIP(c vpnconfig.Server) string`
  - `func dialable(copies []vpnconfig.Server) []vpnconfig.Server`
  - `func (w *Watch) reachable(ctx context.Context, copies []vpnconfig.Server) []vpnconfig.Server`
  - `func (w *Watch) resetFail()`
  - test helpers `captureLog(t) *bytes.Buffer`, `reachWatch(f, servers, up) *Watch`

- [ ] **Step 1: Add the API surface without behaviour**

In `watch.go`, add to the first `const` block, right after the `FetchTimeout = 3 * time.Minute` line (its own comment keeps it a separate alignment section):

```go
	// FastDeadAfter is how long the probe has to fail before an outbound whose
	// server accepts no TCP connection counts as dead; every other failure
	// waits DeadAfter. ReachTimeout bounds one look at a server's addresses.
	FastDeadAfter = time.Minute
	ReachTimeout  = 3 * time.Second
```

In `type Watch struct`, insert two fields between `Fetch` and `Notify` (the lines around them carry no comment, so no other line realigns):

```go
	Fetch         func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	LoadServers   func() ([]vpnconfig.Server, error)
	Reachable     func(ctx context.Context, ip string, port int) bool // nil => no TCP checks: no fast death, no return
	Notify        func(msg string)
```

and one state field right after `failSince`:

```go
	failSince         time.Time // zero => last probe succeeded
	downChecks        int       // checks since failSince that found the active server down; -1 once one did not
	lastImport        time.Time
```

Create `server/internal/subwatch/reach.go` with stubs, so the tests compile and fail on behaviour:

```go
package subwatch

import (
	"context"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func dialIP(c vpnconfig.Server) string { return "" }

func (w *Watch) reachable(ctx context.Context, copies []vpnconfig.Server) []vpnconfig.Server {
	return nil
}
```

- [ ] **Step 2: Write the failing tests**

Create `server/internal/subwatch/reach_test.go`:

```go
package subwatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// captureLog sends slog to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func osloActiveCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	return cfg
}

func osloServers() []vpnconfig.Server {
	return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10"}}}
}

// deadFake is a watch whose outbound fails every probe, with ovpnc2 to fall
// back on.
func deadFake() *fake {
	return &fake{cfg: osloActiveCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
}

// reachWatch is f's watch with servers as servers.json and a TCP check that
// finds the addresses up names reachable.
func reachWatch(f *fake, servers []vpnconfig.Server, up map[string]bool) *Watch {
	w := f.watch()
	w.LoadServers = func() ([]vpnconfig.Server, error) { return servers, nil }
	w.Reachable = func(_ context.Context, ip string, _ int) bool { return up[ip] }
	return w
}

// assertDiesAt ticks every ProbeInterval from f.now, the first miss, and checks
// that the clients move on the tick d later and on none before it.
func assertDiesAt(t *testing.T, w *Watch, f *fake, d time.Duration) {
	t.Helper()
	start := f.now
	for f.now.Sub(start) < d {
		w.Tick(context.Background())
		if f.cfg.Xray.Failover != nil {
			t.Fatalf("moved %v after the first miss, want %v", f.now.Sub(start), d)
		}
		f.now = f.now.Add(ProbeInterval)
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v %v after the first miss, want the move to ovpnc2", f.cfg.Xray.Failover, d)
	}
}

func TestDialIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    vpnconfig.Server
		want string
	}{
		{"resolved address", vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.10"}}, "203.0.113.10"},
		{"empty entries skipped", vpnconfig.Server{Address: "oslo.example", IPs: []string{"", "203.0.113.11"}}, "203.0.113.11"},
		{"IPv4 literal address", vpnconfig.Server{Address: "203.0.113.12"}, "203.0.113.12"},
		{"hostname nothing resolved", vpnconfig.Server{Address: "oslo.example"}, ""},
		{"IPv6 only", vpnconfig.Server{Address: "2001:db8::1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialIP(tc.s); got != tc.want {
				t.Fatalf("dialIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReachable_KeepsTheOrderOfTheCopiesThatAnswered(t *testing.T) {
	w := &Watch{Reachable: func(_ context.Context, ip string, _ int) bool { return ip != "203.0.113.1" }}
	copies := []vpnconfig.Server{
		{Name: "A", IPs: []string{"203.0.113.1"}},
		{Name: "B", IPs: []string{"203.0.113.2"}},
		{Name: "C", IPs: []string{"203.0.113.3"}},
	}
	var names []string
	for _, c := range w.reachable(context.Background(), copies) {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"B", "C"}) {
		t.Fatalf("reachable %v, want [B C]", names)
	}
}

func TestTick_UnreachableServerIsDeadAfterAMinute(t *testing.T) {
	logs := captureLog(t)
	f := deadFake()
	assertDiesAt(t, reachWatch(f, osloServers(), map[string]bool{}), f, FastDeadAfter)
	if !strings.Contains(logs.String(), "reason=unreachable") {
		t.Fatalf("log %q, want the death's reason", logs.String())
	}
}

func TestTick_ReachableServerIsDeadAfterThreeMinutes(t *testing.T) {
	logs := captureLog(t)
	f := deadFake()
	assertDiesAt(t, reachWatch(f, osloServers(), map[string]bool{"203.0.113.10": true}), f, DeadAfter)
	if !strings.Contains(logs.String(), "reason=probe") {
		t.Fatalf("log %q, want the death's reason", logs.String())
	}
}

// One look that found the server up ends the streak for this run of misses.
func TestTick_OneReachableCheckKeepsThreeMinutes(t *testing.T) {
	f := deadFake()
	w := reachWatch(f, osloServers(), nil)
	checks := 0
	w.Reachable = func(context.Context, string, int) bool {
		checks++
		return checks == 2
	}
	assertDiesAt(t, w, f, DeadAfter)
}

func TestTick_NoReachAnswerKeepsThreeMinutes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *Watch)
	}{
		{"server not in servers.json", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) {
				return []vpnconfig.Server{{Name: "Paris", Address: "paris.example", Port: 443, IPs: []string{"203.0.113.30"}}}, nil
			}
		}},
		{"hostname nothing resolved", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) {
				return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
			}
		}},
		{"servers.json unreadable", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) { return nil, errors.New("no such file") }
		}},
		{"no TCP check", func(w *Watch) { w.Reachable = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := deadFake()
			w := reachWatch(f, osloServers(), map[string]bool{})
			tc.setup(w)
			assertDiesAt(t, w, f, DeadAfter)
		})
	}
}

// A working probe ends the run of misses, and the next run's streak starts
// from nothing: a look that found the server up in the last run does not keep
// the next one at three minutes.
func TestTick_HealthyProbeStartsANewUnreachableStreak(t *testing.T) {
	f := deadFake()
	up := map[string]bool{"203.0.113.10": true}
	w := reachWatch(f, osloServers(), up)
	w.Tick(context.Background()) // a miss while the server accepts TCP
	f.now = f.now.Add(ProbeInterval)
	f.probeErr = nil
	w.Tick(context.Background()) // the outbound works again
	f.now = f.now.Add(ProbeInterval)
	f.probeErr = errProbe
	delete(up, "203.0.113.10")
	assertDiesAt(t, w, f, FastDeadAfter)
}
```

- [ ] **Step 3: Run the tests and watch them fail**

Run (build-runner): `cd server && go test ./internal/subwatch -run 'TestDialIP|TestReachable_|TestTick_UnreachableServer|TestTick_ReachableServer|TestTick_OneReachableCheck|TestTick_NoReachAnswer|TestTick_HealthyProbeStarts' -count=1`

Expected: FAIL.
- `TestDialIP`: every case with a non-empty `want` fails (the stub returns `""`).
- `TestReachable_KeepsTheOrder…`: `reachable [], want [B C]`.
- `TestTick_UnreachableServerIsDeadAfterAMinute` and `TestTick_HealthyProbeStartsANewUnreachableStreak`: `no move 1m0s after the first miss`.
- `TestTick_ReachableServerIsDeadAfterThreeMinutes`: the move comes at 3 minutes, but the log lacks `reason=probe`.
- `TestTick_OneReachableCheckKeepsThreeMinutes` and `TestTick_NoReachAnswerKeepsThreeMinutes` pass: they are guards of the 3-minute rule.

- [ ] **Step 4: Implement**

Replace `server/internal/subwatch/reach.go` with:

```go
package subwatch

import (
	"context"
	"net"
	"sync"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// dialIP is the IPv4 address a perAddress copy is dialed at: its one resolved
// address, or an address that is an IPv4 literal itself. "" when it has none -
// a hostname nothing resolved - and the copy then has nothing to check.
func dialIP(c vpnconfig.Server) string {
	for _, ip := range c.IPs {
		if ip != "" {
			return ipv4(ip)
		}
	}
	return ipv4(c.Address)
}

func ipv4(s string) string {
	if ip := net.ParseIP(s).To4(); ip != nil {
		return ip.String()
	}
	return ""
}

// dialable is the copies that have an IPv4 address to dial, in order.
func dialable(copies []vpnconfig.Server) []vpnconfig.Server {
	var out []vpnconfig.Server
	for _, c := range copies {
		if dialIP(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// reachable is the copies whose address accepts a TCP connection, in the order
// given. Every address is dialed at once, so one look costs a ReachTimeout at
// most however many addresses a server has.
func (w *Watch) reachable(ctx context.Context, copies []vpnconfig.Server) []vpnconfig.Server {
	ctx, cancel := context.WithTimeout(ctx, ReachTimeout)
	defer cancel()
	up := make([]bool, len(copies))
	var wg sync.WaitGroup
	for i, c := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up[i] = w.Reachable(ctx, dialIP(c), c.Port)
		}()
	}
	wg.Wait()
	var out []vpnconfig.Server
	for i, c := range copies {
		if up[i] {
			out = append(out, c)
		}
	}
	return out
}

// activeServerDown reports whether the server active_server names accepts no
// TCP connection on any address its servers.json entry lists. Every look
// without an answer is false: no record, no entry, no IPv4 address to dial, a
// look a stop cut short, or a watch without LoadServers or Reachable.
func (w *Watch) activeServerDown(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if w.LoadServers == nil || w.Reachable == nil || cfg == nil || cfg.Xray.ActiveServer == nil {
		return false
	}
	servers, err := w.LoadServers()
	if err != nil {
		return false
	}
	i := chosenIndex(servers, cfg.Xray.ActiveServer)
	if i < 0 {
		return false
	}
	copies := dialable(perAddress(servers[i : i+1]))
	if len(copies) == 0 {
		return false
	}
	up := w.reachable(ctx, copies)
	return len(up) == 0 && ctx.Err() == nil
}

// checkReach adds this tick's look at the active server to the streak of the
// current failSince: one more check that found it down, or the end of the
// streak - until failSince starts over - for a check that did not.
func (w *Watch) checkReach(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.downChecks < 0 {
		return
	}
	if w.activeServerDown(ctx, cfg) {
		w.downChecks++
		return
	}
	w.downChecks = -1
}
```

In `watch.go`, add two methods right above `func (w *Watch) settled()`:

```go
// resetFail forgets a failing outbound: the three minutes - and the streak of
// unreachable checks that can shorten them - start again from its next miss.
func (w *Watch) resetFail() {
	w.failSince = time.Time{}
	w.downChecks = 0
}

// deadReason is why an outbound failing since failSince counts as dead at now,
// and "" while it does not yet. A server that accepted no TCP connection at any
// check since the first miss - two at least - dies after FastDeadAfter; every
// other failure after DeadAfter.
func (w *Watch) deadReason(now time.Time) string {
	failing := now.Sub(w.failSince)
	switch {
	case failing >= FastDeadAfter && w.downChecks >= 2:
		return "unreachable"
	case failing >= DeadAfter:
		return "probe"
	}
	return ""
}
```

Route every reset of `failSince` through `resetFail`, so the streak starts over with it. In `watch.go` there are five:
- `Tick`: the unarmed return, `if !vpnconfig.Armed(cfg) && !w.pendingApply {`.
- `Tick`: the stopped return, `if w.stopped() {` with the comment "Nothing the outbound did while VPN Director is stopped counts".
- `Tick`: the later unarmed return, `if !vpnconfig.Armed(cfg) {` after the `pendingApply` block.
- `Tick`: the healthy probe, `if err == nil {`.
- `settled()`, its first line.

Each one: replace `w.failSince = time.Time{}` with `w.resetFail()`.

In `Tick`, replace

```go
	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	if now.Sub(w.failSince) < DeadAfter {
		slog.Debug("Xray SOCKS probe failed", "socks_port", socks, "error", err)
		return
	}
```

with

```go
	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	w.checkReach(ctx, cfg)
	reason := w.deadReason(now)
	if reason == "" {
		slog.Debug("Xray SOCKS probe failed", "socks_port", socks, "error", err)
		return
	}
```

and a few lines below, the announcement

```go
		slog.Info("Xray outbound declared dead", "socks_port", socks, "error", err)
```

with

```go
		slog.Info("Xray outbound declared dead", "socks_port", socks, "reason", reason, "error", err)
```

- [ ] **Step 5: Run the tests and see them pass**

Run (build-runner): `cd server && go test ./internal/subwatch -count=1 && go test -race ./internal/subwatch -count=1 && gofmt -l ./internal/subwatch`

Expected: PASS for the whole package, under `-race` too; gofmt prints nothing.

- [ ] **Step 6: Document**

In `.claude/rules/telegram-bot.md`, extend the `subwatch/` tree to:

```
│   ├── subwatch/             # Xray outbound watch (SOCKS probe, failover, restore)
│   │   ├── probe.go          # HTTPS 204 through Xray SOCKS
│   │   ├── reach.go          # TCP look at a server's addresses; the unreachable streak
│   │   └── watch.go          # Tick: arming, failover, import, restore
```

In the "Subscription watch" section, in the paragraph that starts "Every 30s the bot probes", insert after "Success is HTTP 204.":

```
A failed probe also dials the active server: every IPv4 address its `servers.json` entry lists (`chosenIndex`, as the walk finds it), `tcp4`, 3 seconds, all at once. When every such look since the first miss found none accepting — two looks at least — the outbound is dead after 1 minute (`FastDeadAfter`) instead of 3, and the log says `reason=unreachable` rather than `reason=probe`. A look without an answer — no record, no entry, no IPv4 address, `servers.json` unreadable, an address the bot does not dial — keeps the 3 minutes for that run of misses; whatever starts the 3 minutes over starts the streak over too. A local failure (Xray restarting, a broken config) leaves the server accepting TCP and still waits the full time.
```

- [ ] **Step 7: Commit**

```bash
git add server/internal/subwatch/reach.go server/internal/subwatch/reach_test.go server/internal/subwatch/watch.go .claude/rules/telegram-bot.md
git commit -F - <<'EOF'
feat(subwatch): declare an unreachable server dead after a minute

The first night of the subscription watch on the RT-AX86U brought four
outages in which the provider's endpoints stopped answering at the IP level
for about ten minutes each. The watch waited the full three minutes before
it moved the Xray clients, and they had no internet for that long.

A failed probe now also dials the active server: every IPv4 address its
servers.json entry lists, tcp4, three seconds, all at once. When every look
since the first miss found none accepting, the outbound is dead after a
minute; a look without an answer keeps the three minutes. A local failure
leaves the server accepting TCP, so it still waits the full time.

The streak belongs to the current run of misses: whatever starts the three
minutes over starts it over too. The death's log line says which rule fired.
EOF
```

---

### Task 2: The bot's reachability check and the wiring

**Files:**
- Create: `server/internal/bot/reach.go`
- Create: `server/internal/bot/reach_test.go`
- Modify: `server/internal/bot/bot.go` (the `subwatch.Watch` literal, ~115-138)
- Modify: `.claude/rules/telegram-bot.md` (tree line under `bot/`)

**Interfaces:**
- Consumes: `subwatch.ReachTimeout` (Task 1), `Watch.LoadServers`, `Watch.Reachable` (Task 1), `ssrf.IsPrivateOrReserved(ip net.IP) bool`, `(*service.ConfigService).LoadServers() ([]vpnconfig.Server, error)` (existing).
- Produces: `type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)`; `func reachTCP4(dial dialFunc) func(ctx context.Context, ip string, port int) bool`.

- [ ] **Step 1: Write the failing tests**

Create `server/internal/bot/reach_test.go`:

```go
package bot

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestReachTCP4_AnAddressThatAcceptsIsReachable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	var network, dialed string
	reach := reachTCP4(func(ctx context.Context, n, addr string) (net.Conn, error) {
		network, dialed = n, addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", ln.Addr().String())
	})
	if !reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("an address that accepts is reachable")
	}
	if network != "tcp4" || dialed != "203.0.113.10:443" {
		t.Fatalf("dialed %s %s, want tcp4 203.0.113.10:443", network, dialed)
	}
}

func TestReachTCP4_AClosedPortIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	reach := reachTCP4(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	if reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("a closed port is unreachable")
	}
}

func TestReachTCP4_AddressesTheBotDoesNotDialCountAsReachable(t *testing.T) {
	dials := 0
	reach := reachTCP4(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	})
	for _, ip := range []string{"192.168.1.10", "127.0.0.1", "100.64.0.1", "2001:db8::1", "not-an-ip"} {
		if !reach(context.Background(), ip, 443) {
			t.Fatalf("%s: an address the bot does not dial counts as reachable", ip)
		}
	}
	if dials != 0 {
		t.Fatalf("dials %d, want none", dials)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run (build-runner): `cd server && go test ./internal/bot -run 'TestReachTCP4' -count=1`

Expected: FAIL to compile with `undefined: reachTCP4` — the function is new.

- [ ] **Step 3: Implement**

Create `server/internal/bot/reach.go`:

```go
package bot

import (
	"context"
	"net"
	"strconv"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subwatch"
)

// dialFunc is the dial reachTCP4 makes; tests hand it one of their own.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// reachTCP4 is the subscription watch's reachability check: does ip accept a
// TCP connection on port within subwatch.ReachTimeout. An address the bot does
// not dial on a subscription's say-so - not IPv4, private, loopback, reserved -
// counts as reachable without a dial, so the watch neither declares a server
// behind it dead early nor holds a return back for it. A nil dial uses a
// net.Dialer.
func reachTCP4(dial dialFunc) func(ctx context.Context, ip string, port int) bool {
	if dial == nil {
		d := &net.Dialer{Timeout: subwatch.ReachTimeout}
		dial = d.DialContext
	}
	return func(ctx context.Context, ip string, port int) bool {
		addr := net.ParseIP(ip).To4()
		if addr == nil || ssrf.IsPrivateOrReserved(addr) {
			return true
		}
		conn, err := dial(ctx, "tcp4", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
}
```

In `bot.go`, add the two fields to the `subwatch.Watch` literal right after `SaveServers`:

```go
			SaveServers:  configSvc.SaveServers,
			LoadServers:  configSvc.LoadServers,
			Reachable:    reachTCP4(nil),
```

- [ ] **Step 4: Run the tests and see them pass**

Run (build-runner): `cd server && go build ./... && go test ./internal/bot -count=1 && go test -race ./internal/bot -count=1 && gofmt -l ./internal/bot`

Expected: build OK; PASS, under `-race` too; gofmt prints nothing.

- [ ] **Step 5: Document**

In `.claude/rules/telegram-bot.md`, add to the `bot/` tree, above `subfetch.go`:

```
│   │   ├── reach.go          # tcp4 reachability check for the subscription watch
```

- [ ] **Step 6: Commit**

```bash
git add server/internal/bot/reach.go server/internal/bot/reach_test.go server/internal/bot/bot.go .claude/rules/telegram-bot.md
git commit -F - <<'EOF'
feat(bot): give the subscription watch a TCP reachability check

The watch reads servers.json through the config service and dials with
reachTCP4: a tcp4 connect bounded by subwatch.ReachTimeout. An address the
bot does not dial on a subscription's say-so - not IPv4, private, loopback,
reserved - counts as reachable without a dial, so the watch never acts on it
early.
EOF
```

---

### Task 3: Return to the preferred server

**Files:**
- Create: `server/internal/subwatch/return.go`
- Create: `server/internal/subwatch/return_test.go`
- Modify: `server/internal/subwatch/watch.go` (message and note constants ~36-58, `Watch` fields, the healthy branch and the death path of `Tick`, `settled`, the walk's pick in `maybeImportAndPick` ~912)
- Modify: `.claude/rules/telegram-bot.md` (tree, "Subscription watch")

**Interfaces:**
- Consumes: from Task 1 `Watch.LoadServers`, `Watch.Reachable`, `dialIP`, `dialable`, `(*Watch).reachable`; existing `chosenIndex`, `perAddress`, `activeID`, `serverID`, `(*Watch).walkGuard(rawURL, started, lastRecorded string, expectedSeq int) func(*vpnconfig.VPNDirectorConfig) error`, `endsWalk(err error) bool`, `(*Watch).restartXray() error`, `(*Watch).socksPort(cfg) int`, `(*Watch).notify(kind noteKind, msg string)`, `errStopped`, `SettleAfterRestart`, `vpnconfig.RecordWalkedServer`, `vpnconfig.RecordActiveServer`, `vpnconfig.ActiveSeq`; test helpers `fake`, `runningWatch`, `baseCfg`, `committedCfg`, `failedOverCfg`, `liveImportWatch`, `selectManual`, `tickUntilDead`, `connected`, `errProbe`, `errApply`.
- Produces: constants `ReturnCheck`, `ReturnRetry`, `ReturnRetryMax`; `msgReturned`, `noteReturned`; fields `returnNotBefore time.Time`, `returnRetry time.Duration`, `lastPicked *vpnconfig.Server`; `(*Watch).maybeReturn(ctx, cfg)`, `(*Watch).tryReturn(ctx, cfg, servers, candidates)`, `type switcher`, `rollbackOrder(copies []vpnconfig.Server, last *vpnconfig.Server) []vpnconfig.Server`, `(*Watch).backOffReturn()`.

- [ ] **Step 1: Add the API surface without behaviour**

In `watch.go`, add to the message constants (after `msgPicked`):

```go
	msgReturned         = "Xray back on the preferred server %s"
```

and to the `noteKind` constants (after `noteRestored`):

```go
	noteReturned
```

Add three state fields right after `running bool`, the last field of the struct. `running` has no comment, so they form their own alignment run and no existing line moves:

```go
	running           bool
	returnNotBefore   time.Time         // no look for the preferred server before this
	returnRetry       time.Duration     // wait after the last failed return; zero before any
	lastPicked        *vpnconfig.Server // the copy the walk picked or a return proved, with the address it ran on
}
```

Create `server/internal/subwatch/return.go` with the constants and a no-op:

```go
package subwatch

import (
	"context"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	// ReturnCheck is how often a healthy watch looks whether the server the
	// user chose accepts TCP again while a walk has another one running.
	ReturnCheck = 5 * time.Minute
	// ReturnRetry is the wait after a return whose probe failed - an address
	// can accept TCP and still refuse the proxy - doubling up to ReturnRetryMax.
	ReturnRetry    = 10 * time.Minute
	ReturnRetryMax = 30 * time.Minute
)

func (w *Watch) maybeReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {}
```

In `Tick`, call it from the healthy branch:

```go
	err = w.Probe(ctx, socks)
	if err == nil {
		w.resetFail()
		w.importRetry = 0
		w.lastRouteKind = noteNone
		w.lastImportKind = noteNone
		w.maybeReturn(ctx, cfg)
		return
	}
```

- [ ] **Step 2: Write the failing tests**

Create `server/internal/subwatch/return_test.go`:

```go
package subwatch

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	osloIP    = "203.0.113.10"
	madridIP  = "203.0.113.20"
	madridIP2 = "203.0.113.21"
)

func returnServers() []vpnconfig.Server {
	return []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{osloIP}},
		{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP}},
	}
}

// awayCfg is a healthy router a walk left on Madrid while the user chose Oslo.
func awayCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Madrid", Address: "madrid.example", Port: 443, Seq: 7}
	cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	return cfg
}

// returnRig is a watch whose Generate records the way production's
// GenerateAndRecordWalkedServer does. running is the address Xray was last
// generated for - "" for the server it started on, which works - and the probe
// passes for the addresses in live. events lists every Generate as "name@ip"
// and every restart as "restart".
type returnRig struct {
	f       *fake
	w       *Watch
	up      map[string]bool // addresses that accept TCP
	live    map[string]bool // addresses the SOCKS probe passes on
	running string
	events  []string
	onProbe func() // runs at every probe of a switched Xray, before it answers
}

func newReturnRig(servers []vpnconfig.Server) *returnRig {
	r := &returnRig{
		f:    &fake{cfg: awayCfg(), now: time.Unix(1_700_000_000, 0)},
		up:   map[string]bool{},
		live: map[string]bool{},
	}
	w := runningWatch(r.f.watch())
	w.LoadServers = func() ([]vpnconfig.Server, error) { return servers, nil }
	w.Reachable = func(_ context.Context, ip string, _ int) bool { return r.up[ip] }
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := r.f.checkGuard(guard); err != nil {
			return false, r.f.seq(), err
		}
		r.running = dialIP(s)
		r.events = append(r.events, s.Name+"@"+r.running)
		vpnconfig.RecordWalkedServer(r.f.cfg, s)
		return true, r.f.seq(), nil
	}
	w.RestartXray = func() error {
		r.events = append(r.events, "restart")
		return nil
	}
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if r.running == "" {
			return nil
		}
		if r.onProbe != nil {
			r.onProbe()
		}
		if r.live[r.running] {
			return nil
		}
		return errProbe
	}
	r.w = w
	return r
}

func (r *returnRig) tick() { r.w.Tick(context.Background()) }

// attempts counts the switches to Oslo.
func (r *returnRig) attempts() int {
	n := 0
	for _, e := range r.events {
		if e == "Oslo@"+osloIP {
			n++
		}
	}
	return n
}

func TestTick_ReturnsToThePreferredServerOnceItAnswers(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Oslo" {
		t.Fatalf("active %+v, want Oslo", a)
	}
	if p := r.f.cfg.Xray.PreferredServer; p != nil {
		t.Fatalf("preferred %+v, want none once Oslo runs again", p)
	}
	if want := []string{"Xray back on the preferred server Oslo"}; !reflect.DeepEqual(r.f.notes, want) {
		t.Fatalf("notes %v, want %v", r.f.notes, want)
	}
	r.f.now = r.f.now.Add(ReturnCheck)
	r.tick()
	if len(r.events) != 2 {
		t.Fatalf("events %v; nothing is left to return to", r.events)
	}
}

func TestTick_FailedReturnGoesBackAndBacksOff(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	start := r.f.now
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" {
		t.Fatalf("active %+v, want Madrid back", a)
	}
	if p := r.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept", p)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v; a failed return tells nobody", r.f.notes)
	}
	// The next attempts wait 10, 20, then 30 minutes.
	for i, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute, 90 * time.Minute} {
		r.f.now = start.Add(at - time.Second)
		r.tick()
		if n := r.attempts(); n != i+1 {
			t.Fatalf("attempts %d at %v, want %d", n, at-time.Second, i+1)
		}
		r.f.now = start.Add(at)
		r.tick()
		if n := r.attempts(); n != i+2 {
			t.Fatalf("attempts %d at %v, want %d", n, at, i+2)
		}
	}
}

func TestTick_ReturnWaitsForThePreferredServerToAcceptTCP(t *testing.T) {
	r := newReturnRig(returnServers())
	r.live[osloIP] = true
	start := r.f.now
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; Oslo accepts no TCP yet", r.events)
	}
	r.up[osloIP] = true
	r.f.now = start.Add(ReturnCheck - time.Second)
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the next look is %v after the last", r.events, ReturnCheck)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_ARestoreHoldsTheFirstReturnForFiveMinutes(t *testing.T) {
	r := newReturnRig(returnServers())
	cfg := committedCfg()
	cfg.Xray.ActiveServer = awayCfg().Xray.ActiveServer
	cfg.Xray.PreferredServer = awayCfg().Xray.PreferredServer
	r.f.cfg = cfg
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick() // the probe passes while failed over: the clients come back
	if r.f.cfg.Xray.Failover != nil {
		t.Fatal("the probe that passed must restore the clients")
	}
	start := r.f.now
	r.f.now = start.Add(ReturnCheck - time.Second)
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the preferred server failed minutes ago", r.events)
	}
	r.f.now = start.Add(ReturnCheck)
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_NoReturnWhileARestoreApplyIsPending(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.f.applyErr = errApply
	r.w.pendingApply = true
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; the pending apply comes first", r.events)
	}
}

func TestTick_ASelectionDuringTheReturnStands(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.onProbe = func() {
		if r.running == osloIP {
			// The Web UI selects a server while the watch probes Oslo.
			selectManual(r.f)
			r.f.cfg.Xray.PreferredServer = nil
		}
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v: nothing is written over the selection", r.events, want)
	}
	if a := r.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Manual" {
		t.Fatalf("active %+v, want the selection", a)
	}
}

func TestTick_AStopDuringTheReturnEndsIt(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	var stopped atomic.Bool
	r.w.Stopped = stopped.Load
	r.w.RestartXray = func() error {
		r.events = append(r.events, "restart")
		stopped.Store(true) // the stop finishes while Xray restarts
		return nil
	}
	r.tick()
	if want := []string{"Oslo@" + osloIP, "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if len(r.f.notes) != 0 {
		t.Fatalf("notes %v", r.f.notes)
	}
}

// A bot restart forgets which address the previous server ran on: the rollback
// tries its addresses in order.
func TestTick_RollbackTriesEveryAddressOfThePreviousServer(t *testing.T) {
	servers := returnServers()
	servers[1].IPs = []string{madridIP, madridIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[madridIP2] = true
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP, "restart", "Madrid@" + madridIP2, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.w.lastPicked == nil || dialIP(*r.w.lastPicked) != madridIP2 {
		t.Fatalf("lastPicked %+v, want the address that came back", r.w.lastPicked)
	}
}

func TestTick_RollbackStartsWithTheAddressTheWalkPicked(t *testing.T) {
	servers := returnServers()
	servers[1].IPs = []string{madridIP, madridIP2}
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[madridIP2] = true
	r.w.lastPicked = &vpnconfig.Server{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{madridIP2}}
	r.tick()
	want := []string{"Oslo@" + osloIP, "restart", "Madrid@" + madridIP2, "restart"}
	if !reflect.DeepEqual(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}

func TestTick_ADeathStartsTheReturnsOver(t *testing.T) {
	r := newReturnRig(returnServers())
	r.up[osloIP] = true
	r.live[madridIP] = true
	r.f.plat = connected("ovpnc2")
	r.tick() // a failed return: the next waits ReturnRetry
	if r.w.returnRetry != ReturnRetry {
		t.Fatalf("returnRetry %v, want %v", r.w.returnRetry, ReturnRetry)
	}
	r.live[madridIP] = false // Madrid dies
	r.f.now = r.f.now.Add(ProbeInterval)
	tickUntilDead(r.w, r.f)
	if r.f.cfg.Xray.Failover == nil {
		t.Fatal("no failover")
	}
	if r.w.returnRetry != 0 {
		t.Fatalf("returnRetry %v; a death starts the returns over", r.w.returnRetry)
	}
}

func TestTick_NoReturnToAPreferredServerWithoutAnAddress(t *testing.T) {
	servers := returnServers()
	servers[0].IPs = nil // oslo.example resolved to nothing at the import
	r := newReturnRig(servers)
	r.up[osloIP] = true
	r.live[osloIP] = true
	r.tick()
	if len(r.events) != 0 {
		t.Fatalf("events %v; there is no address to check", r.events)
	}
}

func TestTick_WalkRemembersTheAddressItPicked(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())
	if w.lastPicked == nil || w.lastPicked.Name != "Oslo" || dialIP(*w.lastPicked) != "203.0.113.10" {
		t.Fatalf("lastPicked %+v, want Oslo at 203.0.113.10", w.lastPicked)
	}
}
```

- [ ] **Step 3: Run the tests and watch them fail**

Run (build-runner): `cd server && go test ./internal/subwatch -run 'TestTick_Returns|TestTick_FailedReturn|TestTick_ReturnWaits|TestTick_ARestoreHolds|TestTick_NoReturn|TestTick_ASelectionDuring|TestTick_AStopDuring|TestTick_Rollback|TestTick_ADeathStarts|TestTick_WalkRemembers' -count=1`

Expected: FAIL.
- The success, failure, cadence, restore-hold, selection, stop and rollback tests fail with `events [], want [...]`.
- `TestTick_ADeathStartsTheReturnsOver`: `returnRetry 0s, want 10m0s`.
- `TestTick_WalkRemembersTheAddressItPicked`: `lastPicked <nil>`.
- `TestTick_NoReturnWhileARestoreApplyIsPending` and `TestTick_NoReturnToAPreferredServerWithoutAnAddress` pass: they are guards.

- [ ] **Step 4: Implement**

Replace `server/internal/subwatch/return.go` with:

```go
package subwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	// ReturnCheck is how often a healthy watch looks whether the server the
	// user chose accepts TCP again while a walk has another one running.
	ReturnCheck = 5 * time.Minute
	// ReturnRetry is the wait after a return whose probe failed - an address
	// can accept TCP and still refuse the proxy - doubling up to ReturnRetryMax.
	ReturnRetry    = 10 * time.Minute
	ReturnRetryMax = 30 * time.Minute
)

// maybeReturn takes Xray back to the server the user chose once a walk has left
// another one running and the chosen one accepts TCP again. It runs after a
// probe that passed, with no failover and no restore apply outstanding, and
// looks every ReturnCheck - after a failed return, ReturnRetry and longer.
func (w *Watch) maybeReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.PreferredServer == nil {
		w.returnRetry = 0
		return
	}
	if w.LoadServers == nil || w.Reachable == nil || w.Generate == nil {
		return
	}
	if cfg.Xray.Failover != nil || w.pendingApply || w.pendingRestore != nil {
		return
	}
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	if active == nil || active.Name == preferred.Name {
		return
	}
	now := w.Now()
	if now.Before(w.returnNotBefore) {
		return
	}
	w.returnNotBefore = now.Add(ReturnCheck)
	servers, err := w.LoadServers()
	if err != nil {
		return
	}
	i := chosenIndex(servers, preferred)
	if i < 0 {
		return
	}
	candidates := w.reachable(ctx, dialable(perAddress(servers[i:i+1])))
	if len(candidates) == 0 || ctx.Err() != nil || w.stopped() {
		return
	}
	w.tryReturn(ctx, cfg, servers, candidates)
}

// tryReturn switches Xray to each reachable copy of the preferred server in
// turn and keeps the first the probe finds live. With none it switches back to
// the server that ran before - the address the walk picked first, when this
// process remembers it - and holds the next attempt back. Every write carries
// the walk's guard: a stop, a newly saved link or a selection made meanwhile
// refuses it, and the attempt ends there.
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, servers, candidates []vpnconfig.Server) {
	before := cfg.Xray.ActiveServer
	sw := &switcher{
		w:       w,
		rawURL:  cfg.Xray.SubscriptionURL,
		started: activeID(before),
		seq:     vpnconfig.ActiveSeq(before),
		socks:   w.socksPort(cfg),
	}
	slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)
	for _, c := range candidates {
		live, ended := sw.to(ctx, c)
		if ended {
			return
		}
		if live {
			slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			w.returnRetry = 0
			w.notify(noteReturned, fmt.Sprintf(msgReturned, c.Name))
			return
		}
	}
	w.backOffReturn()
	if !sw.wrote {
		return
	}
	j := chosenIndex(servers, before)
	if j < 0 {
		slog.Warn("Return to the preferred server failed and the previous server is no longer listed", "previous", before.Name)
		return
	}
	for _, c := range rollbackOrder(perAddress(servers[j:j+1]), w.lastPicked) {
		live, ended := sw.to(ctx, c)
		if ended {
			return
		}
		if live {
			slog.Info("Return to the preferred server failed; back on the previous server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			return
		}
	}
	slog.Warn("Return to the preferred server failed and the previous server did not come back", "previous", before.Name)
}

// switcher is one return attempt's run of switches. It carries the walk's
// bookkeeping from write to write - the record the last one left and the
// counter it moved to - which the guard of the next compares with the config.
type switcher struct {
	w            *Watch
	rawURL       string
	started      string
	lastRecorded string
	seq          int
	socks        int
	wrote        bool // a config.json has been written
}

// to writes c as the running server, restarts Xray and probes it. live is a
// probe that passed; ended is a write the guard refused or a restart a stop
// skipped, after which nothing more may be written.
func (s *switcher) to(ctx context.Context, c vpnconfig.Server) (live, ended bool) {
	w := s.w
	generated, seq, err := w.Generate(c, w.walkGuard(s.rawURL, s.started, s.lastRecorded, s.seq))
	if endsWalk(err) {
		return false, true
	}
	if !generated {
		slog.Warn("Generating Xray config for server failed", "server", c.Name, "error", err)
		return false, false
	}
	s.wrote = true
	if err == nil {
		s.lastRecorded = serverID(c)
	}
	s.seq = seq
	if ctx.Err() != nil {
		return false, true
	}
	if err := w.restartXray(); err != nil {
		if errors.Is(err, errStopped) {
			return false, true
		}
		slog.Warn("Xray restart failed", "server", c.Name, "error", err)
		return false, false
	}
	w.AfterRestart(SettleAfterRestart)
	if err := w.Probe(ctx, s.socks); err != nil {
		slog.Info("Server probe failed", "server", c.Name, "ips", c.IPs, "error", err)
		return false, false
	}
	return true, false
}

// rollbackOrder is the copies of the server that ran before, with the copy the
// walk picked or a return proved first: the address it ran on, when this
// process remembers it.
func rollbackOrder(copies []vpnconfig.Server, last *vpnconfig.Server) []vpnconfig.Server {
	if last == nil {
		return copies
	}
	for i, c := range copies {
		if serverID(c) != serverID(*last) || dialIP(c) != dialIP(*last) {
			continue
		}
		out := make([]vpnconfig.Server, 0, len(copies))
		out = append(out, c)
		out = append(out, copies[:i]...)
		return append(out, copies[i+1:]...)
	}
	return copies
}

// backOffReturn holds the next attempt back after a return that failed:
// ReturnRetry, then twice the last wait, ReturnRetryMax at most.
func (w *Watch) backOffReturn() {
	if w.returnRetry == 0 {
		w.returnRetry = ReturnRetry
	} else {
		w.returnRetry = min(2*w.returnRetry, ReturnRetryMax)
	}
	w.returnNotBefore = w.Now().Add(w.returnRetry)
	slog.Info("Next return attempt backed off", "after", w.returnRetry)
}
```

In `watch.go`:

1. `settled()` holds the first look after every completed restore:

```go
func (w *Watch) settled() {
	w.resetFail()
	w.lastImport = time.Time{}
	w.importRetry = 0
	// The preferred server failed minutes ago: the first look at it waits.
	w.returnNotBefore = w.Now().Add(ReturnCheck)
}
```

Update its doc comment to: `// settled is a working outbound: the three minutes start again from its next miss, the next death refreshes the subscription at once, and the first look for the preferred server waits ReturnCheck.`

2. The death path starts the returns over. In `Tick`, right above `announce := w.lastRouteKind != noteNoTunnel`, add:

```go
	// A death is a new episode: whatever the returns backed off to is done with.
	w.returnRetry = 0
```

3. The walk remembers what it picked. In `maybeImportAndPick`, right below `slog.Info("Subscription server picked", "server", s.Name, "ips", s.IPs)`, add:

```go
		w.lastPicked = &s
```

- [ ] **Step 5: Run the tests and see them pass**

Run (build-runner): `cd server && go test ./internal/subwatch -count=1 && go test -race ./internal/subwatch -count=1 && gofmt -l ./internal/subwatch`

Expected: PASS for the whole package, under `-race` too; gofmt prints nothing.

- [ ] **Step 6: Document**

In `.claude/rules/telegram-bot.md`, add `return.go` to the `subwatch/` tree:

```
│   │   ├── reach.go          # TCP look at a server's addresses; the unreachable streak
│   │   ├── return.go         # Return to the preferred server
│   │   └── watch.go          # Tick: arming, failover, import, restore
```

In the "Subscription watch" section:
- Change "Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored)" to "Telegram gets one message per state change (moved, no tunnel, refresh failed, no live server, restored, back on the preferred server)".
- Append a paragraph at the end of the section:

```
While Xray works and a walk has left another server running (`xray.preferred_server` names the user's choice), the watch looks every 5 minutes (`ReturnCheck`) whether the preferred server's `servers.json` entry accepts TCP; a completed restore holds the first look for 5 minutes, since that server has just failed. When an address accepts, the watch switches Xray to it the way the walk does — `Generate` with the walk's guard, `restart xray-process`, 3 seconds, a SOCKS probe. A live probe ends it: `RecordWalkedServer` clears `preferred_server` and Telegram gets "Xray back on the preferred server <name>". A dead one switches back to the server that ran before — the address the walk picked first, when the process remembers it, else each address of that entry in turn — says nothing, and waits 10, 20, then 30 minutes (`ReturnRetry`, `ReturnRetryMax`) before the next attempt; a death starts that interval over. A Web UI, `/xray` or wizard selection clears `preferred_server` and so ends the returns; one that commits during an attempt refuses the watch's next write and stands. A successful return costs one `restart xray-process`, a failed one two.
```

- [ ] **Step 7: Commit**

```bash
git add server/internal/subwatch/return.go server/internal/subwatch/return_test.go server/internal/subwatch/watch.go .claude/rules/telegram-bot.md
git commit -F - <<'EOF'
feat(subwatch): return to the preferred server once it answers

A walk that picked another server left Xray there until the next death, with
the user's choice kept in preferred_server. On the router that meant Madrid
instead of Oslo for good after the first outage.

A healthy watch now looks every five minutes whether the preferred server
accepts TCP, and when an address does, switches Xray to it the way the walk
does: the walk's guard on every write, restart xray-process, a SOCKS probe.
A live probe ends it with one message; a dead one switches back to the server
that ran - the address the walk picked first - and waits 10, 20, then 30
minutes before the next attempt. A completed restore holds the first look
for five minutes, and a death starts the interval over.
EOF
```

---

### Task 4: Bounded shell DNS

**Files:**
- Modify: `router/opt/vpn-director/lib/common.sh` (above the `_resolve_ip_impl` header ~96; the `nslookup` calls ~200 and ~228)
- Modify: `router/test/mocks/nslookup`
- Modify: `router/test/common.bats` (the `resolve_ip` section ~139-159)
- Modify: `.claude/rules/shell-conventions.md` (section "A dual-family DNS lookup on the router often never answers")

**Interfaces:**
- Consumes: `load_common` (bats helper), `$BATS_TEST_TMPDIR`.
- Produces: `_resolve_nslookup` (shell function); mock contract: with `NSLOOKUP_ENV_LOG` set, the mock appends `RES_OPTIONS=<value>` per call to that file.

- [ ] **Step 1: Let the mock report its environment**

In `router/test/mocks/nslookup`, insert after the shebang line:

```bash
# A test that sets NSLOOKUP_ENV_LOG finds there the RES_OPTIONS each call ran with.
if [[ -n ${NSLOOKUP_ENV_LOG:-} ]]; then
    printf 'RES_OPTIONS=%s\n' "${RES_OPTIONS-}" >> "$NSLOOKUP_ENV_LOG"
fi
```

- [ ] **Step 2: Write the failing tests**

In `router/test/common.bats`, add after the test `resolve_ip: -q suppresses error on failure`:

```bash
@test "resolve_ip: nslookup runs with the resolver's wait bounded" {
    load_common
    export NSLOOKUP_ENV_LOG="$BATS_TEST_TMPDIR/nslookup_env"
    run resolve_ip example.com
    assert_success
    assert_output "93.184.216.34"
    run cat "$NSLOOKUP_ENV_LOG"
    assert_output "RES_OPTIONS=timeout:1 attempts:2"
}

@test "resolve_ip -a: nslookup runs with the resolver's wait bounded" {
    load_common
    export NSLOOKUP_ENV_LOG="$BATS_TEST_TMPDIR/nslookup_env"
    run resolve_ip -a example.com
    assert_success
    assert_output "93.184.216.34"
    run cat "$NSLOOKUP_ENV_LOG"
    assert_output "RES_OPTIONS=timeout:1 attempts:2"
}
```

- [ ] **Step 3: Run the tests and watch them fail**

Run (main session): `bats router/test/common.bats -f "wait bounded"`

Expected: 2 tests, 2 failures. Each shows `RES_OPTIONS=` (empty) against the expected `RES_OPTIONS=timeout:1 attempts:2`.

- [ ] **Step 4: Implement**

In `router/opt/vpn-director/lib/common.sh`, insert above the `_resolve_ip_impl` header block:

```bash
###################################################################################################
# _resolve_nslookup - nslookup with the resolver's wait bounded
# -------------------------------------------------------------------------------------------------
# Asuswrt-Merlin's nslookup (BusyBox 1.25) takes no -type and asks glibc for A and AAAA at once,
# straight at the WAN DNS servers in /etc/resolv.conf, with glibc's default of 5 s per try, two
# tries and two servers: a name whose AAAA answer is lost cost up to 20 s, and tproxy_apply
# resolves every OpenVPN endpoint on each run. glibc 2.26 has no no-aaaa option, but it reads
# RES_OPTIONS: timeout:1 attempts:2 caps a stuck name at about 4 s and changes no answer.
# KeeneticOS already resolves through a local proxy with timeout:1.
###################################################################################################
_resolve_nslookup() {
    RES_OPTIONS="timeout:1 attempts:2" nslookup "$@"
}

```

In `_resolve_ip_impl`, replace both occurrences of

```bash
            nslookup "$host" 2>/dev/null |
```

with

```bash
            _resolve_nslookup "$host" 2>/dev/null |
```

(one in the first-match branch, one in the `-a` branch; the indentation is the same in both).

- [ ] **Step 5: Run the tests and see them pass**

Run (main session): `bats router/test/common.bats`

Expected: all pass, the two new tests included.

Run (main session, from `router/opt/vpn-director`): `shellcheck -x lib/common.sh`

Expected: the baseline only — SC1091.

- [ ] **Step 6: Document**

In `.claude/rules/shell-conventions.md`, section "A dual-family DNS lookup on the router often never answers", append after the paragraph that ends "keep the connect timeout above five seconds so the resolver's second attempt can land.":

```
The shell resolver cannot ask for one family: BusyBox 1.25's `nslookup` on Asuswrt-Merlin takes no
`-type`, and glibc 2.26 predates `options no-aaaa`. `_resolve_ip_impl` bounds the wait instead:
`_resolve_nslookup` runs `nslookup` with `RES_OPTIONS="timeout:1 attempts:2"`, which glibc reads, so
a lost AAAA answer costs about a second per try rather than five. On an RT-AX86U the five OpenVPN
endpoint lookups for `TPROXY_BYPASS` had added about 20 s to every second `tproxy_apply`, the
subscription watch's failover and restore included. KeeneticOS resolves through a local proxy with
`timeout:1` already.
```

- [ ] **Step 7: Commit**

```bash
git add router/opt/vpn-director/lib/common.sh router/test/mocks/nslookup router/test/common.bats .claude/rules/shell-conventions.md
git commit -F - <<'EOF'
fix(shell): bound the resolver's wait in resolve_ip

tproxy_apply resolves every OpenVPN client endpoint on each run, and on
Asuswrt-Merlin that goes through BusyBox 1.25 nslookup: A and AAAA at once
through glibc, straight at the WAN DNS servers, 5 s per try, two tries, two
servers. A lost AAAA answer cost up to 20 s, which the router showed in about
every second apply - the subscription watch's failover and restore included.

Both nslookup calls in _resolve_ip_impl now run with RES_OPTIONS
"timeout:1 attempts:2". glibc 2.26 has no option to skip AAAA and BusyBox's
nslookup no -type, so the wait is bounded instead: about a second per lost
answer, and no answer changes.
EOF
```

---

### Task 5: Full verification

**Files:** none changed.

- [ ] **Step 1: Go**

Run (build-runner), in `server/`:
- `go build ./...`
- `go vet ./...`
- `go test ./... -count=1`
- `go test -race -count=1 ./internal/subwatch ./internal/vpnconfig ./internal/service ./internal/bot ./internal/webapi ./internal/handler ./internal/wizard`
- `gofmt -l ./internal ./cmd`

Expected: all 24 packages pass, the `-race` run too; gofmt lists only the baseline (`internal/ssrf/ssrf_test.go`, `internal/wizard/handler.go`).

- [ ] **Step 2: Bats**

Run (main session, after the `-race` run has finished): `bats router/test/*.bats router/test/unit router/test/integration`

Expected: all pass — 593 (591 before plus the two of Task 4).

- [ ] **Step 3: shellcheck**

Run (main session, from `router/opt/vpn-director`): `shellcheck -x lib/common.sh`

Expected: SC1091 only.

- [ ] **Step 4: Report**

Tell the owner, in Russian, what passed with the numbers, and wait. The PR (Task 6) and deploying a build to the router both need the owner's go-ahead.

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
