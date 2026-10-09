package netpath

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestKindValues(t *testing.T) {
	for i, kind := range []Kind{KindNone, KindDirect, KindSOCKS, KindTunnel} {
		if int(kind) != i {
			t.Fatalf("kind %d: got %d", i, kind)
		}
	}
}

func TestPathString(t *testing.T) {
	for _, tc := range []struct {
		path Path
		want string
	}{
		{Path{}, "none"},
		{Path{Kind: KindDirect}, "direct"},
		{Path{Kind: KindSOCKS}, "socks"},
		{Path{Kind: KindTunnel, ID: "ovpnc2"}, "tunnel:ovpnc2"},
	} {
		if got := tc.path.String(); got != tc.want {
			t.Fatalf("%+v: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPathSameAndParamsEqual(t *testing.T) {
	p := Path{Kind: KindTunnel, ID: "ovpnc2", Iface: "tun12", SOCKSPort: 12346, Mark: 0x10000}
	if !p.Same(p) || !p.ParamsEqual(p) {
		t.Fatal("an unchanged path must have the same identity and parameters")
	}
	for _, tc := range []struct {
		name string
		path Path
		same bool
	}{
		{"kind", Path{Kind: KindDirect, ID: p.ID, Iface: p.Iface, SOCKSPort: p.SOCKSPort, Mark: p.Mark}, false},
		{"id", Path{Kind: p.Kind, ID: "ovpnc3", Iface: p.Iface, SOCKSPort: p.SOCKSPort, Mark: p.Mark}, false},
		{"iface", Path{Kind: p.Kind, ID: p.ID, Iface: "tun13", SOCKSPort: p.SOCKSPort, Mark: p.Mark}, true},
		{"socks port", Path{Kind: p.Kind, ID: p.ID, Iface: p.Iface, SOCKSPort: 23456, Mark: p.Mark}, true},
		{"mark", Path{Kind: p.Kind, ID: p.ID, Iface: p.Iface, SOCKSPort: p.SOCKSPort, Mark: 0x20000}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p.Same(tc.path) != tc.same || tc.path.Same(p) != tc.same {
				t.Fatalf("Same(%+v, %+v): want %t", p, tc.path, tc.same)
			}
			if p.ParamsEqual(tc.path) || tc.path.ParamsEqual(p) {
				t.Fatal("changed identity or dial parameters must not compare equal")
			}
		})
	}
	if !(Path{}).Same(Path{}) || !(Path{}).ParamsEqual(Path{}) {
		t.Fatal("zero paths must compare equal")
	}
}

func TestSOCKSPort(t *testing.T) {
	if SOCKSPort(nil) != 12346 {
		t.Fatalf("nil cfg: got %d", SOCKSPort(nil))
	}
	empty := &vpnconfig.VPNDirectorConfig{}
	if SOCKSPort(empty) != 12346 {
		t.Fatalf("missing advanced: got %d", SOCKSPort(empty))
	}
	cfg := &vpnconfig.VPNDirectorConfig{
		Advanced: map[string]interface{}{"xray": map[string]interface{}{"socks_port": float64(23456)}},
	}
	if SOCKSPort(cfg) != 23456 {
		t.Fatalf("configured: got %d", SOCKSPort(cfg))
	}
	zero := &vpnconfig.VPNDirectorConfig{
		Advanced: map[string]interface{}{"xray": map[string]interface{}{"socks_port": float64(0)}},
	}
	if SOCKSPort(zero) != 12346 {
		t.Fatalf("non-positive: got %d", SOCKSPort(zero))
	}
}

func TestCandidates(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"main":     {Clients: []string{"192.0.2.3"}},
				"ovpnc2":   {Clients: []string{"192.0.2.3"}},
				"ovpnc3":   {Clients: []string{}},
				"wgc1":     {Clients: []string{"192.0.2.4"}},
				"OpenVPN0": {Clients: []string{"192.0.2.5"}},
			},
		},
	}
	plat := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "ovpnc3", Iface: "tun13", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: false},
		{ID: "OpenVPN0", Iface: "", Connected: true},
	}}
	got := Candidates(cfg, plat, true, nil)
	want := []string{"direct", "socks", "tunnel:ovpnc2"}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("names=%v, want %v", names(got), want)
	}
	if got[2].Iface != "tun12" || got[1].SOCKSPort != 12346 {
		t.Fatalf("iface=%q socksPort=%d", got[2].Iface, got[1].SOCKSPort)
	}

	noSocks := Candidates(cfg, plat, false, nil)
	if !reflect.DeepEqual(names(noSocks), []string{"direct", "tunnel:ovpnc2"}) {
		t.Fatalf("socks down: %v", names(noSocks))
	}
	if n := names(Candidates(nil, plat, true, nil)); !reflect.DeepEqual(n, []string{"direct", "socks"}) {
		t.Fatalf("nil cfg: %v", n)
	}
	if n := names(Candidates(cfg, vpnconfig.PlatformInfo{}, true, nil)); !reflect.DeepEqual(n, []string{"direct", "socks"}) {
		t.Fatalf("platform empty: %v", n)
	}
}

func TestLoadTunnelIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tables")
	if err := os.WriteFile(path, []byte("0 ovpnc2\n1 OpenVPN0\n\nbad\n2 wgc1\n-1 rejected\ninvalid rejected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := LoadTunnelIndexes(path)
	want := map[string]int{"ovpnc2": 0, "OpenVPN0": 1, "wgc1": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("indexes %v, want %v", got, want)
	}
	if got := LoadTunnelIndexes(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Fatalf("missing tables: %v", got)
	}
}

func TestCandidates_AppliesTunnelMark(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"ovpnc2": {Clients: []string{"192.0.2.3"}},
			},
		},
	}
	plat := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
	}}
	for _, tc := range []struct {
		name     string
		advanced map[string]interface{}
		indexes  map[string]int
		want     uint32
	}{
		{"index zero", nil, map[string]int{"ovpnc2": 0}, 0x10000},
		{"index one", nil, map[string]int{"ovpnc2": 1}, 0x20000},
		{"no tables", nil, nil, 0},
		{"custom shift", map[string]interface{}{"tunnel_director": map[string]interface{}{"mark_shift": float64(8)}}, map[string]int{"ovpnc2": 0}, 0x100},
		{"fractional shift", map[string]interface{}{"tunnel_director": map[string]interface{}{"mark_shift": 16.5}}, map[string]int{"ovpnc2": 0}, 0x10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Advanced = tc.advanced
			got := Candidates(cfg, plat, false, tc.indexes)
			if len(got) != 2 {
				t.Fatalf("paths %v", names(got))
			}
			if got[1].Mark != tc.want {
				t.Fatalf("mark 0x%x, want 0x%x", got[1].Mark, tc.want)
			}
		})
	}
}

func TestTunnelPath_UsesProvidedTablesAndFailoverExit(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"ovpnc2": {Clients: []string{"192.0.2.3"}},
			"wgc1":   {Clients: []string{"192.0.2.4", "192.0.2.8"}},
		}},
		Xray: vpnconfig.XrayConfig{Failover: &vpnconfig.XrayFailover{
			Tunnel: "wgc1", Clients: []string{"192.0.2.8"}, Added: []string{"192.0.2.8"},
		}},
	}
	plat := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: true},
	}}
	path := filepath.Join(t.TempDir(), "tables")
	if err := os.WriteFile(path, []byte("0 ovpnc2\n1 wgc1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := vpnconfig.FailoverTDExit(cfg, plat)
	got := TunnelPath(cfg, plat, id, path)
	want := Path{Kind: KindTunnel, ID: "wgc1", Iface: "wgc1", Mark: 0x20000}
	if got != want {
		t.Fatalf("path %+v, want %+v", got, want)
	}
	if got := TunnelPath(nil, plat, "ovpnc2", path); got.Mark != 0x10000 {
		t.Fatalf("nil config: mark 0x%x, want the default shift of 16", got.Mark)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	want.Mark = 0
	if got := TunnelPath(cfg, plat, id, missing); got != want {
		t.Fatalf("missing tables: %+v, want %+v", got, want)
	}
}

func names(ps []Path) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}
