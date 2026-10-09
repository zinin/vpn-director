package watchdapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSource is a monitor the socket serves in these tests.
type fakeSource struct {
	mu       sync.Mutex
	snap     Snapshot
	err      error
	requests [][]string
}

func (f *fakeSource) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSource) Request(keys []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, keys)
	if f.err != nil {
		return 0, f.err
	}
	if len(keys) == 0 {
		return len(f.snap.Endpoints), nil
	}
	return len(keys), nil
}

// serve starts Serve on a socket in a short temp dir - a unix socket path
// holds at most 108 bytes, more than t.TempDir leaves for a long test name -
// and returns its client.
func serve(t *testing.T, src Source) (*Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "watchd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, src) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	client, err := waitForSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	return client, path
}

func waitForSocket(path string) (*Client, error) {
	for i := 0; i < 100; i++ {
		client := NewClient(path)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := client.Monitor(ctx)
		cancel()
		if err == nil {
			return client, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, errors.New("the socket never answered")
}

func TestClient_MonitorReadsTheSnapshot(t *testing.T) {
	want := Snapshot{
		State: StateOK, IntervalSeconds: 60,
		UpdatedAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
		Endpoints: map[string]EndpointState{"k": {Status: StatusAlive, LatencyMS: 142}},
	}
	c, _ := serve(t, &fakeSource{snap: want})

	got, err := c.Monitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %+v, want %+v", got, want)
	}
}

func TestClient_CheckQueuesTheKeysOrEveryEndpoint(t *testing.T) {
	src := &fakeSource{snap: Snapshot{Endpoints: map[string]EndpointState{"a": {}, "b": {}, "c": {}}}}
	c, _ := serve(t, src)

	if n, err := c.Check(context.Background(), []string{"a"}); err != nil || n != 1 {
		t.Fatalf("Check(a) = %d, %v", n, err)
	}
	if n, err := c.Check(context.Background(), nil); err != nil || n != 3 {
		t.Fatalf("Check() = %d, %v", n, err)
	}
	if len(src.requests) != 2 || !reflect.DeepEqual(src.requests[0], []string{"a"}) || len(src.requests[1]) != 0 {
		t.Fatalf("requests %v", src.requests)
	}
}

func TestClient_CheckOfAStoppedMonitorIsErrNotActive(t *testing.T) {
	c, _ := serve(t, &fakeSource{err: ErrNotActive, snap: Snapshot{State: StateStopped}})

	if _, err := c.Check(context.Background(), nil); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Check() error %v, want ErrNotActive", err)
	}
}

// The socket is root's alone: every daemon here runs as root, and nobody else
// may queue checks or read which servers exist.
func TestServe_TheSocketIsMode0600(t *testing.T) {
	_, path := serve(t, &fakeSource{})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("socket mode %o, want 600", mode)
	}
}

// A socket file an earlier run left behind does not keep the daemon from
// listening again.
func TestServe_ReplacesAStaleSocketFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "watchd.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, &fakeSource{}) }()
	var client *Client
	for i := 0; i < 100 && client == nil; i++ {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			client = NewClient(path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		cancel()
		t.Fatalf("Serve did not listen over the stale file: %v", <-done)
	}
	if _, err := client.Monitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve() = %v", err)
	}
}

// A daemon that is not there answers nothing, fast.
func TestClient_NoDaemonIsAnErrorWithinTheBound(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "absent.sock"))
	start := time.Now()
	if _, err := c.Monitor(context.Background()); err == nil {
		t.Fatal("Monitor() of an absent socket succeeded")
	}
	if d := time.Since(start); d > clientTimeout {
		t.Fatalf("took %s", d)
	}
}

func TestClient_AHungDaemonTimesOutWithinTheBound(t *testing.T) {
	previous := clientTimeout
	clientTimeout = 100 * time.Millisecond
	t.Cleanup(func() { clientTimeout = previous })

	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	for _, method := range []struct {
		name string
		call func(*Client) error
	}{
		{"Monitor", func(c *Client) error {
			_, err := c.Monitor(context.Background())
			return err
		}},
		{"Check", func(c *Client) error {
			_, err := c.Check(context.Background(), nil)
			return err
		}},
	} {
		t.Run(method.name, func(t *testing.T) {
			path := filepath.Join(dir, method.name+".sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			accepted := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				close(accepted)
				<-release
				done <- nil
			}()
			t.Cleanup(func() {
				close(release)
				listener.Close()
				if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
					t.Errorf("Accept() = %v", err)
				}
			})

			start := time.Now()
			err = method.call(NewClient(path))
			elapsed := time.Since(start)
			select {
			case <-accepted:
			default:
				t.Fatal("the socket did not accept the request")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request error %v, want deadline exceeded", err)
			}
			// Allow scheduling overhead after the shortened deadline.
			if elapsed > clientTimeout+200*time.Millisecond {
				t.Fatalf("took %s with timeout %s", elapsed, clientTimeout)
			}
		})
	}
}

func TestHandler_CheckRejectsNullWithoutRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"null", `null`},
		{"whitespace-null", " \n\tnull \r\n\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{snap: Snapshot{Endpoints: map[string]EndpointState{"a": {}}}}
			rec := httptest.NewRecorder()
			NewHandler(src).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/monitor/check", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest || len(src.requests) != 0 {
				t.Fatalf("status=%d requests=%d; want 400 without Request", rec.Code, len(src.requests))
			}
		})
	}
}

func TestHandler_CheckRequiresTheWholeJSONDocument(t *testing.T) {
	for _, body := range []string{`{} garbage`, `{} {}`, `{"keys":["a"]} null`, `{} ` + strings.Repeat(" ", 1<<20)} {
		t.Run(body[:min(len(body), 24)], func(t *testing.T) {
			src := &fakeSource{}
			rec := httptest.NewRecorder()
			NewHandler(src).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/monitor/check", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest || len(src.requests) != 0 {
				t.Fatalf("status=%d requests=%d; want 400 without Request", rec.Code, len(src.requests))
			}
		})
	}
	for _, body := range []string{"", " \n\t", `{}`, "{\"keys\":[\"a\"]} \n"} {
		src := &fakeSource{}
		rec := httptest.NewRecorder()
		NewHandler(src).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/monitor/check", strings.NewReader(body)))
		if rec.Code != http.StatusAccepted || len(src.requests) != 1 {
			t.Fatalf("valid body status=%d requests=%d", rec.Code, len(src.requests))
		}
	}
}

func ownedSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "watchd.sock")
}

func startOwnedServe(t *testing.T, path string, src Source) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, src) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("owned Serve shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("owned Serve did not stop")
			}
		})
	}
	t.Cleanup(stop)
	if _, err := waitForSocket(path); err != nil {
		t.Fatal(err)
	}
	return stop
}

func attemptDuplicate(t *testing.T, path string) (error, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, &fakeSource{snap: Snapshot{State: StateDisabled}}) }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(500 * time.Millisecond):
		cancel()
		select {
		case err := <-done:
			return err, false
		case <-time.After(3 * time.Second):
			t.Fatal("duplicate Serve did not stop")
			return nil, false
		}
	}
}

func TestServe_RefusesRunningInstanceAndKeepsItsSocket(t *testing.T) {
	path := ownedSocketPath(t)
	startOwnedServe(t, path, &fakeSource{snap: Snapshot{State: StateOK, Endpoints: map[string]EndpointState{"a": {}, "b": {}}}})
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err, early := attemptDuplicate(t, path); err == nil || !early {
		t.Errorf("duplicate took over a running socket: err=%v early=%v", err, early)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Error("duplicate changed the original socket inode")
	}
	client := NewClient(path)
	if snap, err := client.Monitor(context.Background()); err != nil || snap.State != StateOK || len(snap.Endpoints) != 2 {
		t.Errorf("original monitor no longer answers: state=%v err=%v", snap.State, err)
	}
	if n, err := client.Check(context.Background(), []string{"a"}); err != nil || n != 1 {
		t.Errorf("original one check: n=%d err=%v", n, err)
	}
	if n, err := client.Check(context.Background(), nil); err != nil || n != 2 {
		t.Errorf("original all check: n=%d err=%v", n, err)
	}
}

func TestServe_RefusesAcceptingSocketWithoutWaitingForHTTP(t *testing.T) {
	path := ownedSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { listener.Close() })
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err, early := attemptDuplicate(t, path); err == nil || !early {
		t.Errorf("accepting socket without HTTP was replaced or awaited: err=%v early=%v", err, early)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Error("accepting socket inode changed")
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		t.Error("original accepting socket is no longer reachable:", err)
	} else {
		conn.Close()
	}
}

func TestServe_PreservesForeignRegularAndSymlinkPaths(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "symlink"}[symlink], func(t *testing.T) {
			path := ownedSocketPath(t)
			target := path
			if symlink {
				target = filepath.Join(filepath.Dir(path), "foreign")
			}
			data := []byte("foreign synthetic file\n")
			if err := os.WriteFile(target, data, 0640); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err, early := attemptDuplicate(t, path); err == nil || !early {
				t.Errorf("foreign path accepted: err=%v early=%v", err, early)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Error("foreign file/symlink identity or mode changed")
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != string(data) {
				t.Error("foreign content changed:", err)
			}
			if symlink {
				if got, err := os.Readlink(path); err != nil || got != target {
					t.Error("foreign symlink target changed:", err)
				}
			}
		})
	}
}

func TestServe_ConcurrentStartsRefuseEveryDuplicate(t *testing.T) {
	path := ownedSocketPath(t)
	startOwnedServe(t, path, &fakeSource{snap: Snapshot{State: StateOK}})
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := make(chan struct{})
	done := make(chan error, count)
	for range count {
		go func() {
			<-gate
			done <- Serve(ctx, path, &fakeSource{snap: Snapshot{State: StateDisabled}})
		}()
	}
	close(gate)
	deadline := time.After(time.Second)
	finished := 0
collect:
	for finished < count {
		select {
		case err := <-done:
			finished++
			if err == nil {
				t.Error("a concurrent duplicate succeeded")
			}
		case <-deadline:
			t.Error("a concurrent duplicate became a running server")
			break collect
		}
	}
	cancel()
	for finished < count {
		select {
		case <-done:
			finished++
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent duplicate failed to stop")
		}
	}
	if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) {
		t.Error("concurrent starts changed the primary socket")
	}
	if snap, err := NewClient(path).Monitor(context.Background()); err != nil || snap.State != StateOK {
		t.Error("concurrent starts disturbed the primary:", err)
	}
}

func TestServe_ConcurrentFirstStartsHaveExactlyOneOwner(t *testing.T) {
	path := ownedSocketPath(t)
	const count = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate, done := make(chan struct{}), make(chan error, count)
	for range count {
		go func() {
			<-gate
			done <- Serve(ctx, path, &fakeSource{snap: Snapshot{State: StateOK, Endpoints: map[string]EndpointState{"a": {}}}})
		}()
	}
	close(gate)
	finished := 0
	t.Cleanup(func() {
		cancel()
		for finished < count {
			select {
			case <-done:
				finished++
			case <-time.After(3 * time.Second):
				t.Error("concurrent first start did not finish")
				return
			}
		}
	})
	deadline := time.After(time.Second)
	for finished < count-1 {
		select {
		case err := <-done:
			finished++
			if err == nil {
				t.Error("a first-start loser succeeded")
			}
		case <-deadline:
			t.Fatal("more than one concurrent first start remained running")
		}
	}
	if _, err := waitForSocket(path); err != nil {
		t.Fatal("first-start winner did not serve:", err)
	}
	if n, err := NewClient(path).Check(context.Background(), nil); err != nil || n != 1 {
		t.Fatalf("first-start winner check: n=%d err=%v", n, err)
	}
	cancel()
	select {
	case err := <-done:
		finished++
		if err != nil {
			t.Fatal("first-start winner shutdown:", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first-start winner did not stop")
	}
}

func TestServe_LockInodeSurvivesCancellationAndRestart(t *testing.T) {
	path := ownedSocketPath(t)
	stop := startOwnedServe(t, path, &fakeSource{})
	before, err := os.Lstat(path + ".lock")
	if err != nil {
		t.Fatal("lifetime lock was not created:", err)
	}
	stop()
	if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(before, after) {
		t.Fatal("normal cancellation removed/replaced the lock inode")
	}
	stopAgain := startOwnedServe(t, path, &fakeSource{})
	if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(before, after) {
		t.Error("restart did not reuse the stable lock inode")
	}
	stopAgain()
}

func assertLockReleased(t *testing.T, path string) {
	t.Helper()
	lock, err := os.OpenFile(path+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("instance lock was leaked:", err)
	}
}

func TestListen_FailClosedOnAmbiguousSocketProbe(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, os.ErrPermission, syscall.EIO} {
		t.Run(cause.Error(), func(t *testing.T) {
			path := ownedSocketPath(t)
			stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			stale.SetUnlinkOnClose(false)
			stale.Close()
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			listener, err := listen(context.Background(), path, func(ctx context.Context, network, addr string) (net.Conn, error) {
				calls++
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > socketProbeTimeout || network != "unix" || addr != path {
					t.Error("socket probe has no bound or targets another path")
				}
				return nil, cause
			})
			if listener != nil {
				listener.Close()
			}
			if !errors.Is(err, cause) || listener != nil || calls != 1 {
				t.Fatalf("ambiguous probe accepted: listener=%v calls=%d err=%v", listener != nil, calls, err)
			}
			if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) {
				t.Error("ambiguous probe removed or replaced the socket")
			}
			assertLockReleased(t, path)
			if listener, err := Listen(context.Background(), path); err != nil {
				t.Fatal("failed probe prevented recovery:", err)
			} else {
				listener.Close()
			}
		})
	}
}

func TestListen_FailedListenReleasesStableLockAndCanRestart(t *testing.T) {
	base := filepath.Dir(ownedSocketPath(t))
	dir := filepath.Join(base, strings.Repeat("d", 100))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "watchd.sock")
	if listener, err := Listen(context.Background(), path); err == nil {
		listener.Close()
		t.Fatal("overlong Unix socket address unexpectedly listened")
	}
	assertLockReleased(t, path)
	before, err := os.Lstat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	// A relative address is short and names the same directory; a
	// /proc/self/fd one would make the socket's directory a symlink, which
	// Listen refuses.
	t.Chdir(dir)
	listener, err := Listen(context.Background(), "watchd.sock")
	if err != nil {
		t.Fatal("restart on the same filesystem socket/lock via a bounded address failed:", err)
	}
	defer listener.Close()
	if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(before, after) {
		t.Error("failed listen/restart changed the lock inode")
	}
}

// Another user able to write to the socket's directory could replace the
// socket after publication. Listen refuses a directory that is a symlink or
// another user's, before it creates anything there, and creates a missing one.
func TestListen_RefusesAnUntrustedSocketDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		check string // the refusal; empty accepts
	}{
		{"missing directory is created", func(*testing.T, string) {}, ""},
		{"existing 0755 directory", func(t *testing.T, dir string) { mkdirMode(t, dir, 0755) }, ""},
		{"symlink to a directory", func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), dir); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(filepath.Dir(ownedSocketPath(t)), "run")
			tc.setup(t, dir)
			path := filepath.Join(dir, "watchd.sock")
			listener, err := Listen(context.Background(), path)
			if tc.check == "" {
				if err != nil {
					t.Fatal("trusted socket directory refused:", err)
				}
				listener.Close()
				return
			}
			if listener != nil {
				listener.Close()
			}
			var pe *os.PathError
			if !errors.As(err, &pe) || pe.Path != dir || pe.Err.Error() != tc.check {
				t.Fatalf("Listen error %v, want the directory refused for %s", err, tc.check)
			}
			for _, name := range []string{path, path + ".lock"} {
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("refused directory got %s: %v", filepath.Base(name), err)
				}
			}
		})
	}
	t.Run("owned by another user", func(t *testing.T) {
		info, err := os.Lstat("/")
		if err != nil {
			t.Fatal(err)
		}
		uid := info.Sys().(*syscall.Stat_t).Uid
		if int(uid) == os.Geteuid() {
			t.Skip("the test user owns /")
		}
		var pe *os.PathError
		if err := OwnedDir("/", 0755, false); !errors.As(err, &pe) || pe.Err.Error() != fmt.Sprintf("owner uid %d", uid) {
			t.Fatalf("OwnedDir(/) = %v, want the owner refused", err)
		}
	})
}

// Asuswrt-Merlin runs its scripts with umask 0, so the stop of an older release
// left /tmp/vpn-director 0777 until the router rebooted, and refusing it kept
// the first update from starting watchd. Listen takes group and other write off
// a directory of its own instead, keeps the rest of its mode and logs the
// change; the prober's private directory is still refused, not tightened.
func TestListen_TightensItsOwnLooseSocketDirectory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode, want os.FileMode
	}{
		{"group writable", 0775, 0755},
		{"other writable", 0757, 0755},
		{"world writable", 0777, 0755},
		{"group only", 0770, 0750},
		{"shared like /tmp", 0777 | os.ModeSticky, 0755 | os.ModeSticky},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			dir := filepath.Join(filepath.Dir(ownedSocketPath(t)), "run")
			mkdirMode(t, dir, tc.mode)
			listener, err := Listen(context.Background(), filepath.Join(dir, "watchd.sock"))
			if err != nil {
				t.Fatal("own socket directory refused:", err)
			}
			listener.Close()
			info, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode() & (os.ModePerm | os.ModeSticky); got != tc.want {
				t.Fatalf("directory mode %v, want %v", got, tc.want)
			}
			if line := logs.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, fmt.Sprintf("was=%04o", tc.mode.Perm())) {
				t.Fatalf("log %q, want a WARN naming the old mode", line)
			}
		})
	}
	t.Run("private directory is refused, not tightened", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "probe")
		mkdirMode(t, dir, 0777)
		var pe *os.PathError
		if err := OwnedDir(dir, 0700, true); !errors.As(err, &pe) || pe.Err.Error() != "mode 0777" {
			t.Fatalf("OwnedDir(private) = %v, want the mode refused", err)
		}
		if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0777 {
			t.Fatalf("refused private directory changed: %v, %v", info, err)
		}
	})
}

// mkdirMode creates dir with exactly mode, whatever the umask.
func mkdirMode(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}

func TestListen_CanceledContextDoesNotAcquire(t *testing.T) {
	path := ownedSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listener, err := Listen(ctx, path)
	if listener != nil {
		listener.Close()
	}
	if !errors.Is(err, context.Canceled) || listener != nil {
		t.Fatal("canceled Listen acquired resources:", err)
	}
	for _, name := range []string{path, path + ".lock"} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Error("canceled startup created a socket or lock")
		}
	}
	listener, err = Listen(context.Background(), path)
	if err != nil {
		t.Fatal("canceled startup prevented restart:", err)
	}
	listener.Close()
}

func TestListen_PreservesForeignLockPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			path := ownedSocketPath(t)
			lockPath := path + ".lock"
			target := filepath.Join(filepath.Dir(path), "foreign")
			if err := os.WriteFile(target, []byte("synthetic foreign lock target"), 0640); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, lockPath)
			case "directory":
				err = os.Mkdir(lockPath, 0700)
			case "fifo":
				err = syscall.Mkfifo(lockPath, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if listener, err := Listen(context.Background(), path); err == nil {
				listener.Close()
				t.Error("foreign nonregular lock accepted")
			}
			if after, err := os.Lstat(lockPath); err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Error("foreign lock identity or mode changed")
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "synthetic foreign lock target" {
				t.Error("foreign lock target changed:", err)
			}
		})
	}
}

func TestListen_ClosePreservesAReplacementForeignPath(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "symlink"}[symlink], func(t *testing.T) {
			path := ownedSocketPath(t)
			listener, err := Listen(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			target := path
			if symlink {
				target = filepath.Join(filepath.Dir(path), "replacement")
			}
			if err := os.WriteFile(target, []byte("foreign replacement"), 0640); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Error("listener Close removed/changed a foreign replacement")
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "foreign replacement" {
				t.Error("foreign replacement content changed:", err)
			}
			assertLockReleased(t, path)
		})
	}
}

func TestServeListener_FatalFailureRetainsOwnershipUntilClosed(t *testing.T) {
	path := ownedSocketPath(t)
	listener, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() { done <- ServeListener(context.Background(), listener, &fakeSource{}) }()
	if _, err := waitForSocket(path); err != nil {
		t.Fatal(err)
	}
	listener.(*instanceListener).Listener.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("fatal listener error lost:", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed ServeListener did not finish")
	}
	if next, err := Listen(context.Background(), path); err == nil {
		next.Close()
		t.Error("fatal HTTP serving released the lifetime lock before owner shutdown")
	}
	listener.Close()
	if next, err := Listen(context.Background(), path); err != nil {
		t.Fatal("owner shutdown did not release ownership:", err)
	} else {
		next.Close()
	}
}

type readinessListener struct {
	net.Listener
	connected chan struct{}
	once      sync.Once
}

func (l *readinessListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.once.Do(func() { close(l.connected) })
	}
	return c, err
}

func TestSocketReadiness_WaitsForHTTPAfterPermissions(t *testing.T) {
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(unblock)
	l := &readinessListener{Listener: listener, connected: make(chan struct{})}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
			return
		}
		NewHandler(&fakeSource{}).ServeHTTP(w, r)
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() { unblock(); srv.Close(); <-done })
	ready := make(chan error, 1)
	go func() { _, err := waitForSocket(path); ready <- err }()
	select {
	case <-l.connected:
	case <-time.After(time.Second):
		t.Fatal("readiness did not connect")
	}
	select {
	case err := <-ready:
		t.Fatalf("ready before permissions/HTTP: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness did not finish")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions not ready: %v", err)
	}
}

func assertOnlyStableLock(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path)+".lock" {
		t.Errorf("socket/private staging leaked: directory entries %v", entries)
	}
	assertLockReleased(t, path)
}

func openSocketTestFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestListen_PrivatePublicationUnderPermissiveUmask(t *testing.T) {
	if os.Getenv("VPD_TEST_PRIVATE_SOCKET_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestListen_PrivatePublicationUnderPermissiveUmask$", "-test.v")
		cmd.Env = append(os.Environ(), "VPD_TEST_PRIVATE_SOCKET_CHILD=1")
		output, err := cmd.CombinedOutput()
		t.Logf("isolated umask subprocess:\n%s", output)
		if err != nil {
			t.Fatalf("private publication subprocess: %v", err)
		}
		return
	}
	// Only this single-test subprocess changes umask; other Go tests retain theirs.
	syscall.Umask(0)
	path := ownedSocketPath(t)
	parent := filepath.Dir(path)
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var staged string
	var socketInfo, lockInfo os.FileInfo
	chmodCalls, publicationCalls := 0, 0
	assertNotPublic := func() {
		t.Helper()
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("public socket exists before safe publication: %v", err)
		}
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Error("public pathname accepted a connection before permissions were safe")
		}
	}
	listener, err := listenWithPublication(ctx, path, (&net.Dialer{}).DialContext, func(name string, mode os.FileMode) error {
		chmodCalls++
		staged = name
		assertNotPublic()
		if name == path || filepath.Dir(name) == parent {
			t.Error("listener bound outside a newly owned private directory")
		}
		private, err := os.Lstat(filepath.Dir(name))
		if err != nil || !private.IsDir() || private.Mode().Perm() != 0700 {
			t.Errorf("private directory mode: info=%v err=%v", private, err)
		}
		socketInfo, err = os.Lstat(name)
		if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0777 {
			t.Errorf("permissive-umask private bind: info=%v err=%v", socketInfo, err)
		}
		parentInfo, err := os.Stat(parent)
		if err != nil || private.Sys().(*syscall.Stat_t).Dev != parentInfo.Sys().(*syscall.Stat_t).Dev {
			t.Errorf("private bind is on another filesystem: %v", err)
		}
		lockInfo, err = os.Lstat(path + ".lock")
		if err != nil {
			return err
		}
		if duplicate, err := Listen(context.Background(), path); err == nil {
			duplicate.Close()
			t.Error("duplicate acquired the private-bound instance")
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Errorf("private-bound duplicate lost flock cause: %v", err)
		}
		if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(lockInfo, after) {
			t.Error("duplicate changed the private-bound owner's stable lock")
		}
		return os.Chmod(name, mode)
	}, func(old, final string) error {
		publicationCalls++
		assertNotPublic()
		info, err := os.Lstat(old)
		if err != nil || info.Mode().Perm() != 0600 || !os.SameFile(info, socketInfo) || final != path {
			t.Errorf("publication did not receive the private mode-0600 socket: %v", err)
		}
		return os.Link(old, final)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if chmodCalls != 1 || publicationCalls != 1 {
		t.Errorf("chmod/publication stages: %d/%d", chmodCalls, publicationCalls)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0600 || !os.SameFile(info, socketInfo) {
		t.Errorf("published pathname did not retain the verified socket inode/mode: %v", err)
	}
	if listener.Addr().Network() != "unix" || listener.Addr().String() != path {
		t.Errorf("public listener address changed: %v", listener.Addr())
	}
	if staged != path {
		if _, err := os.Lstat(filepath.Dir(staged)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("private directory remains after publication: %v", err)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- ServeListener(ctx, listener, &fakeSource{snap: Snapshot{State: StateOK, Endpoints: map[string]EndpointState{"a": {}, "b": {}}}})
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("published listener shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("published listener failed to stop")
			}
		})
	}
	t.Cleanup(stop)
	client, err := waitForSocket(path)
	if err != nil {
		t.Fatal("final-path HTTP connection failed:", err)
	}
	if n, err := client.Check(context.Background(), []string{"a"}); err != nil || n != 1 {
		t.Errorf("final-path one check: %d, %v", n, err)
	}
	if n, err := client.Check(context.Background(), nil); err != nil || n != 2 {
		t.Errorf("final-path all check: %d, %v", n, err)
	}
	if err, early := attemptDuplicate(t, path); err == nil || !early {
		t.Errorf("published instance was taken over: %v early=%v", err, early)
	}
	if snap, err := client.Monitor(context.Background()); err != nil || snap.State != StateOK || len(snap.Endpoints) != 2 {
		t.Errorf("duplicate disturbed the published API: %+v, %v", snap, err)
	}
	stop()
	if next, err := Listen(context.Background(), path); err == nil {
		next.Close()
		t.Error("HTTP shutdown released ownership before owner Close")
	}
	if err := listener.Close(); err != nil {
		t.Error(err)
	}
	assertOnlyStableLock(t, path)
	if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0755 {
		t.Errorf("shared directory mode changed: %v", err)
	}
	next, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatal("restart after private publication:", err)
	}
	next.Close()
	if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(lockInfo, after) {
		t.Error("publication/shutdown/restart replaced the stable lock")
	}
	assertOnlyStableLock(t, path)
}

func TestListen_PublicationFailuresAndCancellationCleanOwnedResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"chmod-error", syscall.EACCES},
		{"chmod-unverified", os.ErrPermission},
		{"publication-error", syscall.EOPNOTSUPP},
		{"publication-error-after-link", syscall.EIO},
		{"cancel-before-chmod", context.Canceled},
		{"cancel-before-publication", context.Canceled},
		{"cancel-after-publication", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ownedSocketPath(t)
			// Initialize Go's network poller before counting this attempt's descriptors.
			warm, err := Listen(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			warm.Close()
			lockInfo, err := os.Lstat(path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			fds := openSocketTestFDs(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var staged string
			listener, err := listenWithPublication(ctx, path, (&net.Dialer{}).DialContext, func(name string, mode os.FileMode) error {
				staged = name
				switch tc.name {
				case "chmod-error":
					return syscall.EACCES
				case "chmod-unverified":
					return os.Chmod(name, 0666)
				case "cancel-before-chmod":
					cancel()
				}
				err := os.Chmod(name, mode)
				if tc.name == "cancel-before-publication" {
					cancel()
				}
				return err
			}, func(old, final string) error {
				if tc.name == "publication-error" {
					return syscall.EOPNOTSUPP
				}
				if err := os.Link(old, final); err != nil {
					return err
				}
				if tc.name == "cancel-after-publication" {
					cancel()
				}
				if tc.name == "publication-error-after-link" {
					return syscall.EIO
				}
				return nil
			})
			if listener != nil {
				listener.Close()
			}
			if listener != nil || !errors.Is(err, tc.want) {
				t.Errorf("failed/canceled startup returned listener=%v err=%v; want %v", listener != nil, err, tc.want)
			}
			if staged != "" && staged != path {
				if _, err := os.Lstat(filepath.Dir(staged)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("failed startup leaked private directory: %v", err)
				}
			}
			assertOnlyStableLock(t, path)
			if got := openSocketTestFDs(t); got != fds {
				t.Errorf("startup leaked descriptors: before=%d after=%d", fds, got)
			}
			restarted, err := Listen(context.Background(), path)
			if err != nil {
				t.Fatal("failure prevented restart:", err)
			}
			restarted.Close()
			if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(lockInfo, after) {
				t.Error("failed/canceled startup or restart replaced the lock")
			}
			assertOnlyStableLock(t, path)
		})
	}
}

func TestListen_PublicationPreservesConcurrentDestinations(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "socket"} {
		t.Run(kind, func(t *testing.T) {
			path := ownedSocketPath(t)
			var foreign os.FileInfo
			var socket *net.UnixListener
			var staged, target string
			listener, err := listenWithPublication(context.Background(), path, (&net.Dialer{}).DialContext, os.Chmod, func(old, final string) error {
				staged = old
				switch kind {
				case "regular":
					if err := os.WriteFile(final, []byte("concurrent foreign file"), 0640); err != nil {
						return err
					}
				case "symlink":
					target = final + ".target"
					if err := os.WriteFile(target, []byte("concurrent foreign file"), 0640); err != nil {
						return err
					}
					if err := os.Symlink(target, final); err != nil {
						return err
					}
				case "socket":
					var err error
					socket, err = net.ListenUnix("unix", &net.UnixAddr{Name: final, Net: "unix"})
					if err != nil {
						return err
					}
					socket.SetUnlinkOnClose(false)
					t.Cleanup(func() { socket.Close() })
				}
				var err error
				foreign, err = os.Lstat(final)
				if err != nil {
					return err
				}
				return os.Link(old, final)
			})
			if listener != nil {
				listener.Close()
			}
			if listener != nil || !errors.Is(err, os.ErrExist) || foreign == nil {
				t.Fatalf("publication overwrote/missed a concurrent destination: listener=%v err=%v", listener != nil, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(foreign, after) || after.Mode() != foreign.Mode() || after.Size() != foreign.Size() || !after.ModTime().Equal(foreign.ModTime()) {
				t.Fatal("publication failure changed the concurrent destination:", err)
			}
			switch kind {
			case "regular", "symlink":
				if data, err := os.ReadFile(path); err != nil || string(data) != "concurrent foreign file" {
					t.Error("concurrent file/target changed:", err)
				}
				if kind == "symlink" {
					if got, err := os.Readlink(path); err != nil || got != target {
						t.Error("concurrent symlink changed:", err)
					}
				}
			case "socket":
				conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
				if err != nil {
					t.Error("concurrent accepting socket lost reachability:", err)
				} else {
					conn.Close()
				}
				socket.Close()
			}
			if staged != path {
				if _, err := os.Lstat(filepath.Dir(staged)); !errors.Is(err, os.ErrNotExist) {
					t.Error("collision leaked private staging:", err)
				}
			}
			assertLockReleased(t, path)
			lockInfo, err := os.Lstat(path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			// These foreign paths were created by this test and are still the same inode.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if target != "" {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			next, err := Listen(context.Background(), path)
			if err != nil {
				t.Fatal("collision prevented restart:", err)
			}
			next.Close()
			if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(lockInfo, after) {
				t.Error("collision/restart replaced the stable lock")
			}
			assertOnlyStableLock(t, path)
		})
	}
}

func TestListen_PrivatePublicationKeepsExistingDirectoryModes(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0711, 0750, 0755 | os.ModeSticky} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			path := ownedSocketPath(t)
			parent := filepath.Dir(path)
			if err := os.Chmod(parent, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(parent)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := Listen(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			if after, err := os.Stat(parent); err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Error("existing shared/restrictive directory mode or inode changed:", err)
			}
			assertOnlyStableLock(t, path)
		})
	}
}

func TestListen_PrivateBindSupportsNearBoundPublicPath(t *testing.T) {
	base := filepath.Dir(ownedSocketPath(t))
	dir := filepath.Join(base, strings.Repeat("d", 107-len(base)-1-len("/watchd.sock")))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "watchd.sock")
	listener, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatal("valid near-bound public address could not publish:", err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		t.Fatal("near-bound final pathname did not connect:", err)
	}
	conn.Close()
	listener.Close()
	assertOnlyStableLock(t, path)
}
