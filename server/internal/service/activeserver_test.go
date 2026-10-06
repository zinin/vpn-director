// internal/service/activeserver_test.go
package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// stubStore is the slice of ConfigStore this function touches. It reports
// whether it is inside the locked section, which is how the test below can
// tell where the generation ran.
type stubStore struct {
	cfg       *vpnconfig.VPNDirectorConfig
	err       error // fails before the closure runs, as a load or lock failure does
	saveErr   error // fails after it, as the save step does
	inClosure bool
	saved     bool
}

func (s *stubStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) { return s.cfg, nil }
func (s *stubStore) LoadServers() ([]vpnconfig.Server, error)             { return nil, nil }
func (s *stubStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) { return nil, nil }
func (s *stubStore) SaveSubscription(vpnconfig.Subscription) error        { return nil }
func (s *stubStore) DeleteSubscription(string) error                      { return nil }
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

func TestGenerateAndRecordWalkedServer_RecordsHostnameNotDialIP(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}
	identity := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	dial := vpnconfig.Server{Name: "Oslo", Address: "203.0.113.50", Port: 443, SNI: "oslo.example"}

	generated, _, err := GenerateAndRecordWalkedServer(store, xray, dial, identity, InboundPorts{TProxy: 12345, Socks: 12346}, nil)
	if err != nil {
		t.Fatalf("GenerateAndRecordWalkedServer error: %v", err)
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
func TestGenerateAndRecordWalkedServer_GuardRunsUnderTheConfigLock(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}
	xray := &stubXray{store: store}
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	ran, underLock := false, false

	generated, _, err := GenerateAndRecordWalkedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
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

func TestGenerateAndRecordWalkedServer_GuardRefusalWritesNothing(t *testing.T) {
	manual := &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443}
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{ActiveServer: manual}}}
	xray := &stubXray{store: store}
	refused := errors.New("a newer server was selected")
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}

	generated, _, err := GenerateAndRecordWalkedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
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
func TestGenerateAndRecordWalkedServer_CountsTheWrite(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4},
	}}}
	xray := &stubXray{store: store}
	s := vpnconfig.Server{Name: "Oslo", Address: "oslo.example", Port: 443}

	generated, seq, err := GenerateAndRecordWalkedServer(store, xray, s, s, InboundPorts{}, nil)
	if err != nil || !generated {
		t.Fatalf("generated %v, err %v", generated, err)
	}
	if got := vpnconfig.ActiveSeq(store.cfg.Xray.ActiveServer); got != 5 {
		t.Fatalf("Seq %d, want 5: re-selecting the same server is a new write", got)
	}
	if seq != 5 {
		t.Fatalf("returned seq %d, want the counter the transaction wrote", seq)
	}
}

// walkedRecord runs one record of the subscription walk against cfg and returns
// what the config holds afterwards.
func walkedRecord(t *testing.T, cfg *vpnconfig.VPNDirectorConfig, s vpnconfig.Server) *vpnconfig.VPNDirectorConfig {
	t.Helper()
	store := &stubStore{cfg: cfg}
	generated, _, err := GenerateAndRecordWalkedServer(store, &stubXray{store: store}, s, s, InboundPorts{}, nil)
	if err != nil || !generated {
		t.Fatalf("generated %v, err %v", generated, err)
	}
	return store.cfg
}

// The walk moves active_server off the server the user chose, and a walk cut
// short - the bot restarted, a /stop - leaves it on a server nobody picked. The
// choice is kept beside it, from the first record that leaves its name.
func TestGenerateAndRecordWalkedServer_KeepsTheChoiceItMovesAwayFrom(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443, Seq: 4},
	}}
	chosen := vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443}

	cfg = walkedRecord(t, cfg, vpnconfig.Server{Name: "Backup", Address: "backup.example", Port: 443})
	if got := cfg.Xray.PreferredServer; got == nil || *got != chosen {
		t.Fatalf("preferred_server %+v, want %+v", got, chosen)
	}
	if got := cfg.Xray.ActiveServer; got == nil || got.Name != "Backup" || got.Seq != 5 {
		t.Fatalf("active_server %+v, want Backup at seq 5", got)
	}

	// The next server the walk tries is not the user's choice either.
	cfg = walkedRecord(t, cfg, vpnconfig.Server{Name: "Extra", Address: "extra.example", Port: 443})
	if got := cfg.Xray.PreferredServer; got == nil || *got != chosen {
		t.Fatalf("preferred_server %+v after a second walked server, want %+v", got, chosen)
	}
}

// Back on the chosen name - at whatever address the subscription gives it today -
// the walk is where the user put it, and there is nothing left to remember.
func TestGenerateAndRecordWalkedServer_ForgetsTheChoiceBackOnItsName(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer:    &vpnconfig.ActiveServer{Name: "Backup", Address: "backup.example", Port: 443, Seq: 5},
		PreferredServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443},
	}}

	cfg = walkedRecord(t, cfg, vpnconfig.Server{Name: "Oslo", Address: "oslo-7.example", Port: 443})

	if cfg.Xray.PreferredServer != nil {
		t.Fatalf("preferred_server %+v, want none once the walk is back on Oslo", cfg.Xray.PreferredServer)
	}
}

// A subscription that rotates endpoints moves a name to a new address every day.
// The walk that follows it has not moved away from the user's choice.
func TestGenerateAndRecordWalkedServer_ANewAddressOfTheChosenNameIsNoMove(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443, Seq: 4},
	}}

	cfg = walkedRecord(t, cfg, vpnconfig.Server{Name: "Oslo", Address: "oslo-7.example", Port: 443})

	if cfg.Xray.PreferredServer != nil {
		t.Fatalf("preferred_server %+v, want none", cfg.Xray.PreferredServer)
	}
}

// A selection in the Web UI, /xray or the wizard is the user's new choice: what
// the walk remembered of the old one is over.
func TestGenerateAndRecordActiveServer_ASelectionEndsWhatTheWalkRemembered(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer:    &vpnconfig.ActiveServer{Name: "Backup", Address: "backup.example", Port: 443, Seq: 5},
		PreferredServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo-1.example", Port: 443},
	}}}

	generated, err := GenerateAndRecordActiveServer(store, &stubXray{store: store},
		vpnconfig.Server{Name: "Paris", Address: "paris.example", Port: 443}, InboundPorts{})

	if err != nil || !generated {
		t.Fatalf("generated %v, err %v", generated, err)
	}
	if store.cfg.Xray.PreferredServer != nil {
		t.Fatalf("preferred_server %+v after the user selected Paris", store.cfg.Xray.PreferredServer)
	}
}

func guardedActiveFixture(t *testing.T) (*ConfigService, *XrayService, string) {
	t.Helper()
	templatePath, outputPath := writeTemplate(t)
	dir := filepath.Dir(outputPath)
	store := NewConfigService(dir, filepath.Join(dir, "data"))
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Subscription: "sub", Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4},
	}}
	if err := vpnconfig.SaveVPNDirectorConfig(store.ConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return store, newTestXrayService(templatePath, outputPath), outputPath
}

func assertConfigLockHeld(t *testing.T, store *ConfigService) {
	t.Helper()
	f, err := os.OpenFile(store.LockPath(), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Error("generation/guard ran without the exclusive config lock")
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("checking the config lock: %v", err)
	}
}

func TestGenerateAndRecordGuardedWalkedServer_RecordsIdentityAndSequence(t *testing.T) {
	store, xray, outputPath := guardedActiveFixture(t)
	identity := vpnconfig.Server{Subscription: "sub", Name: "Backup", Address: "backup.example", Port: 443}
	dial := vpnconfig.Server{Subscription: "sub", Name: "Backup", Address: "203.0.113.50", Port: 443, UUID: "synthetic-id", Security: "tls", SNI: "backup.example"}
	guards, validations := 0, 0
	xray.validate = func(string) error {
		validations++
		assertConfigLockHeld(t, store)
		if guards == 0 {
			t.Error("generation started before the initial locked guard")
		}
		return nil
	}
	generated, seq, err := GenerateAndRecordGuardedWalkedServer(store, xray, dial, identity,
		InboundPorts{TProxy: 23456, Socks: 23457}, func(cfg *vpnconfig.VPNDirectorConfig) error {
			guards++
			assertConfigLockHeld(t, store)
			if a := cfg.Xray.ActiveServer; a == nil || a.Seq != 4 || a.Name != "Oslo" {
				t.Errorf("guard saw %+v, want the locked original selection", a)
			}
			return nil
		})
	if err != nil || !generated || seq != 5 {
		t.Fatalf("generated %v, seq %d, error %v; want true/5/nil", generated, seq, err)
	}
	if guards < 2 || validations != 1 {
		t.Errorf("guards %d, validations %d; want locked guards before and after one validation", guards, validations)
	}
	cfg, err := store.LoadVPNConfig()
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Xray.ActiveServer; a == nil || a.Subscription != "sub" || a.Name != "Backup" || a.Address != "backup.example" || a.Port != 443 || a.Seq != 5 {
		t.Errorf("recorded %+v, want the hostname identity at seq 5", a)
	}
	if p := cfg.Xray.PreferredServer; p == nil || p.Subscription != "sub" || p.Name != "Oslo" || p.Address != "oslo.example" || p.Port != 443 {
		t.Errorf("preferred %+v, want the original manual choice preserved", p)
	}
	ports := readInboundPorts(t, outputPath)
	if ports["tproxy-in"] != 23456 || ports["socks-in"] != 23457 {
		t.Errorf("ports %v, want the supplied 23456/23457", ports)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil || !strings.Contains(string(content), `"address": "203.0.113.50"`) {
		t.Errorf("generated config %q, error %v; want the dial IP rather than the identity hostname", content, err)
	}
	assertNoXrayTemps(t, outputPath)
}

func TestGenerateAndRecordGuardedWalkedServer_RefusalWritesNothing(t *testing.T) {
	for _, afterValidation := range []bool{false, true} {
		name := "initial guard"
		if afterValidation {
			name = "final guard after injected validation"
		}
		t.Run(name, func(t *testing.T) {
			store, xray, outputPath := guardedActiveFixture(t)
			before, err := os.ReadFile(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			validated, finalGuard := false, false
			refused := errors.New("compatibility is unconfirmed")
			xray.validate = func(string) error {
				assertConfigLockHeld(t, store)
				validated = true
				return nil
			}
			s := vpnconfig.Server{Subscription: "sub", Name: "Backup", Address: "backup.example", Port: 443, Security: "tls"}
			generated, _, err := GenerateAndRecordGuardedWalkedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
				assertConfigLockHeld(t, store)
				if validated {
					finalGuard = true
				}
				if !afterValidation || validated {
					return refused
				}
				return nil
			})
			if generated || !errors.Is(err, refused) {
				t.Errorf("generated %v, error %v; want false and the guard refusal", generated, err)
			}
			if validated != afterValidation || finalGuard != afterValidation {
				t.Errorf("validated %v, final guard %v; want both %v", validated, finalGuard, afterValidation)
			}
			after, readErr := os.ReadFile(store.ConfigPath())
			if readErr != nil || string(after) != string(before) {
				t.Errorf("VPN config changed after refusal: error %v", readErr)
			}
			content, readErr := os.ReadFile(outputPath)
			if readErr != nil || string(content) != "previous\n" {
				t.Errorf("live config %q, error %v; refused generation must not publish", content, readErr)
			}
			assertNoXrayTemps(t, outputPath)
		})
	}
}

func TestGenerateAndRecordGuardedWalkedServer_SaveFailureIsGenerated(t *testing.T) {
	xray, outputPath := testedService(t)
	xray.validate = nil
	saveErr := errors.New("synthetic config save failure")
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4},
	}}, saveErr: saveErr}
	s := vpnconfig.Server{Name: "Backup", Address: "backup.example", Port: 443, Security: "tls"}
	guards := 0
	generated, seq, err := GenerateAndRecordGuardedWalkedServer(store, xray, s, s, InboundPorts{}, func(*vpnconfig.VPNDirectorConfig) error {
		guards++
		if !store.inClosure {
			t.Error("guard ran outside the config transaction")
		}
		return nil
	})
	if !generated || seq != 4 || !errors.Is(err, saveErr) {
		t.Errorf("generated %v, seq %d, error %v; want true/4 and the bookkeeping failure", generated, seq, err)
	}
	if guards < 2 || !store.saved {
		t.Errorf("guards %d, saved %v; the generated server must reach the guarded record/save", guards, store.saved)
	}
	if a := store.cfg.Xray.ActiveServer; a == nil || a.Name != "Backup" || a.Seq != 5 {
		t.Errorf("in-transaction active %+v, want the attempted record at seq 5", a)
	}
	content, readErr := os.ReadFile(outputPath)
	if readErr != nil || !strings.Contains(string(content), "backup.example") {
		t.Errorf("live config %q, error %v; only the VPN record save failed", content, readErr)
	}
	assertNoXrayTemps(t, outputPath)
}

type afterGuardedStore struct {
	*stubStore
	after func()
}

func (s *afterGuardedStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	err := s.stubStore.UpdateVPNConfig(fn)
	if err == nil {
		s.after()
	}
	return err
}

func TestGenerateAndRecordGuardedWalkedServer_ReturnsTransactionSequence(t *testing.T) {
	xray, _ := testedService(t)
	xray.validate = nil
	base := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		ActiveServer: &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 4},
	}}}
	store := &afterGuardedStore{stubStore: base, after: func() {
		base.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Manual", Address: "manual.example", Port: 443, Seq: 99}
	}}
	s := vpnconfig.Server{Name: "Backup", Address: "backup.example", Port: 443, Security: "tls"}
	generated, seq, err := GenerateAndRecordGuardedWalkedServer(store, xray, s, s, InboundPorts{}, nil)
	if err != nil || !generated || seq != 5 {
		t.Errorf("generated %v, seq %d, error %v; want the transaction's seq 5, not a later writer's 99", generated, seq, err)
	}
	if a := base.cfg.Xray.ActiveServer; a == nil || a.Seq != 99 {
		t.Errorf("later writer fixture %+v did not execute", a)
	}
}
