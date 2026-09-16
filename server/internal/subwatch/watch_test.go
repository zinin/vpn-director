package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var errProbe = errors.New("probe failed")
var errApply = errors.New("apply failed")

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type fake struct {
	mu       sync.Mutex
	cfg      *vpnconfig.VPNDirectorConfig
	plat     vpnconfig.PlatformInfo
	probeErr error
	applyErr error
	applies  int
	notes    []string
	now      time.Time
	picked   bool // Generate has written a walked outbound (liveImportWatch)
}

// cloneCfg hands out what production's LoadVPN does: a fresh parse, which a
// later UpdateVPN of f.cfg cannot change under the Tick holding it.
func cloneCfg(cfg *vpnconfig.VPNDirectorConfig) (*vpnconfig.VPNDirectorConfig, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var out vpnconfig.VPNDirectorConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (f *fake) watch() *Watch {
	return &Watch{
		LoadVPN:      func() (*vpnconfig.VPNDirectorConfig, error) { return cloneCfg(f.cfg) },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return f.plat, nil },
		UpdateVPN: func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
			return fn(f.cfg)
		},
		Apply: func() error {
			f.applies++
			return f.applyErr
		},
		Probe:  func(context.Context, int) error { return f.probeErr },
		Notify: func(msg string) { f.notes = append(f.notes, msg) },
		Now:    func() time.Time { return f.now },
	}
}

func baseCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		PausedClients: []string{"192.168.1.9"},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"ovpnc2": {Clients: []string{"192.168.1.3"}},
		}},
		Xray: vpnconfig.XrayConfig{
			Clients:         []string{"192.168.1.8", "192.168.1.9"},
			SubscriptionURL: "https://cdn.example/s/token",
		},
	}
}

// tickUntilDead probes from f.now until elapsed >= DeadAfter, then one more Tick
// (first fail at t=0, move/notify at t=3m).
func tickUntilDead(w *Watch, f *fake) {
	start := f.now
	for f.now.Sub(start) < DeadAfter {
		w.Tick(context.Background())
		f.now = f.now.Add(ProbeInterval)
	}
	w.Tick(context.Background())
}

// tickFor ticks every ProbeInterval from f.now through d later.
func tickFor(w *Watch, f *fake, d time.Duration) {
	end := f.now.Add(d)
	for !f.now.After(end) {
		w.Tick(context.Background())
		f.now = f.now.Add(ProbeInterval)
	}
}

// runningWatch marks w as a watch that has been running in this process, so its
// first Tick does not re-apply the failover record as one left by an earlier process.
func runningWatch(w *Watch) *Watch {
	w.reconciled = true
	return w
}

func TestTick_NotArmedDoesNothing(t *testing.T) {
	f := &fake{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.1.8"}}}, now: time.Unix(0, 0)}
	f.watch().Tick(context.Background())
	if f.applies != 0 {
		t.Fatal("unarmed")
	}
}

func TestTick_ThreeMinutesMovesClients(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	tickUntilDead(w, f)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("192.168.1.8 not on tunnel")
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("still in xray.clients")
	}
	if contains(f.cfg.Xray.Failover.Clients, "192.168.1.9") {
		t.Fatal("paused client in snapshot")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("foreign client dropped")
	}
	if f.applies != 2 {
		t.Fatalf("applies %d, want tunnel rules staged then Xray membership dropped", f.applies)
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
	w.Tick(context.Background())
	if len(f.notes) != 1 {
		t.Fatalf("duplicate notify %v", f.notes)
	}
}

func TestTick_MoveKeepsXrayMembershipUntilTunnelApply(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	var xrayAtApply [][]string
	w := f.watch()
	apply := w.Apply
	w.Apply = func() error {
		xrayAtApply = append(xrayAtApply, append([]string(nil), f.cfg.Xray.Clients...))
		return apply()
	}
	tickUntilDead(w, f)
	if len(xrayAtApply) != 2 {
		t.Fatalf("applies %d, want 2", len(xrayAtApply))
	}
	if !contains(xrayAtApply[0], "192.168.1.8") {
		t.Fatalf("first apply xray %v; fallback rules must be installed while TPROXY still matches", xrayAtApply[0])
	}
	if contains(xrayAtApply[1], "192.168.1.8") {
		t.Fatalf("second apply xray %v; membership drops only after the tunnel apply", xrayAtApply[1])
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("still in xray.clients after the move")
	}
}

func TestTick_DoesNotCommitIfFallbackTunnelWasNotApplied(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.FallbackReady = func(string) bool { return false }
	fetches := 0
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}
	tickUntilDead(w, f)
	assertStagedOnTunnel(t, f.cfg)
	for _, n := range f.notes {
		if strings.HasPrefix(n, "Xray outbound is down; LAN clients moved") {
			t.Fatalf("must not report moved: %v", f.notes)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; a staged apply that is not yet ready must still refresh the subscription", fetches)
	}

	w.Tick(context.Background())
	assertStagedOnTunnel(t, f.cfg)

	w.FallbackReady = func(string) bool { return true }
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	found := false
	for _, n := range f.notes {
		if n == "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_StagedFailoverAbandonsWhenXrayRecovers(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.FallbackReady = func(string) bool { return false }
	tickUntilDead(w, f)
	assertStagedOnTunnel(t, f.cfg)

	f.probeErr = nil
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("healthy Xray must drop the staged failover")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("client must stay on Xray")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("staged tunnel assignment must be rolled back")
	}
}

func TestTick_AbandonStagedApplyFailureDoesNotNotifyRestored(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.FallbackReady = func(string) bool { return false }
	tickUntilDead(w, f)
	assertStagedOnTunnel(t, f.cfg)

	f.probeErr = nil
	f.applyErr = errApply
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("JSON must already be restored")
	}
	f.applyErr = nil
	w.Tick(context.Background())
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("staged abandon must not send restored: %v", f.notes)
		}
	}
}

func TestTick_StagedRestoreApplyFailureKeepsXrayMembership(t *testing.T) {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg:      cfg,
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	w := runningWatch(liveImportWatch(f))
	w.FallbackReady = func(string) bool { return false }
	w.Tick(context.Background())
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("staged restore-Apply failure must not drop Xray membership")
	}
	if f.cfg.Xray.Failover != nil && !vpnconfig.FailoverStaged(f.cfg) {
		t.Fatal("write-back must not commit a staged failover")
	}
}

func TestTick_DoesNotReapplyWhileWaitingForFailoverReady(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.FallbackReady = func(string) bool { return false }
	tickUntilDead(w, f)
	n := f.applies
	if n == 0 {
		t.Fatal("the move apply")
	}
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != n {
		t.Fatalf("applies %d after %d; waiting for failover_ready must not re-apply", f.applies, n)
	}
}

func TestTick_StagedPickNotifiesPickedNotRestored(t *testing.T) {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg:  cfg,
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	w := runningWatch(liveImportWatch(f))
	w.FallbackReady = func(string) bool { return false }
	w.Tick(context.Background())
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("staged pick must not send restored: %v", f.notes)
		}
	}
	found := false
	for _, n := range f.notes {
		if n == "Subscription refreshed; selected server Oslo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestServerForDial_UsesResolvedIPKeepsHostnameSNI(t *testing.T) {
	s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "tls"})
	if s.Address != "203.0.113.50" {
		t.Fatalf("address %q", s.Address)
	}
	if s.SNI != "oslo.example" {
		t.Fatalf("sni %q", s.SNI)
	}
	s = ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "tls", SNI: "cdn.example"})
	if s.SNI != "cdn.example" {
		t.Fatalf("explicit sni %q", s.SNI)
	}
}

func TestTick_NoTunnelStillNotifiesOnce(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	tickUntilDead(w, f)
	if f.cfg.Xray.Failover != nil {
		t.Fatal("moved without a tunnel")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("clients should stay")
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; no Tunnel Director fallback" {
		t.Fatalf("notes %v", f.notes)
	}
	w.Tick(context.Background())
	if len(f.notes) != 1 {
		t.Fatalf("duplicate notify %v", f.notes)
	}
}

func TestTick_NoTunnelDoesNotReloadPlatformEveryTick(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	platforms := 0
	w := f.watch()
	load := w.LoadPlatform
	w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
		platforms++
		return load()
	}
	tickUntilDead(w, f)
	if platforms == 0 {
		t.Fatal("the first no-tunnel tick must read the platform")
	}
	n := platforms
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if platforms != n {
		t.Fatalf("platform %d after %d; must not spawn platform every tick after no-tunnel", platforms, n)
	}
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if platforms == n {
		t.Fatal("must re-check for a tunnel on the import cadence")
	}
}

func TestTick_OneFailureDoesNotMove(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	f.watch().Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("too eager")
	}
}

func TestTick_SuccessResetsTimer(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.Tick(context.Background())
	f.now = f.now.Add(30 * time.Second)
	f.probeErr = nil
	w.Tick(context.Background())
	f.probeErr = errProbe
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(30 * time.Second)
		w.Tick(context.Background())
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("timer must reset after a success")
	}
}

func TestTick_UnarmedTickResetsTimer(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.Tick(context.Background())

	f.cfg.Xray.SubscriptionURL = ""
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())

	f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/token"
	f.now = f.now.Add(10 * time.Minute)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the first failed probe after re-arming must not fail over")
	}
	if f.applies != 0 {
		t.Fatalf("applies %d", f.applies)
	}
}

func TestTick_TunnelGoneAtMoveSkipsApplyAndNotify(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	// The tick's snapshot still lists ovpnc2; the locked update no longer does.
	w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		delete(f.cfg.TunnelDirector.Tunnels, "ovpnc2")
		return fn(f.cfg)
	}
	tickUntilDead(w, f)
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("clients must stay on Xray")
	}
	if f.applies != 0 {
		t.Fatalf("applies %d, want 0", f.applies)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_NoTunnelPickNotifiesSelectedServer(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	generated := false
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generated = true
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if generated {
			return nil
		}
		return f.probeErr
	}
	tickUntilDead(w, f)
	want := []string{
		"Xray outbound is down; no Tunnel Director fallback",
		"Subscription refreshed; selected server Oslo",
	}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_NoTunnelPickApplyFailureLeavesNothingPending(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		applyErr: errApply,
		now:      time.Unix(1_700_000_000, 0),
	}
	generated, probes := false, 0
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generated = true
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	// Oslo answers once generated: the walk's probe and every later one succeed.
	w.Probe = func(context.Context, int) error {
		probes++
		if generated {
			return nil
		}
		return f.probeErr
	}
	tickUntilDead(w, f)
	if f.applies != 1 {
		t.Fatalf("applies %d, want the failed pick Apply", f.applies)
	}
	if w.pendingApply {
		t.Fatal("no failover was restored, so nothing may stay pending")
	}

	probesBefore := probes
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 1 {
		t.Fatalf("applies %d; the next Tick must not re-apply", f.applies)
	}
	if probes != probesBefore+1 {
		t.Fatalf("probes %d, want %d; the health probe must run", probes, probesBefore+1)
	}
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("notes %v; nothing left Xray", f.notes)
		}
	}
}

func TestTick_RestoreKeepsFallbackWhenTPROXYNotReady(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return false }
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil {
		t.Fatal("must keep failover while TPROXY is not intercepting LAN")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("fallback membership")
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("committed write-back must take the client off Xray")
	}
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("must not announce restore: %v", f.notes)
		}
	}
}

func TestTick_WalkAbandonsWhenANewerServerWasSelected(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		if s.Name == "Oslo" {
			f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
		}
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) == 0 {
			return errProbe
		}
		if generated[len(generated)-1] == "Oslo" {
			return errProbe
		}
		return nil
	}
	w.Tick(context.Background())
	if len(generated) != 1 || generated[0] != "Oslo" {
		t.Fatalf("generate %v; must not overwrite a newer manual selection", generated)
	}
	if f.cfg.Xray.ActiveServer == nil || f.cfg.Xray.ActiveServer.Name != "Manual" {
		t.Fatalf("active %+v", f.cfg.Xray.ActiveServer)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("abandoned walk must not restore")
	}
}

func TestTick_CommittedFailoverRestoresWhenSOCKSHealthy(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	w := runningWatch(f.watch())
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("live SOCKS must restore without a subscription fetch")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("client not restored")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel")
	}
	if fetches != 0 {
		t.Fatalf("fetches %d; a healthy outbound must not wait on the subscription", fetches)
	}
	found := false
	for _, n := range f.notes {
		if n == "LAN clients back on Xray; server Oslo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestPickOrder_SameNameFirst(t *testing.T) {
	in := []vpnconfig.Server{
		{Name: "A", Address: "1.example"},
		{Name: "Oslo", Address: "2.example"},
		{Name: "B", Address: "3.example"},
	}
	got := pickOrder(in, "Oslo")
	if got[0].Name != "Oslo" || got[1].Name != "A" || got[2].Name != "B" {
		t.Fatalf("%v", got)
	}
	got = pickOrder(in, "missing")
	if got[0].Name != "A" {
		t.Fatal("keep list order")
	}
}

func TestTick_GenerateKeepsSubscriptionHostname(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	w := runningWatch(liveImportWatch(f))
	var got vpnconfig.Server
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		got = s
		f.picked = true
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, nil
	}
	w.Tick(context.Background())
	if got.Address != "new.example" {
		t.Fatalf("Generate address %q, want the subscription hostname", got.Address)
	}
	if f.cfg.Xray.ActiveServer == nil || f.cfg.Xray.ActiveServer.Address != "new.example" {
		t.Fatalf("active_server %+v, want hostname new.example", f.cfg.Xray.ActiveServer)
	}
}

func TestTick_ImportAndRestoreOnLiveServer(t *testing.T) {
	f := &fake{
		cfg:  baseCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	f.probeErr = errProbe
	saved := 0
	generated := []string{}
	liveAfter := ""
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { saved++; return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443},
			{Name: "Backup", Address: "b.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		generated = append(generated, s.Name)
		liveAfter = s.Name
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if liveAfter == "Oslo" && f.cfg.Xray.Failover != nil {
			return nil
		}
		return f.probeErr
	}
	start := f.now
	for f.now.Sub(start) <= DeadAfter {
		w.Tick(context.Background())
		f.now = f.now.Add(ProbeInterval)
	}
	w.Tick(context.Background()) // the tick that crosses DeadAfter
	if saved != 1 {
		t.Fatalf("saved %d", saved)
	}
	if len(generated) < 1 || generated[0] != "Oslo" {
		t.Fatalf("generate %v", generated)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("should have restored")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("client not restored")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("foreign client")
	}
	if !w.lastImport.IsZero() {
		t.Fatal("lastImport must reset on successful restore so a new death does not wait 5m")
	}
}

func TestTick_SameNameDeadWalksList(t *testing.T) {
	f := &fake{
		cfg:  baseCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	f.probeErr = errProbe
	generated := []string{}
	liveAfter := ""
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443},
			{Name: "Backup", Address: "b.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		generated = append(generated, s.Name)
		liveAfter = s.Name
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if liveAfter == "Backup" {
			return nil
		}
		return f.probeErr
	}
	tickUntilDead(w, f)
	if len(generated) != 2 || generated[0] != "Oslo" || generated[1] != "Backup" {
		t.Fatalf("generate %v", generated)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("should have restored")
	}
	want := "LAN clients back on Xray; server Backup"
	found := false
	for _, n := range f.notes {
		if n == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_FailedFetchDoesNotSave(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	saved := 0
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { saved++; return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return nil, errors.New("cdn down")
	}
	tickUntilDead(w, f)
	if saved != 0 {
		t.Fatalf("saved %d", saved)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("should stay failed over")
	}
	want := "Subscription refresh failed; still on tunnel:ovpnc2"
	if len(f.notes) == 0 || f.notes[len(f.notes)-1] != want {
		t.Fatalf("notes %v", f.notes)
	}
	w.Tick(context.Background())
	n := 0
	for _, msg := range f.notes {
		if msg == want {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate notify %v", f.notes)
	}
}

func TestTick_NoTunnelNoLiveNotifiesOncePerChannel(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches := 0
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	tickFor(w, f, 60*time.Minute)
	if fetches < 3 {
		t.Fatalf("fetches %d; the outage must span several import waves", fetches)
	}
	want := []string{msgNoTunnel, msgNoLive}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_NoTunnelRefreshFailedNotifiesOncePerChannel(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches := 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}
	tickFor(w, f, 30*time.Minute)
	if fetches < 3 {
		t.Fatalf("fetches %d; the outage must span several import waves", fetches)
	}
	want := []string{msgNoTunnel, msgRefreshFailed}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_TunnelImportOutcomesNotifyOnceEach(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches := 0
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		if fetches == 1 {
			return nil, errors.New("cdn down")
		}
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	tickFor(w, f, 30*time.Minute)
	if fetches < 3 {
		t.Fatalf("fetches %d; the outage must span several import waves", fetches)
	}
	want := []string{
		"Xray outbound is down; LAN clients moved to tunnel:ovpnc2",
		"Subscription refresh failed; still on tunnel:ovpnc2",
		"No live server in the subscription; still on tunnel:ovpnc2",
	}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_NewDeathAfterRestoreNotifiesMovedAgain(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	generated, liveOnce := false, true
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generated = true
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	// The first walk finds Oslo live; every probe after the restore fails, so
	// no healthy probe separates the two episodes.
	w.Probe = func(context.Context, int) error {
		if generated && liveOnce {
			liveOnce = false
			return nil
		}
		return f.probeErr
	}
	tickFor(w, f, 30*time.Minute)
	want := []string{
		"Xray outbound is down; LAN clients moved to tunnel:ovpnc2",
		"LAN clients back on Xray; server Oslo",
		"Xray outbound is down; LAN clients moved to tunnel:ovpnc2",
		"No live server in the subscription; still on tunnel:ovpnc2",
	}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_NewEpisodeAfterRestoreNotifiesRestoredAgain(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	justGenerated := false
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		justGenerated = true
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	// Every walk finds Oslo live and every health probe fails, so no healthy
	// probe separates the two episodes.
	w.Probe = func(context.Context, int) error {
		if justGenerated {
			justGenerated = false
			return nil
		}
		return f.probeErr
	}
	// Episode 1 dies at 3m and restores; episode 2 fails from 3m30s, dies at
	// 6m30s and restores; stopping at 7m leaves no room for a third.
	tickFor(w, f, 7*time.Minute)
	want := []string{
		"Xray outbound is down; LAN clients moved to tunnel:ovpnc2",
		"LAN clients back on Xray; server Oslo",
		"Xray outbound is down; LAN clients moved to tunnel:ovpnc2",
		"LAN clients back on Xray; server Oslo",
	}
	if !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

func TestTick_ImportRetryWaitsFiveMinutes(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches := 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}
	tickUntilDead(w, f)
	if fetches != 1 {
		t.Fatalf("first dead tick fetches %d", fetches)
	}
	f.now = f.now.Add(4 * time.Minute)
	w.Tick(context.Background())
	if fetches != 1 {
		t.Fatalf("after 4m fetches %d", fetches)
	}
	f.now = f.now.Add(time.Minute)
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("after 5m fetches %d", fetches)
	}
}

func failedOverCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.Failover = &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	cfg.Xray.Clients = []string{"192.168.1.9"}
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	tun.Clients = []string{"192.168.1.3", "192.168.1.8"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	return cfg
}

func liveImportWatch(f *fake) *Watch {
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443, IPs: []string{"203.0.113.10", "203.0.113.11"}},
			{Name: "Backup", Address: "b.example", Port: 443, IPs: []string{"203.0.113.11", "198.51.100.8", ""}},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		f.picked = true
		return true, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	// Health probe of the current outbound fails until Generate has picked a
	// server; the walk probe then succeeds. A nil Probe restored immediately
	// and skipped Fetch.
	w.Probe = func(context.Context, int) error {
		if f.picked {
			return nil
		}
		return errProbe
	}
	return w
}

// recordingWalkWatch imports servers on every wave. Its Generate records the
// server as xray.active_server, as production does, when generates allows it;
// events lists every Generate by server name and every restart as "restart".
func recordingWalkWatch(f *fake, servers []vpnconfig.Server, generates func(vpnconfig.Server) bool, events *[]string) *Watch {
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) { return servers, nil }
	w.Generate = func(s vpnconfig.Server) (bool, error) {
		*events = append(*events, s.Name)
		if !generates(s) {
			return false, errors.New("rejected")
		}
		f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: s.Name}
		return true, nil
	}
	w.RestartXray = func() error {
		*events = append(*events, "restart")
		return nil
	}
	w.AfterRestart = func(time.Duration) {}
	return w
}

func allGenerate(vpnconfig.Server) bool { return true }

func TestTick_NoLiveWavesBackOffImportRetry(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	start := f.now
	w.Tick(context.Background())
	if fetches != 1 {
		t.Fatalf("fetches %d after the first wave", fetches)
	}
	// Every wave finds only dead servers: the next one waits 10m, 20m, then 30m.
	for _, at := range []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute, 90 * time.Minute} {
		want := fetches
		f.now = start.Add(at - time.Second)
		w.Tick(context.Background())
		if fetches != want {
			t.Fatalf("fetch at %v, a second before the backed-off wave", at-time.Second)
		}
		f.now = start.Add(at)
		w.Tick(context.Background())
		if fetches != want+1 {
			t.Fatalf("no fetch at %v", at)
		}
	}
}

func TestTick_RestoreResetsImportBackoff(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches := 0
	var fetchErr error
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		if fetchErr != nil {
			return nil, fetchErr
		}
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background()) // dead servers: the next wave backs off to 10m
	f.probeErr = nil
	f.now = f.now.Add(10 * time.Minute)
	w.Tick(context.Background()) // current outbound is live; restore without Fetch
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the second wave must restore")
	}

	// A new episode with no healthy probe in between; its first download fails.
	f.probeErr = errProbe
	fetchErr = errors.New("cdn down")
	f.now = f.now.Add(ProbeInterval)
	tickUntilDead(w, f)
	if fetches != 2 {
		t.Fatalf("fetches %d, want the new episode's first wave", fetches)
	}
	dead := f.now
	f.now = dead.Add(ImportRetry - time.Second)
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetch %v after a failed download", ImportRetry-time.Second)
	}
	f.now = dead.Add(ImportRetry)
	w.Tick(context.Background())
	if fetches != 3 {
		t.Fatalf("fetches %d; after a restore the retry is back to %v", fetches, ImportRetry)
	}
}

func TestTick_FailedFetchKeepsFiveMinuteRetry(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	w := runningWatch(f.watch())
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}

	start := f.now
	w.Tick(context.Background())
	for i := 1; i <= 3; i++ {
		at := time.Duration(i) * ImportRetry
		f.now = start.Add(at - time.Second)
		w.Tick(context.Background())
		if fetches != i {
			t.Fatalf("fetches %d at %v, want %d", fetches, at-time.Second, i)
		}
		f.now = start.Add(at)
		w.Tick(context.Background())
		if fetches != i+1 {
			t.Fatalf("fetches %d at %v, want %d", fetches, at, i+1)
		}
	}
}

func TestTick_FailedDownloadAfterNoLiveWaveResetsToFiveMinutes(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	fetchErr := error(nil)
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		if fetchErr != nil {
			return nil, fetchErr
		}
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background()) // all-dead walk: next wave waits 10m
	if fetches != 1 {
		t.Fatalf("fetches %d after the dead wave", fetches)
	}

	fetchErr = errors.New("cdn down")
	f.now = f.now.Add(10 * time.Minute)
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d; the backed-off wave must run", fetches)
	}

	failedAt := f.now
	f.now = failedAt.Add(ImportRetry - time.Second)
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d at 5m-1s; a failed download must wait ImportRetry", fetches)
	}
	f.now = failedAt.Add(ImportRetry)
	w.Tick(context.Background())
	if fetches != 3 {
		t.Fatalf("fetches %d; a failed download must retry after %v, not the 10m all-dead backoff", fetches, ImportRetry)
	}
}

func TestTick_NoLiveWaveReturnsToPreferredServer(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	servers := []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443},
		{Name: "Paris", Address: "paris.example", Port: 443},
		{Name: "SaoPaulo", Address: "saopaulo.example", Port: 443},
	}
	var events []string
	w := runningWatch(recordingWalkWatch(f, servers, allGenerate, &events))

	w.Tick(context.Background())
	want := []string{"Oslo", "restart", "Paris", "restart", "SaoPaulo", "restart", "Oslo", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("dead wave events %v, want %v", events, want)
	}
	if f.cfg.Xray.ActiveServer == nil || f.cfg.Xray.ActiveServer.Name != "Oslo" {
		t.Fatalf("active server %+v, want Oslo", f.cfg.Xray.ActiveServer)
	}

	events = nil
	w.Probe = func(context.Context, int) error {
		if len(events) > 0 {
			return nil
		}
		return errProbe
	}
	f.now = f.now.Add(2 * ImportRetry) // the dead wave backed the next one off to 10m
	w.Tick(context.Background())
	if !reflect.DeepEqual(events, []string{"Oslo", "restart"}) {
		t.Fatalf("live wave events %v, want Oslo tried first", events)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("live wave must restore")
	}
	if n := len(f.notes); n == 0 || f.notes[n-1] != "LAN clients back on Xray; server Oslo" {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_NoLiveWaveWithoutPreferredInListGeneratesNothingExtra(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	servers := []vpnconfig.Server{
		{Name: "Paris", Address: "paris.example", Port: 443},
		{Name: "SaoPaulo", Address: "saopaulo.example", Port: 443},
	}
	var events []string
	w := runningWatch(recordingWalkWatch(f, servers, allGenerate, &events))

	w.Tick(context.Background())
	want := []string{"Paris", "restart", "SaoPaulo", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}

func TestTick_NoLiveWaveOnlyPreferredGeneratedGeneratesNothingExtra(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	servers := []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example", Port: 443},
		{Name: "Paris", Address: "paris.example", Port: 443},
		{Name: "SaoPaulo", Address: "saopaulo.example", Port: 443},
	}
	var events []string
	w := runningWatch(recordingWalkWatch(f, servers, func(s vpnconfig.Server) bool { return s.Name == "Oslo" }, &events))

	w.Tick(context.Background())
	want := []string{"Oslo", "restart", "Paris", "SaoPaulo"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}

func assertStillOnTunnel(t *testing.T, cfg *vpnconfig.VPNDirectorConfig) {
	t.Helper()
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v", cfg.Xray.Failover)
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("client must stay on the tunnel")
	}
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("JSON must not look restored")
	}
}

func assertStagedOnTunnel(t *testing.T, cfg *vpnconfig.VPNDirectorConfig) {
	t.Helper()
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v", cfg.Xray.Failover)
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("client must be on the tunnel")
	}
	if !contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("Xray membership must remain until the tunnel apply succeeds")
	}
}

func TestTick_RestoreApplyFailureKeepsLastImportWindow(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	fetches, generates, restarts := 0, 0, 0
	w := runningWatch(liveImportWatch(f))
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443, IPs: []string{"203.0.113.10"}},
		}, nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generates++
		f.picked = true
		return true, nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if fetches != 1 || generates != 1 || restarts != 1 {
		t.Fatalf("first tick fetches=%d generates=%d restarts=%d", fetches, generates, restarts)
	}
	if w.lastImport.IsZero() {
		t.Fatal("lastImport must stay set after failed restore-Apply")
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if fetches != 1 || generates != 1 || restarts != 1 {
		t.Fatalf("30s later must not re-import, fetches=%d generates=%d restarts=%d", fetches, generates, restarts)
	}
	if f.applies != 2 {
		t.Fatalf("applies %d, want 2 (the second Apply is the pending retry, not a restore-Apply)", f.applies)
	}
}

func TestTick_RestoreApplyFailureKeepsFailoverThenRetries(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if f.applies != 1 {
		t.Fatalf("applies %d, want 1", f.applies)
	}
	for _, n := range f.notes {
		if n == "LAN clients back on Xray; server Oslo" {
			t.Fatalf("must not notify restore before Apply succeeds: %v", f.notes)
		}
	}

	f.applyErr = nil
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("Tick after ImportRetry with Apply succeeding must restore")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("client not restored")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("foreign client")
	}
	found := false
	for _, n := range f.notes {
		if n == "LAN clients back on Xray; server Oslo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes %v", f.notes)
	}
	if !w.lastImport.IsZero() {
		t.Fatal("lastImport must reset on successful restore")
	}
}

func overlapFailedOverCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.Clients = []string{"192.168.1.8", "192.168.1.3"}
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	vpnconfig.MoveXrayClientsToTunnel(cfg, "ovpnc2")
	return cfg
}

func TestTick_RestoreKeepsOverlapOnFallbackTunnel(t *testing.T) {
	f := &fake{
		cfg:  overlapFailedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must restore")
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") || !contains(f.cfg.Xray.Clients, "192.168.1.3") {
		t.Fatalf("xray %v", f.cfg.Xray.Clients)
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap must stay on the tunnel")
	}
}

func TestTick_RestoreApplyFailurePreservesOverlapOnWriteBack(t *testing.T) {
	f := &fake{
		cfg:      overlapFailedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil {
		t.Fatal("write-back")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap after failed restore-Apply")
	}
	if !contains(f.cfg.Xray.Failover.Added, "192.168.1.8") {
		t.Fatalf("added %v", f.cfg.Xray.Failover.Added)
	}
	if contains(f.cfg.Xray.Failover.Added, "192.168.1.3") {
		t.Fatal("overlap is not added")
	}

	f.applyErr = nil
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client still on tunnel after retry")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap must stay on the tunnel after retry")
	}
}

func TestTick_RestoreApplyFailureDoesNotMoveUnrelatedXrayClients(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	// Added or resumed on Xray while the snapshot was already on the tunnel.
	f.cfg.Xray.Clients = append(f.cfg.Xray.Clients, "192.168.1.10")
	w := runningWatch(liveImportWatch(f))
	w.Tick(context.Background())

	assertStillOnTunnel(t, f.cfg)
	if !contains(f.cfg.Xray.Clients, "192.168.1.10") {
		t.Fatal("a client added on Xray during failover must stay on Xray when restore-Apply fails")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.10") {
		t.Fatal("write-back must not move the unrelated Xray client onto the tunnel")
	}
	if f.cfg.Xray.Failover != nil && contains(f.cfg.Xray.Failover.Clients, "192.168.1.10") {
		t.Fatalf("failover snapshot %v must not grow to include the unrelated client", f.cfg.Xray.Failover.Clients)
	}
}

func TestTick_RestoreApplyAndWriteBackFailureRetriesApplyBeforeProbe(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	probes := 0
	w := runningWatch(liveImportWatch(f))
	w.Probe = func(context.Context, int) error {
		probes++
		return nil
	}
	// Fail only the write-back: the update right after the one that restored.
	updates, restoredAt := 0, 0
	w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		updates++
		if restoredAt != 0 && updates == restoredAt+1 {
			return errors.New("config lock timeout")
		}
		if err := fn(f.cfg); err != nil {
			return err
		}
		if restoredAt == 0 && f.cfg.Xray.Failover == nil {
			restoredAt = updates
		}
		return nil
	}

	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v; the write-back was meant to fail", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("JSON must look restored")
	}
	if f.applies != 1 || probes != 1 {
		t.Fatalf("first tick applies=%d probes=%d", f.applies, probes)
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 2 {
		t.Fatalf("applies %d, want the pending Apply retried", f.applies)
	}
	if probes != 1 {
		t.Fatalf("probes %d; no probe while the restore is not applied", probes)
	}

	f.applyErr = nil
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 3 {
		t.Fatalf("applies %d, want the pending Apply", f.applies)
	}
	if n := len(f.notes); n == 0 || f.notes[n-1] != "LAN clients back on Xray; server Oslo" {
		t.Fatalf("notes %v", f.notes)
	}
	if probes != 2 {
		t.Fatalf("probes %d; the probe must run in the Tick whose Apply succeeded", probes)
	}
}

func TestTick_RestoreApplyFailureWithWriteBackRetriesApplyBeforeImport(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	var events []string
	w := runningWatch(liveImportWatch(f))
	apply, fetch := w.Apply, w.Fetch
	w.Apply = func() error {
		events = append(events, "apply")
		return apply()
	}
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		events = append(events, "fetch")
		return fetch(ctx, url)
	}

	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)

	events = nil
	f.applyErr = nil
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if want := []string{"apply"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v (healthy SOCKS restores without Fetch)", events, want)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must restore once Apply succeeds")
	}
}

func TestTick_MoveApplyFailureRetriesApply(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		applyErr: errApply,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	tickUntilDead(w, f)
	assertStagedOnTunnel(t, f.cfg)
	if f.applies != 1 {
		t.Fatalf("applies %d, want 1", f.applies)
	}
	if len(f.notes) != 0 {
		t.Fatalf("must not notify moved until Apply succeeds: %v", f.notes)
	}

	w.Tick(context.Background())
	assertStagedOnTunnel(t, f.cfg)
	if f.applies != 2 {
		t.Fatalf("later Tick must retry Apply, got %d", f.applies)
	}
	if len(f.notes) != 0 {
		t.Fatalf("still failing Apply: %v", f.notes)
	}

	f.applyErr = nil
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if f.applies != 4 {
		t.Fatalf("applies %d, want the pending tunnel apply then the Xray drop", f.applies)
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_FailedApplyRetrySkipsImport(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		applyErr: errApply,
		now:      time.Unix(1_700_000_000, 0),
	}
	fetches, saves, generates, restarts, settles := 0, 0, 0, 0, 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.SaveServers = func([]vpnconfig.Server) error {
		saves++
		return nil
	}
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generates++
		return true, nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.AfterRestart = func(time.Duration) { settles++ }

	tickUntilDead(w, f)
	if f.applies != 1 {
		t.Fatalf("applies %d, want the failed move-Apply", f.applies)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; a staged apply failure must still refresh the subscription", fetches)
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 2 {
		t.Fatalf("applies %d, want the failed retry", f.applies)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; ImportRetry has not elapsed", fetches)
	}
	assertStagedOnTunnel(t, f.cfg)
	if len(f.notes) != 1 || f.notes[0] != "No live server in the subscription; still on tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}

	f.applyErr = nil
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if n := len(f.notes); n == 0 || f.notes[n-1] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_FirstTickReappliesFailoverFromEarlierProcess(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}

	w.Tick(context.Background())
	if f.applies != 1 {
		t.Fatalf("applies %d, want the reconcile Apply", f.applies)
	}
	if len(f.notes) == 0 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; the import must run in the Tick whose Apply succeeded", fetches)
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 1 {
		t.Fatalf("applies %d; reconcile must run once per process", f.applies)
	}
}

func TestTick_CommittedFailoverApplyFailureStillImports(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), applyErr: errApply, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	fetches := 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}

	w.Tick(context.Background())
	if f.applies != 1 {
		t.Fatalf("applies %d, want the reconcile Apply", f.applies)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; a committed failover must still refresh the subscription", fetches)
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("must not look staged")
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != 2 {
		t.Fatalf("applies %d, want the retry", f.applies)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d; ImportRetry has not elapsed", fetches)
	}
}

func TestTick_ImportSyncsXrayServers(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "new.example", Port: 443, IPs: []string{"203.0.113.10", "203.0.113.11"}},
			{Name: "Backup", Address: "b.example", Port: 443, IPs: []string{"203.0.113.11", "198.51.100.8", ""}},
		}, nil
	}
	w.Tick(context.Background())
	want := []string{"198.51.100.8", "203.0.113.10", "203.0.113.11"}
	if !reflect.DeepEqual(f.cfg.Xray.Servers, want) {
		t.Fatalf("Xray.Servers %v, want %v", f.cfg.Xray.Servers, want)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("Generate is nil; must stay failed over")
	}
}

func TestTick_SyncXrayServersFailureStopsTheWave(t *testing.T) {
	f := &fake{
		cfg: failedOverCfg(),
		now: time.Unix(1_700_000_000, 0),
	}
	generates, restarts := 0, 0
	w := runningWatch(liveImportWatch(f))
	w.Generate = func(vpnconfig.Server) (bool, error) {
		generates++
		return true, nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.UpdateVPN = func(func(*vpnconfig.VPNDirectorConfig) error) error {
		return errors.New("config lock")
	}
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if generates != 0 || restarts != 0 {
		t.Fatalf("must not switch servers after xray.servers sync fails, generates=%d restarts=%d", generates, restarts)
	}
	if len(f.cfg.Xray.Servers) != 0 {
		t.Fatalf("Xray.Servers %v, want unchanged", f.cfg.Xray.Servers)
	}
	want := "Subscription refresh failed; still on tunnel:ovpnc2"
	if len(f.notes) != 1 || f.notes[0] != want {
		t.Fatalf("notes %v, want %q", f.notes, want)
	}
}
