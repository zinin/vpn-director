package webapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	body := `{"index": 1, "name": "S2", "address": "s2.example.com", "port": 443}`
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

// The page names the server it showed at that index. A refresh since - the bot's
// subscription watch rotates endpoints, an import in another tab replaces the
// list - puts another server there, and switching to it is a switch the user
// never asked for.
func TestHandleSelectServer_RefusesAnIndexTheListNoLongerMatches(t *testing.T) {
	mc := &mockConfig{
		servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
			{Address: "s3.example.com", Port: 443, UUID: "uuid-3", Name: "S3", IPs: []string{"3.3.3.3"}},
		},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	body := `{"index": 1, "name": "S2", "address": "s2.example.com", "port": 443}`
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleSelectServer(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "server list changed") {
		t.Errorf("body %s, want the reason", rec.Body.String())
	}
	if mc.savedCfg != nil || mc.cfg.Xray.ActiveServer != nil {
		t.Errorf("config written for a refused switch: %+v", mc.savedCfg)
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
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index":0,"name":"S1","address":"s1.example.com","port":443}`))
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

func TestHandleImportServers_EmptyURLUsesSavedURL(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://127.0.0.1/s/token"},
	}}

	handler := handleImportServers(deps)

	req := httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(`{"url":""}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// The loopback refusal comes before any network access and proves the
	// saved link was the one checked.
	if resp["error"] != "URL must not point to private or loopback addresses" {
		t.Errorf("error %q, want the private/loopback refusal of the saved URL", resp["error"])
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

func TestDownloadErrMessage_DropsURL(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://cdn.example/s/SECRET-TOKEN", Err: errors.New("timeout")}
	got := downloadErrMessage(err)
	if strings.Contains(got, "SECRET-TOKEN") {
		t.Errorf("downloadErrMessage leaked the subscription URL: %q", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("downloadErrMessage dropped diagnostic detail: %q", got)
	}
}

func TestSyncXrayServers_SaveError(t *testing.T) {
	mc := &mockConfig{
		cfg:           &vpnconfig.VPNDirectorConfig{},
		saveVPNCfgErr: errors.New("disk full"),
	}

	err := service.PublishServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

	if err == nil {
		t.Fatal("expected error when saving config fails, got nil")
	}
}

func TestSyncXrayServers_LoadError(t *testing.T) {
	mc := &mockConfig{err: errors.New("load failed")}

	err := service.PublishServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

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

	err := service.PublishServers(mc, []vpnconfig.Server{
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

	err := service.PublishServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")

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

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index": 1, "name": "Берлин", "address": "s2.example.com", "port": 8443}`))
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

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"index": 0, "name": "Амстердам", "address": "s1.example.com", "port": 443}`))
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
	if err := service.PublishServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "https://cdn.example/s/token"); err != nil {
		t.Fatal(err)
	}
	if mc.savedCfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatalf("got %q", mc.savedCfg.Xray.SubscriptionURL)
	}
	if err := service.PublishServers(mc, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if mc.savedCfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatal("empty url must not clear the saved link")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// subscriptionHost serves body as every subscription and returns a client that
// takes every request there, whatever host the URL names: the import's own
// checks see the public address a test posts.
func subscriptionHost(t *testing.T, body string) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})}
}

var osloSubscription = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))

// postImport posts body to the import handler and decodes the answer.
func postImport(t *testing.T, deps *Deps, body string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleImportServers(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/servers/import", strings.NewReader(body)))
	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rec.Code, resp
}

// A subscription over the 1 MiB cap was cut at the cap and decoded anyway, and
// a list cut short was published as the subscription. The bot's watch refuses
// such a body; so does the Web UI.
func TestHandleImportServers_RefusesASubscriptionOverTheCap(t *testing.T) {
	line := "vless://uuid-1@203.0.113.10:443?type=tcp#Oslo\n"
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.ImportClient = subscriptionHost(t, base64.StdEncoding.EncodeToString([]byte(strings.Repeat(line, (1<<20)/len(line)))))

	code, resp := postImport(t, deps, `{"url":"https://93.184.216.34/s/token"}`)

	if code != http.StatusBadGateway || resp["error"] != "subscription exceeds 1 MiB" {
		t.Fatalf("got %d %v, want 502 and the cap", code, resp)
	}
	if mc.savedServers != nil {
		t.Fatalf("saved %d servers out of a list cut at the cap", len(mc.savedServers))
	}
}

// "servers saved" is said only when servers.json was written. A publication that
// failed before it - a lock that could not be opened, a re-import against a
// config it could not read - wrote nothing, and was reported as saved.
func TestHandleImportServers_SaysSavedOnlyWhenTheListWasWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mc    *mockConfig
		body  string
		saved bool
	}{
		{
			name: "lock that cannot be opened",
			mc:   &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, updateErr: errors.New("open config lock: permission denied")},
			body: `{"url":"https://93.184.216.34/s/token"}`,
		},
		{
			name: "re-import against a config that does not load",
			mc: &mockConfig{
				cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://93.184.216.34/s/token"}},
				err: errors.New("invalid character 'x' looking for beginning of value"),
			},
			body: `{"url":""}`,
		},
		{
			name:  "config write that fails after the list",
			mc:    &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, saveVPNCfgErr: errors.New("disk full")},
			body:  `{"url":"https://93.184.216.34/s/token"}`,
			saved: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestDeps(t)
			deps.Config = tc.mc
			deps.ImportClient = subscriptionHost(t, osloSubscription)

			code, resp := postImport(t, deps, tc.body)

			msg, _ := resp["error"].(string)
			if code != http.StatusInternalServerError {
				t.Fatalf("got %d %v, want 500", code, resp)
			}
			if said := strings.Contains(msg, "servers saved"); said != tc.saved {
				t.Fatalf("error %q; servers.json written: %v", msg, tc.saved)
			}
			if wrote := tc.mc.savedServers != nil; wrote != tc.saved {
				t.Fatalf("servers.json written: %v, want %v", wrote, tc.saved)
			}
		})
	}
}
