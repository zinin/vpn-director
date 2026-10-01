package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type lifecycleMonitor struct {
	started, canceled, finish, saved chan struct{}
}

func (*lifecycleMonitor) Snapshot() watchdapi.Snapshot  { return watchdapi.Snapshot{} }
func (*lifecycleMonitor) Request([]string) (int, error) { return 0, nil }
func (m *lifecycleMonitor) Run(ctx context.Context) {
	close(m.started)
	<-ctx.Done()
	close(m.canceled)
	<-m.finish
	close(m.saved)
}

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle operation did not finish")
	}
}

func TestRunDaemon_ASocketFailureCancelsAndWaitsForShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &lifecycleMonitor{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var finish sync.Once
	release := func() { finish.Do(func() { close(m.finish) }) }
	t.Cleanup(release)
	sentinel := errors.New("listener failure with SECRET_SENTINEL")
	result := make(chan error, 1)
	go func() {
		result <- runDaemon(ctx, "unused", m, func(context.Context, string, watchdapi.Source) error {
			<-m.started
			return sentinel
		})
	}()
	await(t, m.canceled)
	select {
	case err := <-result:
		t.Fatalf("returned before prober/state shutdown: %v", err)
	default:
	}
	release()
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, sentinel) || strings.Contains(err.Error(), "SECRET_SENTINEL") {
			t.Fatalf("socket failure must return a safe nonzero cause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon failed to finish after shutdown")
	}
	await(t, m.saved)
}

func TestRunDaemon_ASignalWaitsForTheListenerAndExitsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &lifecycleMonitor{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	close(m.finish)
	listenerStopped := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runDaemon(ctx, "unused", m, func(ctx context.Context, _ string, _ watchdapi.Source) error {
			<-ctx.Done()
			close(listenerStopped)
			return nil
		})
	}()
	await(t, m.started)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not finish on cancellation")
	}
	await(t, listenerStopped)
	await(t, m.saved)
}

type cleanupLauncher struct{ session *cleanupSession }
type cleanupSession struct {
	started, exited chan struct{}
	once            sync.Once
}

func (*cleanupLauncher) Ready() error { return nil }
func (l *cleanupLauncher) Start(context.Context, []monitor.Endpoint) (monitor.Session, error) {
	close(l.session.started)
	return l.session, nil
}
func (*cleanupLauncher) Test(context.Context, []monitor.Endpoint) error { return nil }
func (*cleanupSession) Check(ctx context.Context, _ string) (time.Duration, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (s *cleanupSession) Exited() <-chan struct{} { return s.exited }
func (s *cleanupSession) Stop()                   { s.once.Do(func() { close(s.exited) }) }

func TestRunDaemon_EarlyListenerFailureStopsTheProberAndSavesState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session := &cleanupSession{started: make(chan struct{}), exited: make(chan struct{})}
	state := filepath.Join(t.TempDir(), "state.json")
	settings, _ := monitor.SettingsFrom(nil)
	m := monitor.New(monitor.Deps{
		Settings: func() (monitor.Settings, error) { return settings, nil },
		Endpoints: func() ([]monitor.Endpoint, map[string]string, error) {
			return []monitor.Endpoint{{Key: strings.Repeat("a", 64)}}, nil, nil
		},
		Launcher:  &cleanupLauncher{session},
		Stopped:   func() bool { return false },
		WANUp:     func(context.Context) bool { return true },
		StatePath: state,
	})
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := runDaemon(ctx, filepath.Join(parent, "watchd.sock"), m, func(ctx context.Context, path string, src watchdapi.Source) error {
		<-session.started
		return watchdapi.Serve(ctx, path, src)
	})
	if err == nil || ctx.Err() != nil {
		t.Fatalf("listener failure must end the daemon, not wait for a deadline: %v", err)
	}
	await(t, session.exited)
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal("shutdown did not save state:", err)
	}
	var saved struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Entries) != 1 {
		t.Fatalf("shutdown state is incomplete: %v", err)
	}
	info, err := os.Stat(state)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("shutdown state does not have mode 0600")
	}
}

func testConfigService(t *testing.T) *service.ConfigService {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vpn-director.json")
	if err := os.WriteFile(path, []byte(`{"data_dir":"data","xray":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return service.NewConfigService(dir, filepath.Join(dir, "data"), path)
}

func TestEndpointsReader_ConfigOnlySelectionRebuildsPriority(t *testing.T) {
	store := testConfigService(t)
	dir, err := store.SubscriptionsDir()
	if err != nil {
		t.Fatal(err)
	}
	servers := []vpnconfig.Server{
		{Name: "first", Address: "192.0.2.10", Port: 443, IPs: []string{"192.0.2.10"}, UUID: "00000000-0000-0000-0000-000000000001"},
		{Name: "second", Address: "192.0.2.20", Port: 443, IPs: []string{"192.0.2.20"}, UUID: "00000000-0000-0000-0000-000000000002"},
	}
	if err := vpnconfig.SaveSubscription(dir, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "synthetic", Servers: servers}); err != nil {
		t.Fatal(err)
	}
	read := endpointsReader(store)
	before, refused, err := read()
	if err != nil || len(before) != 2 || len(refused) != 0 {
		t.Fatalf("initial endpoint set: count=%d refused=%d error=%v", len(before), len(refused), err)
	}
	active := servers[1]
	active.Subscription = "0a1b2c3d"
	if err := store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(active)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, _, err := read()
	if err != nil || len(after) != 2 || after[0].Key != endpoint.Keys(active)[0] || after[0].Key == before[0].Key {
		t.Fatal("config-only selection did not rebuild active priority:", err)
	}
	if err := vpnconfig.DeleteSubscriptionFile(dir, "0a1b2c3d"); err != nil {
		t.Fatal(err)
	}
	if eps, _, err := read(); err != nil || len(eps) != 0 {
		t.Fatal("deleted subscription remains in endpoint set:", err)
	}
}

func TestSettingsReader_RereadsAndWarnsOnlyForDistinctInvalidValues(t *testing.T) {
	store := testConfigService(t)
	var logs bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(before) })
	write := func(interval string) {
		t.Helper()
		if err := store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
			cfg.Monitor = &vpnconfig.MonitorConfig{Interval: interval}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("1s")
	read := settingsReader(store)
	for i := 0; i < 2; i++ {
		settings, err := read()
		if err != nil || settings.Interval != time.Minute {
			t.Fatal("invalid settings did not use defaults:", err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("repeated invalid settings warned %d times, want 1", n)
	}
	write("10s")
	if settings, err := read(); err != nil || settings.Interval != 10*time.Second {
		t.Fatal("settings were not reread:", err)
	}
	write("1s")
	if _, err := read(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("reintroduced invalid settings warned %d times, want 2", n)
	}
}
