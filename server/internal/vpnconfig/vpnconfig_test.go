package vpnconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadServers_FileNotFound(t *testing.T) {
	_, err := LoadServers("/nonexistent/path/to/servers.json")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got: %v", err)
	}
}

func TestLoadServers_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "servers.json")

	err := os.WriteFile(path, []byte("not valid json"), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err = LoadServers(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadServers_ValidServers(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "servers.json")

	jsonContent := `[
		{"address": "server1.com", "port": 443, "uuid": "uuid1", "name": "Server 1", "ips": ["1.2.3.4"]},
		{"address": "server2.com", "port": 443, "uuid": "uuid2", "name": "Server 2", "ips": ["5.6.7.8"]}
	]`

	err := os.WriteFile(path, []byte(jsonContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	servers, err := LoadServers(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}

	if servers[0].Name != "Server 1" {
		t.Errorf("expected first server name 'Server 1', got '%s'", servers[0].Name)
	}

	if len(servers[1].IPs) != 1 || servers[1].IPs[0] != "5.6.7.8" {
		t.Errorf("expected second server IPs ['5.6.7.8'], got %v", servers[1].IPs)
	}
}

func TestSaveServers_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "servers.json")

	original := []Server{
		{Address: "server1.com", Port: 443, UUID: "uuid1", Name: "Server 1", IPs: []string{"1.2.3.4"}},
		{Address: "server2.com", Port: 8443, UUID: "uuid2", Name: "Server 2", IPs: []string{"5.6.7.8"}},
	}

	err := SaveServers(path, original)
	if err != nil {
		t.Fatalf("unexpected error saving: %v", err)
	}

	loaded, err := LoadServers(path)
	if err != nil {
		t.Fatalf("unexpected error loading: %v", err)
	}

	if !reflect.DeepEqual(original, loaded) {
		t.Errorf("round-trip failed: original %+v != loaded %+v", original, loaded)
	}
}

func TestSaveServers_EmptySlice(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "servers.json")

	err := SaveServers(path, []Server{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := LoadServers(path)
	if err != nil {
		t.Fatalf("unexpected error loading: %v", err)
	}

	if len(loaded) != 0 {
		t.Errorf("expected empty slice, got %d servers", len(loaded))
	}
}

func TestLoadVPNDirectorConfig_FileNotFound(t *testing.T) {
	_, err := LoadVPNDirectorConfig("/nonexistent/path/to/config.json")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestLoadVPNDirectorConfig_ValidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	jsonContent := `{
		"data_dir": "/opt/vpn-director/data",
		"tunnel_director": {
			"tunnels": {
				"wgc1": {
					"clients": ["192.168.50.0/24"],
					"exclude": ["us", "ca"]
				}
			}
		},
		"xray": {
			"clients": ["192.168.50.0/24"],
			"servers": ["server1", "server2"],
			"exclude_sets": ["ru"]
		},
		"advanced": {
			"debug": true
		}
	}`

	err := os.WriteFile(path, []byte(jsonContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DataDir != "/opt/vpn-director/data" {
		t.Errorf("expected DataDir '/opt/vpn-director/data', got '%s'", cfg.DataDir)
	}

	if len(cfg.TunnelDirector.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(cfg.TunnelDirector.Tunnels))
	}

	wgc1, ok := cfg.TunnelDirector.Tunnels["wgc1"]
	if !ok {
		t.Fatal("expected tunnel 'wgc1' to exist")
	}
	if len(wgc1.Clients) != 1 || wgc1.Clients[0] != "192.168.50.0/24" {
		t.Errorf("unexpected wgc1 clients: %v", wgc1.Clients)
	}
	if len(wgc1.Exclude) != 2 {
		t.Errorf("expected 2 exclude sets, got %d", len(wgc1.Exclude))
	}

	if len(cfg.Xray.Clients) != 1 {
		t.Errorf("expected 1 client, got %d", len(cfg.Xray.Clients))
	}

	if len(cfg.Xray.Servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(cfg.Xray.Servers))
	}

	if len(cfg.Xray.ExcludeSets) != 1 {
		t.Errorf("expected 1 exclude set, got %d", len(cfg.Xray.ExcludeSets))
	}

	if cfg.Advanced == nil {
		t.Error("expected Advanced to be non-nil")
	}
}

func TestSaveVPNDirectorConfig_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	original := &VPNDirectorConfig{
		DataDir: "/data",
		TunnelDirector: TunnelDirectorConfig{
			Tunnels: map[string]TunnelConfig{
				"wgc1": {
					Clients: []string{"192.168.1.0/24"},
					Exclude: []string{"ru"},
				},
				"ovpnc1": {
					Clients: []string{"10.0.0.5/32"},
					Exclude: []string{"ru", "cn"},
				},
			},
		},
		Xray: XrayConfig{
			Clients:     []string{"192.168.1.0/24"},
			Servers:     []string{"server1"},
			ExcludeSets: []string{"ru", "cn"},
		},
		Advanced: map[string]interface{}{
			"debug": true,
		},
	}

	err := SaveVPNDirectorConfig(path, original)
	if err != nil {
		t.Fatalf("unexpected error saving: %v", err)
	}

	loaded, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("unexpected error loading: %v", err)
	}

	if loaded.DataDir != original.DataDir {
		t.Errorf("DataDir mismatch: %s != %s", original.DataDir, loaded.DataDir)
	}

	if !reflect.DeepEqual(loaded.TunnelDirector.Tunnels, original.TunnelDirector.Tunnels) {
		t.Errorf("TunnelDirector.Tunnels mismatch")
	}

	if !reflect.DeepEqual(loaded.Xray.Clients, original.Xray.Clients) {
		t.Errorf("Xray.Clients mismatch")
	}

	if !reflect.DeepEqual(loaded.Xray.Servers, original.Xray.Servers) {
		t.Errorf("Xray.Servers mismatch")
	}

	if !reflect.DeepEqual(loaded.Xray.ExcludeSets, original.Xray.ExcludeSets) {
		t.Errorf("Xray.ExcludeSets mismatch")
	}
}

func TestLoadVPNDirectorConfig_PausedClients(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	jsonContent := `{
		"paused_clients": ["192.168.50.10", "192.168.50.20/32"],
		"tunnel_director": {"tunnels": {}},
		"xray": {"clients": ["192.168.50.10"], "servers": [], "exclude_sets": []}
	}`

	err := os.WriteFile(path, []byte(jsonContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.PausedClients) != 2 {
		t.Fatalf("expected 2 paused clients, got %d", len(cfg.PausedClients))
	}
	if cfg.PausedClients[0] != "192.168.50.10" {
		t.Errorf("expected '192.168.50.10', got '%s'", cfg.PausedClients[0])
	}
	if cfg.PausedClients[1] != "192.168.50.20/32" {
		t.Errorf("expected '192.168.50.20/32', got '%s'", cfg.PausedClients[1])
	}
}

func TestSaveVPNDirectorConfig_PausedClients_OmitEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	cfg := &VPNDirectorConfig{
		DataDir:        "/data",
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{}},
		Xray:           XrayConfig{Clients: []string{}, Servers: []string{}, ExcludeSets: []string{}},
	}

	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	content := string(data)
	if strings.Contains(content, "paused_clients") {
		t.Error("expected paused_clients to be omitted when empty")
	}
}

func TestCollectClients_MixedRoutes(t *testing.T) {
	cfg := &VPNDirectorConfig{
		Xray: XrayConfig{
			Clients: []string{"192.168.50.10", "192.168.50.20"},
		},
		TunnelDirector: TunnelDirectorConfig{
			Tunnels: map[string]TunnelConfig{
				"wgc1": {
					Clients: []string{"192.168.50.30/32", "192.168.50.40/32"},
				},
				"ovpnc1": {
					Clients: []string{"192.168.1.5/32"},
				},
			},
		},
		PausedClients: []string{"192.168.50.20", "192.168.50.40/32"},
	}

	clients := CollectClients(cfg)

	if len(clients) != 5 {
		t.Fatalf("expected 5 clients, got %d", len(clients))
	}

	// Check xray clients
	found := findClient(clients, "192.168.50.10", "xray")
	if found == nil {
		t.Error("expected 192.168.50.10 -> xray")
	} else if found.Paused {
		t.Error("expected 192.168.50.10 to be active")
	}

	found = findClient(clients, "192.168.50.20", "xray")
	if found == nil {
		t.Error("expected 192.168.50.20 -> xray")
	} else if !found.Paused {
		t.Error("expected 192.168.50.20 to be paused")
	}

	// Check tunnel clients
	found = findClient(clients, "192.168.50.30/32", "wgc1")
	if found == nil {
		t.Error("expected 192.168.50.30/32 -> wgc1")
	} else if found.Paused {
		t.Error("expected 192.168.50.30/32 to be active")
	}

	found = findClient(clients, "192.168.50.40/32", "wgc1")
	if found == nil {
		t.Error("expected 192.168.50.40/32 -> wgc1")
	} else if !found.Paused {
		t.Error("expected 192.168.50.40/32 to be paused")
	}

	found = findClient(clients, "192.168.1.5/32", "ovpnc1")
	if found == nil {
		t.Error("expected 192.168.1.5/32 -> ovpnc1")
	}
}

func TestCollectClients_Empty(t *testing.T) {
	cfg := &VPNDirectorConfig{
		Xray:           XrayConfig{},
		TunnelDirector: TunnelDirectorConfig{},
	}

	clients := CollectClients(cfg)
	if len(clients) != 0 {
		t.Errorf("expected 0 clients, got %d", len(clients))
	}
}

func TestCollectClients_NoPausedClients(t *testing.T) {
	cfg := &VPNDirectorConfig{
		Xray: XrayConfig{Clients: []string{"192.168.50.10"}},
	}

	clients := CollectClients(cfg)
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Paused {
		t.Error("expected client to be active")
	}
}

func findClient(clients []ClientInfo, ip, route string) *ClientInfo {
	for i := range clients {
		if clients[i].IP == ip && clients[i].Route == route {
			return &clients[i]
		}
	}
	return nil
}

func TestLoadVPNDirectorConfig_WithWebUI(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	jsonContent := `{
		"data_dir": "/opt/vpn-director/data",
		"webui": {
			"port": 8444,
			"cert_file": "/opt/vpn-director/certs/server.crt",
			"key_file": "/opt/vpn-director/certs/server.key",
			"jwt_secret": "supersecret123",
			"log_level": "debug"
		},
		"tunnel_director": {"tunnels": {}},
		"xray": {"clients": [], "servers": [], "exclude_sets": []}
	}`

	err := os.WriteFile(path, []byte(jsonContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.WebUI.Port != 8444 {
		t.Errorf("expected WebUI.Port 8444, got %d", cfg.WebUI.Port)
	}
	if cfg.WebUI.CertFile != "/opt/vpn-director/certs/server.crt" {
		t.Errorf("expected WebUI.CertFile '/opt/vpn-director/certs/server.crt', got '%s'", cfg.WebUI.CertFile)
	}
	if cfg.WebUI.KeyFile != "/opt/vpn-director/certs/server.key" {
		t.Errorf("expected WebUI.KeyFile '/opt/vpn-director/certs/server.key', got '%s'", cfg.WebUI.KeyFile)
	}
	if cfg.WebUI.JWTSecret != "supersecret123" {
		t.Errorf("expected WebUI.JWTSecret 'supersecret123', got '%s'", cfg.WebUI.JWTSecret)
	}
	if cfg.WebUI.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.WebUI.LogLevel, "debug")
	}
}

func TestLoadVPNDirectorConfig_WithoutWebUI(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	jsonContent := `{
		"data_dir": "/opt/vpn-director/data",
		"tunnel_director": {"tunnels": {}},
		"xray": {"clients": [], "servers": [], "exclude_sets": []}
	}`

	err := os.WriteFile(path, []byte(jsonContent), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.WebUI.Port != 0 {
		t.Errorf("expected WebUI.Port 0 when webui section absent, got %d", cfg.WebUI.Port)
	}
	if cfg.WebUI.CertFile != "" {
		t.Errorf("expected WebUI.CertFile '' when webui section absent, got '%s'", cfg.WebUI.CertFile)
	}
	if cfg.WebUI.KeyFile != "" {
		t.Errorf("expected WebUI.KeyFile '' when webui section absent, got '%s'", cfg.WebUI.KeyFile)
	}
	if cfg.WebUI.JWTSecret != "" {
		t.Errorf("expected WebUI.JWTSecret '' when webui section absent, got '%s'", cfg.WebUI.JWTSecret)
	}
}

func TestSaveVPNDirectorConfig_FormattedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vpn-director.json")

	cfg := &VPNDirectorConfig{
		DataDir: "/data",
		TunnelDirector: TunnelDirectorConfig{
			Tunnels: map[string]TunnelConfig{
				"wgc1": {
					Clients: []string{"192.168.1.100/32"},
					Exclude: []string{"ru"},
				},
			},
		},
		Xray: XrayConfig{
			Clients:     []string{},
			Servers:     []string{},
			ExcludeSets: []string{},
		},
	}

	err := SaveVPNDirectorConfig(path, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	// Check that JSON is pretty-printed (contains newlines and indentation)
	content := string(data)
	if len(content) < 10 {
		t.Error("expected formatted JSON output")
	}

	// Should contain newlines (pretty-printed)
	if content[0] != '{' || content[len(content)-1] != '\n' {
		t.Error("expected properly formatted JSON")
	}
}

// assertNoTempFiles fails if writeFileAtomic left a temp file behind.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSaveVPNDirectorConfig_AtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vpn-director.json")
	// A pre-existing world-readable file must come back as 0600.
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SaveVPNDirectorConfig(path, &VPNDirectorConfig{DataDir: "/data"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	loaded, err := LoadVPNDirectorConfig(path)
	if err != nil || loaded.DataDir != "/data" {
		t.Fatalf("round trip failed: %v, %+v", err, loaded)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveServers_AtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")

	if err := SaveServers(path, []Server{{Address: "a", Port: 443, UUID: "u"}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveVPNDirectorConfig_FailedRenameLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	// A directory in place of the target makes the final rename fail after
	// the temp file has already been written and synced.
	target := filepath.Join(dir, "vpn-director.json")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := SaveVPNDirectorConfig(target, &VPNDirectorConfig{}); err == nil {
		t.Fatal("expected an error when the target is a directory")
	}
	assertNoTempFiles(t, dir)
}

func TestXrayInboundPorts(t *testing.T) {
	tests := []struct {
		name          string
		advanced      map[string]interface{}
		tproxy, socks int
	}{
		{
			name: "configured",
			advanced: map[string]interface{}{"xray": map[string]interface{}{
				"tproxy_port": float64(23456), "socks_port": float64(23457),
			}},
			tproxy: 23456, socks: 23457,
		},
		{
			name:     "absent section",
			advanced: map[string]interface{}{},
		},
		{
			name: "non-numeric and non-positive are ignored",
			advanced: map[string]interface{}{"xray": map[string]interface{}{
				"tproxy_port": "23456", "socks_port": float64(0),
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tproxy, socks := XrayInboundPorts(&VPNDirectorConfig{Advanced: tc.advanced})
			if tproxy != tc.tproxy || socks != tc.socks {
				t.Errorf("XrayInboundPorts() = (%d, %d), want (%d, %d)", tproxy, socks, tc.tproxy, tc.socks)
			}
		})
	}
}

func TestRepointPausedClients(t *testing.T) {
	clients := []ClientInfo{
		{IP: "192.168.50.10", Route: "xray"},
		{IP: "192.168.50.20", Route: "wgc1"},
	}

	got := RepointPausedClients([]string{"192.168.50.20/32", "10.0.0.1"}, clients)

	want := []string{"192.168.50.20", "10.0.0.1"}
	if len(got) != len(want) {
		t.Fatalf("RepointPausedClients() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RepointPausedClients()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRepointPausedClients_DropsTheDuplicateItCreates(t *testing.T) {
	clients := []ClientInfo{{IP: "192.168.50.20", Route: "wgc1"}}

	got := RepointPausedClients([]string{"192.168.50.20", "192.168.50.20/32"}, clients)

	if len(got) != 1 || got[0] != "192.168.50.20" {
		t.Errorf("RepointPausedClients() = %v, want one entry 192.168.50.20", got)
	}
}

// A config written before this field existed - every install in the wild -
// must not come back claiming a server is active. omitempty is what keeps the
// key out of the file until something selects one.
func TestXrayConfig_OmitsTheActiveServerUntilOneIsSelected(t *testing.T) {
	out, err := json.Marshal(VPNDirectorConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "active_server") {
		t.Errorf("marshalled %s, want no active_server key at all", out)
	}
}

func TestXrayConfig_PersistsSubscriptionURLAndFailover(t *testing.T) {
	cfg := VPNDirectorConfig{
		Xray: XrayConfig{
			SubscriptionURL: "https://cdn.example/s/token",
			Failover:        &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}},
		},
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"subscription_url":"https://cdn.example/s/token"`) {
		t.Errorf("missing subscription_url: %s", s)
	}
	if !strings.Contains(s, `"failover"`) || !strings.Contains(s, `"ovpnc2"`) {
		t.Errorf("missing failover: %s", s)
	}
	var got VPNDirectorConfig
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Xray.SubscriptionURL != cfg.Xray.SubscriptionURL {
		t.Errorf("url %q", got.Xray.SubscriptionURL)
	}
	if got.Xray.Failover == nil || got.Xray.Failover.Tunnel != "ovpnc2" {
		t.Errorf("failover %+v", got.Xray.Failover)
	}
	if !reflect.DeepEqual(got.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Errorf("failover clients %v", got.Xray.Failover.Clients)
	}
}

// /api/config hands the whole file to the browser. The record identifies the
// server to a reader and stops there: the UUID is the subscription credential
// and has no business travelling with a display name.
func TestNewActiveServer_CarriesNoCredentials(t *testing.T) {
	out, err := json.Marshal(NewActiveServer(Server{
		Name:      "Осло, Норвегия, Extra",
		Address:   "155.117.201.148",
		Port:      443,
		UUID:      "the-subscription-uuid",
		PublicKey: "the-reality-public-key",
		ShortID:   "the-reality-short-id",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"the-subscription-uuid", "the-reality-public-key", "the-reality-short-id"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("marshalled %s, want no %q in it", out, secret)
		}
	}
}

func TestTunnelConfig_GatewaySurvivesASave(t *testing.T) {
	in := `{"data_dir":"/d","tunnel_director":{"tunnels":{"OpenVPN0":{"clients":["192.168.1.5"],"exclude":["ru"],"gateway":"10.73.149.1"}}},"xray":{"clients":[],"servers":[],"exclude_ips":[],"exclude_sets":[]}}`
	var cfg VPNDirectorConfig
	if err := json.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.TunnelDirector.Tunnels["OpenVPN0"].Gateway; got != "10.73.149.1" {
		t.Fatalf("Gateway = %q, want 10.73.149.1", got)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"gateway":"10.73.149.1"`) {
		t.Errorf("save dropped the gateway: %s", out)
	}
	// Absent stays absent: a Merlin config must not grow an empty gateway.
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.1.6"}, Exclude: []string{}}
	out, _ = json.Marshal(cfg)
	if strings.Contains(string(out), `"gateway":""`) {
		t.Errorf("save wrote an empty gateway: %s", out)
	}
}

func TestPlatformInfo_DecodesTheCLIDocumentAndFindsTunnels(t *testing.T) {
	doc := `{"platform":"keenetic","arch":"aarch64","password_file":"/opt/etc/passwd","lan_ifaces":["br0"],"wan_if":"eth2.4","tunnels":[{"id":"OpenVPN0","iface":"ovpn_br0","type":"openvpn","connected":true,"description":"office-ovpn"}]}`
	var info PlatformInfo
	if err := json.Unmarshal([]byte(doc), &info); err != nil {
		t.Fatal(err)
	}
	if info.Platform != "keenetic" || info.PasswordFile != "/opt/etc/passwd" || info.WANIf != "eth2.4" {
		t.Errorf("decoded %+v", info)
	}
	if len(info.Tunnels) != 1 || info.Tunnels[0].Iface != "ovpn_br0" || !info.Tunnels[0].Connected {
		t.Errorf("tunnels = %+v", info.Tunnels)
	}
	if !info.HasTunnel("OpenVPN0") || info.HasTunnel("wgc1") || info.HasTunnel("xray") {
		t.Error("HasTunnel answers for the wrong ids")
	}
}
