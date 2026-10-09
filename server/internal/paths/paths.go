// Package paths provides centralized path configuration for the application
package paths

import (
	"os"
	"path/filepath"
)

// Paths holds all configurable paths for the application
type Paths struct {
	ScriptsDir     string // /opt/vpn-director
	BotConfigPath  string // /opt/vpn-director/telegram-bot.json
	DefaultDataDir string // /opt/vpn-director/data
	XrayTemplate   string // /opt/etc/xray/config.json.template
	XrayConfig     string // /opt/etc/xray/config.json
	BotLogPath     string // /tmp/telegram-bot.log
	VPNLogPath     string // /tmp/vpn-director.log
	WebUILogPath   string // /tmp/vpn-director-webui.log
	XrayLogPath    string // /tmp/xray-error.log (set by the log section of the Xray template)
	WatchdLogPath  string // /tmp/vpn-director-watchd.log
	WatchdSocket   string // /tmp/vpn-director/watchd.sock: the monitor's API
	WatchdState    string // /tmp/vpn-director/watchd-state.json: the monitor's state across restarts
	ProbeDir       string // /tmp/vpn-director/probe: the prober's config
	ProbeBinary    string // /opt/vpn-director/vpn-director-probe: a hard link to xray
	BotBinary      string // /opt/vpn-director/telegram-bot: capabilities checked before automation
	TunnelTables   string // /tmp/tunnel_director/tun_dir_tables
	FailoverReady  string // /tmp/tunnel_director/failover_ready
	TPROXYReady    string // /tmp/xray_tproxy/ready
	// StoppedMarker is the file vpn-director.sh stop writes and a full apply
	// removes; it pauses monitoring and subscription automation.
	StoppedMarker string // /tmp/vpn-director/stopped
}

// Default returns the default paths for production use
func Default() Paths {
	return Paths{
		ScriptsDir:     "/opt/vpn-director",
		BotConfigPath:  "/opt/vpn-director/telegram-bot.json",
		DefaultDataDir: "/opt/vpn-director/data",
		XrayTemplate:   "/opt/etc/xray/config.json.template",
		XrayConfig:     "/opt/etc/xray/config.json",
		BotLogPath:     "/tmp/telegram-bot.log",
		VPNLogPath:     "/tmp/vpn-director.log",
		WebUILogPath:   "/tmp/vpn-director-webui.log",
		XrayLogPath:    "/tmp/xray-error.log",
		WatchdLogPath:  "/tmp/vpn-director-watchd.log",
		WatchdSocket:   "/tmp/vpn-director/watchd.sock",
		WatchdState:    "/tmp/vpn-director/watchd-state.json",
		ProbeDir:       "/tmp/vpn-director/probe",
		ProbeBinary:    "/opt/vpn-director/vpn-director-probe",
		BotBinary:      "/opt/vpn-director/telegram-bot",
		TunnelTables:   "/tmp/tunnel_director/tun_dir_tables",
		FailoverReady:  "/tmp/tunnel_director/failover_ready",
		TPROXYReady:    "/tmp/xray_tproxy/ready",
		StoppedMarker:  "/tmp/vpn-director/stopped",
	}
}

// DevPaths returns paths for development mode using testdata/dev/
func DevPaths() Paths {
	return Paths{
		ScriptsDir:     "testdata/dev",
		BotConfigPath:  "testdata/dev/telegram-bot.json",
		DefaultDataDir: "testdata/dev/data",
		XrayTemplate:   "testdata/dev/xray.template.json",
		XrayConfig:     "testdata/dev/xray.json",
		BotLogPath:     "testdata/dev/bot.log",
		VPNLogPath:     "testdata/dev/vpn.log",
		WebUILogPath:   "testdata/dev/webui.log",
		XrayLogPath:    "testdata/dev/xray-error.log",
		WatchdLogPath:  "testdata/dev/watchd.log",
		WatchdSocket:   "testdata/dev/watchd.sock",
		WatchdState:    "testdata/dev/watchd-state.json",
		ProbeDir:       "testdata/dev/probe",
		ProbeBinary:    "testdata/dev/vpn-director-probe",
		BotBinary:      "testdata/dev/telegram-bot",
		TunnelTables:   "testdata/dev/tun_dir_tables",
		FailoverReady:  "testdata/dev/failover_ready",
		TPROXYReady:    "testdata/dev/tproxy_ready",
		StoppedMarker:  "testdata/dev/stopped",
	}
}

// RotatedLogs lists every log file the daemons truncate at
// logging.DefaultMaxSize. Every daemon rotates the same list; os.Truncate is
// idempotent, so two processes rotating at once are safe.
func (p Paths) RotatedLogs() []string {
	return []string{p.BotLogPath, p.VPNLogPath, p.WebUILogPath, p.XrayLogPath, p.WatchdLogPath}
}

// DetachFromCallerDirectory resolves the paths behind flagPaths against the
// directory this process was started in and then moves it to /, which no
// update deletes.
//
// A daemon has no business holding the directory of whoever started it, and
// here that directory is usually doomed: the update script starts the daemons
// while /tmp/vpn-director-update is still there, and the bot removes it moments
// later, as soon as it has reported the update. What is left is a process whose
// working directory does not exist - monit refuses to run at all without one,
// and every shell spawned from here prints "shell-init: error retrieving
// current directory" into output the Web UI puts on screen.
//
// Asking first whether the directory is still there is no defence: at startup
// it is, and the deletion comes after. Hence unconditional.
//
// The flag paths are resolved before the move, so a relative --config keeps
// naming the same file. Dev mode must not call this at all: DevPaths are
// relative to the source tree.
func DetachFromCallerDirectory(flagPaths ...*string) error {
	for _, p := range flagPaths {
		if p == nil || *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
	return os.Chdir("/")
}

// Resolve reads p against base when p is relative, and hands it back unchanged
// when it is absolute or empty.
//
// It is how the file paths inside vpn-director.json are understood: against
// the config file, never against the directory a daemon happened to be started
// in. Those daemons move to / at startup, and before that the directory was
// whatever the launcher had - the update script's own, at one point, which the
// bot deletes moments later.
//
// base may itself be relative, and then so is the answer: dev mode's paths are
// relative to the source tree.
func Resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
