package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var errProbe = errors.New("probe failed")
var errApply = errors.New("apply failed")

// errSaveConfig is Generate's error when config.json was written and only the
// active_server record of it failed to save.
var errSaveConfig = errors.New("save config: no space left on device")

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

// seq is what production's Generate returns: the counter active_server carries
// once the write is done. A fake that records one returns the value it just
// wrote; one that records nothing returns what the config already held.
func (f *fake) seq() int {
	return vpnconfig.ActiveSeq(f.cfg.Xray.ActiveServer)
}

// checkGuard runs the guard a Generate call carries against f.cfg, where
// production runs it under the config lock right before config.json is written.
func (f *fake) checkGuard(guard func(*vpnconfig.VPNDirectorConfig) error) error {
	if guard == nil {
		return nil
	}
	return guard(f.cfg)
}

// generateAll is a Generate that writes every server its guard lets through.
func (f *fake) generateAll(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
	if err := f.checkGuard(guard); err != nil {
		return false, f.seq(), err
	}
	return true, f.seq(), nil
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
	if f.cfg.Xray.Failover == nil {
		t.Fatal("must keep failover until TPROXY apply succeeds")
	}
	f.applyErr = nil
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must drop failover after TPROXY apply")
	}
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

// REALITY's server name is the site the handshake borrows, never the proxy's
// own host. An entry without one cannot connect - the Web UI and /xray refuse
// it - and the hostname must not make it look complete to the walk.
func TestServerForDial_LeavesARealitySNIEmpty(t *testing.T) {
	s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "reality"})
	if s.Address != "203.0.113.50" {
		t.Fatalf("address %q", s.Address)
	}
	if s.SNI != "" {
		t.Fatalf("sni %q; a REALITY entry without one must stay without one", s.SNI)
	}
}

// A stored outbound gets the IP in its own address slot, whatever the
// protocol keeps it in; the record and the outbound agree on the address.
func TestServerForDial_WritesTheIPIntoTheOutbound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outbound string
		path     []string
	}{
		{"vless vnext", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u"}]}]}}`, []string{"settings", "vnext", "0", "address"}},
		{"vless flat", `{"protocol":"vless","settings":{"address":"oslo.example","port":443,"id":"u"}}`, []string{"settings", "address"}},
		{"trojan", `{"protocol":"trojan","settings":{"servers":[{"address":"oslo.example","port":443,"password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
		{"shadowsocks", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"oslo.example","port":8388,"method":"aes-256-gcm","password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
		{"hysteria", `{"protocol":"hysteria","settings":{"version":2,"address":"oslo.example","port":443}}`, []string{"settings", "address"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"", "203.0.113.50"}, Outbound: json.RawMessage(tc.outbound)})
			if s.Address != "203.0.113.50" {
				t.Fatalf("address %q", s.Address)
			}
			var ob interface{}
			if err := json.Unmarshal(s.Outbound, &ob); err != nil {
				t.Fatal(err)
			}
			v := ob
			for _, key := range tc.path {
				switch node := v.(type) {
				case map[string]interface{}:
					v = node[key]
				case []interface{}:
					v = node[0]
				}
			}
			if v != "203.0.113.50" {
				t.Fatalf("outbound %s", s.Outbound)
			}
		})
	}
}

// Dialing an IP must not change the name the server is reached by: an empty
// TLS server name gets the hostname, and so does the Host of a transport
// without security; with TLS Xray takes that Host from the server name, and
// an explicit value stays.
func TestServerForDial_KeepsTheHostnameWhereTheSourceLeftItToTheAddress(t *testing.T) {
	dial := func(address, outbound string) map[string]interface{} {
		t.Helper()
		s := ServerForDial(vpnconfig.Server{Address: address, IPs: []string{"203.0.113.50"}, Outbound: json.RawMessage(outbound)})
		var ob map[string]interface{}
		if err := json.Unmarshal(s.Outbound, &ob); err != nil {
			t.Fatal(err)
		}
		return ob["streamSettings"].(map[string]interface{})
	}
	ss := dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"ws","security":"tls","wsSettings":{"path":"/ws"}}}`)
	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "cdn.example" {
		t.Fatalf("tls %v", tls)
	}
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; with TLS the Host follows the server name", ws)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"httpupgrade","security":"none"}}`)
	if hu := ss["httpupgradeSettings"].(map[string]interface{}); hu["host"] != "cdn.example" {
		t.Fatalf("httpupgrade %v", hu)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"ws","wsSettings":{"headers":{"Host":"front.example"}}}}`)
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; a Host header the source set stays the Host", ws)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"ws","wsSettings":{"headers":{"host":"front.example"}}}}`)
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; Xray takes a host header in any case, so it stays the Host", ws)
	}
	ss = dial("cdn.example", `{"protocol":"trojan","settings":{"servers":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"sni.example"}}}`)
	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "sni.example" {
		t.Fatalf("tls %v; an explicit server name stays", tls)
	}
	ss = dial("198.51.100.7", `{"protocol":"trojan","settings":{"servers":[{"address":"198.51.100.7","port":443}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	if _, ok := ss["tlsSettings"]; ok {
		t.Fatalf("stream %v; an IP source has no hostname to keep", ss)
	}
	ss = dial("oslo.example", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443}]},"streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"serverName":"www.example.org"}}}`)
	if _, ok := ss["xhttpSettings"]; ok {
		t.Fatalf("stream %v; REALITY gives xhttp its Host", ss)
	}
	// Xray reads xhttpSettings over splithttpSettings and drops the other, so
	// the Host goes into the one the record has.
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"splithttp","splithttpSettings":{"path":"/secret","mode":"packet-up"}}}`)
	if splithttp, _ := ss["splithttpSettings"].(map[string]interface{}); splithttp["host"] != "cdn.example" || splithttp["path"] != "/secret" {
		t.Fatalf("splithttp %v", splithttp)
	}
	if _, ok := ss["xhttpSettings"]; ok {
		t.Fatalf("stream %v; a new xhttpSettings would replace the splithttpSettings", ss)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"xhttp","security":"none","xhttpSettings":{"path":"/a"},"splithttpSettings":{"path":"/b"}}}`)
	if xhttp, _ := ss["xhttpSettings"].(map[string]interface{}); xhttp["host"] != "cdn.example" {
		t.Fatalf("xhttp %v", xhttp)
	}
	if splithttp, _ := ss["splithttpSettings"].(map[string]interface{}); splithttp["host"] != nil {
		t.Fatalf("splithttp %v; Xray reads the xhttpSettings", splithttp)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"xhttp","security":"none"}}`)
	if xhttp, _ := ss["xhttpSettings"].(map[string]interface{}); xhttp["host"] != "cdn.example" {
		t.Fatalf("stream %v", ss)
	}
	// A cleartext gRPC stream takes its :authority from the address when
	// grpcSettings names none; with TLS, Xray takes the server name.
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none","grpcSettings":{"serviceName":"svc"}}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "cdn.example" || grpc["serviceName"] != "svc" {
		t.Fatalf("grpc %v", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none","grpcSettings":{"serviceName":"svc","authority":"front.example"}}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "front.example" {
		t.Fatalf("grpc %v; an explicit authority stays", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"grpc","security":"tls","grpcSettings":{"serviceName":"svc"}}}`)
	if tls, _ := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "cdn.example" {
		t.Fatalf("tls %v", tls)
	}
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != nil {
		t.Fatalf("grpc %v; with TLS the authority follows the server name", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none"}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "cdn.example" {
		t.Fatalf("stream %v", ss)
	}
}

// An endpoint ban takes an address, not the name: a provider's host can resolve
// to one the router cannot reach and another it can. The walk dialed only the
// first, and a server whose first address was banned was rejected whole.
func TestTick_WalkTriesEveryAddressOfAServer(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	var dialed []string
	current := ""
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10", "", "203.0.113.11"}},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		current = ServerForDial(s).Address
		dialed = append(dialed, current)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if current == "203.0.113.11" {
			return nil
		}
		return errProbe
	}

	w.Tick(context.Background())

	if !reflect.DeepEqual(dialed, []string{"203.0.113.10", "203.0.113.11"}) {
		t.Fatalf("dialed %v; every address of the server, in order", dialed)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v; the server's second address works", f.cfg.Xray.Failover)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray; server Oslo"); n != 1 {
		t.Fatalf("notes %v", f.notes)
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

func TestTick_StoppedDoesNotApply(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.Stopped = func() bool { return true }
	tickUntilDead(w, f)
	if f.applies != 0 {
		t.Fatalf("applies %d; /stop must not be undone", f.applies)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must not stage after /stop")
	}
}

// The watch applies with --unless-stopped, so a /stop that takes the lock first
// makes the script skip the apply and exit 0. The marker it left is how the
// watch can tell; nothing more in that tick may write, apply or announce.
func TestTick_ApplySkippedByAStopEndsTheTick(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := f.watch()
	w.Stopped = func() bool { return stopped }
	w.Apply = func() error {
		f.applies++
		stopped = true
		return nil
	}
	tickUntilDead(w, f)
	if f.applies != 1 {
		t.Fatalf("applies %d; nothing may follow the skipped apply", f.applies)
	}
	if !vpnconfig.FailoverStaged(f.cfg) {
		t.Fatal("the move was committed after /stop")
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

// A /stop that lands while the death tick is still probing: moving the clients
// is a write the watch must no longer make.
func TestTick_StopDuringTheDeathProbeStagesNothing(t *testing.T) {
	f := &fake{
		cfg:  baseCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	start := f.now
	stopped := false
	w := f.watch()
	w.Stopped = func() bool { return stopped }
	w.Probe = func(context.Context, int) error {
		if f.now.Sub(start) >= DeadAfter {
			stopped = true
		}
		return errProbe
	}
	tickUntilDead(w, f)
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v written after /stop", f.cfg.Xray.Failover)
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}
}

// The same on the way back: a /stop during the probe that found the outbound
// alive leaves the failover as it was.
func TestTick_StopDuringTheRestoreProbeWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), now: time.Unix(1_700_000_000, 0)}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.Probe = func(context.Context, int) error {
		stopped = true
		return nil
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("restored after /stop: xray.clients %v, failover %+v", f.cfg.Xray.Clients, f.cfg.Xray.Failover)
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}
}

// An unready fallback's retry apply is skipped by a /stop that got the lock
// first; moving the failover to another exit is then a write after /stop.
func TestTick_StopDoesNotRetargetAnUnreadyFallback(t *testing.T) {
	cfg := baseCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg: cfg,
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
			{ID: "ovpnc2", Iface: "tun12", Connected: true},
			{ID: "wgc1", Iface: "wgc1", Connected: true},
		}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.FallbackReady = func(string) bool { return false }
	w.Tick(context.Background()) // not ready: the retry clock starts
	f.now = f.now.Add(ImportRetry)
	w.Apply = func() error {
		f.applies++
		stopped = true
		return nil
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; a stopped router was moved to another exit", f.cfg.Xray.Failover)
	}
}

// A /stop can land anywhere in a walk that takes minutes. From then on the walk
// generates, restarts and announces nothing.
func TestTick_StopDuringTheWalkEndsItQuietly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopAfter int // walk events before the stop lands
	}{
		// The restart runs with --unless-stopped: a stop that took the lock first
		// makes the script skip it and exit 0, and the probe after it would try a
		// server that was never started.
		{"while restarting onto the first candidate", 3},
		{"while probing the first candidate", 4},
		{"while restarting onto the last candidate", 6},
		{"while probing the last candidate", 7},
		{"while restarting onto the preferred server", 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{cfg: failedOverCfg(), now: time.Unix(1_700_000_000, 0)}
			f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
			stopped := false
			var events []string
			record := func(ev string) {
				events = append(events, ev)
				if len(events) == tc.stopAfter {
					stopped = true
				}
			}
			w := runningWatch(f.watch())
			w.Stopped = func() bool { return stopped }
			w.SaveServers = func([]vpnconfig.Server) error { return nil }
			w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
				return []vpnconfig.Server{
					{Name: "Oslo", Address: "oslo.example", Port: 443},
					{Name: "Backup", Address: "backup.example", Port: 443},
				}, nil
			}
			w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				if err := f.checkGuard(guard); err != nil {
					return false, f.seq(), err
				}
				record("generate " + s.Name)
				f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
				return true, f.seq(), nil
			}
			w.RestartXray = func() error {
				record("restart")
				return nil
			}
			w.AfterRestart = func(time.Duration) {}
			// Every server is dead: health probe, Oslo, Backup, then back to Oslo.
			w.Probe = func(context.Context, int) error {
				record("probe")
				return errProbe
			}

			w.Tick(context.Background())

			if len(events) != tc.stopAfter {
				t.Fatalf("events %v; nothing may follow event %d, the stop", events, tc.stopAfter)
			}
			if len(f.notes) != 0 {
				t.Fatalf("notes %v after /stop", f.notes)
			}
		})
	}
}

// The failover branch probes before it drops Xray membership. A /stop that
// lands while that probe waits rules out the commit and the messages after it.
func TestTick_StopDuringTheFailoverProbeKeepsTheStage(t *testing.T) {
	cfg := baseCfg()
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg:  cfg,
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.Probe = func(context.Context, int) error {
		stopped = true
		return errProbe
	}
	w.Tick(context.Background())
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v; a stopped router keeps the staged membership", f.cfg.Xray.Clients)
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}
}

// LoadPlatform shells out to vpn-director.sh and takes no lock, so a /stop can
// finish while it runs. Staging the clients and announcing the fallback are
// writes and messages that stop rules out.
func TestTick_StopDuringThePlatformLookupStagesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		plat vpnconfig.PlatformInfo
	}{
		{"with a fallback tunnel", vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}}},
		{"with no fallback tunnel", vpnconfig.PlatformInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{
				cfg:      baseCfg(),
				probeErr: errProbe,
				now:      time.Unix(1_700_000_000, 0),
			}
			stopped := false
			w := f.watch()
			w.Stopped = func() bool { return stopped }
			w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
				stopped = true
				return tc.plat, nil
			}
			tickUntilDead(w, f)
			if f.cfg.Xray.Failover != nil {
				t.Fatalf("failover %+v staged after /stop", f.cfg.Xray.Failover)
			}
			if f.applies != 0 || len(f.notes) != 0 {
				t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
			}
		})
	}
}

// The same lookup on the retarget path: a stopped router must keep the exit it
// failed over to.
func TestTick_StopDuringTheRetargetPlatformLookupKeepsTheExit(t *testing.T) {
	cfg := baseCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg: cfg,
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
			{ID: "ovpnc2", Iface: "tun12", Connected: true},
			{ID: "wgc1", Iface: "wgc1", Connected: true},
		}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.FallbackReady = func(string) bool { return false }
	w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
		stopped = true
		return f.plat, nil
	}
	w.Tick(context.Background()) // not ready: the retry clock starts
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; a stopped router was moved to another exit", f.cfg.Xray.Failover)
	}
}

// The subscription download blocks for as long as the host takes, and what
// follows it writes servers.json and xray.servers. A /stop finishing meanwhile
// ends the wave, and the wave that never happened does not spend its window.
func TestTick_StopDuringTheSubscriptionFetchWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	stopped := false
	saves := 0
	fetches := 0
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.SaveServers = func([]vpnconfig.Server) error {
		saves++
		return nil
	}
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		stopped = true
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
	}
	w.Generate = f.generateAll

	w.Tick(context.Background())

	if saves != 0 || len(f.cfg.Xray.Servers) != 0 {
		t.Fatalf("saves %d, xray.servers %v written after /stop", saves, f.cfg.Xray.Servers)
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}

	stopped = false
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d; an aborted wave must not spend the import window", fetches)
	}
}

// The download and the resolution of every host behind it can take minutes. A
// /stop that lands meanwhile ends them: the tick's context ends with the stop,
// and nothing the fetch brings back is published.
func TestTick_AStopEndsAFetchThatIsStillRunning(t *testing.T) {
	defer func(poll time.Duration) { stopPoll = poll }(stopPoll)
	stopPoll = time.Millisecond
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	var stopped atomic.Bool
	saves := 0
	w := runningWatch(f.watch())
	w.Stopped = stopped.Load
	w.SaveServers = func([]vpnconfig.Server) error {
		saves++
		return nil
	}
	w.Generate = f.generateAll
	w.Fetch = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		stopped.Store(true)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("the fetch ran on after /stop")
		}
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
	}

	w.Tick(context.Background())

	if saves != 0 || len(f.cfg.Xray.Servers) != 0 {
		t.Fatalf("saves %d, xray.servers %v written after /stop", saves, f.cfg.Xray.Servers)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

// A resolver that answers nothing costs seconds per host, and a subscription has
// dozens: the fetch has one deadline for all of it, or the watch - and every
// probe behind it - waits as long as the list is long.
func TestTick_TheFetchHasADeadline(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	var left time.Duration
	bounded := false
	w := runningWatch(f.watch())
	w.Fetch = func(ctx context.Context, _ string) ([]vpnconfig.Server, error) {
		var deadline time.Time
		deadline, bounded = ctx.Deadline()
		left = time.Until(deadline)
		return nil, errors.New("cdn down")
	}

	w.Tick(context.Background())

	if !bounded || left > FetchTimeout || left < FetchTimeout-time.Minute {
		t.Fatalf("fetch deadline in %v (set: %v), want about %v", left, bounded, FetchTimeout)
	}
}

// Generate can wait for the config lock, and a /stop can finish while it does.
// The write that follows would put a new config.json and active_server on a
// stopped router - taking effect on the next manual apply - so the marker is
// checked under that lock, where the write happens.
func TestTick_StopWhileGenerateWaitsForTheLockWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	stopped := false
	generated, restarts := 0, 0
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		stopped = true // lands while this call waits for the config lock
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated++
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if generated != 0 || restarts != 0 {
		t.Fatalf("generated %d, restarts %d after /stop", generated, restarts)
	}
	if got := vpnconfig.ActiveSeq(f.cfg.Xray.ActiveServer); got != 0 {
		t.Fatalf("active_server %+v rewritten on a stopped router", f.cfg.Xray.ActiveServer)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

// The same wait comes before the return to the preferred server after an
// all-dead walk: a stop that lands there leaves the walk's last write in place
// and writes nothing more.
func TestTick_StopWhileTheReturnToPreferredWaitsWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	stopped := false
	generated := []string{}
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if len(generated) == 2 {
			stopped = true // the return to Oslo waits for the lock
		}
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Oslo", "Backup"}) {
		t.Fatalf("generated %v; the return to the preferred server must not write after /stop", generated)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

// stopWhileWriting is an UpdateVPN whose write waits for the config lock while
// a /stop finishes: by the time the callback runs, the marker is there.
func stopWhileWriting(f *fake, stopped *bool) func(func(*vpnconfig.VPNDirectorConfig) error) error {
	return func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		*stopped = true
		return fn(f.cfg)
	}
}

// Every config write of the watch can wait for the lock, and the check made
// before that wait says nothing about the router after it. A write that lands
// on a stopped router takes effect on its next manual apply.
func TestTick_StopWhileTheRestoreStageWaitsWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.UpdateVPN = stopWhileWriting(f, &stopped)

	w.Tick(context.Background())

	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("staged onto Xray after /stop")
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("the failover record must stay as the stop found it")
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}
}

func TestTick_StopWhileTheMoveWaitsForTheLockStagesNothing(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := f.watch()
	w.Stopped = func() bool { return stopped }
	w.UpdateVPN = stopWhileWriting(f, &stopped)

	tickUntilDead(w, f)

	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v staged after /stop", f.cfg.Xray.Failover)
	}
	if f.applies != 0 || len(f.notes) != 0 {
		t.Fatalf("applies %d, notes %v after /stop", f.applies, f.notes)
	}
}

func TestTick_StopWhileTheCommitWaitsKeepsTheStage(t *testing.T) {
	cfg := baseCfg()
	vpnconfig.StageXrayClientsToTunnel(cfg, "ovpnc2")
	f := &fake{
		cfg:      cfg,
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	stopped := false
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.UpdateVPN = stopWhileWriting(f, &stopped)

	w.Tick(context.Background())

	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("committed the failover after /stop")
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

func TestTick_StopWhileThePublicationWaitsWritesNothing(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	stopped := false
	saves := 0
	w := runningWatch(f.watch())
	w.Stopped = func() bool { return stopped }
	w.UpdateVPN = stopWhileWriting(f, &stopped)
	w.SaveServers = func([]vpnconfig.Server) error {
		saves++
		return nil
	}
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10"}}}, nil
	}
	w.Generate = f.generateAll

	w.Tick(context.Background())

	if saves != 0 || len(f.cfg.Xray.Servers) != 0 {
		t.Fatalf("saves %d, xray.servers %v published after /stop", saves, f.cfg.Xray.Servers)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v after /stop", f.notes)
	}
}

func TestTick_PlatformErrorDoesNotAnnounceNoTunnel(t *testing.T) {
	f := &fake{
		cfg:      baseCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	w := f.watch()
	w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
		return vpnconfig.PlatformInfo{}, errors.New("rci timeout")
	}
	tickUntilDead(w, f)
	for _, n := range f.notes {
		if strings.Contains(n, "no Tunnel Director fallback") {
			t.Fatalf("platform error must not look like no tunnel: %v", f.notes)
		}
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must not stage without platform tunnels")
	}
}

func TestTick_WalkStopsWhenContextCanceled(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	generated := 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated++
		cancel()
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error { return errProbe }
	w.Tick(ctx)
	if generated != 1 {
		t.Fatalf("generate %d; canceled walk must not continue", generated)
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

// import_server_list.sh clears the saved link for a list from a file or a
// plain-http link - an import a user makes over SSH while Xray is down. The
// watch went idle with the link, and the failover it left behind kept its
// clients on the tunnel for good.
func TestTick_AFailoverOutlivesItsLinkAndStillRestores(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.SubscriptionURL = ""
	w := runningWatch(f.watch())

	w.Tick(context.Background())

	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v; a live outbound ends it, link or no link", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") || contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v, ovpnc2 %v", f.cfg.Xray.Clients, f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// A failover with no link still follows its tunnel: gone for a minute with no
// other exit, the clients go back to Xray as they do with one, and are told.
// The message came from the next tick's death path, which a watch without a
// link never reaches.
func TestTick_AFailoverWithoutALinkStillLeavesATunnelThatWentDown(t *testing.T) {
	f := &fake{cfg: committedCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.SubscriptionURL = ""
	f.plat = vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: false}}}
	w := runningWatch(f.watch())

	tickFor(w, f, time.Minute)

	if f.cfg.Xray.Failover != nil || !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
	if n := countNotes(f.notes, "Xray outbound is down; no Tunnel Director fallback"); n != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// With no link there is nothing to refresh: a dead outbound neither downloads
// "" nor reports a refresh that failed, and the failover goes on.
func TestTick_AFailoverWithoutALinkRefreshesNothing(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.SubscriptionURL = ""
	w := runningWatch(f.watch())
	fetches := 0
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("unsupported protocol scheme")
	}

	tickFor(w, f, 2*ImportRetry)

	if fetches != 0 {
		t.Fatalf("fetches %d with no saved link", fetches)
	}
	if n := countNotes(f.notes, "Subscription refresh failed"); n != 0 {
		t.Fatalf("notes %v", f.notes)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("the failover ended while the outbound is down and its tunnel up")
	}
}

// A restore whose last apply failed is retried on the next tick. The JSON
// already says restored, so nothing but a hook or the daily update would apply
// it otherwise, and a retry that loses TPROXY is the one that puts the clients
// back on the tunnel: a link cleared in between must not end it.
func TestTick_ARestoreLeftPendingIsFinishedWithoutALink(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.Apply = func() error {
		f.applies++
		if f.applies == 2 {
			return errApply
		}
		return nil
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil || countNotes(f.notes, "LAN clients back on Xray") != 0 {
		t.Fatalf("failover %+v, notes %v; the restore's last apply failed", f.cfg.Xray.Failover, f.notes)
	}

	f.cfg.Xray.SubscriptionURL = ""
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())

	if f.applies != 3 {
		t.Fatalf("applies %d; the failed apply was not retried", f.applies)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray"); n != 1 {
		t.Fatalf("notes %v", f.notes)
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = true
		return true, f.seq(), nil
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = true
		return true, f.seq(), nil
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

func TestTick_RestoreSkipsClientRemovedFromTunnelDuringFailover(t *testing.T) {
	cfg := failedOverCfg()
	tun := cfg.TunnelDirector.Tunnels["ovpnc2"]
	kept := make([]string, 0)
	for _, ip := range tun.Clients {
		if ip != "192.168.1.8" {
			kept = append(kept, ip)
		}
	}
	tun.Clients = kept
	cfg.TunnelDirector.Tunnels["ovpnc2"] = tun
	f := &fake{cfg: cfg, probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	runningWatch(f.watch()).Tick(context.Background())
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("deleted client must not return to Xray")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("must not put a deleted client back on the tunnel")
	}
}

func TestTick_RestoreSkipsSnapshotWhenFallbackTunnelKeyIsGone(t *testing.T) {
	cfg := failedOverCfg()
	delete(cfg.TunnelDirector.Tunnels, "ovpnc2")
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.8"}}
	f := &fake{cfg: cfg, probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	runningWatch(f.watch()).Tick(context.Background())
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("must not restore onto Xray after the wizard moved the client")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["wgc1"].Clients, "192.168.1.8") {
		t.Fatal("wizard assignment")
	}
}

func TestTick_DoesNotReapplyWhileTPROXYIsNotReady(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return false }
	w.Tick(context.Background())
	n := f.applies
	if n == 0 {
		t.Fatal("first restore must try apply")
	}
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.applies != n {
		t.Fatalf("applies %d after %d; waiting for TPROXY must not re-apply every tick", f.applies, n)
	}
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if f.applies == n {
		t.Fatal("must retry apply on the import cadence")
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
		t.Fatal("must not hand the client to TPROXY while the marker is missing: the PREROUTING jumps go in even when the platform's own rules do not, and Xray beats TUN_DIR, so the tunnel it is still on would carry nothing")
	}
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("must not announce restore: %v", f.notes)
		}
	}
}

// And once the marker is there, the same restore goes through in one tick: the
// clients are staged onto Xray, the fallback membership is dropped and the move
// back is announced once.
func TestTick_RestoreStagesOnceTPROXYIsReady(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	ready := false
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }

	w.Tick(context.Background())
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("staged onto Xray while the marker was missing")
	}

	ready = true
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v; the restore must finish once TPROXY is intercepting", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("the client must be back on Xray")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("fallback membership must be dropped once the clients are on Xray")
	}
	restored := 0
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			restored++
		}
	}
	if restored != 1 {
		t.Fatalf("notes %v, want one restore announcement", f.notes)
	}
}

// The apply that drops the fallback membership is also the one that has to keep
// TPROXY up. When it soft-fails - exit 0, marker gone - the clients have neither
// the proxy nor the tunnel, and after a finished restore nothing would try
// again: the failover goes back in, committed, until TPROXY can carry them.
func TestTick_RestoreReinstatesTheFailoverWhenTheFinalApplyLosesTPROXY(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: nil, now: time.Unix(1_700_000_000, 0)}
	ready := true
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }
	w.Apply = func() error {
		f.applies++
		if f.applies == 2 {
			ready = false // the apply after the membership drop loses TPROXY
		}
		return nil
	}

	w.Tick(context.Background())

	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; a restore that lost TPROXY must go back onto the fallback tunnel", f.cfg.Xray.Failover)
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("the client must leave xray.clients while TPROXY cannot carry it, or TUN_DIR never sees it")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must be back on the fallback tunnel")
	}
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			t.Fatalf("announced a restore that did not hold: %v", f.notes)
		}
	}

	// TPROXY comes back: the next attempt finishes the restore and says so once.
	ready = true
	w.Apply = func() error {
		f.applies++
		return nil
	}
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil || !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; the restore must finish once TPROXY is back", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
	restored := 0
	for _, n := range f.notes {
		if strings.HasPrefix(n, "LAN clients back on Xray") {
			restored++
		}
	}
	if restored != 1 {
		t.Fatalf("notes %v, want one restore announcement", f.notes)
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		if s.Name == "Oslo" {
			f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
		}
		return true, f.seq(), nil
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

func TestTick_WalkAbandonsWhenUserReselectsStartedServer(t *testing.T) {
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
			{Name: "Extra", Address: "extra.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) == 0 {
			return errProbe
		}
		last := generated[len(generated)-1]
		if last == "Backup" {
			f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
			return errProbe
		}
		if last == "Oslo" {
			return errProbe
		}
		return nil
	}
	w.Tick(context.Background())
	if len(generated) != 2 || generated[0] != "Oslo" || generated[1] != "Backup" {
		t.Fatalf("generate %v; must not overwrite a re-selection of the original server", generated)
	}
	if f.cfg.Xray.ActiveServer == nil || f.cfg.Xray.ActiveServer.Name != "Oslo" {
		t.Fatalf("active %+v", f.cfg.Xray.ActiveServer)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("abandoned walk must not restore")
	}
}

// A Web UI or /xray selection that commits after the walk last read the config,
// but before Generate takes the config lock, is the one the walk cannot see from
// outside that lock. Written over, it is gone: active_server then names the
// walk's own server and no later look can tell.
// Picking the server that is already running is the one selection whose name,
// address and port are the ones the walk started from. Only the record's write
// counter tells it from no selection at all, and the walk must still stand
// down: the user asked for this server, not for the next candidate.
func TestTick_WalkAbandonsWhenTheActiveServerIsSelectedAgain(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		plat:     vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	running := &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4}
	f.cfg.Xray.ActiveServer = running
	generated, restarts := []string{}, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		// The user re-selects the running server while the download is out.
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(running, vpnconfig.Server{
			Name: "Oslo", Address: "oslo.example", Port: 443,
		})
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if len(generated) != 0 || restarts != 0 {
		t.Fatalf("generated %v, restarts %d over a re-selection of the running server", generated, restarts)
	}
	if got := vpnconfig.ActiveSeq(f.cfg.Xray.ActiveServer); got != 5 {
		t.Fatalf("active_server %+v, want the user's own write", f.cfg.Xray.ActiveServer)
	}
}

func TestTick_WalkDoesNotOverwriteASelectionMadeJustBeforeGenerate(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	manual := &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
	generated, restarts := []string{}, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		f.cfg.Xray.ActiveServer = manual // commits while Generate waits for the lock
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) == 0 {
			return errProbe
		}
		return nil
	}

	w.Tick(context.Background())

	if len(generated) != 0 {
		t.Fatalf("generated %v over the newer selection", generated)
	}
	if f.cfg.Xray.ActiveServer != manual {
		t.Fatalf("active_server %+v, want the manual selection", f.cfg.Xray.ActiveServer)
	}
	if restarts != 0 {
		t.Fatalf("restarts %d; Xray must keep the manual selection", restarts)
	}
	if f.cfg.Xray.Failover == nil {
		t.Fatal("abandoned walk must not restore")
	}
}

// Every server is dead, and the selection lands just before the walk writes the
// preferred server back: the same window, on the last Generate of the walk.
func TestTick_NoLiveWalkDoesNotReturnOverASelectionMadeJustBeforeGenerate(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	manual := &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
	generated, restarts := []string{}, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if len(generated) == 2 {
			f.cfg.Xray.ActiveServer = manual // commits while the return to Oslo waits for the lock
		}
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Oslo", "Backup"}) {
		t.Fatalf("generated %v, want the two walked servers and no return over the selection", generated)
	}
	if f.cfg.Xray.ActiveServer != manual {
		t.Fatalf("active_server %+v, want the manual selection", f.cfg.Xray.ActiveServer)
	}
	if restarts != 2 {
		t.Fatalf("restarts %d, want one per walked server and none after the selection", restarts)
	}
}

// Generate reports a written config.json whose active_server record failed to
// save with generated=true and an error; the file still names the previous
// server. That is the walk's own state, not a newer selection, so the walk goes
// on to the next candidate.
func TestTick_WalkContinuesPastACandidateWhoseRecordWasNotSaved(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Backup", Address: "backup.example", Port: 443},
			{Name: "Extra", Address: "extra.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		if s.Name == "Backup" {
			return true, f.seq(), errSaveConfig
		}
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) > 0 && generated[len(generated)-1] == "Extra" {
			return nil
		}
		return errProbe
	}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Backup", "Extra"}) {
		t.Fatalf("generated %v; an unsaved record read as a newer selection", generated)
	}
	if f.cfg.Xray.Failover != nil {
		t.Fatal("the live Extra must restore the clients")
	}
}

// The same record failure on the candidate whose probe succeeds: the check
// before the restore must not take the previous record for a newer selection.
func TestTick_WalkRestoresOnACandidateWhoseRecordWasNotSaved(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Backup", Address: "backup.example", Port: 443}}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		return true, f.seq(), errSaveConfig
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) == 0 {
			return errProbe
		}
		return nil
	}

	w.Tick(context.Background())

	if f.cfg.Xray.Failover != nil {
		t.Fatalf("generated %v; the live Backup must restore the clients", generated)
	}
}

// All candidates dead, the last one's record unsaved: the walk still returns
// config.json to the preferred server instead of abandoning over its own write.
func TestTick_NoLiveWalkReturnsToPreferredAfterAnUnsavedRecord(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		if s.Name == "Backup" {
			return true, f.seq(), errSaveConfig
		}
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Oslo", "Backup", "Oslo"}) {
		t.Fatalf("generated %v, want the walk and then the return to Oslo", generated)
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

// selectManual is what a Web UI or /xray selection leaves in the config:
// another server named, and the write counter moved on.
func selectManual(f *fake) {
	f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, vpnconfig.Server{Name: "Manual", Address: "manual.example", Port: 443})
}

// selectOnWrite is an UpdateVPN whose first write after the walk picked a
// server waits for the config lock while a selection commits: by the time the
// callback runs, active_server names the user's server.
func selectOnWrite(f *fake) func(func(*vpnconfig.VPNDirectorConfig) error) error {
	selected := false
	return func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		if f.picked && !selected {
			selected = true
			selectManual(f)
		}
		return fn(f.cfg)
	}
}

// selectDuringTheStageApply has a selection commit while the apply after the
// restore's stage runs - the first apply with the client back in xray.clients.
func selectDuringTheStageApply(w *Watch, f *fake) {
	selected := false
	apply := w.Apply
	w.Apply = func() error {
		if !selected && contains(f.cfg.Xray.Clients, "192.168.1.8") {
			selected = true
			selectManual(f)
		}
		return apply()
	}
}

// The walk looked at active_server before the restore, and the restore wrote
// without looking again. A selection that committed while its stage waited for
// the config lock put another server in place of the one the walk had just
// probed - one still starting, or dead - and the restore took the clients off a
// working tunnel onto it, announcing the walk's server.
func TestTick_ASelectionWhileTheRestoreWaitsKeepsTheClientsOnTheTunnel(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(liveImportWatch(f))
	w.UpdateVPN = selectOnWrite(f)

	w.Tick(context.Background())

	if f.cfg.Xray.Failover == nil || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; restored onto a server nobody probed", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must stay on the tunnel")
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v", f.notes)
	}
}

// The selection can land between the restore's two writes as well, while the
// apply of the stage runs. The clients the stage handed back to Xray leave it
// again; the next tick probes the server that runs now.
func TestTick_ASelectionBetweenTheRestoreWritesTakesTheClientsOffXrayAgain(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(liveImportWatch(f))
	selectDuringTheStageApply(w, f)

	w.Tick(context.Background())

	if f.cfg.Xray.Failover == nil || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; restored onto a server nobody probed", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must stay on the tunnel")
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v", f.notes)
	}
}

// The tick's own restore had the same gap: its probe tested the server
// active_server named when the tick read the config. A selection since is the
// next tick's to probe, and to restore on.
func TestTick_ASelectionAfterTheProbeIsTheNextTicksToRestoreOn(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	selectDuringTheStageApply(w, f)

	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || contains(f.cfg.Xray.Clients, "192.168.1.8") || len(f.notes) != 0 {
		t.Fatalf("failover %+v, xray.clients %v, notes %v; restored on the probe of the server before", f.cfg.Xray.Failover, f.cfg.Xray.Clients, f.notes)
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil || !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; the next probe is of the server selected", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
	if countNotes(f.notes, "LAN clients back on Xray; server Manual") != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

func TestPickOrder_SameNameFirst(t *testing.T) {
	in := []vpnconfig.Server{
		{Name: "A", Address: "1.example"},
		{Name: "Oslo", Address: "2.example"},
		{Name: "B", Address: "3.example"},
	}
	got := pickOrder(in, &vpnconfig.ActiveServer{Name: "Oslo"})
	if got[0].Name != "Oslo" || got[1].Name != "A" || got[2].Name != "B" {
		t.Fatalf("%v", got)
	}
	got = pickOrder(in, &vpnconfig.ActiveServer{Name: "Oslo", Address: "2.example"})
	if got[0].Address != "2.example" {
		t.Fatal("match address when recorded")
	}
	got = pickOrder(in, &vpnconfig.ActiveServer{Name: "missing"})
	if got[0].Name != "A" {
		t.Fatal("keep list order")
	}
	// A subscription that rotates endpoints moves a name to a new address: the
	// name is what the user chose.
	got = pickOrder(in, &vpnconfig.ActiveServer{Name: "Oslo", Address: "9.example", Port: 443})
	if got[0].Name != "Oslo" || got[1].Name != "A" || got[2].Name != "B" {
		t.Fatalf("%v; a recorded address the list no longer has must still put the name first", got)
	}
	two := []vpnconfig.Server{
		{Name: "Oslo", Address: "a.example", Port: 443},
		{Name: "Oslo", Address: "b.example", Port: 443},
	}
	got = pickOrder(two, &vpnconfig.ActiveServer{Name: "Oslo", Address: "b.example", Port: 443})
	if got[0].Address != "b.example" {
		t.Fatalf("%v; the entry the record names beats an earlier one of the same name", got)
	}
}

// The walk starts from the name the user chose at the address it has today, and
// an all-dead wave returns config.json to that server, not to the last one tried.
func TestTick_TheChosenNameIsTriedFirstAtItsNewAddress(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443}
	servers := []vpnconfig.Server{
		{Name: "Amsterdam", Address: "amsterdam.example", Port: 443},
		{Name: "Oslo", Address: "oslo-7.example", Port: 443},
		{Name: "Berlin", Address: "berlin.example", Port: 443},
	}
	var events []string
	w := runningWatch(recordingWalkWatch(f, servers, allGenerate, &events))

	w.Tick(context.Background())

	want := []string{"Oslo", "restart", "Amsterdam", "restart", "Berlin", "restart", "Oslo", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}

// A walk cut short - the bot restarted, a /stop - leaves active_server on a
// server it was only trying. The user's choice is kept beside it, and the next
// wave starts from that one and returns to it, not to the server that happened
// to be tried last.
func TestTick_AnInterruptedWalkStartsAgainFromTheUsersChoice(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Backup", Address: "backup.example", Port: 443}
	f.cfg.Xray.PreferredServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	servers := []vpnconfig.Server{
		{Name: "Backup", Address: "backup.example", Port: 443},
		{Name: "Paris", Address: "paris.example", Port: 443},
		{Name: "Oslo", Address: "oslo.example", Port: 443},
	}
	var events []string
	w := runningWatch(recordingWalkWatch(f, servers, allGenerate, &events))

	w.Tick(context.Background())

	want := []string{"Oslo", "restart", "Backup", "restart", "Paris", "restart", "Oslo", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v", events, want)
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		got = s
		f.picked = true
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		liveAfter = s.Name
		return true, f.seq(), nil
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		liveAfter = s.Name
		f.cfg.Xray.ActiveServer = vpnconfig.NewActiveServer(s)
		return true, f.seq(), nil
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
	w.Generate = f.generateAll
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
	w.Generate = f.generateAll
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = true
		return true, f.seq(), nil
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		justGenerated = true
		return true, f.seq(), nil
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		f.picked = true
		return true, f.seq(), nil
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
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		*events = append(*events, s.Name)
		if !generates(s) {
			return false, f.seq(), errors.New("rejected")
		}
		f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: s.Name}
		return true, f.seq(), nil
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
	w.Generate = f.generateAll
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
	w.Generate = f.generateAll
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
	w.Generate = f.generateAll
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generates++
		f.picked = true
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		return nil
	}
	w.Tick(context.Background())
	assertStagedOnTunnel(t, f.cfg)
	if fetches != 1 || generates != 1 || restarts != 1 {
		t.Fatalf("first tick fetches=%d generates=%d restarts=%d", fetches, generates, restarts)
	}
	if w.lastImport.IsZero() {
		t.Fatal("lastImport must stay set after failed restore-Apply")
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	assertStagedOnTunnel(t, f.cfg)
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
	assertStagedOnTunnel(t, f.cfg)
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

// A restore that did not hold goes back on the tunnel with what it moved and
// nothing else. A client that is on Xray while Xray is healthy - added after
// the outbound came back - belongs there. (One added while Xray is still down
// joins the failover instead; see TestTick_XrayClientsAddedDuringAFailoverJoinIt.)
func TestTick_RestoreThatDidNotHoldDoesNotMoveUnrelatedXrayClients(t *testing.T) {
	f := &fake{
		cfg:  failedOverCfg(),
		plat: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: true}}},
		now:  time.Unix(1_700_000_000, 0),
	}
	f.cfg.Xray.Clients = append(f.cfg.Xray.Clients, "192.168.1.10")
	ready := true
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }
	w.Apply = func() error {
		f.applies++
		if f.applies == 2 {
			ready = false // the apply that drops the tunnel membership loses TPROXY
		}
		return nil
	}
	w.Tick(context.Background())

	if f.cfg.Xray.Failover == nil || !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatalf("failover %+v; the restore goes back on the tunnel", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.10") {
		t.Fatal("a client on Xray while Xray is healthy stays there")
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.10") {
		t.Fatal("the write-back must not move the unrelated Xray client onto the tunnel")
	}
	if contains(f.cfg.Xray.Failover.Clients, "192.168.1.10") {
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
	assertStagedOnTunnel(t, f.cfg)
	if f.applies != 1 || probes != 1 {
		t.Fatalf("first tick applies=%d probes=%d", f.applies, probes)
	}

	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	assertStagedOnTunnel(t, f.cfg)
	if f.applies != 2 {
		t.Fatalf("applies %d, want the staged restore Apply retried", f.applies)
	}

	f.applyErr = nil
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatal("must restore once Apply succeeds")
	}
	if n := len(f.notes); n == 0 || f.notes[n-1] != "LAN clients back on Xray; server Oslo" {
		t.Fatalf("notes %v", f.notes)
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
	assertStagedOnTunnel(t, f.cfg)

	events = nil
	f.applyErr = nil
	f.now = f.now.Add(ImportRetry)
	w.Tick(context.Background())
	if want := []string{"apply", "apply"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v (stage apply, then drop tunnel after TPROXY)", events, want)
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
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generates++
		return true, f.seq(), nil
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
	if len(f.notes) != 1 || f.notes[0] != "No live server in the subscription" {
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

// The Web UI, /import and this wave all publish a list and the bypass IPs read
// against it. Writing servers.json outside the config lock lets two waves
// interleave into one file from each.
func TestTick_ImportPublishesServersUnderTheConfigLock(t *testing.T) {
	f := &fake{
		cfg:      failedOverCfg(),
		probeErr: errProbe,
		now:      time.Unix(1_700_000_000, 0),
	}
	inUpdate := false
	savedUnderLock := false
	w := runningWatch(f.watch())
	w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		inUpdate = true
		defer func() { inUpdate = false }()
		return fn(f.cfg)
	}
	w.SaveServers = func([]vpnconfig.Server) error {
		savedUnderLock = inUpdate
		return nil
	}
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443, IPs: []string{"203.0.113.10"}}}, nil
	}

	w.Tick(context.Background())

	if !savedUnderLock {
		t.Fatal("servers.json must be written inside the config update the watch takes the lock with")
	}
}

// The download can take longer than it takes someone to paste a new
// subscription into the Web UI. Publishing this list then leaves a servers.json
// from the old link beside the new one that is now saved, and the walk picks its
// server out of it.
func TestTick_ImportDoesNotPublishAfterTheSubscriptionChanged(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	saves, fetches, generates := 0, 0, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error {
		saves++
		return nil
	}
	w.Generate = func(vpnconfig.Server, func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		generates++
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		// A Web UI import saves another subscription while this one is in flight.
		f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/other"
		return []vpnconfig.Server{{Name: "Oslo", Address: "old.example", Port: 443, IPs: []string{"203.0.113.10"}}}, nil
	}

	w.Tick(context.Background())

	if saves != 0 || len(f.cfg.Xray.Servers) != 0 {
		t.Fatalf("saves %d, xray.servers %v; a list from the old subscription must not be published", saves, f.cfg.Xray.Servers)
	}
	if generates != 0 {
		t.Fatalf("generates %d; the walk must not run on a list the config no longer asks for", generates)
	}

	// The link that replaced it deserves a wave of its own, not the wait left
	// over from the one that was thrown away.
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d; the abandoned wave must not spend the import window", fetches)
	}
}

// The same change landing once the walk is under way: the list it walks came from
// a link that is no longer saved, and every server it goes on to write is one the
// new subscription may not have. The walk ends where it is, writes and announces
// nothing more, and the new link gets a wave of its own at once.
func TestTick_WalkAbandonsWhenTheSavedLinkChanges(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated, restarts, fetches := []string{}, 0, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error {
		restarts++
		if restarts == 1 {
			// The Web UI imports another subscription while Oslo is being tried.
			f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/other"
		}
		return nil
	}
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Oslo"}) {
		t.Fatalf("generated %v; nothing from the old list may follow the new link", generated)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v; an abandoned walk announces nothing", f.notes)
	}

	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d; the new link must not wait out the abandoned wave's window", fetches)
	}
}

// The same link saved just before the walk writes the preferred server back: the
// return is refused under the lock, the all-dead wave announces nothing, and the
// new link gets its wave at once rather than the backed-off one.
func TestTick_NoLiveWalkDoesNotReturnAfterTheLinkChanged(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated, fetches := []string{}, 0
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if fetches == 1 && len(generated) == 2 {
			// Saved while the return to Oslo waits for the lock.
			f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/other"
		}
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if !reflect.DeepEqual(generated, []string{"Oslo", "Backup"}) {
		t.Fatalf("generated %v; the return must not write after the link changed", generated)
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v; an abandoned walk announces nothing", f.notes)
	}
	w.Tick(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches %d; the new link must not wait out the backoff of the abandoned wave", fetches)
	}
}

// And while the probe of a candidate succeeds: the server is one of the old list,
// and the walk that found it ends without the restore. The next tick's health
// probe finds the live outbound either way.
func TestTick_WalkDoesNotRestoreOnTheOldListAfterTheLinkChanged(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Backup", Address: "backup.example", Port: 443}}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if len(generated) == 0 {
			return errProbe
		}
		f.cfg.Xray.SubscriptionURL = "https://cdn.example/s/other"
		return nil
	}

	w.Tick(context.Background())

	if f.cfg.Xray.Failover == nil {
		t.Fatal("the walk restored on a server of a link that is no longer saved")
	}
	if len(f.notes) != 0 {
		t.Fatalf("notes %v; an abandoned walk announces nothing", f.notes)
	}
}

// A selection that lands after Generate released the config lock, before the
// walk can look at the config again, used to be adopted as the walk's own
// write: the next candidate then matched both the identity and the counter, and
// the user's choice was overwritten. The counter the write itself reports
// cannot be overtaken that way.
func TestTick_WalkAbandonsWhenASelectionLandsRightAfterGenerate(t *testing.T) {
	f := &fake{cfg: failedOverCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4}
	generated := []string{}
	w := runningWatch(f.watch())
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{
			{Name: "Oslo", Address: "oslo.example", Port: 443},
			{Name: "Backup", Address: "backup.example", Port: 443},
		}, nil
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = append(generated, s.Name)
		f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		written := f.seq()
		if len(generated) == 1 {
			// The user picks that same server again, once the lock is gone.
			f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(f.cfg.Xray.ActiveServer, s)
		}
		return true, written, nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}

	w.Tick(context.Background())

	if len(generated) != 1 {
		t.Fatalf("generated %v; the walk must stand down after a write it did not make", generated)
	}
	if got := vpnconfig.ActiveSeq(f.cfg.Xray.ActiveServer); got != 6 {
		t.Fatalf("active_server %+v, want the user's own write", f.cfg.Xray.ActiveServer)
	}
}

func TestTick_SyncXrayServersFailureStopsTheWave(t *testing.T) {
	f := &fake{
		cfg: failedOverCfg(),
		now: time.Unix(1_700_000_000, 0),
	}
	generates, restarts := 0, 0
	w := runningWatch(liveImportWatch(f))
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generates++
		return true, f.seq(), nil
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

// committedCfg is a failover as this build commits it: 192.168.1.8 moved onto
// ovpnc2 and off Xray, with the record saying so. 192.168.1.9 is paused.
func committedCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	vpnconfig.MoveXrayClientsToTunnel(cfg, "ovpnc2")
	return cfg
}

func connected(ids ...string) vpnconfig.PlatformInfo {
	var p vpnconfig.PlatformInfo
	for _, id := range ids {
		p.Tunnels = append(p.Tunnels, vpnconfig.PlatformTunnel{ID: id, Iface: "if-" + id, Connected: true})
	}
	return p
}

func countNotes(notes []string, prefix string) int {
	n := 0
	for _, s := range notes {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// A client TUN_DIR cannot mark stays on Xray: dropped from it onto a tunnel
// that does not mark it, it would leave through the WAN.
func TestTick_StageLeavesWhatTheTunnelCannotCarryOnXray(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.Clients = []string{"192.168.1.8", "100.64.0.8"}
	tickUntilDead(f.watch(), f)
	if f.cfg.Xray.Failover == nil || !reflect.DeepEqual(f.cfg.Xray.Failover.Clients, []string{"192.168.1.8"}) {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "100.64.0.8") {
		t.Fatalf("xray.clients %v", f.cfg.Xray.Clients)
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "100.64.0.8") {
		t.Fatal("an address the tunnel cannot carry must not be put on it")
	}
}

// With nothing the tunnel can carry there is nothing to move - the same as no
// fallback at all, and the subscription is still refreshed.
func TestTick_NothingTheTunnelCanCarryIsNoFallback(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.cfg.Xray.Clients = []string{"100.64.0.8"}
	fetches := 0
	w := f.watch()
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		fetches++
		return nil, errors.New("cdn down")
	}
	tickUntilDead(w, f)
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v for clients no tunnel carries", f.cfg.Xray.Failover)
	}
	if len(f.notes) == 0 || f.notes[0] != "Xray outbound is down; no Tunnel Director fallback" {
		t.Fatalf("notes %v", f.notes)
	}
	if fetches != 1 {
		t.Fatalf("fetches %d", fetches)
	}
}

// Xray clients added, re-added or resumed during a failover used to stay on the
// dead outbound for the rest of it: only the snapshot ever moved. They join the
// failover the way the snapshot did - onto the tunnel first, off Xray once
// TUN_DIR has them.
func TestTick_XrayClientsAddedDuringAFailoverJoinIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(cfg *vpnconfig.VPNDirectorConfig)
		addr  string
	}{
		{"added", func(cfg *vpnconfig.VPNDirectorConfig) { cfg.Xray.Clients = append(cfg.Xray.Clients, "192.168.1.20") }, "192.168.1.20"},
		{"resumed", func(cfg *vpnconfig.VPNDirectorConfig) { cfg.PausedClients = nil }, "192.168.1.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
			tc.setup(f.cfg)
			var xrayAtApply [][]string
			w := runningWatch(f.watch())
			apply := w.Apply
			w.Apply = func() error {
				xrayAtApply = append(xrayAtApply, append([]string(nil), f.cfg.Xray.Clients...))
				return apply()
			}
			w.Tick(context.Background())
			if contains(f.cfg.Xray.Clients, tc.addr) {
				t.Fatalf("xray.clients %v; still on the dead outbound", f.cfg.Xray.Clients)
			}
			if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, tc.addr) {
				t.Fatal("not on the fallback tunnel")
			}
			if f.cfg.Xray.Failover == nil || !contains(f.cfg.Xray.Failover.Clients, tc.addr) {
				t.Fatalf("failover %+v; the restore must bring it back to Xray", f.cfg.Xray.Failover)
			}
			if len(xrayAtApply) != 2 || !contains(xrayAtApply[0], tc.addr) || contains(xrayAtApply[1], tc.addr) {
				t.Fatalf("xray.clients at each apply %v; the tunnel first, then off Xray", xrayAtApply)
			}
		})
	}
}

// Once the clients are committed the watch did not look at the fallback again,
// and a tunnel that went down sent them out through the WAN while Xray stayed
// dead. Another exit takes them.
func TestTick_CommittedFailoverMovesOnWhenItsTunnelStopsBeingAnExit(t *testing.T) {
	cfg := committedCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.plat = connected("wgc1")
	f.plat.Tunnels = append(f.plat.Tunnels, vpnconfig.PlatformTunnel{ID: "ovpnc2", Iface: "tun12", Connected: false})
	w := runningWatch(f.watch())

	tickFor(w, f, time.Minute)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "wgc1" {
		t.Fatalf("failover %+v, want the clients moving to wgc1", f.cfg.Xray.Failover)
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must leave the tunnel that went down")
	}

	w.Tick(context.Background())
	if contains(f.cfg.Xray.Clients, "192.168.1.8") || !contains(f.cfg.TunnelDirector.Tunnels["wgc1"].Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v, wgc1 %v; committed on wgc1", f.cfg.Xray.Clients, f.cfg.TunnelDirector.Tunnels["wgc1"].Clients)
	}
	if countNotes(f.notes, "Xray outbound is down; LAN clients moved to tunnel:wgc1") != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// With no exit left the clients go back to Xray: a dead outbound takes them
// nowhere, a dead tunnel took them out through the WAN. The same as a death
// with no fallback, and announced like one.
func TestTick_CommittedFailoverGoesBackToXrayWhenNoExitIsLeft(t *testing.T) {
	f := &fake{cfg: committedCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	f.plat = vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "ovpnc2", Iface: "tun12", Connected: false}}}
	w := runningWatch(f.watch())

	tickFor(w, f, time.Minute)
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v on a tunnel that is down", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v", f.cfg.Xray.Clients)
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must leave the tunnel that went down")
	}

	tickFor(w, f, DeadAfter)
	if n := len(f.notes); n == 0 || f.notes[n-1] != "Xray outbound is down; no Tunnel Director fallback" {
		t.Fatalf("notes %v", f.notes)
	}
}

// A tunnel reconnecting shows as down for a moment; one look does not move
// anybody.
func TestTick_AFallbackBackWithinAMinuteKeepsTheClients(t *testing.T) {
	cfg := committedCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	up := connected("ovpnc2", "wgc1")
	down := connected("wgc1")
	f.plat = down
	w := runningWatch(f.watch())
	w.Tick(context.Background())
	f.plat = up
	tickFor(w, f, 5*time.Minute)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; one look at a reconnecting tunnel moved the clients", f.cfg.Xray.Failover)
	}
}

// No tunnel list is no answer: on Keenetic "vpn-director.sh platform" prints an
// empty list while RCI does not reply.
func TestTick_AnEmptyPlatformAnswerIsNotADeadFallback(t *testing.T) {
	f := &fake{cfg: committedCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	tickFor(w, f, 10*time.Minute)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
}

// The platform listing its tunnel was all a committed failover was checked for.
// A firewall restart can take the rules that send the clients into the tunnel
// while it stays connected, and an apply that cannot put them back withholds
// failover_ready: off Xray, the clients left through the WAN until Xray came
// back. Another exit takes them.
func TestTick_CommittedFailoverMovesOnWhenItsTunnelStopsCarrying(t *testing.T) {
	cfg := committedCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, plat: connected("ovpnc2", "wgc1"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.FallbackReady = func(id string) bool { return id == "wgc1" }

	tickFor(w, f, 2*time.Minute)

	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "wgc1" {
		t.Fatalf("failover %+v, want the clients on wgc1", f.cfg.Xray.Failover)
	}
	if contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("the client must leave the tunnel that does not carry it")
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") || !contains(f.cfg.TunnelDirector.Tunnels["wgc1"].Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v, wgc1 %v; committed on wgc1", f.cfg.Xray.Clients, f.cfg.TunnelDirector.Tunnels["wgc1"].Clients)
	}
	if countNotes(f.notes, "Xray outbound is down; LAN clients moved to tunnel:wgc1") != 1 {
		t.Fatalf("notes %v", f.notes)
	}
}

// Most of the time an apply is all it takes - tunnel_apply rebuilds a chain the
// firewall emptied - and the clients stay where they are.
func TestTick_AnApplyThatBringsTheFallbackBackKeepsTheClients(t *testing.T) {
	cfg := committedCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, plat: connected("ovpnc2", "wgc1"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	carried := false
	apply := w.Apply
	w.Apply = func() error {
		carried = true
		return apply()
	}
	w.FallbackReady = func(string) bool { return carried }

	tickFor(w, f, 5*time.Minute)

	if f.applies == 0 {
		t.Fatal("no apply: nothing put the rules back")
	}
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; the clients belong on ovpnc2", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
}

// tunnel_apply writes no failover_ready for a tunnel it has nobody to carry
// into - every failover client paused - and that says nothing about the tunnel.
func TestTick_AFailoverWithNobodyToCarryKeepsItsTunnel(t *testing.T) {
	cfg := committedCfg()
	cfg.PausedClients = append(cfg.PausedClients, "192.168.1.8")
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, plat: connected("ovpnc2", "wgc1"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.FallbackReady = func(string) bool { return false }

	tickFor(w, f, 5*time.Minute)

	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; moved for a marker nobody needed", f.cfg.Xray.Failover)
	}
}

func stagedOn(tunnel string, extra ...string) *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	for _, id := range extra {
		cfg.TunnelDirector.Tunnels[id] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	}
	vpnconfig.StageXrayClientsToTunnel(cfg, tunnel)
	return cfg
}

// The next exit used to be "the first one that is not this one": two exits that
// never became ready traded the clients back and forth, and a third that
// worked was never tried.
func TestTick_UnreadyFallbacksMoveOnToTheOneThatWorks(t *testing.T) {
	f := &fake{cfg: stagedOn("ovpnc1", "ovpnc1", "wgc1"), plat: connected("ovpnc1", "ovpnc2", "wgc1"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.FallbackReady = func(id string) bool { return id == "wgc1" }
	tickFor(w, f, 30*time.Minute)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "wgc1" || contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("failover %+v, xray.clients %v; the clients belong on wgc1", f.cfg.Xray.Failover, f.cfg.Xray.Clients)
	}
}

// Every move is a TUN_DIR rebuild - a window for every client of every tunnel.
// Once each exit has had its turn, the next round waits.
func TestTick_TwoUnreadyFallbacksAreNotTradedEveryFiveMinutes(t *testing.T) {
	f := &fake{cfg: stagedOn("ovpnc1", "ovpnc1"), plat: connected("ovpnc1", "ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.FallbackReady = func(string) bool { return false }
	moves, last := 0, f.cfg.Xray.Failover.Tunnel
	end := f.now.Add(2 * time.Hour)
	for !f.now.After(end) {
		w.Tick(context.Background())
		if f.cfg.Xray.Failover != nil && f.cfg.Xray.Failover.Tunnel != last {
			moves++
			last = f.cfg.Xray.Failover.Tunnel
		}
		f.now = f.now.Add(ProbeInterval)
	}
	if moves > 5 {
		t.Fatalf("%d moves in two hours", moves)
	}
	if moves == 0 {
		t.Fatal("the second exit was never tried")
	}
}

// A stage that was never committed kept its clients on Xray all along. When
// its restore does not hold it goes back to being that stage - committing it
// would move the clients onto a tunnel that never became ready.
func TestTick_AStagedRestoreThatLosesTPROXYGoesBackStaged(t *testing.T) {
	cfg := stagedOn("ovpnc2")
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	f := &fake{cfg: cfg, plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	ready := true
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }
	w.Apply = func() error {
		f.applies++
		if f.applies == 2 {
			ready = false
		}
		return nil
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Committed {
		t.Fatalf("failover %+v, want the stage back", f.cfg.Xray.Failover)
	}
	if !contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatal("a stage that never committed keeps its clients on Xray")
	}
	if !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatal("and on the tunnel")
	}
}

// The clients of a stage never left Xray, so its finished restore is no news.
// The retry of a failed last apply used to announce every restore.
func TestTick_AStagedRestoreWhoseLastApplyFailedIsNotAnnounced(t *testing.T) {
	cfg := stagedOn("ovpnc2")
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo"}
	f := &fake{cfg: cfg, plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	w.Apply = func() error {
		f.applies++
		if f.applies == 2 {
			return errApply
		}
		return nil
	}
	w.Tick(context.Background())
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray"); n != 0 {
		t.Fatalf("notes %v; nothing had left Xray", f.notes)
	}
}

// The retry of a failed last restore apply is the apply that has to keep TPROXY
// up. It exited 0 with the marker gone and was announced as a restore, with the
// clients on neither the proxy nor the tunnel and nothing left to try again.
func TestTick_ARetriedRestoreApplyThatLosesTPROXYGoesBackOnTheTunnel(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	ready := true
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }
	w.Apply = func() error {
		f.applies++
		switch f.applies {
		case 2:
			return errApply
		case 3:
			ready = false
		}
		return nil
	}
	w.Tick(context.Background())
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || !f.cfg.Xray.Failover.Committed {
		t.Fatalf("failover %+v, want it back, committed", f.cfg.Xray.Failover)
	}
	if contains(f.cfg.Xray.Clients, "192.168.1.8") || !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v, ovpnc2 %v", f.cfg.Xray.Clients, f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray"); n != 0 {
		t.Fatalf("notes %v; the restore did not hold", f.notes)
	}
}

// Staging the clients back hands them to TPROXY. When that apply loses the
// marker they were left there - intercepted by a TPROXY that cannot carry them,
// while the tunnel they are still on carries nothing, because Xray wins.
func TestTick_ARestoreStageThatLosesTPROXYTakesTheClientsOffXrayAgain(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	ready := true
	w := runningWatch(f.watch())
	w.TPROXYReady = func() bool { return ready }
	w.Apply = func() error {
		f.applies++
		if f.applies == 1 {
			ready = false
		}
		return nil
	}
	w.Tick(context.Background())
	if contains(f.cfg.Xray.Clients, "192.168.1.8") {
		t.Fatalf("xray.clients %v; staged onto a TPROXY that lost its marker", f.cfg.Xray.Clients)
	}
	if f.cfg.Xray.Failover == nil || !contains(f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.8") {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
}

// A restore whose write failed after the stage left the record looking staged,
// and "back on Xray" depended on a flag of that process: the clients came back
// without a word.
func TestTick_ARestoreWhoseWriteFailedIsStillAnnounced(t *testing.T) {
	f := &fake{cfg: committedCfg(), plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	w := runningWatch(f.watch())
	updates := 0
	w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		updates++
		if updates == 2 {
			return errors.New("config lock timeout")
		}
		return fn(f.cfg)
	}
	w.Tick(context.Background())
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if n := countNotes(f.notes, "LAN clients back on Xray; server Oslo"); n != 1 {
		t.Fatalf("notes %v, want one restore announcement", f.notes)
	}
}

// The same after a restart: the process that staged the restore is gone, and
// the file is what says the clients had left Xray.
func TestTick_ARestoreAnEarlierProcessStagedIsAnnounced(t *testing.T) {
	cfg := committedCfg()
	vpnconfig.EnsureFailoverStaged(cfg)
	f := &fake{cfg: cfg, plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0)}
	f.watch().Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}
	if want := []string{"LAN clients back on Xray; server Oslo"}; !reflect.DeepEqual(f.notes, want) {
		t.Fatalf("notes %v, want %v", f.notes, want)
	}
}

// A server picked without a fallback is a working outbound: the three minutes
// start again from its first miss, not from the death before it.
func TestTick_APickWithoutAFallbackStartsTheGraceAgain(t *testing.T) {
	f := &fake{cfg: baseCfg(), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	generated, dead := false, false
	w := f.watch()
	w.SaveServers = func([]vpnconfig.Server) error { return nil }
	w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
		return []vpnconfig.Server{{Name: "Oslo", Address: "new.example", Port: 443}}, nil
	}
	w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		generated = true
		return true, f.seq(), nil
	}
	w.RestartXray = func() error { return nil }
	w.AfterRestart = func(time.Duration) {}
	w.Probe = func(context.Context, int) error {
		if generated && !dead {
			return nil
		}
		return errProbe
	}
	tickUntilDead(w, f)
	if countNotes(f.notes, "Subscription refreshed; selected server Oslo") != 1 {
		t.Fatalf("notes %v", f.notes)
	}
	before := countNotes(f.notes, "Xray outbound is down; no Tunnel Director fallback")

	dead = true
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if countNotes(f.notes, "Xray outbound is down; no Tunnel Director fallback") != before {
		t.Fatalf("notes %v; one miss of the picked server declared it dead", f.notes)
	}
	tickFor(w, f, DeadAfter)
	if countNotes(f.notes, "Xray outbound is down; no Tunnel Director fallback") != before+1 {
		t.Fatalf("notes %v; three minutes of misses are a death", f.notes)
	}
}

// Nothing the outbound did while VPN Director was stopped counts: after the
// stop, the three minutes start again.
func TestTick_AStopStartsTheGraceAgain(t *testing.T) {
	f := &fake{cfg: baseCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	stopped := false
	w := f.watch()
	w.Stopped = func() bool { return stopped }
	tickFor(w, f, 2*time.Minute+30*time.Second)
	stopped = true
	w.Tick(context.Background())
	stopped = false
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v straight after the stop was lifted", f.cfg.Xray.Failover)
	}
	tickFor(w, f, DeadAfter)
	if f.cfg.Xray.Failover == nil {
		t.Fatal("three minutes of misses after the stop are a death")
	}
}

// The retry clock of an unready fallback survived its episode: the next death
// moved to another exit at once instead of giving the first one its minutes.
func TestTick_ANewEpisodeGivesItsFallbackTimeBeforeMovingOn(t *testing.T) {
	cfg := baseCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.1.4"}}
	f := &fake{cfg: cfg, plat: connected("ovpnc2", "wgc1"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
	w := f.watch()
	w.FallbackReady = func(string) bool { return false }
	tickUntilDead(w, f)
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background()) // the fallback's retry clock starts
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v", f.cfg.Xray.Failover)
	}

	f.probeErr = nil
	f.now = f.now.Add(ProbeInterval)
	w.Tick(context.Background())
	if f.cfg.Xray.Failover != nil {
		t.Fatalf("failover %+v after Xray came back", f.cfg.Xray.Failover)
	}

	f.probeErr = errProbe
	f.now = f.now.Add(time.Hour)
	tickUntilDead(w, f)
	tickFor(w, f, ImportRetry-time.Minute)
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v; the new episode's fallback got no time", f.cfg.Xray.Failover)
	}
}
