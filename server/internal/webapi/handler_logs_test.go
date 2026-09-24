package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestHandleLogs_SingleSource(t *testing.T) {
	deps := newTestDeps(t)
	deps.Logs = &mockLogs{output: "vpn log line 1\nvpn log line 2"}

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=vpn&lines=10", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["source"] != "vpn" {
		t.Errorf("expected source 'vpn', got %q", resp["source"])
	}
	if resp["output"] != "vpn log line 1\nvpn log line 2" {
		t.Errorf("unexpected output: %q", resp["output"])
	}
}

func TestHandleLogs_AllSources(t *testing.T) {
	deps := newTestDeps(t)
	deps.Logs = &mockLogs{output: "some log content"}

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, key := range []string{"vpn", "xray", "bot", "webui"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("expected key %q in response", key)
		}
	}
}

func TestHandleLogs_InvalidSource(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=invalid", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLogs_InvalidLines(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?lines=abc", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLogs_NegativeLines(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?lines=-5", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLogs_LinesCapAt500(t *testing.T) {
	deps := newTestDeps(t)
	deps.Logs = &mockLogs{output: "capped"}

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=vpn&lines=1000", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLogs_ReadError(t *testing.T) {
	deps := newTestDeps(t)
	deps.Logs = &mockLogs{err: errors.New("file not found")}

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=vpn", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLogs_AllSourcesWithError(t *testing.T) {
	deps := newTestDeps(t)
	deps.Logs = &mockLogs{err: errors.New("file not found")}

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// When reading all sources, errors are embedded in the response, not returned as HTTP errors.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, key := range []string{"vpn", "xray", "bot", "webui"} {
		if val, ok := resp[key]; !ok {
			t.Errorf("expected key %q in response", key)
		} else if val != "error: file not found" {
			t.Errorf("expected error message for %q, got %q", key, val)
		}
	}
}

func TestHandleLogs_ReadsPathFromDeps(t *testing.T) {
	deps := newTestDeps(t)
	logs := &mockLogs{output: "xray warning"}
	deps.Logs = logs

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=xray", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(logs.paths) != 1 || logs.paths[0] != "/tmp/test-xray-error.log" {
		t.Errorf("expected read of /tmp/test-xray-error.log, got %v", logs.paths)
	}
}

func TestHandleLogs_ReadsWebUIPath(t *testing.T) {
	deps := newTestDeps(t)
	logs := &mockLogs{output: "webui line"}
	deps.Logs = logs

	req := httptest.NewRequest("GET", "/api/logs?source=webui", nil)
	rec := httptest.NewRecorder()
	handleLogs(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(logs.paths) != 1 || logs.paths[0] != "/tmp/test-webui.log" {
		t.Errorf("expected read of /tmp/test-webui.log, got %v", logs.paths)
	}
}

func TestHandleLogs_InvalidSourceListsValidOnes(t *testing.T) {
	deps := newTestDeps(t)

	handler := handleLogs(deps)

	req := httptest.NewRequest("GET", "/api/logs?source=invalid", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "unknown source: valid values are bot, vpn, webui, xray" {
		t.Errorf("unexpected error text: %q", resp["error"])
	}
}

func TestHandleConfig_OK(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{
		cfg: &vpnconfig.VPNDirectorConfig{
			DataDir: "/opt/vpn-director/data",
			WebUI: vpnconfig.WebUIConfig{
				Port:      8443,
				JWTSecret: "super-secret-key",
			},
			Xray: vpnconfig.XrayConfig{
				Clients:     []string{"192.168.50.10"},
				ExcludeSets: []string{"ru"},
				Failover:    &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}},
			},
		},
	}

	handler := handleConfig(deps)

	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp vpnconfig.VPNDirectorConfig
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// JWT secret should be redacted.
	if resp.WebUI.JWTSecret != "" {
		t.Errorf("expected JWTSecret to be redacted, got %q", resp.WebUI.JWTSecret)
	}

	// Other fields should be present.
	if resp.WebUI.Port != 8443 {
		t.Errorf("expected port 8443, got %d", resp.WebUI.Port)
	}
	if resp.DataDir != "/opt/vpn-director/data" {
		t.Errorf("expected data_dir '/opt/vpn-director/data', got %q", resp.DataDir)
	}
	if len(resp.Xray.Clients) != 1 || resp.Xray.Clients[0] != "192.168.50.10" {
		t.Errorf("expected Xray.Clients unchanged, got %v", resp.Xray.Clients)
	}
	if resp.Xray.Failover == nil {
		t.Fatal("expected Failover to be present")
	}
	if resp.Xray.Failover.Tunnel != "ovpnc2" {
		t.Errorf("expected Failover.Tunnel ovpnc2, got %q", resp.Xray.Failover.Tunnel)
	}
	if len(resp.Xray.Failover.Clients) != 1 || resp.Xray.Failover.Clients[0] != "192.168.1.8" {
		t.Errorf("expected Failover.Clients [192.168.1.8], got %v", resp.Xray.Failover.Clients)
	}
}

func TestHandleConfig_Error(t *testing.T) {
	deps := newTestDeps(t)
	deps.Config = &mockConfig{err: errors.New("config error")}

	handler := handleConfig(deps)

	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}
