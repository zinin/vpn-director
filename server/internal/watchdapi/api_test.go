package watchdapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	if err := os.WriteFile(path, nil, 0600); err != nil {
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
