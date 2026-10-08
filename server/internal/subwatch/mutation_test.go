package subwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

func mutationGate(incompatible *atomic.Bool) func() error {
	return func() error {
		if incompatible.Load() {
			return watchcompat.ErrIncompatible
		}
		return nil
	}
}

func mutationConfig() *vpnconfig.VPNDirectorConfig {
	// Tunnel Director requires the existing RFC1918 LAN fixture.
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 7}
	return cfg
}

func commitMutationFailover(t *testing.T, cfg *vpnconfig.VPNDirectorConfig) {
	t.Helper()
	clients := vpnconfig.EffectiveXrayClients(cfg)
	if len(clients) != 1 || !reflect.DeepEqual(vpnconfig.CarriableXrayClients(cfg), clients) {
		t.Fatalf("fixture clients %v must contain one unpaused client Tunnel Director can carry", clients)
	}
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	assertStagedOnTunnel(t, cfg)
	if !vpnconfig.FailoverStaged(cfg) || !reflect.DeepEqual(cfg.Xray.Failover.Clients, clients) {
		t.Fatalf("fixture failover %+v must be staged for %v before the injected wait", cfg.Xray.Failover, clients)
	}
	vpnconfig.CommitXrayFailover(cfg)
	assertStillOnTunnel(t, cfg)
	if !vpnconfig.FailoverCommitted(cfg) || vpnconfig.FailoverStaged(cfg) || !reflect.DeepEqual(cfg.Xray.Clients, cfg.PausedClients) {
		t.Fatalf("fixture failover %+v must be committed with only paused Xray clients; clients %v", cfg.Xray.Failover, cfg.Xray.Clients)
	}
}

func TestMutation_AllowedDefaultsAndReadiness(t *testing.T) {
	w := &Watch{}
	if err := w.mutationAllowed(); err != nil {
		t.Fatalf("nil CanMutate must preserve existing watch defaults: %v", err)
	}
	var incompatible atomic.Bool
	w.CanMutate = mutationGate(&incompatible)
	if err := w.mutationAllowed(); err != nil {
		t.Fatal(err)
	}
	incompatible.Store(true)
	if err := w.mutationAllowed(); !errors.Is(err, watchcompat.ErrIncompatible) {
		t.Fatalf("mutation error %v, want ErrIncompatible", err)
	}
	incompatible.Store(false)
	readiness := netpath.Readiness{StoppedPath: filepath.Join(t.TempDir(), "stopped")}
	w.Stopped = readiness.Stopped
	if err := os.WriteFile(readiness.StoppedPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.mutationAllowed(); !errors.Is(err, errStopped) {
		t.Fatalf("mutation error %v, want the existing /stop refusal", err)
	}
	if err := os.Remove(readiness.StoppedPath); err != nil {
		t.Fatal(err)
	}
	if err := w.mutationAllowed(); err != nil {
		t.Fatalf("removed stop marker must permit a compatible watch: %v", err)
	}
}

type mutationGeneration struct {
	generated bool
	err       error
}

type mutationSystem struct {
	w           *Watch
	store       *service.ConfigService
	xray        *service.XrayService
	outputPath  string
	before      []byte
	generations []mutationGeneration
	restarts    int
	applies     int
	notes       []string
}

func newMutationSystem(t *testing.T) *mutationSystem {
	t.Helper()
	dir := t.TempDir()
	store := service.NewConfigService(dir, filepath.Join(dir, "data"))
	cfg := mutationConfig()
	cfg.Xray.ActiveServer.Subscription = "static"
	commitMutationFailover(t, cfg)
	if err := vpnconfig.SaveVPNDirectorConfig(store.ConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(dir, "xray.template.json")
	outputPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(templatePath, []byte(`{"inbounds":[],"outbounds":[],"routing":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	s := &mutationSystem{store: store, xray: service.NewXrayService(templatePath, outputPath), outputPath: outputPath, before: before}
	s.w = runningWatch(&Watch{
		LoadVPN:   store.LoadVPNConfig,
		UpdateVPN: store.UpdateVPNConfig,
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) {
			return vpnconfig.PlatformInfo{}, nil
		},
		LoadSubscriptions: func() ([]vpnconfig.Subscription, error) {
			return []vpnconfig.Subscription{{ID: "static", Name: "Synthetic", Servers: []vpnconfig.Server{
				{Subscription: "static", Name: "Oslo", Address: "oslo.example", Port: 443, UUID: "synthetic-id", Security: "tls", IPs: []string{"203.0.113.10"}},
			}}}, nil
		},
		Fetch: func(context.Context, string) ([]vpnconfig.Server, error) {
			t.Error("a static subscription must not be fetched")
			return nil, errors.New("unexpected fetch")
		},
		Probe: func(context.Context, int) error { return errProbe },
		Apply: func() error {
			s.applies++
			return nil
		},
		RestartXray: func() error {
			s.restarts++
			return nil
		},
		AfterRestart: func(time.Duration) {},
		Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
		Notify:       func(note string) { s.notes = append(s.notes, note) },
	})
	s.generateWith(store)
	return s
}

func (s *mutationSystem) generateWith(store service.ConfigStore) {
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(store, s.xray, endpoint.ServerForDial(server), server, service.InboundPorts{}, guard)
		s.generations = append(s.generations, mutationGeneration{generated: generated, err: err})
		return generated, seq, err
	}
}

func (s *mutationSystem) assertUnchanged(t *testing.T) {
	t.Helper()
	if len(s.generations) != 1 || s.generations[0].generated || !errors.Is(s.generations[0].err, watchcompat.ErrIncompatible) {
		t.Errorf("generations %+v, want one generated=false compatibility refusal", s.generations)
	}
	after, err := os.ReadFile(s.store.ConfigPath())
	if err != nil || string(after) != string(s.before) {
		t.Errorf("VPN config/selection changed after compatibility loss: error %v", err)
	}
	content, err := os.ReadFile(s.outputPath)
	if err != nil || string(content) != "previous\n" {
		t.Errorf("live Xray config %q, error %v; refused generation must leave it unchanged", content, err)
	}
	if s.restarts != 0 || s.applies != 0 || len(s.notes) != 0 {
		t.Errorf("restarts %d, applies %d, notes %v after compatibility loss", s.restarts, s.applies, s.notes)
	}
	paths, err := filepath.Glob(s.outputPath + ".*")
	if err != nil || len(paths) != 0 {
		t.Errorf("temporary configs %v, error %v; rejected generation must clean its stage", paths, err)
	}
}

type mutationWaitStore struct {
	service.ConfigStore
	entered chan struct{}
	once    sync.Once
}

func (s *mutationWaitStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	s.once.Do(func() { close(s.entered) })
	return s.ConfigStore.UpdateVPNConfig(fn)
}

func awaitMutation(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestMutation_GateAfterWaitAndValidation(t *testing.T) {
	t.Run("gate changes while waiting for the real config lock", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		s := newMutationSystem(t)
		var incompatible atomic.Bool
		s.w.CanMutate = mutationGate(&incompatible)
		held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		tickDone := make(chan struct{})
		tickStarted := false
		t.Cleanup(func() {
			unlock()
			cancel()
			awaitMutation(t, holderDone, "lock-holder shutdown")
			if tickStarted {
				awaitMutation(t, tickDone, "Tick shutdown")
			}
		})
		holderErrors := make(chan error, 1)
		go func() {
			defer close(holderDone)
			holderErrors <- s.store.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error {
				close(held)
				<-release
				return nil
			})
		}()
		awaitMutation(t, held, "the held config lock")
		waitStore := &mutationWaitStore{ConfigStore: s.store, entered: make(chan struct{})}
		s.generateWith(waitStore)
		tickStarted = true
		go func() {
			defer close(tickDone)
			s.w.Tick(ctx)
		}()
		awaitMutation(t, waitStore.entered, "generation entering the locked store")
		incompatible.Store(true)
		unlock()
		awaitMutation(t, tickDone, "guarded generation refusal")
		awaitMutation(t, holderDone, "lock-holder completion")
		if err := <-holderErrors; err != nil {
			t.Fatal(err)
		}
		s.assertUnchanged(t)
	})
	t.Run("gate changes inside injected Xray validation", func(t *testing.T) {
		s := newMutationSystem(t)
		marker := filepath.Join(t.TempDir(), "incompatible")
		s.w.CanMutate = func() error {
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				return watchcompat.ErrIncompatible
			}
			return nil
		}
		binDir := t.TempDir()
		script := fmt.Sprintf("#!/bin/sh\n[ \"$#\" -eq 6 ] && [ \"$1\" = run ] && [ \"$2\" = -test ] && [ \"$3\" = -format ] && [ \"$4\" = json ] && [ \"$5\" = -c ] && [ \"$6\" != %q ] || exit 94\nprintf 'unconfirmed\\n' > %q\n", s.outputPath, marker)
		if err := os.WriteFile(filepath.Join(binDir, "xray"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		s.w.Tick(context.Background())
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("injected validation never executed: %v", err)
		}
		s.assertUnchanged(t)
	})
}

func TestMutation_LockedUpdatesRecheckGate(t *testing.T) {
	cfg := mutationConfig()
	before, err := cloneCfg(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var incompatible atomic.Bool
	bodyCalls := 0
	w := &Watch{
		CanMutate: mutationGate(&incompatible),
		UpdateVPN: func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
			incompatible.Store(true)
			return fn(cfg)
		},
	}
	err = w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		bodyCalls++
		current.Xray.Clients = nil
		return nil
	})
	if !errors.Is(err, watchcompat.ErrIncompatible) || bodyCalls != 0 {
		t.Errorf("update error %v, body calls %d; want incompatibility before the locked mutation", err, bodyCalls)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Error("the locked update changed routing despite compatibility loss during its wait")
	}
}

func TestMutation_ApplyAndRestartCheckBeforeAndAfter(t *testing.T) {
	for _, action := range []string{"apply", "restart"} {
		for _, when := range []string{"before", "during"} {
			t.Run(action+"/"+when, func(t *testing.T) {
				var incompatible atomic.Bool
				incompatible.Store(when == "before")
				calls := 0
				invoke := func() error {
					calls++
					incompatible.Store(true)
					return nil
				}
				w := &Watch{CanMutate: mutationGate(&incompatible), Apply: invoke, RestartXray: invoke}
				run := w.apply
				if action == "restart" {
					run = w.restartXray
				}
				err := run()
				wantCalls := 0
				if when == "during" {
					wantCalls = 1
				}
				if !errors.Is(err, watchcompat.ErrIncompatible) || calls != wantCalls {
					t.Errorf("error %v, calls %d; want ErrIncompatible and %d calls", err, calls, wantCalls)
				}
			})
		}
	}
}

func TestMutation_IncompatibleTickDoesNotAct(t *testing.T) {
	f := &fake{cfg: mutationConfig(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	before, err := cloneCfg(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := f.watch()
	var incompatible atomic.Bool
	incompatible.Store(true)
	w.CanMutate = mutationGate(&incompatible)
	probes, fetches, generations, restarts := 0, 0, 0, 0
	w.Probe = func(context.Context, int) error { probes++; return errProbe }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { fetches++; return nil, errProbe }
	w.Generate = func(vpnconfig.Server, func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		generations++
		return true, 8, nil
	}
	w.RestartXray = func() error { restarts++; return nil }
	tickUntilDead(w, f)
	if probes != 0 || fetches != 0 || generations != 0 || restarts != 0 || f.applies != 0 || len(f.notes) != 0 {
		t.Errorf("probes %d, fetches %d, generations %d, restarts %d, applies %d, notes %v while incompatible", probes, fetches, generations, restarts, f.applies, f.notes)
	}
	if !reflect.DeepEqual(f.cfg, before) {
		t.Error("an incompatible Tick changed the current configuration")
	}
}

func TestMutation_CompatibilityRecoveryStartsFreshDeathConfirmation(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		name := "incompatible tick"
		if duringProbe {
			name = "compatibility lost during probe"
		}
		t.Run(name, func(t *testing.T) {
			f := &fake{cfg: mutationConfig(), plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}}, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
			if clients := vpnconfig.CarriableXrayClients(f.cfg); len(clients) != 1 {
				t.Fatalf("fixture clients %v must allow a fresh confirmed failure to stage failover", clients)
			}
			w := f.watch()
			var incompatible atomic.Bool
			w.CanMutate = mutationGate(&incompatible)
			w.Tick(context.Background())
			f.now = f.now.Add(30 * time.Second)
			w.Tick(context.Background())
			f.now = f.now.Add(3 * time.Minute)
			if duringProbe {
				w.Probe = func(context.Context, int) error {
					incompatible.Store(true)
					return errProbe
				}
			} else {
				incompatible.Store(true)
			}
			w.Tick(context.Background())
			if f.cfg.Xray.Failover != nil || f.applies != 0 || len(f.notes) != 0 {
				t.Fatal("compatibility loss must not finish the old failure confirmation")
			}
			incompatible.Store(false)
			w.Probe = func(context.Context, int) error { return errProbe }
			f.now = f.now.Add(10 * time.Minute)
			w.Tick(context.Background())
			f.now = f.now.Add(3*time.Minute - 30*time.Second)
			w.Tick(context.Background())
			if f.cfg.Xray.Failover != nil || f.applies != 0 {
				t.Fatal("the next compatible Tick must start a new three-minute confirmation")
			}
			f.now = f.now.Add(30 * time.Second)
			w.Tick(context.Background())
			if !vpnconfig.FailoverCommitted(f.cfg) || f.applies != 2 {
				t.Fatalf("failover %+v, applies %d; a fresh confirmed failure must still stage then commit", f.cfg.Xray.Failover, f.applies)
			}
			assertStillOnTunnel(t, f.cfg)
		})
	}
}

func TestMutation_GateAfterWaitDoesNotPublishOrAnnounce(t *testing.T) {
	for _, wait := range []string{"death probe", "restore probe", "platform with tunnel", "platform without tunnel", "subscription fetch", "readiness"} {
		t.Run(wait, func(t *testing.T) {
			cfg := mutationConfig()
			if wait == "restore probe" || wait == "subscription fetch" || wait == "readiness" {
				commitMutationFailover(t, cfg)
			}
			before, err := cloneCfg(cfg)
			if err != nil {
				t.Fatal(err)
			}
			f := &fake{cfg: cfg, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
			w := runningWatch(f.watch())
			w.failSince = f.now.Add(-3 * time.Minute)
			var incompatible atomic.Bool
			w.CanMutate = mutationGate(&incompatible)
			waits, saves, generations, restarts := 0, 0, 0, 0
			loseCompatibility := func() {
				waits++
				incompatible.Store(true)
			}
			save := w.SaveSubscription
			w.SaveSubscription = func(s vpnconfig.Subscription) error { saves++; return save(s) }
			w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				generations++
				return f.generateAll(s, guard)
			}
			w.RestartXray = func() error { restarts++; return nil }
			w.AfterRestart = func(time.Duration) {}
			switch wait {
			case "death probe":
				w.Probe = func(context.Context, int) error { loseCompatibility(); return errProbe }
			case "restore probe":
				w.Probe = func(context.Context, int) error { loseCompatibility(); return nil }
			case "platform with tunnel", "platform without tunnel":
				w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
					loseCompatibility()
					p := vpnconfig.PlatformInfo{}
					if wait == "platform with tunnel" {
						p.Tunnels = []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}
					}
					return p, nil
				}
			case "subscription fetch":
				w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
					loseCompatibility()
					return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, Security: "tls"}}, nil
				}
			case "readiness":
				w.Probe = func(context.Context, int) error { return nil }
				w.TPROXYReady = func() bool { loseCompatibility(); return true }
			}
			w.Tick(context.Background())
			if waits == 0 {
				t.Fatal("the injected wait was not reached")
			}
			if saves != 0 || generations != 0 || restarts != 0 || f.applies != 0 || len(f.notes) != 0 {
				t.Errorf("saves %d, generations %d, restarts %d, applies %d, notes %v after gate loss", saves, generations, restarts, f.applies, f.notes)
			}
			if !reflect.DeepEqual(f.cfg, before) {
				t.Error("the configuration changed after a wait lost compatibility")
			}
		})
	}
}

func TestMutation_SwitcherEndsWhenRestartLosesCompatibility(t *testing.T) {
	r := newReturnRig(returnServers())
	r.f.cfg = mutationConfig()
	r.f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Madrid", Address: "madrid.example", Port: 443, Seq: 7}
	r.f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	r.up[osloIP] = true
	var incompatible atomic.Bool
	r.w.CanMutate = mutationGate(&incompatible)
	restart := r.w.RestartXray
	r.w.RestartXray = func() error {
		err := restart()
		incompatible.Store(true)
		return err
	}
	candidateProbes := 0
	r.onProbe = func() { candidateProbes++ }
	r.tick()
	if want := []string{"Oslo@203.0.113.10", "restart"}; !reflect.DeepEqual(r.events, want) {
		t.Errorf("events %v, want %v and no rollback after compatibility loss", r.events, want)
	}
	if candidateProbes != 0 || r.w.returnFails != 0 || len(r.f.notes) != 0 {
		t.Errorf("candidate probes %d, return failures %d, notes %v; a lost gate must end the switcher", candidateProbes, r.w.returnFails, r.f.notes)
	}
}

func TestMutation_GateLossCancelsInflightFetch(t *testing.T) {
	oldPoll := stopPoll
	stopPoll = time.Millisecond
	t.Cleanup(func() { stopPoll = oldPoll })
	cfg := mutationConfig()
	commitMutationFailover(t, cfg)
	f := &fake{cfg: cfg, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	var incompatible atomic.Bool
	w.CanMutate = mutationGate(&incompatible)
	fetches, saves, generations, restarts := 0, 0, 0, 0
	save := w.SaveSubscription
	w.SaveSubscription = func(sub vpnconfig.Subscription) error { saves++; return save(sub) }
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		generations++
		return f.generateAll(s, guard)
	}
	w.RestartXray = func() error { restarts++; return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Fetch = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		fetches++
		if fetches == 1 {
			incompatible.Store(true)
			select {
			case <-ctx.Done():
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Errorf("fetch context error %v, want cancellation by the mutation poll", ctx.Err())
				}
			case <-time.After(time.Second):
				t.Error("fetch continued after compatibility was lost with no Stopped callback")
			}
		}
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, Security: "tls"}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w.Tick(ctx)
	if ctx.Err() != nil {
		t.Errorf("the parent deadline, not gate loss, ended Tick: %v", ctx.Err())
	}
	if fetches != 1 || saves != 0 || generations != 0 || restarts != 0 || f.applies != 0 || len(f.notes) != 0 {
		t.Errorf("fetches %d, saves %d, generations %d, restarts %d, applies %d, notes %v after gate loss", fetches, saves, generations, restarts, f.applies, f.notes)
	}
	incompatible.Store(false)
	w.Tick(context.Background())
	if fetches != 2 {
		t.Errorf("fetches %d; an incompatible aborted wave must not spend its import window", fetches)
	}
}

// The tick's own end cancels its context while a poll may be asking for
// permission; a refusal that comes back after that is no refusal of the tick,
// and the outbound's failure confirmation keeps counting.
func TestMutation_PollRefusedAtTheTickEndKeepsTheFailureClock(t *testing.T) {
	defer func(poll time.Duration) { stopPoll = poll }(stopPoll)
	stopPoll = time.Millisecond
	f := &fake{cfg: mutationConfig(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	start := f.now.Add(-time.Minute)
	w.failSince = start
	var block, refused atomic.Bool
	var tick context.Context
	inflight := make(chan struct{})
	w.CanMutate = func() error {
		if !block.CompareAndSwap(true, false) {
			return nil
		}
		close(inflight)
		<-tick.Done()
		refused.Store(true)
		return watchcompat.ErrIncompatible
	}
	w.Probe = func(ctx context.Context, _ int) error {
		tick = ctx
		block.Store(true)
		select {
		case <-inflight:
		case <-time.After(5 * time.Second):
			t.Error("no poll asked for permission while the probe waited")
		}
		return errProbe
	}

	w.Tick(context.Background())

	if !refused.Load() {
		t.Fatal("the poll's permission check did not end with the tick")
	}
	if !w.failSince.Equal(start) {
		t.Fatalf("failSince %v, want %v: the tick's own end restarted the failure confirmation", w.failSince, start)
	}
}

func TestMutation_IncompatibleAndCancellationEndWalkAndReturn(t *testing.T) {
	for _, refusal := range []error{watchcompat.ErrIncompatible, context.Canceled, context.DeadlineExceeded} {
		for _, route := range []string{"walk", "preferred return"} {
			t.Run(route+"/"+refusal.Error(), func(t *testing.T) {
				cfg := mutationConfig()
				cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Madrid", Address: "madrid.example", Port: 443, Seq: 7}
				cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
				f := &fake{cfg: cfg, now: time.Unix(1_700_000_000, 0)}
				w := runningWatch(f.watch())
				w.CanMutate = func() error { return nil }
				servers := []vpnconfig.Server{
					{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10", "203.0.113.11"}},
					{Name: "Madrid", Address: "madrid.example", Port: 443, IPs: []string{"203.0.113.20"}},
				}
				subs := []vpnconfig.Subscription{{Servers: servers}}
				w.LoadSubscriptions = func() ([]vpnconfig.Subscription, error) { return cloneSubs(subs), nil }
				generations, restarts := 0, 0
				w.Generate = func(vpnconfig.Server, func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
					generations++
					return false, 7, fmt.Errorf("generation refused: %w", refusal)
				}
				w.RestartXray = func() error { restarts++; return nil }
				w.AfterRestart = func(time.Duration) {}
				if route == "walk" {
					w.walk(context.Background(), cfg, subs)
				} else {
					w.tryReturn(context.Background(), cfg, subs, servers, endpoint.PerAddress(servers[:1]))
				}
				if generations != 1 || restarts != 0 || f.applies != 0 || len(f.notes) != 0 {
					t.Errorf("generations %d, restarts %d, applies %d, notes %v; a terminal refusal must end the attempt", generations, restarts, f.applies, f.notes)
				}
				if a := cfg.Xray.ActiveServer; a == nil || a.Name != "Madrid" || a.Seq != 7 {
					t.Errorf("current selection %+v changed after a terminal refusal", a)
				}
				if w.returnFails != 0 || w.importRetry != 0 {
					t.Errorf("return failures %d, import retry %v; incompatibility/cancellation is not server failure", w.returnFails, w.importRetry)
				}
			})
		}
	}
}
