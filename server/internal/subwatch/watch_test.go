package subwatch

import (
	"context"
	"errors"
	"reflect"
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
}

func (f *fake) watch() *Watch {
	return &Watch{
		LoadVPN:      func() (*vpnconfig.VPNDirectorConfig, error) { return f.cfg, nil },
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
	if f.applies != 1 {
		t.Fatalf("applies %d", f.applies)
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
	w.Tick(context.Background())
	if len(f.notes) != 1 {
		t.Fatalf("duplicate notify %v", f.notes)
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

func TestTick_FailoverPresentSkipsHealthyProbeRestore(t *testing.T) {
	cfg := baseCfg()
	cfg.Xray.Failover = &vpnconfig.XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	cfg.Xray.Clients = []string{"192.168.1.9"}
	f := &fake{cfg: cfg, probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	f.watch().Tick(context.Background())
	if f.cfg.Xray.Failover == nil {
		t.Fatal("live SOCKS must not restore")
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
	w.Generate = func(s vpnconfig.Server) (bool, error) { return true, nil }
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error { return nil }
	return w
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

func TestTick_RestoreApplyFailureKeepsFailoverThenRetries(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:      time.Unix(1_700_000_000, 0),
		applyErr: errApply,
	}
	w := liveImportWatch(f)
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
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("later Tick with Apply succeeding must restore")
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
	assertStillOnTunnel(t, f.cfg)
	if f.applies != 1 {
		t.Fatalf("applies %d, want 1", f.applies)
	}
	if len(f.notes) != 0 {
		t.Fatalf("must not notify moved until Apply succeeds: %v", f.notes)
	}

	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if f.applies != 2 {
		t.Fatalf("later Tick must retry Apply, got %d", f.applies)
	}
	if len(f.notes) != 0 {
		t.Fatalf("still failing Apply: %v", f.notes)
	}

	f.applyErr = nil
	w.Tick(context.Background())
	assertStillOnTunnel(t, f.cfg)
	if f.applies != 3 {
		t.Fatalf("applies %d, want 3", f.applies)
	}
	if len(f.notes) != 1 || f.notes[0] != "Xray outbound is down; LAN clients moved to tunnel:ovpnc2" {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestTick_ImportSyncsXrayServers(t *testing.T) {
	f := &fake{
		cfg: failedOverCfg(),
		now: time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
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

func TestUniqueServerIPs(t *testing.T) {
	got := uniqueServerIPs([]vpnconfig.Server{
		{IPs: []string{"2.2.2.2", "", "1.1.1.1"}},
		{IPs: []string{"1.1.1.1", "3.3.3.3"}},
	})
	want := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	if uniqueServerIPs(nil) == nil {
		t.Fatal("empty result must be non-nil so JSON is []")
	}
}
