---
paths: "server/internal/webapi/**/*, server/internal/auth/**/*, server/cmd/webui/**/*, web/**/*"
---

# Web UI

HTTPS interface for VPN Director, served by the `webui` daemon. Shares the
`internal/service` layer with the Telegram bot; both write `vpn-director.json`
only through `ConfigService.UpdateVPNConfig`, which holds a `flock`.
`configure.sh` is the third writer of that file and takes the same lock
(`.vpn-director.json.lock`) around its read-modify-write; it rebuilds the
config from the existing one merged under the template, so the fields the
daemons own — `jwt_secret`, `exclude_ips`, `paused_clients` — survive a wizard
run.

## Architecture

```
server/cmd/webui/main.go     # Entry point: config, logging, DI, dev mode, TLS
server/internal/
├── webapi/
│   ├── server.go            # http.Server: TLS 1.2+, WriteTimeout 30s
│   ├── router.go            # Deps, route table, SPA fallback with cache headers
│   ├── middleware.go        # JWT + password-fingerprint check, login rate limit, request log
│   ├── deadline.go          # Per-handler write-deadline extensions
│   ├── apply.go             # Config write + apply, shared by the mutating handlers
│   ├── response.go          # jsonOK / jsonError / decodeJSON
│   ├── errline.go           # Last error line of a shell failure, for the client
│   └── handler_*.go         # status, servers, clients, excludes, logs, auth, update
└── auth/
    ├── shadow.go            # /etc/shadow verification, Fingerprint
    └── jwt.go               # HS256 issue and validation
web/                         # Vue 3 SPA (Vite), embedded via go:embed
```

## API

Every route below `/api/` except `POST /api/login` requires a valid token.

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/login` | Password check against `/etc/shadow`, sets the cookie |
| POST | `/api/logout` | Clears the cookie |
| GET | `/api/status` | `vpn-director.sh status` |
| POST | `/api/apply` / `/api/restart` / `/api/stop` | VPN Director control |
| POST | `/api/ipsets/update` | `vpn-director.sh update` (`IPSET_FORCE_UPDATE=1`) |
| GET | `/api/ip` | External IP |
| GET | `/api/version` | Build version and commit |
| GET | `/api/platform` | `vpn-director.sh platform`: firmware, password file, LAN/WAN interfaces, tunnels; 503 when the script cannot answer |
| GET | `/api/servers` | Xray server list plus `active`, the recorded server, and `subscription_saved` |
| POST | `/api/servers/active`, `/api/servers/import` | Select the active server; import a subscription (`url` empty reuses the saved URL) |
| GET/POST/DELETE | `/api/clients` | LAN clients; a POST route must be xray, a tunnel already in the config, or a tunnel `/api/platform` lists; 503 when the platform cannot answer for a route outside the config |
| POST | `/api/clients/pause`, `/api/clients/resume` | Pause and resume a client |
| GET/POST | `/api/excludes/sets` | Country exclusion sets |
| GET/POST/DELETE | `/api/excludes/ips` | Excluded IPs and CIDRs |
| GET | `/api/logs` | One source (`?source=`) or every source at once |
| GET | `/api/config` | `vpn-director.json` with `jwt_secret` and `subscription_url` blanked |
| GET | `/api/update/check` | Latest release; `?force=1` pierces the 30-minute cache |
| POST | `/api/update` | Starts the unified update, answers 202 |
| GET | `/api/update/status` | Whether an update is running |

`xray.active_server` is the only record of which server is running.
`config.json` carries just the outbound, and a subscription routinely puts many
names behind one `address:port` - the router this was written on has 62 servers
across 9 endpoints and one UUID - so the name cannot be recovered from it
afterwards. Every path that generates `config.json` writes the record, and
writes it **under the same config lock as the generation**: the three Go paths
— the Web UI, the bot's `/xray`, the wizard — through
`service.GenerateAndRecordActiveServer`, and `configure.sh` by holding its
`flock` across both steps. Without one lock a switch from the other daemon can
land between this one's `config.json` and its record, and the two then name
different servers with both switches reporting success.

Inside that lock the record follows only a generation that succeeded — an
error from the closure skips the save — so a server whose parameters Xray
rejects is never named as the running one. Callers branch on the returned
`generated`, never on the error: false means `config.json` is untouched and the
switch did not happen (the server was rejected, or the config could not even be
loaded to reach the generation), so the caller reports the failure and stops
short of restarting Xray; true with an error means the opposite — `config.json`
was written and only the record was not — so the caller logs and carries on
rather than sending the user back to redo a switch that worked.

The record holds name, address and port only: `/api/config` hands this file to
the browser, so the UUID and the REALITY material stay out. It is absent, and
`active` is `null`, until something selects a server.

Client and exclusion mutations go through `updateAndApply`: the change is
written under the config lock and `vpn-director.sh apply` runs immediately
after, as the bot does. The two server routes are the exception —
`/api/servers/active` regenerates `config.json`, rewrites `xray.servers` under
the lock and restarts Xray instead of applying, and `/api/servers/import` only
writes `servers.json` and `xray.servers`. All of them serialize on
`Deps.OpMutex`.

## Authentication

- `POST /api/login` verifies the password against `/etc/shadow` (MD5, SHA-256
  and SHA-512 MCF hashes, pure Go), then issues an HS256 JWT valid for 24 hours
  in an `HttpOnly; Secure; SameSite=Strict` cookie.
- The token carries `pwh`: `ShadowAuth.Fingerprint`, the first 8 bytes of the
  SHA-256 of the stored password hash, in hex. `authMiddleware` recomputes it
  on **every** request — no cache — so changing the router password ends every
  session at once. A missing or mismatched claim is 401; an unreadable
  `/etc/shadow` is 500, because a 401 would send the SPA to a login page that
  fails the same way.
- `Authorization: Bearer <token>` is accepted wherever the cookie is, so a
  script can reuse a token. There is no endpoint that hands one out: obtain it
  from the login response's `Set-Cookie`.
- Failed logins are rate limited per IP: 5 attempts a minute, then a 30-second
  lockout.
- `jwt_secret` is generated on first start when empty and written back under
  the config lock. It is never rewritten, which is what lets a login session
  survive an update. `GET /api/config` blanks `jwt_secret` and `subscription_url`
  (the latter is a secret: the subscription token sits in the path).
- The file paths in `vpn-director.json` — `data_dir`, `webui.cert_file`,
  `webui.key_file` — are read against **the config file's own directory** when
  they are relative (`paths.Resolve`). They cannot be read against the working
  directory: outside dev mode both daemons move to `/` at startup, and before
  that the directory was whatever the launcher had. Everything the project
  writes is absolute anyway; the rule is what a hand-edited relative value
  means.

## Dev mode

```bash
cd server && go run ./cmd/webui --dev
```

`testdata/dev/` is gitignored and recreated when missing. A sandbox from before
the paths rule above carries `"data_dir": "testdata/dev/data"`, which now
resolves beside the config and lands in `testdata/dev/testdata/dev/data`: delete
`testdata/dev/vpn-director.json` once, or change the value to `"data"`. Dev mode
keeps the directory it was started in — `DevPaths` are relative to `server/`, so
`--dev` still has to be run from there.

Plain HTTP instead of TLS, `server/testdata/dev/` for config, shadow and logs,
`devmode.Executor` instead of real shell commands, and an `admin`/`admin`
shadow file created on first run. `testdata/dev/vpn-director.json` is
gitignored.

## Build with embed

```bash
make build-webui          # web -> web-embed -> make -C server build-webui
make build-webui-arm64    # the same, cross-compiled
```

`make web-embed` copies `web/dist` into `server/cmd/webui/web/dist`, which
`go:embed` picks up; that directory is gitignored. `spaHandler` serves hashed
bundles under `assets/` as immutable and revalidates `index.html`, so a new
binary's bundle names are picked up right after an update.

## Update flow

`internal/updateflow` is shared with the bot; the Web UI handlers are adapters
over it. See `telegram-bot.md` for the handover's two steps, the script and the
daemon table. Points that belong to the Web UI half:

- Both GitHub-facing routes extend the write deadline twice — once for the API
  call, once after it returns — because the flow serializes its callers on one
  mutex and a queued caller could otherwise consume the whole deadline.
- The handover runs in the goroutine behind the 202. While step 2 downloads,
  `/api/update/status` reports the update running, because the lock names the
  live daemon that started it, and step 1 gives step 2 at most 15 minutes,
  inside the page's twenty-minute wait.
- The front end polls `/api/version` every 3 seconds with `skipAuthRedirect`,
  so the 401s and connection errors of a restarting server do not bounce the
  user to the login page. It reloads as soon as the new version answers. Five
  minutes is the restart window; past it the loop continues only while
  `/api/update/status` still reports an update running, up to twenty minutes.
  Both requests carry their own 8-second timeout: the loop awaits the status
  call, so a hung connection would otherwise keep it from reaching that limit.
  `/api/update/status` also lets a reloaded page rejoin an update it did not
  start — and such a rejoin has no target version, so it records the running
  version first: the script's `EXIT` trap restarts the old binaries, and
  without that baseline a failed update looks exactly like a finished one.
- The login session survives an update because nothing in the flow rewrites
  `jwt_secret`. The exception is the release that introduced the `pwh` claim:
  every token issued before it needs one repeat login.

## Trust model of self-update

Release metadata comes from `api.github.com`, binaries from the URLs that
response supplies and the shell scripts from `raw.githubusercontent.com` at the
release tag; all of it is written to the router and run as root. The **entire**
integrity guarantee is TLS to github.com plus GitHub account security — there
is no signature, no checksum and no pinning. That is the same model
`install.sh`'s `curl … | bash` establishes, and it is stated rather than
assumed. What the code does enforce: an asset URL must be `https`, every string
reaching the generated script passes a strict allow-list, downloads are capped
at 50 MB, and `files/` is wiped before every attempt so a partial download
cannot be executed later. Since the self-update handover the new release's
daemon binary also runs as root before it is installed: step 1 executes it as
the installer (`internal/updater/handover.go`). It is the binary the update
installs and starts anyway, so the trust does not change.

## Known limits

- The update script is a **restart** net, not a **rollback** net: its `EXIT`
  trap brings back the daemons that were running, but a failure part-way
  through the copy step leaves a mixed set of files.
- Three GitHub consumers share the unauthenticated 60-requests-per-hour budget:
  the bot's `Flow`, the Web UI's `Flow` (separate processes, separate caches)
  and `updatechecker`'s own ticker. Nothing coordinates them.
- The update script hand-parses its daemon table in seven separate
  `for entry in $DAEMONS` loops, and the init-script field alone is read in two
  spellings (`${rest##*|}` and `${entry##*|}`). Adding a fourth field to
  `updater.Daemons` breaks all of them at once. What the table may contain is
  pinned by `TestDaemons_CarryNoShellMetacharacters`; what the script renders
  is pinned by the byte-exact golden.
