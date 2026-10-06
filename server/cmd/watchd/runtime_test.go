package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/shell"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type runtimeExecutor func(context.Context, string, ...string) (*shell.Result, error)

func (e runtimeExecutor) Exec(ctx context.Context, name string, args ...string) (*shell.Result, error) {
	return e(ctx, name, args...)
}

func runtimePaths(t *testing.T) paths.Paths {
	t.Helper()
	dir := t.TempDir()
	return paths.Paths{
		ScriptsDir: dir, DefaultDataDir: filepath.Join(dir, "unused-default"),
		BotBinary:    filepath.Join(dir, "telegram-bot"),
		XrayTemplate: filepath.Join(dir, "xray.template.json"), XrayConfig: filepath.Join(dir, "xray.json"),
		TunnelTables: filepath.Join(dir, "custom-tables"), FailoverReady: filepath.Join(dir, "fallback-ready"),
		TPROXYReady: filepath.Join(dir, "tproxy-ready"), StoppedMarker: filepath.Join(dir, "stopped"),
		WatchdSocket: daemonSocketPath(t),
	}
}

func runtimeConfig(t *testing.T, p paths.Paths, raw string) *service.ConfigService {
	t.Helper()
	store := service.NewConfigService(p.ScriptsDir, p.DefaultDataDir)
	if err := os.WriteFile(store.ConfigPath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return store
}

func runtimeQueue(t *testing.T, path string) *notifications.Store {
	t.Helper()
	q, err := notifications.NewStore(path, nil)
	if err != nil || q == nil {
		t.Fatalf("create synthetic queue: %v", err)
	}
	return q
}

func runtimeGate(t *testing.T, p paths.Paths) *watchcompat.Gate {
	t.Helper()
	return &watchcompat.Gate{BotPath: p.BotBinary, ProcRoot: t.TempDir()}
}

func runtimeWatch(t *testing.T, ctx context.Context, p paths.Paths, cfg *service.ConfigService, q *notifications.Store, executor service.ShellExecutor) *subwatch.Watch {
	t.Helper()
	vpn := service.NewVPNDirectorService(p.ScriptsDir, service.WithContext(ctx, executor))
	xray := service.NewXrayServiceForContext(ctx, p.XrayTemplate, p.XrayConfig)
	return newWatch(ctx, p, cfg, vpn, xray, q, runtimeGate(t, p), nil, nil)
}

func startRuntime(t *testing.T, ctx context.Context, cancel context.CancelFunc, path string, build func() (runtimeDeps, error), release ...func()) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- runRuntime(ctx, path, build)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		for _, finish := range release {
			finish()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("runtime shutdown:", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime failed to drain monitor/watch/queue")
		}
	})
	awaitMonitorSocket(t, path)
	return done
}

func runtimeResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("runtime shutdown:", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not finish")
	}
}

const runtimePending = `{"data_dir":"resolved-data","xray":{"clients":["192.168.50.8"],"active_server":{"name":"Oslo","address":"oslo.example","port":443,"subscription":"0a1b2c3d","seq":7},"pending_restore":{"snapshot":{"tunnel":"wgc1","clients":["192.168.50.8"],"added":["192.168.50.8"],"committed":true},"restored":["192.168.50.8"],"active":{"name":"Oslo","address":"oslo.example","port":443,"subscription":"0a1b2c3d","seq":7}}}}`

func TestRuntime_OwnershipBeforeEverySideEffect(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, runtimePending)
	queueDir := filepath.Join(p.ScriptsDir, "resolved-data")
	q := runtimeQueue(t, filepath.Join(queueDir, "watchd-notifications.json"))
	if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(queueDir, queueDir+"-held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queueDir, []byte("temporarily unwritable directory"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := q.Publish("survives shutdown flush")
	if err == nil || id == "" || q.Status().Pending != 1 {
		t.Fatal("fixture must leave a dirty RAM event after a failed save")
	}
	if err := os.Remove(queueDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(queueDir+"-held", queueDir); err != nil {
		t.Fatal(err)
	}
	m := &lifecycleMonitor{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	watchStarted, watchFinish := make(chan struct{}), make(chan struct{})
	var finishMonitor, finishWatch sync.Once
	releaseMonitor := func() { finishMonitor.Do(func() { close(m.finish) }) }
	releaseWatch := func() { finishWatch.Do(func() { close(watchFinish) }) }
	ctx, cancel := context.WithCancel(context.Background())
	w := &subwatch.Watch{
		LoadVPN: cfg.LoadVPNConfig, UpdateVPN: cfg.UpdateVPNConfig,
		LoadSubscriptions: cfg.LoadSubscriptions, TPROXYReady: func() bool { return true },
		Apply: func() error { close(watchStarted); <-watchFinish; return nil },
	}
	done := startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
		return runtimeDeps{Monitor: m, Watch: w, Queue: q}, nil
	}, releaseWatch, releaseMonitor)
	await(t, watchStarted)
	before, err := os.Lstat(p.WatchdSocket)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatal("runtime did not retain the root-only socket")
	}
	duplicateQueue := filepath.Join(p.ScriptsDir, "duplicate", "watchd-notifications.json")
	var buildCalls, queueWrites, proberCleanup, watchMutations atomic.Int64
	tryDuplicate := func() {
		t.Helper()
		duplicateCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer stop()
		err := runRuntime(duplicateCtx, p.WatchdSocket, func() (runtimeDeps, error) {
			buildCalls.Add(1)
			proberCleanup.Add(1)
			queueWrites.Add(1)
			_, _ = notifications.NewStore(duplicateQueue, nil)
			watchMutations.Add(1)
			return runtimeDeps{}, errors.New("duplicate must never build")
		})
		if err == nil || buildCalls.Load() != 0 || queueWrites.Load() != 0 || proberCleanup.Load() != 0 || watchMutations.Load() != 0 {
			t.Fatalf("duplicate effects: error=%v build=%d queue=%d cleanup=%d mutation=%d", err, buildCalls.Load(), queueWrites.Load(), proberCleanup.Load(), watchMutations.Load())
		}
		if _, err := os.Stat(duplicateQueue); !os.IsNotExist(err) {
			t.Fatal("duplicate initialized notification storage")
		}
	}
	tryDuplicate()
	if after, err := os.Lstat(p.WatchdSocket); err != nil || !os.SameFile(before, after) {
		t.Fatal("duplicate replaced the owner's socket")
	}
	cancel()
	await(t, m.canceled)
	tryDuplicate()
	select {
	case err := <-done:
		t.Fatalf("owner returned before monitor/watch drain: %v", err)
	default:
	}
	releaseMonitor()
	await(t, m.saved)
	tryDuplicate()
	select {
	case err := <-done:
		t.Fatalf("monitor completion released ownership while watch still ran: %v", err)
	default:
	}
	releaseWatch()
	runtimeResult(t, done)
	reopened := runtimeQueue(t, filepath.Join(queueDir, "watchd-notifications.json"))
	page, err := reopened.Pending("")
	if err != nil || len(page.Messages) != 1 || page.Messages[0].EventID != id || page.Messages[0].Text != "survives shutdown flush" {
		t.Fatalf("queue shutdown did not flush before unlock: page=%+v error=%v", page, err)
	}
	listener, err := watchdapi.Listen(context.Background(), p.WatchdSocket)
	if err != nil {
		t.Fatal("drained runtime retained ownership:", err)
	}
	listener.Close()
}

func TestRuntime_BotIndependentAndStopped(t *testing.T) {
	t.Run("absent_bot_keeps_scheduled_ticks", func(t *testing.T) {
		p := runtimePaths(t)
		cfg := runtimeConfig(t, p, `{"data_dir":"resolved-data","xray":{"clients":["192.168.50.8"]}}`)
		if err := cfg.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "static", Servers: []vpnconfig.Server{}}); err != nil {
			t.Fatal(err)
		}
		q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
		ctx, cancel := context.WithCancel(context.Background())
		w := runtimeWatch(t, ctx, p, cfg, q, runtimeExecutor(func(context.Context, string, ...string) (*shell.Result, error) {
			return &shell.Result{Output: `{"platform":"merlin","tunnels":[]}`}, nil
		}))
		ticks := make(chan struct{}, 4)
		var watchTicks atomic.Int64
		w.Probe = func(context.Context, int) error { watchTicks.Add(1); ticks <- struct{}{}; return nil }
		m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}
		startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
			return runtimeDeps{Monitor: m, Watch: w, Queue: q}, nil
		})
		await(t, ticks)
		select {
		case <-ticks:
		case <-time.After(32 * time.Second):
			t.Fatal("watchd did not run its next 30-second tick without a bot")
		}
		if watchTicks.Load() < 2 || ctx.Err() != nil {
			t.Fatalf("absent bot stopped watchd: ticks=%d context=%v", watchTicks.Load(), ctx.Err())
		}
	})

	t.Run("stopped_marker_keeps_existing_delivery_without_mutations", func(t *testing.T) {
		p := runtimePaths(t)
		cfg := runtimeConfig(t, p, runtimePending)
		before, _ := os.ReadFile(cfg.ConfigPath())
		if err := os.WriteFile(p.StoppedMarker, []byte("stopped\n"), 0600); err != nil {
			t.Fatal(err)
		}
		q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
		if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		id, err := q.Publish("queued before stop")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		var mutationCalls atomic.Int64
		w := runtimeWatch(t, ctx, p, cfg, q, runtimeExecutor(func(context.Context, string, ...string) (*shell.Result, error) {
			mutationCalls.Add(1)
			return &shell.Result{}, nil
		}))
		m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateStopped}}
		startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
			return runtimeDeps{Monitor: m, Watch: w, Queue: q}, nil
		})
		w.Tick(ctx)
		client := watchdapi.NewClient(p.WatchdSocket)
		page, err := client.Pending(context.Background(), "")
		if err != nil || len(page.Messages) != 1 || page.Messages[0].EventID != id {
			t.Fatalf("stop disabled delivery of existing events: page=%+v error=%v", page, err)
		}
		if err := client.Ack(context.Background(), 100, id); err != nil {
			t.Fatal(err)
		}
		if page, err := client.Pending(context.Background(), ""); err != nil || len(page.Messages) != 0 {
			t.Fatal("stopped runtime did not preserve queue ack semantics")
		}
		if mutationCalls.Load() != 0 {
			t.Fatalf("stopped runtime mutated shell/routing: mutationCalls=%d", mutationCalls.Load())
		}
		if after, err := os.ReadFile(cfg.ConfigPath()); err != nil || !bytes.Equal(before, after) {
			t.Fatal("stopped runtime published config changes")
		}
		if snap, err := client.Watch(context.Background()); err != nil || snap.State != watchdapi.WatchStopped {
			t.Fatalf("stopped watch API unavailable/false: snapshot=%+v error=%v", snap, err)
		}
	})

	t.Run("monitor_disabled_preserves_legacy_failover", func(t *testing.T) {
		p := runtimePaths(t)
		cfg := runtimeConfig(t, p, `{"data_dir":"resolved-data","monitor":{"enabled":false},"tunnel_director":{"tunnels":{"wgc1":{"clients":["192.168.50.20"]}}},"xray":{"clients":["192.168.50.8"]}}`)
		if err := cfg.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "static", Servers: []vpnconfig.Server{}}); err != nil {
			t.Fatal(err)
		}
		for path, text := range map[string]string{p.TunnelTables: "2 wgc1\n", p.FailoverReady: "wgc1\n"} {
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
		ctx, cancel := context.WithCancel(context.Background())
		var applies atomic.Int64
		w := runtimeWatch(t, ctx, p, cfg, q, runtimeExecutor(func(_ context.Context, _ string, args ...string) (*shell.Result, error) {
			if reflect.DeepEqual(args, []string{"platform"}) {
				return &shell.Result{Output: `{"platform":"merlin","tunnels":[{"id":"wgc1","iface":"wgc1","connected":true}]}`}, nil
			}
			if !reflect.DeepEqual(args, []string{"--wait", "--unless-stopped", "apply"}) {
				return nil, fmt.Errorf("unexpected legacy mutation: %q", args)
			}
			applies.Add(1)
			return &shell.Result{}, nil
		}))
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		w.Now = func() time.Time { return time.Unix(0, clock.Load()) }
		firstProbe := make(chan struct{}, 1)
		w.Probe = func(context.Context, int) error {
			select {
			case firstProbe <- struct{}{}:
			default:
			}
			return errors.New("synthetic dead outbound")
		}
		w.Reachable = nil
		m := monitor.New(monitor.Deps{
			Settings:  settingsReader(cfg),
			Endpoints: endpointsReader(cfg),
			Launcher:  monitor.FakeLauncher{},
			Stopped:   func() bool { return false },
			WANUp:     func(context.Context) bool { return true },
		})
		startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
			return runtimeDeps{Monitor: m, Watch: w, Queue: q}, nil
		})
		await(t, firstProbe)
		w.Tick(ctx)
		clock.Add(int64(3 * time.Minute))
		w.Tick(ctx)
		after, err := cfg.LoadVPNConfig()
		if err != nil || after.Xray.Failover == nil || !after.Xray.Failover.Committed || after.Xray.Failover.Tunnel != "wgc1" || !reflect.DeepEqual(after.Xray.Failover.Clients, []string{"192.168.50.8"}) || len(after.Xray.Clients) != 0 || applies.Load() == 0 {
			t.Fatalf("disabled monitor disabled legacy automation: cfg=%+v applies=%d error=%v", after, applies.Load(), err)
		}
		if snap, err := watchdapi.NewClient(p.WatchdSocket).Monitor(context.Background()); err != nil || snap.State != watchdapi.StateDisabled {
			t.Fatalf("monitor.enabled=false was not honored: state=%s error=%v", snap.State, err)
		}
	})
}

func TestRuntime_StatusDuringBlockedProbe(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, `{"data_dir":"resolved-data","xray":{"clients":["192.168.50.8"]}}`)
	if err := cfg.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Servers: []vpnconfig.Server{}}); err != nil {
		t.Fatal(err)
	}
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	ctx, cancel := context.WithCancel(context.Background())
	w := runtimeWatch(t, ctx, p, cfg, q, service.DefaultExecutor())
	w.Probe = func(context.Context, int) error { return nil }
	w.Tick(ctx)
	entered := make(chan struct{})
	w.Probe = func(ctx context.Context, _ int) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{"primary": {Status: watchdapi.StatusAlive}}}}
	startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
		return runtimeDeps{Monitor: m, Watch: w, Queue: q}, nil
	})
	await(t, entered)
	client := watchdapi.NewClient(p.WatchdSocket)
	request, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	watchResult := make(chan error, 1)
	monitorResult := make(chan error, 1)
	go func() {
		snap, err := client.Watch(request)
		if err == nil && (snap.State != watchdapi.WatchActive || snap.UpdatedAt.IsZero()) {
			err = fmt.Errorf("blocked Tick hid its cached watch status: %+v", snap)
		}
		watchResult <- err
	}()
	go func() {
		snap, err := client.Monitor(request)
		if err == nil && (snap.State != watchdapi.StateOK || len(snap.Endpoints) != 1) {
			err = errors.New("blocked Tick hid monitor status")
		}
		monitorResult <- err
	}()
	for _, result := range []<-chan error{watchResult, monitorResult} {
		select {
		case err := <-result:
			if err != nil {
				t.Error(err)
			}
		case <-request.Done():
			t.Fatal("watch or monitor IPC waited for a long Tick beyond 2 seconds")
		}
	}
	direct := make(chan watchdapi.WatchSnapshot, 1)
	go func() { direct <- w.Snapshot() }()
	select {
	case snap := <-direct:
		if snap.State != watchdapi.WatchActive {
			t.Errorf("direct cached Snapshot state=%s", snap.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot used the long-held Tick mutex")
	}
}

type runtimeLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *runtimeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *runtimeLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestRuntime_StorageFailureKeepsAutomation(t *testing.T) {
	for _, failure := range []string{"unreadable", "corrupt", "failed_save", "unresolved_data_path"} {
		t.Run(failure, func(t *testing.T) {
			p := runtimePaths(t)
			queuePath := filepath.Join(p.ScriptsDir, "watchd-notifications.json")
			switch failure {
			case "unreadable":
				if err := os.Mkdir(queuePath, 0700); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(queuePath, []byte("corrupt ORIGINAL_CREDENTIAL_SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
			case "failed_save":
				parent := filepath.Join(p.ScriptsDir, "not-a-directory")
				if err := os.WriteFile(parent, nil, 0600); err != nil {
					t.Fatal(err)
				}
				queuePath = filepath.Join(parent, "watchd-notifications.json")
			case "unresolved_data_path":
				queuePath = ""
			}
			q, storageErr := notifications.NewStore(queuePath, nil)
			if q == nil || storageErr == nil {
				t.Fatal("fixture must return both bounded RAM store and storage error")
			}
			_ = q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}})
			for i := 1; i <= 21; i++ {
				_, _ = q.Publish(fmt.Sprintf("m%02d", i))
			}
			var logs runtimeLog
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			ctx, cancel := context.WithCancel(context.Background())
			ticks := make(chan struct{}, 1)
			var watchTicks atomic.Int64
			w := &subwatch.Watch{
				LoadVPN: func() (*vpnconfig.VPNDirectorConfig, error) {
					return &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.8"}}}, nil
				},
				LoadSubscriptions: func() ([]vpnconfig.Subscription, error) { return []vpnconfig.Subscription{{ID: "0a1b2c3d"}}, nil },
				Probe:             func(context.Context, int) error { watchTicks.Add(1); ticks <- struct{}{}; return nil },
			}
			m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}
			startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
				return runtimeDeps{Monitor: m, Watch: w, Queue: q}, storageErr
			})
			await(t, ticks)
			client := watchdapi.NewClient(p.WatchdSocket)
			page, err := client.Pending(context.Background(), "")
			if err != nil || len(page.Messages) != 20 || page.Messages[0].Text != "m02" || page.Messages[19].Text != "m21" || watchTicks.Load() == 0 {
				t.Fatalf("storage error disabled bounded RAM delivery/automation: page=%+v ticks=%d error=%v", page, watchTicks.Load(), err)
			}
			if snap, err := client.Monitor(context.Background()); err != nil || snap.State != watchdapi.StateOK {
				t.Fatal("storage error disabled monitor API:", err)
			}
			snap, err := client.Watch(context.Background())
			if err != nil || snap.Notifications.Pending != 20 || failure != "corrupt" && snap.Notifications.StorageError == "" {
				t.Fatalf("runtime lost storage diagnostic/status: snapshot=%+v error=%v", snap, err)
			}
			if diagnostic := logs.String(); !strings.Contains(strings.ToLower(diagnostic), "storage") || strings.Contains(diagnostic, "ORIGINAL_CREDENTIAL_SENTINEL") {
				t.Errorf("missing/unsafe storage initialization diagnostic: %q", diagnostic)
			}
			if failure == "corrupt" {
				backups, err := filepath.Glob(queuePath + ".corrupt-*")
				if err != nil || len(backups) == 0 {
					t.Fatal("corrupt original was not preserved")
				}
				raw, err := os.ReadFile(backups[0])
				if err != nil || string(raw) != "corrupt ORIGINAL_CREDENTIAL_SENTINEL" {
					t.Fatal("corrupt original content was lost")
				}
			}
		})
	}
}

func TestRuntime_NewWatchPreservesPersistedRecoveryAndNoReplay(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, runtimePending)
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	var applies atomic.Int64
	var mode atomic.Int64
	executor := runtimeExecutor(func(_ context.Context, _ string, args ...string) (*shell.Result, error) {
		if !reflect.DeepEqual(args, []string{"--wait", "--unless-stopped", "apply"}) {
			return nil, fmt.Errorf("recovery changed process/routing command: %q", args)
		}
		applies.Add(1)
		if mode.Load() == 0 {
			return &shell.Result{ExitCode: 1}, nil
		}
		if mode.Load() == 2 {
			if err := os.WriteFile(p.TPROXYReady, []byte("ready\n"), 0600); err != nil {
				return nil, err
			}
		}
		return &shell.Result{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for phase := int64(0); phase <= 2; phase++ {
		mode.Store(phase)
		w := runtimeWatch(t, ctx, p, cfg, q, executor)
		w.Tick(ctx)
		after, err := cfg.LoadVPNConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after.Xray.Clients, []string{"192.168.50.8"}) || after.Xray.ActiveServer.Seq != 7 {
			t.Fatal("restart recovery changed current assignments/selection")
		}
		page, err := q.Pending("")
		if err != nil {
			t.Fatal(err)
		}
		if phase < 2 {
			if after.Xray.PendingRestore == nil || len(page.Messages) != 0 {
				t.Fatal("failed/soft-failed apply cleared durable intent or replayed an event")
			}
		} else if after.Xray.PendingRestore != nil || len(page.Messages) != 1 || page.Messages[0].Text != "LAN clients back on Xray; server Oslo" {
			t.Fatalf("ready restart did not complete exactly once: intent=%+v page=%+v", after.Xray.PendingRestore, page)
		}
	}
	if applies.Load() != 3 {
		t.Fatalf("restart recovery applies=%d, want 3", applies.Load())
	}
	restarted := runtimeWatch(t, ctx, p, cfg, q, executor)
	restarted.Tick(ctx)
	page, err := q.Pending("")
	if err != nil || len(page.Messages) != 1 || applies.Load() != 3 {
		t.Fatal("completed no-subscription restore replayed after another restart")
	}
}

func TestRuntime_NewWatchUsesSoleMonitorAndCallerWANContext(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, runtimePending)
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	before, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	m := monitor.New(monitor.Deps{})
	calls := 0
	var received context.Context
	wan := func(ctx context.Context) bool {
		calls++
		received = ctx
		return ctx.Err() == nil
	}
	shellCalls := 0
	vpn := service.NewVPNDirectorService(p.ScriptsDir, service.WithContext(root, runtimeExecutor(func(context.Context, string, ...string) (*shell.Result, error) {
		shellCalls++
		return &shell.Result{}, nil
	})))
	xray := service.NewXrayServiceForContext(root, p.XrayTemplate, p.XrayConfig)
	w := newWatch(root, p, cfg, vpn, xray, q, runtimeGate(t, p), m, wan)
	if w.Health != m || w.WANUp == nil || calls != 0 || shellCalls != 0 {
		t.Fatal("newWatch must share the runtime Monitor, without constructor checks or side effects")
	}
	caller, end := context.WithTimeout(root, 2*time.Second)
	defer end()
	if !w.WANUp(caller) || received != caller || calls != 1 {
		t.Fatal("WAN injection replaced the caller's context or lost its result")
	}
	wantDeadline, _ := caller.Deadline()
	gotDeadline, bounded := received.Deadline()
	if !bounded || !gotDeadline.Equal(wantDeadline) {
		t.Fatal("WAN control did not retain the caller's bounded deadline")
	}
	end()
	if w.WANUp(caller) || received != caller || calls != 2 || root.Err() != nil {
		t.Fatal("caller cancellation must stop its WAN look without canceling the runtime lifetime")
	}
	if _, err := w.Health.CheckEvidence(caller, []string{"synthetic-key"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the shared Monitor did not retain the evidence API's caller cancellation: %v", err)
	}
	after, err := os.ReadFile(cfg.ConfigPath())
	if err != nil || !bytes.Equal(before, after) || shellCalls != 0 || q.Status().Pending != 0 {
		t.Fatal("health/WAN injection changed config, shell or notification ownership")
	}
}

func TestRuntime_NewWatchUsesSharedReadinessAndCompatibilityGate(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, runtimePending)
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := runtimeWatch(t, ctx, p, cfg, q, service.DefaultExecutor())
	for path, text := range map[string]string{p.TunnelTables: "9 wgc1\n", p.FailoverReady: "wgc1\n", p.TPROXYReady: "ready\n"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if w.FallbackReady == nil || !w.FallbackReady("wgc1") || w.FallbackReady("ovpnc2") || w.TPROXYReady == nil || !w.TPROXYReady() || w.Stopped == nil || w.Stopped() {
		t.Fatal("watchd did not consume custom tables/markers through shared Readiness")
	}
	if err := os.Remove(p.FailoverReady); err != nil {
		t.Fatal(err)
	}
	if w.FallbackReady("wgc1") {
		t.Fatal("tables alone were treated as ready")
	}
	if err := os.WriteFile(p.BotBinary, []byte("#!/bin/sh\nprintf '%s\\n' '{\"protocol_version\":1,\"watch_owner\":\"bot\"}'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if w.CanMutate == nil || !errors.Is(w.CanMutate(), watchcompat.ErrIncompatible) {
		t.Fatal("newWatch did not bind the real compatibility gate")
	}
	before, _ := os.ReadFile(cfg.ConfigPath())
	w.Tick(ctx)
	if after, err := os.ReadFile(cfg.ConfigPath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("incompatible bot permitted recovery mutation")
	}
	if snap := w.Snapshot(); snap.State != watchdapi.WatchIncompatible {
		t.Fatalf("incompatible automation hid status: %+v", snap)
	}
}

func TestRuntime_NewWatchGeneratesUnderTheRealConfigLock(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, `{"data_dir":"resolved-data","advanced":{"xray":{"tproxy_port":23456,"socks_port":23457}},"xray":{"active_server":{"name":"old","address":"old.example","port":443,"subscription":"0a1b2c3d","seq":7}}}`)
	if err := os.WriteFile(p.XrayTemplate, []byte(`{"inbounds":[{"tag":"tproxy-in","protocol":"dokodemo-door","port":12345},{"tag":"socks-in","protocol":"socks","port":12346}],"outbounds":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.XrayConfig, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := runtimeWatch(t, ctx, p, cfg, q, service.DefaultExecutor())
	server := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"192.0.2.10"}, UUID: "00000000-0000-0000-0000-000000000001", Security: "tls", SNI: "oslo.example", Subscription: "0a1b2c3d"}
	refused := errors.New("synthetic stale generation")
	if generated, _, err := w.Generate(server, func(*vpnconfig.VPNDirectorConfig) error { return refused }); generated || !errors.Is(err, refused) {
		t.Fatalf("newWatch bypassed its generation guard: generated=%v error=%v", generated, err)
	}
	if raw, _ := os.ReadFile(p.XrayConfig); string(raw) != "previous\n" {
		t.Fatal("refused generation replaced live Xray")
	}
	generated, seq, err := w.Generate(server, func(c *vpnconfig.VPNDirectorConfig) error {
		if c.Xray.ActiveServer.Seq != 7 {
			return errors.New("guard did not read the locked current config")
		}
		return nil
	})
	if err != nil || !generated || seq != 8 {
		t.Fatalf("guarded DI generation=%v seq=%d error=%v", generated, seq, err)
	}
	after, err := cfg.LoadVPNConfig()
	if err != nil || after.Xray.ActiveServer.Address != "oslo.example" || after.Xray.ActiveServer.Seq != 8 {
		t.Fatal("walk DI lost original hostname or persisted sequence")
	}
	raw, err := os.ReadFile(p.XrayConfig)
	var doc struct {
		Inbounds []struct {
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Settings struct {
				Vnext []struct {
					Address string `json:"address"`
				} `json:"vnext"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err != nil || json.Unmarshal(raw, &doc) != nil || len(doc.Outbounds) != 1 || len(doc.Outbounds[0].Settings.Vnext) != 1 || doc.Outbounds[0].Settings.Vnext[0].Address != "192.0.2.10" || len(doc.Inbounds) != 2 || doc.Inbounds[0].Port != 23456 || doc.Inbounds[1].Port != 23457 {
		t.Fatalf("newWatch did not share per-address generation/configured ports: %s error=%v", raw, err)
	}
}

func TestRuntime_QueueDirtyRetryRunsWithoutBot(t *testing.T) {
	p := runtimePaths(t)
	queueDir := filepath.Join(p.ScriptsDir, "queue")
	queuePath := filepath.Join(queueDir, "watchd-notifications.json")
	q := runtimeQueue(t, queuePath)
	if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}
	startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
		return runtimeDeps{Monitor: m, Watch: &subwatch.Watch{}, Queue: q}, nil
	})
	if err := os.Rename(queueDir, queueDir+"-held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queueDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := q.Publish("dirty save retries without Telegram")
	if err == nil || id == "" {
		t.Fatal("fixture did not leave a dirty event")
	}
	if err := os.Remove(queueDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(queueDir+"-held", queueDir); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(12 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(queuePath)
		if err == nil && bytes.Contains(raw, []byte("dirty save retries without Telegram")) && q.Status().StorageError == "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runtime did not run the queue's 10-second dirty-save retry")
		case <-ticker.C:
		}
	}
	if ctx.Err() != nil {
		t.Fatal("dirty queue only flushed by stopping automation")
	}
}

func TestRuntime_ShutdownFlushFailureIsDiagnosedAndKeepsMainXray(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, `{"xray":{}}`)
	before, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.XrayConfig, []byte("current main Xray\n"), 0600); err != nil {
		t.Fatal(err)
	}
	queueDir := filepath.Join(p.ScriptsDir, "queue")
	q := runtimeQueue(t, filepath.Join(queueDir, "watchd-notifications.json"))
	var logs runtimeLog
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	ctx, cancel := context.WithCancel(context.Background())
	m := &ownershipMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}
	done := startRuntime(t, ctx, cancel, p.WatchdSocket, func() (runtimeDeps, error) {
		return runtimeDeps{Monitor: m, Watch: &subwatch.Watch{}, Queue: q}, nil
	})
	if err := os.Rename(queueDir, queueDir+"-held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queueDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if id, err := q.Publish("cannot flush yet"); id == "" || err == nil {
		t.Fatal("shutdown save failure fixture was not dirty")
	}
	cancel()
	runtimeResult(t, done)
	if diagnostic := strings.ToLower(logs.String()); !strings.Contains(diagnostic, "storage") && !strings.Contains(diagnostic, "flush") {
		t.Error("shutdown silently discarded the queue flush failure")
	}
	if raw, err := os.ReadFile(p.XrayConfig); err != nil || string(raw) != "current main Xray\n" {
		t.Fatal("notification flush failure changed/stopped main Xray")
	}
	if after, err := os.ReadFile(cfg.ConfigPath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("notification flush failure changed current routing config")
	}
}

func TestRuntime_NewWatchKeepsShellErrorsPrivate(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("transport_failure_%v", transportFailure), func(t *testing.T) {
			p := runtimePaths(t)
			cfg := runtimeConfig(t, p, `{"xray":{}}`)
			q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "watchd-notifications.json"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("RAW_PROVIDER_CREDENTIAL_SENTINEL")
			w := runtimeWatch(t, ctx, p, cfg, q, runtimeExecutor(func(context.Context, string, ...string) (*shell.Result, error) {
				if transportFailure {
					return nil, sentinel
				}
				return &shell.Result{ExitCode: 1, Output: sentinel.Error()}, nil
			}))
			for _, operation := range []func() error{
				func() error { _, err := w.LoadPlatform(); return err },
				w.Apply,
				w.RestartXray,
			} {
				err := operation()
				if err == nil || strings.Contains(err.Error(), sentinel.Error()) || transportFailure && !errors.Is(err, sentinel) {
					t.Fatalf("shell diagnostic lost failure identity or exposed provider output: %v", err)
				}
			}
		})
	}
}

func TestRuntime_NewWatchCancelsShellWithTickWhileParentLives(t *testing.T) {
	p := runtimePaths(t)
	cfg := runtimeConfig(t, p, runtimePending)
	before, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
	root, stopRoot := context.WithCancel(context.Background())
	defer stopRoot()
	started := make(chan context.Context, 1)
	w := runtimeWatch(t, root, p, cfg, q, runtimeExecutor(func(ctx context.Context, _ string, args ...string) (*shell.Result, error) {
		if !reflect.DeepEqual(args, []string{"--wait", "--unless-stopped", "apply"}) {
			return nil, fmt.Errorf("unexpected recovery command: %q", args)
		}
		started <- ctx
		<-ctx.Done()
		return &shell.Result{ExitCode: -1}, ctx.Err()
	}))
	tick, cancel := context.WithCancel(root)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Tick(tick)
		close(done)
	}()
	var operation context.Context
	select {
	case operation = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not start its scoped shell command")
	}
	cancel()
	await(t, done)
	if root.Err() != nil || !errors.Is(operation.Err(), context.Canceled) {
		t.Fatalf("Tick cancellation did not reach shell independently: root=%v operation=%v", root.Err(), operation.Err())
	}
	if after, err := os.ReadFile(cfg.ConfigPath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled Tick changed durable recovery intent")
	}
}

func TestRuntime_NewWatchPortSnapshotAcrossConfigLockWait(t *testing.T) {
	for _, test := range []struct {
		name      string
		preferred bool
		disabled  bool
	}{
		{name: "legacy_nil_health"},
		{name: "legacy_disabled_monitor", disabled: true},
		{name: "preferred_return_nil_health", preferred: true},
		{name: "preferred_return_disabled_monitor", preferred: true, disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := runtimePaths(t)
			cfg := runtimeConfig(t, p, `{"data_dir":"resolved-data","paused_clients":["192.168.50.9"],"advanced":{"xray":{"tproxy_port":23456,"socks_port":23457}},"tunnel_director":{"tunnels":{"wgc1":{"clients":["192.168.50.20","192.168.50.8"],"exclude":["ru"]},"wgc2":{"clients":["192.168.50.30"],"exclude":[]}}},"xray":{"clients":["192.168.50.9"],"servers":["192.0.2.10","192.0.2.20"],"exclude_ips":["198.51.100.0/24"],"exclude_sets":["ru"],"active_server":{"name":"Oslo","address":"oslo.example","port":443,"subscription":"0a1b2c3d","seq":7},"failover":{"tunnel":"wgc1","clients":["192.168.50.8"],"added":["192.168.50.8"],"committed":true}}}`)
			if err := cfg.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
				enabled := !test.disabled
				c.Monitor = &vpnconfig.MonitorConfig{Enabled: &enabled}
				if test.preferred {
					c.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Madrid", Address: "madrid.example", Port: 443, Subscription: "0a1b2c3d", Seq: 7}
					c.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Subscription: "0a1b2c3d"}
					c.Xray.Failover = nil
					c.Xray.Clients = []string{"192.168.50.9", "192.168.50.8"}
					tunnel := c.TunnelDirector.Tunnels["wgc1"]
					tunnel.Clients = []string{"192.168.50.20"}
					c.TunnelDirector.Tunnels["wgc1"] = tunnel
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			servers := []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"192.0.2.10"}, UUID: "00000000-0000-0000-0000-000000000001", Security: "tls", SNI: "oslo.example"}}
			if test.preferred {
				servers = append(servers, vpnconfig.Server{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{"192.0.2.20"}, UUID: "00000000-0000-0000-0000-000000000002", Security: "tls", SNI: "madrid.example"})
			}
			if err := cfg.SaveSubscription(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "synthetic", Servers: servers}); err != nil {
				t.Fatal(err)
			}
			subPath := filepath.Join(p.ScriptsDir, "resolved-data", "subscriptions", "0a1b2c3d.json")
			subBefore, err := os.ReadFile(subPath)
			if err != nil {
				t.Fatal(err)
			}
			for path, text := range map[string]string{
				p.XrayTemplate: `{"inbounds":[{"tag":"tproxy-in","protocol":"dokodemo-door","port":12345},{"tag":"socks-in","protocol":"socks","port":12346}],"outbounds":[]}`,
				p.XrayConfig:   "previous main Xray\n",
				p.TunnelTables: "2 wgc1\n3 wgc2\n", p.FailoverReady: "wgc1\n", p.TPROXYReady: "ready\n",
			} {
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", t.TempDir())
			q := runtimeQueue(t, filepath.Join(p.ScriptsDir, "resolved-data", "watchd-notifications.json"))
			if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var emitted []service.InboundPorts
			var dialed []string
			var probes []int
			var steps []string
			applies := 0
			executor := runtimeExecutor(func(_ context.Context, _ string, args ...string) (*shell.Result, error) {
				switch {
				case reflect.DeepEqual(args, []string{"platform"}):
					return &shell.Result{Output: `{"platform":"merlin","tunnels":[{"id":"wgc1","iface":"wgc1","connected":true}]}`}, nil
				case reflect.DeepEqual(args, []string{"--wait", "--unless-stopped", "apply"}):
					applies++
					return &shell.Result{}, nil
				case reflect.DeepEqual(args, []string{"--wait", "--unless-stopped", "restart", "xray-process"}):
					raw, err := os.ReadFile(p.XrayConfig)
					if err != nil {
						return nil, err
					}
					var doc struct {
						Inbounds []struct {
							Tag  string `json:"tag"`
							Port int    `json:"port"`
						} `json:"inbounds"`
						Outbounds []struct {
							Settings struct {
								Vnext []struct {
									Address string `json:"address"`
								} `json:"vnext"`
							} `json:"settings"`
						} `json:"outbounds"`
					}
					if err := json.Unmarshal(raw, &doc); err != nil {
						return nil, err
					}
					ports := service.InboundPorts{}
					for _, inbound := range doc.Inbounds {
						switch inbound.Tag {
						case "tproxy-in":
							ports.TProxy = inbound.Port
						case "socks-in":
							ports.Socks = inbound.Port
						}
					}
					if ports.TProxy <= 0 || ports.Socks <= 0 || len(doc.Outbounds) != 1 || len(doc.Outbounds[0].Settings.Vnext) != 1 {
						return nil, errors.New("generated config lacks the synthetic inbounds or outbound")
					}
					emitted = append(emitted, ports)
					dialed = append(dialed, doc.Outbounds[0].Settings.Vnext[0].Address)
					steps = append(steps, "restart")
					return &shell.Result{}, nil
				default:
					return nil, fmt.Errorf("unexpected port-snapshot command: %q", args)
				}
			})
			var health subwatch.HealthMonitor
			if test.disabled {
				health = monitor.New(monitor.Deps{Settings: settingsReader(cfg), Endpoints: endpointsReader(cfg), Launcher: monitor.FakeLauncher{}})
			}
			vpn := service.NewVPNDirectorService(p.ScriptsDir, service.WithContext(ctx, executor))
			xray := service.NewXrayServiceForContext(ctx, p.XrayTemplate, p.XrayConfig)
			w := newWatch(ctx, p, cfg, vpn, xray, q, runtimeGate(t, p), health, nil)
			w.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
			w.Reachable = func(_ context.Context, ip string, port int) bool { return ip == "192.0.2.10" && port == 443 }
			w.AfterRestart = func(wait time.Duration) {
				steps = append(steps, "settle:"+wait.String())
			}
			w.Probe = func(_ context.Context, port int) error {
				probes = append(probes, port)
				if len(emitted) == 0 {
					if test.preferred {
						return nil
					}
					return errors.New("synthetic dead main outbound")
				}
				steps = append(steps, "probe")
				live := emitted[len(emitted)-1].Socks
				if port != live {
					return fmt.Errorf("SOCKS probe used %d, generated listener is %d", port, live)
				}
				return nil
			}
			var generations []struct {
				generated bool
				seq       int
				err       error
			}
			generate := w.Generate
			entered := make(chan struct{})
			var enterOnce sync.Once
			w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				enterOnce.Do(func() { close(entered) })
				generated, seq, err := generate(s, guard)
				generations = append(generations, struct {
					generated bool
					seq       int
					err       error
				}{generated, seq, err})
				return generated, seq, err
			}
			writer := service.NewConfigService(p.ScriptsDir, p.DefaultDataDir, cfg.ConfigPath())
			held, release, writerDone, tickDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			writerErrors := make(chan error, 1)
			var writerActive vpnconfig.ActiveServer
			var releaseOnce sync.Once
			unlock := func() { releaseOnce.Do(func() { close(release) }) }
			tickStarted := false
			t.Cleanup(func() {
				unlock()
				cancel()
				await(t, writerDone)
				if tickStarted {
					await(t, tickDone)
				}
			})
			go func() {
				defer close(writerDone)
				writerErrors <- writer.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
					close(held)
					<-release
					c.Advanced["xray"].(map[string]interface{})["socks_port"] = float64(33457)
					writerActive = *c.Xray.ActiveServer
					return nil
				})
			}()
			await(t, held)
			tickStarted = true
			go func() { defer close(tickDone); w.Tick(ctx) }()
			await(t, entered)
			runtimeAwaitConfigLockWait(t, cfg.LockPath(), tickDone)
			if raw, err := os.ReadFile(p.XrayConfig); err != nil || string(raw) != "previous main Xray\n" {
				t.Fatal("generation did not wait for the real config lock")
			}
			unlock()
			await(t, writerDone)
			await(t, tickDone)
			if err := <-writerErrors; err != nil {
				t.Fatal("Advanced-only writer failed:", err)
			}
			wantWriterActive := vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Subscription: "0a1b2c3d", Seq: 7}
			if test.preferred {
				wantWriterActive.Name, wantWriterActive.Address = "Madrid", "madrid.example"
			}
			if writerActive != wantWriterActive || ctx.Err() != nil {
				t.Fatalf("lock-wait fixture changed selection or expired: active=%+v context=%v", writerActive, ctx.Err())
			}
			if len(generations) != 1 || !generations[0].generated || generations[0].seq != 8 || generations[0].err != nil {
				t.Errorf("healthy consumer generated/rolled back unexpectedly: %+v", generations)
			}
			if len(emitted) != 1 || emitted[0].TProxy != 23456 || emitted[0].Socks != 23457 && emitted[0].Socks != 33457 || !reflect.DeepEqual(dialed, []string{"192.0.2.10"}) {
				t.Errorf("healthy server was restarted on unexpected ports/endpoints: ports=%+v dialed=%v", emitted, dialed)
			}
			if len(probes) != 2 || probes[0] != 23457 || len(emitted) == 0 || probes[1] != emitted[0].Socks {
				t.Errorf("generated/probed SOCKS snapshot diverged after config lock wait: emitted=%+v probes=%v", emitted, probes)
			}
			if !reflect.DeepEqual(steps, []string{"restart", "settle:3s", "probe"}) {
				t.Errorf("healthy switch skipped settle/probe or rolled back: %v", steps)
			}
			wantApplies := 3
			wantNotes := []string{"Xray outbound is down; LAN clients moved to tunnel:wgc1", "LAN clients back on Xray; server synthetic / Oslo"}
			if test.preferred {
				wantApplies = 0
				wantNotes = []string{"Xray back on the preferred server synthetic / Oslo"}
			}
			if applies != wantApplies {
				t.Errorf("routing applies=%d, want %d", applies, wantApplies)
			}
			current, err := cfg.LoadVPNConfig()
			if err != nil {
				t.Fatal(err)
			}
			wantActive := vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Subscription: "0a1b2c3d", Seq: 8}
			if current.Xray.ActiveServer == nil || *current.Xray.ActiveServer != wantActive || current.Xray.PreferredServer != nil || current.Xray.Failover != nil || current.Xray.PendingRestore != nil {
				t.Errorf("healthy server was rejected or rollback/backoff retained: xray=%+v", current.Xray)
			}
			if !reflect.DeepEqual(current.Xray.Clients, []string{"192.168.50.9", "192.168.50.8"}) || !reflect.DeepEqual(current.PausedClients, []string{"192.168.50.9"}) || !reflect.DeepEqual(current.TunnelDirector.Tunnels, map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{"192.168.50.20"}, Exclude: []string{"ru"}},
				"wgc2": {Clients: []string{"192.168.50.30"}, Exclude: []string{}},
			}) {
				t.Errorf("healthy consumer left clients on fallback or changed unrelated assignments: xray=%v paused=%v tunnels=%+v", current.Xray.Clients, current.PausedClients, current.TunnelDirector.Tunnels)
			}
			if tproxy, socks := vpnconfig.XrayInboundPorts(current); tproxy != 23456 || socks != 33457 || current.Monitor == nil || current.Monitor.Enabled == nil || *current.Monitor.Enabled != !test.disabled || !reflect.DeepEqual(current.Xray.Servers, []string{"192.0.2.10", "192.0.2.20"}) || !reflect.DeepEqual(current.Xray.ExcludeIPs, []string{"198.51.100.0/24"}) || !reflect.DeepEqual(current.Xray.ExcludeSets, []string{"ru"}) {
				t.Error("walk/return overwrote the writer's ports, monitor setting or TPROXY bypass inputs")
			}
			if after, err := os.ReadFile(subPath); err != nil || !bytes.Equal(subBefore, after) {
				t.Error("port-snapshot handling changed the subscription")
			}
			page, err := q.Pending("")
			if err != nil {
				t.Fatal(err)
			}
			var notes []string
			for _, message := range page.Messages {
				notes = append(notes, message.Text)
			}
			if !reflect.DeepEqual(notes, wantNotes) {
				t.Errorf("healthy server emitted false failure/rollback events: notes=%v want=%v", notes, wantNotes)
			}
			stages, err := filepath.Glob(filepath.Join(p.ScriptsDir, "config.json.*"))
			if err != nil || len(stages) != 0 {
				t.Errorf("generation leaked staged configs: stages=%v error=%v", stages, err)
			}
		})
	}
}

func runtimeAwaitConfigLockWait(t *testing.T, lockPath string, done <-chan struct{}) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal("observe real config lock waiter:", err)
		}
		opened := 0
		for _, entry := range entries {
			if target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name())); err == nil && target == lockPath {
				opened++
			}
		}
		// The writer holds one descriptor; Generate opens another before flock.
		if opened >= 2 {
			return
		}
		select {
		case <-done:
			t.Fatal("Tick ended before real Generate waited for the held config lock")
		case <-deadline:
			t.Fatal("real Generate did not open the held config lock")
		case <-ticker.C:
		}
	}
}
