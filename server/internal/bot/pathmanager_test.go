package bot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// liveMap guards the per-path probe verdict: a ReportFailure reselect probes
// from its own goroutine while the test flips entries.
type liveMap struct {
	mu sync.Mutex
	m  map[string]bool
}

func (l *liveMap) set(key string, v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[key] = v
}

func (l *liveMap) get(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[key]
}

func testMgr(t *testing.T, live *liveMap, cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, socksUp bool) *PathManager {
	t.Helper()
	return NewPathManager(PathManagerConfig{
		Token: "TOKEN",
		LoadVPN: func() (*vpnconfig.VPNDirectorConfig, error) {
			if cfg == nil {
				return nil, errors.New("no config")
			}
			return cfg, nil
		},
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return plat, nil },
		Listening:    func(port int) bool { return socksUp },
		Probe: func(ctx context.Context, p Path) error {
			if live.get(p.String()) {
				return nil
			}
			return errors.New("dead")
		},
		APIBase:  "http://127.0.0.1:1",
		Interval: time.Hour,
	})
}

func tdCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"ovpnc2": {Clients: []string{"192.168.1.3"}},
			},
		},
	}
}

func tdPlat() vpnconfig.PlatformInfo {
	return vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
	}}
}

func TestPathManager_DirectWinsAndSwitchesBack(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": false, "socks": true, "tunnel:ovpnc2": true}}
	m := testMgr(t, live, tdCfg(), tdPlat(), true)
	m.SelectOnce(context.Background())
	if m.Current().String() != "socks" {
		t.Fatalf("want socks, got %s", m.Current())
	}
	live.set("direct", true)
	m.SelectOnce(context.Background())
	if m.Current().String() != "direct" {
		t.Fatalf("switch back: %s", m.Current())
	}
}

func TestPathManager_EqualsStick(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": false, "socks": true, "tunnel:ovpnc2": true}}
	m := testMgr(t, live, tdCfg(), tdPlat(), true)
	m.SelectOnce(context.Background())
	if m.Current().String() != "socks" {
		t.Fatalf("got %s", m.Current())
	}
	m.SelectOnce(context.Background())
	if m.Current().String() != "socks" {
		t.Fatalf("flapped to %s", m.Current())
	}
}

func TestPathManager_PlatformErrorSkipsTunnels(t *testing.T) {
	m := NewPathManager(PathManagerConfig{
		LoadVPN: func() (*vpnconfig.VPNDirectorConfig, error) { return tdCfg(), nil },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) {
			return vpnconfig.PlatformInfo{}, errors.New("rci down")
		},
		Listening: func(int) bool { return false },
		Probe: func(ctx context.Context, p Path) error {
			if p.Kind == netpath.KindTunnel {
				t.Fatal("must not probe tunnels when platform fails")
			}
			return errors.New("dead")
		},
	})
	m.SelectOnce(context.Background())
	if m.Current().String() != "none" {
		t.Fatalf("got %s", m.Current())
	}
}

func TestPathManager_ReportFailureNoopIfStale(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": true}}
	m := testMgr(t, live, tdCfg(), tdPlat(), false)
	m.SelectOnce(context.Background())
	if m.Current().String() != "direct" {
		t.Fatal(m.Current())
	}
	m.ReportFailure(Path{Kind: netpath.KindTunnel, ID: "ovpnc2"})
	time.Sleep(50 * time.Millisecond)
	if m.Current().String() != "direct" {
		t.Fatalf("stale failure flapped to %s", m.Current())
	}
}

func TestPathManager_ReportFailureReselects(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": false, "socks": true, "tunnel:ovpnc2": true}}
	m := testMgr(t, live, tdCfg(), tdPlat(), true)
	m.SelectOnce(context.Background())
	if m.Current().String() != "socks" {
		t.Fatal(m.Current())
	}
	live.set("socks", false)
	m.ReportFailure(m.Current())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Current().String() == "tunnel:ovpnc2" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("still %s", m.Current())
}

func TestPathManager_TunnelPathGetsAppliedMark(t *testing.T) {
	m := NewPathManager(PathManagerConfig{
		LoadVPN:      func() (*vpnconfig.VPNDirectorConfig, error) { return tdCfg(), nil },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return tdPlat(), nil },
		Listening:    func(int) bool { return false },
		LoadTunnelIdx: func() map[string]int {
			return map[string]int{"ovpnc2": 0}
		},
		Probe: func(ctx context.Context, p Path) error {
			if p.Kind == netpath.KindTunnel && p.Mark == 0x10000 {
				return nil
			}
			return errors.New("dead")
		},
		Interval: time.Hour,
	})
	m.SelectOnce(context.Background())
	if m.Current().String() != "tunnel:ovpnc2" || m.Current().Mark != 0x10000 {
		t.Fatalf("%s mark=0x%x", m.Current(), m.Current().Mark)
	}
}

func TestPathManager_FailureCycleHasNoDeadline(t *testing.T) {
	ctx, cancel := newFailureCycleContext()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("failure reselect must not share a deadline across probes")
	}
}

func TestPathManager_ReportFailureReachesLaterTunnel(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"ovpnc2": {Clients: []string{"192.168.1.3"}},
				"ovpnc3": {Clients: []string{"192.168.1.4"}},
			},
		},
	}
	plat := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "ovpnc3", Iface: "tun13", Connected: true},
	}}
	live := &liveMap{m: map[string]bool{"direct": false, "socks": true, "tunnel:ovpnc2": true, "tunnel:ovpnc3": true}}
	m := NewPathManager(PathManagerConfig{
		LoadVPN:      func() (*vpnconfig.VPNDirectorConfig, error) { return cfg, nil },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return plat, nil },
		Listening:    func(int) bool { return true },
		Probe: func(ctx context.Context, p Path) error {
			if live.get(p.String()) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
				return errors.New("dead")
			}
		},
		Interval: time.Hour,
	})
	m.SelectOnce(context.Background())
	if m.Current().String() != "socks" {
		t.Fatalf("setup: %s", m.Current())
	}
	live.set("socks", false)
	live.set("tunnel:ovpnc2", false)
	m.ReportFailure(m.Current())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Current().String() == "tunnel:ovpnc3" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("later tunnel skipped: %s", m.Current())
}

func TestPathManager_RefreshesSOCKSPortWithoutDropping(t *testing.T) {
	var mu sync.Mutex
	port := 12346
	livePort := 12346
	m := NewPathManager(PathManagerConfig{
		LoadVPN: func() (*vpnconfig.VPNDirectorConfig, error) {
			mu.Lock()
			defer mu.Unlock()
			return &vpnconfig.VPNDirectorConfig{
				Advanced: map[string]interface{}{"xray": map[string]interface{}{"socks_port": float64(port)}},
			}, nil
		},
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) { return vpnconfig.PlatformInfo{}, nil },
		Listening:    func(int) bool { return true },
		Probe: func(ctx context.Context, p Path) error {
			if p.Kind == netpath.KindDirect {
				return errors.New("dead")
			}
			mu.Lock()
			live := livePort
			mu.Unlock()
			if p.Kind == netpath.KindSOCKS && p.SOCKSPort == live {
				return nil
			}
			return errors.New("dead")
		},
	})
	m.SelectOnce(context.Background())
	if m.Current().Kind != netpath.KindSOCKS || m.Current().SOCKSPort != 12346 {
		t.Fatalf("first: %s port=%d", m.Current(), m.Current().SOCKSPort)
	}
	n := 0
	m.RegisterIdleCloser(func(Path) { n++ })
	mu.Lock()
	port = 23456
	livePort = 23456
	mu.Unlock()
	m.SelectOnce(context.Background())
	cur := m.Current()
	if cur.Kind != netpath.KindSOCKS || cur.SOCKSPort != 23456 {
		t.Fatalf("after port change: %s port=%d", cur, cur.SOCKSPort)
	}
	if n != 1 {
		t.Fatalf("param change must retire connections: closers=%d", n)
	}
}

func TestPathManager_RefreshesTunnelIfaceWithoutDropping(t *testing.T) {
	var mu sync.Mutex
	iface := "tun12"
	liveIface := "tun12"
	m := NewPathManager(PathManagerConfig{
		LoadVPN: func() (*vpnconfig.VPNDirectorConfig, error) { return tdCfg(), nil },
		LoadPlatform: func() (vpnconfig.PlatformInfo, error) {
			mu.Lock()
			defer mu.Unlock()
			return vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
				{ID: "ovpnc2", Iface: iface, Connected: true},
			}}, nil
		},
		Listening: func(int) bool { return false },
		Probe: func(ctx context.Context, p Path) error {
			if p.Kind != netpath.KindTunnel {
				return errors.New("dead")
			}
			mu.Lock()
			live := liveIface
			mu.Unlock()
			if p.Iface == live {
				return nil
			}
			return errors.New("dead")
		},
	})
	m.SelectOnce(context.Background())
	if m.Current().String() != "tunnel:ovpnc2" || m.Current().Iface != "tun12" {
		t.Fatalf("first: %s iface=%q", m.Current(), m.Current().Iface)
	}
	mu.Lock()
	iface = "tun13"
	liveIface = "tun13"
	mu.Unlock()
	m.SelectOnce(context.Background())
	cur := m.Current()
	if cur.String() != "tunnel:ovpnc2" || cur.Iface != "tun13" {
		t.Fatalf("after iface change: %s iface=%q", cur, cur.Iface)
	}
}

func noPathKey(m *PathManager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastNoPath
}

// The WARN for a set of dead paths fires once; an identical repeat drops to
// DEBUG, and a cycle that finds a path arms the WARN again.
func TestPathManager_NoPathKeyTracksTheTriedSet(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": false, "socks": false, "tunnel:ovpnc2": false}}
	m := testMgr(t, live, tdCfg(), tdPlat(), true)

	m.SelectOnce(context.Background())
	if m.Current().String() != "none" {
		t.Fatalf("setup: %s", m.Current())
	}
	first := noPathKey(m)
	if first == "" {
		t.Fatal("a cycle with no path must record the tried set")
	}

	m.SelectOnce(context.Background())
	if got := noPathKey(m); got != first {
		t.Fatalf("an identical outage changed the key: %q -> %q", first, got)
	}

	live.set("direct", true)
	m.SelectOnce(context.Background())
	if m.Current().String() != "direct" {
		t.Fatalf("recovery: %s", m.Current())
	}
	if got := noPathKey(m); got != "" {
		t.Fatalf("a cycle with a path must clear the key, got %q", got)
	}
}

func TestPathManager_ClosesIdleOnChange(t *testing.T) {
	live := &liveMap{m: map[string]bool{"direct": true}}
	m := testMgr(t, live, tdCfg(), tdPlat(), false)
	n := 0
	m.RegisterIdleCloser(func(Path) { n++ })
	m.SelectOnce(context.Background()) // none -> direct
	if n != 1 {
		t.Fatalf("first select closes idle: %d", n)
	}
	live.set("direct", false)
	live.set("tunnel:ovpnc2", true)
	// socks down; replacement is tunnel
	m.SelectOnce(context.Background())
	if n != 2 {
		t.Fatalf("path change: %d", n)
	}
}
