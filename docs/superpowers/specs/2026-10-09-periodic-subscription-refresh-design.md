# Periodic subscription refresh in `vpn-director-watchd`: design

Date: 2026-10-09
Branch: `feature/periodic-subscription-refresh`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Keep every subscription's server list current, so that the monitor and the failover walk work from
the addresses a provider serves now rather than those of the last failover. `vpn-director-watchd`
downloads every subscription with a link every 5 minutes by default and writes only what changed.
A server that did not change keeps its monitor status until its own next check.

Out of scope: downloading a subscription through the main Xray when the WAN cannot reach its link.
That is a separate design.

## 2. Where it stands

- A list changes only through a manual refresh (the Web UI, the bot's `/import` and `/subs`,
  `import_server_list.sh`) or through the watch's wave, which runs only after the main Xray's SOCKS
  probe has failed. A provider that rotates addresses leaves the monitor checking old ones for days:
  on 2026-10-09 one manual refresh raised the live endpoints from 7 of 93 to 56 of 92 on the
  RT-AX86U, and from 0 to 26 of 30 on the KN-4521.
- `vpnconfig.RefreshSubscription` writes the subscription file and `vpn-director.json`
  (`xray.servers`) and moves `refreshed` on every call, whether or not the list changed.
- The monitor keeps an endpoint's status, latency, next check and dead backoff for as long as the
  endpoint's key stays in the set (`Monitor.merge`); a new key starts unknown and is checked at once.
  The key hashes the whole outbound with the IP in its address slot (`endpoint.Key`).
- Panels choose some fields at random on every request. 3x-ui (`applyShareRealityParams` in
  `internal/sub/service.go`) picks the REALITY `sni` from `serverNames` and the `sid` from
  `shortIds`; Marzban (`app/subscription/share.py`) does the same with `random.choice`, and picks
  hosts and addresses that way too. Downloaded twice, such a subscription gives every REALITY server
  a new key, so a refresh every 5 minutes would reset their statuses every 5 minutes.
- 3x-ui's default remark template, `{{INBOUND}}-{{EMAIL}}|📊{{TRAFFIC_LEFT}}|⏳{{DAYS_LEFT}}D`, puts
  the traffic and the days left into every server's name, and the name changes as traffic is used.
  Names anchor the selected server:
  - `xray.active_server`, `xray.preferred_server` and `xray.pending_restore.active` hold its
    subscription, name, address and port;
  - the watch's guards compare these records whole (`activeID`, `sameRestoreActive`);
  - `monitor.Build` puts the active server's endpoints first by them;
  - `/xray` buttons and Web UI rows carry `ServerFingerprint` (subscription, name, address, port);
  - the bot wizard looks its pick up again by all four (`wizard.State.PickedIndex`).

## 3. Decisions

| Question | Decision |
|---|---|
| Where the refresh runs | A loop in `subwatch`, in its own goroutine of watchd. It needs no Xray client: the monitor runs without them. |
| How often | `monitor.subscription_refresh`, `5m` by default, `0` off. It does not depend on `monitor.enabled`: the failover walk wants current lists too. |
| When it stands down | VPN Director stopped, the compatibility gate closed, the watch in a failure episode (the wave refreshes then). |
| How it writes | It downloads outside the watch's tick and publishes between ticks, holding `tickMu`, one config-lock update per subscription. |
| What counts as a change | The fresh list merged with the stored one (4.3). When nothing changed, nothing is written, `refreshed` included. |
| REALITY `serverName`, `shortId`, `spiderX` | Not a change: the stored copy stays, and with it the endpoint key. |
| Names | The fresh name is taken. `active_server`, `preferred_server` and `pending_restore.active` follow a renamed server in the same write; `seq` does not move. |
| A host that does not resolve this time | The server keeps its stored addresses. |
| Manual refresh | Takes fresh copies, as today, and carries the records over a rename the same way. |
| The wave, `import_server_list.sh` | Unchanged: fresh copies, no carry-over. |
| What the user sees | The Web UI's Refreshed column becomes Changed; `/subs` says `changed 2 h ago`. |
| Notifications | None. The watchd log says what changed. |

## 4. Design

### 4.1 The loop

`subwatch` gains a refresh loop, `(*Watch).StartRefresh`, which `runRuntime` starts beside
`Monitor.Run`, `Watch.Start`, `Queue.Run` and the health publisher; it ends with the daemon's
context.

The first round starts a minute after watchd starts, since at boot the WAN may not be up yet. Each
later round starts one interval after the previous round ended, so rounds never overlap. The loop
reads the interval before every round; while it is `0`, the loop looks again every minute. A round
that stands down is tried again a minute later.

A round stands down when:

- `/tmp/vpn-director/stopped` exists;
- the compatibility gate refuses mutations (`CanMutate`);
- the watch is in a failure episode: its probe has failed since the last success (`failSince`),
  `xray.failover` or `xray.pending_restore` exists, or one of its applies has not yet succeeded
  (`pendingApply`);
- no subscription has a link.

The loop reads the watch's state while holding `tickMu`: it waits for a running tick to end and
reads what that tick left.

A round downloads every subscription with a link at once, each within `FetchTimeout` (3 minutes),
through the wave's fetcher (4.8). A stop or a closed gate cancels the downloads within a second, as
`stopPoll` cancels a tick's waits, and so does the daemon's shutdown. A subscription that keeps
failing is tried every round; its error is written once (4.4).

When every download has ended, the loop takes `tickMu` and checks the stand-down conditions again.
An episode that began during the downloads drops the results: the wave owns the refresh then.
Otherwise the loop publishes each subscription in turn (4.4) and releases `tickMu`. It stops
publishing once the daemon's context ends.

The loop never touches the running Xray: no apply, no restart. A changed address set recomputes
`xray.servers`, as a manual refresh does, and `TPROXY_BYPASS` follows at the next apply.

In `--dev` the loop downloads the links of the dev subscriptions, as the wave does there.

### 4.2 Server identity

A server's identity is its stored outbound without the REALITY fields a panel may pick at random:
`serverName`, `shortId` and `spiderX` of every `realitySettings` object in it, an xhttp
`downloadSettings` included. The keys are matched as Xray matches them, folding case as
`strings.EqualFold` does, so no spelling escapes the removal and the decoders' guarded-key list
needs no new name. Everything else stays: address, port, protocol, credentials, transport, and the
`serverName` of plain TLS, which chooses the backend on a CDN. The identity is the canonical JSON
of what remains (decoded and marshalled again, which sorts the keys), so key order and whitespace
do not matter.

A record without an outbound, stored before outbounds were, has no identity. It pairs with
nothing, and the first refresh replaces it with a fresh copy, as today.

The function lives in `vpnconfig`: the publication (4.4) and the wizard (4.7) both use it, and
`endpoint` imports `vpnconfig`, not the reverse.

### 4.3 The merge

The merge pairs the fresh list with the list in the file:

1. Each fresh server, in list order, pairs with the first stored server of the same identity that
   is not yet paired.
2. A paired server is the stored record, with its outbound and so with the stored `sni`, `sid` and
   `spx`, under the fresh name. Its addresses are:
   - the stored ones, in the stored order, when the fresh addresses are the same set;
   - the stored ones when the fresh host did not resolve this time, so that a DNS failure neither
     drops the server nor resets its status;
   - the fresh ones otherwise.
3. A fresh server without a pair comes in as it is: it is new, or it changed. One whose host did not
   resolve is left out, as today.
4. A stored server without a pair leaves.
5. The servers take the fresh list's order.

The merge also reports the renames, the paired servers whose name changed, for 4.5, and counts the
servers it added, removed, renamed and readdressed, for the log.

Marzban's random addresses and its `*` wildcard hosts still change a server's identity: such a
server comes in as a new one on every download.

### 4.4 Publication and errors

A new `vpnconfig` operation publishes a periodic refresh in one config-lock update. It refuses
what `RefreshSubscription` refuses: `ErrSubscriptionGone` when the subscription was deleted, or
given another link, while it downloaded. The update's guard checks the stop marker and the gate
under the lock, as the watch's other writes do. The merge runs against the file as it is then, not
against what the round read before downloading, since a manual refresh may have written in
between.

- When the merged list equals the file's, server by server and field by field in the same order,
  and the file records no error, the operation writes nothing: neither the subscription file nor
  `vpn-director.json`.
- Otherwise it writes once: the merged list, `refreshed` set to now, `error` cleared,
  `xray.servers` recomputed, and the carry-over of 4.5.
- When the merge comes out empty although the body held servers (nothing resolved, nothing
  paired), the list stays and the round records "could not resolve IP for any server".

A download that failed, a body without a supported server and the empty merge are recorded with
`RecordSubscriptionError`, against the `refreshed` the round read before downloading. It writes
nothing when the same error is recorded already.

### 4.5 Carrying the records over a rename

The update that writes the merged list renames every record that names a renamed server:
`xray.active_server`, `xray.preferred_server` and `xray.pending_restore.active`. A record names a
server when its `subscription` is this subscription and its name, address and port are those of a
stored server, the first such server, as elsewhere. Address and port belong to the identity and do
not change. `seq` does not move: a rename is not a selection, and the watch's write counter must not
take it for one. A record whose server left the list stays as it is, as today.

All three records move together. `sameRestoreActive` compares `active_server` with
`pending_restore.active` whole, and a restore that found them different would discard its intent as
superseded by a new selection.

The watch's guards compare the records whole as well (`activeID` includes the name): a rename in
the middle of a walk or a return attempt would read as a new selection and end it. The loop
publishes only between ticks and stands down during episodes, so neither sees a record change under
it. One window remains. A successful probe remembers the active server's identity
(`probeOKActive`); if Xray dies within about 30 seconds of a rename, before the next successful
probe, the fast path does not recognise the server for that episode, and the legacy confirmation
decides in 1 or 3 minutes.

### 4.6 Manual refresh

A manual refresh keeps taking the fresh copies: the Web UI's refresh and refresh-all, an add of a
saved link, the bot's `/import` and `/subs`. It carries the records over renames with the same
code: the identity pairs the stored list with the fresh one, and 4.5 applies. Without it, a manual
refresh of a subscription whose names change would keep losing the Active mark and the return to the
preferred server.

A manual refresh runs in another daemon and cannot wait for the watch's tick. If it lands in the
middle of a walk, which runs only during a failover, or of a return attempt, which takes seconds,
its rename ends them, and the next tick starts over.

The wave and `import_server_list.sh` are unchanged: fresh copies, no carry-over.

### 4.7 The bot wizard

`wizard.State.Picked` also keeps the identity of the server step 1 picked. When step 4 or the
apply finds no server with the picked subscription, name, address and port, it takes the first
server of that subscription with that identity. Without it, a subscription whose names change would
often make the wizard report its pick gone after the minutes its steps take.

### 4.8 The fetcher

The loop downloads through the wave's fetcher, `service.SubscriptionFetcher`: the WAN, then the
Tunnel Director tunnel. The fetcher gains a way to return, beside the resolved servers, the decoded
servers whose host did not resolve, without addresses: the merge needs them for 4.3, step 2. A body
none of whose hosts resolved, even over the tunnel, is still a body to the loop, and the merge keeps
the stored addresses of the servers it pairs. The wave ignores the unresolved servers and behaves as
before.

### 4.9 What the user sees

The monitor works as before: a status lives while its key does. With the stored copies kept, a key
changes only when its server changed. A server whose address changed gets a new key, unknown and
checked at once, since its old status belonged to another address. New servers start unknown and
are checked at once; servers that left disappear. The prober restarts only when the endpoint set
really changes. The monitor reads the files every minute, so a published change reaches it within a
minute, and the subscription-health notifications follow the statuses as before.

In the Web UI the subscription table's Refreshed column becomes Changed: the time the list was last
written by a change, a manual refresh or a cleared error. The Status badge shows the last check: a
failed round records its error, and the next successful one clears it. The API field keeps the name
`refreshed`.

In the bot, `/subs` reads `Alpha — sub.example.com — 32 servers — changed 2 h ago — OK`.

`/xray` buttons and Web UI rows carry the name in their fingerprint: after a rename they answer
that the list has changed, and the user opens it again.

### 4.10 Logging

No link reaches the watchd log.

- INFO for each subscription whose list changed, with the counts of added, removed, renamed and
  readdressed servers.
- INFO for each record carried over a rename, with the old and the new name.
- DEBUG for a round that changed nothing.
- WARN when a subscription's error appears or changes, INFO when the subscription downloads again.
- WARN once per value for a `monitor.subscription_refresh` out of bounds.

No Telegram message.

## 5. Configuration

`monitor.subscription_refresh` in `vpn-director.json` is a Go duration, `5m` when absent, `0` to
turn the refresh off. A value that does not parse, is negative or is below `1m` takes the default
with a WARN, as the other monitor keys do (`monitor.SettingsFrom`). The template, `README.md` and
`README.ru.md` list it, and `configure.sh` brings it into an existing config with the rest of the
template. After an update, a router whose config lacks the key refreshes every 5 minutes.

## 6. Compatibility

The watchd socket does not change. A Web UI or bot of an earlier release works with the new watchd:
its column still says Refreshed and shows the time of the last write. A new Web UI or bot with an
earlier watchd shows Changed, which holds there too, since every refresh then writes. The
configuration gains one optional key.

## 7. Known limits

- A stored REALITY `sni` or `sid` that the server's admin removes leaves the stored copy dead until a
  manual refresh or the wave takes fresh copies.
- `configure.sh` records the name from the list it read when the user chose. A rename during its
  steps records the old name, and the Active mark returns at the next selection. A shell twin of the
  identity would fix it; it is out of scope.
- The wave and `import_server_list.sh` take fresh names without carrying the records over, as
  today.
- Marzban's random addresses and wildcard hosts still produce a new endpoint on every download.
- The fast-path window of 4.5.
- `TPROXY_BYPASS` holds the previous addresses until the next apply.

## 8. Testing

Go:

- `vpnconfig`, identity: the REALITY fields go from every `realitySettings`, `downloadSettings`
  included, under any spelling Xray folds; the TLS `serverName` stays; key order does not matter; a
  record without an outbound has no identity.
- `vpnconfig`, merge: a random `sid`, `sni` or `spx` keeps the stored copy; the fresh name is taken;
  addresses for the same set, an unresolved host and a new set; duplicates pair in order; the fresh
  order; an empty merge.
- `vpnconfig`, publication: nothing is written when nothing changed, neither the file nor the
  config; clearing an error writes; a deleted or relinked subscription is refused; the carry-over
  moves all three records, keeps `seq`, touches only this subscription's records and leaves a record
  whose server left.
- `service`: a manual refresh takes fresh copies and carries the records over; the fetcher returns
  the unresolved servers, and the wave ignores them.
- `subwatch`, the loop: the first round after a minute; the interval counted from a round's end and
  read every round; `0` off; standing down on a stop, a closed gate, `failSince`, a failover, a
  pending restore, a pending apply; an episode that begins during the downloads drops the results;
  no publication inside a tick; a stop cancels the downloads; an error is recorded once.
- `cmd/watchd`: after a published refresh in which every `sid` changed, the monitor keeps its
  statuses and the prober does not restart.
- `wizard`: a renamed pick is found by its identity.
- `monitor.SettingsFrom`: the new key's default, `0`, a value below a minute, garbage.
- The bot's `/subs` line and the Web UI's column header.

No Bats test: the shell changes only in the template.

## 9. Documentation

- `.claude/rules/watchd.md`: a section on the periodic refresh.
- `.claude/rules/telegram-bot.md`: the wave takes fresh copies while the periodic refresh keeps the
  stored ones; the `/subs` line.
- `.claude/rules/webui.md`: the Changed column; a manual refresh carries the records over.
- `CLAUDE.md`: the monitor settings line.
- `router/opt/vpn-director/vpn-director.json.template`, `README.md`, `README.ru.md`: the new key.
