package subwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const watchStatusSecret = "synthetic-watch-status-credential-13"

func assertWatchSnapshotSafe(t *testing.T, snapshot watchdapi.WatchSnapshot) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{watchStatusSecret, "https://provider.example/private/", "vless://", "bot_token", "outbound", "uuid"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("snapshot exposes %q: %s", forbidden, raw)
		}
	}
}

func assertWatchSnapshotAction(t *testing.T, snapshot watchdapi.WatchSnapshot, want string) {
	t.Helper()
	if snapshot.State != watchdapi.WatchActive || snapshot.Action != want {
		t.Fatalf("snapshot state=%q action=%q, want active/%q", snapshot.State, snapshot.Action, want)
	}
	assertWatchSnapshotSafe(t, snapshot)
}

func TestWatchSnapshot_AvailabilityAndAction(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	t.Run("starting before the first tick", func(t *testing.T) {
		w := &Watch{}
		snapshot := w.Snapshot()
		if snapshot.State != watchdapi.WatchStarting || snapshot.Action != "" || !snapshot.UpdatedAt.IsZero() || snapshot.CommittedFailover || snapshot.PendingRestore {
			t.Fatalf("unstarted snapshot %+v", snapshot)
		}
		assertWatchSnapshotSafe(t, snapshot)
	})

	for _, tc := range []struct {
		name   string
		change func(*fake, *Watch)
		want   watchdapi.WatchState
		probes int
	}{
		{"active unarmed", nil, watchdapi.WatchActive, 0},
		{"active with monitor disabled", func(f *fake, _ *Watch) { f.noSubs = false }, watchdapi.WatchActive, 1},
		{"stopped unarmed", func(_ *fake, w *Watch) { w.Stopped = func() bool { return true } }, watchdapi.WatchStopped, 0},
		{"incompatible unarmed", func(_ *fake, w *Watch) {
			w.CanMutate = func() error { return fmt.Errorf("%w: %s", watchcompat.ErrIncompatible, watchStatusSecret) }
		}, watchdapi.WatchIncompatible, 0},
		{"incompatible with monitor disabled", func(f *fake, w *Watch) {
			f.noSubs = false
			w.CanMutate = func() error { return watchcompat.ErrIncompatible }
		}, watchdapi.WatchIncompatible, 0},
		{"configuration error", func(_ *fake, w *Watch) {
			w.LoadVPN = func() (*vpnconfig.VPNDirectorConfig, error) {
				return nil, errors.New("https://provider.example/private/" + watchStatusSecret)
			}
		}, watchdapi.WatchError, 0},
		{"mutation gate error", func(_ *fake, w *Watch) {
			w.CanMutate = func() error { return errors.New("provider refusal: " + watchStatusSecret) }
		}, watchdapi.WatchError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disabled := false
			f := &fake{cfg: baseCfg(), noSubs: true, now: at}
			f.cfg.Monitor = &vpnconfig.MonitorConfig{Enabled: &disabled}
			w := f.watch()
			probes := 0
			w.Probe = func(context.Context, int) error { probes++; return nil }
			if tc.change != nil {
				tc.change(f, w)
			}
			w.Tick(context.Background())
			if probes != tc.probes {
				t.Fatalf("availability/gate changed primary probing: probes=%d, want %d", probes, tc.probes)
			}
			snapshot := w.Snapshot()
			if snapshot.State != tc.want || snapshot.Action != "" || !snapshot.UpdatedAt.Equal(at) {
				t.Fatalf("snapshot %+v, want state=%s, idle action and current timestamp", snapshot, tc.want)
			}
			if tc.want != watchdapi.WatchActive && snapshot.Message == "" {
				t.Fatal("unavailable automation has no safe explanation")
			}
			if f.applies != 0 || len(f.notes) != 0 {
				t.Fatalf("availability read caused routing or notification effects: applies=%d notes=%v", f.applies, f.notes)
			}
			assertWatchSnapshotSafe(t, snapshot)
		})
	}

	t.Run("checking the primary outbound", func(t *testing.T) {
		f := &fake{cfg: baseCfg(), now: at}
		w := f.watch()
		var during watchdapi.WatchSnapshot
		w.Probe = func(context.Context, int) error {
			during = w.Snapshot()
			return nil
		}
		w.Tick(context.Background())
		assertWatchSnapshotAction(t, during, "checking")
		assertWatchSnapshotAction(t, w.Snapshot(), "")
	})

	t.Run("checking fresh monitor evidence", func(t *testing.T) {
		s := newFastFixture(t)
		snapshots := make(chan watchdapi.WatchSnapshot, 8)
		s.h.check = func(_ context.Context, keys []string) (monitor.Evidence, error) {
			snapshots <- s.w.Snapshot()
			return copyFastEvidence(s.h.fresh, keys), nil
		}
		s.w.Tick(context.Background())
		close(snapshots)
		if len(snapshots) == 0 {
			t.Fatal("the fast path did not request fresh evidence")
		}
		for snapshot := range snapshots {
			assertWatchSnapshotAction(t, snapshot, "checking")
		}
		assertWatchSnapshotAction(t, s.w.Snapshot(), "")
	})

	t.Run("switching without moving clients", func(t *testing.T) {
		s := newFastFixture(t)
		generate := s.w.Generate
		var during watchdapi.WatchSnapshot
		s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
			during = s.w.Snapshot()
			return generate(server, guard)
		}
		s.w.Tick(context.Background())
		assertWatchSnapshotAction(t, during, "switching")
		assertWatchSnapshotAction(t, s.w.Snapshot(), "")
		if snapshot := s.w.Snapshot(); snapshot.CommittedFailover || snapshot.PendingRestore {
			t.Fatalf("a server-only switch claimed a client recovery intent: %+v", snapshot)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: at}
		w := f.watch()
		var during watchdapi.WatchSnapshot
		platform := w.LoadPlatform
		w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
			during = w.Snapshot()
			return platform()
		}
		tickUntilDead(w, f)
		assertWatchSnapshotAction(t, during, "fallback")
		assertWatchSnapshotAction(t, w.Snapshot(), "")
		if snapshot := w.Snapshot(); !snapshot.CommittedFailover || snapshot.PendingRestore {
			t.Fatalf("committed fallback flags %+v", snapshot)
		}
	})

	t.Run("refreshing then walking", func(t *testing.T) {
		server := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10"}, UUID: watchStatusSecret}
		f := &fake{
			cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: at,
			subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "North", URL: "https://provider.example/private/" + watchStatusSecret, Servers: []vpnconfig.Server{server}}},
		}
		w := f.watch()
		refreshed := make(chan watchdapi.WatchSnapshot, 1)
		var walking watchdapi.WatchSnapshot
		w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
			refreshed <- w.Snapshot()
			return []vpnconfig.Server{server}, nil
		}
		w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
			walking = w.Snapshot()
			if err := f.checkGuard(guard); err != nil {
				return false, f.seq(), err
			}
			vpnconfig.RecordWalkedServer(f.cfg, server)
			f.probeErr = nil
			return true, f.seq(), nil
		}
		w.AfterRestart = func(time.Duration) {}
		tickUntilDead(w, f)
		if len(refreshed) != 1 {
			t.Fatal("the failed outbound did not start the subscription refresh")
		}
		assertWatchSnapshotAction(t, <-refreshed, "refreshing")
		assertWatchSnapshotAction(t, walking, "walking")
		assertWatchSnapshotAction(t, w.Snapshot(), "")
		if snapshot := w.Snapshot(); snapshot.CommittedFailover || snapshot.PendingRestore {
			t.Fatalf("completed walk retained recovery flags: %+v", snapshot)
		}
	})

	t.Run("returning to the preferred server", func(t *testing.T) {
		r := newReturnRig(returnServers())
		r.up[osloIP], r.live[osloIP] = true, true
		generate := r.w.Generate
		var during watchdapi.WatchSnapshot
		r.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
			during = r.w.Snapshot()
			return generate(server, guard)
		}
		r.tick()
		assertWatchSnapshotAction(t, during, "returning")
		assertWatchSnapshotAction(t, r.w.Snapshot(), "")
	})

	t.Run("durable flags do not come from the RAM restore mirror", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			xray      string
			committed bool
			pending   bool
		}{
			{"no intent", `{"clients":["192.168.1.8"]}`, false, false},
			{"staged fallback", `{"clients":["192.168.1.8"],"failover":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"]}}`, false, false},
			{"committed fallback", `{"clients":[],"failover":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"],"committed":true}}`, true, false},
			{"legacy committed fallback", `{"clients":[],"failover":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"]}}`, true, false},
			{"pending restore after restart", `{"clients":["192.168.1.8"],"pending_restore":{"snapshot":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"],"committed":true},"restored":["192.168.1.8"],"active":null}}`, false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				store := service.NewConfigService(dir, filepath.Join(dir, "data"))
				document := `{"monitor":{"enabled":false},"tunnel_director":{"tunnels":{"ovpnc2":{"clients":["192.168.1.8"]}}},"xray":` + tc.xray + `}`
				if err := os.WriteFile(store.ConfigPath(), []byte(document), 0600); err != nil {
					t.Fatal(err)
				}
				w := &Watch{
					LoadVPN:       store.LoadVPNConfig,
					Now:           func() time.Time { return at },
					Probe:         func(context.Context, int) error { return errProbe },
					FallbackReady: func(string) bool { return false },
				}
				if !tc.pending {
					w.pendingRestore = &vpnconfig.XrayPendingRestore{}
				}
				w.Tick(context.Background())
				snapshot := w.Snapshot()
				if snapshot.State != watchdapi.WatchActive || snapshot.CommittedFailover != tc.committed || snapshot.PendingRestore != tc.pending {
					t.Fatalf("snapshot %+v, want active committed=%t pending=%t from the persisted config", snapshot, tc.committed, tc.pending)
				}
				after, err := os.ReadFile(store.ConfigPath())
				if err != nil || string(after) != document {
					t.Fatalf("status-only fixture changed persisted intent: %s, error=%v", after, err)
				}
				assertWatchSnapshotSafe(t, snapshot)
			})
		}
	})

	t.Run("restoring clears persisted intent only after apply", func(t *testing.T) {
		dir := t.TempDir()
		store := service.NewConfigService(dir, filepath.Join(dir, "data"))
		document := `{"monitor":{"enabled":false},"tunnel_director":{"tunnels":{"ovpnc2":{"clients":["192.168.1.8"]}}},"xray":{"clients":[],"active_server":{"subscription":"0a1b2c3d","name":"Oslo","address":"oslo.example","port":443,"seq":7},"failover":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"],"committed":true}}}`
		if err := os.WriteFile(store.ConfigPath(), []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		newWatch := func() *Watch {
			return &Watch{
				LoadVPN:     store.LoadVPNConfig,
				UpdateVPN:   store.UpdateVPNConfig,
				Probe:       func(context.Context, int) error { return nil },
				TPROXYReady: func() bool { return true },
				Now:         func() time.Time { return at },
			}
		}
		w := newWatch()
		w.Apply = func() error {
			cfg, err := store.LoadVPNConfig()
			if err != nil {
				return err
			}
			if cfg.Xray.PendingRestore != nil {
				return errApply
			}
			return nil
		}
		w.Tick(context.Background())
		if snapshot := w.Snapshot(); snapshot.CommittedFailover || !snapshot.PendingRestore {
			t.Fatalf("failed final apply hid the durable recovery intent: %+v", snapshot)
		}
		cfg, err := store.LoadVPNConfig()
		if err != nil || cfg.Xray.Failover != nil || cfg.Xray.PendingRestore == nil {
			t.Fatalf("failed apply did not persist pending restore: cfg=%+v error=%v", cfg, err)
		}
		w = newWatch()
		var during watchdapi.WatchSnapshot
		w.Apply = func() error {
			during = w.Snapshot()
			return nil
		}
		w.Tick(context.Background())
		assertWatchSnapshotAction(t, during, "restoring")
		if during.CommittedFailover || !during.PendingRestore {
			t.Fatalf("restore apply has wrong durable intent flags: %+v", during)
		}
		assertWatchSnapshotAction(t, w.Snapshot(), "")
		if snapshot := w.Snapshot(); snapshot.CommittedFailover || snapshot.PendingRestore {
			t.Fatalf("successful restore retained stale recovery flags: %+v", snapshot)
		}
		cfg, err = store.LoadVPNConfig()
		if err != nil || cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil {
			t.Fatalf("successful restore left persisted intent: cfg=%+v error=%v", cfg, err)
		}
	})
}

func TestWatchSnapshot_ClosedGate(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		close func(*Watch)
		want  watchdapi.WatchState
	}{
		{"stopped", func(w *Watch) { w.Stopped = func() bool { return true } }, watchdapi.WatchStopped},
		{"incompatible", func(w *Watch) { w.CanMutate = func() error { return watchcompat.ErrIncompatible } }, watchdapi.WatchIncompatible},
	} {
		t.Run("first tick reads persisted intent/"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := service.NewConfigService(dir, filepath.Join(dir, "data"))
			document := `{"monitor":{"enabled":false},"tunnel_director":{"tunnels":{"ovpnc2":{"clients":["192.168.1.8"]}}},"xray":{"clients":[],"failover":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"],"committed":true},"pending_restore":{"snapshot":{"tunnel":"ovpnc2","clients":["192.168.1.8"],"added":["192.168.1.8"],"committed":true},"restored":["192.168.1.8"],"active":null}}}`
			if err := os.WriteFile(store.ConfigPath(), []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			applies, restarts, probes := 0, 0, 0
			var notes []string
			w := &Watch{
				LoadVPN:     store.LoadVPNConfig,
				UpdateVPN:   store.UpdateVPNConfig,
				Apply:       func() error { applies++; return nil },
				RestartXray: func() error { restarts++; return nil },
				Probe:       func(context.Context, int) error { probes++; return nil },
				Notify:      func(msg string) { notes = append(notes, msg) },
				Now:         func() time.Time { return at },
			}
			tc.close(w)
			w.Tick(context.Background())
			snapshot := w.Snapshot()
			if snapshot.State != tc.want || !snapshot.CommittedFailover || !snapshot.PendingRestore {
				t.Fatalf("snapshot %+v, want %s with the persisted committed failover and pending restore", snapshot, tc.want)
			}
			if applies != 0 || restarts != 0 || probes != 0 || len(notes) != 0 {
				t.Fatalf("a closed gate acted: applies=%d restarts=%d probes=%d notes=%v", applies, restarts, probes, notes)
			}
			after, err := os.ReadFile(store.ConfigPath())
			if err != nil || string(after) != document {
				t.Fatalf("a closed gate changed persisted intent: %s, error=%v", after, err)
			}
			assertWatchSnapshotSafe(t, snapshot)
		})
	}

	t.Run("a refusing gate check is not shown as active", func(t *testing.T) {
		f := &fake{cfg: baseCfg(), now: at}
		w := f.watch()
		var during []watchdapi.WatchSnapshot
		w.CanMutate = func() error {
			during = append(during, w.Snapshot())
			return watchcompat.ErrIncompatible
		}
		w.Tick(context.Background())
		w.Tick(context.Background())
		if len(during) != 2 {
			t.Fatalf("gate checks %d, want one a tick", len(during))
		}
		for i, snapshot := range during {
			if snapshot.State == watchdapi.WatchActive {
				t.Fatalf("tick %d showed the watch active while its gate check refused: %+v", i+1, snapshot)
			}
		}
		if during[1].State != watchdapi.WatchIncompatible {
			t.Fatalf("later tick's check saw %q, want the incompatible state the last tick left", during[1].State)
		}
	})
}

func TestWatchSnapshot_LogsExcludeRawErrors(t *testing.T) {
	fault := errors.New("https://provider.example/private/" + watchStatusSecret + " bot_token=synthetic-token provider-payload=private")
	for _, tc := range []struct {
		name    string
		prepare func() *Watch
		message string
		level   string
		state   watchdapi.WatchState
	}{
		{"config read", func() *Watch {
			f := &fake{cfg: baseCfg(), now: time.Unix(1_700_000_000, 0)}
			w := f.watch()
			w.LoadVPN = func() (*vpnconfig.VPNDirectorConfig, error) { return nil, fault }
			return w
		}, "Failed to load VPN Director config for the subscription watch", "WARN", watchdapi.WatchError},
		{"primary probe", func() *Watch {
			f := &fake{cfg: baseCfg(), probeErr: fault, now: time.Unix(1_700_000_000, 0)}
			return f.watch()
		}, "Xray SOCKS probe failed", "DEBUG", watchdapi.WatchActive},
		{"restore apply retry", func() *Watch {
			f := &fake{cfg: baseCfg(), applyErr: fault, now: time.Unix(1_700_000_000, 0)}
			w := f.watch()
			w.pendingApply = true
			return w
		}, "Apply retry after restoring Xray clients failed", "WARN", watchdapi.WatchActive},
		{"preferred return generation", func() *Watch {
			r := newReturnRig(returnServers())
			r.up[osloIP] = true
			r.w.Generate = func(vpnconfig.Server, func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				return false, r.f.seq(), fault
			}
			return r.w
		}, "Generating Xray config for server failed", "WARN", watchdapi.WatchActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })
			w := tc.prepare()
			w.Tick(context.Background())
			for _, forbidden := range []string{watchStatusSecret, "https://provider.example/private/", "synthetic-token", "provider-payload", "bot_token"} {
				if strings.Contains(output.String(), forbidden) {
					t.Fatalf("watch log leaked %q: %s", forbidden, &output)
				}
			}
			found := false
			decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
			for {
				var record struct {
					Level   string `json:"level"`
					Message string `json:"msg"`
					Error   struct {
						Kind string `json:"kind"`
					} `json:"error"`
				}
				if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if record.Message == tc.message {
					found = true
					if record.Level != tc.level || record.Error.Kind == "" {
						t.Fatalf("failure lost its level or safe diagnostic: %+v", record)
					}
				}
			}
			if !found {
				t.Fatalf("failure context %q was suppressed: %s", tc.message, &output)
			}
			if snapshot := w.Snapshot(); snapshot.State != tc.state || snapshot.Action != "" {
				t.Fatalf("safe logging changed watch availability: %+v", snapshot)
			}
		})
	}

	t.Run("guarded operations preserve original error identity", func(t *testing.T) {
		f := &fake{cfg: baseCfg(), now: time.Unix(1_700_000_000, 0)}
		w := f.watch()
		w.Apply = func() error { return fault }
		w.RestartXray = func() error { return fault }
		if err := w.apply(); err != fault || !errors.Is(err, fault) {
			t.Fatalf("Apply error was replaced for logging: %v", err)
		}
		if err := w.restartXray(); err != fault || !errors.Is(err, fault) {
			t.Fatalf("RestartXray error was replaced for logging: %v", err)
		}
	})
}
