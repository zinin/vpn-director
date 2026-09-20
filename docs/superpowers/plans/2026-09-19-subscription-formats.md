# Subscription Formats Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import share links of every protocol the router's Xray runs (vless, vmess, trojan, ss, hysteria2), base64-encoded or plain, and Xray JSON subscriptions, in both the shell and the Go importer, storing every server as a ready Xray outbound.

**Architecture:** Two twin decoders — `router/opt/vpn-director/lib/subscription.sh` (bash, jq, gawk) and the Go package `server/internal/subscription` — turn a subscription body into servers that carry a ready Xray `outbound`. The shared cases in `testdata/subscription/` hold both to the same answers. Both config generators insert the stored outbound as `proxy-out` and have `xray run -test` check a config before it replaces the live one; a record from before outbounds were stored keeps today's VLESS builder. Labels name each server's protocol in the Web UI, the bot and `configure.sh`.

**Tech Stack:** Go 1.25 (standard library only); bash 5, jq 1.8 without oniguruma and gawk from Entware; bats-core with bats-support and bats-assert; Vue 3 + TypeScript (vue-tsc, Vite).

**Spec:** `docs/superpowers/specs/2026-09-18-subscription-formats-design.md` — read it before starting; this plan implements it section by section and names the sections it follows.

## Global Constraints

- **Parity.** Every decoding rule lives in both `router/opt/vpn-director/lib/subscription.sh` and `server/internal/subscription`, and every case in `testdata/subscription/` must decode identically in both (`router/test/unit/subscription.bats`, `server/internal/subscription/fixtures_test.go`). A rule changed on one side only is a bug.
- **jq on the routers.** Entware's jq 1.8.1 is built without oniguruma: shipped jq must not use `test`, `match`, `capture`, `scan`, `splits`, `sub`, `gsub` or the two-argument `split`. `label` is a jq keyword; never name a jq function `label`.
- **Shell on the routers.** bash 5 from Entware; scripts run under `set -euo pipefail`. Byte-wise work runs under `LC_ALL=C`. Never `tr '[:upper:]'` (see `.claude/rules/shell-conventions.md`); no `paste`. A function that returns non-zero on purpose is called as `f || :` or inside a condition.
- **Fixtures are synthetic.** Addresses only from 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32; hosts under `example.com`, `example.org`, `example.net`; invented ids, passwords and keys, in the formats Xray checks (a REALITY key is base64url of 32 bytes, a pin 64 hex digits, an SS-2022 key base64 of the cipher's key length). No real provider host, SNI, key or token: the repository is public.
- **Xray.** The routers run Entware's xray-core 26.2.6. Since 2026-06-01 Xray refuses any config with `tlsSettings.allowInsecure: true`. `xray run -test` needs `-format json` for a file whose name does not end in `.json`.
- **Go.** Module `github.com/zinin/vpn-director/server`, `go 1.25.5`, no new dependencies. `go test ./...` compiles `cmd/webui`, which embeds `server/cmd/webui/web/dist`; if that directory is missing, run `make web-embed` once from the repository root. CI runs `go vet ./...` and `go test ./... -count=1` in `server/`.
- **Fixed words.** Skip reasons are exactly `unsupported`, `composite`, `invalid`, `placeholder`. Decode errors are exactly `empty subscription`, `invalid JSON subscription`, `unrecognized subscription format`.
- **Commits.** One per task, conventional style as in `git log` (`feat(scope): …`, `test: …`, `docs: …`); `git add` only the files the task names — the working tree holds unrelated untracked files.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `server/internal/vpnconfig/vpnconfig.go` | `Server.Outbound`; `uuid` omitted when empty | 1 |
| `server/internal/vpnconfig/outbound.go` | `DecodeOutbound`, `OutboundTarget` (the address slot), `Server.Label` | 1 |
| `server/internal/subscription/subscription.go` | `Result`, `Skip`, reasons, decode errors, `Result.add` (placeholder test, naming) | 2 |
| `server/internal/subscription/names.go` | `cleanName` (byte machine), `isPlaceholder` | 2 |
| `server/internal/subscription/text.go` | strict and lenient percent-decoding, base64, JSON reading, `splitList`, `prune` | 2 |
| `server/internal/subscription/decode.go` | `Decode`: container detection | 3, 5 |
| `server/internal/subscription/links.go` | link list, share-link grammar, query, stream settings | 3, 4 |
| `server/internal/subscription/{vless,trojan,vmess,shadowsocks,hysteria2}.go` | one converter per scheme | 3, 4 |
| `server/internal/subscription/xrayjson.go` | Xray JSON: the single proxy outbound, checks, sanitization | 5 |
| `server/internal/subscription/resolve.go` | `Import`, `LookupIPv4`, `DecodeAndResolve`, `DecodeAndResolveLookup` | 6 |
| `server/internal/subscription/summary.go` | `Counts`, `Details`, `Summary`, `NoServers`, `SkippedByReason` | 6 |
| `server/internal/subscription/fixtures_test.go` | runs every shared case through `Decode` | 3 |
| `router/opt/vpn-director/lib/subscription.sh` | the shell twin: `subscription_decode` | 2–5 |
| `router/test/unit/subscription.bats` | runs every shared case through `subscription_decode`; helper tests; jq-regex guard | 2, 3 |
| `testdata/subscription/*.in`, `*.want.json` | the shared cases | 3–5 |
| `server/internal/bot/subfetch.go`, `handler/import.go`, `webapi/handler_servers.go` | callers switch from `vless` to `subscription`; import reports | 6 |
| `server/internal/vless/` | removed | 6 |
| `server/internal/service/xray.go` | `serverOutbound`, `xrayTest` before the rename | 7 |
| `server/internal/subwatch/watch.go` | `ServerForDial` on a stored outbound, `keepHostname` | 8 |
| `server/internal/webapi/handler_servers.go`, `handler/servers.go`, `web/src/*` | protocol labels, no credentials in `/api/servers` | 9 |
| `router/opt/vpn-director/lib/xrayconf.sh`, `configure.sh`, `router/test/mocks/xray` | stored outbound, `xrayconf_validate`, labels | 10 |
| `router/opt/vpn-director/import_server_list.sh`, `install.sh` | the shell importer on the decoder | 11 |
| `CLAUDE.md`, `.claude/rules/*.md`, `README*.md` | documentation | 12 |

Patches are unified diffs against the tree the earlier tasks left; run each block from the repository root as shown. If `git apply` refuses a hunk, the tree differs from what the earlier tasks produce: stop and compare, do not force it.

---

### Task 1: Stored outbound on the server record
✅ Done — see commit(s): `f04b16d`

### Task 2: Decoder foundations in both languages
✅ Done — see commit(s): `436427d`

### Task 3: Share-link grammar, VLESS and Trojan, and the shared cases
✅ Done — see commit(s): `172b375`

### Task 4: VMess, Shadowsocks and Hysteria2 links
✅ Done — see commit(s): `477fd64`

### Task 5: Xray JSON subscriptions
✅ Done — see commit(s): `a648a19`

### Task 6: Resolution, import reports, and the Go callers
✅ Done — see commit(s): `9464a11`

### Task 7: Go generator inserts the stored outbound and has Xray test it
✅ Done — see commit(s): `1f493d9`

### Task 8: The watch dials a stored outbound by IP
✅ Done — see commit(s): `68365f9`

### Task 9: Protocol labels in the Web UI and the bot
✅ Done — see commit(s): `f6b263d`

### Task 10: Shell generator, Xray test and the wizard's list
✅ Done — see commit(s): `7720333`

### Task 11: The shell importer on the decoder
✅ Done — see commit(s): `7255cd9`

### Task 12: Documentation
✅ Done — see commit(s): `0d57c82`

### Task 13: Final verification

Spec 13. Everything CI runs, then an optional check against a real Xray, then the device check.

- [ ] **Step 1: The whole Go suite, as CI runs it**

Run: `cd server && go vet ./... && go test ./... -count=1`
Expected: every package `ok`.

- [ ] **Step 2: The whole bats suite, as CI runs it**

Run: `bats router/test/*.bats router/test/unit router/test/integration`
Expected: no `not ok` line (578+ tests at the time of writing).

- [ ] **Step 3: The Web UI build**

Run: `cd web && npm ci && npm run build`
Expected: `✓ built`.

- [ ] **Step 4 (optional, needs network): every case's servers load in Xray 26.2.6**

The routers run Entware's xray-core 26.2.6; every server a shared case yields must load in it. This downloads the release into a temporary directory:

```bash
tmp=$(mktemp -d)
curl -sSL -o "$tmp/xray.zip" https://github.com/XTLS/Xray-core/releases/download/v26.2.6/Xray-linux-64.zip
python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extract('xray', sys.argv[2])" "$tmp/xray.zip" "$tmp"
chmod +x "$tmp/xray"
for w in testdata/subscription/*.want.json; do
    jq -c '.servers[]? | {name, outbound}' "$w" | while IFS= read -r s; do
        jq --argjson s "$s" '.outbounds = [$s.outbound + {tag: "proxy-out"}]' \
            router/opt/etc/xray/config.json.template > "$tmp/config.json.test"
        "$tmp/xray" run -test -format json -c "$tmp/config.json.test" >/dev/null 2>&1 ||
            echo "REJECTED ${w##*/}: $(jq -r .name <<< "$s")"
    done
done
rm -rf "$tmp"
```
Expected: no `REJECTED` line.

- [ ] **Step 5: Device check — only with the owner's permission**

Ask the owner before touching the router. On the author's router:
1. Install the build (the owner's usual update path), then import both real subscriptions — once with `/opt/vpn-director/import_server_list.sh` over SSH, once from the Web UI. The Web UI shows `Imported 32 of 40 servers: 7 composite, 1 DNS error` for the Xray JSON one (counts may move with the provider's list).
2. Select a VLESS, a Hysteria2 and a Shadowsocks server in turn; from a LAN client in `xray.clients`, check traffic through each (for example `curl -4 https://ifconfig.me` shows the server's exit).
3. Break the running server (for example select one whose port is closed) and watch the bot's subscription watch fail over and walk to a working server of any protocol.

- [ ] **Step 6: Before opening the pull request**

The repository keeps design and plan documents out of pull requests (the owner's instructions): when the branch is ready, `git rm -r docs/superpowers/` and commit that, so the documents stay in the branch history only.
