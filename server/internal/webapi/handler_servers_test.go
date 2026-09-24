package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestHandleListServers_OK(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "server1.example.com", Port: 443, UUID: "uuid-1", Name: "Server 1", IPs: []string{"1.1.1.1"}},
			{Address: "server2.example.com", Port: 443, UUID: "uuid-2", Name: "Server 2", IPs: []string{"2.2.2.2"}},
		}}},
	}

	handler := handleListServers(deps)

	req := httptest.NewRequest("GET", "/api/servers", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Subscriptions []struct {
			Servers []vpnconfig.Server `json:"servers"`
		} `json:"subscriptions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Subscriptions) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(resp.Subscriptions))
	}
	if len(resp.Subscriptions[0].Servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(resp.Subscriptions[0].Servers))
	}
	if resp.Subscriptions[0].Servers[0].Name != "Server 1" {
		t.Errorf("expected 'Server 1', got %q", resp.Subscriptions[0].Servers[0].Name)
	}
}

// The page gets what it shows - and no credential of the record: an import
// now stores passwords in the outbound, beside the UUID of a legacy record.
func TestHandleListServers_ShowsTheProtocolAndNoCredentials(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
		{Address: "legacy.example.com", Port: 443, UUID: "secret-uuid", Name: "Legacy", IPs: []string{"1.1.1.1"}, Security: "reality", PublicKey: "secret-key"},
		{Address: "hy.example.com", Port: 8443, Name: "Hy", IPs: []string{"2.2.2.2"},
			Outbound: json.RawMessage(`{"protocol":"hysteria","settings":{"address":"hy.example.com","port":8443},"streamSettings":{"hysteriaSettings":{"auth":"secret-auth"}}}`)},
	}}}}
	rec := httptest.NewRecorder()
	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))

	body := rec.Body.String()
	for _, secret := range []string{"secret-uuid", "secret-key", "secret-auth", "outbound"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response carries %q: %s", secret, body)
		}
	}
	var resp struct {
		Subscriptions []struct {
			Servers []map[string]interface{} `json:"servers"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Subscriptions) != 1 {
		t.Fatalf("subscriptions %v", resp.Subscriptions)
	}
	servers := resp.Subscriptions[0].Servers
	if len(servers) != 2 || servers[0]["protocol"] != "vless·reality" || servers[1]["protocol"] != "hysteria2" {
		t.Fatalf("servers %v", servers)
	}
}

func TestHandleListServers_Error(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{subsErr: errors.New("load failed")}

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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
			{Address: "s2.example.com", Port: 443, UUID: "uuid-2", Name: "S2", IPs: []string{"2.2.2.2"}},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Servers: []string{"old-ip"},
			},
		},
	}

	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleSelectServer(deps)

	body := `{"subscription":"0a1b2c3d","index": 1, "name": "S2", "address": "s2.example.com", "port": 443}`
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
			{Address: "s3.example.com", Port: 443, UUID: "uuid-3", Name: "S3", IPs: []string{"3.3.3.3"}},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	body := `{"subscription":"0a1b2c3d","index": 1, "name": "S2", "address": "s2.example.com", "port": 443}`
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		}}},
	}

	handler := handleSelectServer(deps)

	body := `{"subscription":"0a1b2c3d","index": 5}`
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		}}},
	}

	handler := handleSelectServer(deps)

	body := `{"subscription":"0a1b2c3d","index": -1}`
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSelectServer_RestartXraySurfacesTheShellLine(t *testing.T) {
	mc := &mockConfig{
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "S1", IPs: []string{"1.1.1.1"}},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.VPN = &mockVPN{err: errors.New("xray: failed\nlast line of init")}

	handler := handleSelectServer(deps)
	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"subscription":"0a1b2c3d","index":0,"name":"S1","address":"s1.example.com","port":443}`))
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

// The selection is recorded nowhere else. config.json holds only the outbound,
// and a subscription puts many names behind one address:port - on the router
// this was written for, eight names share the running endpoint - so the choice
// has to be written down at the moment it is made.
func TestHandleSelectServer_RecordsTheSelection(t *testing.T) {
	mc := &mockConfig{
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "Амстердам", IPs: []string{"1.1.1.1"}},
			{Address: "s2.example.com", Port: 8443, UUID: "uuid-2", Name: "Берлин", IPs: []string{"2.2.2.2"}},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc

	handler := handleSelectServer(deps)

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"subscription":"0a1b2c3d","index": 1, "name": "Берлин", "address": "s2.example.com", "port": 8443}`))
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, UUID: "uuid-1", Name: "Амстердам", IPs: []string{"1.1.1.1"}},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := newTestDeps(t)
	deps.Config = mc
	deps.Xray = &mockXray{err: errors.New("reality requires a public key")}

	handler := handleSelectServer(deps)

	req := httptest.NewRequest("POST", "/api/servers/active", strings.NewReader(`{"subscription":"0a1b2c3d","index": 0, "name": "Амстердам", "address": "s1.example.com", "port": 443}`))
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, Name: "Амстердам", IPs: []string{"1.1.1.1"}},
		}}},
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
		subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
			{Address: "s1.example.com", Port: 443, Name: "Амстердам"},
		}}},
		cfg: &vpnconfig.VPNDirectorConfig{},
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

// Two subscriptions name a server Germany-1. The one the page showed in Beta
// is the one that runs, and the record names Beta.
func TestHandleSelectServer_TheSameNameInAnotherSubscription(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)

	code, resp := call(t, handleSelectServer(deps), "POST", "/api/servers/active",
		`{"subscription":"1b2c3d4e","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`)

	if code != http.StatusOK {
		t.Fatalf("%d %v", code, resp)
	}
	a := mc.cfg.Xray.ActiveServer
	if a == nil || a.Subscription != "1b2c3d4e" || a.Address != "198.51.100.20" {
		t.Fatalf("active %+v", a)
	}
	// xray.servers covers every subscription.
	if want := []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"}; !reflect.DeepEqual(mc.cfg.Xray.Servers, want) {
		t.Fatalf("xray.servers %v, want %v", mc.cfg.Xray.Servers, want)
	}
}

// A subscription deleted, or an index that names another server in its list,
// is a list that changed since the page was drawn.
func TestHandleSelectServer_AChangedListIsAConflict(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	for _, body := range []string{
		`{"subscription":"ffffffff","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`,
		`{"subscription":"0a1b2c3d","index":0,"name":"Germany-1","address":"198.51.100.20","port":8443}`,
	} {
		if code, resp := call(t, handleSelectServer(deps), "POST", "/api/servers/active", body); code != http.StatusConflict {
			t.Errorf("%s: %d %v", body, code, resp)
		}
	}
}

func TestHandleListServers_GroupsBySubscriptionAndNamesTheActive(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	mc.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "198.51.100.20", Port: 8443}
	rec := httptest.NewRecorder()

	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))

	var resp struct {
		Subscriptions []struct {
			ID      string       `json:"id"`
			Name    string       `json:"name"`
			Servers []serverView `json:"servers"`
		} `json:"subscriptions"`
		Active *vpnconfig.ActiveServer `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	g := resp.Subscriptions
	if len(g) != 2 || g[0].Name != "Alpha" || len(g[0].Servers) != 2 || g[1].ID != "1b2c3d4e" || g[1].Servers[0].Name != "Germany-1" {
		t.Fatalf("%+v", g)
	}
	if resp.Active == nil || resp.Active.Subscription != "1b2c3d4e" {
		t.Fatalf("active %+v", resp.Active)
	}
	if strings.Contains(rec.Body.String(), "uuid-") || strings.Contains(rec.Body.String(), "/s/t") {
		t.Fatalf("a credential or the link reached the page: %s", rec.Body)
	}
}
