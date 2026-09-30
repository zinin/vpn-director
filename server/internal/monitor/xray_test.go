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
	helperRole   = "VPD_TASK6_R1_HELPER_ROLE"
	helperMethod = "VPD_TASK6_R1_HELPER_METHOD"
	helperRoot   = "VPD_TASK6_R1_HELPER_ROOT"
	helperReady  = "VPD_TASK6_R1_HELPER_READY"
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
	id := helperIdentity{PID: pid}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
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
	if err != nil || id.State == "Z" || id.State == "X" {
		return id, err
	}
	id.Exe, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
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
	default:
		return errors.New("unknown process helper role")
	}
}

func helperEnvironment(role, method, root, ready string) []string {
	return append(os.Environ(), helperRole+"="+role, helperMethod+"="+method, helperRoot+"="+root, helperReady+"="+ready)
}

// The isolated supervisor adopts and reaps children after their parent dies.
func superviseParentDeath() error {
	prctl := map[string]uintptr{"amd64": 157, "arm64": 167, "386": 172, "arm": 172, "mipsle": 4192}[runtime.GOARCH]
	if prctl == 0 {
		return errors.New("unsupported Linux subreaper architecture")
	}
	if _, _, errno := syscall.Syscall6(prctl, 36, 1, 0, 0, 0, 0); errno != 0 { // PR_SET_CHILD_SUBREAPER
		return errno
	}
	root, method := os.Getenv(helperRoot), os.Getenv(helperMethod)
	ready := filepath.Join(root, "child-ready")
	self, err := os.Executable()
	if err != nil {
		return err
	}
	parent := exec.Command(self)
	parent.Env = helperEnvironment("parent", method, root, ready)
	if err := parent.Start(); err != nil {
		return err
	}
	parentDone := make(chan struct{})
	go func() { _ = parent.Wait(); close(parentDone) }()
	defer func() { _ = parent.Process.Kill(); <-parentDone }()
	var child helperIdentity
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ready)
		if err == nil && json.Unmarshal(data, &child) == nil && child.PID > 0 {
			break
		}
		select {
		case <-parentDone:
			return errors.New("launcher parent exited before its child was ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if child.PID <= 0 {
		return errors.New("launcher parent did not publish its child")
	}
	defer func() {
		_ = parent.Process.Kill()
		<-parentDone
		id, err := readHelperIdentity(child.PID)
		if err == nil && id.Started == child.Started {
			if id.State != "Z" && id.State != "X" && id.Exe == child.Exe {
				_ = syscall.Kill(child.PID, syscall.SIGKILL)
			}
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(child.PID, &status, 0, nil)
		}
	}()
	current, err := readHelperIdentity(child.PID)
	if err != nil || current.Started != child.Started || current.PPID != parent.Process.Pid || current.Exe != filepath.Join(root, "vpn-director-probe") || current.State == "Z" || current.State == "X" {
		return errors.New("launcher child is not the owned running executable")
	}
	if method == "Start" {
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, "parent-ready")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(filepath.Join(root, "parent-ready")); err != nil {
			return errors.New("Start never returned its running session")
		}
	}
	if err := parent.Process.Kill(); err != nil {
		return err
	}
	<-parentDone
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
			fmt.Printf("%s: owned child terminated as %s after parent SIGKILL and was reaped\n", method, id.State)
			return nil
		}
		if id.Exe != child.Exe {
			return errors.New("owned child executable identity changed")
		}
		time.Sleep(10 * time.Millisecond)
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
			cmd := exec.CommandContext(ctx, self)
			cmd.Env = helperEnvironment("supervisor", method, root, "")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("parent-death helper: %v\n%s", err, out)
			}
			t.Log(strings.TrimSpace(string(out)))
		})
	}
}
