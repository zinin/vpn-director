# Multiple subscriptions: design

Date: 2026-09-24
Branch: `feature/multi-subscriptions`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

VPN Director keeps one subscription. `xray.subscription_url` holds one link,
every import replaces `servers.json` whole, and the subscription watch refreshes
that one link. The author now pays two providers — one serves Xray JSON with
about 32 usable servers, the other a base64 list of 62 `vless://` links — and
wants both on the router at once, with room for more.

This change keeps up to ten subscriptions:

- each one is added, refreshed, renamed and deleted on its own, from the Web UI,
  the Telegram bot and the shell;
- the user picks the running server from any of them;
- when the running server dies, the watch looks for a live one across all of
  them, and a provider that is down as a whole costs a minute of attempts, not
  its whole list.

Xray still runs one server for every Xray client. The probe, the death rules,
the Tunnel Director failover and restore, and the return to the preferred
server keep working as they do; this change teaches them about subscriptions.

## 2. Decisions

| Question | Decision |
|---|---|
| What "several at once" means | One running server for all Xray clients, chosen from any subscription; the watch switches between subscriptions when it dies. |
| Storage | One file per subscription: `<data_dir>/subscriptions/<id>.json` holds its link, its status and its servers. |
| Migration | None. Nothing reads the old link or `servers.json`; subscriptions are added again after the update. |
| Walk order | Hybrid: the chosen server and two more of its subscription, then one server of each subscription in turn. |
| Management | Full parity: the Web UI, the bot (inline buttons) and the shell (a menu) all add, refresh, rename and delete. |
| Background health checks | Not in this change: the next design. |

Rejected:

- **A subscription tag on each record of a flat `servers.json`.** Every current
  reader of the array would keep working, but a subscription would live in two
  places — its link in the config, its servers in the shared list — and every
  import would rewrite the shared file. The compatibility bought nothing: the
  owner is the only user and waived the migration.
- **`servers.json` as an object grouped by subscription.** One self-describing
  file, but a new format under the old name breaks every reader of the array at
  once.
- **A server per LAN client or group.** Several outbounds at once and routing by
  source address change the whole model of the watch. Deferred.
- **An Xray balancer with an observatory.** Xray would switch in seconds, but
  here the user chooses the running server, and the watch already walks. The
  subscription formats design declined to carry balancers into `config.json`
  for the same reason: the watch does what they do.
- **Walking the chosen subscription to its end first.** A provider that is down
  as a whole — expired, blocked — would cost 8 to 15 minutes of dead attempts
  before the first server of another provider.
- **Strict round-robin from the start.** It finds a live provider fastest, but
  leaves the user's provider when a single server fails.

## 3. Storage

### 3.1 The subscription file

```json
{
  "id": "a3f9c2d1",
  "name": "Alpha",
  "url": "https://sub.example.com/s/…",
  "added": "2026-09-24T18:00:00Z",
  "refreshed": "2026-09-24T18:05:00Z",
  "error": "",
  "servers": [ … ]
}
```

- **Path:** `<data_dir>/subscriptions/<id>.json`, mode 0600: the file holds the
  link, whose path carries the subscription token, and every server's
  credentials. The first write creates the directory.
- **`id`:** 8 lowercase hex digits, random, never changed. A file named anything
  but `<8 hex digits>.json` is ignored — the temp files of an atomic write look
  like neither — and so is a file whose `id` differs from its name, with a WARN.
- **`name`:** 1 to 32 characters (code points) once leading and trailing spaces
  are trimmed, none of them a control character (C0, DEL, C1), unique under
  ASCII case folding: jq (`ascii_downcase`) and Go can apply that fold alike.
  The default is the link's host name, or the file's base name for a list read
  from a file; a default that is taken becomes `host-2`, `host-3` and so on,
  and a default is cut so that it fits 32 characters, suffix included. A name
  the user gives that is taken is refused.
- **`url`:** an `https` link, or absent. A subscription without one is a
  **static list**: the shell imported it from a file or a plain-http link, and
  nothing refreshes it.
- **`added`:** RFC 3339 UTC, when the subscription was created. Subscriptions
  are ordered by `added`, then by `id`; every list and the walk's rotation
  follow that order.
- **`refreshed`:** when `servers` was last replaced; for a static list, the
  import time.
- **`error`:** why the last refresh failed; absent or empty after a success.
- **`servers`:** the records `servers.json` holds today (`name`, `address`,
  `port`, `ips`, `outbound`).

At most ten subscriptions exist: the watch downloads them in parallel, and the
first keyboard of the bot lists them all.

### 3.2 Identity

A server is identified by its subscription, name, address and port.
`xray.active_server` and `xray.preferred_server` gain `subscription`, the id.
In memory `vpnconfig.Server` carries the id of the file it came from; the
records in the file do not repeat it.

The fallback to the name alone — a subscription that rotates endpoints gives a
name a new address every day — stays inside the subscription: a server of the
same name in another subscription never matches. A record without
`subscription` matches no server.

### 3.3 The bypass list and the lock

`xray.servers` (TPROXY_BYPASS) is the union of the `ips` of every file. Every
writer to `subscriptions/` recomputes it in the same locked update.

Every write to `subscriptions/` — from either daemon or the shell — happens
under the config lock (`.vpn-director.json.lock`) and replaces the file with an
atomic rename. Readers take no lock.

### 3.4 What goes away

- `xray.subscription_url`. The Go field goes, so the next config write of a
  daemon drops the key, and `/api/config` no longer serializes it. The shell
  deletes the key whenever it writes a subscription.
- `servers.json`. Nothing reads it, and every write of a subscription deletes it.
- Nothing is migrated. After the update the watch stays unarmed until a
  subscription is added, the running Xray keeps its config, and the old
  `active_server`, which has no `subscription`, marks no server until the next
  selection.

## 4. Operations

The Web UI and the bot call one Go service: the store — reading, writing, the
union of addresses — lives in `vpnconfig`, and the locked operations in
`service`, beside `PublishImport`. The shell does the same with jq, the same
file format and the same lock.

| Operation | Behavior |
|---|---|
| Add(url, name?) | Download, decode and resolve outside the lock, as today. Under the lock, check the limit and the name again, write a new file with a new id and recompute `xray.servers`. A link already saved, compared as written — before the download, or by another writer while it ran — turns the Add into a Refresh of that subscription, and into a Rename too when a free name was given. A list without a usable server creates nothing. |
| Refresh(id) | Download the saved link and publish under the lock only while the subscription still exists with that link. A success replaces `servers`, sets `refreshed` and clears `error`. A failure — download, decode, no usable server — sets `error` alone and keeps the list. A static list has nothing to refresh. |
| RefreshAll | Refresh every subscription with a link, in parallel, with one result per subscription. |
| Rename(id, name) | Change `name` and nothing else. |
| Delete(id) | Remove the file, recompute `xray.servers` and clear `preferred_server` when it names this subscription. The running Xray is left alone: when `active_server` names this subscription, the caller hears so and tells the user to select another server. |

All three front ends share these rules:

- The daemons accept `https` only, through the SSRF-hardened client and with
  the 1 MiB body cap. The shell also reads a file or a plain-http link and
  stores it as a static list.
- No message, log line or API answer carries a link: a `*url.Error` gives up
  its URL, as today, and lists and summaries show the host at most.
- An import summary names its subscription:
  `Alpha: Imported 32 of 40 servers: 7 composite, 1 DNS error`.
- The Web UI and the bot download through one service function; today they hold
  two copies of the same code with the same limits. The watch keeps its own
  path: the WAN, then the tunnel.
- A new link for a subscription means deleting it and adding the link. The
  publication guard compares the id and the link, so an in-place change can
  come later without a new guard.

## 5. The subscription watch

### 5.1 Armed

A failover record arms the watch, as today, and so do effective Xray clients
together with at least one subscription, a static list included. Today a list
imported from a file leaves the watch without a failover of its own.

### 5.2 The wave

A wave runs when the outbound dies and then on today's cadence: every 5
minutes, and 10, 20, then 30 minutes after a wave that found no live server.

1. Every subscription with a link downloads in parallel, each through today's
   path (the WAN, then the tunnel the clients are on) and each within
   `FetchTimeout` (3 minutes). A stop cancels them all, and a wave cut short
   leaves its window to the next.
2. Each download that succeeds publishes its own file under its guard (the id
   and the link). Each one that fails records `error`, unless the file's
   `refreshed` has moved since the wave read it: a Web UI or bot refresh that
   succeeded meanwhile is not marked failed by an older download.
3. The walk runs when at least one download succeeded, or when no subscription
   has a link. A subscription whose download failed is walked from its last
   list: a provider's panel can be down while its servers work. When every
   download failed there is no walk: Telegram gets "Subscription refresh
   failed: Alpha, Beta", and the next wave comes 5 minutes later, as today. A
   WAN outage fails every download, and a walk would then cost one Xray
   restart per server for nothing.

### 5.3 Walk order

The chosen server is `preferred_server`, or `active_server` when there is no
preferred one. Its subscription, while it exists, is the **own** subscription.

1. The chosen server, found by all four fields, or else by subscription and
   name.
2. The next servers of the own subscription in list order, skipping the chosen
   one, until three of its servers are placed (`OwnFirst = 3`).
3. The rotation: one server from each subscription in turn, in subscription
   order, starting with the subscription after the own one. The remaining
   servers of the own subscription take their turns too, and a subscription
   with no server left drops out.

Without an own subscription — nothing was chosen, or its subscription is
deleted — the rotation starts at the first subscription. With one subscription
the order is today's: the chosen server, then the rest of the list.

With subscriptions A (32 servers) and B (62), and A5 chosen:
A5, A1, A2, B1, A3, B2, A4, B3, A6, B4, …

Each server is tried at each of its addresses in turn, as today (`perAddress`).

**Deduplication.** Within a wave, a candidate whose outbound after
`ServerForDial` — the IPv4 address in its address slot — is byte-identical to
one already tried is skipped, with no write and no restart. One of the
author's providers puts 62 names on 9 endpoints. A record without an outbound
is never skipped this way.

### 5.4 Guards

- The walk's guard keeps its checks: a stop or a newer selection (`seq`) ends
  the walk.
- The guard adds one: the candidate's subscription must still exist with the
  link the wave read. Otherwise the write is refused (`errSubscriptionGone`),
  the remaining candidates of that subscription are skipped, and the walk goes
  on with the others. Today a changed link ends the whole walk.
- The restore after a live probe does not look at the subscription: the server
  runs and answers, and the deletion of its subscription does not keep the
  clients off it.
- The return to the preferred server works as today; its guard also checks
  that the preferred server's subscription still exists.
- `sameServer`, `chosenIndex` and `RecordWalkedServer` compare the subscription
  as well (section 3.2).

### 5.5 Messages

A server is named `<subscription> / <server>`: "LAN clients back on Xray;
server Beta / Germany-1". "No live server in the subscription" becomes "No live
server in any subscription". The variants that append "; still on tunnel:X"
keep it. A wave in which only some downloads failed sends no message: the
failure goes to the log and stays in `error`, which the Web UI and `/subs`
show.

### 5.6 Cost

A walk over two dead subscriptions of 32 and 62 servers takes up to about 25
minutes, less what deduplication skips. The Xray clients wait on the Tunnel
Director fallback meanwhile, as today. The background checks of the next design
shorten this.

## 6. Web UI

### 6.1 API

Parameters travel in the body, as they do for `/api/clients`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/subscriptions` | Every subscription: `id`, `name`, `host`, `static`, the server count, `added`, `refreshed`, `error`. No link. |
| POST | `/api/subscriptions` | Add `{url, name?}`; a saved link is refreshed instead. Answers today's import summary plus `id` and `name`. |
| POST | `/api/subscriptions/refresh` | `{id}` refreshes one subscription, `{}` all of them in parallel; one result per subscription. |
| POST | `/api/subscriptions/rename` | `{id, name}` |
| DELETE | `/api/subscriptions` | `{id}`; the answer says whether the running server came from it. |
| GET | `/api/servers` | The servers grouped by subscription, and `active` with `subscription`. |
| POST | `/api/servers/active` | `{subscription, index, name, address, port}`, the index counted within that subscription. 409 "server list changed" as today, and also when the subscription is gone. |

`POST /api/servers/import` and `subscription_saved` go away, and `/api/config`
has no link left to blank. Every mutating route serializes on `Deps.OpMutex`,
and Add and Refresh extend the write deadline as the import does today.

### 6.2 Servers tab

- **Subscriptions card**, on top: a table of name, host, server count, the time
  of the last refresh ("2 h ago") and status ("OK", or the error in red). Each
  row has ⟳ refresh (disabled for a static list), ✎ rename (edited in the row)
  and 🗑 delete, which asks first and warns when the running server comes from
  that subscription. Below the table: an add form (URL, optional name) and
  "⟳ Refresh all". The summary of the last add or refresh shows under the card,
  as today.
- **Servers card:** a collapsible section per subscription ("Alpha — 32
  servers") holding today's table with Select and the Active badge, which now
  compares the subscription too. The section of the running server starts
  open, the others collapsed.

### 6.3 Status tab

The Xray Server card reads "Beta / Germany-1". A server whose subscription was
deleted reads "<name> — not in any subscription".

## 7. Telegram bot

- **`/import <url> [name]`** adds a subscription, the name being the rest of
  the line after the link; a saved link is refreshed instead, and renamed when
  a free name is given. **`/import`** alone refreshes every subscription in
  parallel and answers with one line per subscription.
- **`/subs`** (new) lists the subscriptions, one line each:
  `Alpha — sub.example.com — 32 servers — 2 h ago — OK`, or the error. Each
  subscription has inline buttons ⟳ Refresh (not for a static list), ✎ Rename
  and 🗑 Delete. Delete asks "Delete Alpha? Yes / No" and warns when the running
  server comes from it. Rename asks "Send the new name for Alpha (or /cancel)"
  and takes the next text message of that chat. A text message goes to a
  pending rename first, and to the wizard only when no rename is pending; any
  command, `/cancel` and 5 minutes clear a pending rename.
- **`/xray`** chooses in two steps: first the subscriptions ("✓ Alpha (32)",
  "Beta (62)"; the check marks the running server's subscription), then that
  subscription's servers, 30 a page, with ◀ ▶ and « Back. A single subscription
  skips the first step. A server button carries
  `xray:select:<sub>:<index>:<fingerprint>`, the fingerprint now taken over
  `sub|name|address|port`. A keyboard sent before the update indexes a list
  that no longer exists and answers "server list changed — send /xray again".
  Two steps are needed in any case: Telegram limits the size of a keyboard
  (about 100 buttons), and two subscriptions already bring 94 servers.
- **The `/configure` wizard** chooses the server the same way in step 1, with
  Cancel. Its state records the subscription beside the name, address and
  port; step 4 and the apply look the server up by all four. A server whose
  subscription was deleted is reported gone, as one a refresh dropped is today.
- **`/servers`** groups by subscription, then by country as today, in pages.
- Notifications name servers as section 5.5 does. The help of `/start` and the
  bot's command list gain `/subs`.

## 8. Shell

### 8.1 `import_server_list.sh`

The script is interactive today and stays so, as a menu:

```
Subscriptions:
  1) Alpha   sub.example.com     32 servers   refreshed 2026-09-24 18:05
  2) Beta    panel.example.net   62 servers   error: HTTP 403

a) Add  r) Refresh  R) Refresh all  n) Rename  d) Delete  q) Quit
```

- With no subscription the menu opens on Add, so a first install still runs
  install, then import, then configure.
- **Add** asks for a link or a file path — https, http or a file, as today —
  then for a name (Enter takes the default). The download, `lib/subscription.sh`
  and `resolve_ip` work as today. Under the lock it writes
  `subscriptions/<id>.json` and recomputes `xray.servers`. Every write of the
  script also removes `servers.json` and `xray.subscription_url` (section 3.4).
- **Refresh** offers the subscriptions that have a link; its guard is the
  daemons' guard.
- **Rename** and **Delete** run under the lock. Delete clears
  `preferred_server` when it names that subscription, and warns when
  `active_server` does.
- The id comes from `/dev/urandom` through `od`, which BusyBox has, and is
  checked against the existing files. Times come from
  `date -u +%Y-%m-%dT%H:%M:%SZ`. Files are written as today: `mktemp` beside the
  target, `chmod 600`, `mv`.
- Names are checked without regular expressions, since Entware's jq is built
  without oniguruma: `explode` does it, as it does in `printable`.

### 8.2 `configure.sh`

The server step chooses in two steps, as the bot does: the subscriptions
(skipped when there is one), then the numbered servers of that subscription
with their protocol labels, as today. It writes `active_server` with
`subscription`, builds `xray.servers` from the union of the files instead of
`servers.json`, and deletes `preferred_server`, as today. At start it requires
at least one subscription ("Run import_server_list.sh first").

### 8.3 `lib/substore.sh`

A new library that both scripts source: the subscription order, reading, the
atomic write under the lock, the union of addresses, and the id and name rules.
The decoder `lib/subscription.sh` does not change; the name `substore` keeps the
two apart. `router/files.manifest` lists the new file, because the updater's
manifest test fails for any file under `router/` the manifest does not list.

## 9. Testing

**Go**

- The store: order by `added`, foreign files ignored, mode 0600, atomic writes,
  the union of addresses, ids, the name rules (ASCII case folding, the default
  host, the `-2` suffix, a taken name refused), the limit of ten, the guard on
  the id and the link.
- The operations: Add of a new link and of a saved one (a refresh, a rename),
  a failed Refresh (only `error` changes), RefreshAll in parallel, Rename,
  Delete (`preferred_server` cleared, `active_server` kept and reported), two
  writers under the lock.
- The watch: arming, static lists included; the wave rule (a walk after at
  least one download or with nothing to download, failed subscriptions walked
  from their last list, no walk and a retry in 5 minutes when every download
  failed); no error written over a newer success; the hybrid order (the chosen
  server present, the chosen server gone, the own subscription deleted, one
  subscription giving today's order); deduplication; one name in two
  subscriptions; a subscription deleted mid-walk; `seq` and `/stop`; the return
  to the preferred server; the message texts. `watch_test.go` (4,283 lines)
  moves to the new model with its invariants unchanged.
- The Web API: the subscription routes, the grouped servers, a selection with
  a subscription and its 409s, no link in any answer.
- The bot: `/import` in both forms, `/subs` with its buttons and the rename
  flow, `/xray` in two steps with pages and a keyboard from before the update,
  step 1 of the wizard, `/servers`.

**Bats:** `lib/substore.sh`, with the guard against jq's regex builtins that
`subscription.bats` has; `import_server_list.bats` (add, refresh, rename,
delete, the lock, static lists, the removal of the old files); the server step
of `configure.sh`.

**Parity:** synthetic subscription files in `testdata/substore/`, read by the Go
tests and the bats tests alike. No real link, host, key or id goes into them,
as none goes into `testdata/subscription/`.

**Web:** `npm run build`, which runs `vue-tsc`.

## 10. Documentation

`CLAUDE.md` (the architecture table, data storage),
`.claude/rules/telegram-bot.md` (commands, the subscription watch),
`.claude/rules/webui.md` (the API table), `.claude/rules/xray-tproxy.md`,
`.claude/rules/testing.md`, `README.md`, and the header comment of
`lib/xrayconf.sh`, which names `servers.json`.

## 11. Device check

Last, and only with the owner's explicit permission, on the owner's router:
add both real subscriptions from the Web UI and from the shell; select a server
from each; break the running server and watch the walk reach the other
subscription; rename and delete; go through the bot's keyboards. The links are
asked for at that point and never written into the repository, a log or a
fixture.

## 12. Out of scope

- Background health checks, latency in the Web UI, and a switch from one Xray
  server to another without the tunnel: the next design.
- A server per LAN client or group.
- Reordering subscriptions, or keeping one out of the failover.
- Changing the link of a subscription in place (delete and add instead).
- Refreshing subscriptions while the outbound works.
- Resolving the hosts of a cached list again during a wave.
- Migrating the single subscription of earlier releases.
