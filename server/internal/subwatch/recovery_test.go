package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

const recoveryConfigJSON = `{
	"paused_clients": ["192.168.1.9"],
	"tunnel_director": {"tunnels": {
		"ovpnc2": {"clients": ["192.168.1.3", "192.168.1.4"], "exclude": ["zz"]},
		"wgc1": {"clients": ["192.168.1.20"], "exclude": []}
	}},
	"xray": {
		"clients": ["192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"],
		"active_server": {"name": "Oslo", "address": "example.com", "port": 443, "subscription": "alpha", "seq": 7},
		"pending_restore": {
			"snapshot": {"tunnel": "ovpnc2", "clients": ["192.168.1.8", "192.168.1.3"], "added": ["192.168.1.8"], "committed": true},
			"restored": ["192.168.1.8", "192.168.1.3"],
			"active": {"name": "Oslo", "address": "example.com", "port": 443, "subscription": "alpha", "seq": 7}
		}
	}
}`

// Read the persisted contract independently of the production config decoder.
type recoveryIntent struct {
	Snapshot *vpnconfig.XrayFailover
	Restored []string
	Active   *vpnconfig.ActiveServer
}

func decodeRecoveryIntent(t *testing.T, raw []byte) *recoveryIntent {
	t.Helper()
	var doc struct {
		Xray struct {
			PendingRestore json.RawMessage `json:"pending_restore"`
		} `json:"xray"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Xray.PendingRestore) == 0 {
		return nil
	}
	if string(doc.Xray.PendingRestore) == "null" {
		t.Fatal("nil pending_restore must be omitted, not written as null")
	}
	var intent recoveryIntent
	if err := json.Unmarshal(doc.Xray.PendingRestore, &intent); err != nil {
		t.Fatal(err)
	}
	return &intent
}

func recoveryIntentForConfig(t *testing.T, cfg *vpnconfig.VPNDirectorConfig) *recoveryIntent {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return decodeRecoveryIntent(t, raw)
}

func recoveryFailoverConfig() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		PausedClients: []string{"192.168.1.9"},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"ovpnc2": {Clients: []string{"192.168.1.3", "192.168.1.4", "192.168.1.8"}, Exclude: []string{"zz"}},
			"wgc1":   {Clients: []string{"192.168.1.20"}, Exclude: []string{}},
		}},
		Xray: vpnconfig.XrayConfig{
			Clients:      []string{"192.168.1.9", "192.168.1.10"},
			ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
			Failover:     &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}, Committed: true},
		},
	}
}

type recoverySystem struct {
	store        *service.ConfigService
	readiness    netpath.Readiness
	now          time.Time
	applies      int
	probes       int
	probeErr     error
	notes        []string
	subs         []vpnconfig.Subscription
	incompatible atomic.Bool
}

func newRecoverySystem(t *testing.T) *recoverySystem {
	t.Helper()
	dir := t.TempDir()
	s := &recoverySystem{
		store: service.NewConfigService(dir, filepath.Join(dir, "data")),
		readiness: netpath.Readiness{
			TPROXYPath:  filepath.Join(dir, "tproxy-ready"),
			StoppedPath: filepath.Join(dir, "stopped"),
		},
		now: time.Unix(1_700_000_000, 0),
	}
	if err := vpnconfig.SaveVPNDirectorConfig(s.store.ConfigPath(), recoveryFailoverConfig()); err != nil {
		t.Fatal(err)
	}
	s.setReady(t, true)
	return s
}

func (s *recoverySystem) seedPending(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.store.ConfigPath(), []byte(recoveryConfigJSON), 0600); err != nil {
		t.Fatal(err)
	}
}

func (s *recoverySystem) setReady(t *testing.T, ready bool) {
	t.Helper()
	if ready {
		if err := os.WriteFile(s.readiness.TPROXYPath, []byte("ready\n"), 0600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Remove(s.readiness.TPROXYPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func (s *recoverySystem) watch() *Watch {
	return &Watch{
		LoadVPN:   s.store.LoadVPNConfig,
		UpdateVPN: s.store.UpdateVPNConfig,
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) {
			return connected("ovpnc2"), nil
		},
		LoadSubscriptions: func() ([]vpnconfig.Subscription, error) {
			return cloneSubs(s.subs), nil
		},
		Apply: func() error {
			s.applies++
			return nil
		},
		Probe: func(context.Context, int) error {
			s.probes++
			return s.probeErr
		},
		Notify:        func(msg string) { s.notes = append(s.notes, msg) },
		Now:           func() time.Time { return s.now },
		TPROXYReady:   s.readiness.TPROXYReady,
		Stopped:       s.readiness.Stopped,
		FallbackReady: func(id string) bool { return id == "ovpnc2" },
		CanMutate: func() error {
			if s.incompatible.Load() {
				return watchcompat.ErrIncompatible
			}
			return nil
		},
	}
}

func (s *recoverySystem) read(t *testing.T) (*vpnconfig.VPNDirectorConfig, *recoveryIntent, []byte) {
	t.Helper()
	raw, err := os.ReadFile(s.store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.store.LoadVPNConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, decodeRecoveryIntent(t, raw), raw
}

func assertRecoveryFinished(t *testing.T, s *recoverySystem) {
	t.Helper()
	cfg, pending, _ := s.read(t)
	if pending != nil || cfg.Xray.Failover != nil {
		t.Fatalf("finished restore retained pending %+v or failover %+v", pending, cfg.Xray.Failover)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["wgc1"].Clients, []string{"192.168.1.20"}) {
		t.Fatalf("finished restore changed overlap or unrelated assignments: xray %v, tunnels %v", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels)
	}
	if countNotes(s.notes, "LAN clients back on Xray") != 1 {
		t.Fatalf("notes %v; successful persistent recovery must announce once", s.notes)
	}
}

func TestRecovery_PendingRestoreAfterRestart(t *testing.T) {
	s := newRecoverySystem(t)
	w := s.watch()
	w.Apply = func() error {
		s.applies++
		cfg, _, _ := s.read(t)
		if cfg.Xray.Failover == nil {
			return errApply
		}
		return nil
	}
	w.Tick(context.Background())
	cfg, pending, beforeStop := s.read(t)
	want := &recoveryIntent{
		Snapshot: &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}, Committed: true},
		Restored: []string{"192.168.1.8", "192.168.1.3"},
		Active:   &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
	}
	if cfg.Xray.Failover != nil || !reflect.DeepEqual(pending, want) || len(s.notes) != 0 {
		t.Fatalf("interrupted restore: failover %+v, pending %+v, notes %v; intent must survive the failed Apply", cfg.Xray.Failover, pending, s.notes)
	}
	if err := os.WriteFile(s.readiness.StoppedPath, nil, 0600); err != nil {
		t.Fatal(err)
	}

	// This Watch has none of the earlier process's pendingApply/pendingRestore state.
	w = s.watch()
	applies, probes := s.applies, s.probes
	w.Tick(context.Background())
	_, _, afterStop := s.read(t)
	if string(afterStop) != string(beforeStop) || s.applies != applies || s.probes != probes || len(s.notes) != 0 {
		t.Fatal("startup recovery wrote, applied, probed or notified while /stop was present")
	}
	if err := os.Remove(s.readiness.StoppedPath); err != nil {
		t.Fatal(err)
	}
	s.setReady(t, false)
	w.Tick(context.Background())
	cfg, pending, _ = s.read(t)
	if !reflect.DeepEqual(cfg.Xray.Failover, want.Snapshot) || pending != nil {
		t.Fatalf("missing TPROXY: failover %+v, pending %+v; durable fallback snapshot must replace the completed config restore", cfg.Xray.Failover, pending)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4", "192.168.1.8"}) || len(s.notes) != 0 {
		t.Fatalf("rollback assignments: xray %v, ovpnc2 %v, notes %v", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, s.notes)
	}
	s.now = s.now.Add(ProbeInterval)
	w.Tick(context.Background())
	assertStillOnTunnel(t, mustLoadRecovery(t, s))
	if countNotes(s.notes, "LAN clients back on Xray") != 0 {
		t.Fatalf("restore announced without LAN readiness: %v", s.notes)
	}

	s.setReady(t, true)
	s.now = s.now.Add(ImportRetry)
	w.Tick(context.Background())
	assertRecoveryFinished(t, s)
	applies = s.applies
	w.Tick(context.Background())
	s.watch().Tick(context.Background())
	assertRecoveryFinished(t, s)
	if s.applies != applies {
		t.Fatalf("applies %d after %d; completed intent was replayed", s.applies, applies)
	}
}

func mustLoadRecovery(t *testing.T, s *recoverySystem) *vpnconfig.VPNDirectorConfig {
	t.Helper()
	cfg, _, _ := s.read(t)
	return cfg
}

func TestRecovery_UnarmedAndManualChanges(t *testing.T) {
	t.Run("no subscriptions still completes and does not replay", func(t *testing.T) {
		s := newRecoverySystem(t)
		s.seedPending(t)
		w := s.watch()
		w.Tick(context.Background())
		if s.applies == 0 {
			t.Fatal("a persisted pending restore was ignored without subscriptions")
		}
		assertRecoveryFinished(t, s)
		applies := s.applies
		w.Tick(context.Background())
		s.watch().Tick(context.Background())
		assertRecoveryFinished(t, s)
		if s.applies != applies {
			t.Fatal("successful recovery was applied again after pending_restore was cleared")
		}
	})
	t.Run("no clients still finishes intent without resurrecting them", func(t *testing.T) {
		s := newRecoverySystem(t)
		s.seedPending(t)
		if err := s.store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
			cfg.Xray.Clients = []string{}
			cfg.TunnelDirector.Tunnels = map[string]vpnconfig.TunnelConfig{}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, before, _ := s.read(t)
		if before == nil {
			t.Fatal("config update discarded the intent before the unarmed recovery could finish it")
		}
		s.watch().Tick(context.Background())
		cfg, pending, _ := s.read(t)
		if pending != nil || cfg.Xray.Failover != nil || len(cfg.Xray.Clients) != 0 || len(cfg.TunnelDirector.Tunnels) != 0 {
			t.Fatalf("empty assignments resurrected or intent left pending: %+v, pending %+v", cfg, pending)
		}
	})

	for _, tc := range []struct {
		name         string
		mutate       func(*vpnconfig.VPNDirectorConfig)
		wantXray     []string
		wantWGC      []string
		wantPaused   []string
		wantActive   *vpnconfig.ActiveServer
		wantRollback bool
	}{
		{
			name: "same server manually reselected",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(cfg.Xray.ActiveServer, vpnconfig.Server{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha"})
			},
			wantXray:   []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"},
			wantWGC:    []string{"192.168.1.20"},
			wantPaused: []string{"192.168.1.9"},
			wantActive: &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 8},
		},
		{
			name: "different active identity with unchanged seq",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Manual", Address: "example.net", Port: 8443, Subscription: "beta", Seq: 7}
			},
			wantXray:   []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"},
			wantWGC:    []string{"192.168.1.20"},
			wantPaused: []string{"192.168.1.9"},
			wantActive: &vpnconfig.ActiveServer{Name: "Manual", Address: "example.net", Port: 8443, Subscription: "beta", Seq: 7},
		},
		{
			name: "paused client",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				cfg.PausedClients = []string{"192.168.1.9", "192.168.1.8"}
			},
			wantXray:     []string{"192.168.1.9", "192.168.1.10", "192.168.1.8"},
			wantWGC:      []string{"192.168.1.20"},
			wantPaused:   []string{"192.168.1.9", "192.168.1.8"},
			wantActive:   &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
			wantRollback: true,
		},
		{
			name: "deleted client without detach",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				cfg.Xray.Clients = []string{"192.168.1.9", "192.168.1.10", "192.168.1.3"}
			},
			wantXray:     []string{"192.168.1.9", "192.168.1.10"},
			wantWGC:      []string{"192.168.1.20"},
			wantPaused:   []string{"192.168.1.9"},
			wantActive:   &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
			wantRollback: true,
		},
		{
			name: "moved client through MoveClient",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				vpnconfig.MoveClient(cfg, "192.168.1.8/32", "wgc1")
			},
			wantXray:     []string{"192.168.1.9", "192.168.1.10"},
			wantWGC:      []string{"192.168.1.20", "192.168.1.8"},
			wantPaused:   []string{"192.168.1.9"},
			wantActive:   &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
			wantRollback: true,
		},
		{
			name: "moved client without detach",
			mutate: func(cfg *vpnconfig.VPNDirectorConfig) {
				cfg.Xray.Clients = []string{"192.168.1.9", "192.168.1.10", "192.168.1.3"}
				cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.20", "192.168.1.8"}, Exclude: []string{}}
			},
			wantXray:     []string{"192.168.1.9", "192.168.1.10"},
			wantWGC:      []string{"192.168.1.20", "192.168.1.8"},
			wantPaused:   []string{"192.168.1.9"},
			wantActive:   &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
			wantRollback: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRecoverySystem(t)
			s.seedPending(t)
			if err := s.store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
				tc.mutate(cfg)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, before, _ := s.read(t)
			if before == nil {
				t.Fatal("manual config write discarded pending_restore instead of preserving/filtering it")
			}
			s.setReady(t, false)
			s.watch().Tick(context.Background())
			cfg, _, _ := s.read(t)
			if !reflect.DeepEqual(cfg.Xray.Clients, tc.wantXray) || !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["wgc1"].Clients, tc.wantWGC) ||
				!reflect.DeepEqual(cfg.PausedClients, tc.wantPaused) || !reflect.DeepEqual(cfg.Xray.ActiveServer, tc.wantActive) {
				t.Fatalf("recovery overrode manual assignments: xray %v, wgc1 %v, paused %v, active %+v", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["wgc1"].Clients, cfg.PausedClients, cfg.Xray.ActiveServer)
			}
			if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4"}) {
				t.Fatalf("stale client was returned to the fallback: %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
			}
			if tc.wantRollback {
				want := &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.3"}, Added: []string{}, Committed: true}
				if !reflect.DeepEqual(cfg.Xray.Failover, want) {
					t.Fatalf("filtered rollback = %+v, want %+v", cfg.Xray.Failover, want)
				}
			} else if cfg.Xray.Failover != nil {
				t.Fatalf("newer manual active selection got stale rollback %+v", cfg.Xray.Failover)
			}
			if len(s.notes) != 0 {
				t.Fatalf("recovery announced a restore without readiness or for an old selection: %v", s.notes)
			}
		})
	}
}

func TestRecovery_RollbackMovesOnlyRestoredClients(t *testing.T) {
	s := newRecoverySystem(t)
	raw := strings.Replace(recoveryConfigJSON,
		`"clients": ["192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"]`,
		`"clients": ["192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3", "192.168.1.7"]`, 1)
	raw = strings.Replace(raw,
		`"clients": ["192.168.1.8", "192.168.1.3"], "added": ["192.168.1.8"]`,
		`"clients": ["192.168.1.8", "192.168.1.3", "192.168.1.7"], "added": ["192.168.1.8", "192.168.1.7"]`, 1)
	if err := os.WriteFile(s.store.ConfigPath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	s.setReady(t, false)
	s.watch().Tick(context.Background())
	cfg, pending, _ := s.read(t)
	want := &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}, Committed: true}
	if pending != nil || !reflect.DeepEqual(cfg.Xray.Failover, want) || !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.7"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4", "192.168.1.8"}) || len(s.notes) != 0 {
		t.Fatalf("rollback used snapshot clients that were not restored: failover %+v, pending %+v, xray %v, ovpnc2 %v, notes %v", cfg.Xray.Failover, pending, cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, s.notes)
	}
}

func TestRecovery_StagedSnapshotKeepsXrayMembership(t *testing.T) {
	s := newRecoverySystem(t)
	raw := strings.Replace(recoveryConfigJSON, `"committed": true`, `"committed": false`, 1)
	if err := os.WriteFile(s.store.ConfigPath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	s.setReady(t, false)
	w := s.watch()
	w.Tick(context.Background())
	cfg, pending, _ := s.read(t)
	want := &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}}
	if pending != nil || !reflect.DeepEqual(cfg.Xray.Failover, want) || !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4", "192.168.1.8"}) || len(s.notes) != 0 {
		t.Fatalf("staged intent was committed during recovery: failover %+v, pending %+v, xray %v, notes %v", cfg.Xray.Failover, pending, cfg.Xray.Clients, s.notes)
	}
	s.setReady(t, true)
	s.now = s.now.Add(ImportRetry)
	w.Tick(context.Background())
	cfg, pending, _ = s.read(t)
	if pending != nil || cfg.Xray.Failover != nil || !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4"}) || len(s.notes) != 0 {
		t.Fatalf("abandoned stage announced a restore or lost ownership: failover %+v, pending %+v, xray %v, notes %v", cfg.Xray.Failover, pending, cfg.Xray.Clients, s.notes)
	}
}

func recoveryMutationTarget(t *testing.T, phase string, cfg *vpnconfig.VPNDirectorConfig, applies int) bool {
	t.Helper()
	switch phase {
	case "creation":
		return cfg.Xray.Failover != nil && contains(cfg.Xray.Clients, "192.168.1.8")
	case "reinstatement":
		return recoveryIntentForConfig(t, cfg) != nil
	case "final clear":
		return applies > 0 && recoveryIntentForConfig(t, cfg) != nil
	default:
		t.Fatalf("unknown recovery mutation phase %q", phase)
		return false
	}
}

func TestRecovery_LockedMutationGuards(t *testing.T) {
	for _, phase := range []string{"creation", "reinstatement", "final clear"} {
		for _, refusal := range []string{"stop", "incompatible", "canceled", "deadline"} {
			t.Run(phase+"/"+refusal, func(t *testing.T) {
				s := newRecoverySystem(t)
				if phase != "creation" {
					s.seedPending(t)
				}
				s.setReady(t, phase != "reinstatement")
				w := s.watch()
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				wantErr := errStopped
				switch refusal {
				case "incompatible":
					wantErr = watchcompat.ErrIncompatible
				case "canceled":
					wantErr = context.Canceled
				case "deadline":
					wantErr = context.DeadlineExceeded
				}
				refused := false
				var before []byte
				appliesAtRefusal := 0
				w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
					return s.store.UpdateVPNConfig(func(current *vpnconfig.VPNDirectorConfig) error {
						if !refused && recoveryMutationTarget(t, phase, current, s.applies) {
							refused = true
							var err error
							before, err = os.ReadFile(s.store.ConfigPath())
							if err != nil {
								return err
							}
							appliesAtRefusal = s.applies
							switch refusal {
							case "stop":
								if err := os.WriteFile(s.readiness.StoppedPath, nil, 0600); err != nil {
									return err
								}
							case "incompatible":
								s.incompatible.Store(true)
							case "canceled", "deadline":
								cancel(wantErr)
							}
						}
						err := fn(current)
						if refused && !errors.Is(err, wantErr) {
							t.Errorf("locked %s error %v, want %v after %s", phase, err, wantErr, refusal)
						}
						return err
					})
				}
				w.Tick(ctx)
				if !refused {
					t.Fatal("persistent recovery never reached the guarded config-lock boundary")
				}
				_, _, after := s.read(t)
				if string(after) != string(before) || s.applies != appliesAtRefusal || len(s.notes) != 0 {
					t.Fatalf("%s continued after locked %s: config changed %v, applies %d after %d, notes %v", phase, refusal, string(after) != string(before), s.applies, appliesAtRefusal, s.notes)
				}
			})
		}
	}
}

func TestRecovery_IdentitySequenceGuards(t *testing.T) {
	for _, phase := range []string{"creation", "reinstatement", "final clear"} {
		for _, change := range []string{"same server new seq", "different identity same seq"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				s := newRecoverySystem(t)
				if phase != "creation" {
					s.seedPending(t)
				}
				s.setReady(t, phase != "reinstatement")
				w := s.watch()
				selected := false
				wantActive := &vpnconfig.ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 8}
				if change == "different identity same seq" {
					wantActive = &vpnconfig.ActiveServer{Name: "Manual", Address: "example.net", Port: 8443, Subscription: "beta", Seq: 7}
				}
				w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
					current, err := s.store.LoadVPNConfig()
					if err != nil {
						return err
					}
					if !selected && recoveryMutationTarget(t, phase, current, s.applies) {
						selected = true
						// Commit the selection before the queued recovery callback takes its lock.
						if err := s.store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
							a := *wantActive
							cfg.Xray.ActiveServer = &a
							return nil
						}); err != nil {
							return err
						}
					}
					return s.store.UpdateVPNConfig(fn)
				}
				w.Tick(context.Background())
				if !selected {
					t.Fatal("persistent recovery never reached the selection/config-lock race")
				}
				cfg, pending, _ := s.read(t)
				if !reflect.DeepEqual(cfg.Xray.ActiveServer, wantActive) || len(s.notes) != 0 {
					t.Fatalf("manual selection overwritten or stale restore announced: active %+v, notes %v", cfg.Xray.ActiveServer, s.notes)
				}
				if phase == "creation" {
					if pending != nil || !reflect.DeepEqual(cfg.Xray.Failover, recoveryFailoverConfig().Xray.Failover) || contains(cfg.Xray.Clients, "192.168.1.8") {
						t.Fatalf("intent created for an unprobed selection: pending %+v, failover %+v, xray %v", pending, cfg.Xray.Failover, cfg.Xray.Clients)
					}
				} else if cfg.Xray.Failover != nil || !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
					!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4"}) {
					t.Fatalf("pending intent rolled back a newer selection: failover %+v, xray %v, ovpnc2 %v", cfg.Xray.Failover, cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
				}
			})
		}
	}
}

func TestRecovery_PendingMutationWriteFailure(t *testing.T) {
	for _, phase := range []string{"reinstatement", "final clear"} {
		t.Run(phase, func(t *testing.T) {
			s := newRecoverySystem(t)
			s.seedPending(t)
			s.setReady(t, phase != "reinstatement")
			w := s.watch()
			failedWrites := 0
			w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
				cfg, err := s.store.LoadVPNConfig()
				if err != nil {
					return err
				}
				if recoveryMutationTarget(t, phase, cfg, s.applies) {
					failedWrites++
					return service.ErrConfigLockTimeout
				}
				return s.store.UpdateVPNConfig(fn)
			}
			w.Tick(context.Background())
			cfg, pending, _ := s.read(t)
			if failedWrites == 0 || pending == nil || cfg.Xray.Failover != nil || len(s.notes) != 0 {
				t.Fatalf("failed persistent mutation lost intent or announced success: attempts %d, pending %+v, failover %+v, notes %v", failedWrites, pending, cfg.Xray.Failover, s.notes)
			}
			if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
				!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4"}) {
				t.Fatalf("failed locked mutation partially changed routing: xray %v, ovpnc2 %v", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
			}
			s.now = s.now.Add(ImportRetry)
			w = s.watch()
			w.Tick(context.Background())
			if phase == "reinstatement" {
				cfg, pending, _ = s.read(t)
				if pending != nil || cfg.Xray.Failover == nil {
					t.Fatalf("new process failed to retry reinstatement: pending %+v, failover %+v", pending, cfg.Xray.Failover)
				}
				s.setReady(t, true)
				s.now = s.now.Add(ImportRetry)
				w.Tick(context.Background())
			}
			assertRecoveryFinished(t, s)
		})
	}
}

func TestRecovery_NewDeathRequiresFreshConfirmation(t *testing.T) {
	s := newRecoverySystem(t)
	s.seedPending(t)
	w := s.watch()
	w.failSince = s.now.Add(-time.Hour)
	w.downChecks = 2
	w.Tick(context.Background())
	assertRecoveryFinished(t, s)

	s.subs = []vpnconfig.Subscription{{ID: "static", Name: "Synthetic"}}
	s.probeErr = errProbe
	start := s.now.Add(ProbeInterval)
	for elapsed := time.Duration(0); elapsed < 3*time.Minute; elapsed += 30 * time.Second {
		s.now = start.Add(elapsed)
		w.Tick(context.Background())
		if cfg := mustLoadRecovery(t, s); cfg.Xray.Failover != nil {
			t.Fatalf("new death reused old confirmation at %v: %+v", elapsed, cfg.Xray.Failover)
		}
	}
	s.now = start.Add(3 * time.Minute)
	w.Tick(context.Background())
	if cfg := mustLoadRecovery(t, s); cfg.Xray.Failover == nil {
		t.Fatal("fresh legacy failure was not confirmed after three minutes")
	}
	if countNotes(s.notes, "Xray outbound is down; LAN clients moved") != 1 {
		t.Fatalf("fresh death must announce one move, notes %v", s.notes)
	}
}
