package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestHandleListClients_OK(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			PausedClients: []string{"192.168.50.20"},
			Xray: vpnconfig.XrayConfig{
				Clients: []string{"192.168.50.10", "192.168.50.20"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
				},
			},
		},
	}

	handler := handleListClients(deps)

	req := httptest.NewRequest("GET", "/api/clients", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Clients []vpnconfig.ClientInfo `json:"clients"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Clients) != 3 {
		t.Fatalf("expected 3 clients, got %d", len(resp.Clients))
	}

	// Check that paused client is marked.
	for _, c := range resp.Clients {
		if c.IP == "192.168.50.20" && !c.Paused {
			t.Error("expected 192.168.50.20 to be paused")
		}
		if c.IP == "192.168.50.10" && c.Paused {
			t.Error("expected 192.168.50.10 to not be paused")
		}
	}
}

func TestHandleListClients_Error(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{err: errors.New("config error")}

	handler := handleListClients(deps)

	req := httptest.NewRequest("GET", "/api/clients", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAddClient_XrayRoute(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Clients: []string{"192.168.50.10"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.20", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.Xray.Clients) != 2 {
		t.Fatalf("expected 2 xray clients, got %d", len(mc.savedCfg.Xray.Clients))
	}
	if mc.savedCfg.Xray.Clients[1] != "192.168.50.20" {
		t.Errorf("expected new client '192.168.50.20', got %q", mc.savedCfg.Xray.Clients[1])
	}
}

func TestHandleAddClient_XrayDuplicate(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Clients: []string{"192.168.50.10"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.10", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "client already configured for xray" {
		t.Errorf("unexpected error text: %q", resp["error"])
	}
	if mc.savedCfg != nil {
		t.Error("config must not be saved on conflict")
	}
	if vpn.applyCalls != 0 {
		t.Errorf("Apply() must not run on conflict, got %d calls", vpn.applyCalls)
	}
}

func TestHandleAddClient_TunnelRoute(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{ExcludeSets: []string{"ru", "us"}},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.30", "route": "wgc1"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	tunnel, ok := mc.savedCfg.TunnelDirector.Tunnels["wgc1"]
	if !ok {
		t.Fatal("expected wgc1 tunnel to be created")
	}
	if len(tunnel.Clients) != 1 || tunnel.Clients[0] != "192.168.50.30" {
		t.Errorf("expected tunnel client [192.168.50.30], got %v", tunnel.Clients)
	}
	// A new tunnel inherits the Xray country exclusions, like the bot wizard.
	// t.Fatalf, not t.Errorf: the aliasing check below indexes Exclude[0].
	if strings.Join(tunnel.Exclude, ",") != "ru,us" {
		t.Fatalf("expected tunnel exclude [ru us], got %v", tunnel.Exclude)
	}
	// The copy must not alias xray.exclude_sets.
	tunnel.Exclude[0] = "changed"
	if mc.savedCfg.Xray.ExcludeSets[0] != "ru" {
		t.Error("tunnel exclude must be a copy of xray.exclude_sets, not the same slice")
	}
}

func TestHandleAddClient_TunnelRouteNilTunnels(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: nil,
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.30", "route": "ovpnc1"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if mc.savedCfg.TunnelDirector.Tunnels == nil {
		t.Fatal("expected tunnels map to be initialized")
	}
	tunnel, ok := mc.savedCfg.TunnelDirector.Tunnels["ovpnc1"]
	if !ok {
		t.Fatal("expected ovpnc1 tunnel to be created")
	}
	if len(tunnel.Clients) != 1 {
		t.Errorf("expected 1 client, got %d", len(tunnel.Clients))
	}
	if tunnel.Exclude == nil {
		t.Error("expected non-nil exclude slice so it marshals to [] rather than null")
	}
}

func TestHandleAddClient_MissingIP(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{},
	}

	handler := handleAddClient(deps)

	body := `{"ip": "", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAddClient_MissingRoute(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{},
	}

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.10", "route": ""}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePauseClient_OK(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
			PausedClients: []string{},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.PausedClients) != 1 || mc.savedCfg.PausedClients[0] != "192.168.50.10" {
		t.Errorf("expected PausedClients=[192.168.50.10], got %v", mc.savedCfg.PausedClients)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("expected 1 Apply() call, got %d", vpn.applyCalls)
	}
}

func TestHandlePauseClient_AlreadyPaused(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
			PausedClients: []string{"192.168.50.10"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Should not add duplicate.
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.PausedClients) != 1 {
		t.Fatalf("expected 1 paused client (no duplicate), got %d", len(mc.savedCfg.PausedClients))
	}
	if mc.savedCfg.PausedClients[0] != "192.168.50.10" {
		t.Errorf("expected the entry to keep the stored form, got %q", mc.savedCfg.PausedClients[0])
	}
}

// master's Web UI accepted IPv6, and a hand-edited config can hold anything.
// GET /api/clients still lists such an entry, so rejecting it here would leave
// it visible and undeletable.
func TestHandleDeleteClient_RemovesAnEntryThatFailsNormalization(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{Clients: []string{"2001:db8::1", "192.168.50.10"}},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{}

	req := httptest.NewRequest("DELETE", "/api/clients?ip=2001:db8::1", nil)
	rec := httptest.NewRecorder()
	handleDeleteClient(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.Xray.Clients) != 1 || mc.savedCfg.Xray.Clients[0] != "192.168.50.10" {
		t.Errorf("clients = %v, want only the remaining IPv4 entry", mc.savedCfg.Xray.Clients)
	}
}

// The pass-through only exists for entries that are actually configured;
// anything else must still stop before the config is touched.
// An address can sit in two routes in two spellings; the shell subtracts
// paused_clients literally, so pausing has to name both. Keeping only the
// first would resume the tunnel client while answering 200.
func TestHandlePauseClient_KeepsEverySpellingPaused(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.10/32"}},
				},
			},
			PausedClients: []string{"192.168.50.10", "192.168.50.10/32"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{}

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()
	handlePauseClient(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	got := make(map[string]int)
	for _, e := range mc.savedCfg.PausedClients {
		got[e]++
	}
	for _, want := range []string{"192.168.50.10", "192.168.50.10/32"} {
		if got[want] != 1 {
			t.Errorf("paused_clients = %v, want %q exactly once", mc.savedCfg.PausedClients, want)
		}
	}
}

func TestHandlePauseClient_UnparseableAddressThatIsNotConfigured(t *testing.T) {
	// 404, not 400: the address is looked up now instead of validated, so an
	// entry an older build stored stays reachable and a typo still stops here.
	for _, ip := range []string{"::1", "not-an-address"} {
		t.Run(ip, func(t *testing.T) {
			mc := &mockConfig{
				cfg: &vpnconfig.VPNDirectorConfig{
					Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
				},
			}
			deps := newTestDeps(t)
			deps.Config = mc

			req := httptest.NewRequest("POST", "/api/clients/pause?ip="+ip, nil)
			rec := httptest.NewRecorder()
			handlePauseClient(deps).ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
			}
			if mc.savedCfg != nil {
				t.Error("a miss must not write the config")
			}
		})
	}
}

func TestHandlePauseClient_MissingIP(t *testing.T) {
	deps := newTestDeps(t)

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleResumeClient_OK(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10", "192.168.50.20"}},
			PausedClients: []string{"192.168.50.10", "192.168.50.20"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleResumeClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/resume?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.PausedClients) != 1 {
		t.Fatalf("expected 1 paused client after resume, got %d", len(mc.savedCfg.PausedClients))
	}
	if mc.savedCfg.PausedClients[0] != "192.168.50.20" {
		t.Errorf("expected remaining paused client '192.168.50.20', got %q", mc.savedCfg.PausedClients[0])
	}
}

func TestHandleResumeClient_MissingIP(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleResumeClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/resume", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteClient_OK(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Clients: []string{"192.168.50.10", "192.168.50.20"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {
						Clients: []string{"192.168.50.10", "192.168.50.30"},
						Exclude: []string{"ru"},
					},
				},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleDeleteClient(deps)

	req := httptest.NewRequest("DELETE", "/api/clients?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}

	// Removed from Xray.
	if len(mc.savedCfg.Xray.Clients) != 1 || mc.savedCfg.Xray.Clients[0] != "192.168.50.20" {
		t.Errorf("expected Xray.Clients=[192.168.50.20], got %v", mc.savedCfg.Xray.Clients)
	}

	// Removed from tunnel.
	tunnel := mc.savedCfg.TunnelDirector.Tunnels["wgc1"]
	if len(tunnel.Clients) != 1 || tunnel.Clients[0] != "192.168.50.30" {
		t.Errorf("expected wgc1 clients=[192.168.50.30], got %v", tunnel.Clients)
	}
	// Tunnel config preserved (exclude still there).
	if len(tunnel.Exclude) != 1 {
		t.Errorf("expected wgc1 exclude preserved, got %v", tunnel.Exclude)
	}
}

func TestHandleDeleteClient_MissingIP(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleDeleteClient(deps)

	req := httptest.NewRequest("DELETE", "/api/clients", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAddClient_ConflictOtherRoute(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.10/32"}, Exclude: []string{"ru"}},
				},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.10", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "client already configured for wgc1" {
		t.Errorf("unexpected error text: %q", resp["error"])
	}
	if mc.savedCfg != nil {
		t.Error("config must not be saved on conflict")
	}
	if vpn.applyCalls != 0 {
		t.Errorf("Apply() must not run on conflict, got %d calls", vpn.applyCalls)
	}
}

func TestHandleAddClient_StripsSlash32(t *testing.T) {
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.40/32", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(mc.savedCfg.Xray.Clients, ","); got != "192.168.50.40" {
		t.Errorf("expected /32 stripped, got %q", got)
	}
}

func TestHandleAddClient_RejectsIPv6(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}

	handler := handleAddClient(deps)

	body := `{"ip": "::1", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "invalid IPv4 address or CIDR" {
		t.Errorf("unexpected error text: %q", resp["error"])
	}
}

func TestHandleAddClient_AppliesAfterSave(t *testing.T) {
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.50", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if vpn.applyCalls != 1 {
		t.Errorf("expected 1 Apply() call after save, got %d", vpn.applyCalls)
	}
}

func TestHandleAddClient_SavedButApplyFailed(t *testing.T) {
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps := newTestDeps(t)
	deps.Config = mc
	// Shell failures carry the whole script output; lastErrorLine echoes only
	// its last line, so the fixture spans lines like the real apply output.
	deps.VPN = &mockVPN{err: errors.New("apply failed (exit 1):\n[ERROR] iptables missing")}

	handler := handleAddClient(deps)

	body := `{"ip": "192.168.50.50", "route": "xray"}`
	req := httptest.NewRequest("POST", "/api/clients", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved even though apply failed")
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["saved"] != true {
		t.Errorf("expected saved: true, got %v", resp["saved"])
	}
	if resp["error"] != "configuration saved, but apply failed: [ERROR] iptables missing" {
		t.Errorf("unexpected error text: %v", resp["error"])
	}
}

func TestHandlePauseClient_NotFound(t *testing.T) {
	deps := newTestDeps(t)
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps.Config = mc

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.99", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg != nil {
		t.Error("config must not be saved for an unknown client")
	}
}

func TestHandlePauseClient_KeepsStoredForm(t *testing.T) {
	// The shell subtracts paused_clients from the clients arrays by exact
	// string, so the paused entry must use the stored spelling (here with /32).
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.20/32"}, Exclude: []string{"ru"}},
				},
			},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.20", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(mc.savedCfg.PausedClients, ","); got != "192.168.50.20/32" {
		t.Errorf("expected paused entry in stored form 192.168.50.20/32, got %q", got)
	}
}

func TestHandlePauseClient_RepairsMismatchedSpelling(t *testing.T) {
	// paused_clients disagrees with the clients array about the spelling, the
	// state a bot /configure run used to leave behind. The shell subtracts by
	// exact string and CollectClients reports Paused by exact lookup, so the
	// stale entry pauses nothing. Pausing must replace it with the stored
	// spelling, not see it as equivalent and skip the write.
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
			PausedClients: []string{"192.168.50.10/32"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handlePauseClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/pause?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if mc.savedCfg == nil {
		t.Fatal("expected config to be saved")
	}
	if len(mc.savedCfg.PausedClients) != 1 {
		t.Fatalf("expected exactly 1 paused entry, got %v", mc.savedCfg.PausedClients)
	}
	if mc.savedCfg.PausedClients[0] != "192.168.50.10" {
		t.Errorf("expected the stale /32 entry replaced by the stored form 192.168.50.10, got %q", mc.savedCfg.PausedClients[0])
	}
}

func TestHandleResumeClient_MatchesStoredForm(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.20/32"}, Exclude: []string{"ru"}},
				},
			},
			PausedClients: []string{"192.168.50.20/32"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleResumeClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/resume?ip=192.168.50.20", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mc.savedCfg.PausedClients) != 0 {
		t.Errorf("expected paused list empty, got %v", mc.savedCfg.PausedClients)
	}
}

func TestHandleResumeClient_NotFound(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{PausedClients: []string{"192.168.50.10"}}}

	handler := handleResumeClient(deps)

	req := httptest.NewRequest("POST", "/api/clients/resume?ip=192.168.50.10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a paused entry that is no longer a client, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteClient_NotFound(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}

	handler := handleDeleteClient(deps)

	req := httptest.NewRequest("DELETE", "/api/clients?ip=192.168.50.99", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteClient_RemovesStoredForm(t *testing.T) {
	mc := &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.20/32", "192.168.50.30"}, Exclude: []string{"ru"}},
				},
			},
			PausedClients: []string{"192.168.50.20/32"},
		},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	vpn := &mockVPN{}
	deps.VPN = vpn

	handler := handleDeleteClient(deps)

	req := httptest.NewRequest("DELETE", "/api/clients?ip=192.168.50.20", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(mc.savedCfg.TunnelDirector.Tunnels["wgc1"].Clients, ","); got != "192.168.50.30" {
		t.Errorf("expected wgc1 clients [192.168.50.30], got %q", got)
	}
	if len(mc.savedCfg.PausedClients) != 0 {
		t.Errorf("expected paused list cleared, got %v", mc.savedCfg.PausedClients)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("expected 1 Apply() call, got %d", vpn.applyCalls)
	}
}

func TestRemoveAddr(t *testing.T) {
	got := removeAddr([]string{"192.168.50.20/32", "192.168.50.30", "192.168.50.20", "garbage"}, "192.168.50.20")
	if strings.Join(got, ",") != "192.168.50.30,garbage" {
		t.Errorf("removeAddr = %v, want [192.168.50.30 garbage]", got)
	}
	if !containsAddr([]string{"1.2.3.4/32"}, "1.2.3.4") {
		t.Error("containsAddr must match the normalized form")
	}
	if containsAddr(nil, "1.2.3.4") {
		t.Error("containsAddr on nil must be false")
	}
}

func TestHandleAddClient_RouteMustBeATunnelThePlatformLists(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps.VPN = &mockVPN{platform: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "OpenVPN0"}}}}

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"wgc1"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid route") {
		t.Fatalf("wgc1 on a Keenetic: status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"OpenVPN0"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("OpenVPN0: status = %d: %s", rec.Code, rec.Body.String())
	}
	saved := deps.Config.(*mockConfig).savedCfg
	if saved == nil || len(saved.TunnelDirector.Tunnels["OpenVPN0"].Clients) != 1 {
		t.Errorf("saved = %+v", saved)
	}
}

func TestHandleAddClient_XrayNeedsNoPlatform(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps.VPN = &mockVPN{platformErr: errors.New("down")}

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"xray"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

// ClientsTab offers exactly the configured tunnels when the platform cannot be
// asked, and the bot and the wizard accept one in that state; the API was the
// one path that still answered 503 for it.
func TestHandleAddClient_ConfiguredTunnelNeedsNoPlatform(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{"OpenVPN0": {Clients: []string{"192.168.1.9"}}},
		},
	}}
	deps.VPN = &mockVPN{platformErr: errors.New("down")}

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"OpenVPN0"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	saved := deps.Config.(*mockConfig).savedCfg
	if saved == nil || len(saved.TunnelDirector.Tunnels["OpenVPN0"].Clients) != 2 {
		t.Errorf("saved = %+v", saved)
	}
}

// The same RCI outage can also arrive as a successful platform document with
// no tunnels at all (spec 13): not an invalid route, an answer that cannot
// validate one.
func TestHandleAddClient_EmptyPlatformAnswerIs503(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps.VPN = &mockVPN{platform: vpnconfig.PlatformInfo{}}

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"OpenVPN0"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "platform info unavailable") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.(*mockConfig).savedCfg != nil {
		t.Error("nothing may be saved when the route could not be validated")
	}
}

func TestHandleAddClient_PlatformUnavailableIs503(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}}
	deps.VPN = &mockVPN{platformErr: errors.New("down")}

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients",
		strings.NewReader(`{"ip":"192.168.1.5","route":"OpenVPN0"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "platform info unavailable") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.(*mockConfig).savedCfg != nil {
		t.Error("nothing may be saved when the route could not be validated")
	}
}

// failedOverClients is a committed failover of 192.168.50.8 onto wgc1.
func failedOverClients() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{
			Failover: &vpnconfig.XrayFailover{
				Tunnel: "wgc1", Clients: []string{"192.168.50.8"}, Added: []string{"192.168.50.8"}, Committed: true,
			},
		},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{"192.168.50.3", "192.168.50.8"}, Exclude: []string{"ru"}},
			},
		},
	}
}

// Deleting an address during a failover is a decision about it: the restore
// must not bring it back, and an address added again later is a new client,
// not the old snapshot.
func TestHandleDeleteClient_DetachesTheAddressFromTheFailover(t *testing.T) {
	mc := &mockConfig{cfg: failedOverClients()}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleDeleteClient(deps).ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/clients?ip=192.168.50.8", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	fo := mc.savedCfg.Xray.Failover
	if fo == nil || len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Fatalf("failover %+v; the deleted address is still one the restore brings back", fo)
	}
}

// Putting an address on a tunnel during a failover is where the user wants it.
// Left in the failover record, the restore took it off that tunnel and back to
// Xray.
func TestHandleAddClient_DetachesTheAddressFromTheFailover(t *testing.T) {
	cfg := failedOverClients()
	// Deleted by an earlier build, which left the record as it was.
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.50.3"}, Exclude: []string{"ru"}}
	mc := &mockConfig{cfg: cfg}
	deps := newTestDeps(t)
	deps.Config = mc

	rec := httptest.NewRecorder()
	handleAddClient(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/clients", strings.NewReader(`{"ip": "192.168.50.8", "route": "wgc1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	fo := mc.savedCfg.Xray.Failover
	if fo == nil || len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Fatalf("failover %+v; the restore would take the address off the tunnel it was just put on", fo)
	}
	if !strings.Contains(strings.Join(mc.savedCfg.TunnelDirector.Tunnels["wgc1"].Clients, ","), "192.168.50.8") {
		t.Fatalf("wgc1 %v", mc.savedCfg.TunnelDirector.Tunnels["wgc1"].Clients)
	}
}
