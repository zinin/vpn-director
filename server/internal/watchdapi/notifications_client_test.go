package watchdapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var _ watchdapi.WatchAPI = (*watchdapi.Client)(nil)
var _ watchdapi.NotificationAPI = (*watchdapi.Client)(nil)
var _ func(context.Context, net.Listener, watchdapi.Source, ...watchdapi.AutomationSource) error = watchdapi.ServeListener

func notificationSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "watchd.sock")
}

func notificationHTTPClient(t *testing.T, handler http.Handler) *watchdapi.Client {
	t.Helper()
	path := notificationSocketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	t.Cleanup(func() {
		srv.Close()
		listener.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("notification HTTP server: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("notification HTTP server did not stop")
		}
	})
	return watchdapi.NewClient(path)
}

func notificationListenerClient(t *testing.T, source watchdapi.AutomationSource) *watchdapi.Client {
	t.Helper()
	path := notificationSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := watchdapi.Listen(ctx, path)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	monitor := &notificationMonitor{snapshot: watchdapi.Snapshot{
		State: watchdapi.StateOK, IntervalSeconds: 60,
		Endpoints: map[string]watchdapi.EndpointState{"fixture": {Status: watchdapi.StatusAlive, LatencyMS: 142}},
	}}
	done := make(chan error, 1)
	go func() { done <- watchdapi.ServeListener(ctx, listener, monitor, source) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeListener: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeListener did not stop")
		}
		listener.Close()
	})
	return watchdapi.NewClient(path)
}

func TestNotificationClient_RoundTrip(t *testing.T) {
	s := newNotificationSource(t)
	c := notificationListenerClient(t, s)
	ctx := context.Background()
	if snapshot, err := c.Monitor(ctx); err != nil || snapshot.State != watchdapi.StateOK || snapshot.IntervalSeconds != 60 || len(snapshot.Endpoints) != 1 {
		t.Fatalf("legacy Monitor = %+v, %v", snapshot, err)
	}
	if queued, err := c.Check(ctx, []string{"fixture"}); err != nil || queued != 1 {
		t.Fatalf("legacy Check = %d, %v", queued, err)
	}
	if err := c.SetRecipients(ctx, []watchdapi.Recipient{{ChatID: 100, FirstSeen: s.at}, {ChatID: -1000, FirstSeen: s.at}}); err != nil {
		t.Fatal(err)
	}
	id := publishAPIEvent(t, s, "outbound.example.test is alive")
	if snapshot, err := c.Watch(ctx); err != nil || !reflect.DeepEqual(snapshot, s.WatchSnapshot()) {
		t.Fatalf("Watch = %+v, %v", snapshot, err)
	}
	want := watchdapi.NotificationPage{Messages: []watchdapi.Notification{
		{ChatID: -1000, EventID: id, At: s.at, Text: "outbound.example.test is alive"},
		{ChatID: 100, EventID: id, At: s.at, Text: "outbound.example.test is alive"},
	}}
	if page, err := c.Pending(ctx, ""); err != nil || !reflect.DeepEqual(page, want) {
		t.Fatalf("Pending = %+v, %v, want %+v", page, err, want)
	}
	if err := c.Ack(ctx, -1000, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Ack(ctx, -1000, id); err != nil {
		t.Fatalf("repeated Ack: %v", err)
	}
	want.Messages = want.Messages[1:]
	if page, err := c.Pending(ctx, ""); err != nil || !reflect.DeepEqual(page, want) {
		t.Fatalf("per-chat Ack pending = %+v, %v", page, err)
	}
	if snapshot, err := c.Monitor(ctx); err != nil || snapshot.State != watchdapi.StateOK {
		t.Fatal("notification delivery changed the monitor API", err)
	}
}

func TestNotificationClient_EmptyRecipientsRevokesAll(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recipients []watchdapi.Recipient
	}{
		{"nil", nil},
		{"explicit-empty", []watchdapi.Recipient{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newNotificationSource(t, 100, 200)
			publishAPIEvent(t, s, "before revocation")
			h := notificationHandler(s)
			type capturedRequest struct {
				method      string
				path        string
				contentType string
				body        string
			}
			captured := make(chan capturedRequest, 1)
			c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/notifications/recipients" {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					captured <- capturedRequest{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(body)}
					r.Body = io.NopCloser(bytes.NewReader(body))
				}
				h.ServeHTTP(w, r)
			}))
			if err := c.SetRecipients(context.Background(), tc.recipients); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-captured:
				if got.method != http.MethodPost || got.path != "/v1/notifications/recipients" || got.contentType != "application/json" || got.body != `{"recipients":[]}` {
					t.Fatalf("recipient revocation wire request = %+v", got)
				}
			default:
				t.Fatal("recipient synchronization did not use the exact route")
			}
			publishAPIEvent(t, s, "after revocation")
			page, err := c.Pending(context.Background(), "")
			if err != nil || len(page.Messages) != 0 || page.NextCursor != "" || s.Status().Pending != 0 {
				t.Fatalf("empty recipients did not revoke current/future deliveries: count=%d, %v", len(page.Messages), err)
			}
		})
	}
}

func TestNotificationClient_PendingTreatsCursorAsOpaque(t *testing.T) {
	cursor := "opaque+cursor/with?chat=-1000&event=10"
	captured := make(chan url.Values, 1)
	c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/notifications/pending" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		captured <- r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messages":[],"next_cursor":""}`)
	}))
	if _, err := c.Pending(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
	select {
	case query := <-captured:
		if !reflect.DeepEqual(query, url.Values{"cursor": {cursor}}) {
			t.Fatalf("cursor query = %v", query)
		}
	default:
		t.Fatal("pending client did not use the exact route")
	}
}

type notificationClientCall struct {
	name     string
	method   string
	path     string
	response string
	call     func(context.Context, *watchdapi.Client) error
}

func notificationClientCalls() []notificationClientCall {
	return []notificationClientCall{
		{"Watch", http.MethodGet, "/v1/watch", `{"state":"active","updated_at":"2026-10-05T10:00:00Z","message":"watch is active","action":"monitor","committed_failover":false,"pending_restore":false,"notifications":{"pending":0,"storage_error":""}}`, func(ctx context.Context, c *watchdapi.Client) error {
			_, err := c.Watch(ctx)
			return err
		}},
		{"SetRecipients", http.MethodPost, "/v1/notifications/recipients", `{"ok":true}`, func(ctx context.Context, c *watchdapi.Client) error {
			return c.SetRecipients(ctx, nil)
		}},
		{"Pending", http.MethodGet, "/v1/notifications/pending", `{"messages":[],"next_cursor":""}`, func(ctx context.Context, c *watchdapi.Client) error {
			_, err := c.Pending(ctx, "")
			return err
		}},
		{"Ack", http.MethodPost, "/v1/notifications/ack", `{"ok":true}`, func(ctx context.Context, c *watchdapi.Client) error {
			return c.Ack(ctx, -1000, watchdapi.EventID("0123456789abcdef0123456789abcdef:1"))
		}},
	}
}

func hungNotificationClient(t *testing.T) (*watchdapi.Client, <-chan struct{}) {
	t.Helper()
	path := notificationSocketPath(t)
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
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("hung socket: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("hung socket did not stop")
		}
	})
	return watchdapi.NewClient(path), accepted
}

func assertNotificationDeadline(t *testing.T, call notificationClientCall, c *watchdapi.Client, ctx context.Context, bound time.Duration) {
	t.Helper()
	start := time.Now()
	err := call.call(ctx, c)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s error = %v, want deadline exceeded", call.name, err)
	}
	// Wall time includes scheduling after the request's deadline.
	if elapsed > bound+200*time.Millisecond {
		t.Fatalf("%s took %s, request bound is %s", call.name, elapsed, bound)
	}
}

func TestNotificationClient_DeadlineAndBodyCap(t *testing.T) {
	for _, call := range notificationClientCalls() {
		t.Run(call.name, func(t *testing.T) {
			t.Run("hung-socket", func(t *testing.T) {
				c, accepted := hungNotificationClient(t)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				assertNotificationDeadline(t, call, c, ctx, 2*time.Second)
				select {
				case <-accepted:
				default:
					t.Fatal("deadline test never reached the Unix socket")
				}
			})
			t.Run("hung-after-valid-json", func(t *testing.T) {
				c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, call.response)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				assertNotificationDeadline(t, call, c, ctx, 2*time.Second)
			})
			t.Run("shorter-caller-deadline", func(t *testing.T) {
				c, _ := hungNotificationClient(t)
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				assertNotificationDeadline(t, call, c, ctx, 50*time.Millisecond)
			})
			for _, tc := range []struct {
				name   string
				suffix string
				ok     bool
			}{
				{"whole-document", " \n\t", true},
				{"trailing-object", ` {}`, false},
				{"trailing-null", ` null`, false},
				{"trailing-garbage", ` trailing`, false},
				{"oversized-valid-prefix", strings.Repeat(" ", (16<<20)+1-len(call.response)), false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != call.method || r.URL.Path != call.path {
							t.Errorf("%s wire route = %s %s", call.name, r.Method, r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, call.response+tc.suffix)
					}))
					err := call.call(context.Background(), c)
					if tc.ok && err != nil {
						t.Fatalf("valid whole %s response: %v", call.name, err)
					}
					if !tc.ok && err == nil {
						t.Fatalf("%s accepted %s response", call.name, tc.name)
					}
				})
			}
		})
	}
}

func TestNotificationClient_RejectsFailedMutationAndHTTPStatuses(t *testing.T) {
	for _, call := range notificationClientCalls() {
		for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%s/%d", call.name, status), func(t *testing.T) {
				c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					io.WriteString(w, `{"error":"storage unavailable"}`)
				}))
				if err := call.call(context.Background(), c); err == nil {
					t.Fatalf("%s accepted HTTP %d", call.name, status)
				}
			})
		}
		if call.method != http.MethodPost {
			continue
		}
		for _, response := range []string{`{"ok":false}`, `{}`} {
			t.Run(call.name+"/unconfirmed-success/"+response, func(t *testing.T) {
				c := notificationHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, response)
				}))
				if err := call.call(context.Background(), c); err == nil {
					t.Fatal("mutation reported success without ok:true")
				}
			})
		}
	}
}

type heldNotificationAck struct {
	*notificationSource
	started chan struct{}
	release chan struct{}
}

func (s *heldNotificationAck) Ack(chatID int64, id watchdapi.EventID) error {
	s.started <- struct{}{}
	<-s.release
	return s.Store.Ack(chatID, id)
}

func TestNotificationsAPI_InFlightAckDoesNotBlockReads(t *testing.T) {
	s := newNotificationSource(t, 100)
	id := publishAPIEvent(t, s, "delivery in progress")
	source := &heldNotificationAck{notificationSource: s, started: make(chan struct{}, 1), release: make(chan struct{})}
	c := notificationListenerClient(t, source)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- c.Ack(ctx, 100, id) }()
	t.Cleanup(func() {
		close(source.release)
		defer cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("released Ack: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("released Ack did not finish")
		}
	})
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("Ack did not reach the automation source")
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	watch, err := c.Watch(readCtx)
	readCancel()
	if err != nil || watch.State != watchdapi.WatchActive || watch.Notifications.Pending != 1 {
		t.Errorf("Watch blocked by in-flight Ack: %v", err)
	}
	readCtx, readCancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	page, err := c.Pending(readCtx, "")
	readCancel()
	if err != nil || len(page.Messages) != 1 || page.Messages[0].EventID != id {
		t.Errorf("Pending blocked by in-flight Ack: %v", err)
	}
	readCtx, readCancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	monitor, err := c.Monitor(readCtx)
	readCancel()
	if err != nil || monitor.State != watchdapi.StateOK {
		t.Errorf("Monitor blocked by in-flight Ack: %v", err)
	}
}
