package subwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var errProbe = errors.New("probe failed")

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
		Apply:  func() error { f.applies++; return nil },
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
