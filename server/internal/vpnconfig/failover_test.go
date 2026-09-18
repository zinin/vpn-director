package vpnconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sample() *VPNDirectorConfig {
	return &VPNDirectorConfig{
		PausedClients: []string{"192.168.1.9"},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"main":   {Clients: []string{"192.168.1.1"}},
			"ovpnc1": {Clients: []string{"192.168.1.2"}},
			"ovpnc2": {Clients: []string{"192.168.1.3"}},
			"ovpnc3": {Clients: []string{}},
			"wgc1":   {Clients: []string{"192.168.1.4"}},
		}},
		Xray: XrayConfig{
			Clients:         []string{"192.168.1.8", "192.168.1.9", "192.168.1.3"},
			SubscriptionURL: "https://cdn.example/s/token",
		},
	}
}

func plat() PlatformInfo {
	return PlatformInfo{Tunnels: []PlatformTunnel{
		{ID: "ovpnc1", Iface: "tun11", Connected: false},
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "ovpnc3", Iface: "tun13", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: false},
	}}
}

func TestTDExits_MatchesPathManagerFilter(t *testing.T) {
	got := TDExits(sample(), plat())
	want := []string{"ovpnc2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TDExits = %v, want %v", got, want)
	}
	if FirstTDExit(sample(), plat()) != "ovpnc2" {
		t.Fatal("FirstTDExit")
	}
	if FirstTDExit(sample(), PlatformInfo{}) != "" {
		t.Fatal("no platform tunnels")
	}
}

func TestFailoverTDExit_PrefersTheRecordedFailoverTunnel(t *testing.T) {
	both := PlatformInfo{Tunnels: []PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: true},
	}}
	firstOnly := PlatformInfo{Tunnels: []PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: false},
	}}
	tests := []struct {
		name     string
		failover *XrayFailover
		plat     PlatformInfo
		want     string
	}{
		{"moved to the second exit", &XrayFailover{Tunnel: "wgc1"}, both, "wgc1"},
		{"recorded tunnel disconnected", &XrayFailover{Tunnel: "wgc1"}, firstOnly, "ovpnc2"},
		{"recorded tunnel is not an exit", &XrayFailover{Tunnel: "ovpnc3"}, both, "ovpnc2"},
		{"not failed over", nil, both, "ovpnc2"},
		{"no exits", &XrayFailover{Tunnel: "wgc1"}, PlatformInfo{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := sample()
			cfg.Xray.Failover = tt.failover
			if got := FailoverTDExit(cfg, tt.plat); got != tt.want {
				t.Fatalf("FailoverTDExit = %q, want %q", got, tt.want)
			}
		})
	}
	if got := FailoverTDExit(nil, both); got != "" {
		t.Fatalf("FailoverTDExit(nil) = %q, want no exit", got)
	}
}

func TestEffectiveXrayClients_DropsPaused(t *testing.T) {
	got := EffectiveXrayClients(sample())
	want := []string{"192.168.1.8", "192.168.1.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestArmed(t *testing.T) {
	cfg := sample()
	if !Armed(cfg) {
		t.Fatal("url + clients")
	}
	cfg.Xray.Clients = nil
	if Armed(cfg) {
		t.Fatal("url but no clients and no failover")
	}
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	if !Armed(cfg) {
		t.Fatal("failover arms even with empty xray.clients")
	}
	cfg.Xray.SubscriptionURL = ""
	if Armed(cfg) {
		t.Fatal("no url")
	}
}

func TestMoveAndRestore_KeepsForeignTunnelClients(t *testing.T) {
	cfg := sample()
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	tun.Clients = append(tun.Clients, "192.168.1.4")
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover = %+v", cfg.Xray.Failover)
	}
	// 192.168.1.3 is already on ovpnc2 and in xray.clients (Xray wins). It
	// still belongs in the snapshot so restore puts it back on Xray.
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8", "192.168.1.3"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9"}) {
		t.Fatalf("xray.clients %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.4", "192.168.1.8"}) {
		t.Fatalf("tunnel %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if len(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients) != 3 {
		t.Fatal("second move must not duplicate")
	}
	RestoreXrayClientsFromFailover(cfg)
	if cfg.Xray.Failover != nil {
		t.Fatal("failover")
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.8", "192.168.1.3"}) {
		t.Fatalf("restored xray %v", cfg.Xray.Clients)
	}
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("moved client must leave the tunnel")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap must stay on the tunnel")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.4") {
		t.Fatal("foreign tunnel-only client")
	}
}

func TestRestore_ReturnsOverlapClientToXray(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.5"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = TunnelConfig{Clients: []string{"192.168.1.5"}}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.5"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	if contains(cfg.Xray.Clients, "192.168.1.5") {
		t.Fatal("overlap must leave Xray during failover")
	}
	RestoreXrayClientsFromFailover(cfg)
	if !contains(cfg.Xray.Clients, "192.168.1.5") {
		t.Fatal("overlap must return to Xray")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.5") {
		t.Fatal("overlap must stay on the tunnel")
	}
}

func TestRestore_OverlapCIDRStaysOnTunnel(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.50.0/24"}
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.0/24"}}
	MoveXrayClientsToTunnel(cfg, "wgc1")
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.50.0/24"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	RestoreXrayClientsFromFailover(cfg)
	if !contains(cfg.Xray.Clients, "192.168.50.0/24") {
		t.Fatal("overlap must return to Xray")
	}
	if !contains(cfg.TunnelDirector.Tunnels["wgc1"].Clients, "192.168.50.0/24") {
		t.Fatal("original wgc1 assignment must survive restore")
	}
}

func TestCommit_DoesNotDropClientRemovedThenReaddedToXray(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	// DELETE /api/clients strips the address from every route and leaves
	// xray.failover as it was. Adding the same address back as xray must not
	// look staged: Commit would drop it and Restore would not put it back.
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	kept := make([]string, 0, len(tun.Clients))
	for _, ip := range tun.Clients {
		if ip != "192.168.1.8" {
			kept = append(kept, ip)
		}
	}
	tun.Clients = kept
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	cfg.Xray.Clients = []string{"192.168.1.9", "192.168.1.8"}
	if FailoverStaged(cfg) {
		t.Fatal("re-added xray client is not on the fallback tunnel")
	}
	CommitXrayFailover(cfg)
	if !contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("re-added xray client must stay")
	}
}

func TestStageThenCommit_DropsXrayAfterTunnelHasClients(t *testing.T) {
	cfg := sample()
	StageXrayClientsToTunnel(cfg, "ovpnc2")
	if !FailoverStaged(cfg) {
		t.Fatal("staged")
	}
	if !contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("TPROXY must still match during the tunnel apply")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("fallback client")
	}
	CommitXrayFailover(cfg)
	if FailoverStaged(cfg) {
		t.Fatal("committed")
	}
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("still in xray.clients")
	}
	if contains(cfg.Xray.Clients, "192.168.1.3") {
		t.Fatal("overlap client must leave Xray")
	}
}

func TestRestore_SkipsSnapshotWhenFallbackTunnelKeyIsGone(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.7")
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	// Wizard rebuilt tunnel_director.tunnels without the old fallback key
	// and put those addresses on another tunnel.
	delete(cfg.TunnelDirector.Tunnels, "ovpnc2")
	wgc := cfg.TunnelDirector.Tunnels["wgc1"]
	wgc.Clients = append(wgc.Clients, "192.168.1.8", "192.168.1.7")
	cfg.TunnelDirector.Tunnels["wgc1"] = wgc

	RestoreXrayClientsFromFailover(cfg)
	if cfg.Xray.Failover != nil {
		t.Fatal("failover")
	}
	if contains(cfg.Xray.Clients, "192.168.1.8") || contains(cfg.Xray.Clients, "192.168.1.7") {
		t.Fatalf("reassigned clients back on Xray: %v", cfg.Xray.Clients)
	}
	if !contains(cfg.TunnelDirector.Tunnels["wgc1"].Clients, "192.168.1.8") {
		t.Fatal("wgc1 assignment")
	}
}

func TestRestore_SkipsClientsRemovedFromTunnelDuringFailover(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.7")
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8", "192.168.1.3", "192.168.1.7"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	// DELETE /api/clients drops the address from its route and leaves
	// xray.failover as it was.
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	tun.Clients = []string{"192.168.1.3", "192.168.1.8"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun

	RestoreXrayClientsFromFailover(cfg)
	if cfg.Xray.Failover != nil {
		t.Fatal("failover")
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.8", "192.168.1.3"}) {
		t.Fatalf("restored xray %v, want the removed client left out", cfg.Xray.Clients)
	}
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatalf("tunnel after restore %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
}

func TestRestore_NilAddedRemovesAllRestoredFromTunnel(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.9"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = TunnelConfig{Clients: []string{"192.168.1.5", "192.168.1.4"}}
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.5"}}
	RestoreXrayClientsFromFailover(cfg)
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.5") {
		t.Fatal("legacy record without added must still take restored addresses off the tunnel")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.4") {
		t.Fatal("foreign tunnel-only client")
	}
}

func TestFailoverJSON_RoundTripKeepsOverlapOnTunnel(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	raw, err := json.Marshal(cfg.Xray.Failover)
	if err != nil {
		t.Fatal(err)
	}
	var fo XrayFailover
	if err := json.Unmarshal(raw, &fo); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fo.Added, []string{"192.168.1.8"}) {
		t.Fatalf("added %v from %s", fo.Added, raw)
	}
	cfg.Xray.Failover = &fo
	RestoreXrayClientsFromFailover(cfg)
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("added client")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap after JSON round-trip")
	}
}

func TestFailoverJSON_EmptyAddedIsNotNilAfterRoundTrip(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.3"}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if cfg.Xray.Failover.Added == nil {
		t.Fatal("all-overlap added must be an empty slice, not nil")
	}
	raw, err := json.Marshal(cfg.Xray.Failover)
	if err != nil {
		t.Fatal(err)
	}
	var fo XrayFailover
	if err := json.Unmarshal(raw, &fo); err != nil {
		t.Fatal(err)
	}
	if fo.Added == nil {
		t.Fatalf("round-trip nil added from %s; omitempty would drop overlap on restore", raw)
	}
	cfg.Xray.Failover = &fo
	RestoreXrayClientsFromFailover(cfg)
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
		t.Fatal("overlap after empty-added round-trip")
	}
}

func TestApplyFailoverSnapshot_OnlyMovesListedClients(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	RestoreXrayClientsFromFailover(cfg)
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.10")

	ApplyFailoverSnapshot(cfg, &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}, Committed: true})
	if cfg.Xray.Failover == nil || !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Fatalf("snapshot %+v", cfg.Xray.Failover)
	}
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("listed client still on xray")
	}
	if !contains(cfg.Xray.Clients, "192.168.1.10") {
		t.Fatal("unrelated Xray client")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("listed client not on tunnel")
	}
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.10") {
		t.Fatal("unrelated client on tunnel")
	}
}

func TestEnsureFailoverStaged_SkipsClientsRemovedFromTunnel(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	kept := make([]string, 0)
	for _, ip := range tun.Clients {
		if ip != "192.168.1.8" {
			kept = append(kept, ip)
		}
	}
	tun.Clients = kept
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	EnsureFailoverStaged(cfg)
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("deleted client must not return to Xray")
	}
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("must not put a deleted client back on the tunnel")
	}
}

func TestEnsureFailoverStaged_SkipsWhenTunnelKeyIsGone(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	delete(cfg.TunnelDirector.Tunnels, "ovpnc2")
	EnsureFailoverStaged(cfg)
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("must not restore onto Xray after the wizard dropped the fallback tunnel")
	}
}

func TestEnsureFailoverStaged_PutsSnapshotOnXrayAndTunnel(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("committed")
	}
	EnsureFailoverStaged(cfg)
	if !contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("must return to Xray")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("must stay on the tunnel")
	}
	if cfg.Xray.Failover == nil {
		t.Fatal("failover")
	}
}

func TestXrayConfig_OmitsSubscriptionURLAndFailoverWhenEmpty(t *testing.T) {
	out, err := json.Marshal(VPNDirectorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, k := range []string{"subscription_url", "failover"} {
		if strings.Contains(s, k) {
			t.Errorf("marshalled %s, want no %q", s, k)
		}
	}
}

// TDCarries is the Go side of what tunnel.sh marks: an IPv4 address or CIDR the
// kernel tools read as written (is_ipv4_net), inside RFC1918 (is_lan_ip).
func TestTDCarries(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.5":     true,
		"192.168.50.0/24": true,
		"10.0.0.0/8":      true,
		"172.16.5.4":      true,
		"172.31.255.1":    true,
		"192.168.1.1000":  false,
		"192.168.1.08":    false,
		"fd00::10":        false,
		"100.64.0.8":      false,
		"8.8.8.8":         false,
		"172.32.0.1":      false,
		"":                false,
	} {
		if got := TDCarries(addr); got != want {
			t.Errorf("TDCarries(%q) = %v, want %v", addr, got, want)
		}
	}
}

// A client TUN_DIR cannot mark - outside RFC1918, IPv6, a typo iptables refuses
// - is still TPROXY's to take. Moved to the tunnel and dropped from Xray, it
// would leave through the WAN; on a dead Xray it at least goes nowhere.
func TestStageXrayClientsToTunnel_LeavesWhatTheTunnelCannotCarryOnXray(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.8", "100.64.0.8", "fd00::10", "192.168.1.1000"}
	StageXrayClientsToTunnel(cfg, "ovpnc2")
	CommitXrayFailover(cfg)
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"100.64.0.8", "fd00::10", "192.168.1.1000"}) {
		t.Fatalf("xray.clients %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.8"}) {
		t.Fatalf("tunnel %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
}

func TestCarriableXrayClients(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.8", "192.168.1.9", "100.64.0.8"}
	if got := CarriableXrayClients(cfg); !reflect.DeepEqual(got, []string{"192.168.1.8"}) {
		t.Fatalf("CarriableXrayClients = %v", got)
	}
}

// Whether the clients ever left Xray decides what a restore says and what a
// failed one goes back to. The file is what survives a bot restart, and a
// restore that re-staged the snapshot looks exactly like a stage that was never
// committed.
func TestFailoverCommitted(t *testing.T) {
	cfg := sample()
	StageXrayClientsToTunnel(cfg, "ovpnc2")
	if FailoverCommitted(cfg) {
		t.Fatal("a stage is not a commit")
	}
	CommitXrayFailover(cfg)
	if !cfg.Xray.Failover.Committed || !FailoverCommitted(cfg) {
		t.Fatalf("failover %+v after the commit", cfg.Xray.Failover)
	}
	EnsureFailoverStaged(cfg)
	if !FailoverCommitted(cfg) {
		t.Fatal("staged back for a restore, the clients still left Xray once")
	}
	raw, err := json.Marshal(cfg.Xray.Failover)
	if err != nil {
		t.Fatal(err)
	}
	var fo XrayFailover
	if err := json.Unmarshal(raw, &fo); err != nil {
		t.Fatal(err)
	}
	if !fo.Committed {
		t.Fatalf("committed lost in %s", raw)
	}

	// A record from before the field: committed is what is not staged.
	legacy := sample()
	legacy.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.3"}}
	legacy.Xray.Clients = []string{"192.168.1.9"}
	if !FailoverCommitted(legacy) {
		t.Fatal("legacy record off Xray")
	}
	if FailoverCommitted(sample()) {
		t.Fatal("no failover")
	}
}

// A record from before the Committed field is committed only while its
// snapshot is off Xray - and the restore stage puts it back on. Past that
// stage, a restore whose last apply failed read the record as a stage that
// never committed and brought the clients back without a word.
func TestEnsureFailoverStaged_KeepsThatALegacyRecordWasCommitted(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.9"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = TunnelConfig{Clients: []string{"192.168.1.3", "192.168.1.8"}}
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}}
	EnsureFailoverStaged(cfg)
	if !cfg.Xray.Failover.Committed || !FailoverCommitted(cfg) {
		t.Fatalf("failover %+v; staged for the restore, it was committed all the same", cfg.Xray.Failover)
	}

	staged := sample()
	StageXrayClientsToTunnel(staged, "ovpnc2")
	EnsureFailoverStaged(staged)
	if staged.Xray.Failover.Committed {
		t.Fatal("a stage that never committed stays one")
	}
}

// A restore that fails is put back the way it started. A stage that was never
// committed kept its clients on Xray all along, and committing it now would
// move them onto a tunnel that never became ready.
func TestApplyFailoverSnapshot_AStagedSnapshotKeepsXrayMembership(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.8"}
	ApplyFailoverSnapshot(cfg, &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}, Added: []string{"192.168.1.8"}})
	if !contains(cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("a staged snapshot stays on Xray")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("and goes back on the tunnel")
	}
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Committed || !FailoverStaged(cfg) {
		t.Fatalf("failover %+v", cfg.Xray.Failover)
	}
}

// Xray clients added, re-added or resumed during a failover used to stay on
// the dead outbound for as long as it lasted: only the snapshot was ever moved.
func TestExtendXrayFailover_StagesNewXrayClients(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = []string{"192.168.1.8"}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.20", "fd00::1")

	if !ExtendXrayFailover(cfg) {
		t.Fatal("a new Xray client joins the failover")
	}
	fo := cfg.Xray.Failover
	if !reflect.DeepEqual(fo.Clients, []string{"192.168.1.8", "192.168.1.20"}) {
		t.Fatalf("snapshot %v", fo.Clients)
	}
	if !reflect.DeepEqual(fo.Added, []string{"192.168.1.8", "192.168.1.20"}) {
		t.Fatalf("added %v", fo.Added)
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.20") {
		t.Fatal("on the fallback tunnel")
	}
	if !contains(cfg.Xray.Clients, "192.168.1.20") || !FailoverStaged(cfg) {
		t.Fatal("staged, not committed: TPROXY keeps it until TUN_DIR has it")
	}
	if contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "fd00::1") {
		t.Fatal("an address the tunnel cannot carry stays where it is")
	}
	if ExtendXrayFailover(cfg) {
		t.Fatal("nothing new the second time")
	}

	// Deleted from the tunnel and added back as xray: a member again.
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	tun.Clients = []string{"192.168.1.3", "192.168.1.20"}
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.8")
	if !ExtendXrayFailover(cfg) {
		t.Fatal("a re-added client joins again")
	}
	if !contains(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("re-added client on the tunnel")
	}
	if ExtendXrayFailover(sample()) {
		t.Fatal("no failover, nothing to extend")
	}
}

// A user who puts an address on a tunnel, or deletes it, during a failover has
// decided where it goes. The restore used to take it back to Xray.
func TestDetachFailoverClient(t *testing.T) {
	cfg := sample()
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}}
	DetachFailoverClient(cfg, "192.168.1.8/32")
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.3"}) {
		t.Fatalf("snapshot %v", cfg.Xray.Failover.Clients)
	}
	if cfg.Xray.Failover.Added == nil || len(cfg.Xray.Failover.Added) != 0 {
		t.Fatalf("added %#v; empty, not nil - nil is the older record whose restore drops every restored address", cfg.Xray.Failover.Added)
	}
	DetachFailoverClient(sample(), "192.168.1.8")
}

// Two exits that never become ready traded the clients back and forth every
// five minutes, and a third one that worked was never tried.
func TestNextTDExit_SkipsEveryTriedExit(t *testing.T) {
	cfg := sample()
	all := PlatformInfo{Tunnels: []PlatformTunnel{
		{ID: "ovpnc1", Iface: "tun11", Connected: true},
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: true},
	}}
	if got := NextTDExit(cfg, all, map[string]bool{"ovpnc1": true, "ovpnc2": true}); got != "wgc1" {
		t.Fatalf("NextTDExit = %q, want wgc1", got)
	}
	if got := NextTDExit(cfg, all, map[string]bool{"ovpnc1": true, "ovpnc2": true, "wgc1": true}); got != "" {
		t.Fatalf("NextTDExit = %q, want none left", got)
	}
}
