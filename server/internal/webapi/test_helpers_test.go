package webapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/auth"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updateflow"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockVPN implements service.VPNDirector for testing.
type mockVPN struct {
	statusOutput string
	err          error
	applyCalls   int // number of Apply() calls, to assert auto-apply
	updateCalls  int // number of Update() calls
	platform     vpnconfig.PlatformInfo
	platformErr  error
}

func (m *mockVPN) Status() (string, error) { return m.statusOutput, m.err }
func (m *mockVPN) Apply() error            { m.applyCalls++; return m.err }
func (m *mockVPN) Restart() error          { return m.err }
func (m *mockVPN) RestartXray() error      { return m.err }
func (m *mockVPN) Stop() error             { return m.err }
func (m *mockVPN) Update() error           { m.updateCalls++; return m.err }
func (m *mockVPN) Platform() (vpnconfig.PlatformInfo, error) {
	return m.platform, m.platformErr
}

// mockNetwork implements service.NetworkInfo for testing.
type mockNetwork struct {
	ip  string
	err error
}

func (m *mockNetwork) GetExternalIP() (string, error) { return m.ip, m.err }

// mockLogs implements service.LogReader for testing and records the paths read.
type mockLogs struct {
	output string
	err    error
	paths  []string
}

func (m *mockLogs) Read(path string, _ int) (string, error) {
	m.paths = append(m.paths, path)
	return m.output, m.err
}

// mockConfig implements service.ConfigStore for testing.
type mockConfig struct {
	cfg           *vpnconfig.VPNDirectorConfig
	servers       []vpnconfig.Server
	err           error
	saveVPNCfgErr error                        // independent error for the save step of UpdateVPNConfig
	savedCfg      *vpnconfig.VPNDirectorConfig // captured by UpdateVPNConfig
	savedServers  []vpnconfig.Server           // captured by SaveServers
	updateErr     error                        // returned by UpdateVPNConfig before fn runs, e.g. service.ErrConfigLockTimeout
	// subs are the subscription files. UpdateVPNConfig serializes on upd as
	// the flock does, and subsMu guards subs: RefreshAll runs one refresh per
	// subscription at once.
	subs    []vpnconfig.Subscription
	subsErr error // LoadSubscriptions fails
	upd     sync.Mutex
	subsMu  sync.Mutex
}

func (m *mockConfig) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	return m.cfg, m.err
}
func (m *mockConfig) LoadServers() ([]vpnconfig.Server, error) { return m.servers, m.err }
func (m *mockConfig) SaveServers(servers []vpnconfig.Server) error {
	m.savedServers = servers
	return m.err
}

func (m *mockConfig) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	if m.subsErr != nil {
		return nil, m.subsErr
	}
	out := make([]vpnconfig.Subscription, len(m.subs))
	for i, s := range m.subs {
		for j := range s.Servers {
			s.Servers[j].Subscription = s.ID
		}
		out[i] = s
	}
	return out, nil
}

func (m *mockConfig) SaveSubscription(sub vpnconfig.Subscription) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == sub.ID {
			m.subs[i] = sub
			return nil
		}
	}
	m.subs = append(m.subs, sub)
	return nil
}

func (m *mockConfig) DeleteSubscription(id string) error {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == id {
			m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
			return nil
		}
	}
	return nil
}

// UpdateVPNConfig mirrors the real contract: a load failure (err or no cfg)
// comes back wrapped in service.ErrConfigLoad before fn runs; an fn error
// skips the save; otherwise the mutated cfg is recorded as savedCfg and
// saveVPNCfgErr, if set, is returned after it.
func (m *mockConfig) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	if m.err != nil {
		return fmt.Errorf("%w: %w", service.ErrConfigLoad, m.err)
	}
	if m.cfg == nil {
		return fmt.Errorf("%w: %w", service.ErrConfigLoad, os.ErrNotExist)
	}
	if err := fn(m.cfg); err != nil {
		return err
	}
	m.savedCfg = m.cfg
	return m.saveVPNCfgErr
}

func (m *mockConfig) DataDir() (string, error) { return "/tmp/test-data", m.err }
func (m *mockConfig) DataDirOrDefault() string { return "/tmp/test-data" }
func (m *mockConfig) ScriptsDir() string       { return "/tmp/test-scripts" }

// mockXray implements service.XrayGenerator for testing.
type mockXray struct {
	err error
}

func (m *mockXray) GenerateConfig(_ vpnconfig.Server, _ ...service.InboundPorts) error { return m.err }

// mockShadow implements password verification for testing.
// It acts as a thin wrapper that allows tests to control Verify results.
type mockShadow struct {
	validUser string
	validPass string
}

func (m *mockShadow) verify(username, password string) (bool, error) {
	if username == m.validUser && password == m.validPass {
		return true, nil
	}
	return false, nil
}

// mockUpdateFlow implements UpdateFlow for testing.
type mockUpdateFlow struct {
	checkResult updateflow.CheckResult
	checkErr    error
	checkForce  bool

	startResult    updateflow.StartResult
	startErr       error
	startInitiator string
	startChatID    int64

	inProgress bool
}

func (m *mockUpdateFlow) Check(_ context.Context, force bool) (updateflow.CheckResult, error) {
	m.checkForce = force
	return m.checkResult, m.checkErr
}

func (m *mockUpdateFlow) Start(_ context.Context, initiator string, chatID int64, _ func(string)) (updateflow.StartResult, error) {
	m.startInitiator = initiator
	m.startChatID = chatID
	return m.startResult, m.startErr
}

func (m *mockUpdateFlow) InProgress() bool { return m.inProgress }

// testSHA256Hash is a known SHA-256 shadow hash for password "testpass".
const testSHA256Hash = "$5$testsalt$GR6PqdknD2fHavVjM//Q.4Qni8EXZKnxS838p5GC9r5"

// writeShadowFixture creates a temporary shadow file and returns its path.
func writeShadowFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestDeps creates a Deps instance wired with test mocks.
func newTestDeps(t *testing.T) *Deps {
	t.Helper()

	// authMiddleware compares every request's pwh claim against this file, so
	// the test deps need a real one. Handler tests that exercise login
	// override deps.Shadow with a fixture of their own.
	shadow := auth.NewShadowAuth(writeShadowFixture(t, "admin:"+testSHA256Hash+":19000:0:99999:7:::\n"))

	jwt := auth.NewJWTService("test-secret-key-32bytes!!!!!!!!", 1*time.Hour)

	return &Deps{
		Config: &mockConfig{},
		VPN: &mockVPN{statusOutput: "all systems operational", platform: vpnconfig.PlatformInfo{
			Platform: "merlin",
			Tunnels: []vpnconfig.PlatformTunnel{
				{ID: "wgc1", Iface: "wgc1", Type: "wireguard", Connected: true, Description: "Office WG"},
				{ID: "ovpnc1", Iface: "tun11", Type: "openvpn", Description: "Office OVPN"},
			},
		}},
		Xray:    &mockXray{},
		Network: &mockNetwork{ip: "203.0.113.42"},
		Logs:    &mockLogs{output: "log line 1\nlog line 2"},
		LogPaths: map[string]string{
			"bot":   "/tmp/test-telegram-bot.log",
			"vpn":   "/tmp/test-vpn-director.log",
			"xray":  "/tmp/test-xray-error.log",
			"webui": "/tmp/test-webui.log",
		},
		Update:       &mockUpdateFlow{},
		Shadow:       shadow,
		JWT:          jwt,
		Version:      "1.0.0-test",
		Commit:       "abc1234",
		OpMutex:      &sync.Mutex{},
		loginLimiter: newRateLimiter(5, 1*time.Minute, 30*time.Second),
	}
}

// newTestToken mints a token the middleware accepts for these deps: the
// subject is the fixture's only user and the fingerprint is the current one.
func newTestToken(t *testing.T, deps *Deps) string {
	t.Helper()
	fp, err := deps.Shadow.Fingerprint("admin")
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	token, err := deps.JWT.Create("admin", fp)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}
