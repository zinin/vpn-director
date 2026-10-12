# The user's choice by kind: design

Date: 2026-10-12
Branch: `feature/keep-xray-working`
Status: approved in brainstorming, awaiting implementation plan
Builds on: `docs/superpowers/specs/2026-10-12-keep-xray-working-design.md`

## 1. Goal

The watch must keep the server the user chose, and its kind, through replacements and returns. Today it
knows the choice by subscription and name, and a panel can give one name to several entries: one per
inbound, so "Oslo" can be VLESS-REALITY and Shadowsocks alike. The choice becomes subscription, name and
kind: the protocol and security of the outbound, `Server.Group`.

## 2. Where it stands

Four places match the choice by subscription and name:

- `vpnconfig.sameChoice`, which `RecordWalkedServer` uses. A write of the walk, the keeper, the fast path or
  the return either leaves the choice, and `preferred_server` keeps it, or comes back to it, and
  `preferred_server` is cleared.
- `subwatch.chosenIndex` finds the choice in a list: the entry with its name, address and port, or else the
  first entry with its name. The walk's first candidate, the return's target, the TCP look of the
  unreachable rule and the return's way back rest on it.
- `maybeReturn` does nothing while `active_server` has the preferred server's subscription and name.
- `classifyActive` calls the selected server moved when exactly one entry has its name, of any kind.

So a replacement that lands on a same-named entry of another kind counts as the choice itself:
`preferred_server` is not kept, that entry's kind becomes the reference (`refFor`), and no return brings
Xray back to the kind the user chose. The return can also take a same-named entry of another kind for the
preferred server, and a follow can carry the choice to another kind. The herdr review found this (issue 3);
commit `999a5e2` documented it and left the behaviour as it was.

## 3. Decisions

| Question | Decision |
|---|---|
| What the choice is | Its subscription, name and kind. |
| A record without `group` (`configure.sh`, older daemons, an outbound that does not parse) | Subscription and name, as now. |
| A new address or new keys, the same kind | The same choice: followed, and a write onto it comes back to the choice. |
| An entry of another kind under the name, even at the record's address and port | Another server, never the choice. It stays a candidate. |
| A replacement onto a same-named entry of another kind | A move away: `preferred_server` keeps the choice and its kind. |
| Two server labels in one message that read alike | Each carries its kind. |

The owner chose this strict rule over two others: a kind change at the same address and port that still
counts as the choice, and a follow that carries the choice to any kind, which needs a write path of its own.

## 4. Design

### 4.1 The rule

`vpnconfig.SameChoice(a *ActiveServer, s Server) bool` replaces `sameChoice`. An entry is the choice a
record names when both hold:

- its subscription and name are the record's;
- the record has no `group`, or the entry's `Group()` is the record's `group`.

`vpnconfig.SameChoiceRecord(a, b *ActiveServer) bool` applies the rule to a record `b`, with `b.Group` as
the entry's kind. `a` is the choice, so a `b` without `group` is not of `a`'s kind when `a` has one.

`RecordWalkedServer` uses `SameChoice` where it uses `sameChoice` now.

### 4.2 Finding the choice

`chosenIndex` returns the first entry that is the choice (`SameChoice`) at the record's address and port,
else the first entry that is the choice anywhere in the list, else -1. For a record without `group` this is
the present rule.

### 4.3 The return's look

`maybeReturn` does nothing while `active_server` is the preferred server by `SameChoiceRecord`. A same-named
entry of another kind no longer passes for it, and the return looks for the preferred server, or for a
server of its kind (section 4.6 of the keep-xray-working design).

### 4.4 Is the selected server current?

`classifyActive` counts only the entries that are the choice (`SameChoice`):

| Case | Condition | Action |
|---|---|---|
| Current | Such an entry at the record's address and port carries the record's `identity`, or the record has none | None |
| Changed in place | Such entries at the record's address and port, none with the record's `identity` | Follow the first |
| Moved | None at the record's address and port, exactly one elsewhere | Follow it |
| Gone | Anything else: the subscription deleted, no entry of the name and kind, or several elsewhere | Replace |

A record without an address and a port is current while an entry that is the choice is listed. For a
record without `group` the cases are the present ones.

### 4.5 Messages

The keeper's replacement messages name two servers:

- `Xray server %s left its subscription; Xray switched to %s`
- `Xray server %s is not answering; Xray switched to %s`

When the two labels read alike, each gets its kind in parentheses where the kind is known:
`Xray server Alpha / Oslo (vless+reality) left its subscription; Xray switched to Alpha / Oslo (shadowsocks)`.
The replaced server's kind comes from its record (`group`), the new server's from its entry. Every other
message stays as it is: it names one server, or its text names the kind (`Xray back on a %s server %s`).

### 4.6 What stays

- `refFor`: the reference group comes from the record, and for a record without `group` from
  `chosenIndex`, which then matches by name, as now.
- `exactFastServer`: the entry at the record's subscription, name, address and port, for the fast path's
  evidence and the replacement's last try of the dead server. It names an entry, not the choice.
- `followRenames` and the periodic refresh: a rename keeps the kind.
- The Web UI's Active badge and `/xray`'s ✓, which match name, address and port; `pending_restore`; the
  deletion of a subscription; `configure.sh`.
- The JSON of `active_server` and `preferred_server`, the config keys, the watchd socket and every shell
  script.

## 5. What changes for the user

- A replacement - keeper, walk or fast path - that lands on a same-named server of another kind keeps the
  user's choice in `preferred_server`. Its kind stays the reference: the return to the preferred server and
  the return to its kind work.
- The return takes no same-named server of another kind for the preferred server.
- A selected server whose kind changed, at a new address or at the same one, has left its list, unless
  exactly one other entry of its name and kind is listed, which is then followed:
  - in keeper mode the best server of the user's kind replaces it, and it stays a candidate;
  - with clients on Xray the death rules move the clients into a tunnel, and the walk picks the server.
- A new address or new keys of the same kind: followed, as now.

## 6. Compatibility

A record written without `group` matches by subscription and name until a Go writer records it again;
every Go writer fills `group` (`NewActiveServer`). The JSON does not change, so an older daemon reads the
records as before.

## 7. Known limits

- A choice the old code already lost - `preferred_server` cleared, `active_server` on a same-named entry of
  another kind - stays lost: that entry's kind is the reference now. A selection in the Web UI, `/xray` or
  the wizard sets the choice again.
- Entries of one subscription with the same name and kind are one choice, as rotating endpoints are. A
  message that names two of them reads alike.
- A provider that changes the kind of the selected server makes it leave its list. A selection adopts the
  new kind.
- A record without `group` (`configure.sh`) matches by name until a Go writer records it.

## 8. Testing

Go, test first:

- `vpnconfig`:
  - `SameChoice`: the same kind, another kind, a record without `group`, another subscription, another
    name, an entry without an outbound;
  - `SameChoiceRecord`: the same pairs for two records;
  - `RecordWalkedServer`: a write onto a same-named entry of another kind keeps the choice in
    `preferred_server`; a write onto the choice's kind at a new address clears it.
- `subwatch`:
  - `chosenIndex`: an entry of another kind at the record's address and port is passed over for the first
    entry of the name and kind; a record without `group` matches by name;
  - `classifyActive`: another kind alone under the name is gone; several entries of the name with one of
    the record's kind is moved to it; another kind at the record's address and port is gone; a record
    without `group` keeps the present cases;
  - the keeper: the selected server dies, and only a same-named server of another kind is alive. The keeper
    switches to it, `preferred_server` keeps the choice, and the message names both kinds. Once the chosen
    server has been alive for `ReturnAlive`, the return goes back to it and clears `preferred_server`;
  - `maybeReturn` looks for the preferred server while a same-named server of another kind runs;
  - the walk's order puts the entry of the choice's kind first, though a same-named entry of another kind
    comes first in the list;
  - existing tests that rest on matching by name are updated.
- `gofmt`, `go vet`, `make -C server test`, and `-race` on `subwatch` and `vpnconfig`.

No Bats test: the shell changes nothing.

## 9. Documentation

- `.claude/rules/watchd.md`, "Keeping Xray on a working server": the choice by subscription, name and kind,
  in place of the limit `999a5e2` stated; the classification by kind; the kinds in labels that read alike.
- `.claude/rules/telegram-bot.md`: the entry the walk tries first is of the choice's kind;
  `RecordWalkedServer` keeps the choice from the first record that leaves its subscription, name and kind.
- Comments: `SameChoice`, `SameChoiceRecord`, `RecordWalkedServer`, `chosenIndex`, `walkOrder`,
  `classifyActive`, `replaceActive`, `maybeReturn`.
- `README.md` and `README.ru.md` need no change.
