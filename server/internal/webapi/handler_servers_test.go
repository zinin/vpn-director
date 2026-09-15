package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestHandleListServers_OK(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "server1.example.com", Port: 443, UUID: "uuid-1", Name: "Server 1", IPs: []string{"1.1.1.1"}},
			{Address: "server2.example.com", Port: 443, UUID: "uuid-2", Name: "Server 2", IPs: []string{"2.2.2.2"}},
		},
	}

	handler := handleListServers(deps)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Servers []vpnconfig.Server `json:"servers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(resp.Servers))
	}
	if resp.Servers[0].Name != "Server 1" {
		t.Errorf("expected 'Server 1', got %q", resp.Servers[0].Name)
	}
}

func TestHandleListServers_Error(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{err: errors.New("load failed")}

	handler := handleListServers(deps)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSelectServer_OK(t *testing.T) {
	mc := &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
			{Address: "s2.example.com", Port: 443, UUID: "uuid-2", Name: "S2", IPs: []string{"2.2.2.2"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Servers: []string{"old-ip"},
			},
		},
	}

	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleSelectServer(deps)

	body := `{"index": 1}`
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]bool
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp["ok"] {
		t.Error("expected ok: true")
	}

	// Verify saved config has ALL servers' IPs (parity with the import path):
	// xray.servers feeds the TPROXY bypass set, so every server endpoint must
	// stay present after a switch, not just the selected one.
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if joined := strings.Join(mc.savedCfg.Xray.Servers, ","); joined != "1.1.1.1,2.2.2.2" {
		t.Errorf("expected Xray.Servers to be all servers' IPs 1.1.1.1,2.2.2.2, got %q", joined)
	}
}

func TestHandleSelectServer_IndexOutOfRange(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		},
	}

	handler := handleSelectServer(deps)

	body := `{"index": 5}`
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSelectServer_NegativeIndex(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		},
	}

	handler := handleSelectServer(deps)

	body := `{"index": -1}`
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSelectServer_RestartXraySurfacesTheShellLine(t *testing.T) {
	mc := &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{err: errors.New("xray: failed\nlast line of init")}

	handler := handleSelectServer(deps)
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index":0}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "last line of init") {
		t.Errorf("body %s, want lastErrorLine of the shell error", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to restart xray") {
		t.Errorf("body %s, want the restart prefix", rec.Body.String())
	}
}

func TestHandleSelectServer_InvalidJSON(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleSelectServer(deps)

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleImportServers_InvalidURL(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleImportServers(deps)

	body := `{"url": "http://example.com/sub"}`
	req := httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "only https URLs are allowed" {
		t.Errorf("unexpected error: %q", resp["error"])
	}
}

func TestHandleImportServers_EmptyURL(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleImportServers(deps)

	body := `{"url": ""}`
	req := httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleImportServers_PrivateIP(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleImportServers(deps)

	body := `{"url": "https://192.168.1.1/sub"}`
	req := httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "private") {
		t.Errorf("expected private IP error, got %q", resp["error"])
	}
}

func TestHandleImportServers_LoopbackIP(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleImportServers(deps)

	body := `{"url": "https://127.0.0.1/sub"}`
	req := httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCollectServerIPs(t *testing.T) {
	servers := []vpnconfig.Server{
		{IPs: []string{"2.2.2.2", "1.1.1.1"}},
		{IPs: []string{"1.1.1.1", "3.3.3.3"}}, // 1.1.1.1 is a duplicate
		{IPs: []string{""}},                   // empty IP skipped
	}

	got := collectServerIPs(servers)

	if joined := strings.Join(got, ","); joined != "1.1.1.1,2.2.2.2,3.3.3.3" {
		t.Errorf("collectServerIPs = %q, want sorted deduped 1.1.1.1,2.2.2.2,3.3.3.3", joined)
	}
}

func TestCollectServerIPs_EmptyMarshalsToJSONArray(t *testing.T) {
	// With no usable IPs the result must marshal to an empty JSON array, not
	// null: it is persisted as xray.servers in vpn-director.json, where null
	// reads differently than [] for consumers that don't apply a `// []` default.
	cases := map[string][]vpnconfig.Server{
		"nil-slice": nil,
		"empty-IPs": {{IPs: []string{""}}},
	}
	for name, servers := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(collectServerIPs(servers))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != "[]" {
				t.Errorf("collectServerIPs(%s) marshals to %s, want []", name, b)
			}
		})
	}
}

func TestDownloadErrMessage(t *testing.T) {
	// A blocked-address error must NOT echo the resolved internal IP back to the
	// client (the dial guard wraps ssrf.ErrBlockedAddress around the IP).
	blocked := fmt.Errorf(`Get "https://evil.test": %w: 10.0.0.1`, ssrf.ErrBlockedAddress)
	if got := downloadErrMessage(blocked); strings.Contains(got, "10.0.0.1") {
		t.Errorf("downloadErrMessage leaked the resolved IP: %q", got)
	}
	// Non-SSRF errors keep their detail for diagnostics.
	other := errors.New("dial tcp: i/o timeout")
	if got := downloadErrMessage(other); !strings.Contains(got, "i/o timeout") {
		t.Errorf("downloadErrMessage dropped diagnostic detail: %q", got)
	}
}

func TestSyncXrayServers_SaveError(t *testing.T) {
	mc := &mockConfig{
		cfg:           &vpnconfig.VPNDirectorConfig{},
		saveVPNCfgErr: errors.New("disk full"),
	}

	err := syncXrayServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

	if err == nil {
		t.Fatal("expected error when saving config fails, got nil")
	}
}

func TestSyncXrayServers_LoadError(t *testing.T) {
	mc := &mockConfig{err: errors.New("load failed")}

	err := syncXrayServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

	if err == nil {
		t.Fatal("expected error when LoadVPNConfig fails, got nil")
	}
}

func TestSyncXrayServers_Success(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{Servers: []string{"stale-ip"}},
		},
	}

	err := syncXrayServers(mc, []vpnconfig.Server{
		{IPs: []string{"2.2.2.2"}},
		{IPs: []string{"1.1.1.1"}},
	}, "")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if joined := strings.Join(mc.savedCfg.Xray.Servers, ","); joined != "1.1.1.1,2.2.2.2" {
		t.Errorf("Xray.Servers = %q, want sorted all IPs 1.1.1.1,2.2.2.2", joined)
	}
}

func TestSyncXrayServers_MissingConfigIsSurfaced(t *testing.T) {
	// An absent vpn-director.json used to be skipped silently. UpdateVPNConfig
	// reports it as service.ErrConfigLoad, and the import handler turns that
	// into "servers saved, but xray.servers sync failed": a stale xray.servers
	// leaves the proxy's own endpoints out of the TPROXY bypass set, which the
	// caller must learn about rather than read as success.
	mc := &mockConfig{cfg: nil}

	err := syncXrayServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

	if !errors.Is(err, service.ErrConfigLoad) {
		t.Fatalf("expected a service.ErrConfigLoad failure when config is absent, got %v", err)
	}
	if mc.savedCfg != nil {
		t.Error("expected no save when config is absent")
	}
}

func TestNoServersMessage(t *testing.T) {
	if got := noServersMessage(nil); got != "no VLESS servers found in subscription" {
		t.Errorf("no errors: got %q", got)
	}
	two := []error{errors.New("line 1: bad scheme"), errors.New("line 2: missing uuid")}
	want := "no VLESS servers found in subscription: line 1: bad scheme; line 2: missing uuid"
	if got := noServersMessage(two); got != want {
		t.Errorf("two errors: got %q, want %q", got, want)
	}
	five := []error{errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4"), errors.New("e5")}
	if got := noServersMessage(five); got != "no VLESS servers found in subscription: e1; e2; e3" {
		t.Errorf("five errors must be capped at three: got %q", got)
	}
}

// The selection is recorded nowhere else. config.json holds only the outbound,
// and a subscription puts many names behind one address:port - on the router
// this was written for, eight names share the running endpoint - so the choice
// has to be written down at the moment it is made.
func TestHandleSelectServer_RecordsTheSelection(t *testing.T) {
	mc := &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "Амстердам", IPs: []string{"1.1.1.1"}},
			{Address: "s2.example.com", Port: 8443, UUID: "uuid-2", Name: "Берлин", IPs: []string{"2.2.2.2"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleSelectServer(deps)

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index": 1}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	active := mc.cfg.Xray.ActiveServer
	if active == nil {
		t.Fatal("nothing was recorded; the UI has no other way to name the running server")
	}
	if active.Name != "Берлин" || active.Address != "s2.example.com" || active.Port != 8443 {
		t.Errorf("recorded %+v, want the server at index 1", *active)
	}
}

// A server whose parameters Xray rejects - an incomplete REALITY record out of
// a subscription is the usual way - leaves the old config.json running. Naming
// it as active would be a plain lie, and the caller has already been told the
// switch failed.
func TestHandleSelectServer_RecordsNothingWhenGenerationFails(t *testing.T) {
	mc := &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "Амстердам", IPs: []string{"1.1.1.1"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.Xray = &mockXray{err: errors.New("reality requires a public key")}

	handler := handleSelectServer(deps)

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index": 0}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if active := mc.cfg.Xray.ActiveServer; active != nil {
		t.Errorf("recorded %+v after the config failed to generate", *active)
	}
}

func TestHandleListServers_ReportsTheActiveServer(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, Name: "Амстердам", IPs: []string{"1.1.1.1"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				ActiveServer: &vpnconfig.ActiveServer{Name: "Амстердам", Address: "s1.example.com", Port: 443},
			},
		},
	}

	handler := handleListServers(deps)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Active *vpnconfig.ActiveServer `json:"active"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Active == nil {
		t.Fatal("expected the response to name the active server")
	}
	if resp.Active.Name != "Амстердам" || resp.Active.Port != 443 {
		t.Errorf("active = %+v, want the recorded server", *resp.Active)
	}
}

// An install that predates the field, or one where nobody has picked a server
// yet, must come back as null rather than as some server the client then
// presents as running.
func TestHandleListServers_ActiveIsNullWhenNothingIsRecorded(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		servers: []vpnconfig.Server{{Address: "s1.example.com", Port: 443, Name: "Амстердам"}},
		cfg:     &vpnconfig.VPNDirectorConfig{},
	}

	handler := handleListServers(deps)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, ok := resp["active"]; !ok || string(got) != "null" {
		t.Errorf("active = %s (present: %v), want null", got, ok)
	}
}

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
