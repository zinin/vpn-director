package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

type ownershipMonitor struct {
	runs atomic.Int64
	snap watchdapi.Snapshot
}

func (m *ownershipMonitor) Snapshot() watchdapi.Snapshot { return m.snap }
func (m *ownershipMonitor) Request(keys []string) (int, error) {
	if len(keys) == 0 {
		return len(m.snap.Endpoints), nil
	}
	return len(keys), nil
}
func (m *ownershipMonitor) Run(ctx context.Context) {
	m.runs.Add(1)
	<-ctx.Done()
}

func daemonSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd4m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "watchd.sock")
}

func awaitMonitorSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := watchdapi.NewClient(path).Monitor(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned monitor did not answer")
}

func TestRunMonitor_DuplicateCannotCleanOrStartAndKeepsPrimary(t *testing.T) {
	path := daemonSocketPath(t)
	var cleanupCalls atomic.Int64
	primary := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{"primary": {Status: watchdapi.StatusAlive}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runMonitor(ctx, path, func() daemonMonitor {
			cleanupCalls.Add(1)
			return primary
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("primary shutdown:", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("primary did not shut down")
		}
	})
	awaitMonitorSocket(t, path)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}
	duplicateCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	err = runMonitor(duplicateCtx, path, func() daemonMonitor {
		cleanupCalls.Add(1)
		return duplicate
	})
	if err == nil || cleanupCalls.Load() != 1 || duplicate.runs.Load() != 0 {
		t.Errorf("duplicate performed cleanup/start: err=%v cleanup=%d runs=%d", err, cleanupCalls.Load(), duplicate.runs.Load())
	}
	if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) {
		t.Error("duplicate changed the primary socket")
	}
	client := watchdapi.NewClient(path)
	if snap, err := client.Monitor(context.Background()); err != nil || snap.State != watchdapi.StateOK || len(snap.Endpoints) != 1 {
		t.Errorf("primary health was disturbed: state=%s err=%v", snap.State, err)
	}
	for _, keys := range [][]string{{"primary"}, nil} {
		if n, err := client.Check(context.Background(), keys); err != nil || n != 1 {
			t.Errorf("primary check failed after duplicate: n=%d err=%v", n, err)
		}
	}
}

func TestRunMonitor_CancellationKeepsOwnershipThroughStateShutdown(t *testing.T) {
	path := daemonSocketPath(t)
	m := &lifecycleMonitor{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var released sync.Once
	finish := func() { released.Do(func() { close(m.finish) }) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runMonitor(ctx, path, func() daemonMonitor { return m }) }()
	t.Cleanup(func() {
		cancel()
		finish()
		select {
		case err := <-done:
			if err != nil {
				t.Error("owner shutdown:", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("owner shutdown did not finish")
		}
	})
	awaitMonitorSocket(t, path)
	await(t, m.started)
	cancel()
	await(t, m.canceled)
	initialized := false
	duplicateCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	if err := runMonitor(duplicateCtx, path, func() daemonMonitor {
		initialized = true
		return &ownershipMonitor{}
	}); err == nil || initialized {
		t.Fatal("pending prober/state shutdown released instance ownership")
	}
	finish()
	await(t, m.saved)
}

func TestRunMonitor_AcquisitionFailureIsSafeAndSkipsInitialization(t *testing.T) {
	path := daemonSocketPath(t)
	parent := filepath.Join(filepath.Dir(path), "SECRET_SENTINEL")
	if err := os.WriteFile(parent, []byte("synthetic regular file"), 0600); err != nil {
		t.Fatal(err)
	}
	var initialized bool
	m := &ownershipMonitor{}
	err := runMonitor(context.Background(), filepath.Join(parent, "watchd.sock"), func() daemonMonitor {
		initialized = true
		return m
	})
	var cause *os.PathError
	if err == nil || !errors.As(err, &cause) || strings.Contains(err.Error(), "SECRET_SENTINEL") {
		t.Fatalf("acquisition did not return a safe wrapped cause: %v", err)
	}
	if initialized || m.runs.Load() != 0 {
		t.Error("failed acquisition initialized/started the monitor")
	}
}

func TestRunMonitor_CanceledStartupSkipsInitialization(t *testing.T) {
	path := daemonSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var initialized bool
	m := &ownershipMonitor{}
	err := runMonitor(ctx, path, func() daemonMonitor {
		initialized = true
		return m
	})
	if !errors.Is(err, context.Canceled) || initialized || m.runs.Load() != 0 {
		t.Errorf("canceled acquisition initialized/started: err=%v initialized=%v runs=%d", err, initialized, m.runs.Load())
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

func TestEndpointsReader_RetriesFailedLoadWithoutStampChanges(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			store := testConfigService(t)
			dir, err := store.SubscriptionsDir()
			if err != nil {
				t.Fatal(err)
			}
			stable := vpnconfig.Subscription{ID: "0a1b2c3d", Servers: []vpnconfig.Server{{Name: "stable", Address: "192.0.2.10", Port: 443, UUID: "00000000-0000-4000-8000-000000000001"}}}
			if err := vpnconfig.SaveSubscription(dir, stable); err != nil {
				t.Fatal(err)
			}
			cache := vpnconfig.NewSubscriptionCache()
			calls, fail := 0, false
			read := endpointsReaderWithLoader(store, func(dir string) ([]vpnconfig.Subscription, error) {
				calls++
				if fail {
					fail = false
					return []vpnconfig.Subscription{stable}, syscall.EIO
				}
				return cache.Load(dir)
			})
			if warm {
				if eps, _, err := read(); err != nil || len(eps) != 1 {
					t.Fatalf("initial set: count=%d err=%v", len(eps), err)
				}
			}
			if err := vpnconfig.SaveSubscription(dir, vpnconfig.Subscription{ID: "1b2c3d4e", Servers: []vpnconfig.Server{{Name: "new", Address: "192.0.2.20", Port: 443, UUID: "00000000-0000-4000-8000-000000000002"}}}); err != nil {
				t.Fatal(err)
			}
			stamp := monitor.Stamp(dir, store.ConfigPath())
			raw, err := os.ReadFile(store.ConfigPath())
			if err != nil || stamp == "" {
				t.Fatal("could not capture unchanged files:", err)
			}
			fail = true
			if eps, refused, err := read(); !errors.Is(err, syscall.EIO) || eps != nil || refused != nil {
				t.Fatalf("load failure published a partial set: count=%d err=%v", len(eps), err)
			}
			failedCalls := calls
			eps, refused, err := read()
			if err != nil || len(eps) != 2 || len(refused) != 0 || calls != failedCalls+1 {
				t.Fatalf("unchanged stamp prevented recovery: count=%d calls=%d err=%v", len(eps), calls, err)
			}
			if again, _, err := read(); err != nil || len(again) != 2 || calls != failedCalls+1 {
				t.Fatal("successful unchanged set did not reuse the stamp shortcut:", err)
			}
			if after := monitor.Stamp(dir, store.ConfigPath()); after != stamp {
				t.Fatal("retry changed subscription/config inode, size or mtime")
			}
			if after, err := os.ReadFile(store.ConfigPath()); err != nil || !bytes.Equal(raw, after) {
				t.Fatal("retry changed config content")
			}
		})
	}
}

type endpointsReadLauncher struct {
	monitor.FakeLauncher
	ready, started chan struct{}
	starts         atomic.Int64
}

func (l *endpointsReadLauncher) Ready() error {
	l.ready <- struct{}{}
	return nil
}

func (l *endpointsReadLauncher) Start(ctx context.Context, eps []monitor.Endpoint) (monitor.Session, error) {
	l.starts.Add(1)
	sess, err := l.FakeLauncher.Start(ctx, eps)
	l.started <- struct{}{}
	return sess, err
}

func TestEndpointsReader_LoadFailureKeepsEngineEndpointSet(t *testing.T) {
	store := testConfigService(t)
	dir, err := store.SubscriptionsDir()
	if err != nil {
		t.Fatal(err)
	}
	stable := vpnconfig.Subscription{ID: "0a1b2c3d", Servers: []vpnconfig.Server{{Address: "192.0.2.10", Port: 443, UUID: "00000000-0000-4000-8000-000000000001"}}}
	previous := vpnconfig.Subscription{ID: "2c3d4e5f", Servers: []vpnconfig.Server{{Address: "192.0.2.30", Port: 443, UUID: "00000000-0000-4000-8000-000000000003"}}}
	for _, sub := range []vpnconfig.Subscription{stable, previous} {
		if err := vpnconfig.SaveSubscription(dir, sub); err != nil {
			t.Fatal(err)
		}
	}
	cache := vpnconfig.NewSubscriptionCache()
	var fail atomic.Bool
	read := endpointsReaderWithLoader(store, func(dir string) ([]vpnconfig.Subscription, error) {
		if fail.Load() {
			return []vpnconfig.Subscription{stable}, syscall.EIO
		}
		return cache.Load(dir)
	})
	var clock atomic.Int64
	now := time.Now()
	clock.Store(now.UnixNano())
	launcher := &endpointsReadLauncher{ready: make(chan struct{}, 4), started: make(chan struct{}, 4)}
	settings, _ := monitor.SettingsFrom(nil)
	m := monitor.New(monitor.Deps{
		Settings:  func() (monitor.Settings, error) { return settings, nil },
		Endpoints: read,
		Launcher:  launcher,
		Stopped:   func() bool { return false },
		WANUp:     func(context.Context) bool { return true },
		Now:       func() time.Time { return time.Unix(0, clock.Load()) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); await(t, done) })
	await(t, launcher.ready)
	await(t, launcher.started)
	key := endpoint.Keys(stable.Servers[0])[0]
	previousKey := endpoint.Keys(previous.Servers[0])[0]
	if snap := m.Snapshot(); len(snap.Endpoints) != 2 {
		t.Fatal("initial engine endpoint set is incomplete")
	}
	if err := vpnconfig.SaveSubscription(dir, vpnconfig.Subscription{ID: "1b2c3d4e", Servers: []vpnconfig.Server{{Address: "192.0.2.20", Port: 443, UUID: "00000000-0000-4000-8000-000000000002"}}}); err != nil {
		t.Fatal(err)
	}
	stamp := monitor.Stamp(dir, store.ConfigPath())
	fail.Store(true)
	clock.Store(now.Add(2 * time.Minute).UnixNano())
	if _, err := m.Request(nil); err != nil {
		t.Fatal(err)
	}
	await(t, launcher.ready)
	if snap := m.Snapshot(); len(snap.Endpoints) != 2 || snap.Endpoints[key].Status == "" || snap.Endpoints[previousKey].Status == "" || launcher.starts.Load() != 1 {
		t.Fatal("failed load changed the last successful engine set or replaced its session")
	}
	fail.Store(false)
	clock.Store(now.Add(4 * time.Minute).UnixNano())
	if _, err := m.Request(nil); err != nil {
		t.Fatal(err)
	}
	await(t, launcher.ready)
	await(t, launcher.started)
	if snap := m.Snapshot(); len(snap.Endpoints) != 3 || launcher.starts.Load() != 2 {
		t.Fatal("engine did not publish the recovered set")
	}
	if monitor.Stamp(dir, store.ConfigPath()) != stamp {
		t.Fatal("recovery changed files")
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

func TestEndpointsReader_LiteralDataPathsSeeSubscriptionOnlyChanges(t *testing.T) {
	for _, name := range []string{"data[1]", "data*", "data?", "data["} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "vpn-director.json")
			raw, _ := json.Marshal(map[string]any{"data_dir": name, "xray": map[string]any{}})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := service.NewConfigService(root, filepath.Join(root, name), path)
			dir, err := store.SubscriptionsDir()
			if err != nil {
				t.Fatal(err)
			}
			server := vpnconfig.Server{Name: "synthetic", Address: "192.0.2.10", Port: 443, IPs: []string{"192.0.2.10"}, UUID: "00000000-0000-0000-0000-000000000001"}
			sub := vpnconfig.Subscription{ID: "0a1b2c3d", Name: "synthetic", Servers: []vpnconfig.Server{server}}
			if err := vpnconfig.SaveSubscription(dir, sub); err != nil {
				t.Fatal(err)
			}
			read := endpointsReader(store)
			before, _, err := read()
			if err != nil || len(before) != 1 {
				t.Fatalf("initial set count=%d err=%v", len(before), err)
			}
			info, err := os.Stat(filepath.Join(dir, sub.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			sub.Servers[0].UUID = "00000000-0000-0000-0000-000000000002"
			if err := vpnconfig.SaveSubscription(dir, sub); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(dir, sub.ID+".json"), info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, _, err := read()
			if err != nil || len(after) != 1 || after[0].Key == before[0].Key {
				t.Error("same-size/time subscription-only replacement kept old key")
			}
			sub.ID = "1b2c3d4e"
			sub.Servers[0].UUID = "00000000-0000-0000-0000-000000000003"
			if err := vpnconfig.SaveSubscription(dir, sub); err != nil {
				t.Fatal(err)
			}
			if eps, _, err := read(); err != nil || len(eps) != 2 {
				t.Errorf("subscription-only add count=%d err=%v", len(eps), err)
			}
			for _, id := range []string{"0a1b2c3d", "1b2c3d4e"} {
				if err := vpnconfig.DeleteSubscriptionFile(dir, id); err != nil {
					t.Fatal(err)
				}
			}
			if eps, _, err := read(); err != nil || len(eps) != 0 {
				t.Errorf("removed endpoints count=%d err=%v", len(eps), err)
			}
			if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, raw) {
				t.Fatal("regression changed config")
			}
		})
	}
}

func TestWatchdInit_RestartRetainsStopFailure(t *testing.T) {
	raw, err := os.ReadFile("../../../router/opt/etc/init.d/S98vpn-director-watchd")
	if err != nil {
		t.Fatal(err)
	}
	for _, survivor := range []bool{true, false} {
		t.Run(map[bool]string{true: "surviving", false: "stopped"}[survivor], func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "watchd")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			src := strings.Replace(string(raw), `WATCHD_PATH="/opt/vpn-director/vpn-director-watchd"`, `WATCHD_PATH="`+binary+`"`, 1)
			stub := `
pidof() { [ -f "$SANDBOX/running" ]; }
killall() { printf 'kill %s\n' "$*" >> "$SANDBOX/events"; [ "$SURVIVE" = 1 ] || rm -f "$SANDBOX/running"; }
sleep() { if [ "$1" = 2 ]; then n=0; while [ ! -f "$SANDBOX/running" ] && [ "$n" -lt 100 ]; do /bin/sleep 0.01; n=$((n+1)); done; fi; }
logger() { :; }
nohup() { printf 'start\n' >> "$SANDBOX/events"; touch "$SANDBOX/running"; }
`
			src = strings.Replace(src, "start() {", stub+"\nstart() {", 1)
			init := filepath.Join(dir, "init")
			if err := os.WriteFile(init, []byte(src), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "running"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			flag := "0"
			if survivor {
				flag = "1"
			}
			cmd := exec.Command("/bin/sh", init, "restart")
			cmd.Env = append(os.Environ(), "SANDBOX="+dir, "SURVIVE="+flag)
			out, err := cmd.CombinedOutput()
			events, _ := os.ReadFile(filepath.Join(dir, "events"))
			if survivor {
				if err == nil || !bytes.Contains(out, []byte("Failed to stop")) || bytes.Contains(out, []byte("Starting")) || bytes.Contains(events, []byte("start")) {
					t.Fatalf("failed stop was masked: err=%v output=%s events=%s", err, out, events)
				}
			} else if err != nil || !bytes.Contains(events, []byte("start")) {
				t.Fatalf("successful restart err=%v output=%s events=%s", err, out, events)
			}
		})
	}
}

func TestEndpointsReader_AListingFailureDoesNotReuseAMissingDirectorySnapshot(t *testing.T) {
	store := testConfigService(t)
	dir, err := store.SubscriptionsDir()
	if err != nil {
		t.Fatal(err)
	}
	read := endpointsReader(store)
	if eps, _, err := read(); err != nil || len(eps) != 0 {
		t.Fatalf("missing directory count=%d err=%v", len(eps), err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := read(); err == nil {
			t.Fatal("failed directory listing reused a successful snapshot")
		}
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	sub := vpnconfig.Subscription{ID: "0a1b2c3d", Servers: []vpnconfig.Server{{Address: "192.0.2.10", Port: 443, UUID: "00000000-0000-0000-0000-000000000001"}}}
	if err := vpnconfig.SaveSubscription(dir, sub); err != nil {
		t.Fatal(err)
	}
	if eps, _, err := read(); err != nil || len(eps) != 1 {
		t.Fatalf("repaired listing count=%d err=%v", len(eps), err)
	}
}

func TestRuntime_WatchdStartupUsesResolvedDataDir(t *testing.T) {
	if raw := os.Getenv("VPD_TASK8_WATCHD_MAIN"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"vpn-director-watchd"}, args...)
		flag.CommandLine = flag.NewFlagSet("vpn-director-watchd", flag.ExitOnError)
		main()
		return
	}

	for _, failure := range []bool{false, true} {
		name := "resolved_relative_data_dir"
		if failure {
			name = "unresolved_data_dir_keeps_ram_api"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Dir(daemonSocketPath(t))
			dev := filepath.Join(root, "testdata", "dev")
			if err := os.MkdirAll(dev, 0700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dev, "vpn-director.json")
			raw := []byte(`{"data_dir":"queues[1]","monitor":{"enabled":false},"xray":{}}`)
			if failure {
				raw = []byte("not a readable config")
			}
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dev, "telegram-bot.json"), []byte("no token; not a bot config"), 0600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal([]string{"--dev", "--config", configPath, "--platform", "keenetic"})
			if err != nil {
				t.Fatal(err)
			}
			processCtx, kill := context.WithTimeout(context.Background(), 6*time.Second)
			defer kill()
			cmd := exec.CommandContext(processCtx, os.Args[0], "-test.run=^TestRuntime_WatchdStartupUsesResolvedDataDir$")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "VPD_TASK8_WATCHD_MAIN="+string(args), "VPD_PLATFORM=invalid-platform")
			var output runtimeLog
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				kill()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("owned startup helper did not exit")
				}
			})
			socket := filepath.Join(dev, "watchd.sock")
			client := watchdapi.NewClient(socket)
			var snap watchdapi.WatchSnapshot
			deadline := time.Now().Add(3 * time.Second)
			for {
				request, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
				snap, err = client.Watch(request)
				stop()
				if err == nil {
					break
				}
				select {
				case exitErr := <-done:
					t.Fatalf("watchd startup exited before its combined API: %v output=%s", exitErr, output.String())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("watchd did not expose automation/queue API: %v output=%s", err, output.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := client.Monitor(context.Background()); err != nil {
				t.Fatal("queue initialization disabled monitor API:", err)
			}
			queuePath := filepath.Join(dev, "queues[1]", "watchd-notifications.json")
			if failure {
				if snap.Notifications.StorageError == "" {
					t.Fatal("failed DataDir resolution did not expose explicit RAM-only storage_error")
				}
			} else {
				if err := client.SetRecipients(context.Background(), []watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Minute)}}); err != nil {
					t.Fatal("real startup did not wire the sole queue writer:", err)
				}
				info, err := os.Stat(queuePath)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("queue was not published at resolved ConfigService.DataDir with mode 0600")
				}
			}
			if _, err := os.Stat(filepath.Join(dev, "data", "watchd-notifications.json")); !os.IsNotExist(err) {
				t.Fatal("runtime silently used default data_dir rather than resolved config path")
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("SIGTERM did not drain runtime cleanly: %v output=%s", err, output.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queue storage/path error blocked shutdown")
			}
			if after, err := os.ReadFile(configPath); err != nil || !bytes.Equal(after, raw) {
				t.Fatal("startup or shutdown changed main routing/Xray config")
			}
			if _, err := os.Stat(filepath.Join(dev, "xray.json")); !os.IsNotExist(err) {
				t.Fatal("unarmed watchd startup/shutdown generated main Xray config")
			}
		})
	}
}

func TestRuntime_WatchdBindsSelectedShellConfig(t *testing.T) {
	if raw := os.Getenv("VPD_TASK8_CONFIG_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		os.Args = append([]string{"vpn-director-watchd"}, args...)
		flag.CommandLine = flag.NewFlagSet("vpn-director-watchd", flag.ExitOnError)
		selected := os.Getenv("VPD_TASK8_SELECTED_CONFIG")
		cfg := service.NewConfigService(filepath.Dir(selected), "", selected)
		current, err := cfg.LoadVPNConfig()
		if err != nil || current == nil || current.Xray.ActiveServer == nil || current.Xray.ActiveServer.Seq != 7 {
			t.Fatalf("selected Go config must start with seq=7: config=%+v error=%v", current, err)
		}
		done := make(chan int, 1)
		go func() { done <- run() }()
		client := watchdapi.NewClient(filepath.Join("testdata", "dev", "watchd.sock"))
		deadline := time.Now().Add(3 * time.Second)
		for {
			request, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err := client.Watch(request)
			cancel()
			if err == nil {
				break
			}
			select {
			case code := <-done:
				t.Fatalf("watchd exited before shell configuration check: code=%d", code)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("watchd did not expose its API before shell configuration check:", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Use the real executor in the running daemon's exported environment.
		vpn := service.NewVPNDirectorService(os.Getenv("VPD_TASK8_SHELL_DIR"), service.DefaultExecutor())
		if err := vpn.ApplyUnlessStopped(); err != nil {
			t.Error("owned shell configuration recorder failed:", err)
		} else {
			trace := os.Getenv("VPD_TASK8_SHELL_TRACE")
			path, pathErr := os.ReadFile(trace + ".path")
			usedPath, absErr := filepath.Abs(strings.TrimSpace(string(path)))
			raw, readErr := os.ReadFile(trace + ".json")
			var used vpnconfig.VPNDirectorConfig
			decodeErr := json.Unmarshal(raw, &used)
			if pathErr != nil || absErr != nil || readErr != nil || decodeErr != nil {
				t.Errorf("shell config trace unavailable: path=%v absolute=%v read=%v decode=%v", pathErr, absErr, readErr, decodeErr)
			} else if usedPath != selected || used.Xray.ActiveServer == nil || used.Xray.ActiveServer.Seq != 7 {
				t.Errorf("Go selected %s seq=7; shell Apply used %s active=%+v", selected, usedPath, used.Xray.ActiveServer)
			}
			if argv, err := os.ReadFile(trace + ".args"); err != nil || string(argv) != "--wait\n--unless-stopped\napply\n" {
				t.Errorf("automatic Apply lost its wait/stop guards: argv=%q error=%v", argv, err)
			}
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("watchd shutdown exit=%d", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("watchd did not drain after shell configuration check")
		}
		return
	}

	for _, tc := range []struct {
		name      string
		relative  bool
		inherited bool
	}{
		{"absolute_config_with_inherited_mismatch", false, true},
		{"relative_config_with_inherited_mismatch", true, true},
		{"relative_config_without_inherited_override", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Dir(daemonSocketPath(t))
			dev := filepath.Join(root, "testdata", "dev")
			selected := filepath.Join(root, "custom configs", "selected.json")
			live := filepath.Join(dev, "vpn-director.json")
			for _, dir := range []string{dev, filepath.Dir(selected)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			selectedRaw := []byte(`{"data_dir":"selected-data","monitor":{"enabled":false},"xray":{"active_server":{"name":"Oslo","address":"oslo.example","port":443,"seq":7}}}`)
			liveRaw := []byte(`{"data_dir":"live-data","monitor":{"enabled":false},"xray":{"active_server":{"name":"Oslo","address":"oslo.example","port":443,"seq":41}}}`)
			for path, raw := range map[string][]byte{selected: selectedRaw, live: liveRaw} {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			trace := filepath.Join(dev, "shell-config")
			// The installed default is replaced by an owned file for this recorder.
			script := "#!/bin/sh\nset -eu\n" +
				"config=\"${VPD_CONFIG_FILE:-$VPD_TASK8_DEFAULT_CONFIG}\"\n" +
				"printf '%s\\n' \"$config\" > \"$VPD_TASK8_SHELL_TRACE.path\"\n" +
				"cat \"$config\" > \"$VPD_TASK8_SHELL_TRACE.json\"\n" +
				"printf '%s\\n' \"$@\" > \"$VPD_TASK8_SHELL_TRACE.args\"\n"
			if err := os.WriteFile(filepath.Join(dev, "vpn-director.sh"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			configArg := selected
			if tc.relative {
				configArg = filepath.Join("custom configs", "selected.json")
			}
			args, err := json.Marshal([]string{"--dev", "--config", configArg, "--platform", "keenetic"})
			if err != nil {
				t.Fatal(err)
			}
			processCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(processCtx, os.Args[0], "-test.run=^TestRuntime_WatchdBindsSelectedShellConfig$")
			cmd.Dir = root
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "VPD_CONFIG_FILE=") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "VPD_TASK8_CONFIG_ARGS="+string(args), "VPD_TASK8_SELECTED_CONFIG="+selected,
				"VPD_TASK8_SHELL_DIR="+dev, "VPD_TASK8_SHELL_TRACE="+trace, "VPD_TASK8_DEFAULT_CONFIG="+live)
			if tc.inherited {
				cmd.Env = append(cmd.Env, "VPD_CONFIG_FILE="+live)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("non-default --config diverged from shell configuration: %v\n%s", err, output)
			}
			for path, before := range map[string][]byte{selected: selectedRaw, live: liveRaw} {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
					t.Errorf("read-only configuration recorder changed %s: error=%v", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dev, "xray.json")); !os.IsNotExist(err) {
				t.Fatal("unarmed configuration check generated main Xray config")
			}
		})
	}
}
