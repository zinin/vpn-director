// internal/service/activeserver_test.go
package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// stubStore is the slice of ConfigStore this function touches. It reports
// whether it is inside the locked section, which is how the test below can
// tell where the generation ran.
type stubStore struct {
	cfg              *vpnconfig.VPNDirectorConfig
	err              error // fails before the closure runs, as a load or lock failure does
	saveErr          error // fails after it, as the save step does
	serversErr       error // fails the servers.json write
	inClosure        bool
	saved            bool
	savedServers     []vpnconfig.Server
	serversUnderLock bool
}

func (s *stubStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) { return s.cfg, nil }
func (s *stubStore) LoadServers() ([]vpnconfig.Server, error)             { return nil, nil }
func (s *stubStore) SaveServers(servers []vpnconfig.Server) error {
	s.serversUnderLock = s.inClosure
	if s.serversErr != nil {
		return s.serversErr
	}
	s.savedServers = servers
	return nil
}
func (s *stubStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if s.err != nil {
		return s.err
	}
	s.inClosure = true
	err := fn(s.cfg)
	s.inClosure = false
	if err != nil {
		return err
	}
	s.saved = true
	return s.saveErr
}
func (s *stubStore) DataDir() (string, error) { return "", nil }
func (s *stubStore) DataDirOrDefault() string { return "" }
func (s *stubStore) ScriptsDir() string       { return "" }

// stubXray notes whether the store was inside its locked section when the
// generation ran.
type stubXray struct {
	store     *stubStore
	err       error
	called    bool
	underLock bool
	got       vpnconfig.Server
	gotPorts  InboundPorts
}

func (x *stubXray) GenerateConfig(s vpnconfig.Server, ports ...InboundPorts) error {
	x.called = true
	x.underLock = x.store.inClosure
	x.got = s
	if len(ports) > 0 {
		x.gotPorts = ports[0]
	}
	return x.err
}

func TestGenerateAndRecordDialedServer_RecordsHostnameNotDialIP(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}
	identity := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	dial := vpnconfig.Server{Name: "Oslo", Address: "203.0.113.50", Port: 443, SNI: "oslo.example"}

	generated, err := GenerateAndRecordDialedServer(store, xray, dial, identity, InboundPorts{TProxy: 12345, Socks: 12346}, nil)
	if err != nil {
		t.Fatalf("GenerateAndRecordDialedServer error: %v", err)
	}
	if !generated {
		t.Fatal("generated = false")
	}
	if xray.got.Address != "203.0.113.50" {
		t.Fatalf("config.json address %q, want the dial IP", xray.got.Address)
	}
	got := store.cfg.Xray.ActiveServer
	if got == nil {
		t.Fatal("nothing was recorded")
	}
	if got.Name != "Oslo" || got.Address != "oslo.example" || got.Port != 443 {
		t.Errorf("recorded %+v, want the subscription hostname", *got)
	}
}

// The subscription watch decides whether its walk still owns active_server.
// Asked before the lock, a Web UI or /xray selection can commit in between and
// be written over, so the guard has to see the config the lock protects.
func TestGenerateAndRecordDialedServer_GuardRunsUnderTheConfigLock(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	ran, underLock := false, false

	generated, err := GenerateAndRecordDialedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
		ran, underLock = true, store.inClosure
		return nil
	})

	if err != nil || !generated {
		t.Fatalf("generated = %v, error = %v; a passing guard must not stop the switch", generated, err)
	}
	if !ran {
		t.Fatal("the guard never ran")
	}
	if !underLock {
		t.Error("the guard ran outside the config lock; a concurrent selection can land after it")
	}
}

func TestGenerateAndRecordDialedServer_GuardRefusalWritesNothing(t *testing.T) {
	manual := &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{ActiveServer: manual}}}
	xray := &stubXray{store: store}
	refused := errors.New("a newer server was selected")
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}

	generated, err := GenerateAndRecordDialedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
		return refused
	})

	if generated {
		t.Error("generated = true although the guard refused")
	}
	if !errors.Is(err, refused) {
		t.Errorf("error = %v, want the guard's refusal", err)
	}
	if xray.called {
		t.Error("config.json was generated although the guard refused")
	}
	if store.saved {
		t.Error("the config was saved although the guard refused")
	}
	if store.cfg.Xray.ActiveServer != manual {
		t.Errorf("active_server = %+v, want the selection the guard protected", store.cfg.Xray.ActiveServer)
	}
}

func TestGenerateAndRecordActiveServer_RecordsWhatIdentifiesTheServer(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}

	generated, err := GenerateAndRecordActiveServer(store, xray, vpnconfig.Server{
		Name:      "Берлин, Германия, Extra",
		Address:   "155.117.201.148",
		Port:      443,
		UUID:      "the-subscription-uuid",
		PublicKey: "PBK",
	}, InboundPorts{TProxy: 12345, Socks: 12346})

	if err != nil {
		t.Fatalf("GenerateAndRecordActiveServer error: %v", err)
	}
	if !generated {
		t.Fatal("generated = false after a successful generation")
	}
	if !xray.called {
		t.Fatal("the config was never generated")
	}
	if xray.gotPorts.TProxy != 12345 || xray.gotPorts.Socks != 12346 {
		t.Errorf("generated with ports %+v, want the ones passed in", xray.gotPorts)
	}
	got := store.cfg.Xray.ActiveServer
	if got == nil {
		t.Fatal("nothing was recorded")
	}
	if got.Name != "Берлин, Германия, Extra" || got.Address != "155.117.201.148" || got.Port != 443 {
		t.Errorf("recorded %+v, want the name, address and port of the generated server", *got)
	}
}

// The whole point of the pair: with the generation outside the lock, a switch
// from the other daemon can land between this one's config.json and its
// record, and the two then name different servers with both switches reporting
// success.
func TestGenerateAndRecordActiveServer_GeneratesUnderTheConfigLock(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}

	if _, err := GenerateAndRecordActiveServer(store, xray, vpnconfig.Server{Name: "Осло"}, InboundPorts{}); err != nil {
		t.Fatalf("GenerateAndRecordActiveServer error: %v", err)
	}

	if !xray.underLock {
		t.Error("config.json was generated outside the config lock; a concurrent switch can then outrun the record")
	}
}

// A rejected server - an incomplete REALITY entry is the ordinary way - leaves
// the previous config.json running, and the error reaches the caller unwrapped
// so its message can carry Xray's own complaint.
func TestGenerateAndRecordActiveServer_ReportsARejectedServerAsNotGenerated(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store, err: errors.New("reality requires a public key")}

	generated, err := GenerateAndRecordActiveServer(store, xray, vpnconfig.Server{Name: "Осло"}, InboundPorts{})

	if generated {
		t.Error("generated = true although Xray rejected the server")
	}
	if err == nil || !strings.Contains(err.Error(), "reality requires a public key") {
		t.Errorf("error = %v, want Xray's own complaint to survive", err)
	}
	if store.cfg.Xray.ActiveServer != nil {
		t.Errorf("recorded %+v after the config failed to generate", *store.cfg.Xray.ActiveServer)
	}
	if store.saved {
		t.Error("the config was saved even though the generation failed")
	}
}

// A config that cannot even be loaded never reaches the generation, so nothing
// was written. Reading that as "only the bookkeeping failed" would have the
// caller restart Xray and report a switch that never happened.
func TestGenerateAndRecordActiveServer_ReportsAnUnreachableConfigAsNotGenerated(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}, err: errors.New("load config: file does not exist")}
	xray := &stubXray{store: store}

	generated, err := GenerateAndRecordActiveServer(store, xray, vpnconfig.Server{Name: "Осло"}, InboundPorts{})

	if generated {
		t.Error("generated = true although the closure never ran")
	}
	if xray.called {
		t.Error("the config was generated even though the store failed before the closure")
	}
	if err == nil {
		t.Fatal("expected the store failure to come back, got nil")
	}
}

// The other half: the generation succeeded and only the save did not. The
// switch happened, so the caller must not report it as failed.
func TestGenerateAndRecordActiveServer_ReportsASaveFailureAsGenerated(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}, saveErr: errors.New("config lock timeout")}
	xray := &stubXray{store: store}

	generated, err := GenerateAndRecordActiveServer(store, xray, vpnconfig.Server{Name: "Осло"}, InboundPorts{})

	if !generated {
		t.Error("generated = false although config.json was written")
	}
	if err == nil {
		t.Fatal("expected the save failure to come back, got nil")
	}
}

// Every record moves the counter on, so a selection of the server already named
// is still visible to a reader that remembers what it last saw.
func TestGenerateAndRecordDialedServer_CountsTheWrite(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4},
	}}}
	xray := &stubXray{store: store}
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}

	generated, err := GenerateAndRecordDialedServer(store, xray, s, s, InboundPorts{}, nil)
	if err != nil || !generated {
		t.Fatalf("generated %v, err %v", generated, err)
	}
	if got := vpnconfig.ActiveSeq(store.cfg.Xray.ActiveServer); got != 5 {
		t.Fatalf("Seq %d, want 5: re-selecting the same server is a new write", got)
	}
}
