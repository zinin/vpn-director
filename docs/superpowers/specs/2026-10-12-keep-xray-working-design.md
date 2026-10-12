# Keep Xray on a working server: design

Date: 2026-10-12
Branch: `feature/keep-xray-working`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

Xray must keep carrying traffic. When the selected server moves, leaves its subscription or dies,
`vpn-director-watchd` follows it or switches Xray to the best working server. It does so with or
without LAN clients on Xray. It prefers servers of the same kind (protocol and security), because
other kinds in the same subscriptions often answer a check but carry nothing.

## 2. Where it stands

On 2026-10-12 both routers ran v0.20.1. On each, Xray had been dead for days:

- The provider moved the selected server to a new address: a new IP on the RT-AX86U, a new host on
  the KN-4521. Its UUID, key and port stayed.
- The periodic refresh took it in as a new server, because the address is part of the server
  identity. `xray.active_server` follows renames only, so it kept the old address. The subscription
  held one entry with that name, at the new address, and no entry at the old one.
- `/opt/etc/xray/config.json` still held the old address. It was generated on 2026-10-10 on the
  Asus and on 2026-10-09 on the Keenetic. A SOCKS probe through Xray timed out on both. The
  monitor reported the same server at its new address alive.
- Nothing switched Xray, because the watch acts only while it is armed:
  - `vpnconfig.Armed` needs effective Xray clients;
  - the Asus had its only client paused;
  - the Keenetic had no client at all;
  - an unarmed `Tick` returns at once: no probe, no failover, no return.
- The periodic refresh never touches the running Xray (its spec, 4.1). A manual refresh does not
  either: it replaces the list and leaves the record and `config.json` as they were.

## 3. Decisions

| Question | Decision |
|---|---|
| When to act | Always: with clients (the watch armed) and without them (a new keeper mode of the tick). |
| The selected server moved | Follow it: the same subscription, a single entry with the same name. |
| It changed in place (keys, UUID) | Regenerate `config.json` from the entry and restart Xray. |
| It left the list, or cannot be found | Switch to the best server: same subscription, then other subscriptions. |
| It died while still listed | Switch to the best server, remember the user's choice and return to it. |
| What "best" is | Alive per the monitor, lowest latency, same group first (section 4.5). |
| A server's group | Protocol and security of its outbound: `vless+reality`, `trojan+tls`, `shadowsocks`… Transport is no part of it. |
| No alive server of the group anywhere | Last resort: the best alive server of any group. Back to the group as soon as one is alive. |
| Return to the user's choice | When it has been alive for 5 minutes in a row. |
| The existing failover with clients | Uses the same order of candidates (fast path and walk). |
| Telegram | One message per switch, through watchd's durable queue. |

## 4. Design

### 4.1 Two modes of the tick

`Tick` runs every 30 seconds as now.

- **Clients mode**: the watch is armed (`vpnconfig.Armed`). It works as today, plus the actuality
  check (4.2) between episodes. The fast path and the walk order their candidates with
  `bestServers` (4.5, 4.7).
- **Keeper mode**: the watch is not armed, but `xray.active_server` names a server of a
  subscription. There are no clients to move into a tunnel and no wave to run. The tick:
  - probes Xray through SOCKS as the armed tick does;
  - applies the same death rules (4.4);
  - runs the actuality check (4.2);
  - runs the returns (4.6);
  - switches servers the way the walk does (4.3).

Both modes run inside the tick, holding `tickMu`, with every existing guard:

- the stop marker;
- the compatibility gate (`CanMutate`);
- the selection fence: `seq`, through the walk's guarded write;
- the context the tick carries.

The periodic refresh switches nothing itself. It refreshes the lists, and the next tick, at most
30 seconds later, finds the difference. A manual refresh from the Web UI or the bot is found the
same way.

### 4.2 Is the selected server current?

`ActiveServer` gains two optional fields. They describe the server the record names:

- `identity`: a short hash (the first 16 hex digits of a SHA-256) of the server's loose identity
  (`looseIdentity`: the stored outbound without the REALITY picks and without the generated
  WebSocket/HTTPUpgrade Host and path).
- `group`: its group (4.5).

Every Go writer of `active_server` and `preferred_server` fills them in: the Web UI's select,
`/xray`, the bot's wizard, the walk, the fast path, the return and the keeper. `followRenames`
leaves them as they are: a rename pairs by identity, so the server's outbound is unchanged.
`configure.sh` and older daemons write records without them.

Each tick outside an episode classifies the record against the current list of its subscription:

| Case | Condition | Action |
|---|---|---|
| Current | An entry with the record's subscription, name, address and port exists, and the record has no `identity` or the entry's is the same | None |
| Changed in place | That entry exists, and its `identity` differs from the record's | Follow (4.3) |
| Moved | No such entry; exactly one entry of the subscription has the record's name | Follow it (4.3) |
| Gone | Neither: the subscription is gone, no entry has the name, or several do | Replace (4.3) |

A record without a `subscription`, or no record at all (no server selected), is left alone. An
episode runs while the probe fails (`failSince`), `xray.failover` or `xray.pending_restore`
exists, or an apply of the watch's own is pending. These are the conditions under which the
periodic refresh stands down. During an episode, the episode's own logic owns the record.

### 4.3 Actions

Every switch works as the walk does. The watch writes `config.json` from the server with the walk's
guard (`GenerateAndRecordGuardedWalkedServer` or its successor), runs `restart xray-process`,
waits, and probes through SOCKS. If the probe fails, it tries the next candidate. A selection that
the user commits meanwhile ends the action, as it ends a walk.

- **Follow** (changed in place, moved): the same server at its current entry. `active_server` moves
  to it, with a new `identity` and `group`. So does `preferred_server` when it named the same
  server. The probe decides: a live one ends the action; a dead one goes on as "died" (below), with
  the followed entry as the user's choice.
- **Replace, gone**: the user's choice has left the list. The watch switches to the first working
  server in `bestServers` order. `RecordWalkedServer` keeps the vanished choice in
  `preferred_server`, as it keeps any choice the walk leaves, so its `group` stays the reference
  group across a watchd restart. Nothing returns to it while no entry of its subscription has its
  name. Should one appear again, the return treats it as the user's choice, as any other.
- **Replace, died**: the server is listed but Xray does not work through it (4.4). The watch
  switches to the first working server in `bestServers`, leaving out the dead one. The user's
  choice stays in `preferred_server`, as the walk keeps it today (`RecordWalkedServer`).
- **Nothing works**: no candidate passed the probe. Xray stays on the last one tried. One message
  says so (4.8), and the next attempt waits 10, 20, then 30 minutes, as a walk that finds no live
  server waits today.

### 4.4 When the selected server counts as dead

These are the rules the armed tick applies today, now in keeper mode too:

- **Fast**: fresh monitor evidence says the active server's endpoints are dead, and the SOCKS
  probe fails (`fastFailover`).
- **Unreachable**: the probe fails, and the server's addresses refuse TCP while a control address
  accepts. Dead after `FastDeadAfter` (1 minute).
- **Legacy**: the probe fails for 3 minutes.

In keeper mode a death does not move clients and does not run a wave: it runs the replacement of
4.3 at once. In clients mode the existing failover runs as today, with the new candidate order
(4.7).

### 4.5 `bestServers`: the order of candidates

**Group.** A server's group is its outbound's protocol and its stream's security, for example
`vless+reality`, `vless+tls`, `vless`, `vmess+tls`, `trojan+tls`, `shadowsocks`, `hysteria2`. A
legacy record without an outbound has no group, and neither does an outbound that does not parse.
A server without a group never matches the reference group: it sorts with the other groups.

**Reference group.** The group the user wants:

1. `preferred_server.group` when a preferred server is set (a vanished choice included, 4.3);
2. otherwise `active_server.group`;
3. otherwise the group of the entry the record names, when it is listed.

With none of these, groups do not order the candidates.

**Alive and latency.** A server can have several addresses, each its own monitor endpoint. A server
is alive when at least one of its endpoints is alive in fresh evidence (`watchdapi.StateOK`). Its
latency is the lowest `LatencyMS` among its alive endpoints. Its alive addresses are dialed first,
fastest first (`endpoint.PerAddress` copies, reordered).

**Order.** Each tier is sorted by latency, ties by list order:

1. alive servers of the reference group in the reference subscription;
2. alive servers of the reference group in the other subscriptions, as one list;
3. alive servers of other groups in the reference subscription;
4. alive servers of other groups in the other subscriptions;
5. every other server (unchecked, dead): those of the reference group first, then the rest. Each
   part keeps the walk's present order (`walkOrder`: `OwnFirst` of the user's subscription, then
   one server of each subscription in turn). A provider that fails as a whole must not cost a try
   of each of its servers before another provider's first.

Left out:

- endpoints the monitor rejected;
- the server that died;
- servers already tried in this attempt.

The reference subscription is the user's choice's.

**Without evidence.** The monitor may be disabled, or its state not OK (WAN down, prober failing).
Nothing counts as alive then: the order is that of tier 5, the reference group first and each part
in the walk's present order. Latency plays no part.

The watch never switches between alive servers to gain latency. A switch happens only for the
cases of 4.2, a death, or a return (4.6).

### 4.6 Returns

The existing return (`preferred_server`, `ReturnCheck` every 5 minutes, `ReturnRetry` 10, 20,
30 minutes, `ReturnFailsMax`) runs in both modes, with two changes:

- **The look.** With fresh evidence, a return is attempted when the preferred server has been alive
  for at least 5 minutes: an alive endpoint whose `Since` is 5 minutes old or more. Without
  evidence, the present TCP look applies.
- **Back to the group.** Xray may run a server of another group while the reference group has an
  alive server (5 minutes, as above). The preferred server itself may be unavailable or dead. Then
  the watch switches to the best alive server of the reference group, by the order of 4.5, tiers 1
  and 2. `preferred_server` stays, so a later return to the user's own server still happens.

A return that fails rolls back as today and backs off. A user selection clears `preferred_server`
and ends the returns.

### 4.7 The existing failover uses the same order

- **The fast path** (`fastFailover`) takes its candidates in `bestServers` order. Today it uses
  `healthWalkOrder`: alive first, then walk order.
- **The walk** after a wave tries servers in `bestServers` order. Today it uses `walkOrder`:
  `OwnFirst`, then one server of each subscription in turn. The dedupe of copies already tried
  (`endpoint.DialKey`) stays.

The reference group and subscription are those of the user's choice. Without evidence, the walk's
present order applies inside the tiers (4.5).

### 4.8 Messages and log

One Telegram message per switch, through the durable queue, in English like every watchd message:

- `Xray server Alpha / DE-1 moved; Xray follows it`
- `Xray server Alpha / DE-1 changed; Xray follows it`
- `Xray server Alpha / DE-1 left its subscription; Xray switched to Alpha / NL-2`
- `Xray server Alpha / DE-1 is not answering; Xray switched to Beta / FI-3`
- `Xray back on the preferred server Alpha / DE-1` (the present return message)
- `Xray back on a vless+reality server Alpha / NL-2`
- `No working Xray server found; Xray stays on Alpha / DE-1`

The armed failover keeps its present messages. A switch the fast path makes keeps its present
message (`Xray switched to server Alpha / NL-2`), in either mode. Each switch sends its message: the watch's
deduplication by message kind (`notify`), which suppresses a kind sent twice in a row, does not
apply to these. "No working Xray server found" goes once per run of failed attempts. A candidate
that fails gets no message; it gets the log line the walk writes today. Each action logs one INFO
line with its reason. No log line carries a link.

### 4.9 When the keeper does nothing

- `/tmp/vpn-director/stopped` exists (VPN Director stopped).
- The compatibility gate refuses mutations.
- No server is selected, or the record has no subscription.
- Xray is not installed. The probe and `Generate` need it.

## 5. Compatibility

- `identity` and `group` are optional. A record without them is checked by name, address and port,
  as now. An older daemon that writes the config drops them, and the next Go selection writes them
  again.
- The watchd socket does not change, and the config gains no key.
- The bot and the Web UI need no change beyond writing the two fields when they record a selection.

## 6. Costs

- Every switch restarts the Xray process: the open connections of LAN clients on Xray break.
- The monitor's latency is that of its last check, so the choice among alive servers is noisy. The
  watch never switches for latency alone (4.5).

## 7. Known limits

- A server renamed and moved in one download is "gone". It is replaced by the best server of its
  group, and nothing returns to it.
- Several entries of one subscription with the record's name make it "gone" as well.
- A vanished choice stays in `preferred_server` (4.3). Should an entry of its subscription take its
  name again, the return treats that entry as the user's choice.
- The probe proves an HTTP 204 through Xray, not throughput. A server that answers and throttles
  stays selected. The group preference keeps such a server out unless no alive server of the group
  exists.
- Keeper mode runs no wave: the periodic refresh keeps the lists current. With
  `subscription_refresh` set to `"0"`, the keeper works from the lists as they are.

## 8. Testing

Go:

- **Groups:** every protocol and security pair, a legacy record, an outbound that does not parse.
- **`bestServers`:**
  - the tiers, latency inside a tier, ties by list order;
  - several endpoints per server (alive if one is alive, lowest latency, alive addresses first);
  - rejected and dead servers, servers already tried;
  - no evidence: tiers with the walk order inside;
  - the reference group from `preferred_server`, `active_server` and the listed entry.
- **Actuality:** current, changed in place, moved, gone (no name, an ambiguous name, a deleted
  subscription), a record without `identity`, a record without a subscription.
- **Keeper tick:**
  - follow (moved, changed) with a live probe;
  - follow with a dead probe, which goes on as "died";
  - gone (best of the group; the vanished choice kept in `preferred_server`);
  - died (fast, unreachable, legacy), with `preferred_server` kept;
  - nothing works (message and backoff);
  - return after 5 minutes, back to the group, rollback on a failed probe;
  - a stop, a closed gate and a user selection during an action;
  - Xray not installed.
- **Clients mode:** the fast path and the walk in `bestServers` order (update the existing tests);
  the actuality check between episodes.
- **Writers:** the Web UI's select, `/xray`, the wizard, the walk, the fast path and the return
  write `identity` and `group`; `followRenames` keeps them.
- **Messages:** each case's text, one per switch.

No Bats test: the shell changes nothing.

## 9. Documentation

- `.claude/rules/watchd.md`:
  - a section on keeper mode and the actuality check;
  - the new candidate order for the fast path and the walk;
  - the returns;
  - the messages.
- `.claude/rules/telegram-bot.md`: the walk order in the subscription watch section, and the
  messages.
- `.claude/rules/webui.md`: `identity` and `group` in the selection record.
- `README.md`, `README.ru.md`: Xray follows a moved server and switches away from a dead one,
  with or without clients.
- `CLAUDE.md`: the `subwatch` row.
