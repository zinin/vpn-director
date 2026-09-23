---
paths: "router/opt/vpn-director/**"
---

# Xray TPROXY

Transparent proxy routing for LAN clients via Xray.

Module location: `lib/tproxy.sh`

## Usage via CLI

```bash
vpn-director.sh status xray       # Show Xray TPROXY status
vpn-director.sh restart xray      # Restart Xray TPROXY
vpn-director.sh restart xray-process  # Restart the Xray process only, TPROXY rules kept
vpn-director.sh apply             # Apply all (including TPROXY)
```

`restart xray` is the process restart plus a stop and an apply of the TPROXY rules, and between the
two the jump is gone and the Xray clients leave through the WAN. `restart xray-process` restarts the
process alone: a client meets a restarting Xray and waits. It is for a caller whose only change is
`config.json` — the subscription watch trying one server after another.

## How It Works

1. Selected LAN clients → mangle PREROUTING chain
2. Exclude: servers, private IPs, specified countries
3. Remaining traffic → TPROXY to Xray port
4. Xray dokodemo-door inbound → the selected server's outbound (proxy-out; any protocol the import stored)

## Outbound Generation

`config.json.template` is valid JSON with an empty `outbounds: []`. The selected server's
outbound goes into it as `outbounds[0]`, tagged `proxy-out`:

- Shell: `lib/xrayconf.sh` (`xrayconf_generate`, jq) — used by `configure.sh`.
- Go: `service/xray.go` (`serverOutbound` + `encoding/json`) — used by the Web UI, the bot wizard,
  `/xray` and the subscription watch.

An import stores each server's Xray outbound in `servers.json` (`outbound`): converted from a share
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
starts reading.
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
| `xray.servers` | `[]` | Xray server IPs to exclude (avoid loops) |
| `xray.exclude_sets` | `[]` | Country codes/ipsets to skip |
| `advanced.xray.tproxy_port` | `12345` | Xray dokodemo-door port |
| `advanced.xray.route_table` | `100` | ip route table number |
| `advanced.xray.rule_pref` | `200` | ip rule priority |
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

## Routing Setup

```bash
# Route table (for TPROXY to work)
ip route add local default dev lo table $XRAY_ROUTE_TABLE

# ip rule (fwmark-based lookup)
ip rule add pref $XRAY_RULE_PREF fwmark $XRAY_FWMARK/$XRAY_FWMARK_MASK table $XRAY_ROUTE_TABLE
```

**Fwmark bit allocation**: `0x100` (bit 8) is free from firmware VPN marking (bit 0) and Tunnel Director (bits 16-23).

The TPROXY ip rule uses pref 200. On KeeneticOS 5.1.5 that preference was free of
NDM connection policies (a user policy landed at prefs 102/103, table 4097; no
tables in the 40s). `_tproxy_setup_routing` still only removes rules carrying our
mark or our table, because other firmware versions may still park policies at 200.

## Requirements

- `xt_TPROXY` kernel module
- Xray running with dokodemo-door inbound (tproxy mode)
- ipsets from `lib/ipset.sh` (for country exclusions)

## Fail-Safe

Script exits without changes if:
- Required exclusion ipsets not found
- xt_TPROXY module unavailable

> An unknown country code in `xray.exclude_sets` is **not** one of those
> reasons: `_tproxy_exclude_sets` drops it with a WARN before the check, so a
> typo cannot abort apply.

## Extended Exclusion Sets

Prefers `{set}_ext` variant if exists:
```bash
resolve_exclude_set "<country_code>"  # Returns: <country_code>_ext if exists, else <country_code>
```

## Key Functions

**Public API**:

| Function | Purpose |
|----------|---------|
| `tproxy_status()` | Show XRAY_TPROXY chain, routing, xray process |
| `tproxy_apply()` | Apply TPROXY rules (idempotent), soft-fail if unavailable |
| `tproxy_stop()` | Remove chain and routing |
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
| `_tproxy_setup_clients_ipset()` | Create and populate client ipset |
| `_tproxy_setup_bypass_ipset()` | Create and populate bypass ipset |
| `_tproxy_validate_ipv4_cidr()` | Validate IPv4 CIDR format |
| `_tproxy_setup_iptables()` | Build XRAY_TPROXY chain with exclusions |
| `_tproxy_teardown_iptables()` | Remove chain and ipsets |

**Soft-fail behavior**: `tproxy_apply()` returns 0 even if xt_TPROXY unavailable or ipsets missing, allowing caller scripts to continue.

**Ready marker**: `tproxy_apply()` writes `/tmp/xray_tproxy/ready` (`XRAY_TPROXY_READY`) only when
every rule went in, the platform's own included, and removes it on any soft-fail. The Telegram
bot's subscription watch waits for it before Xray clients leave the fallback tunnel. A failed
`platform_tproxy_extra_rules apply` (Keenetic's mangle INPUT accept) fails `_tproxy_setup_iptables`
only after the PREROUTING jumps are in place, so interception stays as it was and only the marker
is withheld. The chain flush, rule 1 (`! XRAY_CLIENTS`) or a private-range RETURN that does not go
in ends it before the TPROXY targets instead, on a flushed chain that intercepts nothing: without
those rules the targets would take every LAN client, or traffic to the router and the rest of the
LAN. Every other RETURN (bypass, loopback, link-local, multicast, broadcast, an exclusion) only
decides what a client reaches directly instead of through the proxy: its failure is logged, the
targets and the jumps still go in, and only the marker is withheld — ending the setup there left
every Xray client on the WAN over one busy xtables lock. A PREROUTING jump that does not go in
withholds the marker as well: `sync_fw_rule` returns 1 for an insert the kernel refused.
`XRAY_CLIENTS` has to be complete too: nothing validates `xray.clients`, and an address the set
does not take is RETURNed by rule 1 and left unproxied, so `_tproxy_setup_clients_ipset` reports it
and the marker is withheld although the chain itself is in place. The adds pass `-exist`, so a
repeated address is not a failure, and an entry that is no IPv4 address or CIDR (`is_ipv4_net`: an
IPv6 address an older Web UI saved, a typo like `192.168.1.1000`) is skipped with a WARN and does
not count — no set takes it, no tunnel carries it, and waiting for it held every restore of the
LAN on the fallback tunnel.
