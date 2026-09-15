# Xray Subscription Watch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist the VLESS subscription URL and, in the Telegram bot, watch the Xray outbound through SOCKS; after three minutes of consecutive failures move Xray LAN clients onto a Tunnel Director exit, refresh the subscription, pick the same name or the first working server, then restore the clients.

**Architecture:** Config mutations (`subscription_url`, `failover`, TD exit filter, move/restore) live in `vpnconfig` as pure functions. SOCKS probe and the watch loop live in `internal/subwatch` with injected I/O. The bot wires the loop next to PathManager (not in `--dev`), fetches the subscription WAN-first then `DialPath` through a TD tunnel, and notifies active chats. Web UI and `/import` only save and reuse the URL; they do not run the watch.

**Tech Stack:** Go 1.x (existing module), `golang.org/x/net/proxy` SOCKS5, Vue 3 Servers tab, `vpn-director.json` via `ConfigStore.UpdateVPNConfig`.

**Spec:** `docs/superpowers/specs/2026-09-16-xray-subscription-watch-design.md`

## Global Constraints

- Watch loop runs only inside `telegram-bot`. No watch in `webui`. No new daemon.
- `--dev` does not start SubscriptionWatch.
- Probe URL is `https://www.gstatic.com/generate_204`; success is HTTP 204 through SOCKS on `127.0.0.1:<socks_port>` (default 12346). Missing SOCKS listener is a failed probe.
- Probe interval 30 s. Dead after 3 minutes of consecutive failed probes (six ticks). Import retry while failed over: 5 minutes. Settle 3 s after Xray restart.
- Timing values are constants, not `advanced.*` knobs.
- TD fallback uses the same candidate filter as PathManager, **not** a Telegram probe: key in `tunnel_director.tunnels`, not `main`, `clients` length ≥ 1, platform `connected` with non-empty `iface`. Sort ids, take the first.
- Xray clients must leave `xray.clients` during failover or TPROXY wins.
- `xray.subscription_url` is a secret. `GET /api/config` blanks it. Do not put a real subscription token in tests, logs, or commits.
- `xray.failover` may appear in `/api/config`.
- `configure.sh` is not failover-aware. Do not change it in this plan.
- PathManager stickiness is unchanged.
- IPv6 is out of scope; SOCKS and fetch dial `tcp4`.
- English for code, comments, commits, docs, Telegram watch text. Do not commit `docs/superpowers/` in a later PR (git-rm before the PR). Stage by name, never `git add -A`.
- Tests: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./<pkg> -count=1`.

## File map

| File | Role |
|------|------|
| `server/internal/vpnconfig/vpnconfig.go` | `SubscriptionURL`, `Failover *XrayFailover` on `XrayConfig` |
| Create `server/internal/vpnconfig/failover.go` | `XrayFailover`, `TDExits`, `FirstTDExit`, `EffectiveXrayClients`, `Armed`, `MoveXrayClientsToTunnel`, `RestoreXrayClientsFromFailover` |
| `server/internal/bot/path.go` | `candidates` uses `TDExits` so the filter cannot drift |
| `server/internal/webapi/handler_servers.go` | `subscription_saved`; resolve empty import URL; persist URL on import |
| `server/internal/webapi/handler_logs.go` | Blank `subscription_url` on `GET /api/config` |
| `server/internal/handler/import.go` | `/import` with no args uses saved URL; with args saves it |
| `web/src/types.ts`, `web/src/api.ts`, `web/src/components/ServersTab.vue` | Re-import from saved URL |
| Create `server/internal/subwatch/` | Probe + `Watch.Tick` |
| Create `server/internal/bot/subfetch.go` | WAN then `DialPath` download |
| `server/internal/bot/bot.go` | Start/stop the watch with PathManager |
| `.claude/rules/telegram-bot.md`, `.claude/rules/webui.md` | Document the behaviour |

---

### Task 1: Config types and failover mutations

**Files:**
- Modify: `server/internal/vpnconfig/vpnconfig.go` (`XrayConfig` around the `ActiveServer` field)
- Create: `server/internal/vpnconfig/failover.go`
- Create: `server/internal/vpnconfig/failover_test.go`
- Modify: `server/internal/vpnconfig/vpnconfig_test.go` (omitempty tests next to `TestXrayConfig_OmitsTheActiveServerUntilOneIsSelected`)
- Modify: `server/internal/bot/path.go` (`candidates`, the tunnel loop)

**Interfaces:**
- Consumes: existing `VPNDirectorConfig`, `PlatformInfo`, `TunnelConfig`
- Produces:

```go
type XrayFailover struct {
	Tunnel  string   `json:"tunnel"`
	Clients []string `json:"clients"`
}

func TDExits(cfg *VPNDirectorConfig, plat PlatformInfo) []string
func FirstTDExit(cfg *VPNDirectorConfig, plat PlatformInfo) string
func EffectiveXrayClients(cfg *VPNDirectorConfig) []string
func Armed(cfg *VPNDirectorConfig) bool
func MoveXrayClientsToTunnel(cfg *VPNDirectorConfig, tunnel string)
func RestoreXrayClientsFromFailover(cfg *VPNDirectorConfig)
```

`XrayConfig` gains:

```go
SubscriptionURL string        `json:"subscription_url,omitempty"`
Failover        *XrayFailover `json:"failover,omitempty"`
```

- [ ] **Step 1: Write the failing tests**

Create `server/internal/vpnconfig/failover_test.go`:

```go
package vpnconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sample() *VPNDirectorConfig {
	return &VPNDirectorConfig{
		PausedClients: []string{"192.168.1.9"},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"main":   {Clients: []string{"192.168.1.1"}},
			"ovpnc1": {Clients: []string{"192.168.1.2"}},
			"ovpnc2": {Clients: []string{"192.168.1.3"}},
			"ovpnc3": {Clients: []string{}},
			"wgc1":   {Clients: []string{"192.168.1.4"}},
		}},
		Xray: XrayConfig{
			Clients:         []string{"192.168.1.8", "192.168.1.9", "192.168.1.3"},
			SubscriptionURL: "https://cdn.example/s/token",
		},
	}
}

func plat() PlatformInfo {
	return PlatformInfo{Tunnels: []PlatformTunnel{
		{ID: "ovpnc1", Iface: "tun11", Connected: false},
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "ovpnc3", Iface: "tun13", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: false},
	}}
}

func TestTDExits_MatchesPathManagerFilter(t *testing.T) {
	got := TDExits(sample(), plat())
	want := []string{"ovpnc2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TDExits = %v, want %v", got, want)
	}
	if FirstTDExit(sample(), plat()) != "ovpnc2" {
		t.Fatal("FirstTDExit")
	}
	if FirstTDExit(sample(), PlatformInfo{}) != "" {
		t.Fatal("no platform tunnels")
	}
}

func TestEffectiveXrayClients_DropsPaused(t *testing.T) {
	got := EffectiveXrayClients(sample())
	want := []string{"192.168.1.8", "192.168.1.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestArmed(t *testing.T) {
	cfg := sample()
	if !Armed(cfg) {
		t.Fatal("url + clients")
	}
	cfg.Xray.Clients = nil
	if Armed(cfg) {
		t.Fatal("url but no clients and no failover")
	}
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	if !Armed(cfg) {
		t.Fatal("failover arms even with empty xray.clients")
	}
	cfg.Xray.SubscriptionURL = ""
	if Armed(cfg) {
		t.Fatal("no url")
	}
}

func TestMoveAndRestore_KeepsForeignTunnelClients(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover = %+v", cfg.Xray.Failover)
	}
	// failover.clients is only addresses this move added, not IPs already on
	// the tunnel. 192.168.1.3 stays on ovpnc2 (spec §9).
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Fatalf("added snapshot %v", cfg.Xray.Failover.Clients)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9"}) {
		t.Fatalf("xray.clients %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.8"}) {
		t.Fatalf("tunnel %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if len(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients) != 2 {
		t.Fatal("second move must not duplicate")
	}
	RestoreXrayClientsFromFailover(cfg)
	if cfg.Xray.Failover != nil {
		t.Fatal("failover")
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.8"}) {
		t.Fatalf("restored xray %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3"}) {
		t.Fatalf("tunnel after restore %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
}

func TestXrayConfig_OmitsSubscriptionURLAndFailoverWhenEmpty(t *testing.T) {
	out, err := json.Marshal(VPNDirectorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, k := range []string{"subscription_url", "failover"} {
		if strings.Contains(s, k) {
			t.Errorf("marshalled %s, want no %q", s, k)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/vpnconfig/ -count=1 -run 'TestTDExits|TestEffectiveXrayClients|TestArmed|TestMoveAndRestore|TestXrayConfig_OmitsSubscriptionURL'`

Expected: FAIL compile, `undefined: TDExits` (and the rest).

- [ ] **Step 3: Implement**

Add fields on `XrayConfig`. Create `failover.go`:

```go
package vpnconfig

import "sort"

type XrayFailover struct {
	Tunnel  string   `json:"tunnel"`
	Clients []string `json:"clients"`
}

func pausedSet(cfg *VPNDirectorConfig) map[string]struct{} {
	s := make(map[string]struct{}, len(cfg.PausedClients))
	for _, ip := range cfg.PausedClients {
		s[ip] = struct{}{}
	}
	return s
}

func EffectiveXrayClients(cfg *VPNDirectorConfig) []string {
	if cfg == nil {
		return nil
	}
	paused := pausedSet(cfg)
	out := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func Armed(cfg *VPNDirectorConfig) bool {
	if cfg == nil || cfg.Xray.SubscriptionURL == "" {
		return false
	}
	if cfg.Xray.Failover != nil {
		return true
	}
	return len(EffectiveXrayClients(cfg)) > 0
}

func TDExits(cfg *VPNDirectorConfig, plat PlatformInfo) []string {
	if cfg == nil {
		return nil
	}
	byID := make(map[string]PlatformTunnel, len(plat.Tunnels))
	for _, t := range plat.Tunnels {
		byID[t.ID] = t
	}
	ids := make([]string, 0, len(cfg.TunnelDirector.Tunnels))
	for id, tun := range cfg.TunnelDirector.Tunnels {
		if id == "main" || len(tun.Clients) == 0 {
			continue
		}
		pt, ok := byID[id]
		if !ok || !pt.Connected || pt.Iface == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func FirstTDExit(cfg *VPNDirectorConfig, plat PlatformInfo) string {
	ids := TDExits(cfg, plat)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func MoveXrayClientsToTunnel(cfg *VPNDirectorConfig, tunnel string) {
	if cfg == nil || tunnel == "" {
		return
	}
	if cfg.Xray.Failover != nil {
		return
	}
	tun, ok := cfg.TunnelDirector.Tunnels[tunnel]
	if !ok {
		return
	}
	paused := pausedSet(cfg)
	added := make([]string, 0)
	keptXray := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			keptXray = append(keptXray, ip)
			continue
		}
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
			added = append(added, ip)
		}
		// unpaused Xray clients leave xray.clients even if they already sat on the tunnel
	}
	if cfg.TunnelDirector.Tunnels == nil {
		cfg.TunnelDirector.Tunnels = map[string]TunnelConfig{}
	}
	cfg.TunnelDirector.Tunnels[tunnel] = tun
	cfg.Xray.Clients = keptXray
	cfg.Xray.Failover = &XrayFailover{Tunnel: tunnel, Clients: added}
}

func RestoreXrayClientsFromFailover(cfg *VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	fo := cfg.Xray.Failover
	for _, ip := range fo.Clients {
		if !contains(cfg.Xray.Clients, ip) {
			cfg.Xray.Clients = append(cfg.Xray.Clients, ip)
		}
	}
	if tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]; ok {
		kept := make([]string, 0, len(tun.Clients))
		drop := make(map[string]struct{}, len(fo.Clients))
		for _, ip := range fo.Clients {
			drop[ip] = struct{}{}
		}
		for _, ip := range tun.Clients {
			if _, ok := drop[ip]; !ok {
				kept = append(kept, ip)
			}
		}
		tun.Clients = kept
		cfg.TunnelDirector.Tunnels[fo.Tunnel] = tun
	}
	cfg.Xray.Failover = nil
}
```

Fix the accidental `mar` token if you paste — there must be no `mar` in the paused skip.

Refactor `candidates` in `path.go` to iterate `vpnconfig.TDExits(cfg, plat)` instead of repeating the filter. Keep SOCKS/direct prefix. Existing `TestCandidates` must still pass.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/vpnconfig/ ./internal/bot/ -count=1`

Expected: PASS (bot tests that use `candidates` included).

- [ ] **Step 5: Commit**

```bash
git add server/internal/vpnconfig/vpnconfig.go \
  server/internal/vpnconfig/failover.go \
  server/internal/vpnconfig/failover_test.go \
  server/internal/vpnconfig/vpnconfig_test.go \
  server/internal/bot/path.go
git commit -m "feat(config): persist subscription failover fields and TD exits"
```

---

### Task 2: Web API — save, redact, re-import URL

**Files:**
- Modify: `server/internal/webapi/handler_servers.go` (`handleListServers`, `handleImportServers`, `syncXrayServers`)
- Modify: `server/internal/webapi/handler_servers_test.go`
- Modify: `server/internal/webapi/handler_logs.go` (`handleConfig`)
- Modify: `server/internal/webapi/handler_logs_test.go` (`TestHandleConfig_OK`)

**Interfaces:**
- Consumes: `vpnconfig.XrayConfig.SubscriptionURL`
- Produces: `GET /api/servers` includes `subscription_saved` (bool, never the URL). Empty `POST /api/servers/import` `url` uses the saved URL. Non-empty `url` is written to `xray.subscription_url` in the same `UpdateVPNConfig` as `xray.servers`. `GET /api/config` returns `subscription_url` as `""`.

Do **not** add a full download test against `httptest` in webapi: `ssrf.IsPrivateHost` rejects loopback and the handler requires `https`. Cover URL resolution and persistence through `syncXrayServers` and `resolveSubscriptionURL`.

- [ ] **Step 1: Write the failing tests**

Add to `handler_servers_test.go`:

```go
func TestHandleListServers_SubscriptionSaved(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{{Name: "S", Address: "a.example", Port: 443}},
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://cdn.example/s/token"},
		},
	}
	rec := httptest.NewRecorder()
	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SubscriptionSaved bool `json:"subscription_saved"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.SubscriptionSaved {
		t.Fatal("expected subscription_saved")
	}
	if strings.Contains(rec.Body.String(), "cdn.example") {
		t.Fatal("URL leaked")
	}
}

func TestResolveSubscriptionURL(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://saved.example/s/a"}}
	got, err := resolveSubscriptionURL("https://new.example/s/b", cfg)
	if err != nil || got != "https://new.example/s/b" {
		t.Fatalf("posted URL: %q %v", got, err)
	}
	got, err = resolveSubscriptionURL("", cfg)
	if err != nil || got != "https://saved.example/s/a" {
		t.Fatalf("saved: %q %v", got, err)
	}
	_, err = resolveSubscriptionURL("", &vpnconfig.VPNDirectorConfig{})
	if err == nil {
		t.Fatal("want error when nothing saved")
	}
}

func TestSyncXrayServers_WritesSubscriptionURL(t *testing.T) {
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	if err := syncXrayServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "https://cdn.example/s/token"); err != nil {
		t.Fatal(err)
	}
	if mc.savedCfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatalf("got %q", mc.savedCfg.Xray.SubscriptionURL)
	}
	if err := syncXrayServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if mc.savedCfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatal("empty url must not clear the saved link")
	}
}
```

Keep `TestHandleImportServers_EmptyURL`: still 400 when no saved URL (`mockConfig` with nil/empty cfg — `LoadVPNConfig` returns nil cfg; `resolveSubscriptionURL` must treat that as missing).

In `TestHandleConfig_OK` set `SubscriptionURL: "https://cdn.example/s/token"` on the fixture and assert `resp.Xray.SubscriptionURL == ""` while `resp.Xray.Clients` is unchanged. Also set `Failover: &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}` and assert it **is** present in the response.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/webapi/ -count=1 -run 'TestHandleListServers_SubscriptionSaved|TestResolveSubscriptionURL|TestSyncXrayServers_WritesSubscriptionURL|TestHandleConfig_OK'`

Expected: FAIL `undefined: resolveSubscriptionURL` / wrong signature for `syncXrayServers` / secret still in config JSON.

- [ ] **Step 3: Implement**

```go
func resolveSubscriptionURL(reqURL string, cfg *vpnconfig.VPNDirectorConfig) (string, error) {
	if reqURL != "" {
		return reqURL, nil
	}
	if cfg != nil && cfg.Xray.SubscriptionURL != "" {
		return cfg.Xray.SubscriptionURL, nil
	}
	return "", errors.New("url is required")
}

func syncXrayServers(config service.ConfigStore, servers []vpnconfig.Server, subscriptionURL string) error {
	return config.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		cfg.Xray.Servers = collectServerIPs(servers)
		if subscriptionURL != "" {
			cfg.Xray.SubscriptionURL = subscriptionURL
		}
		return nil
	})
}
```

`handleListServers`: after loading cfg, add `"subscription_saved": cfg != nil && cfg.Xray.SubscriptionURL != ""`.

`handleImportServers`: replace the empty-URL 400 with:

```go
cfg, _ := deps.Config.LoadVPNConfig()
url, err := resolveSubscriptionURL(req.URL, cfg)
if err != nil {
	jsonError(w, http.StatusBadRequest, "url is required")
	return
}
```

Use `url` for parse/fetch. Call `syncXrayServers(deps.Config, resolved, req.URL)` — persist only when the client **posted** a URL (`req.URL`), so a re-import does not need to rewrite it but also does not clear it.

`handleConfig`:

```go
redacted := *cfg
redacted.WebUI.JWTSecret = ""
redacted.Xray.SubscriptionURL = ""
jsonOK(w, &redacted)
```

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/webapi/ -count=1`

Expected: PASS. `TestHandleImportServers_EmptyURL` still 400.

- [ ] **Step 5: Commit**

```bash
git add server/internal/webapi/handler_servers.go \
  server/internal/webapi/handler_servers_test.go \
  server/internal/webapi/handler_logs.go \
  server/internal/webapi/handler_logs_test.go
git commit -m "feat(webui): save and redact the Xray subscription URL"
```

---

### Task 3: Bot `/import` saves and reuses the URL

**Files:**
- Modify: `server/internal/handler/import.go`
- Modify: `server/internal/handler/import_test.go`
- Modify: `server/internal/handler/misc.go` (`HandleStart` usage line)

**Interfaces:**
- Consumes: `cfg.Xray.SubscriptionURL`, `ConfigStore.UpdateVPNConfig`
- Produces: `/import` with no args uses the saved URL; `/import <url>` writes it in the same `UpdateVPNConfig` that sets `xray.servers`. `/start` documents `/import [url]`.

`mockConfigStore` in `handler/status_test.go` always returns `ErrConfigLoad` from `UpdateVPNConfig` and `nil` from `LoadVPNConfig`. Do **not** change that: it models a router before first configure. Extend `mockConfigStoreForImport` only:

```go
type mockConfigStoreForImport struct {
	mockConfigStore
	savedServers []vpnconfig.Server
	dataDirVal   string
	cfg          *vpnconfig.VPNDirectorConfig
}

func (m *mockConfigStoreForImport) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	if m.cfg != nil {
		return m.cfg, nil
	}
	return m.mockConfigStore.LoadVPNConfig()
}

func (m *mockConfigStoreForImport) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if m.cfg == nil {
		return m.mockConfigStore.UpdateVPNConfig(fn)
	}
	if err := fn(m.cfg); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 1: Write the failing tests**

Change `TestImportHandler_HandleImport_NoURL` so that with an empty config it still contains `Usage`.

Add:

```go
func TestImportHandler_HandleImport_NoArgsUsesSavedURL(t *testing.T) {
	vlessURL := "vless://test-uuid-1234@example.com:443?type=tcp#TestServer"
	encoded := base64.StdEncoding.EncodeToString([]byte(vlessURL))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	config.cfg = &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{SubscriptionURL: server.URL},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = &http.Client{}
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		Text: "/import",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)
	if len(config.savedServers) == 0 {
		t.Fatalf("expected import from saved URL, last message %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_SavesPostedURL(t *testing.T) {
	// same httptest fixture as TestImportHandler_HandleImport_ValidSubscription
	// then assert config.cfg.Xray.SubscriptionURL == server.URL after HandleImport
}
```

If `mockConfigStore.UpdateVPNConfig` does not keep `cfg`, extend it the same way as webapi `mockConfig`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/handler/ -count=1 -run 'TestImportHandler_HandleImport_No'`

Expected: FAIL `NoArgsUsesSavedURL` still shows Usage.

- [ ] **Step 3: Implement**

In `HandleImport`, if `args == ""`, `LoadVPNConfig` and use `SubscriptionURL`; if still empty, send Usage (`/import [url]`) and return. Scheme check stays. After a successful `SaveServers`, in the existing `UpdateVPNConfig` set `vpnCfg.Xray.SubscriptionURL = args` when `args != ""`.

`HandleStart`: `/import \[url\] \- import servers`.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/handler/ -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/import.go \
  server/internal/handler/import_test.go \
  server/internal/handler/misc.go
git commit -m "feat(bot): reuse the saved subscription URL on /import"
```

---

### Task 4: Servers tab re-import

**Files:**
- Modify: `web/src/types.ts` (`ServersResponse`)
- Modify: `web/src/api.ts` (`importServers`)
- Modify: `web/src/components/ServersTab.vue`

**Interfaces:**
- Consumes: `subscription_saved` from Task 2
- Produces: a control that POSTs `{url: ""}` when a URL is saved; pasted URL still replaces it. List Refresh stays a GET.

No Vue unit test harness in this repo. Verify with `go test` for API (already done) plus a manual/dev check in Step 4.

- [ ] **Step 1: Extend the types and API**

```ts
export interface ServersResponse {
  servers: Server[] | null
  active: ActiveServer | null
  subscription_saved?: boolean
}
```

`importServers` already posts `{ url }`. Allow calling it with `''`.

- [ ] **Step 2: Update ServersTab.vue**

```ts
const subscriptionSaved = ref(false)

async function loadServers() {
  loading.value = true
  error.value = ''
  try {
    const resp = await api.getServers()
    servers.value = resp.data.servers ?? []
    active.value = resp.data.active ?? null
    subscriptionSaved.value = !!resp.data.subscription_saved
  } catch (e: any) {
    error.value = e.response?.data?.error || e.message
  } finally {
    loading.value = false
  }
}

async function importServers() {
  if (!importUrl.value && !subscriptionSaved.value) {
    alert('Please enter a subscription URL')
    return
  }
  importLoading.value = true
  try {
    await api.importServers(importUrl.value)
    await loadServers()
  } catch (e: any) {
    alert('Error: ' + (e.response?.data?.error || e.message))
  } finally {
    importLoading.value = false
  }
}
```

Template: keep Import disabled when `!importUrl`. Add next to it:

```html
<button
  class="btn btn-blue"
  :disabled="importLoading || !subscriptionSaved"
  @click="importUrl = ''; importServers()"
>
  {{ importLoading ? '...' : '⬇ Re-import saved' }}
</button>
```

Setting `importUrl = ''` then calling `importServers` uses the saved URL. Do not clear the input if the user had typed a new URL and clicked Import — only the Re-import button clears the field.

- [ ] **Step 3: Typecheck / build the SPA if the repo has a script**

From the repository root: `make build-webui` is heavy (embed). For this task run `cd web && npx vue-tsc --noEmit` if `vue-tsc` is in `web/package.json`; otherwise `cd web && npm run build`. Expected: success.

- [ ] **Step 4: Dev check**

`cd server && go run ./cmd/webui --dev`, open the Servers tab: with no saved URL, Re-import is disabled; after Import of `https://example.invalid/s/test` (will 502 — that is OK) the next GET should show `subscription_saved` only if the import succeeded. If import fails before `syncXrayServers`, saved stays false. That is correct.

- [ ] **Step 5: Commit**

```bash
git add web/src/types.ts web/src/api.ts web/src/components/ServersTab.vue
git commit -m "feat(webui): re-import servers from the saved subscription URL"
```

---

### Task 5: SOCKS probe

**Files:**
- Create: `server/internal/subwatch/probe.go`
- Create: `server/internal/subwatch/probe_test.go`

**Interfaces:**
- Consumes: none
- Produces:

```go
const ProbeURL = "https://www.gstatic.com/generate_204"
const ProbeSuccess = 204

func ProbeSOCKS(ctx context.Context, socksAddr string, probeURL string) error
```

`socksAddr` is `"127.0.0.1:12346"` (host:port). Tests pass a `httptest` URL as `probeURL` and a local SOCKS listener, **or** inject the HTTP client.

To avoid standing up a real SOCKS server in CI, split:

```go
func probeThrough(ctx context.Context, client *http.Client, probeURL string) error
func socksClient(socksAddr string) (*http.Client, error)
func ProbeSOCKS(ctx context.Context, socksAddr string, probeURL string) error
```

`probeThrough` is what tests call. `ProbeSOCKS` builds a SOCKS5 `http.Client` (`golang.org/x/net/proxy`, `DisableKeepAlives: true`, dial `tcp4`) and calls `probeThrough`.

- [ ] **Step 1: Write the failing test**

```go
package subwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeThrough_Requires204(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := probeThrough(ctx, ok.Client(), ok.URL); err != nil {
		t.Fatalf("204: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer bad.Close()
	if err := probeThrough(ctx, bad.Client(), bad.URL); err == nil {
		t.Fatal("200 must fail")
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if err := probeThrough(ctx, down.Client(), down.URL); err == nil {
		t.Fatal("dial error must fail")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1`

Expected: FAIL package not found / `undefined: probeThrough`.

- [ ] **Step 3: Implement**

```go
package subwatch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

const (
	ProbeURL     = "https://www.gstatic.com/generate_204"
	ProbeSuccess = http.StatusNoContent
	probeTimeout = 10 * time.Second
)

func probeThrough(ctx context.Context, client *http.Client, probeURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != ProbeSuccess {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}

func socksClient(socksAddr string) (*http.Client, error) {
	base := &net.Dialer{Timeout: probeTimeout}
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, base)
	if err != nil {
		return nil, err
	}
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks dialer lacks DialContext")
	}
	tr := &http.Transport{
		DialContext:         ctxDialer.DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: probeTimeout,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{Transport: tr, Timeout: probeTimeout}, nil
}

func ProbeSOCKS(ctx context.Context, socksAddr string, probeURL string) error {
	if probeURL == "" {
		probeURL = ProbeURL
	}
	client, err := socksClient(socksAddr)
	if err != nil {
		return err
	}
	return probeThrough(ctx, client, probeURL)
}
```

Do not log the probe URL's host as an error string that could grow to include a token later. Status-only errors are enough.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/subwatch/probe.go server/internal/subwatch/probe_test.go
git commit -m "feat(subwatch): probe Xray SOCKS with generate_204"
```

---

### Task 6: Watch — arming, death timer, failover, notify once

**Files:**
- Create: `server/internal/subwatch/watch.go`
- Create: `server/internal/subwatch/watch_test.go`

**Interfaces:**
- Consumes: Task 1 mutations, Task 5 `ProbeSOCKS` (injected)
- Produces:

```go
const (
	ProbeInterval      = 30 * time.Second
	DeadAfter          = 3 * time.Minute
	ImportRetry        = 5 * time.Minute
	SettleAfterRestart = 3 * time.Second
)

type Watch struct {
	LoadVPN      func() (*vpnconfig.VPNDirectorConfig, error)
	LoadPlatform func() (vpnconfig.PlatformInfo, error)
	UpdateVPN    func(func(*vpnconfig.VPNDirectorConfig) error) error
	Apply        func() error
	RestartXray  func() error
	SaveServers  func([]vpnconfig.Server) error
	Generate     func(vpnconfig.Server) (generated bool, err error)
	Probe        func(ctx context.Context, socksPort int) error
	Fetch        func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	Notify       func(msg string)
	Now          func() time.Time
	AfterRestart func(time.Duration)

	mu           sync.Mutex
	failSince    time.Time // zero => last probe succeeded
	lastImport   time.Time
	lastNote     string
	lastNoteKind noteKind
	running      bool
}

func (w *Watch) Start(ctx context.Context) // idempotent; ticks every ProbeInterval
func (w *Watch) Tick(ctx context.Context)
```

Telegram copy (exact strings; `%s` is `tunnel:ovpnc2` or the server name):

- `Xray outbound is down; LAN clients moved to %s`
- `Xray outbound is down; no Tunnel Director fallback`
- `Subscription updated; selected server %s`
- `Subscription refresh failed; still on %s`
- `No live server in the subscription; still on %s`
- `LAN clients back on Xray; server %s`

`Notify` is not called when `lastNoteKind` is unchanged. For "refresh failed" / "no live server", also skip if kind is unchanged even on a later 5-minute wave (spec §11).

This task implements healthy probing, declaring dead, moving clients (or skipping when no TD), `Apply`, and the two "down" notifies. `Fetch`/`Generate` may be nil: after a move, if `Fetch == nil`, do not import (Task 7 fills that in). Call `Apply` after a successful `UpdateVPNConfig`. If `UpdateVPNConfig` fails, do not pretend failover happened. If `Apply` fails after a successful write, keep `failover` and return; the next `Tick` sees `failover` and does not reverse it.

SOCKS port: `vpnconfig.XrayInboundPorts(cfg)`; if socks is 0, use 12346.

- [ ] **Step 1: Write the failing tests**

```go
package subwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var errProbe = errors.New("probe failed")

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type fake struct {
	mu       sync.Mutex
	cfg      *vpnconfig.VPNDirectorConfig
	plat     vpnconfig.PlatformInfo
	probeErr error
	applies  int
	notes    []string
	now      time.Time
}

func (f *fake) watch() *Watch {
	return &Watch{
		LoadVPN:      func() (*vpnconfig.VPNDirectorConfig, error) { return f.cfg, nil },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return f.plat, nil },
		UpdateVPN: func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
			return fn(f.cfg)
		},
		Apply: func() error { f.applies++; return nil },
		Probe: func(context.Context, int) error { return f.probeErr },
		Notify: func(msg string) { f.notes = append(f.notes, msg) },
		Now:    func() time.Time { return f.now },
	}
}

func baseCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		PausedClients: []string{"192.168.1.9"},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"ovpnc2": {Clients: []string{"192.168.1.3"}},
		}},
		Xray: vpnconfig.XrayConfig{
			Clients:         []string{"192.168.1.8", "192.168.1.9"},
			SubscriptionURL: "https://cdn.example/s/token",
		},
	}
}

func TestTick_NotArmedDoesNothing(t *testing.T) {
	f := &fake{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.1.8"}}}, now: time.Unix(0, 0)}
	f.watch().Tick(context.Background())
	if f.applies != 0 {
		t.Fatal("unarmed")
	}
}

func TestTick_SixFailuresMovesClients(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	for i := 0; i < 6; i++ {
		w.Tick(context.Background())
		f.now = f.now.Add(30 * time.Second)
	}
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("192.168.1.8 not on tunnel")
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("still in xray.clients")
	}
	if contains(f.cfg.Xray.Failover.Clients, "192.168.1.9") {
		t.Fatal("paused client in snapshot")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("foreign client dropped")
	}
	if f.applies != 1 {
		t.Fatalf("applies %d", f.applies)
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
	w.Tick(context.Background())
	if len(f.notes) != 1 {
		t.Fatalf("duplicate notify %v", f.notes)
	}
}

func TestTick_NoTunnelStillNotifiesOnce(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	for i := 0; i < 6; i++ {
		w.Tick(context.Background())
		f.now = f.now.Add(30 * time.Second)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("moved without a tunnel")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("clients should stay")
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; no Tunnel Director fallback" {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_OneFailureDoesNotMove(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	f.watch().Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("too eager")
	}
}

func TestTick_SuccessResetsTimer(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.Tick(context.Background())
	f.now = f.now.Add(30 * time.Second)
	f.probeErr = nil
	w.Tick(context.Background())
	f.probeErr = errProbe
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(30 * time.Second)
		w.Tick(context.Background())
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("timer must reset after a success")
	}
}

func TestTick_FailoverPresentSkipsHealthyProbeRestore(t *testing.T) {
	cfg := baseCfg()
	cfg.Xray.Failover = &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	cfg.Xray.Clients = []string{"192.168.1.9"}
	f := &fake{cfg: cfg, probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	f.watch().Tick(context.Background())
	if f.cfg.Xray.Failover == nil {
		t.Fatal("live SOCKS must not restore")
	}
}
```

Six ticks at 0,30,60,90,120,150 seconds: elapsed at the sixth tick is 150 s, which is **less** than 3 minutes. Spec: dead after 3 minutes, tests say six probes at 30 s.

Count so that `Now().Sub(failSince) >= DeadAfter` on the tick that moves. `failSince` is set on the first failure. After 3 minutes of consecutive failures the **next** tick that still fails should move. Sequence:

- t=0 fail, failSince=0
- t=30,60,90,120,150 fail (elapsed 150s)
- t=180 fail, elapsed 3 min, **move**

That is **7** failing ticks, not 6. Spec §13 says "six consecutive failed probes (3 minutes at 30 s)". Prefer the spec table: 3 minutes. Implement `Now().Sub(failSince) >= DeadAfter` and in the test advance **180 s** of failures (first fail at t=0, move at t=180). Name the test `TestTick_ThreeMinutesMovesClients` and loop until `f.now - start >= DeadAfter` then one more Tick, or tick 7 times. Spec §13 wording "six" is 3 min / 30 s counting intervals; the table in §5 wins: 3 minutes. Use 3 minutes in code and tests.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1 -run TestTick`

Expected: FAIL `undefined: Watch`.

- [ ] **Step 3: Implement `Watch.Tick`**

Defaults in `NewWatch` or at the top of `Tick`: if `Now == nil`, `time.Now`; if `AfterRestart == nil`, `time.Sleep`; if `Probe == nil`, wrap `ProbeSOCKS("127.0.0.1:"+strconv.Itoa(port), ProbeURL)`.

`Start`: if `w.running { return }`; `w.running = true`; ticker `ProbeInterval`; `Tick` immediately then on each tick until `ctx.Done()`. On Done, `w.running = false`.

`Tick` locks `w.mu`. Load cfg; if `!vpnconfig.Armed(cfg)` return. If `cfg.Xray.Failover != nil`, return (Task 7 replaces this branch). Else probe. Success: `w.failSince = time.Time{}`. Failure: if `failSince.IsZero() { failSince = Now() }`; if `Now().Sub(failSince) < DeadAfter` return. Then load platform, `id := vpnconfig.FirstTDExit(cfg, plat)`. If `id == ""`, notify the no-tunnel string once, leave clients, **do not** set failover. If `id != ""`, `UpdateVPNConfig` calling `MoveXrayClientsToTunnel`, then `Apply()`, then notify moved (`tunnel:`+id). Dedup notifies by storing `lastNoteKind`.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/subwatch/watch.go server/internal/subwatch/watch_test.go
git commit -m "feat(subwatch): fail Xray clients over to a TD tunnel after 3m"
```

---

### Task 7: Watch — import, pick server, restore

**Files:**
- Modify: `server/internal/subwatch/watch.go`
- Modify: `server/internal/subwatch/watch_test.go`

**Interfaces:**
- Consumes: `Watch.Fetch`, `SaveServers`, `Generate`, `RestartXray`, `AfterRestart`, `Probe`
- Produces: while `failover != nil` (or just declared dead with no tunnel), import at most once per `ImportRetry`, and immediately on the first dead tick. Same `active_server.name` once, then the list. Restore only after **this walk's** probe succeeds.

Pick order helper (testable):

```go
func pickOrder(servers []vpnconfig.Server, activeName string) []vpnconfig.Server
```

If `activeName` matches one or more imported names, those entries come first (stable, first match in list order), then the rest skipping already-listed names.

Failed `Fetch` must not call `SaveServers`. Failed decode is a `Fetch` error (Fetch returns servers or error). Empty slice is a failed refresh.

Notify:

- success restore: `LAN clients back on Xray; server %s`
- fetch error: `Subscription refresh failed; still on tunnel:%s` (or without "still on …" when no failover tunnel — use `Subscription refresh failed`)
- all probed dead: `No live server in the subscription; still on tunnel:%s`

Do not re-send those two failure kinds while they stay the current kind.

- [ ] **Step 1: Write the failing tests**

```go
func TestPickOrder_SameNameFirst(t *testing.T) {
	in := []vpnconfig.Server{
		{Name: "A", Address: "1.example"},
		{Name: "Oslo", Address: "2.example"},
		{Name: "B", Address: "3.example"},
	}
	got := pickOrder(in, "Oslo")
	if got[0].Name != "Oslo" || got[1].Name != "A" || got[2].Name != "B" {
		t.Fatalf("%v", got)
	}
	got = pickOrder(in, "missing")
	if got[0].Name != "A" {
		t.Fatal("keep list order")
	}
}

func TestTick_ImportAndRestoreOnLiveServer(t *testing.T) {
	f := &fake{
		cfg:  baseCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	f.probeErr = errProbe
	saved := 0
	generated := []string{}
	liveAfter := ""
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { saved++; return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443},
			{Name: "Backup", Address: "b.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		generated = append(generated, s.Name)
		liveAfter = s.Name
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if liveAfter == "Oslo" && f.cfg.Xray.Failover != nil {
			return nil
		}
		return f.probeErr
	}
	start := f.now
	for f.now.Sub(start) <= DeadAfter {
		w.Tick(context.Background())
		f.now = f.now.Add(ProbeInterval)
	}
	w.Tick(context.Background()) // the tick that crosses DeadAfter
	if saved != 1 {
		t.Fatalf("saved %d", saved)
	}
	if len(generated) < 1 || generated[0] != "Oslo" {
		t.Fatalf("generate %v", generated)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("should have restored")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("client not restored")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("foreign client")
	}
}

func TestTick_SameNameDeadWalksList(t *testing.T) {
	// Fetch Oslo + Backup; Probe fails for Oslo, succeeds for Backup.
	// generated names: Oslo, Backup. Restored. Notify mentions Backup.
}

func TestTick_FailedFetchDoesNotSave(t *testing.T) {
	// After failover, Fetch returns error. saved==0. failover remains.
}

func TestTick_ImportRetryWaitsFiveMinutes(t *testing.T) {
	// Fetch error on first dead tick (count 1). Advance 4 minutes, Tick, still 1.
	// Advance to 5 minutes, Tick, count 2.
}
```

Drive time the same way as Task 6. After the move tick, `Fetch` runs in **that same Tick** (do not wait 5 minutes for the first attempt).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1 -run 'TestPickOrder|TestTick_Import|TestTick_SameName|TestTick_FailedFetch|TestTick_ImportRetry'`

Expected: FAIL missing `pickOrder` / no restore.

- [ ] **Step 3: Implement**

In the failover branch (and immediately after a successful move in the same Tick): `maybeImportAndPick`.

```go
func pickOrder(servers []vpnconfig.Server, activeName string) []vpnconfig.Server {
	if activeName == "" {
		return servers
	}
	var first, rest []vpnconfig.Server
	seen := false
	for _, s := range servers {
		if !seen && s.Name == activeName {
			first = append(first, s)
			seen = true
			continue
		}
		rest = append(rest, s)
	}
	return append(first, rest...)
}
```

`maybeImportAndPick`: if `Fetch == nil` return. If `!lastImport.IsZero() && Now().Sub(lastImport) < ImportRetry` return. Set `lastImport = Now()` **before** Fetch so a panic/slow Fetch does not stampede. Fetch; on error notify refresh-failed and return. `SaveServers`. For each server in `pickOrder(servers, cfg.Xray.ActiveServer.Name)` (nil-safe): `Generate`; if `!generated` continue; `RestartXray`; `AfterRestart(SettleAfterRestart)`; `Probe`. On probe success: `UpdateVPNConfig(RestoreXrayClientsFromFailover)`, `Apply`, notify restored with `s.Name`, `failSince = time.Time{}`, return. If none live: notify no-live-server.

When there is no failover (no-tunnel death), still Fetch+pick; on probe success there is nothing to restore, but notify `LAN clients back on Xray; server %s` and reset `failSince` so we do not immediately declare dead again.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/subwatch/ -count=1`

Expected: PASS. Re-run `./internal/vpnconfig/` too.

- [ ] **Step 5: Commit**

```bash
git add server/internal/subwatch/watch.go server/internal/subwatch/watch_test.go
git commit -m "feat(subwatch): refresh the subscription and restore Xray clients"
```

---

### Task 8: Wire the bot, DialPath fetch, docs

**Files:**
- Create: `server/internal/bot/subfetch.go`
- Create: `server/internal/bot/subfetch_test.go`
- Modify: `server/internal/bot/bot.go` (`New`, `Run`, `Bot` fields)
- Modify: `.claude/rules/telegram-bot.md` (architecture tree, commands `/import`, new subsection Subscription watch)
- Modify: `.claude/rules/webui.md` (API table `GET /api/servers` and `POST /api/servers/import`; mention `subscription_url` redaction next to `jwt_secret`)

**Interfaces:**
- Consumes: `subwatch.Watch`, `DialPath`, `vpnconfig.TDExits`, `chatstore.GetActiveUsers`, `telegram.MessageSender`
- Produces: production fetch: SSRF WAN client first; on error, first `TDExits` tunnel `Path` via `DialPath`. Watch started in `bot.New` when `!devMode`, cancelled if `New` fails (same `stopMonitor` as PathManager). `Start` idempotent so `Run` may call it. `--dev` does not start it.

- [ ] **Step 1: Write fetch tests**

`subfetch.go` exports for tests in package `bot`:

```go
func fetchSubscription(ctx context.Context, rawURL string, wan *http.Client, tunnel *http.Client) ([]byte, error)
```

WAN success returns body. WAN error + nil tunnel returns the WAN error. WAN error + tunnel success returns tunnel body. HTTP non-200 is an error and must not return a body to decode. Cap body at 1<<20 like the importer.

Use two `httptest` servers. No real DialPath in this unit test.

Add `TestNew_DevModeDoesNotStartWatch` if `bot.New` is already tested with `WithDevMode`: after New, `b.subWatch == nil`. If `New` needs a live Telegram, skip New and test a small helper `func shouldStartWatch(devMode bool) bool { return !devMode }` only if New is too heavy — prefer asserting on `Bot` after New with `withAPIBase` pointing at a local getMe server (copy the pattern from `bot_test.go`). Read `bot_test.go` and follow its fixture.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./internal/bot/ -count=1 -run 'TestFetchSubscription|TestNew_DevMode'`

Expected: FAIL undefined.

- [ ] **Step 3: Implement fetch + wiring**

`fetchSubscription`: try `wan.Get`; if err or status != 200, if `tunnel == nil` return error; else `tunnel.Get`.

In `bot.New` else-branch (not dev): after PathManager is created, build:

```go
sw := &subwatch.Watch{
	LoadVPN:      configSvc.LoadVPNConfig,
	LoadPlatform: vpnSvc.Platform,
	UpdateVPN:    configSvc.UpdateVPNConfig,
	Apply:        vpnSvc.Apply,
	RestartXray:  vpnSvc.RestartXray,
	SaveServers:  configSvc.SaveServers,
	Generate: func(s vpnconfig.Server) (bool, error) {
		cfg, err := configSvc.LoadVPNConfig()
		if err != nil {
			return false, err
		}
		ports := service.InboundPorts{}
		ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(cfg)
		return service.GenerateAndRecordActiveServer(configSvc, xraySvc, s, ports)
	},
	Probe: func(ctx context.Context, port int) error {
		return subwatch.ProbeSOCKS(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), subwatch.ProbeURL)
	},
	Fetch: func(ctx context.Context, rawURL string) ([]vpnconfig.Server, error) {
		body, err := b.fetchSub(ctx, rawURL, configSvc, vpnSvc)
		if err != nil {
			return nil, err
		}
		parsed, _ := vless.DecodeSubscription(string(body))
		if len(parsed) == 0 {
			return nil, errors.New("no VLESS servers")
		}
		var resolved []vpnconfig.Server
		for _, s := range parsed {
			if err := s.ResolveIPs(); err != nil {
				continue
			}
			resolved = append(resolved, s.ToVPNConfig())
		}
		if len(resolved) == 0 {
			return nil, errors.New("could not resolve IP for any server")
		}
		return resolved, nil
	},
	Notify: func(msg string) {
		if b.chatStore == nil || b.sender == nil {
			return
		}
		users, err := b.chatStore.GetActiveUsers()
		if err != nil {
			return
		}
		for _, u := range users {
			b.sender.SendPlain(u.ChatID, msg)
		}
	},
}
b.subWatch = sw
go sw.Start(monitorCtx)
```

`Notify` is assigned **after** `sender` exists. PathManager starts before `getMe`; the watch can start **after** `sender` is set (still before `return b, nil`) so the first ticks can notify. Cancel `monitorCtx` on failed `New` as today.

`fetchSub` builds wan `ssrf.NewClient(10*time.Second)`. On failure, load cfg+platform, `id := vpnconfig.FirstTDExit(...)`, construct `Path{kind: kindTunnel, id: id, iface, mark}` the same way `candidates` does (iface from platform, mark from `loadTunnelIdxFile(defaultTunnelTablesPath)` + `tunnelMark`). `http.Client{Transport: &http.Transport{DialContext: func(ctx, network, addr) { return DialPath(ctx, p, "tcp4", addr) }, ForceAttemptHTTP2: false}}`.

`Run`: `if b.subWatch != nil { go b.subWatch.Start(ctx) }` (idempotent).

Docs: architecture tree lists `internal/subwatch/` and `bot/subfetch.go`. `/import` line: optional URL. New subsection: armed when URL saved and clients or failover; SOCKS 204; 3 minutes; TD filter; Telegram messages; `--dev` off. webui.md API rows: `GET /api/servers` also `subscription_saved`; `POST /api/servers/import` empty url reuses saved; `/api/config` blanks `subscription_url`.

- [ ] **Step 4: Run tests**

Run: `cd /opt/github/zinin/asuswrt-merlin-vpn-director/server && go test ./... -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/bot/bot.go \
  server/internal/bot/subfetch.go \
  server/internal/bot/subfetch_test.go \
  .claude/rules/telegram-bot.md \
  .claude/rules/webui.md
git commit -m "feat(bot): run the Xray subscription watch next to PathManager"
```

---

## Self-review

**Spec coverage**

| Spec | Task |
|------|------|
| §4 Arming | 1 `Armed`, 6 skip unarmed |
| §5 Timing constants | 6 `DeadAfter` / `ProbeInterval` / `ImportRetry` / `SettleAfterRestart` |
| §6 Probe 204 SOCKS | 5, 8 `ProbeSOCKS` |
| §7 Failover move, TD filter, no Telegram probe | 1 `TDExits` + `path.go`, 6 |
| §7 restart with failover does not restore on live SOCKS | 6 `TestTick_FailoverPresentSkipsHealthyProbeRestore` |
| §8 Import WAN then tunnel | 8 `fetchSub` |
| §8 failed import keeps servers.json | 7 `TestTick_FailedFetchDoesNotSave` |
| §8 same name then walk | 7 `pickOrder` |
| §9 Restore only added addresses | 1 `Move`/`Restore`, 7 |
| §10 Persist + redact + UI re-import | 2, 3, 4 |
| §11 Notify once per state | 6, 7 |
| §12 errors, `--dev` | 6, 7, 8 |
| §13 tests listed | spread across 1–8 |
| §14 device check | not CI; do not SSH unless the owner asks |
| §15 out of scope | Global Constraints |

**Placeholder scan:** no TBD/TODO. `contains` in Task 6 tests is a local helper, not a reference to an undefined production API.

**Type consistency:** `XrayFailover`, `MoveXrayClientsToTunnel`, `Watch.Fetch`, `Watch.Generate`, `ProbeSOCKS(ctx, socksAddr, probeURL)` used with the same names in later tasks.

**Note for executors:** Spec §13 says "six consecutive failed probes"; §5 says 3 minutes. Implement `>= DeadAfter` (3 minutes). Tests advance the fake clock by 3 minutes, not a magic six.
