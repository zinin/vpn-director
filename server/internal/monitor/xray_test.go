package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
