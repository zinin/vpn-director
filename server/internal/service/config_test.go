// internal/service/config_test.go
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// --config names the file. Deriving the directory from it and appending
// vpn-director.json reads a different file, or none at all.
func TestConfigService_ConfigPathOverride(t *testing.T) {
	s := NewConfigService("/opt/vpn-director", "/opt/vpn-director/data", "/tmp/custom.json")

	if got, want := s.ConfigPath(), "/tmp/custom.json"; got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := s.LockPath(), "/tmp/.custom.json.lock"; got != want {
		t.Errorf("LockPath() = %q, want %q", got, want)
	}
}

func TestConfigService_DefaultConfigPath(t *testing.T) {
	s := NewConfigService("/opt/vpn-director", "/opt/vpn-director/data")

	if got, want := s.ConfigPath(), "/opt/vpn-director/vpn-director.json"; got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := s.LockPath(), "/opt/vpn-director/.vpn-director.json.lock"; got != want {
		t.Errorf("LockPath() = %q, want %q", got, want)
	}
}

func TestConfigService_DataDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create minimal vpn-director.json
	configPath := filepath.Join(tmpDir, "vpn-director.json")
	os.WriteFile(configPath, []byte(`{"data_dir": "/custom/data"}`), 0644)

	svc := NewConfigService(tmpDir, filepath.Join(tmpDir, "data"))
	dataDir, err := svc.DataDir()
	if err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}

	if dataDir != "/custom/data" {
		t.Errorf("expected /custom/data, got %s", dataDir)
	}
}

func TestConfigService_DataDir_Default(t *testing.T) {
	tmpDir := t.TempDir()

	// Create config without data_dir
	configPath := filepath.Join(tmpDir, "vpn-director.json")
	os.WriteFile(configPath, []byte(`{}`), 0644)

	svc := NewConfigService(tmpDir, filepath.Join(tmpDir, "data"))
	dataDir, err := svc.DataDir()
	if err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}

	expected := filepath.Join(tmpDir, "data")
	if dataDir != expected {
		t.Errorf("expected %s, got %s", expected, dataDir)
	}
}

func TestConfigService_DataDir_Error(t *testing.T) {
	tmpDir := t.TempDir()
	// No config file - should return error

	svc := NewConfigService(tmpDir, filepath.Join(tmpDir, "data"))
	_, err := svc.DataDir()
	if err == nil {
		t.Error("expected error for missing config, got nil")
	}
}

func TestConfigService_SaveServers_CreatesDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create config with data_dir pointing to non-existent directory
	dataDir := filepath.Join(tmpDir, "newdata")
	configPath := filepath.Join(tmpDir, "vpn-director.json")
	os.WriteFile(configPath, []byte(`{"data_dir": "`+dataDir+`"}`), 0644)

	svc := NewConfigService(tmpDir, filepath.Join(tmpDir, "data"))
	err := svc.SaveServers(nil)
	if err != nil {
		t.Fatalf("SaveServers() error: %v", err)
	}

	// Directory should exist now
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Error("SaveServers should create data directory")
	}
}

func writeTestConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "vpn-director.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigService_UpdateVPNConfig_LoadMutateSave(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"data_dir": "/d", "xray": {"clients": ["1.1.1.1"]}}`)
	svc := NewConfigService(dir, filepath.Join(dir, "data"))

	err := svc.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		cfg.Xray.Clients = append(cfg.Xray.Clients, "2.2.2.2")
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateVPNConfig: %v", err)
	}

	cfg, err := svc.LoadVPNConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Xray.Clients) != 2 || cfg.Xray.Clients[1] != "2.2.2.2" {
		t.Errorf("clients = %v, want the appended entry saved", cfg.Xray.Clients)
	}
	if _, err := os.Stat(svc.LockPath()); err != nil {
		t.Errorf("lock file %s not created: %v", svc.LockPath(), err)
	}
}

func TestConfigService_UpdateVPNConfig_FnErrorSkipsSaveAndReleasesLock(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"data_dir": "/d"}`)
	svc := NewConfigService(dir, filepath.Join(dir, "data"))
	svc.lockTimeout = 200 * time.Millisecond

	sentinel := errors.New("nope")
	err := svc.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		cfg.DataDir = "/changed"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the fn error unchanged", err)
	}
	cfg, _ := svc.LoadVPNConfig()
	if cfg.DataDir != "/d" {
		t.Error("a change made by a failing fn must not be saved")
	}
	// The lock must be released: a second update succeeds within the timeout.
	if err := svc.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error { return nil }); err != nil {
		t.Fatalf("lock not released after fn error: %v", err)
	}
}

func TestConfigService_UpdateVPNConfig_MissingConfigIsErrConfigLoad(t *testing.T) {
	dir := t.TempDir()
	svc := NewConfigService(dir, filepath.Join(dir, "data"))

	called := false
	err := svc.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error { called = true; return nil })
	if !errors.Is(err, ErrConfigLoad) {
		t.Fatalf("err = %v, want ErrConfigLoad", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cause was lost: %v", err)
	}
	if called {
		t.Error("fn must not run when the config cannot be loaded")
	}
}

func TestConfigService_UpdateVPNConfig_TimesOutWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"data_dir": "/d"}`)
	svc := NewConfigService(dir, filepath.Join(dir, "data"))
	svc.lockTimeout = 200 * time.Millisecond
	svc.lockPoll = 20 * time.Millisecond

	holder, err := os.OpenFile(svc.LockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	called := false
	start := time.Now()
	err = svc.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error { called = true; return nil })
	if !errors.Is(err, ErrConfigLockTimeout) {
		t.Fatalf("err = %v, want ErrConfigLockTimeout", err)
	}
	if err.Error() != "config lock timeout" {
		t.Errorf("error text = %q, want %q", err.Error(), "config lock timeout")
	}
	if called {
		t.Error("fn must not run without the lock")
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Error("returned before the lock timeout elapsed")
	}
}

func TestConfigService_UpdateVPNConfig_SerializesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"xray": {"clients": []}}`)
	// Two services, two lock descriptors, like the bot and the Web UI.
	a := NewConfigService(dir, filepath.Join(dir, "data"))
	b := NewConfigService(dir, filepath.Join(dir, "data"))

	const n = 25
	var wg sync.WaitGroup
	for _, svc := range []*ConfigService{a, b} {
		wg.Add(1)
		go func(svc *ConfigService) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				err := svc.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
					cfg.Xray.Clients = append(cfg.Xray.Clients, "x")
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(svc)
	}
	wg.Wait()

	cfg, err := a.LoadVPNConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Xray.Clients) != 2*n {
		t.Errorf("lost updates: %d clients saved, want %d", len(cfg.Xray.Clients), 2*n)
	}
}

// A relative data_dir means "beside vpn-director.json". It cannot mean "beside
// the directory the daemon was started in": production daemons move to / at
// startup, and before that the directory was whatever the launcher had -
// including, after a self-update, one that had just been deleted.
func TestConfigService_DataDir_ResolvesARelativeValueAgainstTheConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "vpn-director.json")
	if err := os.WriteFile(configPath, []byte(`{"data_dir": "data"}`), 0644); err != nil {
		t.Fatal(err)
	}

	svc := NewConfigService(dir, filepath.Join(dir, "default-data"), configPath)

	dataDir, err := svc.DataDir()
	if err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}
	if want := filepath.Join(dir, "data"); dataDir != want {
		t.Errorf("DataDir() = %q, want %q", dataDir, want)
	}
}

// newStoreWithData is a ConfigService whose config names dataDir.
func newStoreWithData(t *testing.T) (*ConfigService, string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	writeTestConfig(t, dir, fmt.Sprintf(`{"data_dir": %q}`, data))
	return NewConfigService(dir, filepath.Join(dir, "default-data")), data
}

func TestConfigService_SubscriptionsLiveInTheDataDirectory(t *testing.T) {
	svc, data := newStoreWithData(t)
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t",
		Servers: []vpnconfig.Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}}}}

	if err := svc.SaveSubscription(sub); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(data, "subscriptions", "0a1b2c3d.json")); err != nil {
		t.Fatal(err)
	}
	subs, err := svc.LoadSubscriptions()
	if err != nil || len(subs) != 1 || subs[0].Name != "Alpha" {
		t.Fatalf("subs %+v, err %v", subs, err)
	}
	servers, err := svc.LoadServers()
	if err != nil || len(servers) != 1 || servers[0].Subscription != "0a1b2c3d" {
		t.Fatalf("servers %+v, err %v", servers, err)
	}
	if err := svc.DeleteSubscription("0a1b2c3d"); err != nil {
		t.Fatal(err)
	}
	if subs, err := svc.LoadSubscriptions(); err != nil || len(subs) != 0 {
		t.Fatalf("after delete: %+v, %v", subs, err)
	}
}

// servers.json is the single list of earlier releases. Nothing reads it now,
// and the first write of a subscription takes it away.
func TestConfigService_ASubscriptionWriteRemovesTheOldServerList(t *testing.T) {
	svc, data := newStoreWithData(t)
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "servers.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(data, "servers.json")); !os.IsNotExist(err) {
		t.Fatal("servers.json survived the first subscription write")
	}
}

// A router fresh from the previous release has servers.json and no
// subscription: nothing reads the old list, and no server is listed.
func TestConfigService_NoSubscriptionListsNoServer(t *testing.T) {
	svc, data := newStoreWithData(t)
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "servers.json"), []byte(`[{"name":"Old","address":"a.example.com","port":443}]`), 0600); err != nil {
		t.Fatal(err)
	}

	servers, err := svc.LoadServers()

	if err != nil || len(servers) != 0 {
		t.Fatalf("servers %+v, err %v", servers, err)
	}
}
