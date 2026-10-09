package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Launcher starts probers. XrayLauncher runs Xray; FakeLauncher stands in for
// it in dev mode, and the tests fake it.
type Launcher interface {
	// Ready is nil when a prober can start; ErrNoXray when there is no xray.
	Ready() error
	// Start runs a prober holding eps. A config Xray refuses comes back as a
	// *RefusedError when Xray names the endpoint.
	Start(ctx context.Context, eps []Endpoint) (Session, error)
	// Test loads eps without starting a session; ConfigError permits bisection.
	Test(ctx context.Context, eps []Endpoint) error
}

// Session is a running prober.
type Session interface {
	// Check fetches ProbeURL through the endpoint of key once and answers the
	// time to the response headers.
	Check(ctx context.Context, key string) (time.Duration, error)
	// Exited is closed when the prober ends, on its own or by Stop.
	Exited() <-chan struct{}
	Stop()
}

// ErrNoXray is a router without xray on PATH.
var ErrNoXray = errors.New("xray not found on PATH")

// RefusedError is a prober that did not start because Xray refused the
// outbound of Key.
type RefusedError struct {
	Key    string
	Reason string
}

func (e *RefusedError) Error() string {
	return "xray refused the outbound of " + e.Key + ": " + e.Reason
}

func (e *RefusedError) Unwrap() error { return &ConfigError{Reason: e.Reason} }

// ConfigError is a diagnosed config rejection, not a local launch failure.
type ConfigError struct{ Reason string }

func (e *ConfigError) Error() string { return "xray refused the config: " + e.Reason }

// ConfigRefusal identifies errors that may be bisected to isolate an outbound.
func (*ConfigError) ConfigRefusal() bool { return true }

// launchError preserves an infrastructure cause without displaying its output.
type launchError struct {
	operation string
	cause     error
}

func (e *launchError) Error() string { return e.operation }
func (e *launchError) Unwrap() error { return e.cause }

// startTimeout bounds the wait for a new prober to listen; testTimeout one
// "xray run -test". Vars so a test can shorten them.
var (
	startTimeout = 10 * time.Second
	testTimeout  = 30 * time.Second
)

// XrayLauncher runs the prober as ProbeBinary, a hard link to the xray on PATH
// under a name of its own (EnsureProbeBinary), with its config in ConfigDir.
type XrayLauncher struct {
	ProbeBinary string
	ConfigDir   string
	// ProbeURL is what a check fetches; empty means ProbeURL.
	ProbeURL string
}

// Ready links ProbeBinary to the xray on PATH, again after Entware upgraded it.
func (l *XrayLauncher) Ready() error {
	xray, err := exec.LookPath("xray")
	if err != nil {
		return ErrNoXray
	}
	if err := EnsureProbeBinary(xray, l.ProbeBinary); err != nil {
		return &launchError{operation: "prepare the probe executable", cause: err}
	}
	return nil
}

// Start writes the config for eps and runs the prober on a port free at
// start.
func (l *XrayLauncher) Start(ctx context.Context, eps []Endpoint) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, &launchError{operation: "allocate the prober port", cause: err}
	}
	accounts, err := newAccounts(len(eps))
	if err != nil {
		return nil, &launchError{operation: "create the prober accounts", cause: err}
	}
	path, err := l.writeConfig("config.json", eps, accounts, port)
	if err != nil {
		return nil, &launchError{operation: "write the prober config", cause: err}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proc, err := startProcess(l.ProbeBinary, "run", "-format", "json", "-c", path)
	if err != nil {
		return nil, &launchError{operation: "start the prober", cause: err}
	}
	if err := proc.waitListening(ctx, port, startTimeout); err != nil {
		proc.stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, errExited) {
			if proc.cmd.ProcessState.Exited() && !proc.cmd.ProcessState.Success() {
				if refused := configRejection(proc.out.String(), eps); refused != nil {
					return nil, refused
				}
			}
			return nil, &launchError{operation: "the prober exited before listening", cause: err}
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		proc.stop()
		return nil, err
	}
	s := &xraySession{proc: proc, addr: fmt.Sprintf("127.0.0.1:%d", port), url: l.ProbeURL, accounts: make(map[string]account, len(eps))}
	if s.url == "" {
		s.url = ProbeURL
	}
	for i, ep := range eps {
		s.accounts[ep.Key] = accounts[i]
	}
	return s, nil
}

// Test has Xray load a config holding eps.
func (l *XrayLauncher) Test(ctx context.Context, eps []Endpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	accounts, err := newAccounts(len(eps))
	if err != nil {
		return &launchError{operation: "create the prober accounts", cause: err}
	}
	path, err := l.writeConfig("test.json", eps, accounts, 1)
	if err != nil {
		return &launchError{operation: "write the prober test config", cause: err}
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	proc, err := startProcess(l.ProbeBinary, "run", "-test", "-format", "json", "-c", path)
	if err != nil {
		return &launchError{operation: "start the prober config test", cause: err}
	}
	if err := proc.wait(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.Exited() {
			if refused := configRejection(proc.out.String(), eps); refused != nil {
				return refused
			}
		}
		return &launchError{operation: "the prober config test failed", cause: err}
	}
	return nil
}

// writeConfig writes the prober's config as name in ConfigDir, mode 0600: it
// holds every server's credentials.
func (l *XrayLauncher) writeConfig(name string, eps []Endpoint, accounts []account, port int) (string, error) {
	cfg, err := probeConfig(eps, accounts, port)
	if err != nil {
		return "", err
	}
	if err := watchdapi.OwnedDir(l.ConfigDir, 0700, true); err != nil {
		return "", err
	}
	path := filepath.Join(l.ConfigDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, cfg, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// xraySession is a running Xray prober.
type xraySession struct {
	proc     *process
	addr     string
	url      string
	accounts map[string]account
}

func (s *xraySession) Check(ctx context.Context, key string) (time.Duration, error) {
	a, ok := s.accounts[key]
	if !ok {
		return 0, fmt.Errorf("the prober holds no endpoint %s", key)
	}
	return probeGet(ctx, s.addr, a.User, a.Pass, s.url)
}

func (s *xraySession) Exited() <-chan struct{} { return s.proc.exited }

func (s *xraySession) Stop() { s.proc.stop() }

// freePort is a TCP port on 127.0.0.1 that nothing listens on now.
func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// refusedTagRE finds the endpoint Xray could not build: it stops at the first
// such outbound and names only its tag.
var refusedTagRE = regexp.MustCompile(`failed to build outbound config with tag m(\d+)`)

// refusedTag reads the index of the endpoint whose outbound Xray refused, and
// why, from the prober's output.
func refusedTag(out string) (int, string, bool) {
	for _, line := range strings.Split(out, "\n") {
		m := refusedTagRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return i, refusalReason(line), true
	}
	return 0, "", false
}

// configRejection recognizes loader diagnostics, not arbitrary process output.
func configRejection(out string, eps []Endpoint) error {
	if i, reason, ok := refusedTag(out); ok {
		if i < len(eps) {
			return &RefusedError{Key: eps[i].Key, Reason: reason}
		}
		return &ConfigError{Reason: reason}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "infra/conf: failed to build ") || strings.Contains(line, "infra/conf/serial: failed to decode config") {
			return &ConfigError{Reason: xrayDiagnostic(out)}
		}
	}
	return nil
}

// xrayDiagnostic keeps only fixed safe segments, never a chain's final value.
func xrayDiagnostic(out string) string {
	for _, line := range strings.Split(out, "\n") {
		segs := strings.Split(line, " > ")
		start := 0
		for i, s := range segs {
			if refusedTagRE.MatchString(s) {
				start = i + 1
			}
		}
		var keep []string
		for i := start; i < len(segs)-1; i++ {
			s := strings.TrimPrefix(strings.TrimSpace(segs[i]), "infra/conf: ")
			switch s {
			case "failed to build stream settings for outbound detour", "Failed to build REALITY config.", "failed to build routing settings":
				keep = append(keep, s)
			}
		}
		if len(keep) > 0 {
			return strings.Join(keep, " > ")
		}
	}
	return "Xray refused the outbound"
}

func refusalReason(out string) string { return xrayDiagnostic(out) }

func lastLine(out string) string { return xrayDiagnostic(out) }

// EnsureProbeBinary makes link the file xray names, under a name no "pidof
// xray", "killall xray" or monit rule matching "xray" can take for the live
// Xray: rc.func's start answers "already running" when pidof finds any xray,
// and a symbolic link does not hide it, because BusyBox pidof also compares
// the resolved /proc/PID/exe. It links again when xray changed (an Entware
// upgrade) and copies where a hard link cannot cross filesystems, keeping
// xray's modification time so the next call sees the copy as current.
func EnsureProbeBinary(xray, link string) error {
	xray, err := filepath.EvalSymlinks(xray)
	if err != nil {
		return err
	}
	xi, err := os.Stat(xray)
	if err != nil {
		return err
	}
	if !xi.Mode().IsRegular() || xi.Mode().Perm()&0111 == 0 {
		return errors.New("xray is not a regular executable")
	}
	if li, err := os.Lstat(link); err == nil && li.Mode().IsRegular() && (os.SameFile(xi, li) || li.Size() == xi.Size() && li.ModTime().Equal(xi.ModTime())) {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Link(xray, tmp); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(xray, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Chtimes(tmp, xi.ModTime(), xi.ModTime()); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// KillLeftovers kills every process running bin: a prober an earlier daemon
// left behind when it died too fast for Pdeathsig.
func KillLeftovers(bin string) {
	bin, err := filepath.Abs(bin)
	if err != nil {
		return
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(bin))
	if err != nil {
		return
	}
	// Keep the leaf name: a probe hard link shares the live Xray's inode.
	bin = filepath.Join(parent, filepath.Base(bin))
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		if exe == bin || exe == bin+" (deleted)" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
