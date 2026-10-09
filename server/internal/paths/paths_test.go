package paths

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	p := Default()

	tests := []struct {
		name    string
		got     string
		wantPfx string
		wantSfx string
	}{
		{"ScriptsDir", p.ScriptsDir, "/opt/vpn-director", ""},
		{"BotConfigPath", p.BotConfigPath, "/opt/vpn-director/", "telegram-bot.json"},
		{"DefaultDataDir", p.DefaultDataDir, "/opt/vpn-director/", "data"},
		{"XrayTemplate", p.XrayTemplate, "/opt/etc/xray/", ".template"},
		{"XrayConfig", p.XrayConfig, "/opt/etc/xray/", ".json"},
		{"BotLogPath", p.BotLogPath, "/tmp/", "telegram-bot.log"},
		{"VPNLogPath", p.VPNLogPath, "/tmp/", "vpn-director.log"},
		{"WebUILogPath", p.WebUILogPath, "/tmp/", "vpn-director-webui.log"},
		{"XrayLogPath", p.XrayLogPath, "/tmp/", "xray-error.log"},
		{"WatchdLogPath", p.WatchdLogPath, "/tmp/", "vpn-director-watchd.log"},
		{"WatchdSocket", p.WatchdSocket, "/tmp/vpn-director/", "watchd.sock"},
		{"WatchdState", p.WatchdState, "/tmp/vpn-director/", "watchd-state.json"},
		{"ProbeDir", p.ProbeDir, "/tmp/vpn-director/", "probe"},
		{"ProbeBinary", p.ProbeBinary, "/opt/vpn-director/", "vpn-director-probe"},
		{"StoppedMarker", p.StoppedMarker, "/tmp/vpn-director/", "stopped"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got == "" {
				t.Errorf("%s is empty", tt.name)
			}
			if tt.wantPfx != "" && !strings.HasPrefix(tt.got, tt.wantPfx) {
				t.Errorf("%s = %q, want prefix %q", tt.name, tt.got, tt.wantPfx)
			}
			if tt.wantSfx != "" && !strings.HasSuffix(tt.got, tt.wantSfx) {
				t.Errorf("%s = %q, want suffix %q", tt.name, tt.got, tt.wantSfx)
			}
		})
	}
}

func TestDefaultNotEmpty(t *testing.T) {
	p := Default()

	if p.ScriptsDir == "" {
		t.Error("ScriptsDir should not be empty")
	}
	if p.BotConfigPath == "" {
		t.Error("BotConfigPath should not be empty")
	}
	if p.DefaultDataDir == "" {
		t.Error("DefaultDataDir should not be empty")
	}
	if p.XrayTemplate == "" {
		t.Error("XrayTemplate should not be empty")
	}
	if p.XrayConfig == "" {
		t.Error("XrayConfig should not be empty")
	}
	if p.BotLogPath == "" {
		t.Error("BotLogPath should not be empty")
	}
	if p.VPNLogPath == "" {
		t.Error("VPNLogPath should not be empty")
	}
	if p.WebUILogPath == "" {
		t.Error("WebUILogPath should not be empty")
	}
	if p.XrayLogPath == "" {
		t.Error("XrayLogPath should not be empty")
	}
}

func TestDevPaths(t *testing.T) {
	p := DevPaths()

	tests := []struct {
		name    string
		got     string
		wantPfx string
		wantSfx string
	}{
		{"ScriptsDir", p.ScriptsDir, "testdata/dev", ""},
		{"BotConfigPath", p.BotConfigPath, "testdata/dev/", "telegram-bot.json"},
		{"DefaultDataDir", p.DefaultDataDir, "testdata/dev/", "data"},
		{"XrayTemplate", p.XrayTemplate, "testdata/dev/", "xray.template.json"},
		{"XrayConfig", p.XrayConfig, "testdata/dev/", "xray.json"},
		{"BotLogPath", p.BotLogPath, "testdata/dev/", "bot.log"},
		{"VPNLogPath", p.VPNLogPath, "testdata/dev/", "vpn.log"},
		{"WebUILogPath", p.WebUILogPath, "testdata/dev/", "webui.log"},
		{"XrayLogPath", p.XrayLogPath, "testdata/dev/", "xray-error.log"},
		{"WatchdLogPath", p.WatchdLogPath, "testdata/dev/", "watchd.log"},
		{"WatchdSocket", p.WatchdSocket, "testdata/dev/", "watchd.sock"},
		{"WatchdState", p.WatchdState, "testdata/dev/", "watchd-state.json"},
		{"ProbeDir", p.ProbeDir, "testdata/dev/", "probe"},
		{"ProbeBinary", p.ProbeBinary, "testdata/dev/", "vpn-director-probe"},
		{"StoppedMarker", p.StoppedMarker, "testdata/dev/", "stopped"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got == "" {
				t.Errorf("%s is empty", tt.name)
			}
			if tt.wantPfx != "" && !strings.HasPrefix(tt.got, tt.wantPfx) {
				t.Errorf("%s = %q, want prefix %q", tt.name, tt.got, tt.wantPfx)
			}
			if tt.wantSfx != "" && !strings.HasSuffix(tt.got, tt.wantSfx) {
				t.Errorf("%s = %q, want suffix %q", tt.name, tt.got, tt.wantSfx)
			}
		})
	}
}

func TestRotatedLogs(t *testing.T) {
	p := Default()
	want := []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath, p.WatchdLogPath}
	if got := p.RotatedLogs(); !reflect.DeepEqual(got, want) {
		t.Errorf("RotatedLogs() = %v, want %v", got, want)
	}
	for _, path := range p.RotatedLogs() {
		if path == "" {
			t.Error("RotatedLogs contains an empty path")
		}
	}
}

// A daemon has no business holding the directory it was started from, and here
// that directory is usually doomed: the update script starts the daemons while
// /tmp/vpn-director-update still exists and the bot deletes it moments later,
// once it has reported the update. Asking whether the directory is still there
// is no defence - at startup it is, and the deletion comes after - so the move
// is unconditional.
func TestDetachFromCallerDirectory_MovesToRootFromALiveDirectory(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })

	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if err := DetachFromCallerDirectory(); err != nil {
		t.Fatalf("DetachFromCallerDirectory() error = %v", err)
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/" {
		t.Errorf("working directory is %q, want /", got)
	}
}

// The move must not change what a relative flag names: --config vpn.json is
// resolved against the directory the daemon was started in, before it leaves.
func TestDetachFromCallerDirectory_ResolvesTheFlagPathsFirst(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vpn-director.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	config := "vpn-director.json"
	if err := DetachFromCallerDirectory(&config); err != nil {
		t.Fatalf("DetachFromCallerDirectory() error = %v", err)
	}

	if !filepath.IsAbs(config) {
		t.Fatalf("the flag still reads %q, want an absolute path", config)
	}
	if _, err := os.ReadFile(config); err != nil {
		t.Errorf("the resolved path no longer names the file: %v", err)
	}
}

// The same call is what saves a daemon whose directory has already gone -
// os.Chdir does not care, where a relative path would.
func TestDetachFromCallerDirectory_WorksFromADirectoryThatIsGone(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })

	dir, err := os.MkdirTemp("", "deleted-cwd")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := DetachFromCallerDirectory(); err != nil {
		t.Fatalf("DetachFromCallerDirectory() error = %v", err)
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatalf("the working directory is still gone: %v", err)
	}
	if got != "/" {
		t.Errorf("working directory is %q, want /", got)
	}
}

// Paths inside vpn-director.json are read against the config file, because the
// daemons no longer keep the directory they were started in - and before they
// moved to /, that directory depended on whoever launched them.
func TestResolve(t *testing.T) {
	cases := []struct {
		name, base, path, want string
	}{
		{"a relative path lands beside the config", "/opt/vpn-director", "data", "/opt/vpn-director/data"},
		{"an absolute path is its own answer", "/opt/vpn-director", "/mnt/usb/data", "/mnt/usb/data"},
		{"an empty path stays empty", "/opt/vpn-director", "", ""},
		{"a relative base keeps the result relative", "testdata/dev", "data", "testdata/dev/data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.base, tc.path); got != tc.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
			}
		})
	}
}

func TestRuntime_WatchReadinessPathsMatchProductionAndStayIsolatedInDev(t *testing.T) {
	p := Default()
	for _, tc := range []struct {
		name, got, want string
	}{
		{"bot executable", p.BotBinary, "/opt/vpn-director/telegram-bot"},
		{"tunnel tables", p.TunnelTables, "/tmp/tunnel_director/tun_dir_tables"},
		{"failover readiness", p.FailoverReady, "/tmp/tunnel_director/failover_ready"},
		{"TPROXY readiness", p.TPROXYReady, "/tmp/xray_tproxy/ready"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s=%q, want the existing router path %q", tc.name, tc.got, tc.want)
		}
	}
	dev := DevPaths()
	for _, path := range []string{dev.BotBinary, dev.TunnelTables, dev.FailoverReady, dev.TPROXYReady, dev.StoppedMarker} {
		if path == "" || filepath.IsAbs(path) {
			t.Fatalf("dev automation would access a router path: %q", path)
		}
		rel, err := filepath.Rel("testdata/dev", filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("dev readiness escaped testdata/dev: %q", path)
		}
	}
}
