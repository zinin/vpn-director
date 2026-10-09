package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const watchAPISecret = "synthetic-watch-api-credential-13"

const watchAPIJSON = `{"state":"active","updated_at":"2026-10-05T12:00:00Z","message":"Automation is ready","action":"walking","committed_failover":true,"pending_restore":false,"notifications":{"pending":2,"storage_error":"cannot write notification storage"}}`

type watchAPIRead func(context.Context) (watchdapi.WatchSnapshot, error)

func (f watchAPIRead) Watch(ctx context.Context) (watchdapi.WatchSnapshot, error) { return f(ctx) }

func watchAPISocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Keep Unix socket paths below the kernel limit, including long subtest names.
	dir, err := os.MkdirTemp("", "vpd-watch-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "w.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the fixture's Unix HTTP server did not stop")
		}
	})
	return socket
}

func watchAPIRequest(t *testing.T, deps *Deps, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := NewRouter(deps, nil, ctx)
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "token", Value: token})
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertWatchAPIResponseSafe(t *testing.T, raw string) {
	t.Helper()
	for _, forbidden := range []string{watchAPISecret, "https://provider.example/private/", "bot_token", "jwt_secret", "outbound", "vless://"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("watch JSON exposes %q: %s", forbidden, raw)
		}
	}
}

func TestHandleWatch_AuthUnavailableAndStorageError(t *testing.T) {
	t.Run("authenticated socket read preserves queue diagnostics", func(t *testing.T) {
		socket := watchAPISocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/watch" {
				http.Error(w, "unexpected watch request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(watchAPIJSON))
		}))
		deps := newTestDeps(t)
		deps.Watch = watchdapi.NewClient(socket)
		deps.Monitor = &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}
		deps.Config = &mockConfig{
			err: errors.New("local config unavailable: " + watchAPISecret),
			subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", URL: "https://provider.example/private/" + watchAPISecret,
				Servers: []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, UUID: watchAPISecret}}}},
		}
		rec := watchAPIRequest(t, deps, http.MethodGet, "/api/watch", newTestToken(t, deps))
		if rec.Code != http.StatusOK {
			t.Fatalf("authenticated watch read: %d %s", rec.Code, rec.Body)
		}
		var snapshot watchdapi.WatchSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		want := watchdapi.WatchSnapshot{
			State: watchdapi.WatchActive, UpdatedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
			Message: "Automation is ready", Action: "walking", CommittedFailover: true, PendingRestore: false,
			Notifications: watchdapi.NotificationsStatus{Pending: 2, StorageError: "cannot write notification storage"},
		}
		if !reflect.DeepEqual(snapshot, want) {
			t.Fatalf("watch response %+v, want %+v despite disabled monitor and local config error", snapshot, want)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 7 {
			t.Fatalf("watch response is not the seven-field WatchSnapshot: %s", rec.Body)
		}
		for _, key := range []string{"state", "updated_at", "message", "action", "committed_failover", "pending_restore", "notifications"} {
			if _, ok := fields[key]; !ok {
				t.Fatalf("missing WatchSnapshot field %q: %s", key, rec.Body)
			}
		}
		assertWatchAPIResponseSafe(t, rec.Body.String())
	})

	t.Run("pending restore is distinct from committed fallback", func(t *testing.T) {
		socket := watchAPISocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"state":"active","updated_at":"2026-10-05T12:00:00Z","message":"","action":"restoring","committed_failover":false,"pending_restore":true,"notifications":{"pending":1,"storage_error":""}}`))
		}))
		deps := newTestDeps(t)
		deps.Watch = watchdapi.NewClient(socket)
		rec := watchAPIRequest(t, deps, http.MethodGet, "/api/watch", newTestToken(t, deps))
		var snapshot watchdapi.WatchSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("pending restore read: %d %s error=%v", rec.Code, rec.Body, err)
		}
		if snapshot.State != watchdapi.WatchActive || snapshot.Action != "restoring" || snapshot.CommittedFailover || !snapshot.PendingRestore || snapshot.Notifications.Pending != 1 || snapshot.Notifications.StorageError != "" {
			t.Fatalf("pending restore was hidden or confused with committed failover: %+v", snapshot)
		}
		assertWatchAPIResponseSafe(t, rec.Body.String())
	})

	t.Run("hung socket is bounded by two seconds", func(t *testing.T) {
		socket := watchAPISocket(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		deps := newTestDeps(t)
		deps.Watch = watchdapi.NewClient(socket)
		token := newTestToken(t, deps)
		started := time.Now()
		rec := watchAPIRequest(t, deps, http.MethodGet, "/api/watch", token)
		elapsed := time.Since(started)
		var snapshot watchdapi.WatchSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil || rec.Code != http.StatusOK || snapshot.State != watchdapi.WatchNotRunning {
			t.Fatalf("hung socket: %d %s error=%v", rec.Code, rec.Body, err)
		}
		if elapsed < 1500*time.Millisecond || elapsed > 3*time.Second {
			t.Fatalf("hung watch read took %s, want the 2-second IPC bound", elapsed)
		}
		assertWatchAPIResponseSafe(t, rec.Body.String())
	})

	t.Run("auth middleware remains required", func(t *testing.T) {
		deps := newTestDeps(t)
		deps.Watch = watchAPIRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
			return watchdapi.WatchSnapshot{State: watchdapi.WatchActive}, nil
		})
		oldToken, err := deps.JWT.Create("admin", "obsolete-password-fingerprint")
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"", "invalid-token", oldToken} {
			rec := watchAPIRequest(t, deps, http.MethodGet, "/api/watch", token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized watch read: %d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), `"state"`) {
				t.Fatalf("unauthorized response exposed automation state: %s", rec.Body)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		client func(*testing.T) watchdapi.WatchAPI
	}{
		{"no client", func(*testing.T) watchdapi.WatchAPI { return nil }},
		{"missing socket", func(t *testing.T) watchdapi.WatchAPI {
			return watchdapi.NewClient(filepath.Join(t.TempDir(), "missing.sock"))
		}},
		{"down socket", func(t *testing.T) watchdapi.WatchAPI {
			dir, err := os.MkdirTemp("", "vpd-watch-down-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "w.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			listener.SetUnlinkOnClose(false)
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			return watchdapi.NewClient(socket)
		}},
		{"old monitor-only daemon", func(t *testing.T) watchdapi.WatchAPI {
			socket := watchAPISocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/monitor" {
					_, _ = w.Write([]byte(`{"state":"disabled","interval_seconds":60,"lag_seconds":0,"endpoints":{}}`))
					return
				}
				http.Error(w, watchAPISecret, http.StatusNotFound)
			}))
			return watchdapi.NewClient(socket)
		}},
		{"daemon error", func(t *testing.T) watchdapi.WatchAPI {
			socket := watchAPISocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "provider error: "+watchAPISecret, http.StatusInternalServerError)
			}))
			return watchdapi.NewClient(socket)
		}},
		{"malformed daemon response", func(t *testing.T) watchdapi.WatchAPI {
			socket := watchAPISocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"state":"` + watchAPISecret))
			}))
			return watchdapi.NewClient(socket)
		}},
		{"raw transport error discards stale snapshot", func(*testing.T) watchdapi.WatchAPI {
			return watchAPIRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
				return watchdapi.WatchSnapshot{State: watchdapi.WatchActive, Action: "switching", CommittedFailover: true, PendingRestore: true},
					errors.New("https://provider.example/private/" + watchAPISecret)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestDeps(t)
			deps.Watch = tc.client(t)
			rec := watchAPIRequest(t, deps, http.MethodGet, "/api/watch", newTestToken(t, deps))
			if rec.Code != http.StatusOK {
				t.Fatalf("unavailable daemon: %d %s; want a readable client-side state", rec.Code, rec.Body)
			}
			var snapshot watchdapi.WatchSnapshot
			if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.State != watchdapi.WatchNotRunning || snapshot.Action != "" || snapshot.CommittedFailover || snapshot.PendingRestore {
				t.Fatalf("unavailable socket inherited live automation: %+v", snapshot)
			}
			if snapshot.Message == "" {
				t.Fatal("unavailable automation has no safe explanation")
			}
			assertWatchAPIResponseSafe(t, rec.Body.String())
		})
	}

	t.Run("watch exposes no mutation routes", func(t *testing.T) {
		deps := newTestDeps(t)
		deps.Watch = watchAPIRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
			return watchdapi.WatchSnapshot{State: watchdapi.WatchActive}, nil
		})
		token := newTestToken(t, deps)
		for _, request := range []struct{ method, path string }{
			{http.MethodPost, "/api/watch"}, {http.MethodPut, "/api/watch"}, {http.MethodDelete, "/api/watch"},
			{http.MethodPost, "/api/watch/check"}, {http.MethodPost, "/api/watch/stop"},
		} {
			rec := watchAPIRequest(t, deps, request.method, request.path, token)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("unexpected watch mutation route %s %s: %d %s", request.method, request.path, rec.Code, rec.Body)
			}
		}
	})

	t.Run("direct handler maps a nil client to not_running", func(t *testing.T) {
		deps := newTestDeps(t)
		rec := httptest.NewRecorder()
		handleWatch(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/watch", nil))
		var snapshot watchdapi.WatchSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil || rec.Code != http.StatusOK || snapshot.State != watchdapi.WatchNotRunning {
			t.Fatalf("direct unavailable handler: %d %s error=%v", rec.Code, rec.Body, err)
		}
	})
}
