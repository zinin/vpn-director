package vpnconfig

import (
	"reflect"
	"strings"
	"testing"
)

// movable is 192.168.50.10 on Xray, which excludes ru, and 192.168.50.30 on wgc1.
func movable() *VPNDirectorConfig {
	return &VPNDirectorConfig{
		Xray: XrayConfig{Clients: []string{"192.168.50.10"}, ExcludeSets: []string{"ru"}},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
		}},
	}
}

func TestClientRoutes(t *testing.T) {
	cfg := movable()
	// A staged failover: the address sits on Xray and on the fallback tunnel.
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.30", "192.168.50.10/32"}}
	if got := ClientRoutes(cfg, "192.168.50.10"); !reflect.DeepEqual(got, []string{"xray", "wgc1"}) {
		t.Errorf("ClientRoutes = %v, want [xray wgc1]", got)
	}
	if got := ClientRoutes(cfg, "192.168.50.99"); len(got) != 0 {
		t.Errorf("ClientRoutes of an unknown address = %v", got)
	}
}

func TestMoveClient_XrayToTunnel(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
}

func TestMoveClient_TunnelToXray(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.30", "xray"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if strings.Join(cfg.Xray.Clients, ",") != "192.168.50.10,192.168.50.30" {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	// A tunnel whose last client leaves stays, empty, as after a delete.
	tun, ok := cfg.TunnelDirector.Tunnels["wgc1"]
	if !ok || len(tun.Clients) != 0 || strings.Join(tun.Exclude, ",") != "ru" {
		t.Errorf("wgc1 = %+v, %v", tun, ok)
	}
}

func TestMoveClient_TakesEverySpellingOffEveryRoute(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.10", "192.168.50.30"}}
	if got := MoveClient(cfg, "192.168.50.10", "ovpnc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 || strings.Join(cfg.TunnelDirector.Tunnels["wgc1"].Clients, ",") != "192.168.50.30" {
		t.Errorf("xray %v, wgc1 %v: a spelling is left behind", cfg.Xray.Clients, cfg.TunnelDirector.Tunnels["wgc1"].Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["ovpnc1"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("ovpnc1 = %v", got)
	}
}

// An empty exclude would carry the client's local-country traffic through the
// tunnel too, so a new tunnel starts from Xray's exclusions - a copy of them.
func TestMoveClient_NewTunnelInheritsTheXrayExclusions(t *testing.T) {
	cfg := movable()
	MoveClient(cfg, "192.168.50.10", "OpenVPN0")
	tun := cfg.TunnelDirector.Tunnels["OpenVPN0"]
	if strings.Join(tun.Clients, ",") != "192.168.50.10" || strings.Join(tun.Exclude, ",") != "ru" {
		t.Fatalf("OpenVPN0 = %+v", tun)
	}
	cfg.Xray.ExcludeSets[0] = "de"
	if cfg.TunnelDirector.Tunnels["OpenVPN0"].Exclude[0] != "ru" {
		t.Error("the tunnel shares its exclude slice with xray.exclude_sets")
	}
}

// The shell subtracts paused_clients by exact string: the paused entry follows
// the address to its new spelling, or the client resumes on its own.
func TestMoveClient_PausedClientMovesPaused(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10/32"}
	MoveClient(cfg, "192.168.50.10", "wgc1")
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.10"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
}

// Review Focus 5: a paused /32 on the fallback tunnel of a committed failover.
func TestMoveClient_PausedSlash32DuringAFailover(t *testing.T) {
	cfg := &VPNDirectorConfig{
		PausedClients: []string{"192.168.50.8/32"},
		Xray: XrayConfig{
			ExcludeSets: []string{"ru"},
			Failover: &XrayFailover{
				Tunnel: "wgc1", Clients: []string{"192.168.50.8/32"}, Added: []string{"192.168.50.8/32"}, Committed: true,
			},
		},
		TunnelDirector: TunnelDirectorConfig{Tunnels: map[string]TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.3", "192.168.50.8/32"}, Exclude: []string{"ru"}},
		}},
	}
	if got := MoveClient(cfg, "192.168.50.8", "ovpnc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if got := cfg.TunnelDirector.Tunnels["ovpnc1"].Clients; strings.Join(got, ",") != "192.168.50.8" {
		t.Errorf("ovpnc1 = %v", got)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.3" {
		t.Errorf("wgc1 = %v", got)
	}
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.8"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
	if fo := cfg.Xray.Failover; len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Errorf("failover %+v: the restore would take the address back to Xray", fo)
	}
}

// A move repoints the moved address's pause, nobody else's. 192.168.50.20 sits
// on xray and, as 192.168.50.20/32, on wgc1, both paused - what the pause
// handlers write. Repointed onto its first stored spelling, the /32 entry went,
// and the shell, which subtracts by exact string, resumed the client on wgc1.
func TestMoveClient_LeavesAnotherClientsPausesAlone(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.50.20")
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{
		Clients: []string{"192.168.50.30", "192.168.50.20/32"}, Exclude: []string{"ru"},
	}
	cfg.PausedClients = []string{"192.168.50.20", "192.168.50.20/32"}

	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.20", "192.168.50.20/32"}) {
		t.Errorf("paused_clients = %v; another client's pause changed", cfg.PausedClients)
	}
}

// A stale entry - 192.168.50.20 paused by an older build for a client stored as
// 192.168.50.20/32 - pauses nothing, and the UI shows the client active. An
// unrelated move must not turn it into a pause that sends the client direct.
func TestMoveClient_LeavesAStalePauseStale(t *testing.T) {
	cfg := movable()
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{
		Clients: []string{"192.168.50.30", "192.168.50.20/32"}, Exclude: []string{"ru"},
	}
	cfg.PausedClients = []string{"192.168.50.20"}

	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.20"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
	for _, c := range CollectClients(cfg) {
		if c.IP == "192.168.50.20/32" && c.Paused {
			t.Error("192.168.50.20/32 is paused now: an unrelated move paused it")
		}
	}
}

// The moved address itself: a stale entry of it goes, and it moves as it was -
// active here, since no stored spelling of it was paused.
func TestMoveClient_DropsTheMovedAddressesStalePause(t *testing.T) {
	cfg := movable()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10", "192.168.50.99"}

	MoveClient(cfg, "192.168.50.10", "wgc1")
	if !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.50.99"}) {
		t.Errorf("paused_clients = %v", cfg.PausedClients)
	}
}

func TestMoveClient_AlreadyThereChangesNothing(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.10", "xray"); got != ClientAlreadyThere {
		t.Fatalf("MoveClient = %v, want ClientAlreadyThere", got)
	}
	if !reflect.DeepEqual(cfg, movable()) {
		t.Errorf("cfg changed: %+v", cfg)
	}
}

func TestMoveClient_NotFoundChangesNothing(t *testing.T) {
	cfg := movable()
	if got := MoveClient(cfg, "192.168.50.99", "wgc1"); got != ClientNotFound {
		t.Fatalf("MoveClient = %v, want ClientNotFound", got)
	}
	if !reflect.DeepEqual(cfg, movable()) {
		t.Errorf("cfg changed: %+v", cfg)
	}
}

// During a staged failover the address sits on Xray and on the fallback tunnel.
// A move to that tunnel is a real move: it leaves the address there once.
func TestMoveClient_StagedFailoverEndsOnOneRoute(t *testing.T) {
	cfg := movable()
	cfg.TunnelDirector.Tunnels["wgc1"] = TunnelConfig{Clients: []string{"192.168.50.10", "192.168.50.30"}}
	if got := MoveClient(cfg, "192.168.50.10", "wgc1"); got != ClientMoved {
		t.Fatalf("MoveClient = %v, want ClientMoved", got)
	}
	if len(cfg.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", cfg.Xray.Clients)
	}
	if got := cfg.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
}
