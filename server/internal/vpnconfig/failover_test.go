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
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if cfg.Xray.Failover == nil || cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover = %+v", cfg.Xray.Failover)
	}
	// failover.clients is only addresses this move added, not IPs already on
	// the tunnel. 192.168.1.3 stays on ovpnc2 (spec §9).
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Fatalf("added snapshot %v", cfg.Xray.Failover.Clients)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9"}) {
		t.Fatalf("xray.clients %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3", "192.168.1.8"}) {
		t.Fatalf("tunnel %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if len(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients) != 2 {
		t.Fatal("second move must not duplicate")
	}
	RestoreXrayClientsFromFailover(cfg)
	if cfg.Xray.Failover != nil {
		t.Fatal("failover")
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.8"}) {
		t.Fatalf("restored xray %v", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3"}) {
		t.Fatalf("tunnel after restore %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
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

func TestRestore_SkipsClientsRemovedFromTunnelDuringFailover(t *testing.T) {
	cfg := sample()
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.7")
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	if !reflect.DeepEqual(cfg.Xray.Failover.Clients, []string{"192.168.1.8", "192.168.1.7"}) {
		t.Fatalf("added snapshot %v", cfg.Xray.Failover.Clients)
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
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.8"}) {
		t.Fatalf("restored xray %v, want the removed client left out", cfg.Xray.Clients)
	}
	if !reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3"}) {
		t.Fatalf("tunnel after restore %v", cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
}

func TestApplyFailoverSnapshot_OnlyMovesListedClients(t *testing.T) {
	cfg := sample()
	MoveXrayClientsToTunnel(cfg, "ovpnc2")
	RestoreXrayClientsFromFailover(cfg)
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.10")

	ApplyFailoverSnapshot(cfg, &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8"}})
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
