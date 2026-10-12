# The User's Choice by Kind Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `vpn-director-watchd` knows the user's choice by its subscription, name and kind (`Server.Group`), so a same-named server of another kind never passes for it.

**Architecture:**
- `vpnconfig` gains the rule: `SameChoice` (a record against a list entry) and `SameChoiceRecord` (a record against a record). `RecordWalkedServer` uses it.
- `subwatch` applies it in three places: `chosenIndex` (the walk's first candidate, the return's target, the reach look, the rollback), `classifyActive` (current, changed, moved, gone) and `maybeReturn`'s "already on the preferred server" check.
- The keeper's two-server messages carry each server's kind when the two labels read alike.

**Tech Stack:** Go 1.25 (`server/`), Markdown docs. No new dependency.

**Spec:** `docs/superpowers/specs/2026-10-12-choice-by-kind-design.md`. It builds on `docs/superpowers/specs/2026-10-12-keep-xray-working-design.md`.

## Global Constraints

- **The rule:** an entry is the choice a record names when its subscription and name are the record's, and the record has no `group` or the entry's `Group()` is the record's `group`.
- **A record without `group`** (`configure.sh`, older daemons, an outbound that does not parse): subscription and name, as now.
- **`SameChoiceRecord(a, b)`:** the same rule with `b.Group` as the entry's kind; `a` is the choice, so a `b` without `group` is not of `a`'s kind when `a` has one. Either record `nil` gives `false`.
- **Messages, exact:**
  - `Xray server %s left its subscription; Xray switched to %s`
  - `Xray server %s is not answering; Xray switched to %s`

  `%s` is `<subscription> / <server>` (`label`). When the two labels read alike, each becomes `<label> (<group>)`, the group omitted where it is empty. Every other message is unchanged.
- **Unchanged:**
  - the JSON of `active_server` and `preferred_server`, the config keys, the watchd socket, every shell script and `configure.sh`;
  - `exactFastServer`, `refFor`, `followRenames`, the Web UI's Active badge, `/xray`'s ✓, `pending_restore`, the deletion of a subscription.
- **Language and style:**
  - code, comments, commit messages and docs in English;
  - comments are full sentences that say why, in the surrounding style, with no TODO;
  - no line of a Go doc comment starts with `- ` (gofmt turns it into a list item);
  - no log line carries a subscription link.
- **Tests:**
  - `cd server && go test ./...`; `make -C server test` runs `go test -v ./...`;
  - `-race` on `subwatch` and `vpnconfig`;
  - in the main session every build and test command runs through `build-forge:build-runner` with `model: opus`.
- **The owner's rules:**
  - stage by name only; never stage `.claude/settings.local.json`, the untracked `docs/session-transfer-*.md`, the untracked prompt files under `docs/superpowers/plans/`, `docs/superpowers/2026-10-09-*.md`, `review.diff` or `test_exit.sh`;
  - every subagent runs on opus with the model passed explicitly; never Fable, no haiku;
  - never push, open a PR or post `@codex review`; never touch the routers.

## Review Focus

1. **Rotation within the kind is no move away.** A write onto the choice's name and kind at a new address clears `preferred_server`, and the walk finds that entry first. Pinned: Task 1's `RecordWalkedServer` test (its second half) and Task 2's walk-order test.
2. **A record without `group` keeps the name rule** in `SameChoice`, `SameChoiceRecord`, `chosenIndex` and `classifyActive`. Pinned: the no-group rows of Task 1's tables, Task 2's `chosenIndex` test (its legacy assertion) and Task 2's `classifyActive` row "a record without a group follows an entry of any kind".
3. **Twins of different kinds on one address and port** (a panel's fallback inbounds). The record's own twin stays current, and the other kind is never followed as "changed in place". Pinned: Task 2's `classifyActive` rows "current: another kind at the record's address and port beside it" and "gone: another kind at the record's address and port".
4. **A follow of a moved server of the same kind keeps the choice**: no `preferred_server` after the follow. Pinned already by `TestKeeper_FollowsAMovedServer`, which asserts `PreferredServer == nil`; Task 2's package run must keep it green.
5. **The fast path that lands on a same-named server of another kind keeps the choice**, and the return can then bring Xray back. Pinned: Task 1's fast-path test and Task 2's `maybeReturn` test.

---

## File Structure

| File | Responsibility |
|---|---|
| `server/internal/vpnconfig/vpnconfig.go` | `SameChoice`, `SameChoiceRecord`; `RecordWalkedServer` uses them |
| `server/internal/subwatch/watch.go` | `chosenIndex` finds the choice by kind |
| `server/internal/subwatch/order.go` | `walkOrder`'s doc comment |
| `server/internal/subwatch/actuality.go` | `classifyActive` counts only entries of the choice |
| `server/internal/subwatch/return.go` | `maybeReturn`'s check by `SameChoiceRecord` |
| `server/internal/subwatch/keeper.go` | `replaceActive` takes the replaced record and names kinds; `withKind` |
| `.claude/rules/watchd.md`, `.claude/rules/telegram-bot.md` | documentation |

---

### Task 1: The choice is its subscription, name and kind

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go:224-247` (`RecordWalkedServer`, `sameChoice`)
- Modify: `server/internal/subwatch/keeper.go:281-302` (the doc comment of `replaceActive`)
- Modify: `.claude/rules/watchd.md:313-318`, `.claude/rules/telegram-bot.md` (the `RecordWalkedServer` phrase)
- Test: `server/internal/vpnconfig/vpnconfig_test.go`, `server/internal/vpnconfig/group_test.go`, `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Produces, in package `vpnconfig`:
  - `func SameChoice(a *ActiveServer, s Server) bool`
  - `func SameChoiceRecord(a, b *ActiveServer) bool`

- [ ] **Step 1: Write the failing tests that compile today**

In `server/internal/vpnconfig/vpnconfig_test.go`, insert after `TestRecordWalkedServer_TheSameNameInAnotherSubscriptionIsAMove`:

```go
// A panel lists Oslo over VLESS-REALITY and over Trojan. A walk that moves from
// the user's REALITY Oslo to the Trojan one has left the choice, and the record
// keeps it aside with its kind; back on the REALITY Oslo at a new address, the
// walk is back on the choice.
func TestRecordWalkedServer_ASameNamedServerOfAnotherKindIsAMove(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11")
	oslo.Subscription = "0a1b2c3d"
	cfg := &VPNDirectorConfig{}
	cfg.Xray.ActiveServer = RecordActiveServer(&ActiveServer{Seq: 4}, oslo)

	RecordWalkedServer(cfg, Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443, Outbound: trojanTo("de.example")})

	p := cfg.Xray.PreferredServer
	if p == nil || p.Name != "Oslo" || p.Group != "vless+reality" || p.Seq != 0 {
		t.Fatalf("preferred %+v, want the user's REALITY Oslo", p)
	}
	if a := cfg.Xray.ActiveServer; a.Group != "trojan" || a.Seq != 6 {
		t.Fatalf("active %+v, want the Trojan Oslo one write on", a)
	}

	rotated := oslo
	rotated.Address, rotated.Outbound = "nl.example", realityOutbound("nl.example", "www.example.com", "aa11")
	RecordWalkedServer(cfg, rotated)

	if cfg.Xray.PreferredServer != nil {
		t.Fatalf("preferred %+v, want none: the REALITY Oslo is the choice", cfg.Xray.PreferredServer)
	}
}
```

In `server/internal/subwatch/keeper_test.go`, insert after `TestKeeper_FastEvidenceSwitchesAtOnce`:

```go
// setHealthOf is setHealth for servers a name does not tell apart: the monitor
// holds the endpoints of dead dead and those of alive alive, and the main probe
// last worked on the active server a minute before the monitor saw them die.
func (s *fastFixture) setHealthOf(dead, alive []vpnconfig.Server) {
	cached := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	fresh := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	put := func(servers []vpnconfig.Server, status watchdapi.Status) {
		for _, server := range servers {
			for _, key := range endpoint.Keys(server) {
				state := watchdapi.EndpointState{Status: status, CheckedAt: s.f.now, NextAt: s.f.now.Add(time.Minute), Since: s.f.now}
				if status == watchdapi.StatusAlive {
					state.LatencyMS = 17
				} else {
					state.Fails, state.Error = 1, "timeout"
				}
				fresh.Endpoints[key] = state
				state.CheckedAt = s.f.now.Add(-time.Minute)
				cached.Endpoints[key] = state
			}
		}
	}
	put(dead, watchdapi.StatusDead)
	put(alive, watchdapi.StatusAlive)
	s.h = &fastHealth{cached: cached, fresh: fresh, invalid: make(map[string]bool)}
	s.w.Health = s.h
	s.w.probeOKAt, s.w.probeOKActive = s.f.now.Add(-time.Minute), activeID(s.f.cfg.Xray.ActiveServer)
}

// A panel lists Oslo twice: the user's vless+tls server and a Shadowsocks
// inbound. Oslo dies while the twin is alive: the fast path switches to the
// twin, and the record keeps Oslo, the user's kind, in preferred_server - the
// twin is another server, and the return can bring Xray back.
func TestKeeper_AFastSwitchToASameNamedServerOfAnotherKindKeepsTheChoice(t *testing.T) {
	s := newKeeperFixture(t)
	oslo, twin := s.f.subs[0].Servers[0], ssServer("Oslo", "198.51.100.40")
	s.f.subs[0].Servers = []vpnconfig.Server{oslo, twin}
	s.setHealthOf([]vpnconfig.Server{oslo}, []vpnconfig.Server{twin})
	s.passThenMiss(time.Second)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@198.51.100.40"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Address != "oslo.example" || p.Group != "vless+tls" {
		t.Fatalf("preferred %+v, want the user's vless+tls Oslo", p)
	}
	if a := s.f.cfg.Xray.ActiveServer; a == nil || a.Group != "shadowsocks" {
		t.Fatalf("active %+v, want the twin", a)
	}
	if want := []string{"Xray switched to server Alpha / Oslo"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd server && go test -count=1 -run 'TestRecordWalkedServer_ASameNamedServerOfAnotherKindIsAMove' ./internal/vpnconfig/ && go test -count=1 -run 'TestKeeper_AFastSwitchToASameNamedServerOfAnotherKindKeepsTheChoice' ./internal/subwatch/`

Expected: both FAIL on the `preferred` assertion (`preferred <nil>, want the user's REALITY Oslo` and `preferred <nil>, want the user's vless+tls Oslo`): today a same-named server counts as the choice.

- [ ] **Step 3: Write the tests of the new functions**

Append to `server/internal/vpnconfig/group_test.go`:

```go
// The choice is a server's subscription, name and kind. A panel can give one
// name to several inbounds; a record without a group knows no kind, and its
// subscription and name decide.
func TestSameChoice(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11")
	oslo.Subscription = "0a1b2c3d"
	record := NewActiveServer(oslo)
	rotated := oslo
	rotated.Address, rotated.Outbound = "nl.example", realityOutbound("nl.example", "www.example.com", "aa11")
	trojan := Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443, Outbound: trojanTo("de.example")}
	legacy := Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	other := oslo
	other.Subscription = "1b2c3d4e"
	bergen := oslo
	bergen.Name = "Bergen"
	noGroup := &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	cases := []struct {
		name string
		a    *ActiveServer
		s    Server
		want bool
	}{
		{"the same server", record, oslo, true},
		{"the same kind at a new address", record, rotated, true},
		{"another kind at the same address and port", record, trojan, false},
		{"an entry without an outbound", record, legacy, false},
		{"another subscription", record, other, false},
		{"another name", record, bergen, false},
		{"a record without a group, another kind", noGroup, trojan, true},
		{"no record", nil, oslo, false},
	}
	for _, tc := range cases {
		if got := SameChoice(tc.a, tc.s); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// SameChoiceRecord holds the server another record names to the choice by that
// record's group. The first record is the choice: a second one without a group
// is not of its kind when the choice has one.
func TestSameChoiceRecord(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11")
	oslo.Subscription = "0a1b2c3d"
	choice := NewActiveServer(oslo)
	trojan := NewActiveServer(Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443, Outbound: trojanTo("de.example")})
	rotated := *choice
	rotated.Address = "nl.example"
	noGroup := &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	other := *choice
	other.Subscription = "1b2c3d4e"
	cases := []struct {
		name string
		a, b *ActiveServer
		want bool
	}{
		{"the same kind at a new address", choice, &rotated, true},
		{"another kind", choice, trojan, false},
		{"a record without a group for a choice with one", choice, noGroup, false},
		{"a choice without a group", noGroup, trojan, true},
		{"another subscription", choice, &other, false},
		{"no record", choice, nil, false},
		{"no choice", nil, choice, false},
	}
	for _, tc := range cases {
		if got := SameChoiceRecord(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

- [ ] **Step 4: Run them and watch the build fail**

Run: `cd server && go test -count=1 -run 'TestSameChoice' ./internal/vpnconfig/`

Expected: FAIL, the package does not build: `undefined: SameChoice` and `undefined: SameChoiceRecord`.

- [ ] **Step 5: Implement the rule**

In `server/internal/vpnconfig/vpnconfig.go`, replace `RecordWalkedServer` and `sameChoice` (lines 224-247) with:

```go
// RecordWalkedServer names s as the running server for the subscription walk.
// The walk tries servers nobody chose, and one cut short - the bot restarted, a
// stop - leaves active_server on one of them. So the server the user chose is
// kept in preferred_server from the first record that leaves its choice until
// one comes back to it. A new address under the same name and kind is no move
// away: a subscription that rotates endpoints gives a name one every day. The
// same name in another subscription, or of another kind, is another server
// (SameChoice).
func RecordWalkedServer(cfg *VPNDirectorConfig, s Server) {
	x := &cfg.Xray
	switch {
	case x.PreferredServer != nil && SameChoice(x.PreferredServer, s):
		x.PreferredServer = nil
	case x.PreferredServer == nil && x.ActiveServer != nil && !SameChoice(x.ActiveServer, s):
		chosen := *x.ActiveServer
		chosen.Seq = 0
		x.PreferredServer = &chosen
	}
	x.ActiveServer = RecordActiveServer(x.ActiveServer, s)
}

// SameChoice reports whether s is the server the record a names as a choice:
// its subscription, its name and its kind (Group). A panel can give one name
// to several inbounds - Oslo over VLESS-REALITY and over Shadowsocks - and a
// server of another kind fares otherwise on a censored path, so it is another
// server. A record without a group - configure.sh's, an older daemon's, or one
// whose outbound does not parse - knows no kind, and its subscription and name
// decide.
func SameChoice(a *ActiveServer, s Server) bool {
	return a != nil && a.Subscription == s.Subscription && a.Name == s.Name &&
		(a.Group == "" || s.Group() == a.Group)
}

// SameChoiceRecord is SameChoice for the server another record b names: b's
// group stands for its kind. a is the choice, so a b without a group is not of
// a's kind when a has one.
func SameChoiceRecord(a, b *ActiveServer) bool {
	return a != nil && b != nil && a.Subscription == b.Subscription && a.Name == b.Name &&
		(a.Group == "" || b.Group == a.Group)
}
```

- [ ] **Step 6: Run the tests and watch them pass**

Run: `cd server && go test -count=1 ./internal/vpnconfig/ && go test -count=1 -run 'TestKeeper_AFastSwitchToASameNamedServerOfAnotherKindKeepsTheChoice|TestKeeper_FastEvidenceSwitchesAtOnce' ./internal/subwatch/`

Expected: PASS, the whole `vpnconfig` package included.

- [ ] **Step 7: Update the comment and the docs that stated the old limit**

In `server/internal/subwatch/keeper.go`, replace the whole doc comment of `replaceActive` (lines 281-302, from `// replaceActive switches Xray` to `// caller has asked replacementWaits first: with the WAN down none would work.`) with:

```go
// replaceActive switches Xray to the best working server (spec 4.3) through
// sw, the action's fence. cfg is the record the action started from, and it
// orders the candidates too: a follow that failed recorded the same choice,
// which walkOrder still finds, and the write after it keeps the record it left
// in preferred_server (RecordWalkedServer), as does a write of the fast
// path's, one onto a same-named server of another kind included: that is
// another server (vpnconfig.SameChoice). It leaves out each copy in tried with
// the copies that dial its connection (endpoint.DialKey): spec 4.5 leaves out
// the servers already tried in the attempt. dead - every copy of it, and every
// copy that dials the same connection, which shares its fate - goes last,
// after every other candidate, rather than out: its stored entry can differ
// from what Xray ran in ways the identity mark ignores - a REALITY pick or a
// ws or httpupgrade Host or path a manual refresh took fresh, new IPs of its
// host - so with nothing else working it is still worth one try. It sends msg
// with was, the server replaced, and the server found; a copy of dead that
// works sends msgFollowedChanged instead, since Xray then runs dead's current
// entry. With none working it says so once a run and backs the next attempt
// off. Every caller has asked replacementWaits first: with the WAN down none
// would work.
```

In `.claude/rules/watchd.md`, replace these lines (313-318):

```
`RecordWalkedServer` keeps the user's choice in `preferred_server`, a vanished one included, so its
group stays the reference across a restart. The choice is its subscription and name, as everywhere
the watch matches it. A replacement that lands on another entry of that name - one of the several a
vanished choice can leave behind, or a twin of another group - is taken for the choice itself:
nothing is kept in `preferred_server`, that entry's group becomes the reference, and no return
brings Xray back to the group the user chose. The return runs in both modes and looks every
```

with:

```
`RecordWalkedServer` keeps the user's choice in `preferred_server`, a vanished one included, so its
group stays the reference across a restart. The choice is its subscription, name and kind
(`vpnconfig.SameChoice`), wherever the watch matches it: a panel can give one name to several
inbounds, and a replacement that lands on a same-named entry of another kind has left the choice,
which `preferred_server` keeps. A record without `group` (`configure.sh`'s, an older daemon's)
knows no kind and goes by subscription and name. The return runs in both modes and looks every
```

In `.claude/rules/telegram-bot.md` (one long line; match the text, not a line), replace

```
(`vpnconfig.RecordWalkedServer`, from the first record that leaves its subscription and name until one comes back to them)
```

with

```
(`vpnconfig.RecordWalkedServer`, from the first record that leaves its subscription, name and kind until one comes back to them; a record without `group` goes by subscription and name)
```

- [ ] **Step 8: Run both packages**

Run: `cd server && go test -count=1 ./internal/vpnconfig/ ./internal/subwatch/`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go server/internal/vpnconfig/vpnconfig_test.go server/internal/vpnconfig/group_test.go server/internal/subwatch/keeper.go server/internal/subwatch/keeper_test.go .claude/rules/watchd.md .claude/rules/telegram-bot.md
git commit -m "fix(vpnconfig): the user's choice is its subscription, name and kind"
```

---

### Task 2: The walk, the return and the actuality check find the choice by its kind

**Files:**
- Modify: `server/internal/subwatch/watch.go:764-787` (`chosenIndex`)
- Modify: `server/internal/subwatch/order.go:12-20` (`walkOrder`'s doc comment)
- Modify: `server/internal/subwatch/actuality.go` (the constants' comments, `classifyActive`)
- Modify: `server/internal/subwatch/return.go:85-88` (`maybeReturn`'s check)
- Modify: `.claude/rules/watchd.md:250-255`, `.claude/rules/telegram-bot.md` (the walk's first entry)
- Test: `server/internal/subwatch/watch_test.go`, `order_test.go`, `actuality_test.go`, `keeper_test.go`

**Interfaces:**
- Consumes: `vpnconfig.SameChoice(a *ActiveServer, s Server) bool`, `vpnconfig.SameChoiceRecord(a, b *ActiveServer) bool` (Task 1).
- Produces: no new names; `chosenIndex`, `classifyActive` and `maybeReturn` keep their signatures.

- [ ] **Step 1: Write the failing tests**

In `server/internal/subwatch/watch_test.go`, insert after `TestChosenIndex_TheNameFallbackStaysInItsSubscription`:

```go
// A panel can list a Shadowsocks inbound under the user's server's name, even
// at its address and port. It is never the choice of a vless+tls record: the
// entry of the record's kind is, wherever it is now. A record without a group
// knows no kind and keeps the name rule.
func TestChosenIndex_AnEntryOfAnotherKindIsNotTheChoice(t *testing.T) {
	twin := ssServer("Oslo", "198.51.100.40")
	twin.Address, twin.Port = "oslo.example", 443
	rotated := fastServer("Oslo", "oslo-2.example", "203.0.113.20")
	servers := []vpnconfig.Server{twin, rotated}
	record := vpnconfig.RecordActiveServer(nil, fastServer("Oslo", "oslo.example", "203.0.113.10"))

	if i := chosenIndex(servers, record); i != 1 {
		t.Fatalf("chosenIndex %d, want 1: the vless+tls Oslo", i)
	}
	legacy := &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}
	if i := chosenIndex(servers, legacy); i != 0 {
		t.Fatalf("chosenIndex %d for a record without a group, want 0: its name, address and port", i)
	}
}
```

In `server/internal/subwatch/order_test.go`, insert after `TestWalkOrder_TheSameNameInAnotherSubscriptionIsNotTheChoice`:

```go
// Oslo's entry at the record's address is gone, and the list names Oslo twice:
// a Shadowsocks twin first, then the user's kind at a new address. The walk
// starts from the user's kind, not from the first entry of the name.
func TestWalkOrder_TheChoicesKindFirstAmongEntriesOfItsName(t *testing.T) {
	twin := ssServer("Oslo", "198.51.100.40")
	rotated := fastServer("Oslo", "oslo-2.example", "203.0.113.20")
	backup := fastServer("Backup", "backup.example", "198.51.100.20")
	subs := []vpnconfig.Subscription{{ID: "alpha", Name: "Alpha", Servers: []vpnconfig.Server{twin, backup, rotated}}}
	chosen := vpnconfig.RecordActiveServer(nil, fastServer("Oslo", "oslo.example", "203.0.113.10"))

	order, first := walkOrder(subs, chosen)

	if !first || order[0].Address != "oslo-2.example" {
		t.Fatalf("first %v, order %v; want the vless+tls Oslo first", first, healthLabels(order))
	}
}
```

In `server/internal/subwatch/actuality_test.go`, add after the `beta` lines (`beta.ID, beta.Name, beta.URL = ...`):

```go
	// A Shadowsocks inbound of the provider under the same name: another kind.
	ss := ssServer("Oslo", "198.51.100.40")
	// The same inbound at the record's own address and port.
	ssHere := ss
	ssHere.Address, ssHere.Port = "oslo.example", 443
```

and add these rows to `cases`, after `{"moved: another port at the address", ...}`:

```go
		{"current: another kind at the record's address and port beside it", alpha(ssHere, oslo), record, actualCurrent, &oslo},
		{"moved: the one entry of the name of the record's kind", alpha(ss, moved), record, actualMoved, &moved},
		{"gone: only another kind left under the name", alpha(bergen, ss), record, actualGone, nil},
		{"gone: another kind at the record's address and port", alpha(ssHere), record, actualGone, nil},
		{"a record without a group follows an entry of any kind", alpha(bergen, ss), &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}, actualMoved, &ss},
```

In `server/internal/subwatch/keeper_test.go`, insert after `TestMaybeReturn_TwoMinutesAliveIsNotEnough`:

```go
// Xray runs Oslo's Shadowsocks twin, a server of the same name and another
// kind, while Oslo, the user's vless+tls choice, waits in preferred_server. The
// twin is not the preferred server: once Oslo has been alive five minutes, Xray
// goes back to it.
func TestMaybeReturn_ASameNamedServerOfAnotherKindIsNotThePreferredServer(t *testing.T) {
	s := newReturnKeeper(t)
	oslo, twin := s.f.subs[0].Servers[0], ssServer("Oslo", "198.51.100.40")
	s.f.subs[0].Servers = []vpnconfig.Server{oslo, twin}
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, twin)
	s.setStates(map[string]watchdapi.EndpointState{"Oslo": aliveSince(s.f.now.Add(-6*time.Minute), 40)})
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo.example"}) || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("written %v, preferred %+v", got, s.f.cfg.Xray.PreferredServer)
	}
	if want := []string{"Xray back on the preferred server Alpha / Oslo"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd server && go test -count=1 -run 'TestChosenIndex_AnEntryOfAnotherKindIsNotTheChoice|TestWalkOrder_TheChoicesKindFirstAmongEntriesOfItsName|TestClassifyActive|TestMaybeReturn_ASameNamedServerOfAnotherKindIsNotThePreferredServer' -v ./internal/subwatch/`

Expected: FAIL:
- `chosenIndex 0, want 1: the vless+tls Oslo`;
- `first true, order [Oslo@198.51.100.40 ...]; want the vless+tls Oslo first`;
- `TestClassifyActive` on three rows: "moved: the one entry of the name of the record's kind" (got gone), "gone: only another kind left under the name" (got moved), "gone: another kind at the record's address and port" (got changed). The rows "current: another kind ... beside it" and "a record without a group ..." pass already: they pin present behaviour;
- `written [], preferred &{...}`: `maybeReturn` takes the twin for the preferred server.

- [ ] **Step 3: Find the choice by its kind**

In `server/internal/subwatch/watch.go`, replace `chosenIndex` and its doc comment (lines 764-787) with:

```go
// chosenIndex is where servers has the server a names: the entry of its
// choice (vpnconfig.SameChoice: its subscription, name and kind) at its address
// and port, or else the first entry of its choice - a subscription that rotates
// endpoints gives a name a new address every day, and the name and the kind are
// what the user chose. Another subscription's server of the same name is never
// it, nor a server of another kind: a panel can give one name to several
// inbounds. -1 when the list has neither.
func chosenIndex(servers []vpnconfig.Server, a *vpnconfig.ActiveServer) int {
	if a == nil || a.Name == "" {
		return -1
	}
	first := -1
	for i, s := range servers {
		if !vpnconfig.SameChoice(a, s) {
			continue
		}
		if sameServer(s, a) {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}
```

In `server/internal/subwatch/order.go`, replace `walkOrder`'s doc comment (lines 12-20) with:

```go
// walkOrder is the order a walk tries the servers of subs in (spec 5.3): the
// chosen server - found by chosenIndex, by its subscription, name and kind -
// then the next servers of its subscription in list order, OwnFirst of them
// in all; then one server of each subscription in turn, in subscription order,
// starting with the one after the chosen server's; the chosen subscription's
// remaining servers take their turns too. Without the chosen server's
// subscription the turns start at the first. With one subscription this is
// pickOrder. chosenFirst reports that the chosen server leads the order. Every
// server carries its subscription's id.
```

- [ ] **Step 4: Classify against the entries of the choice**

In `server/internal/subwatch/actuality.go`, replace the comments of `actualCurrent`, `actualChanged`, `actualMoved` and `actualGone` with:

```go
	// actualCurrent is an entry of the record's choice - its name and kind -
	// at its address and port with the record's mark, or any such entry when
	// the record has no mark; for a record without an address and a port, any
	// entry of its choice.
	actualCurrent
	// actualChanged is entries of the record's choice at its address and port,
	// all with another mark: new keys or a new UUID behind the same name and
	// address.
	actualChanged
	// actualMoved is no such entry, and exactly one entry of the choice
	// elsewhere.
	actualMoved
	// actualGone is neither: the subscription deleted, no entry of the choice,
	// or several - the name and the kind no longer tell which server the user
	// chose.
	actualGone
```

Replace the doc comment of `classifyActive` and the head of its loop. The new doc comment:

```go
// classifyActive compares a with its subscription's list in subs, and returns
// the entry a is to follow when the server changed in place or moved. Only an
// entry of a's choice counts (vpnconfig.SameChoice: its subscription, name and
// kind): a panel can give one name to several inbounds, and one of another
// kind is another server, even at a's address and port. Several entries of the
// choice can share an address and port - a panel's inbounds behind one name -
// and the Web UI selects by index, so a can name any of them: the server
// changed only when none of them carries a's mark, and the first is then
// followed. A record without a mark - configure.sh's, an older daemon's - is
// current while an entry of its choice at its address and port is listed; one
// without an address and a port while an entry of its choice is. A record
// without a group knows no kind and goes by subscription and name.
```

The loop's head, from

```go
	for _, s := range subs[i].Servers {
		if s.Name != a.Name {
			continue
		}
		s.Subscription = subs[i].ID
```

to

```go
	for _, s := range subs[i].Servers {
		s.Subscription = subs[i].ID
		if !vpnconfig.SameChoice(a, s) {
			continue
		}
```

The rest of the function stays as it is.

- [ ] **Step 5: Hold the return's look to the kind**

In `server/internal/subwatch/return.go`, replace

```go
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	if active == nil || (active.Subscription == preferred.Subscription && active.Name == preferred.Name) {
		return
	}
```

with

```go
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	// A same-named server of another kind is not the preferred server
	// (vpnconfig.SameChoice): the return goes on looking.
	if active == nil || vpnconfig.SameChoiceRecord(preferred, active) {
		return
	}
```

- [ ] **Step 6: Run the tests and the package**

Run: `cd server && go test -count=1 -run 'TestChosenIndex_AnEntryOfAnotherKindIsNotTheChoice|TestWalkOrder_TheChoicesKindFirstAmongEntriesOfItsName|TestClassifyActive|TestMaybeReturn_ASameNamedServerOfAnotherKindIsNotThePreferredServer' -v ./internal/subwatch/ && go test -count=1 ./internal/subwatch/ ./internal/vpnconfig/`

Expected: PASS, `TestKeeper_FollowsAMovedServer` included (Review Focus 4). If an existing test fails because its fixture pairs a record that carries a group with list entries that have no outbound, production cannot produce that pairing: stop, and report the test name to the controller instead of weakening the rule.

- [ ] **Step 7: Update the docs**

In `.claude/rules/watchd.md`, replace these lines (250-255):

```
The record is current while an entry of its subscription with its name, address and port carries its
mark - a panel can list twins of one name, address and port, and the user may run any of them - or,
for a record without a mark (`configure.sh`'s, an older daemon's), while such an entry is listed. It
changed in place when such entries are listed but none carries the mark, and the first of them is
followed; it moved when there is none and exactly one entry has its name; anything else - the
subscription deleted, no entry of the name, several - means it left its list. A changed or moved
```

with:

```
Only an entry of the record's choice counts - its subscription, name and kind
(`vpnconfig.SameChoice`): a panel can give one name to several inbounds, and an entry of another kind
is another server, even at the record's address and port. The record is current while an entry of its
choice at its address and port carries its mark - a panel can list twins of one name, address and
port, and the user may run any of them - or, for a record without a mark (`configure.sh`'s, an older
daemon's), while such an entry is listed. It changed in place when such entries are listed but none
carries the mark, and the first of them is followed; it moved when there is none and exactly one
entry of its choice is listed elsewhere; anything else - the subscription deleted, no entry of its
choice, several - means it left its list. A record without `group` knows no kind and goes by
subscription and name. A changed or moved
```

In `.claude/rules/telegram-bot.md` (one long line; match the text, not a line), replace

```
its subscription's entry with its name, address and port, or else that subscription's first entry with its name, since a subscription that rotates endpoints gives a name a new address every day; the same name in another subscription is never it
```

with

```
its subscription's entry with its name and kind at its address and port, or else that subscription's first entry with its name and kind, since a subscription that rotates endpoints gives a name a new address every day; the same name in another subscription is never it, nor an entry of another kind (`vpnconfig.SameChoice`; a record without `group` goes by name)
```

- [ ] **Step 8: Commit**

```bash
git add server/internal/subwatch/watch.go server/internal/subwatch/order.go server/internal/subwatch/actuality.go server/internal/subwatch/return.go server/internal/subwatch/watch_test.go server/internal/subwatch/order_test.go server/internal/subwatch/actuality_test.go server/internal/subwatch/keeper_test.go .claude/rules/watchd.md .claude/rules/telegram-bot.md
git commit -m "fix(subwatch): the walk, the return and the actuality check find the choice by its kind"
```

---

### Task 3: A replacement message tells two servers that read alike apart by kind

**Files:**
- Modify: `server/internal/subwatch/keeper.go` (`followActive`'s two `replaceActive` calls, `replaceDead`'s call, `replaceActive`'s signature, comment and message, the new `withKind`)
- Modify: `.claude/rules/watchd.md` (the paragraph that begins `Each of these switches sends one message`; Task 2 moves it down four lines)
- Test: `server/internal/subwatch/keeper_test.go`

**Interfaces:**
- Consumes: Task 1's rule (the keeper's write keeps `preferred_server`), Task 2's `classifyActive` (the twin alone under the name is gone).
- Produces:
  - `func (w *Watch) replaceActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, sw *switcher, dead *vpnconfig.Server, tried []vpnconfig.Server, msg string, replaced *vpnconfig.ActiveServer)`: the last parameter is the record whose server is replaced, in place of the label `was`;
  - `func withKind(l, group string) string`.

- [ ] **Step 1: Write the failing test**

In `server/internal/subwatch/keeper_test.go`, insert after `TestKeeper_TheDeadServerGoesLast`:

```go
// Oslo, the user's vless+tls choice, is replaced by its Shadowsocks twin of the
// same name - after it left the list, or once its death is confirmed. The
// keeper keeps Oslo in preferred_server, and the message tells the two apart by
// their kinds: "Alpha / Oslo" twice says nothing.
func TestKeeper_AReplacementOntoASameNamedServerOfAnotherKindNamesBothKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		left bool // Oslo's entry left the list; otherwise it is listed and dead
		note string
	}{
		{"left its subscription", true, "Xray server Alpha / Oslo (vless+tls) left its subscription; Xray switched to Alpha / Oslo (shadowsocks)"},
		{"not answering", false, "Xray server Alpha / Oslo (vless+tls) is not answering; Xray switched to Alpha / Oslo (shadowsocks)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newKeeperFixture(t)
			s.w.Health = nil
			oslo, twin := s.f.subs[0].Servers[0], ssServer("Oslo", "198.51.100.40")
			if tc.left {
				s.f.subs[0].Servers = []vpnconfig.Server{twin}
				s.missOnce(t)
				s.w.Tick(context.Background())
			} else {
				s.f.subs[0].Servers = []vpnconfig.Server{oslo, twin}
				tickUntilDead(s.w, s.f)
			}
			if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@198.51.100.40"}) {
				t.Fatalf("written %v", got)
			}
			if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Address != "oslo.example" || p.Group != "vless+tls" {
				t.Fatalf("preferred %+v, want the user's vless+tls Oslo", p)
			}
			if want := []string{tc.note}; !reflect.DeepEqual(s.f.notes, want) {
				t.Fatalf("notes %v, want %v", s.f.notes, want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd server && go test -count=1 -run 'TestKeeper_AReplacementOntoASameNamedServerOfAnotherKindNamesBothKinds' -v ./internal/subwatch/`

Expected: both subtests FAIL on the notes: `notes [Xray server Alpha / Oslo left its subscription; Xray switched to Alpha / Oslo]` and `notes [Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Oslo]`. The `written` and `preferred` assertions already pass (Tasks 1 and 2).

- [ ] **Step 3: Pass the replaced record and name the kinds**

In `server/internal/subwatch/keeper.go`:

1. In `followActive`, change `w.replaceActive(ctx, cfg, subs, sw, nil, nil, msgReplacedGone, was)` to `w.replaceActive(ctx, cfg, subs, sw, nil, nil, msgReplacedGone, a)`, and `w.replaceActive(ctx, cfg, subs, sw, &entry, followed, msgReplacedDead, was)` to `w.replaceActive(ctx, cfg, subs, sw, &entry, followed, msgReplacedDead, a)`. `followActive` keeps its own `was` for its logs and the follow's message.
2. In `replaceDead`, change `w.replaceActive(ctx, cfg, subs, w.newSwitcher(cfg, subs), dead, tried, msgReplacedDead, was)` to `w.replaceActive(ctx, cfg, subs, w.newSwitcher(cfg, subs), dead, tried, msgReplacedDead, died)`. `replaceDead` keeps its `was` for its log.
3. Replace `replaceActive`'s doc comment, as Task 1 left it, with:

```go
// replaceActive switches Xray to the best working server (spec 4.3) through
// sw, the action's fence. cfg is the record the action started from, and it
// orders the candidates too: a follow that failed recorded the same choice,
// which walkOrder still finds, and the write after it keeps the record it left
// in preferred_server (RecordWalkedServer), as does a write of the fast
// path's, one onto a same-named server of another kind included: that is
// another server (vpnconfig.SameChoice). It leaves out each copy in tried with
// the copies that dial its connection (endpoint.DialKey): spec 4.5 leaves out
// the servers already tried in the attempt. dead - every copy of it, and every
// copy that dials the same connection, which shares its fate - goes last,
// after every other candidate, rather than out: its stored entry can differ
// from what Xray ran in ways the identity mark ignores - a REALITY pick or a
// ws or httpupgrade Host or path a manual refresh took fresh, new IPs of its
// host - so with nothing else working it is still worth one try. It sends msg
// with the server replaced - replaced, the record the action decided on - and
// the server found; when the two read alike, a same-named server of another
// kind, each carries its kind. A copy of dead that works sends
// msgFollowedChanged instead, since Xray then runs dead's current entry. With
// none working it says so once a run and backs the next attempt off. Every
// caller has asked replacementWaits first: with the WAN down none would work.
```

4. Change the signature line to:

```go
func (w *Watch) replaceActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, sw *switcher, dead *vpnconfig.Server, tried []vpnconfig.Server, msg string, replaced *vpnconfig.ActiveServer) {
```

5. Replace

```go
	names := subscriptionNames(subs)
	ref := refFor(cfg, vpnconfig.AllServers(subs))
	switched, ended := w.switchAmong(ctx, sw, append(w.bestWalkOrder(copies, cfg, ref), w.bestWalkOrder(last, cfg, ref)...), func(c vpnconfig.Server) string {
		// Xray runs the dead server's current entry now, which changed in a
		// way the identity mark ignores: it is followed, not replaced.
		if dead != nil && serverID(c) == serverID(*dead) {
			return fmt.Sprintf(msgFollowedChanged, was)
		}
		return fmt.Sprintf(msg, was, label(names, c.Subscription, c.Name))
	})
```

with

```go
	names := subscriptionNames(subs)
	was := label(names, replaced.Subscription, replaced.Name)
	ref := refFor(cfg, vpnconfig.AllServers(subs))
	switched, ended := w.switchAmong(ctx, sw, append(w.bestWalkOrder(copies, cfg, ref), w.bestWalkOrder(last, cfg, ref)...), func(c vpnconfig.Server) string {
		// Xray runs the dead server's current entry now, which changed in a
		// way the identity mark ignores: it is followed, not replaced.
		if dead != nil && serverID(c) == serverID(*dead) {
			return fmt.Sprintf(msgFollowedChanged, was)
		}
		from, to := was, label(names, c.Subscription, c.Name)
		// A same-named server of another kind reads like the one it replaces:
		// their kinds tell the two apart.
		if from == to {
			from, to = withKind(from, replaced.Group), withKind(to, c.Group())
		}
		return fmt.Sprintf(msg, from, to)
	})
```

The rest of `replaceActive` (`stays := was` among it) stays as it is.

6. Add after `replaceActive`:

```go
// withKind is the label l with the kind group in parentheses, l alone when the
// kind is not known.
func withKind(l, group string) string {
	if group == "" {
		return l
	}
	return l + " (" + group + ")"
}
```

- [ ] **Step 4: Run the test and the package**

Run: `cd server && go test -count=1 -run 'TestKeeper_AReplacementOntoASameNamedServerOfAnotherKindNamesBothKinds' -v ./internal/subwatch/ && go test -count=1 ./internal/subwatch/`

Expected: PASS. The other replacement tests keep their messages, whose two labels differ (`TestKeeper_ReplacesADeadServer`, `TestKeeper_ReplacesAServerThatLeftItsSubscription`, `TestKeeper_TheDeadServerGoesLast`).

- [ ] **Step 5: Update the docs**

In `.claude/rules/watchd.md`, replace this paragraph (find it by its text):

```
Each of these switches sends one message, past `notify`'s one-per-kind deduplication:
`Xray server <s> moved; Xray follows it`, `… changed; Xray follows it`,
`… left its subscription; Xray switched to <s>`, `… is not answering; Xray switched to <s>`,
`Xray back on a <group> server <s>`, `<s>` being `<subscription> / <server>`; the failover, the
fast path and the return to the preferred server keep their messages. No line names a link.
```

with:

```
Each of these switches sends one message, past `notify`'s one-per-kind deduplication:
`Xray server <s> moved; Xray follows it`, `… changed; Xray follows it`,
`… left its subscription; Xray switched to <s>`, `… is not answering; Xray switched to <s>`,
`Xray back on a <group> server <s>`, `<s>` being `<subscription> / <server>`. When the two
servers of a replacement read alike - a same-named server of another kind - each carries its kind:
`Xray server Alpha / Oslo (vless+reality) left its subscription; Xray switched to Alpha / Oslo
(shadowsocks)`. The failover, the fast path and the return to the preferred server keep their
messages. No line names a link.
```

- [ ] **Step 6: Commit**

```bash
git add server/internal/subwatch/keeper.go server/internal/subwatch/keeper_test.go .claude/rules/watchd.md
git commit -m "feat(subwatch): a replacement message tells two servers that read alike apart by kind"
```

---

### Task 4: Whole-branch verification

No commits unless a check fails and the fix is approved.

- [ ] **Step 1: Format and vet**

Run: `cd server && gofmt -l . && go vet ./...`

Expected: no output.

- [ ] **Step 2: The full test suite**

Run: `make -C server test`

Expected: every package `ok` (31 of 31), no `--- FAIL:`.

- [ ] **Step 3: The race detector on the concurrent packages**

Run: `cd server && go test -race -count=1 ./internal/subwatch/... ./internal/vpnconfig/... ./internal/service/... ./cmd/watchd/...`

Expected: every package `ok`, no `DATA RACE`.

- [ ] **Step 4: Cross-compile for the routers**

Run: `cd server && GOOS=linux GOARCH=arm64 go build ./... && GOOS=linux GOARCH=arm GOARM=7 go build ./... && GOOS=linux GOARCH=mipsle go build ./...`

Expected: no output.

- [ ] **Step 5: No stale statement of the old rule**

Run: `grep -rn "subscription and name" .claude/rules/watchd.md .claude/rules/telegram-bot.md server/internal/subwatch/*.go server/internal/vpnconfig/*.go | grep -v _test.go`

Expected: each hit either describes a record without `group` (the name rule's own case) or is unrelated to the choice. A hit that states the choice is its subscription and name alone is stale: report it.

- [ ] **Step 6: The working tree**

Run: `git status --short | grep -v '^??'`

Expected: ` M .claude/settings.local.json` alone: nothing of this plan is left unstaged or uncommitted.
