package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Xray's own words for an outbound it cannot build, the key it quotes last.
const refusalLine = `Failed to start: main: failed to load config files: [config.json] > infra/conf: failed to build outbound config with tag m1 > infra/conf: failed to build stream settings for outbound detour > infra/conf: Failed to build REALITY config. > infra/conf: invalid "password": not-a-key`

func TestRefusedTag_NamesTheEndpointAndDropsTheQuotedValue(t *testing.T) {
	i, reason, ok := refusedTag("some line\n" + refusalLine + "\n")
	if !ok || i != 1 {
		t.Fatalf("refusedTag() = %d, %q, %v", i, reason, ok)
	}
	if reason != "failed to build stream settings for outbound detour > Failed to build REALITY config." {
		t.Fatalf("reason %q", reason)
	}
	if _, _, ok := refusedTag("Failed to start: main: failed to create server"); ok {
		t.Fatal("a failure that names no tag was pinned on an endpoint")
	}
	if got := refusalReason("infra/conf: failed to build outbound config with tag m0 > infra/conf: invalid \"password\": secret"); got != "Xray refused the outbound" || strings.Contains(got, "secret") {
		t.Fatalf("reason %q", got)
	}
	if got := lastLine("a\n" + refusalLine + "\n"); strings.Contains(got, "not-a-key") {
		t.Fatalf("lastLine kept the value: %q", got)
	}
}

// script writes an executable shell script standing in for xray.
func script(t *testing.T, dir, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "vpn-director-probe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestXrayLauncher_ARefusedOutboundNamesItsEndpoint(t *testing.T) {
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo '"+refusalLine+"' >&2\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}

	_, err := l.Start(context.Background(), twoEndpoints())

	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Key != "k1" || strings.Contains(refused.Reason, "not-a-key") {
		t.Fatalf("Start() error %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "probe", "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config %v, %v: it holds every server's credentials", info, err)
	}
}

func TestXrayLauncher_AProberThatNeverListensIsStopped(t *testing.T) {
	old := startTimeout
	startTimeout = 300 * time.Millisecond
	defer func() { startTimeout = old }()
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo 'starting' >&2\nexec sleep 30"), ConfigDir: filepath.Join(dir, "probe")}

	start := time.Now()
	_, err := l.Start(context.Background(), twoEndpoints())
	if err == nil || !strings.Contains(err.Error(), "did not listen") {
		t.Fatalf("Start() error %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Start() took %s; the sleeping prober was not stopped", d)
	}
}

func TestXrayLauncher_TestReportsTheRefusalWithoutTheValue(t *testing.T) {
	dir := t.TempDir()
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo '"+refusalLine+"'\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}

	err := l.Test(context.Background(), twoEndpoints())
	if err == nil || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("Test() error %v", err)
	}
}

// The probe binary is a hard link to xray, and follows it when Entware
// replaces the file.
func TestEnsureProbeBinary_LinksAndFollowsAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	xray := filepath.Join(dir, "xray")
	link := filepath.Join(dir, "vpn-director-probe")
	if err := os.WriteFile(xray, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProbeBinary(xray, link); err != nil {
		t.Fatal(err)
	}
	same := func() bool {
		xi, _ := os.Stat(xray)
		li, err := os.Stat(link)
		return err == nil && os.SameFile(xi, li)
	}
	if !same() {
		t.Fatal("the probe binary is not a hard link to xray")
	}

	// opkg writes a new file: a new inode behind the name.
	if err := os.Remove(xray); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xray, []byte("v2, longer"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProbeBinary(xray, link); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(link); !same() || string(data) != "v2, longer" {
		t.Fatalf("the probe binary still runs %q", data)
	}
}

// With a real xray on PATH, the prober reaches each endpoint's own outbound:
// freedom answers, blackhole does not. KillLeftovers ends a prober left behind.
func TestXrayLauncher_RealXray(t *testing.T) {
	xray, err := exec.LookPath("xray")
	if err != nil {
		t.Skip("xray not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "vpn-director-probe")
	if err := EnsureProbeBinary(xray, bin); err != nil {
		t.Fatal(err)
	}
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	l := &XrayLauncher{ProbeBinary: bin, ConfigDir: filepath.Join(dir, "probe"), ProbeURL: url}
	eps := []Endpoint{
		{Key: "free", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
		{Key: "hole", Outbound: json.RawMessage(`{"protocol":"blackhole"}`)},
	}

	sess, err := l.Start(context.Background(), eps)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	if _, err := sess.Check(context.Background(), "free"); err != nil {
		t.Fatalf("freedom: %v", err)
	}
	if _, err := sess.Check(context.Background(), "hole"); err == nil {
		t.Fatal("blackhole answered")
	}

	KillLeftovers(bin)
	select {
	case <-sess.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("KillLeftovers left the prober running")
	}
}

const diagnosticSentinel = "TASK6_CREDENTIAL_SENTINEL"

func configRefusal(err error) bool {
	var refused interface{ ConfigRefusal() bool }
	return errors.As(err, &refused) && refused.ConfigRefusal()
}

func TestXrayDiagnostics_KeepOnlyKnownSafeSegments(t *testing.T) {
	for name, out := range map[string]string{
		"bare":          diagnosticSentinel,
		"unknown chain": "unknown " + diagnosticSentinel + " > another secret",
		"tagged":        strings.ReplaceAll(refusalLine, "not-a-key", diagnosticSentinel),
		"untagged":      "Failed to start: main: failed to load config files: [config.json] > infra/conf: failed to build routing settings > infra/conf: invalid rule: " + diagnosticSentinel,
		"inner value":   "infra/conf: failed to build outbound config with tag m0 > infra/conf: credential " + diagnosticSentinel + " > infra/conf: invalid password",
		"trailing":      refusalLine + "\n" + diagnosticSentinel + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, reason := range []string{refusalReason(out), lastLine(out)} {
				if strings.Contains(reason, diagnosticSentinel) || strings.Contains(reason, "not-a-key") || strings.Contains(reason, "another secret") {
					t.Fatalf("unsafe diagnostic %q", reason)
				}
				if reason == "" {
					t.Fatal("missing safe diagnostic")
				}
			}
		})
	}
	for _, out := range []string{diagnosticSentinel, "unknown " + diagnosticSentinel + " > another secret"} {
		if got := lastLine(out); got != "Xray refused the outbound" {
			t.Fatalf("unknown diagnostic %q", got)
		}
	}
}

func TestXrayLauncher_DiagnosticsNeverExposeValues(t *testing.T) {
	for name, out := range map[string]string{
		"bare":        diagnosticSentinel,
		"tagged":      strings.ReplaceAll(refusalLine, "not-a-key", diagnosticSentinel),
		"untagged":    "Failed to start: main: failed to load config files: [config.json] > infra/conf: failed to build routing settings > infra/conf: invalid rule: " + diagnosticSentinel,
		"trailing":    refusalLine + "\n" + diagnosticSentinel,
		"inner value": "infra/conf: failed to build outbound config with tag m0 > infra/conf: credential " + diagnosticSentinel + " > invalid password",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := &XrayLauncher{ProbeBinary: script(t, dir, "cat <<'DIAGNOSTIC' >&2\n"+out+"\nDIAGNOSTIC\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}
			_, startErr := l.Start(context.Background(), twoEndpoints())
			for _, err := range []error{startErr, l.Test(context.Background(), twoEndpoints())} {
				if err == nil || strings.Contains(err.Error(), diagnosticSentinel) || strings.Contains(err.Error(), "not-a-key") {
					t.Fatalf("launcher diagnostic %v", err)
				}
			}
		})
	}
}

func TestXrayLauncher_ConfigRefusalsAreTyped(t *testing.T) {
	for name, out := range map[string]string{
		"tagged":   refusalLine,
		"untagged": "Failed to start: main: failed to load config files: [config.json] > infra/conf: failed to build routing settings > invalid rule: " + diagnosticSentinel,
		"decode":   "Failed to start: main: failed to load config files: [config.json] > infra/conf/serial: failed to decode config: " + diagnosticSentinel,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := &XrayLauncher{ProbeBinary: script(t, dir, "cat <<'DIAGNOSTIC' >&2\n"+out+"\nDIAGNOSTIC\nexit 23"), ConfigDir: filepath.Join(dir, "probe")}
			_, startErr := l.Start(context.Background(), twoEndpoints())
			for _, err := range []error{startErr, l.Test(context.Background(), twoEndpoints())} {
				if !configRefusal(err) {
					t.Fatalf("config refusal is not typed: %T, %v", err, err)
				}
			}
		})
	}
}

func TestXrayLauncher_InfrastructureIsNotAConfigRefusal(t *testing.T) {
	for _, name := range []string{"exec", "filesystem", "exit", "signaled"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := &XrayLauncher{ProbeBinary: filepath.Join(dir, "missing"), ConfigDir: filepath.Join(dir, "probe")}
			switch name {
			case "filesystem":
				if err := os.WriteFile(l.ConfigDir, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			case "exit":
				l.ProbeBinary = script(t, dir, "echo 'Failed to start: main: failed to create server > failed to listen > "+diagnosticSentinel+"' >&2\nexit 23")
			case "signaled":
				l.ProbeBinary = script(t, dir, "echo '"+refusalLine+"' >&2\nkill -KILL $$")
			}
			_, startErr := l.Start(context.Background(), twoEndpoints())
			for _, err := range []error{startErr, l.Test(context.Background(), twoEndpoints())} {
				if err == nil || configRefusal(err) || strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), diagnosticSentinel) {
					t.Fatalf("infrastructure misclassified: %T, %v", err, err)
				}
				if name == "exec" && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("exec cause lost: %v", err)
				}
				if name == "filesystem" {
					var pe *os.PathError
					if !errors.As(err, &pe) {
						t.Fatalf("filesystem cause lost: %v", err)
					}
				}
			}
		})
	}
}

func scriptPID(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid %q: %v", data, err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	return pid
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("prober PID %d is still present: %v", pid, err)
	}
}

func TestXrayLauncher_StartTimeoutActuallyReapsTheProcess(t *testing.T) {
	old := startTimeout
	startTimeout = 200 * time.Millisecond
	defer func() { startTimeout = old }()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo $$ > '"+pidFile+"'\nexec sleep 30"), ConfigDir: filepath.Join(dir, "probe")}
	_, err := l.Start(context.Background(), twoEndpoints())
	pid := scriptPID(t, pidFile)
	if err == nil || !strings.Contains(err.Error(), "did not listen") || configRefusal(err) {
		t.Fatalf("start timeout: %v", err)
	}
	requireProcessGone(t, pid)
}

func TestXrayLauncher_CancellationAndTimeoutArePreserved(t *testing.T) {
	for _, method := range []string{"Start", "Test"} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			l := &XrayLauncher{ProbeBinary: script(t, dir, "echo $$ > '"+pidFile+"'\nexec sleep 30"), ConfigDir: filepath.Join(dir, "probe")}
			run := func(ctx context.Context) error {
				if method == "Test" {
					return l.Test(ctx, twoEndpoints())
				}
				_, err := l.Start(ctx, twoEndpoints())
				return err
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run(ctx); !errors.Is(err, context.Canceled) || configRefusal(err) {
				t.Fatalf("cancelled: %v", err)
			}
			if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancelled launch started a process: %v", err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) || configRefusal(err) {
				t.Fatalf("deadline: %v", err)
			}
			requireProcessGone(t, scriptPID(t, pidFile))
		})
	}
	old := testTimeout
	testTimeout = 200 * time.Millisecond
	defer func() { testTimeout = old }()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	l := &XrayLauncher{ProbeBinary: script(t, dir, "echo $$ > '"+pidFile+"'\nexec sleep 30"), ConfigDir: filepath.Join(dir, "probe")}
	if err := l.Test(context.Background(), twoEndpoints()); !errors.Is(err, context.DeadlineExceeded) || configRefusal(err) {
		t.Fatalf("test timeout: %v", err)
	}
	requireProcessGone(t, scriptPID(t, pidFile))
}

func TestEnsureProbeBinary_ResolvesSourceAndReplacesDestinationSymlinks(t *testing.T) {
	for _, destination := range []string{"source", "fresh copy"} {
		t.Run(destination, func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "installed")
			source := filepath.Join(dir, "xray")
			link := filepath.Join(dir, "vpn-director-probe")
			if err := os.WriteFile(real, []byte("v1"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, source); err != nil {
				t.Fatal(err)
			}
			target := real
			if destination == "fresh copy" {
				target = filepath.Join(dir, "copy")
				if err := os.WriteFile(target, []byte("v1"), 0755); err != nil {
					t.Fatal(err)
				}
				info, _ := os.Stat(real)
				if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := EnsureProbeBinary(source, link); err != nil {
				t.Fatal(err)
			}
			li, err := os.Lstat(link)
			xi, _ := os.Stat(real)
			if err != nil || !li.Mode().IsRegular() || !os.SameFile(xi, li) {
				t.Fatalf("probe destination is not a regular hard link: %v, %v", li, err)
			}
			if _, err := os.Lstat(link + ".tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publication left a temporary link: %v", err)
			}
		})
	}
}

func TestEnsureProbeBinary_CopiesAcrossFilesystemsAndFollowsAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	xray := filepath.Join(dir, "xray")
	if err := os.WriteFile(xray, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	other, err := os.MkdirTemp("/dev/shm", "vpn-director-task6-")
	if err != nil {
		t.Skip("no writable cross-filesystem temporary directory")
	}
	t.Cleanup(func() { os.RemoveAll(other) })
	link := filepath.Join(other, "vpn-director-probe")
	if err := os.Link(xray, link); !errors.Is(err, syscall.EXDEV) {
		t.Skipf("cross-filesystem copy cannot be tested here: %v", err)
	}
	for _, contents := range []string{"v1", "v2, longer"} {
		if err := os.WriteFile(xray, []byte(contents), 0755); err != nil {
			t.Fatal(err)
		}
		if err := EnsureProbeBinary(xray, link); err != nil {
			t.Fatal(err)
		}
		xi, _ := os.Stat(xray)
		li, err := os.Lstat(link)
		got, readErr := os.ReadFile(link)
		if err != nil || readErr != nil || !li.Mode().IsRegular() || os.SameFile(xi, li) || string(got) != contents || !li.ModTime().Equal(xi.ModTime()) || li.Mode().Perm()&0111 == 0 {
			t.Fatalf("copy failed: %v, %v, %v", li, err, readErr)
		}
		if err := EnsureProbeBinary(xray, link); err != nil {
			t.Fatal(err)
		}
		again, _ := os.Lstat(link)
		if !os.SameFile(li, again) {
			t.Fatal("a current copy was unnecessarily replaced")
		}
	}
}

func TestXrayLauncher_ReadyLinksTheExecutableOrReportsNoXray(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	l := &XrayLauncher{ProbeBinary: filepath.Join(dir, "vpn-director-probe")}
	if err := l.Ready(); !errors.Is(err, ErrNoXray) {
		t.Fatalf("missing xray: %v", err)
	}
	real := filepath.Join(dir, "installed")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "xray")); err != nil {
		t.Fatal(err)
	}
	if err := l.Ready(); err != nil {
		t.Fatal(err)
	}
	li, _ := os.Lstat(l.ProbeBinary)
	xi, _ := os.Stat(real)
	if li == nil || !li.Mode().IsRegular() || !os.SameFile(xi, li) {
		t.Fatal("Ready did not publish the resolved executable as a hard link")
	}
}

func TestTail_BoundsFinishedAndUnfinishedOutput(t *testing.T) {
	for name, chunks := range map[string][]string{
		"unfinished":       {strings.Repeat("x", 512*1024)},
		"split unfinished": {strings.Repeat("x", 48*1024), strings.Repeat("x", 48*1024)},
		"finished":         {strings.Repeat("x", 512*1024) + "\n"},
		"many lines":       {strings.Repeat(strings.Repeat("x", 16*1024)+"\n", 20)},
	} {
		t.Run(name, func(t *testing.T) {
			tail := &tail{max: 20}
			for _, chunk := range chunks {
				if n, err := tail.Write([]byte(chunk)); err != nil || n != len(chunk) {
					t.Fatalf("Write() = %d, %v", n, err)
				}
				if n := len(tail.String()); n > 64*1024 {
					t.Fatalf("retained %d bytes", n)
				}
			}
			tail.Write([]byte("\nlast\npart"))
			if !strings.HasSuffix(tail.String(), "last\npart") {
				t.Fatal("latest output was lost")
			}
		})
	}
	tail := &tail{max: 2}
	tail.Write([]byte("first\nsecond\nthird\npart"))
	if got := tail.String(); got != "second\nthird\npart" {
		t.Fatalf("line bound: %q", got)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				tail.Write([]byte("line\n"))
				_ = tail.String()
			}
		}()
	}
	wg.Wait()
}

func TestFakeLauncher_StableResultsCancellationAndStop(t *testing.T) {
	var l Launcher = FakeLauncher{}
	if err := l.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := l.Test(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Start(context.Background(), []Endpoint{{Key: "00"}}); err == nil {
		t.Fatal("fake did not refuse its rejected key")
	}
	sess, err := l.Start(context.Background(), []Endpoint{{Key: "33"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	if latency, err := sess.Check(context.Background(), "33"); err != nil || latency != 152*time.Millisecond {
		t.Fatalf("fake live check: %s, %v", latency, err)
	}
	if _, err := sess.Check(context.Background(), "20"); err == nil {
		t.Fatal("fake dead check answered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Check(ctx, "33"); !errors.Is(err, context.Canceled) {
		t.Fatalf("fake cancellation: %v", err)
	}
	sess.Stop()
	sess.Stop()
	select {
	case <-sess.Exited():
	default:
		t.Fatal("fake Stop did not close Exited")
	}
}

const (
	helperRole     = "VPD_TASK6_R1_HELPER_ROLE"
	helperMethod   = "VPD_TASK6_R1_HELPER_METHOD"
	helperRoot     = "VPD_TASK6_R1_HELPER_ROOT"
	helperReady    = "VPD_TASK6_R1_HELPER_READY"
	helperScenario = "VPD_TASK6_R2_SCENARIO"
)

func TestMain(m *testing.M) {
	if role := os.Getenv(helperRole); role != "" {
		if err := runProcessHelper(role); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type helperIdentity struct {
	PID     int
	PPID    int
	State   string
	Started string
	Exe     string
}

func readHelperIdentity(pid int) (helperIdentity, error) {
	return readHelperIdentityWith(pid, os.ReadFile, os.Readlink)
}

func readHelperIdentityWith(pid int, readFile func(string) ([]byte, error), readlink func(string) (string, error)) (helperIdentity, error) {
	id, err := readHelperStat(pid, readFile)
	if err != nil || id.State == "Z" || id.State == "X" {
		return id, err
	}
	id.Exe, err = readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if errors.Is(err, os.ErrNotExist) {
		// The executable can disappear before stat reports termination.
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			terminated, statErr := readHelperStat(pid, readFile)
			if statErr != nil || terminated.Started != id.Started {
				break
			}
			if terminated.State == "Z" || terminated.State == "X" {
				return terminated, nil
			}
			if !time.Now().Before(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	return id, err
}

func readHelperStat(pid int, readFile func(string) ([]byte, error)) (helperIdentity, error) {
	id := helperIdentity{PID: pid}
	data, err := readFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return id, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return id, errors.New("invalid helper process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return id, errors.New("short helper process stat")
	}
	id.State, id.Started = fields[0], fields[19]
	id.PPID, err = strconv.Atoi(fields[1])
	return id, err
}

func runProcessHelper(role string) error {
	switch role {
	case "child":
		if os.Getenv(helperMethod) == "Start" {
			var path string
			for i, arg := range os.Args {
				if arg == "-c" && i+1 < len(os.Args) {
					path = os.Args[i+1]
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var cfg struct{ Inbounds []struct{ Port int } }
			if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Inbounds) != 1 {
				return errors.New("invalid helper inbound")
			}
			l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.Inbounds[0].Port))
			if err != nil {
				return err
			}
			defer l.Close()
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
		}
		id, err := readHelperIdentity(os.Getpid())
		if err != nil {
			return err
		}
		data, err := json.Marshal(id)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(os.Getenv(helperRoot), "probe-owned"), data, 0600); err != nil {
			return err
		}
		if strings.HasPrefix(os.Getenv(helperScenario), "before-ready") && os.Getenv(helperMethod) != "idle" {
			if err := waitHelperGate(context.Background(), filepath.Join(os.Getenv(helperRoot), "publish-child")); err != nil {
				return err
			}
		}
		if err := os.WriteFile(os.Getenv(helperReady), data, 0600); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	case "parent":
		if err := os.Setenv(helperRole, "child"); err != nil {
			return err
		}
		startTimeout, testTimeout = time.Hour, time.Hour
		root := os.Getenv(helperRoot)
		l := &XrayLauncher{ProbeBinary: filepath.Join(root, "vpn-director-probe"), ConfigDir: filepath.Join(root, "config")}
		if os.Getenv(helperMethod) == "Test" {
			return l.Test(context.Background(), twoEndpoints())
		}
		sess, err := l.Start(context.Background(), twoEndpoints())
		if err != nil {
			return err
		}
		defer sess.Stop()
		if err := os.WriteFile(filepath.Join(root, "parent-ready"), []byte("started"), 0600); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	case "supervisor":
		return superviseParentDeath()
	case "watchdog":
		return exerciseWatchdog()
	default:
		return errors.New("unknown process helper role")
	}
}

func helperEnvironment(role, method, root, ready string) []string {
	return append(os.Environ(), helperRole+"="+role, helperMethod+"="+method, helperRoot+"="+root, helperReady+"="+ready)
}

// The isolated supervisor adopts and reaps children after their parent dies.
func superviseParentDeath() (result error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if err := helperSubreaper(); err != nil {
		return err
	}
	root, method := os.Getenv(helperRoot), os.Getenv(helperMethod)
	ready := filepath.Join(root, "child-ready")
	self, err := os.Executable()
	if err != nil {
		return err
	}
	parent := exec.Command(self)
	parent.Env = helperEnvironment("parent", method, root, ready)
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parent.WaitDelay = 2 * time.Second
	parentDone := make(chan struct{})
	var parentErr error
	var report helperCleanupReport
	var started, normalReaped bool
	defer func() {
		if !started {
			return
		}
		if !normalReaped {
			cleanupErr := cleanupParentGroup(parent, parentDone, &report)
			result = errors.Join(result, cleanupErr)
			if cleanupErr != nil {
				return
			}
		}
		if parent.ProcessState == nil || !parent.ProcessState.Sys().(syscall.WaitStatus).Signaled() || parent.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			result = errors.Join(result, fmt.Errorf("parent Wait did not reap SIGKILL: %v", parentErr))
			return
		}
		report.ParentWaited = true
		data, err := json.Marshal(report)
		if err == nil {
			err = os.WriteFile(filepath.Join(root, "cleanup-complete"), data, 0600)
		}
		result = errors.Join(result, err)
	}()
	if err := parent.Start(); err != nil {
		return err
	}
	started = true
	report.Parent, report.Group = parent.Process.Pid, parent.Process.Pid
	go func() { parentErr = parent.Wait(); close(parentDone) }()
	if group, err := syscall.Getpgid(parent.Process.Pid); err != nil || group != report.Group || group == syscall.Getpgrp() {
		return errors.New("parent helper did not establish its isolated process group")
	}
	var child helperIdentity
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(ready)
		if err == nil && json.Unmarshal(data, &child) == nil && child.PID > 0 {
			break
		}
		select {
		case <-parentDone:
			return errors.New("launcher parent exited before its child was ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if child.PID <= 0 {
		return errors.New("launcher parent did not publish its child")
	}
	current, err := readHelperIdentity(child.PID)
	if err != nil || current.Started != child.Started || current.PPID != parent.Process.Pid || current.Exe != filepath.Join(root, "vpn-director-probe") || current.State == "Z" || current.State == "X" {
		return errors.New("launcher child is not the owned running executable")
	}
	if group, err := syscall.Getpgid(child.PID); err != nil || group != report.Group {
		return errors.New("probe did not inherit its owned parent process group")
	}
	if method == "Start" {
		readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := waitHelperGate(readyCtx, filepath.Join(root, "parent-ready"))
		cancel()
		if err != nil {
			return fmt.Errorf("Start never returned its running session: %w", err)
		}
		if _, err := os.Stat(filepath.Join(root, "parent-ready")); err != nil {
			return errors.New("Start never returned its running session")
		}
	}
	if scenario := os.Getenv(helperScenario); scenario != "" && !strings.HasPrefix(scenario, "before-ready") {
		if err := os.WriteFile(filepath.Join(root, "watchdog-ready"), []byte("ready"), 0600); err != nil {
			return err
		}
		if err := waitHelperGate(ctx, filepath.Join(root, "kill-parent")); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.Process.Kill(); err != nil {
		return err
	}
	select {
	case <-parentDone:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return errors.New("parent waiter did not complete after parent-only SIGKILL")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		id, err := readHelperIdentity(child.PID)
		if err != nil || id.Started != child.Started {
			return errors.New("owned child identity disappeared before reaping")
		}
		if id.State == "Z" || id.State == "X" {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(child.PID, &status, 0, nil)
			if err != nil || pid != child.PID || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				return errors.New("child did not die by SIGKILL or was not reaped")
			}
			normalReaped = true
			report.Children = append(report.Children, pid)
			fmt.Printf("%s: owned child terminated as %s after parent SIGKILL and was reaped\n", method, id.State)
			return nil
		}
		if id.Exe != child.Exe {
			return errors.New("owned child executable identity changed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: owned child is still running after parent SIGKILL", method)
}

type ownedHelper struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func startOwnedHelper(t *testing.T, bin, ready string) *ownedHelper {
	t.Helper()
	p := &ownedHelper{cmd: exec.Command(bin), exited: make(chan struct{})}
	p.cmd.Env = helperEnvironment("child", "idle", filepath.Dir(ready), ready)
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); <-p.exited })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ready)
		var id helperIdentity
		if err == nil && json.Unmarshal(data, &id) == nil && id.PID == p.cmd.Process.Pid && id.State != "Z" && id.State != "X" {
			return p
		}
		select {
		case <-p.exited:
			t.Fatal("owned executable exited before readiness")
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("owned executable never became ready")
	return nil
}

func TestKillLeftovers_CanonicalPathKillsOnlyTheProbe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux executable identity regression")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alias", "relative alias", "deleted", "replaced", "leaf symlink", "unresolvable parent"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			real, alias := filepath.Join(root, "real"), filepath.Join(root, "alias")
			if err := os.Mkdir(real, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, alias); err != nil {
				t.Fatal(err)
			}
			probe, live := filepath.Join(real, "vpn-director-probe"), filepath.Join(real, "xray")
			if err := copyFile(self, probe); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(probe, live); err != nil {
				t.Fatal(err)
			}
			pi, _ := os.Stat(probe)
			li, _ := os.Stat(live)
			if !os.SameFile(pi, li) {
				t.Fatal("isolation control does not share the probe inode")
			}
			prober := startOwnedHelper(t, filepath.Join(alias, "vpn-director-probe"), filepath.Join(root, "probe-ready"))
			active := startOwnedHelper(t, live, filepath.Join(root, "live-ready"))
			if name == "deleted" || name == "replaced" || name == "leaf symlink" {
				if err := os.Remove(probe); err != nil {
					t.Fatal(err)
				}
				if name == "replaced" {
					if err := copyFile(self, probe); err != nil {
						t.Fatal(err)
					}
				}
				if name == "leaf symlink" {
					if err := os.Symlink(live, probe); err != nil {
						t.Fatal(err)
					}
				}
				id, err := readHelperIdentity(prober.cmd.Process.Pid)
				if err != nil || id.Exe != probe+" (deleted)" {
					t.Fatalf("deleted executable identity: %+v, %v", id, err)
				}
			}
			bin := filepath.Join(alias, "vpn-director-probe")
			if name == "relative alias" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				bin, err = filepath.Rel(cwd, bin)
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "unresolvable parent" {
				bin = filepath.Join(root, "missing", "vpn-director-probe")
			}
			KillLeftovers(bin)
			if name != "unresolvable parent" {
				select {
				case <-prober.exited:
				case <-time.After(time.Second):
					t.Fatal("canonical cleanup left the owned probe running")
				}
				requireProcessGone(t, prober.cmd.Process.Pid)
			} else {
				select {
				case <-prober.exited:
					t.Fatal("unresolvable parent widened the cleanup target")
				default:
				}
			}
			time.Sleep(50 * time.Millisecond)
			select {
			case <-active.exited:
				t.Fatal("cleanup killed the same-inode live-Xray sibling")
			default:
			}
			id, err := readHelperIdentity(active.cmd.Process.Pid)
			if err != nil || id.Exe != live || id.State == "Z" || id.State == "X" {
				t.Fatalf("live-Xray sibling is not running: %+v, %v", id, err)
			}
		})
	}
}

func TestXrayLauncher_ParentDeathKillsBothLaunchPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux parent-death regression")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"Start", "Test"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			if err := copyFile(self, filepath.Join(root, "vpn-director-probe")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			run, err := startParentDeathSupervisor(ctx, self, method, root)
			if err != nil {
				t.Fatal(err)
			}
			out, err := run.wait(ctx)
			if err != nil {
				t.Fatalf("parent-death helper: %v\n%s", err, out)
			}
			t.Log(strings.TrimSpace(string(out)))
		})
	}
}

func helperSubreaper() error {
	prctl := map[string]uintptr{"amd64": 157, "arm64": 167, "386": 172, "arm": 172, "mipsle": 4192}[runtime.GOARCH]
	if prctl == 0 {
		return errors.New("unsupported Linux subreaper architecture")
	}
	if _, _, errno := syscall.Syscall6(prctl, 36, 1, 0, 0, 0, 0); errno != 0 { // PR_SET_CHILD_SUBREAPER
		return errno
	}
	return nil
}

type supervisorRun struct {
	cmd          *exec.Cmd
	out          *tail
	err          error
	cancelErr    error
	exited       chan struct{}
	cancelExited chan struct{}
}

func startParentDeathSupervisor(ctx context.Context, self, method, root string) (*supervisorRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &supervisorRun{cmd: exec.Command(self), out: &tail{max: 40}, exited: make(chan struct{}), cancelExited: make(chan struct{})}
	r.cmd.Env = helperEnvironment("supervisor", method, root, "")
	r.cmd.Stdout, r.cmd.Stderr = r.out, r.out
	// No CommandContext: cancellation must not kill the surviving subreaper.
	r.cmd.WaitDelay = 2 * time.Second
	if err := r.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { r.err = r.cmd.Wait(); close(r.exited) }()
	go func() {
		defer close(r.cancelExited)
		select {
		case <-ctx.Done():
			r.cancelErr = r.cmd.Process.Signal(syscall.SIGTERM)
			if errors.Is(r.cancelErr, os.ErrProcessDone) {
				r.cancelErr = nil
			}
		case <-r.exited:
		}
	}()
	return r, nil
}

func (r *supervisorRun) wait(ctx context.Context) ([]byte, error) {
	<-r.exited
	<-r.cancelExited
	if ctx.Err() != nil {
		return []byte(r.out.String()), errors.Join(ctx.Err(), r.cancelErr)
	}
	return []byte(r.out.String()), errors.Join(r.err, r.cancelErr)
}

func waitHelperGate(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func helperIsRunning(id helperIdentity, exe string) bool {
	current, err := readHelperIdentity(id.PID)
	return err == nil && current.Started == id.Started && current.Exe == exe && current.State != "Z" && current.State != "X"
}

func readFixtureChild(ctx context.Context, root string) (helperIdentity, error) {
	for {
		data, err := os.ReadFile(filepath.Join(root, "probe-owned"))
		var id helperIdentity
		if err == nil && json.Unmarshal(data, &id) == nil && id.PID > 0 && id.Exe == filepath.Join(root, "vpn-director-probe") {
			return id, nil
		}
		select {
		case <-ctx.Done():
			return id, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Only this isolated driver is a backup reaper for a failing supervisor.
func reapWatchdogFixture(run *supervisorRun, parent, child helperIdentity) error {
	for _, id := range []helperIdentity{parent, child} {
		if id.PID <= 0 {
			continue
		}
		current, err := readHelperIdentity(id.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || current.Started != id.Started || current.State != "Z" && current.State != "X" && current.Exe != id.Exe {
			return errors.New("backup cleanup refuses an unverified helper PID")
		}
		if current.State != "Z" && current.State != "X" {
			if err := syscall.Kill(id.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	select {
	case <-run.exited:
	case <-time.After(8 * time.Second):
		return errors.New("supervisor waiter did not complete after cleanup request")
	}
	select {
	case <-run.cancelExited:
	case <-time.After(time.Second):
		return errors.New("supervisor cancellation waiter did not complete")
	}
	for _, id := range []helperIdentity{parent, child} {
		if id.PID <= 0 {
			continue
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(id.PID, &status, syscall.WNOHANG, nil)
			if pid == id.PID {
				if !status.Signaled() || status.Signal() != syscall.SIGKILL {
					return errors.New("backup cleanup reaped an unexpected helper status")
				}
				fmt.Printf("backup owner reaped owned PID %d by Wait4\n", pid)
			}
			_, identityErr := readHelperIdentity(id.PID)
			if errors.Is(identityErr, os.ErrNotExist) && (pid == id.PID || errors.Is(err, syscall.ECHILD)) {
				break
			}
			if err != nil && !errors.Is(err, syscall.EINTR) && !errors.Is(err, syscall.ECHILD) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("backup cleanup did not reap an owned helper")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

func exerciseWatchdog() (result error) {
	if err := helperSubreaper(); err != nil {
		return err
	}
	root, method, scenario := os.Getenv(helperRoot), os.Getenv(helperMethod), os.Getenv(helperScenario)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	live := filepath.Join(root, "xray")
	if err := os.Link(filepath.Join(root, "vpn-director-probe"), live); err != nil {
		return err
	}
	pi, _ := os.Stat(filepath.Join(root, "vpn-director-probe"))
	li, _ := os.Stat(live)
	if !os.SameFile(pi, li) {
		return errors.New("watchdog sibling does not share the probe inode")
	}
	sibling := exec.Command(live)
	sibling.Env = helperEnvironment("child", "idle", root, filepath.Join(root, "sibling-ready"))
	if err := sibling.Start(); err != nil {
		return err
	}
	defer func() {
		_ = sibling.Process.Kill()
		var exit *exec.ExitError
		if err := sibling.Wait(); !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			result = errors.Join(result, errors.New("sibling cleanup did not complete its SIGKILL Wait"))
		}
	}()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := waitHelperGate(readyCtx, filepath.Join(root, "sibling-ready")); err != nil {
		return err
	}
	siblingID, err := readHelperIdentity(sibling.Process.Pid)
	if err != nil || !helperIsRunning(siblingID, live) {
		return errors.New("watchdog isolation sibling is not running")
	}
	limit := 12 * time.Second
	if scenario == "deadline" {
		limit = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	run, err := startParentDeathSupervisor(ctx, self, method, root)
	if err != nil {
		return err
	}
	var parent, child helperIdentity
	defer func() {
		cancel()
		result = errors.Join(result, reapWatchdogFixture(run, parent, child))
	}()
	child, err = readFixtureChild(ctx, root)
	if err != nil {
		return err
	}
	parent, err = readHelperIdentity(child.PPID)
	if err != nil || parent.PPID != run.cmd.Process.Pid || !helperIsRunning(parent, self) || !helperIsRunning(child, filepath.Join(root, "vpn-director-probe")) {
		return errors.New("watchdog fixture was not a verified running parent/probe tree")
	}
	parentGroup, parentGroupErr := syscall.Getpgid(parent.PID)
	probeGroup, probeGroupErr := syscall.Getpgid(child.PID)
	supervisorGroup, supervisorGroupErr := syscall.Getpgid(run.cmd.Process.Pid)
	siblingGroup, siblingGroupErr := syscall.Getpgid(sibling.Process.Pid)
	if parentGroupErr != nil || probeGroupErr != nil || supervisorGroupErr != nil || siblingGroupErr != nil || parentGroup != parent.PID || probeGroup != parentGroup || supervisorGroup == parentGroup || siblingGroup == parentGroup {
		return errors.New("watchdog subtree is not an independently established isolated process group")
	}
	if !strings.HasPrefix(scenario, "before-ready") {
		if err := waitHelperGate(ctx, filepath.Join(root, "watchdog-ready")); err != nil {
			return err
		}
	} else if _, err := os.Stat(filepath.Join(root, "child-ready")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("early-failure fixture already published readiness")
	}
	if scenario == "cancel" || scenario == "before-ready-cancel" {
		cancel()
	}
	out, waitErr := run.wait(ctx)
	var failures []error
	if scenario == "before-ready-failure" {
		if waitErr == nil || !strings.Contains(string(out), "launcher parent did not publish its child") {
			failures = append(failures, fmt.Errorf("readiness failure was not reported: %v, %s", waitErr, out))
		}
	} else if !errors.Is(waitErr, ctx.Err()) || waitErr == nil {
		failures = append(failures, fmt.Errorf("watchdog cause was not preserved: %v", waitErr))
	}
	for label, id := range map[string]helperIdentity{"parent": parent, "probe": child} {
		current, err := readHelperIdentity(id.PID)
		if !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("watchdog left owned %s unreaped (state %s, error %v)", label, current.State, err))
		}
	}
	data, reportErr := os.ReadFile(filepath.Join(root, "cleanup-complete"))
	var report helperCleanupReport
	if reportErr != nil || json.Unmarshal(data, &report) != nil || !report.ParentWaited || report.Parent != parent.PID || report.Group != parent.PID || !report.GroupKilled {
		failures = append(failures, errors.New("supervisor did not publish completed cleanup/reaping"))
	}
	childReaped := false
	for _, pid := range report.Children {
		if pid == child.PID {
			childReaped = true
		}
	}
	if !childReaped {
		failures = append(failures, errors.New("probe was not explicitly reaped by the supervisor Wait4"))
	}
	if _, err := readHelperIdentity(run.cmd.Process.Pid); !errors.Is(err, os.ErrNotExist) || run.cmd.ProcessState == nil {
		failures = append(failures, errors.New("supervisor was not reaped by its owning Wait"))
	}
	if !helperIsRunning(siblingID, live) {
		failures = append(failures, errors.New("watchdog killed the unrelated same-inode sibling"))
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	fmt.Printf("%s/%s: expected failure %v; %s\n", method, scenario, waitErr, strings.TrimSpace(string(out)))
	fmt.Printf("%s/%s: watchdog cause retained; parent/probe reaped; supervisor Wait/output complete; unrelated sibling running\n", method, scenario)
	return nil
}

func TestXrayLauncher_WatchdogCleansAndReapsOwnedSubtree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux watchdog/subreaper regression")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"Start", "Test"} {
		for _, scenario := range []string{"cancel", "deadline", "before-ready-cancel", "before-ready-failure"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				if err := copyFile(self, filepath.Join(root, "vpn-director-probe")); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(self)
				cmd.Env = append(helperEnvironment("watchdog", method, root, ""), helperScenario+"="+scenario)
				cmd.WaitDelay = 2 * time.Second
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("watchdog helper: %v\n%s", err, out)
				}
				requireProcessGone(t, cmd.Process.Pid)
				t.Log(strings.TrimSpace(string(out)))
			})
		}
	}
}

type helperCleanupReport struct {
	Parent       int
	Group        int
	ParentWaited bool
	GroupKilled  bool
	Children     []int
}

// This group was created by Setpgid at Start and never contains the subreaper.
func cleanupParentGroup(parent *exec.Cmd, parentDone <-chan struct{}, report *helperCleanupReport) error {
	if report.Group <= 0 || report.Group == syscall.Getpgrp() || report.Group != parent.Process.Pid {
		return errors.New("cleanup refuses an unowned process group")
	}
	if err := syscall.Kill(-report.Group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("owned process-group termination failed: %w", err)
	}
	report.GroupKilled = true
	select {
	case <-parentDone:
	case <-time.After(3 * time.Second):
		return errors.New("cleanup did not complete the parent Wait")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-report.Group, &status, syscall.WNOHANG, nil)
		if pid > 0 {
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				return errors.New("owned group child did not terminate by SIGKILL")
			}
			report.Children = append(report.Children, pid)
			fmt.Printf("supervisor reaped owned PID %d by Wait4\n", pid)
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("owned process-group reaping failed: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("cleanup did not reap the owned process group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelperIdentity_MissingExeObservationIsBounded(t *testing.T) {
	started := time.Now()
	id, err := readHelperIdentityWith(123, func(string) ([]byte, error) {
		return []byte("123 (probe) R 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 42"), nil
	}, func(string) (string, error) { return "", os.ErrNotExist })
	if err == nil || id.State == "Z" || id.State == "X" {
		t.Fatalf("running process was accepted as terminated: id=%+v err=%v", id, err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("identity observation exceeded its bound: %s", elapsed)
	}
}

func TestHelperIdentity_ExeDisappearanceWaitsForTheSameExit(t *testing.T) {
	stat := func(state string) []byte {
		return []byte("123 (probe) " + state + " 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 42")
	}
	for _, state := range []string{"Z", "X"} {
		t.Run(state, func(t *testing.T) {
			reads := 0
			id, err := readHelperIdentityWith(123, func(string) ([]byte, error) {
				reads++
				if reads < 4 {
					return stat("R"), nil
				}
				return stat(state), nil
			}, func(string) (string, error) { return "", os.ErrNotExist })
			if err != nil || id.PID != 123 || id.Started != "42" || id.State != state || reads != 4 {
				t.Fatalf("exit transition not observed: id=%+v err=%v reads=%d", id, err, reads)
			}
		})
	}
}

func TestHelperIdentity_ExeDisappearanceRequiresTheSameTerminatedProcess(t *testing.T) {
	stat := func(state, started string) []byte {
		fields := strings.Fields(state + " 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 " + started)
		return []byte("123 (probe) " + strings.Join(fields, " "))
	}
	for _, tc := range []struct {
		name, state, started string
		statErr, exeErr      error
		wantOK               bool
	}{
		{"zombie", "Z", "42", nil, os.ErrNotExist, true},
		{"dead", "X", "42", nil, os.ErrNotExist, true},
		{"pid reused", "Z", "43", nil, os.ErrNotExist, false},
		{"still running", "S", "42", nil, os.ErrNotExist, false},
		{"disappeared", "", "", os.ErrNotExist, os.ErrNotExist, false},
		{"permission error", "Z", "42", nil, os.ErrPermission, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			id, err := readHelperIdentityWith(123, func(string) ([]byte, error) {
				reads++
				if reads == 1 {
					return stat("S", "42"), nil
				}
				return stat(tc.state, tc.started), tc.statErr
			}, func(string) (string, error) { return "", tc.exeErr })
			if tc.wantOK {
				if err != nil || id.PID != 123 || id.Started != "42" || id.State != tc.state || reads != 2 {
					t.Fatalf("same-process termination not confirmed: id=%+v err=%v reads=%d", id, err, reads)
				}
			} else if err == nil {
				t.Fatalf("unverified identity was accepted: %+v", id)
			}
		})
	}
}
