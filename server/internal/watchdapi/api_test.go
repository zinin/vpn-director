package watchdapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	fd, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	short := fmt.Sprintf("/proc/self/fd/%d/watchd.sock", fd.Fd())
	listener, err := Listen(context.Background(), short)
	if err != nil {
		t.Fatal("restart on the same filesystem socket/lock via a bounded address failed:", err)
	}
	defer listener.Close()
	if after, err := os.Lstat(path + ".lock"); err != nil || !os.SameFile(before, after) {
		t.Error("failed listen/restart changed the lock inode")
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
