---
paths: "router/opt/vpn-director/**"
---

# Xray TPROXY

Transparent proxy routing for LAN clients via Xray.

Module location: `lib/tproxy.sh`

## Usage via CLI

```bash
vpn-director.sh status xray       # Show Xray TPROXY status
vpn-director.sh restart xray      # Restart the Xray process and apply TPROXY again in place
vpn-director.sh restart xray-process  # Restart the Xray process only, TPROXY rules kept
vpn-director.sh apply             # Apply all (including TPROXY)
```

`restart xray` restarts the process, then applies the TPROXY rules in place (`tproxy_apply`,
`tproxy_prune`): nothing is stopped, so a client meets a restarting Xray and waits — TPROXY drops
what no socket takes. The Web UI server switch, `/xray` and the wizard use it; its apply also swaps
in `TPROXY_BYPASS`, whose `xray.servers` a switch may have recomputed. Once `pref_base`,
`mark_mask` or `mark_shift` has moved the direct rule since the last apply, the apply of
`restart xray`, as `apply xray`, runs in full, with a WARN (`guard_layout_moved`, `lib/guard.sh`):
alone it would move the direct rule ahead of Tunnel Director's lookup rules, still at the former
base, and send what Xray lets through of a client on a tunnel too to main. `restart xray-process`
restarts the process alone, for a caller whose only change is `config.json` — the subscription
watch trying one server after another.

## How It Works

1. Selected LAN clients → mangle PREROUTING chain
2. Exclude: servers, private IPs, specified countries
3. Remaining traffic → TPROXY to Xray port
4. Xray dokodemo-door inbound → the selected server's outbound (proxy-out; any protocol the import stored)

## Applying without a window

`tproxy_apply` flushes nothing a packet is crossing:

- `XRAY_TPROXY` is built as `XRAY_TPROXY_NEW` on every apply (`_tproxy_build_chain`) and swapped in
  by `swap_fw_chain`. Flushing the live chain and refilling it one rule per call left every Xray
  client unproxied — out through the WAN — for the length of the refill, on every apply.
- `TPROXY_BYPASS` is built as `TPROXY_BYPASS_NEW` and swapped in (`ipset swap`).
- `XRAY_CLIENTS` is only added to, by the soft-fails as well: with xt_TPROXY missing or an
  exclusion set not ready, a live set still takes every effective client
  (`_tproxy_add_to_live_clients`). `tproxy_prune`, the last step of an apply, swaps in the
  effective clients - `xray.clients` less `paused_clients`, an entry that is no IPv4 address or
  CIDR skipped - plus every client the caller names that the live set holds: after `tunnel_apply`
  (a full apply) the clients it reported as not carried, for `apply xray` and `restart xray` every
  client the config puts on a tunnel, `main`'s left out (`tunnel-director.md`, "What tunnel_apply
  reports as not carried") - unless the direct rule moved since the last apply, when they apply in
  full, with a WARN (above). A client moving to a tunnel stays proxied that way until an apply in
  which Tunnel Director carries it (`packet-flow.md`, "The apply: make before break"); one that left
  Xray for direct - moved to `main` as well - is let go. A named client the new set already matches
  gets no element of its own: `ipset test` finds a host through any network the set holds, so a host
  inside an effective network is in through that network, and `x/32` of an effective `x` is `x`. A
  WARN names the clients kept.
- The guards follow `XRAY_CLIENTS` ("Fail-Safe"): `tproxy_apply` syncs them once the chain is in,
  and the set only grows there, so a client that left Xray keeps its guards until `tproxy_prune`,
  whose sync after the swap takes them away. Each sync puts the new list in before the old one
  goes (`guard_sync`). While the chain does not answer for its direct mark, the prune puts no guard
  in, but still takes away those of a client the swap let go (`_tproxy_guards_prune`).
- A name from `advanced.xray.chain` leaves room for `_NEW` (24 characters at most), and so does
  one from `advanced.xray.clients_ipset` or `bypass_ipset` (27).

## Outbound Generation

`config.json.template` is valid JSON with an empty `outbounds: []`. The selected server's
outbound goes into it as `outbounds[0]`, tagged `proxy-out`:

- Shell: `lib/xrayconf.sh` (`xrayconf_generate`, jq) — used by `configure.sh`.
- Go: `service/xray.go` (`serverOutbound` + `encoding/json`) — used by the Web UI, the bot wizard,
  `/xray` and the subscription watch.

An import stores each server's Xray outbound in its subscription's file
(`<data_dir>/subscriptions/<id>.json`, `servers[].outbound`): converted from a share
link, or taken from an Xray JSON subscription — the decoders are `lib/subscription.sh`
and `server/internal/subscription`, which answer to the same cases in `testdata/subscription/`. The
generators insert it as stored, so any protocol and transport Xray runs works: VLESS (tcp, ws, grpc,
httpupgrade, xhttp), VMess, Trojan, Shadowsocks, Hysteria2. They look for three shapes only, and
refuse all three: an `outbound` that is there but is no object (a string, a null — only a record
without the key is a legacy one); an xhttp `downloadSettings` anywhere in it that names no `address`,
a download stream Xray gives no destination and dereferences on the first dial (`splithttp` in
26.2.6); and an xhttp `scMaxEachPostBytes` of 8192 or less — read from `extra` when there is one,
since Xray builds from `extra` with the outer `host`, `path` and `mode` copied onto it — in a stream
Xray would run in packet-up mode (mode `packet-up`, or empty/`auto` without REALITY), on which
`splithttp`'s dialer panics (`scMaxEachPostBytes should be bigger than 8192`). Their names are found
under the same folding Xray applies (below), since a record stored before the importers refused other
spellings can still hold them. Either panic takes Xray down, and every Xray client with it, and only
a dial reaches it: `xray run -test` never dials, so it passes such a config; the selection fails with
the reason instead, and the watch walk moves on to the next server.

Sanitizing keeps routing ours, and it reads the whole outbound, not its top only: an xhttp `extra`
holds whatever the subscription wrote, and Xray reads a whole `downloadSettings` stream config out of
it. Every key the decoders, the sanitizer, the generators and the watch read is matched as Xray
matches it: Xray loads its config with `encoding/json`, which folds case — the long s (U+017F) and
the Kelvin sign (U+212A) included — so a key that folds to one of those names without being it
(`Sockopt`, `MasterKeyLog`, `marK`) would pass every exact lookup and still reach Xray. Such a key
makes the entry `invalid` (`key "Sockopt" is spelled "sockopt"`; with several, the first in byte
order), except under `headers`, which Xray reads as a map: a header's name is the panel's to spell.
The names are `guardedKeys` in Go and `guard` in the shell, and they grow with whatever a consumer
starts reading. Values are read as Xray reads them too: an Xray JSON entry's `protocol`, and every
`security` and `network` in its proxy outbound (`headers` excepted), are stored lowercased, since
Xray lowercases all three before it looks — so `TLS` is a tls stream to the sanitizer, the label,
`tcpChecked` and `keepHostname` alike. Share links already refuse another spelling of their `type`
and `security` as `unsupported`; the `extra` an xhttp link carries is Xray's JSON, and its
`security` and `network` values are lowercased the same way before any check reads them, so a
`TLS` download stream there loses its `allowInsecure` as a `tls` one does.
A stored outbound carries no `tag` and no `sendThrough`, and no `sockopt` in it keeps `mark`,
`interface`, `tproxy` or `customSockopt` — a foreign fwmark could collide with ours (0x100 Xray,
0x01 firmware VPN, 0x00ff0000 Tunnel Director), and an interface would route around the WAN; a
`sockopt` left empty goes too. `tlsSettings.echSockopt`, the socket the ECH config query is dialed
on and the only other SocketConfig Xray reads, gets the same. A `dialerProxy` in a `sockopt` or an
`echSockopt` anywhere makes the entry `composite` (`chained`), as one at the top always did. No
`tlsSettings` or `realitySettings` anywhere keeps a `masterKeyLog`, whatever the stream's security:
Xray opens that path to append every session's keys to, creating it 0644 (`GetTLSConfig` in
26.2.6, REALITY alike) — a file on the router the subscription picks, and traffic anyone who reads
it can decrypt. A tls stream anywhere loses its `allowInsecure` whatever
the value is, and the entry is `unsupported` (`insecure TLS`) when that value was the boolean `true`
and no `pinnedPeerCertSha256` replaces it — Xray has loaded no config with the flag since
2026-06-01, and it reads the field as a bool, so a panel's `false` is noise while its `"true"` would
make Xray refuse the config outright. Only a tls stream is read for it: Xray ignores `tlsSettings`
under any other security, and 26.2.6 loads a stray flag there without complaint.

A record without `outbound` predates stored outbounds: the generators build a VLESS outbound from
its flat fields — `security` (`reality`|`tls`), `network`, `flow`, `sni`, `fingerprint`,
`public_key`, `short_id`, `alpn`:

- `security=reality` -> `realitySettings { serverName, fingerprint, publicKey, shortId }`, user `flow`.
- `security=tls` -> `tlsSettings { serverName (sni||address), fingerprint?, alpn? }`.
- empty `security` -> legacy `tlsSettings { alpn:["h2"], serverName:address }`, no flow.

The next import rewrites such records; nothing converts them. A link that names no `security` then
stores `none` — how the share-link standard reads it, and every other client with it — where the
legacy record dialed TLS; a server that wants TLS says so in its link.

**Every config is tested before it replaces the live one:** `xray run -test -format json -c <temp>`
(`xrayconf_validate` in shell, `xrayTest` in Go). An outbound from a subscription can name a protocol
the installed Xray lacks, or a key it refuses — since 2026-06-01 Xray loads no config with
`tlsSettings.allowInsecure: true` — and a config Xray rejects would take every Xray client, and the
bot, offline at the next restart. The temp file (`config.json.XXXXXX`) does not end in `.json`, which
keeps `xray -confdir` from loading it and is why `-format json` is needed. Without an `xray` binary
(dev mode, a workstation) the test is skipped. The test runs under the config lock and is bounded at
15 s, half of the 30 s every other writer waits for that lock: a hung `xray` lets them take their
turn instead of using up the whole wait, and the error says the test timed out rather than that Xray
rejected the config. The shell probes which `timeout` the router has — BusyBox
before 1.30 takes the seconds only after `-t` — and runs the test unbounded when it can drive
neither form.

## Configuration

In `vpn-director.json`:

| JSON Path | Default | Purpose |
|-----------|---------|---------|
| `xray.clients` | `[]` | LAN IPs/CIDRs to proxy (JSON array) |
| `xray.servers` | `[]` | Xray server IPs to exclude (avoid loops): every address of every subscription, recomputed whenever a list is added, refreshed or deleted |
| `xray.exclude_sets` | `[]` | Country codes/ipsets to skip |
| `advanced.xray.tproxy_port` | `12345` | Xray dokodemo-door port |
| `advanced.xray.route_table` | `100` | ip route table number |
| `advanced.xray.rule_pref` | `200` | ip rule priority. Below the direct rule (`pref_base + 2 × field_max`, 16894 with the defaults; `lib/guard.sh`): a packet TPROXY takes carries the direct mark too, and meeting the direct rule first it would go to main and be dropped. `guard_layout_check` refuses a value at or above that preference with an ERROR |
| `advanced.xray.fwmark` | `0x100` | Routing fwmark (bit 8) |
| `advanced.xray.fwmark_mask` | `0x100` | Fwmark mask |
| `advanced.xray.chain` | `XRAY_TPROXY` | mangle chain name |
| `advanced.xray.clients_ipset` | `XRAY_CLIENTS` | Source clients ipset |
| `advanced.xray.bypass_ipset` | `TPROXY_BYPASS` | Bypass ipset (servers + user excludes + OpenVPN endpoints) |

`advanced.xray.tproxy_port` and `socks_port` are not just read by the firewall
rules: both generators write them into `config.json` — `xrayconf_generate` takes
them as its second and third arguments, `XrayService.GenerateConfig` as
`InboundPorts` — so the dokodemo-door inbound listens where the TPROXY rules
send traffic. Regenerating `config.json` from the template alone would silently
put the inbound back on 12345.

## IPSets Created

| Name | Type | Purpose |
|------|------|---------|
| `XRAY_CLIENTS_IPSET` | `hash:net` | Source clients |
| `TPROXY_BYPASS` | `hash:net` | Bypass set (servers + user excludes + OpenVPN endpoints) |

## Chain Structure

```
PREROUTING (pos 1)
    └─→ XRAY_TPROXY
          ├─ ! src clients → RETURN
          ├─ → MARK 0x200 (direct)
          ├─ → PPE                     (Keenetic only)
          ├─ dst servers → RETURN
          ├─ dst 127.0.0.0/8 → RETURN
          ├─ dst 10.0.0.0/8 → RETURN
          ├─ dst 172.16.0.0/12 → RETURN
          ├─ dst 192.168.0.0/16 → RETURN
          ├─ dst 169.254.0.0/16 → RETURN
          ├─ dst 224.0.0.0/4 → RETURN
          ├─ dst 255.255.255.255 → RETURN
          ├─ dst {exclude_sets} → RETURN
          ├─ tcp → TPROXY :port mark
          └─ udp → TPROXY :port mark

mangle INPUT (Keenetic, pos 1):
    -m mark --mark <fwmark> → ACCEPT
    (so TPROXY-marked HTTPS never reaches _NDM_HTTP_INPUT_TLS_)
```

Every packet of an Xray client gets the direct mark (`advanced.tunnel_director.direct_mark`,
`0x200`; `lib/guard.sh`) right after rule 1. What the RETURNs let go leaves the chain with it, and
so does a protocol TPROXY does not take, such as ICMP. The direct rule at 16894,
`fwmark 0x200/0x200 lookup main`, sends it to main ahead of the guards, which would refuse it. A
packet TPROXY takes gets Xray's mark beside it and meets the rule at 200 first. `TUN_DIR` tests
its own field alone, so what Xray lets through of a client that is on a tunnel too is still
marked for the tunnel, whose lookup rule comes before the direct rule. `tproxy_apply` puts the
direct rule in (`guard_direct_ensure`) before the chain, and leaves one in place alone.

The platform's offload opt-out follows: `platform_tunnel_offload_target` prints `PPE` on Keenetic
(nothing, rc 1 on Merlin, where the rule does not exist). The fast path routes a flow it took over
by the ip rules alone, never by a mark, so an accelerated flow that Xray lets through would meet
the guards after its first packets (`packet-flow.md`, "The firmware fast path"). A flow TPROXY
takes ends in a local socket, and no fast path takes it.

## Routing Setup

```bash
# Route table (for TPROXY to work)
ip route add local default dev lo table $XRAY_ROUTE_TABLE

# ip rule (fwmark-based lookup)
ip rule add pref $XRAY_RULE_PREF fwmark $XRAY_FWMARK/$XRAY_FWMARK_MASK table $XRAY_ROUTE_TABLE

# The direct rule, Tunnel Director's as well (guard_direct_ensure, lib/guard.sh)
ip rule add pref 16894 fwmark 0x200/0x200 lookup main

# The guards of every client XRAY_CLIENTS holds, per LAN interface, at 16897 or 16898
# (guard_sync, lib/guard.sh; see "Fail-Safe"): its direct destinations, then the refusal
ip rule add pref 16897 from 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main
ip rule add pref 16897 from 192.168.1.100 iif br0 prohibit
```

**Fwmark bit allocation**: `0x100` (bit 8) is free from firmware VPN marking (bit 0), Tunnel
Director (bits 16-23) and the direct mark (bit 9 by default), which stays out of `fwmark_mask`.
Without `advanced.tunnel_director.direct_mark`, a `fwmark_mask` that holds bit 9 (`0x300`,
`0xff00`) moves the direct mark to the lowest bit above it outside both masks, with a WARN;
`guard_layout_check` refuses a `direct_mark` that is set there, and one set in the firmware's mark
byte, bits 0-7 (`lib/guard.sh`).

The TPROXY ip rule uses pref 200. On KeeneticOS 5.1.5 that preference was free of
NDM connection policies (a user policy landed at prefs 102/103, table 4097; no
tables in the 40s). `_tproxy_setup_routing` still only removes rules carrying our
mark or our table, because other firmware versions may still park policies at 200.

## Requirements

- `xt_TPROXY` kernel module
- Xray running with dokodemo-door inbound (tproxy mode)
- ipsets from `lib/ipset.sh` (for country exclusions)

## Fail-Safe

`tproxy_apply` sets up no rule, no route and no bypass set if:
- Required exclusion ipsets not found
- xt_TPROXY module unavailable

It still adds every effective client to an `XRAY_CLIENTS` that exists: the rules of an earlier apply
may still intercept with it, and a client moving from a tunnel to Xray has to be in it before
`tunnel_apply` takes its mark away. While the live chain marks direct, the clients get their guards
there as well (below): such a client loses its tunnel guard in `tunnel_apply`.

> An unknown country code in `xray.exclude_sets` is **not** one of those
> reasons: `_tproxy_exclude_sets` drops it with a WARN before the check, so a
> typo cannot abort apply.

A direct mark, guard preferences or a `rule_pref` that `guard_layout_check` refuses (`lib/guard.sh`:
a `direct_mark` that is not one bit, or lies in the firmware's mark byte (`0xff`), in `fwmark_mask`
or in Tunnel Director's `mark_mask`; without the key, masks that leave no bit free; guards that
would reach main's rule at 32766; a `rule_pref` at or above the direct rule's preference) are no
soft-fail: `tproxy_apply` logs the ERROR and returns 1 before it touches anything - no module,
route, rule, set, chain or marker - and the run ends there, as `tunnel_apply` would end it.

The firmware's firewall rebuilds take `XRAY_TPROXY` away: KeeneticOS's NDM deletes it (every WAN
lease renewal did on the KN-4521), Merlin's firewall start empties it (`iptables -t mangle -F`).
Until the hook's apply puts it back, an Xray client's packets are neither proxied nor marked, and
they would reach main and the WAN. ip rules survive both, so every client `XRAY_CLIENTS` holds has
guards (`lib/guard.sh`): ip rules by source and LAN interface (`platform_lan_ifaces`; `iif` takes
one name, so each interface has its own) at Xray's guard preferences, 16897-16898, after Tunnel
Director's 16895-16896 and the direct rule at 16894. Xray cannot be reached without the chain, so
they refuse a client's packets, but for the private ranges and its address exclusions, which go
direct. The fixture's client, with `exclude_ips` `5.6.7.8` and `10.20.0.0/16`:

```
16897: from 192.168.1.100 to 10.0.0.0/8 iif br0 lookup main     # likewise 172.16.0.0/12,
                                                                # 192.168.0.0/16, 169.254.0.0/16,
                                                                # 224.0.0.0/4
16897: from 192.168.1.100 to 5.6.7.8 iif br0 lookup main        # each address or CIDR
16897: from 192.168.1.100 to 10.20.0.0/16 iif br0 lookup main   # of exclude_ips
16897: from 192.168.1.100 iif br0 prohibit                      # everything else refused
```

A packet the chain marked - for Xray, or direct - meets Xray's rule at 200 or the direct rule
first, so in normal operation none reaches a guard. While the chain is gone, a client's new
connections are refused and its proxied ones stall until the chain is back. A country of
`exclude_sets` is no ip rule (no rule matches a set), and the Xray servers and the firmware's VPN
endpoints in `TPROXY_BYPASS` get no guard either: their traffic is refused as well. A client on
Xray and on a tunnel meets Tunnel Director's guards first, and goes through its tunnel - but for
what Xray lets through between `tproxy_apply` and `tunnel_apply` of a full apply that finds both
chains gone, which the direct rule sends to main (`packet-flow.md`, "Left open"). `iif` keeps the
router's own traffic out - a client CIDR may hold the router's LAN address - and the local table,
at preference 0, answers traffic to the router before any guard. An `exclude_ips` entry
gets its guard only where ip and ipset read it as written (`is_ipv4_net`) and it is no `/0`, which
no `hash:net` set takes: IPv6, a leading zero (ipset refuses it, ip reads `010` as 8) or a `/0`
gets none, and costs the apply nothing.

`_tproxy_guard_lines` takes the members from `ipset list` (`_tproxy_members`), sorted - the kernel
lists them in hash order, which a swap of the set changes - and `_tproxy_guards_sync` hands their
guards to `guard_sync`, which writes the new list at one preference while the old one guards from
the other.
It syncs only while the live chain holds the direct-mark rule (`_tproxy_guards_wanted`, rule 2 in
the exact form `_tproxy_build_chain` writes it): a chain an earlier version left returns bare from
every exclusion, and the guards would refuse what it lets go direct; without a chain - no
xt_TPROXY, the firmware's rebuild - Xray proxies nobody. The guards then stay as they are.
`tproxy_apply` syncs once `_tproxy_setup_iptables` has run, and the soft-fails above sync too. The
set only grows there, so a client that left Xray keeps its guards until `tproxy_prune`, which syncs
after its swap. Where the check finds no direct mark, the prune adds no guard but still drops those
of the clients the swap let go, with a WARN (`_tproxy_guards_prune`): rule 1 returns their packets
unmarked, and their refusal would meet them in normal operation, where they go direct. It hands
`guard_sync` the guards in place of the members the set still holds, so no line goes in that was
not there. A guard the kernel refuses is an ERROR and withholds the ready marker of
`tproxy_apply`, and so does a check that finds no direct mark in the chain it has just swapped in
("Ready marker"); the prune leaves the marker alone, and the old guards of every client the set
still holds stay (`guard_sync`). `tproxy_stop` removes the guards (`guard_clear`), those a former
`pref_base` left too, then the direct rule once neither module needs it (`guard_direct_release`:
not while `TUN_DIR` is in mangle or Tunnel Director has guards). `status xray` lists the direct rule
and the guards.

## Extended Exclusion Sets

Prefers `{set}_ext` variant if exists:
```bash
resolve_exclude_set "<country_code>"  # Returns: <country_code>_ext if exists, else <country_code>
```

## Key Functions

**Public API**:

| Function | Purpose |
|----------|---------|
| `tproxy_status()` | Show XRAY_TPROXY chain, routing, the direct rule and the guards, xray process |
| `tproxy_apply()` | The make half of an apply: routing, sets, the direct rule, chain swapped in, then the guards; clients added, never removed; soft-fail if unavailable; rc 1, nothing touched, for a layout `guard_layout_check` refuses |
| `tproxy_prune([addr...])` | The break half: `XRAY_CLIENTS` swapped to the effective clients (paused ones subtracted, non-IPv4 entries skipped) plus every named address the live set holds; one the new set already matches (a host inside an effective network, `x/32` of an effective `x`) is not kept as an element of its own; the guards follow the set after the swap, and while the chain does not answer for its direct mark only those of the clients it let go are dropped; always returns 0 and leaves the ready marker alone |
| `tproxy_stop()` | Remove chain and routing (an interrupted swap's shadows included) and Xray's guards; the direct rule once neither module needs it |
| `tproxy_restart_process()` | Restart Xray process via Entware init script |
| `tproxy_get_required_ipsets()` | Return list of valid exclude ipsets (unknown codes dropped with a WARN) |

**Internal functions** (for testing):

| Function | Purpose |
|----------|---------|
| `_tproxy_init()` | Initialize module state |
| `_tproxy_check_module()` | Verify/load xt_TPROXY kernel module |
| `_tproxy_check_required_ipsets()` | Fail-safe: exit if exclusion ipsets missing |
| `_tproxy_exclude_sets([-q])` | Validated, lower-cased `XRAY_EXCLUDE_SETS` on one line; `-q` suppresses the WARN per dropped code |
| `_tproxy_resolve_exclude_set(key)` | Try `{set}_ext` first, fall back to `{set}` |
| `_tproxy_setup_routing()` | Create route table + ip rule |
| `_tproxy_teardown_routing()` | Remove route table + ip rule |
| `_tproxy_setup_clients_ipset()` | Create the client ipset and add every effective client (never removes) |
| `_tproxy_add_to_live_clients()` | The same for the soft-fails, only when the client ipset already exists |
| `_tproxy_shadow_set(name)` | An empty `<name>_NEW` to fill and swap in |
| `_tproxy_swap_set(name)` | `ipset swap` of `<name>_NEW` into `<name>`, then destroy the shadow |
| `_tproxy_setup_bypass_ipset()` | Build `TPROXY_BYPASS_NEW` from the three sources and swap it in |
| `_tproxy_validate_ipv4_cidr()` | Validate IPv4 CIDR format |
| `_tproxy_build_chain(chain)` | Fill a fresh chain with the TPROXY rules: 0, 1 (a narrowing RETURN or the offload opt-out missing), 2 (a bounding RETURN, the direct mark or a target missing) |
| `_tproxy_jump_pos()` | Position of the first jump: 1 |
| `_tproxy_setup_iptables()` | Platform rules, then `XRAY_TPROXY` swapped in with its jumps |
| `_tproxy_guards_wanted()` | Does the live `XRAY_TPROXY` hold the direct-mark rule, in the form `_tproxy_build_chain` writes it? No guard is synced without it |
| `_tproxy_members()` | The members of `XRAY_CLIENTS`, `/32` dropped, sorted (the kernel lists hash order); rc 1 with an ERROR when the set cannot be listed |
| `_tproxy_guard_lines()` | The guards of every member of `XRAY_CLIENTS`, sorted, per LAN interface: the private ranges and `exclude_ips` direct, then the refusal; rc 1 when the platform names no LAN interface or the set cannot be listed |
| `_tproxy_guards_sync()` | Make Xray's guards those of the set (`guard_sync`) while the live chain marks direct; rc 2, the guards left as they are, when it does not; rc 1 when one did not go in |
| `_tproxy_guards_prune()` | For the prune while the chain does not answer for its direct mark: hand `guard_sync` the guards in place of the members the set still holds, so those of the clients it let go go and none goes in; nothing when none is to go or `guard_list` prints nothing |
| `_tproxy_teardown_iptables()` | Remove chain and ipsets |

**Soft-fail behavior**: `tproxy_apply()` returns 0 even if xt_TPROXY unavailable or ipsets missing, allowing caller scripts to continue; a live `XRAY_CLIENTS` still gets every effective client, and they get their guards while the live chain marks direct. A layout `guard_layout_check` refuses is the one failure it returns 1 for ("Fail-Safe").

**Ready marker**: `tproxy_apply()` writes `/tmp/xray_tproxy/ready` (`XRAY_TPROXY_READY`) only when
every rule went in, the platform's own included, and removes it on any soft-fail.
watchd's subscription watch waits for it before Xray clients leave the fallback tunnel. A failed
`platform_tproxy_extra_rules apply` (Keenetic's mangle INPUT accept) fails `_tproxy_setup_iptables`
only after the PREROUTING jumps are in place, so interception stays as it was and only the marker
is withheld. Rule 1 (`! XRAY_CLIENTS`), the direct mark, a private-range RETURN or a TPROXY
target that does not go in keeps the rebuilt chain out (`_tproxy_build_chain` returns 2): the
chain already in place keeps working and the marker is withheld. Without rule 1 or the
private-range RETURNs the targets would take every LAN client, or traffic to the router and the
rest of the LAN; without the direct mark what the chain lets through would meet the guards
instead of the direct rule, and Xray's refuse it. Every other RETURN (bypass, loopback,
link-local, multicast, broadcast, an exclusion) only decides what a client reaches directly
instead of through the proxy, and the offload opt-out (`PPE`) only whether the fast path may take
it: its failure is logged, the targets and the jumps still go in, and only the marker is withheld
— ending the setup there left every Xray client on the WAN over one busy xtables lock. A direct
rule that does not go in (`guard_direct_ensure`) withholds the marker too, and the chain and the
guards go in all the same, the side that leaks nothing: until an apply puts the rule in, what the
chain lets through meets the guards and is refused rather than sent direct, all but the private
ranges and `exclude_ips`, which the guards send direct themselves. A PREROUTING jump that does not
go in withholds the marker as well: `swap_fw_chain` returns 3, and that interface stays on the old
chain until the next apply finishes the swap.
`XRAY_CLIENTS` has to be complete too: nothing validates `xray.clients`, and an address the set
does not take is RETURNed by rule 1 and left unproxied, so `_tproxy_setup_clients_ipset` reports it
and the marker is withheld although the chain itself is in place. The adds pass `-exist`, so a
repeated address is not a failure, and an entry that is no IPv4 address or CIDR (`is_ipv4_net`: an
IPv6 address an older Web UI saved, a typo like `192.168.1.1000`) is skipped with a WARN and does
not count — no set takes it, no tunnel carries it, and waiting for it held every restore of the
LAN on the fallback tunnel. Every client the set holds needs its guards as well ("Fail-Safe"): a
guard that does not go in (`guard_sync` logs the ERROR), or a set or LAN interfaces
`_tproxy_guard_lines` cannot read, withholds the marker. So does a check that finds no direct mark
in the chain the apply has just swapped in (`_tproxy_guards_wanted`): the chain was built with that
rule, so the failure is one of the moment - a busy xtables lock - and no guard was synced. The
watch takes the clients off the fallback tunnel on the marker, and one without its guards would
leave through the WAN in the next rebuild of the firewall.
