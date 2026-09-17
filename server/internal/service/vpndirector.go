// internal/service/vpndirector.go
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zinin/vpn-director/server/internal/shell"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Per-command limits for vpn-director.sh. A limit is a safety net against a
// hung script, not an expected duration: apply and update download country
// ipsets and legitimately run for minutes on a throttled link. Each limit
// includes up to two minutes the script may spend in --wait for its lock.
const (
	// StatusTimeout bounds `status`, which only reads kernel state.
	StatusTimeout = 30 * time.Second
	// ApplyTimeout bounds apply, restart, stop and restart xray.
	ApplyTimeout = 5 * time.Minute
	// UpdateTimeout bounds `update`, which re-downloads every configured
	// country set from up to three sources.
	UpdateTimeout = 15 * time.Minute
)

// Compile-time interface check
var _ VPNDirector = (*VPNDirectorService)(nil)

// VPNDirectorService handles VPN Director shell operations
type VPNDirectorService struct {
	scriptsDir string
	executor   ShellExecutor
}

// NewVPNDirectorService creates a new VPNDirectorService
func NewVPNDirectorService(scriptsDir string, executor ShellExecutor) *VPNDirectorService {
	if executor == nil {
		executor = DefaultExecutor()
	}
	return &VPNDirectorService{
		scriptsDir: scriptsDir,
		executor:   executor,
	}
}

func (s *VPNDirectorService) scriptPath() string {
	return filepath.Join(s.scriptsDir, "vpn-director.sh")
}

// run executes vpn-director.sh with args under timeout.
func (s *VPNDirectorService) run(timeout time.Duration, args ...string) (*shell.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.executor.Exec(ctx, s.scriptPath(), args...)
}

// runChecked runs a mutating command and turns a non-zero exit into an error
// carrying the script output, so callers can show its last line. --wait makes
// the script queue for its own lock instead of exiting 0 when another
// instance is running, which used to turn a concurrent apply into a silent
// no-op reported as success.
func (s *VPNDirectorService) runChecked(timeout time.Duration, what string, args ...string) error {
	result, err := s.run(timeout, append([]string{"--wait"}, args...)...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s failed (exit %d): %s", what, result.ExitCode, result.Output)
	}
	return nil
}

// Status returns VPN Director status
func (s *VPNDirectorService) Status() (string, error) {
	result, err := s.run(StatusTimeout, "status")
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

// Apply applies VPN Director configuration
func (s *VPNDirectorService) Apply() error { return s.runChecked(ApplyTimeout, "apply", "apply") }

// Restart restarts VPN Director
func (s *VPNDirectorService) Restart() error { return s.runChecked(ApplyTimeout, "restart", "restart") }

// RestartXray restarts only Xray
func (s *VPNDirectorService) RestartXray() error {
	return s.runChecked(ApplyTimeout, "restart xray", "restart", "xray")
}

// ApplyUnlessStopped is Apply for an automatic caller, the subscription watch.
// Under its lock the script skips it, exit 0 and the marker kept, when stop has
// run since the last apply: a stop the watch queued behind stays in force.
func (s *VPNDirectorService) ApplyUnlessStopped() error {
	return s.runChecked(ApplyTimeout, "apply", "--unless-stopped", "apply")
}

// RestartXrayUnlessStopped is RestartXray for the same caller, skipped the same way.
func (s *VPNDirectorService) RestartXrayUnlessStopped() error {
	return s.runChecked(ApplyTimeout, "restart xray", "--unless-stopped", "restart", "xray")
}

// Stop stops VPN Director
func (s *VPNDirectorService) Stop() error { return s.runChecked(ApplyTimeout, "stop", "stop") }

// Update downloads fresh ipsets and reapplies VPN Director configuration
func (s *VPNDirectorService) Update() error { return s.runChecked(UpdateTimeout, "update", "update") }

// Platform runs `vpn-director.sh platform` and decodes its document. No
// --wait: the command takes no lock. The script's log() writes WARN lines to
// stderr and the executor merges the streams, so the document - printed
// last, on one line - is the last non-empty line of the output.
func (s *VPNDirectorService) Platform() (vpnconfig.PlatformInfo, error) {
	var info vpnconfig.PlatformInfo
	result, err := s.run(StatusTimeout, "platform")
	if err != nil {
		return info, err
	}
	if result.ExitCode != 0 {
		return info, fmt.Errorf("platform failed (exit %d): %s", result.ExitCode, result.Output)
	}
	doc := lastLine(result.Output)
	if err := json.Unmarshal([]byte(doc), &info); err != nil {
		return info, fmt.Errorf("decode platform info: %w", err)
	}
	return info, nil
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
