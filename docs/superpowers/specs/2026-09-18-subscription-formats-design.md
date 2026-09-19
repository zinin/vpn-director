# Subscription formats: design

Date: 2026-09-18
Branch: `feature/subscription-formats`
Status: approved in brainstorming, awaiting implementation plan

## 1. Goal

VPN Director imports only `vless://` links, and generates only VLESS over TCP
with TLS or REALITY. The author's new provider serves no links at all: its
subscription is an Xray JSON array of complete client configs, with Hysteria2
and Shadowsocks servers beside the VLESS ones.

This change imports the standard formats an Xray client meets:

- share links, base64-encoded or plain: `vless://`, `vmess://`, `trojan://`,
  `ss://`, `hysteria2://` and `hy2://`;
- Xray JSON: an array of Xray client configs, or a single config.

Every protocol the router's Xray can run from those formats works end to end —
import, selection, the subscription watch, the Web UI and the bot: VLESS over
the common transports, VMess, Trojan, Shadowsocks and Hysteria2. The shell
importer (`import_server_list.sh`) and the Go importer (bot, Web UI, watch)
stay at full parity.

## 2. What the two subscriptions serve

Measured on 2026-09-18 with several User-Agent strings.

**Old provider:** a base64 list of 62 `vless://` links — 56 REALITY over TCP
with Vision, 6 WebSocket with TLS. The generators reject `network=ws` today,
so those six import but cannot be selected. `clash-verge` gets Clash YAML.

**New provider:** no links. The panel picks the format by User-Agent:

| User-Agent | Body |
|---|---|
| curl, Go-http-client, v2rayN, v2rayNG, Happ, Streisand, any unknown one | Xray JSON: an array of 40 configs, each with `remarks` |
| clash-verge, mihomo, Clash.Meta | Clash YAML |
| sing-box, Hiddify, Karing | sing-box JSON |
| a browser | an HTML page |

Paths such as `/v2ray` and `/base64` return 404. The Xray JSON holds:

- 26 VLESS REALITY over TCP with Vision, one outbound each; one of them has
  only an IPv6 address;
- 4 Hysteria2 (`protocol: "hysteria"`, with `finalmask.quicParams`) and
  3 Shadowsocks (`chacha20-ietf-poly1305`);
- 1 "Auto" entry: a balancer over 12 VLESS outbounds with `burstObservatory`;
- 6 "whitelist bypass" entries: two proxy outbounds each (a decoy and a relay,
  some over xhttp) under a `leastLoad` balancer.

Entry order and REALITY `shortId` values change between requests. The Clash
and sing-box variants carry fewer of the same servers (Clash has no
Shadowsocks).

Xray facts behind the design. Entware ships xray-core 26.2.6; each fact below
was checked in the 26.2.6 and 26.3.27 sources.

- The Hysteria2 outbound exists since 26.1.23: `protocol: "hysteria"`,
  `version: 2`.
- Since 2026-06-01 Xray refuses to load a config with
  `tlsSettings.allowInsecure: true`, so the whole process fails to start.
  `pinnedPeerCertSha256` (hex, colons allowed) replaces it.
- Hysteria2 port hopping moved between versions
  (`hysteriaSettings.udphop.port` in 26.2.6,
  `finalmask.quicParams.udpHop.ports` in 26.3). No single config works on both.
- VLESS, VMess, Trojan and Shadowsocks outbounds accept `vnext` / `servers`
  arrays or a flat `settings.address` and `settings.port`.
- `xray run -test -format json -c <file>` checks a config without starting a
  server. A bad config exits with 23.
- `S24xray` runs `xray run -confdir /opt/etc/xray`, which loads only `*.json`.
  `xray run -test -c` on a file whose name does not end in `.json` fails with
  "Failed to get format" unless `-format json` is given.
- Xray 26.2.6 panics on a malformed VLESS `encryption` value; `-test` exits
  non-zero on it like on any other bad config.
- A missing Host of ws and httpupgrade falls back to the TLS server name, then
  to the dialed address; xhttp tries the REALITY server name before the
  address.
- Entware's jq is built without oniguruma: `test`, `match`, `capture`, `scan`,
  `splits`, `sub`, `gsub` and the two-argument `split` do not exist there.

## 3. Decisions

| Question | Decision |
|---|---|
| Formats | Share links (base64 or plain) and Xray JSON. No Clash, sing-box or SIP008. |
| Protocols | VLESS, VMess, Trojan, Shadowsocks and Hysteria2, in links and in JSON. |
| Shell importer | Full parity with Go, proven by shared fixtures. |
| Composite JSON entries (balancers, chains) | Skipped and counted. |
| Storage | Each server stores a ready Xray outbound. |
| Generators | Insert the stored outbound; the legacy builder serves only records without one. |
| Config safety | `xray run -test` before every replacement of config.json. |

Two alternatives were rejected:

- **A wider flat record.** Every protocol and transport would live in two
  importers and two generators, and Xray JSON would be flattened with loss:
  whatever the record does not model, such as xhttp `extra` or `finalmask`,
  would silently disappear.
- **Storing the source link or JSON element.** Parsing would move into both
  generators and the watch, and a parse error would surface at selection
  instead of import.

## 4. The servers.json record

```json
{
  "name": "Switzerland-1",
  "address": "198.51.100.10",
  "port": 8443,
  "ips": ["198.51.100.10"],
  "outbound": {
    "protocol": "vless",
    "settings": {"vnext": [{"address": "198.51.100.10", "port": 8443,
      "users": [{"id": "…", "encryption": "none", "flow": "xtls-rprx-vision"}]}]},
    "streamSettings": {"network": "tcp", "security": "reality",
      "realitySettings": {"serverName": "…", "fingerprint": "firefox",
        "publicKey": "…", "shortId": "…"}}
  }
}
```

- `name`, `address` and `port` identify the server, as today:
  `xray.active_server`, the `/xray` button fingerprint, the wizard and the
  Web UI compare these three. `address` and `port` are the outbound's own;
  section 6.5, step 6, names where each protocol keeps them.
- Resolution fills `ips`, as today, and `ips` feeds `xray.servers`
  (TPROXY_BYPASS).
- `outbound` never carries `tag`.
- Importers stop writing the legacy flat fields (`uuid`, `security`,
  `network`, `flow`, `sni`, `fingerprint`, `public_key`, `short_id`, `alpn`).
  Readers still accept them on a record without `outbound` (section 12).
- `vpnconfig.Server` gains `Outbound json.RawMessage`
  (`json:"outbound,omitempty"`), and its `uuid` tag gets `omitempty`, so a new
  record carries no empty `uuid`. A new `vpnconfig/outbound.go` holds
  `DecodeOutbound` (numbers kept as written), `OutboundTarget` (the address
  slot of 6.5, step 6) and `Server.Label` (section 10).

## 5. Components

**Go.** Package `server/internal/subscription` replaces
`server/internal/vless`.

| File | Content |
|---|---|
| `subscription.go` | `Result`, `Skip`, the reasons, the errors of 6.1 |
| `decode.go` | `Decode(body string) (Result, error)`: container detection |
| `text.go` | percent-decoding, base64, JSON reading, `prune` |
| `links.go` | the link list, share-link grammar, query decoding, stream settings shared by vless, trojan and vmess |
| `vless.go`, `vmess.go`, `trojan.go`, `shadowsocks.go`, `hysteria2.go` | one converter per scheme |
| `xrayjson.go` | proxy selection, checks and sanitization of Xray JSON entries |
| `names.go` | `cleanName` and the placeholder test |
| `resolve.go` | `LookupIPv4`, `DecodeAndResolve`, `DecodeAndResolveLookup`, moved from `vless` |
| `summary.go` | `Import.Counts`, `Details`, `Summary`, `NoServers`, `SkippedByReason` (section 11) |

- `Result` is `{Total int; Servers []vpnconfig.Server; Skipped []Skip}`, with
  `IPs` still empty. `Total` counts entries: link lines or JSON configs.
- `Skip` is `{Name, Reason, Detail string}`.
- `Decode` returns an error only for a body it cannot read at all: the three
  errors of 6.1. A readable body whose entries were all skipped is a `Result`
  with no servers, and the caller reports it (section 11).
- `Import`, the result after resolution, is
  `{Servers, Total, Parsed, Skipped, ResolveErrors}`, where
  `Parsed = len(Result.Servers)`. `DecodeAndResolve` and
  `DecodeAndResolveLookup` return `(Import, error)` and pass `Decode`'s error
  through.

Three files import `vless` today and switch to `subscription`:
`bot/subfetch.go`, `handler/import.go` and `webapi/handler_servers.go`.

**Shell.** A new `router/opt/vpn-director/lib/subscription.sh` enters
`files.manifest` as `common`. `subscription_decode` reads a body on stdin and
prints the same `Result` as JSON:
`{"total": N, "servers": [...], "skipped": [...]}`. For a body it cannot read
it prints the 6.1 error on stderr and returns 1. It needs only the tools
the routers already have: bash, jq, gawk and base64. bash applies the grammar
and the checks, gawk cleans every name in one pass, and jq builds the JSON:
one call per link, a single program for the whole Xray JSON body, and one for
the result.

`import_server_list.sh` sources the library and drops its own parser. It keeps
input, download, resolution and publication under the config lock.

## 6. Decoding

### 6.1 Container detection

1. Drop every NUL byte (bash cannot hold one) and a leading UTF-8 BOM, and
   trim surrounding ASCII whitespace. An empty body is the error
   `empty subscription`.
2. A body starting with `[` or `{` is Xray JSON (6.5). Invalid JSON is the
   error `invalid JSON subscription`.
3. Otherwise a body containing `://` is a plain link list.
4. Otherwise remove all whitespace and decode base64: `-` and `_` map to `+`
   and `/`, so either alphabet - even both mixed - decodes, padded or not.
   Failure is the error `unrecognized subscription format`. The decoded text,
   its NUL bytes dropped, is a plain link list. The same base64 rule decodes
   an ss userinfo and a v2rayN vmess link (6.4).

In a link list each line is trimmed, a trailing CR included. A line is an
entry when it starts with `<scheme>://`, where the scheme matches
`[A-Za-z][A-Za-z0-9+.-]*`. Other lines — blank ones, comments, notices — are
ignored and not counted. A body that yields no entry at all, as a link list or
as JSON, is the error `unrecognized subscription format`; a Clash YAML or an
HTML page ends here.

Schemes dispatch case-insensitively to `vless`, `vmess`, `trojan`, `ss`,
`hysteria2` and `hy2`. Any other scheme is skipped as `unsupported`, with the
scheme as the detail.

Within an entry, checks run in the order sections 6.2 to 6.6 list them, and
the first failing check decides the reason. The fixtures compare reasons, so
both implementations keep this order.

### 6.2 Share-link grammar

For `vless`, `trojan`, `hysteria2` / `hy2` and the URL form of `vmess`:

```
scheme://userinfo@host[:port][/][?query][#fragment]
```

- The fragment starts at the first `#`. The authority ends at the first `/`,
  `?` or `#` after `://`.
- Userinfo ends at the last `@` of the authority and is percent-decoded with
  `+` kept: a trojan password or a hysteria2 auth may hold one.
- The host is `[IPv6]`, a name or an IPv4 literal; the stored address drops
  the brackets. The port follows the last `:` and must be decimal, 1–65535.
  A missing port makes the entry `invalid`, except in hysteria2 (6.4).
- A missing or empty userinfo makes the entry `invalid`.
- The query is `&`-separated `key=value` pairs. Keys and values are
  percent-decoded with `+` read as a space. A malformed `%` escape, or `%00`,
  makes the entry `invalid` in both implementations; the shell's lenient
  decoding goes. The first occurrence of a repeated key wins; an empty key and
  the keys no converter reads are ignored.
- A port is one to five decimal digits, 1–65535.
- The fragment - the name - is decoded leniently: a valid escape becomes its
  byte, `%00` becomes nothing, anything else stays as written, `+` is a
  space.

### 6.3 Stream settings (vless, trojan, vmess)

The parameters follow the XTLS share-link standard.

- `type` becomes `network`: `tcp` (also `raw`), `ws` (also `websocket`),
  `grpc`, `httpupgrade`, `xhttp` (also `splithttp`). Converters emit the
  first spelling of each. The default is `tcp`. Any other value (`kcp`,
  `http`, `h2`, `quic` and so on) is `unsupported`, detail
  `transport <type>`.
- `headerType` with `tcp` must be empty or `none`; anything else is
  `unsupported`.
- `security` is `none` by default (`tls` for trojan), `tls` or `reality`;
  anything else is `unsupported`.
- `security=tls` builds `tlsSettings`: `sni` → `serverName`,
  `fp` → `fingerprint`, `alpn` (a comma list) → `alpn`,
  `pcs` → `pinnedPeerCertSha256`, `vcn` → `verifyPeerCertByName`.
- `security=reality` builds `realitySettings`: `sni` → `serverName`,
  `fp` → `fingerprint`, `pbk` → `publicKey`, `sid` → `shortId`,
  `spx` → `spiderX`, `pqv` → `mldsa65Verify`. An empty `pbk`, `sni` or `fp`
  makes the entry `invalid`.
- With `security=tls`, `allowInsecure` or `insecure` set to `1` or `true`:
  with `pcs` the flag is dropped; without `pcs` the entry is `unsupported`,
  detail `insecure TLS`. Xray no longer loads `allowInsecure`. Other
  securities never read the flag.
- `ws` → `wsSettings {path, host}`.
- `httpupgrade` → `httpupgradeSettings {path, host}`.
- `grpc` → `grpcSettings {serviceName, authority, multiMode}`, where
  `multiMode` is `true` for `mode=multi`.
- `xhttp` → `xhttpSettings {path, host, mode, extra}`. `extra` must parse as
  a JSON object; anything else makes the entry `invalid`.

A converter builds the whole outbound, then removes every string, array and
object left empty, recursively. `network` and `security` are never empty, so
they stay. This rule applies to converted links only; Xray JSON passes through
as described in 6.5.

### 6.4 Schemes

**vless.** Userinfo is the id and is required.

```json
{"protocol": "vless",
 "settings": {"vnext": [{"address": "…", "port": 443,
   "users": [{"id": "…", "encryption": "none", "flow": "…"}]}]},
 "streamSettings": {…}}
```

`encryption` is the query value, or `none`; `flow` is taken as given.

**trojan.** Userinfo is the password and is required.

```json
{"protocol": "trojan",
 "settings": {"servers": [{"address": "…", "port": 443, "password": "…"}]},
 "streamSettings": {…}}
```

**vmess.** Two forms:

- The URL form, recognized by an `@` in the authority. Userinfo is the id,
  and `security` is the `encryption` parameter or `auto`. Stream settings
  come from the query.
- The v2rayN form. The rest of the link, up to `#`, is base64 (either
  alphabet, padding optional) of a JSON object:

  | Field | Meaning |
  |---|---|
  | `add`, `port` | address; the port may be a number or a numeric string |
  | `id`, `scy` | user id; `security`, default `auto` |
  | `net` | the `type` parameter |
  | `type` | `headerType` for tcp; `mode` for grpc and xhttp |
  | `host`, `path` | as in 6.3; for grpc `path` is the `serviceName` |
  | `tls` | `tls` or `reality` becomes `security`; anything else is `none` |
  | `sni`, `alpn`, `fp`, `pbk`, `sid`, `spx` | as in 6.3 |
  | `ps` | the name |

  A field that is a number counts as its decimal text; one that is neither a
  string nor a number is empty; a used field holding a NUL makes the entry
  `invalid`. `ps` is taken as written, not percent-decoded. These fields feed
  6.3 as if they were query parameters. `aid` is ignored: Xray speaks VMess
  AEAD only.
- A link that fits neither form is `invalid`.

Both forms produce:

```json
{"protocol": "vmess",
 "settings": {"vnext": [{"address": "…", "port": 443,
   "users": [{"id": "…", "security": "auto"}]}]},
 "streamSettings": {…}}
```

**ss.**

- SIP002: `ss://userinfo@host:port[/][?plugin=…][#name]`. Base64 may hold a
  `/`, so the userinfo is all of the part before `?` up to its last `@`, and
  only the host part ends at a `/`. Userinfo is percent-decoded with `+` kept.
  If it contains `:`, it is `method:password` in plain text, the SS-2022
  convention. Otherwise it is base64 of `method:password`, trimmed of ASCII
  whitespace after decoding.
- Legacy: `ss://base64(method:password@host:port)[#name]`: all of the part
  before `?` is base64. The decoded body, trimmed of ASCII whitespace, splits
  at its last `@`.
- `method` and `password` split at the first `:`, because a Shadowsocks 2022
  password may itself contain one.
- A non-empty `plugin` is `unsupported`: Xray has no SIP003 plugins.
- The method, lowercased, must be one Xray 26.2.6 accepts: `aes-128-gcm`,
  `aes-256-gcm`, `chacha20-poly1305`, `chacha20-ietf-poly1305`,
  `xchacha20-poly1305`, `xchacha20-ietf-poly1305`, `2022-blake3-aes-128-gcm`,
  `2022-blake3-aes-256-gcm`, `2022-blake3-chacha20-poly1305`, `none`,
  `plain`. Any other method is `unsupported`.

```json
{"protocol": "shadowsocks",
 "settings": {"servers": [{"address": "…", "port": 8388,
   "method": "…", "password": "…"}]}}
```

**hysteria2 / hy2.**

- Userinfo is the auth string and is required.
- The port defaults to 443. A port list or range (`443,5000-6000`) is
  `unsupported`, detail `port hopping`; this test runs before the 6.2 port
  rule.
- Query: `sni`; `alpn`, default `h3`; `obfs`, where only `salamander` is
  accepted and it requires `obfs-password` (another value is `unsupported`);
  `insecure`, under the 6.3 rule with `pinSHA256` as the pin;
  `pinSHA256` → `pinnedPeerCertSha256`.
- `mport` and other keys are ignored. The link connects to its base port.

```json
{"protocol": "hysteria",
 "settings": {"version": 2, "address": "…", "port": 443},
 "streamSettings": {"network": "hysteria", "security": "tls",
   "hysteriaSettings": {"version": 2, "auth": "…"},
   "tlsSettings": {"serverName": "…", "alpn": ["h3"],
     "pinnedPeerCertSha256": "…"},
   "finalmask": {"udp": [{"type": "salamander",
     "settings": {"password": "…"}}]}}}
```

### 6.5 Xray JSON

A top-level array lists the entries; a top-level object with an `outbounds`
array is a single entry. Any other JSON is the error
`unrecognized subscription format`.

For each entry:

1. An entry that is not an object with an `outbounds` array is `invalid`.
2. Proxy outbounds are the outbounds that have a `protocol` other than
   `freedom`, `blackhole`, `dns` and `loopback`. An outbound without
   `protocol`, such as a sing-box one, is not a proxy.
3. An entry without a proxy outbound is `unsupported`, detail
   `no proxy outbound`. An entry with more than one is `composite`.
4. The proxy's protocol must be `vless`, `vmess`, `trojan`, `shadowsocks` or
   `hysteria`; any other is `unsupported`.
5. A `proxySettings` with a non-empty `tag`, a non-empty
   `streamSettings.sockopt.dialerProxy`, or more than one element in
   `settings.vnext` or `settings.servers` makes the entry `composite`.
6. The address and port come from `settings.vnext[0]` (vless, vmess) or
   `settings.servers[0]` (trojan, shadowsocks). Without that array they come
   from `settings` itself, the flat form. Hysteria always uses `settings`. A
   missing address, or a port outside 1–65535, makes the entry `invalid`.
7. With `security: "tls"`, `tlsSettings.allowInsecure: true`: with a
   non-empty `pinnedPeerCertSha256`, the flag is deleted; without one, the
   entry is `unsupported`, detail `insecure TLS`. Xray reads `tlsSettings`
   only for tls, so the other securities are left alone.
8. `security: "reality"` needs a non-empty `publicKey` (or `password`),
   `serverName` and `fingerprint` in `realitySettings`. Without them the entry
   is `invalid`.
9. Sanitization deletes `tag` and `sendThrough`, and from
   `streamSettings.sockopt`: `mark`, `interface`, `tproxy` and
   `customSockopt`. `sockopt` goes too once it is empty. A foreign `mark`
   could collide with our fwmarks (0x100 Xray, 0x01 firmware VPN, 0x00ff0000
   Tunnel Director), and `interface` could route around the WAN. Everything
   else passes through untouched.
10. A placeholder address (6.6) is skipped as `placeholder`.

A top-level array without entries is the error
`unrecognized subscription format`, as 6.1 says. The name comes from
`remarks`. The entry's `dns`, `routing`, `inbounds`,
`log`, `observatory` and `burstObservatory` are ignored: routing is VPN
Director's own.

### 6.6 Names, addresses, placeholders

- The name comes from the fragment (links), `ps` (v2rayN vmess) or `remarks`
  (JSON). `cleanName` then:
  - keeps ASCII letters, digits, the space and `.,;:!?()-`;
  - keeps every two-byte UTF-8 character (U+0080–U+07FF: Cyrillic, Greek,
    accented Latin);
  - drops the rest, emoji and other symbols included;
  - collapses runs of spaces and trims spaces and commas at both ends.

  The filter walks bytes, not characters, in both implementations: a lead
  byte C2–DF followed by a continuation byte (80–BF) is kept with it; a lead
  byte E0–EF followed by two, or F0–F4 followed by three, is dropped with
  them; any other byte that is not an allowed ASCII character is dropped
  alone. So the two agree on invalid UTF-8 too. Control characters in a JSON
  `remarks` drop out like any other disallowed byte.

  An empty result falls back to the address. Two small parity fixes come with
  this: the Go side widens from Cyrillic only to the whole two-byte range the
  shell already keeps, and the gawk filter follows the byte rule above.
- A placeholder is an IP literal written just so: four dot-separated groups
  of one to three digits, each at most 255, the first 0 or 127; or `::`, or
  `::1`. The test is textual, the same on both sides. It is skipped as
  `placeholder`; this is how panels show "subscription expired". The test runs
  last, on an entry that passed every other check.
- Resolution stays IPv4-only. An IPv6-only server fails resolution and counts
  as a DNS error.

### 6.7 Skip reasons

The reasons are `unsupported`, `composite`, `invalid` and `placeholder`. Each
skip carries the cleaned name, or `#<n>` (the entry's 1-based position) when
there is none, and a short detail. DNS failures are counted after decoding,
as today.

## 7. Generation

- Go `serverOutbound` (in `service/xray.go`): a server with `Outbound` yields
  that object plus `"tag": "proxy-out"`, its numbers as written. Without it,
  today's `buildOutbound` and `validateStreamParams` run unchanged.
- Shell `xrayconf_build_outbound`: `.outbound + {tag: "proxy-out"}` when the
  record has one; the current jq program otherwise.
- `config.json.template`, the inbound ports and the atomic write stay as they
  are.

## 8. Validating config.json

Xray tests every config.json before it replaces the live one.

- **Go.** `XrayService.GenerateConfig` writes the temp file (`config.json.*`)
  and runs `xray run -test -format json -c <temp>` with a 30-second timeout.
  It renames only on exit 0. Otherwise it removes the temp file and returns
  `xray rejected the config: <the last three non-empty lines of its output>`,
  and config.json stays untouched.
- **Where Go finds xray.** Through PATH: both daemons' init scripts put
  `/opt/sbin` first. Without a binary the check is skipped and logged at debug
  level. The validator is a field of `XrayService`, so tests can replace it.
- **Shell.** `xrayconf_validate <file>` in `lib/xrayconf.sh` runs the same
  command; without `xray` it warns and succeeds. `configure.sh` calls it
  between generation and `mv`. A failure removes the temp file, keeps
  config.json and exits 1 with Xray's message.
- **Temp names.** They do not end in `.json`, so the running
  `xray -confdir` never loads them; hence the explicit `-format json`.
- **Callers.** No change needed. The Web UI, `/xray` and the bot's
  `/configure` wizard already report a failed generation, and the watch walk
  already moves on to the next server when `generated` is false.
- **Cost.** Roughly 0.5–2 s per selection, and per server the walk tries.

## 9. Subscription watch

For a server with `Outbound`, `subwatch.ServerForDial`:

- copies the outbound and writes the first IP into its address slot
  (6.5, step 6);
- when the source address is a hostname, writes it where Xray would
  otherwise take the name from an address that is now an IP: an empty
  `tlsSettings.serverName` (security `tls`, Hysteria2 included), and, for a
  stream without security, an empty `host` of `wsSettings`,
  `httpupgradeSettings` or `xhttpSettings` (a ws `headers.Host` counts as
  set). With TLS, Xray takes the Host from the server name; with REALITY,
  xhttp takes it from the REALITY server name - so neither needs a Host of
  its own;
- sets `Address` to the IP, as today.

A server without `Outbound` takes today's path. `perAddress`, resolution over
the tunnel, `xray.servers` and the identity record
(`GenerateAndRecordWalkedServer` records the original server) do not change.
The SOCKS probe does not depend on the protocol.

## 10. Display and API

- **Label.** `<protocol>[·<network>][·<security>]`, with `shadowsocks` shown
  as `ss` and `hysteria` as `hysteria2`. The network is omitted for `tcp`, the
  security for `none`, and both for ss and hysteria2. Examples:
  `vless·reality`, `vless·ws·tls`, `vmess·ws`, `trojan·tls`, `ss`,
  `hysteria2`. A legacy record shows `vless` with its stored network and
  security; an empty security reads as `tls`, which is what the legacy
  builder generates. Go computes it in `vpnconfig.Server.Label()`,
  `configure.sh` in a jq function.
- **`GET /api/servers`** returns `{name, address, port, ips, protocol}` per
  server: no `outbound`, no legacy credentials. The records now hold
  passwords, and the page needs none of them. `web/src/types.ts` drops `uuid`
  and adds `protocol`; the Servers tab gains a Protocol column.
- **Bot and shell.** Each `/servers` line ends with ` · <label>`, and
  `configure.sh` shows it in brackets after each name. (`label` is a jq
  keyword; the jq function is `protocol_label`.)

## 11. Import results and messages

Every channel reports how many servers were imported out of how many entries,
and the skips by reason.

- **A body `Decode` cannot read.** The bot says `Error: <the 6.1 error>`; the
  shell script logs `Cannot read the subscription: <the error>` and exits 1.
- **Bot `/import`.** `Imported 32 of 40 servers:`, or `Imported N servers:`
  when nothing was lost. Then the country grouping, then a line of non-zero
  counts in a fixed order — unsupported, composite, invalid, placeholder, DNS
  (e.g. `7 composite, 1 DNS error`) — and up to three `name: detail` lines
  for unsupported and invalid entries.
- **Nothing decoded.** `No supported servers in subscription`, the counts and
  up to three details. This replaces `No VLESS servers found`.
- **Web UI `POST /api/servers/import`.** A 200 carries
  `{ok, count, total, skipped: {unsupported, composite, invalid, placeholder}, dns_errors, summary}`;
  the page shows `summary` (`Import.Summary`) above the list. The 400 for an
  empty result says
  `no supported servers in subscription: <counts>; <up to three details>`
  (`Import.NoServers`); a body `Decode` cannot read answers 400 with its error.
- **Watch.** The refresh error becomes `no supported servers`; the existing
  "refresh failed" notification carries it.
- **Shell.** One `WARN` per skipped entry,
  `Skipping <name>: <reason> (<detail>)`, then
  `Found N servers in M entries (7 composite, …)`. With no server it logs
  `ERROR` and exits 1, as today.
- **Wording.** "VLESS" leaves the prompts of `import_server_list.sh`, the
  next steps of `install.sh` and the READMEs.

## 12. Migration

There is no conversion step. A record without `outbound` keeps generating
through the legacy builders, and `ServerForDial` keeps today's logic for it.
The first import after the upgrade — `/import`, the Web UI, the shell script
or a watch refresh — rewrites servers.json in the new form. VLESS servers from
links keep their `address` and `port`, and their `name` too unless it held a
run of spaces or, on the Go side, a two-byte character outside Cyrillic
(6.6). The old subscription's names hold neither, so the Web UI's Active badge
still matches after the re-import. Removing the legacy path is a later change.

## 13. Testing

### 13.1 Shared fixtures

`testdata/subscription/` at the repository root holds pairs:

- `<case>.in`: a body exactly as served;
- `<case>.want.json`:
  `{"total": N, "servers": [{name, address, port, outbound}], "skipped": [{name, reason}]}`.

A Go test in `internal/subscription` and `router/test/unit/subscription.bats`
run every pair through their decoder and compare the JSON by value, without
`skipped[].detail`; array order matters. A new case is two files, and both
implementations must pass it.

The cases cover at least:

- **Containers:** standard and URL-safe unpadded base64; a plain list with
  CRLF, blank lines and comment lines.
- **Links:**
  - every transport and security;
  - vmess in both forms; trojan's default TLS;
  - ss as SIP002 base64, SS-2022 percent-encoded, legacy base64, with a
    plugin and with an unsupported method;
  - hy2 plain, with salamander, with a pin, insecure without a pin, with a
    port range, with `mport`, without a port;
  - tuic and ssr links.
- **Invalid entries and placeholders:** a bad port, a malformed `%`, REALITY
  without `pbk`, a non-JSON `extra`; a placeholder; an IPv6 literal.
- **Names:** flags, percent-encoded Cyrillic, `ü`, double spaces, no name at
  all.
- **Xray JSON:** an array shaped like the new subscription — single VLESS
  REALITY, Hysteria2, Shadowsocks, a balancer, a `dialerProxy` chain, the flat
  `settings.address` form, a `sockopt.mark` to strip, `allowInsecure` with and
  without a pin; a single config object; a sing-box-like object.
- **Unrecognized bodies:** a Clash YAML and an HTML page.

The fixtures are synthetic: addresses from 192.0.2.0/24, 198.51.100.0/24,
203.0.113.0/24 and 2001:db8::/32, hosts under `example.com`, invented ids,
passwords and keys. The repository is public, and a provider's real hosts in
it would be a ready-made blocklist. The invented secrets have the formats Xray
checks - a REALITY key is base64url of 32 bytes, a pin 64 hex digits, an
SS-2022 key base64 of the cipher's key length - so every server a case
yields also loads in Xray 26.2.6 (`xray run -test`), which was checked while
the design was prototyped.

### 13.2 Go

- `buildOutbound` and `GenerateConfig`: a stored outbound gains only the tag;
  a legacy record produces today's output byte for byte.
- The validator, driven by fake `xray` scripts:
  - exit 0: config.json is replaced;
  - exit 23: config.json is untouched and the error carries Xray's message;
  - no binary: no check runs.
- `ServerForDial`:
  - every protocol's address slot, in both forms;
  - serverName and host are filled only when empty, and only when the source
    address is a hostname;
  - legacy records behave as today.
- Web UI and bot:
  - `/api/servers` carries no `outbound` and no credentials;
  - the import response carries the new fields;
  - `/import` texts for a partial import and for no supported servers;
  - the watch's refresh error.
- The existing `vless` tests move with the code.

### 13.3 Bats

- `xrayconf_build_outbound` with a stored outbound (the tag added, the rest
  equal under `jq -S`) and with a legacy record.
- `xrayconf_validate` against a new mock, `router/test/mocks/xray`:
  `XRAY_MOCK_EXIT`, `XRAY_MOCK_OUTPUT` and `XRAY_MOCK_LOG` set its exit code,
  its output and where its arguments go.
- `lib/subscription.sh`: no jq regex builtin (Entware's jq has none); the
  strict and the lenient percent-decoding, base64 and the name filter, on the
  inputs the Go unit tests use.
- `configure.sh`: a rejected config keeps config.json; the server list shows
  labels.
- `import_server_list.sh` end to end on the fixtures, through the existing DNS
  mock:
  - servers.json holds `ips` and `outbound`;
  - the summary line is printed;
  - `xray.subscription_url` handling does not change.
- The `parse_vless_uri` tests in `import_server_list.bats` become fixture
  cases.

### 13.4 Device check (owner's permission only)

On the author's router:

- import both real subscriptions through the shell script and through the
  Web UI;
- select a VLESS, a Hysteria2 and a Shadowsocks server, and confirm a LAN
  client's traffic through each;
- confirm the watch walk crosses protocols.

## 14. Documentation

| File | Change |
|---|---|
| `CLAUDE.md` | architecture rows for `lib/subscription.sh` and `internal/subscription` |
| `.claude/rules/xray-tproxy.md` | outbound generation: stored outbound, legacy path, validation |
| `.claude/rules/telegram-bot.md` | package tree, `/import`, watch wording |
| `.claude/rules/webui.md` | API fields |
| `.claude/rules/testing.md` | shared fixtures, the new bats file, the `xray` mock |
| `README.md`, `README.ru.md` | supported formats |

## 15. Files touched

| Area | Files |
|---|---|
| Go, new | `server/internal/subscription/` (section 5) and its tests |
| Go, removed | `server/internal/vless/` |
| Go, changed | `internal/vpnconfig/vpnconfig.go` and a new `outbound.go`; `internal/service/xray.go`; `internal/subwatch/watch.go`; `internal/bot/subfetch.go`; `internal/handler/import.go`; `internal/handler/servers.go`; `internal/webapi/handler_servers.go`; their tests |
| Web | `web/src/types.ts`, `web/src/api.ts`, `web/src/components/ServersTab.vue` |
| Shell, new | `router/opt/vpn-director/lib/subscription.sh` |
| Shell, changed | `import_server_list.sh`, `lib/xrayconf.sh`, `configure.sh`, `router/files.manifest`, `install.sh` |
| Tests | `testdata/subscription/` (new); `router/test/unit/subscription.bats` (new); `router/test/mocks/xray` (new); `router/test/import_server_list.bats`; `router/test/unit/xrayconf.bats`; `router/test/unit/configure.bats`; the Go tests of every changed package |
| Docs | section 14 |

## 16. Out of scope

- Clash / mihomo YAML, sing-box JSON, SIP008.
- Composite entries: balancers, observatories, chains.
- WireGuard, SOCKS, HTTP, TUIC, Hysteria v1, SSR and AnyTLS.
- Hysteria2 port hopping, mKCP, TCP HTTP header obfuscation, `ech`.
- User-Agent changes, and the subscription metadata headers
  (`subscription-userinfo`, `profile-title`, `announce`).
- IPv6 server addresses.
- An automatic servers.json migration, and the removal of the legacy builders.
