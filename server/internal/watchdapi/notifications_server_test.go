package watchdapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var _ func(watchdapi.Source, ...watchdapi.AutomationSource) http.Handler = watchdapi.NewHandler

// External tests use the real store without a watchdapi -> notifications import cycle.
type notificationSource struct {
	*notifications.Store
	path  string
	at    time.Time
	watch watchdapi.WatchSnapshot
}

func (s *notificationSource) WatchSnapshot() watchdapi.WatchSnapshot {
	snapshot := s.watch
	snapshot.Notifications = s.Status()
	return snapshot
}

func newNotificationSource(t *testing.T, chatIDs ...int64) *notificationSource {
	t.Helper()
	s := &notificationSource{
		path: filepath.Join(t.TempDir(), "watchd-notifications.json"),
		at:   time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond),
	}
	store, err := notifications.NewStore(s.path, func() time.Time { return s.at })
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	s.watch = watchdapi.WatchSnapshot{
		State: watchdapi.WatchActive, UpdatedAt: s.at,
		Message: "watch is active", Action: "monitor",
		CommittedFailover: true, PendingRestore: true,
	}
	var recipients []watchdapi.Recipient
	for _, chatID := range chatIDs {
		recipients = append(recipients, watchdapi.Recipient{ChatID: chatID, FirstSeen: s.at})
	}
	if err := s.ReplaceRecipients(recipients); err != nil {
		t.Fatal(err)
	}
	return s
}

func publishAPIEvent(t *testing.T, s *notificationSource, text string) watchdapi.EventID {
	t.Helper()
	id, err := s.Publish(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type notificationMonitor struct {
	snapshot watchdapi.Snapshot
}

func (m *notificationMonitor) Snapshot() watchdapi.Snapshot { return m.snapshot }

func (m *notificationMonitor) Request(keys []string) (int, error) {
	if m.snapshot.State == watchdapi.StateStopped || m.snapshot.State == watchdapi.StateDisabled {
		return 0, watchdapi.ErrNotActive
	}
	if len(keys) == 0 {
		return len(m.snapshot.Endpoints), nil
	}
	return len(keys), nil
}

func notificationHandler(s watchdapi.AutomationSource) http.Handler {
	return watchdapi.NewHandler(&notificationMonitor{snapshot: watchdapi.Snapshot{State: watchdapi.StateOK}}, s)
}

func notificationRequest(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	h.ServeHTTP(rec, req)
	return rec
}

func decodeAPIPage(t *testing.T, rec *httptest.ResponseRecorder) watchdapi.NotificationPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want 200", rec.Code)
	}
	if size := rec.Body.Len(); size >= 16<<20 {
		t.Fatalf("encoded pending response = %d bytes, want < 16 MiB", size)
	}
	var page watchdapi.NotificationPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) > 100 || len(page.NextCursor) > 256 {
		t.Fatalf("page has %d messages and a %d-byte cursor", len(page.Messages), len(page.NextCursor))
	}
	return page
}

func readAPIPages(t *testing.T, h http.Handler, cursor string) []watchdapi.Notification {
	t.Helper()
	var messages []watchdapi.Notification
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		page := decodeAPIPage(t, notificationRequest(h, http.MethodGet, "/v1/notifications/pending?cursor="+url.QueryEscape(cursor), ""))
		messages = append(messages, page.Messages...)
		if page.NextCursor == "" {
			return messages
		}
		if len(page.Messages) == 0 || seen[page.NextCursor] || page.NextCursor == cursor {
			t.Fatalf("pagination did not advance: %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	t.Fatal("pending pagination did not finish")
	return nil
}

func assertMutationOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("mutation status/body = %d/%q, want 200/{\"ok\":true}", rec.Code, rec.Body.String())
	}
}

func assertRejectedNotificationMutation(t *testing.T, s *notificationSource, target, body string, status int) {
	t.Helper()
	before, err := s.Pending("")
	if err != nil {
		t.Fatal(err)
	}
	durable, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	rec := notificationRequest(notificationHandler(s), http.MethodPost, target, body)
	if rec.Code != status {
		t.Errorf("mutation status = %d, want %d", rec.Code, status)
	}
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil || failure.Error == "" {
		t.Errorf("invalid mutation did not return a JSON error: %v", err)
	}
	after, err := s.Pending("")
	if err != nil || !reflect.DeepEqual(after, before) || s.Status().Pending != len(before.Messages) {
		t.Errorf("rejected mutation changed pending: error=%v count=%d, want %d", err, s.Status().Pending, len(before.Messages))
	}
	got, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(got, durable) {
		t.Errorf("rejected mutation changed durable recipients/pending: %v", err)
	}
	id := publishAPIEvent(t, s, "after rejected mutation")
	messages := readAPIPages(t, notificationHandler(s), "")
	var want []watchdapi.Notification
	for _, chatID := range []int64{100, 200} {
		for _, message := range before.Messages {
			if message.ChatID == chatID {
				want = append(want, message)
			}
		}
		want = append(want, watchdapi.Notification{ChatID: chatID, EventID: id, At: s.at, Text: "after rejected mutation"})
	}
	if !reflect.DeepEqual(messages, want) {
		t.Error("rejected mutation changed recipients or their FirstSeen eligibility")
	}
}

func TestNotificationsAPI_ValidationIsAtomic(t *testing.T) {
	t.Run("recipients", func(t *testing.T) {
		stamp := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
		valid := fmt.Sprintf(`{"recipients":[{"chat_id":300,"first_seen":%q}]}`, stamp)
		for _, tc := range []struct {
			name string
			body string
		}{
			{"empty", ""},
			{"whitespace", " \n\t"},
			{"null", `null`},
			{"missing-recipients", `{}`},
			{"null-recipients", `{"recipients":null}`},
			{"array-document", `[]`},
			{"wrong-recipients-type", `{"recipients":{}}`},
			{"null-recipient", `{"recipients":[null]}`},
			{"wrong-recipient-type", `{"recipients":[42]}`},
			{"missing-chat-id", fmt.Sprintf(`{"recipients":[{"first_seen":%q}]}`, stamp)},
			{"zero-chat-id", fmt.Sprintf(`{"recipients":[{"chat_id":0,"first_seen":%q}]}`, stamp)},
			{"string-chat-id", fmt.Sprintf(`{"recipients":[{"chat_id":"300","first_seen":%q}]}`, stamp)},
			{"fractional-chat-id", fmt.Sprintf(`{"recipients":[{"chat_id":300.5,"first_seen":%q}]}`, stamp)},
			{"duplicate-chat-id", fmt.Sprintf(`{"recipients":[{"chat_id":300,"first_seen":%q},{"chat_id":300,"first_seen":%q}]}`, stamp, stamp)},
			{"missing-first-seen", `{"recipients":[{"chat_id":300}]}`},
			{"null-first-seen", `{"recipients":[{"chat_id":300,"first_seen":null}]}`},
			{"zero-first-seen", `{"recipients":[{"chat_id":300,"first_seen":"0001-01-01T00:00:00Z"}]}`},
			{"future-first-seen", fmt.Sprintf(`{"recipients":[{"chat_id":300,"first_seen":%q}]}`, future)},
			{"wrong-first-seen-type", `{"recipients":[{"chat_id":300,"first_seen":42}]}`},
			{"malformed-first-seen", `{"recipients":[{"chat_id":300,"first_seen":"yesterday"}]}`},
			{"trailing-object", valid + ` {}`},
			{"trailing-null", valid + ` null`},
			{"trailing-garbage", valid + ` trailing`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newNotificationSource(t, 100, 200)
				publishAPIEvent(t, s, "before rejected mutation")
				assertRejectedNotificationMutation(t, s, "/v1/notifications/recipients", tc.body, http.StatusBadRequest)
			})
		}
	})

	t.Run("ack", func(t *testing.T) {
		for _, name := range []string{
			"empty", "null", "missing-chat", "missing-event", "null-event", "wrong-document-type",
			"zero-chat", "string-chat", "wrong-event-type", "malformed-event", "zero-sequence",
			"leading-zero-sequence", "negative-sequence", "uppercase-epoch", "sequence-overflow", "future-sequence",
			"trailing-object", "trailing-null", "trailing-garbage",
		} {
			t.Run(name, func(t *testing.T) {
				s := newNotificationSource(t, 100, 200)
				id := publishAPIEvent(t, s, "before rejected mutation")
				epoch, _, _ := strings.Cut(string(id), ":")
				valid := fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, id)
				body := map[string]string{
					"empty":                 "",
					"null":                  `null`,
					"missing-chat":          fmt.Sprintf(`{"event_id":%q}`, id),
					"missing-event":         `{"chat_id":100}`,
					"null-event":            `{"chat_id":100,"event_id":null}`,
					"wrong-document-type":   `[]`,
					"zero-chat":             fmt.Sprintf(`{"chat_id":0,"event_id":%q}`, id),
					"string-chat":           fmt.Sprintf(`{"chat_id":"100","event_id":%q}`, id),
					"wrong-event-type":      `{"chat_id":100,"event_id":42}`,
					"malformed-event":       `{"chat_id":100,"event_id":"not-an-event"}`,
					"zero-sequence":         fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, epoch+":0"),
					"leading-zero-sequence": fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, epoch+":01"),
					"negative-sequence":     fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, epoch+":-1"),
					"uppercase-epoch":       `{"chat_id":100,"event_id":"0123456789ABCDEF0123456789ABCDEF:1"}`,
					"sequence-overflow":     fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, epoch+":18446744073709551616"),
					"future-sequence":       fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, epoch+":2"),
					"trailing-object":       valid + ` {}`,
					"trailing-null":         valid + ` null`,
					"trailing-garbage":      valid + ` trailing`,
				}[name]
				assertRejectedNotificationMutation(t, s, "/v1/notifications/ack", body, http.StatusBadRequest)
			})
		}
	})

	t.Run("body-cap-includes-trailing-whitespace", func(t *testing.T) {
		for _, target := range []string{"/v1/notifications/recipients", "/v1/notifications/ack"} {
			t.Run(target, func(t *testing.T) {
				s := newNotificationSource(t, 100, 200)
				id := publishAPIEvent(t, s, "before rejected mutation")
				body := `{"recipients":[]}`
				if strings.HasSuffix(target, "/ack") {
					body = fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, id)
				}
				body += strings.Repeat(" ", (1<<20)+1-len(body))
				assertRejectedNotificationMutation(t, s, target, body, http.StatusRequestEntityTooLarge)
			})
		}
	})

	t.Run("exact-body-limit-allows-explicit-revocation", func(t *testing.T) {
		s := newNotificationSource(t, 100, 200)
		publishAPIEvent(t, s, "before revocation")
		body := `{"recipients":[]}`
		body += strings.Repeat(" ", (1<<20)-len(body))
		assertMutationOK(t, notificationRequest(notificationHandler(s), http.MethodPost, "/v1/notifications/recipients", body))
		publishAPIEvent(t, s, "after revocation")
		if messages := readAPIPages(t, notificationHandler(s), ""); len(messages) != 0 || s.Status().Pending != 0 {
			t.Fatal("explicit [] did not revoke every recipient")
		}
	})

	t.Run("negative-telegram-chat-is-valid", func(t *testing.T) {
		s := newNotificationSource(t, 100, 200)
		id := publishAPIEvent(t, s, "negative chat history")
		body := fmt.Sprintf(`{"recipients":[{"chat_id":-100123,"first_seen":%q}]} `, s.at.Format(time.RFC3339Nano))
		h := notificationHandler(s)
		assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/recipients", body))
		want := []watchdapi.Notification{{ChatID: -100123, EventID: id, At: s.at, Text: "negative chat history"}}
		if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("negative chat pending = %+v, want %+v", got, want)
		}
		ack := fmt.Sprintf(`{"chat_id":-100123,"event_id":%q}`, id)
		assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", ack))
		assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", ack))
		if got := readAPIPages(t, h, ""); len(got) != 0 {
			t.Fatal("negative chat acknowledgement left pending messages")
		}
	})
}

type unfinishedNotificationSource struct {
	*notificationSource
	recipientsCalls atomic.Int64
	ackCalls        atomic.Int64
}

func (s *unfinishedNotificationSource) ReplaceRecipients(recipients []watchdapi.Recipient) error {
	s.recipientsCalls.Add(1)
	return s.Store.ReplaceRecipients(recipients)
}

func (s *unfinishedNotificationSource) Ack(chatID int64, eventID watchdapi.EventID) error {
	s.ackCalls.Add(1)
	return s.Store.Ack(chatID, eventID)
}

func TestNotificationsAPI_UnfinishedPOSTDeadlineIsAtomic(t *testing.T) {
	for _, name := range []string{"recipients", "ack"} {
		t.Run(name, func(t *testing.T) {
			s := newNotificationSource(t, 100, 200)
			id := publishAPIEvent(t, s, "before unfinished POST")
			before, err := s.Pending("")
			if err != nil {
				t.Fatal(err)
			}
			statusBefore := s.Status()
			durableBefore, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			src := &unfinishedNotificationSource{notificationSource: s}
			path := notificationSocketPath(t)
			ctx, cancel := context.WithCancel(context.Background())
			listener, err := watchdapi.Listen(ctx, path)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			served := make(chan error, 1)
			go func() {
				served <- watchdapi.ServeListener(ctx, listener, &notificationMonitor{snapshot: watchdapi.Snapshot{State: watchdapi.StateOK}}, src)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-served:
					if err != nil {
						t.Errorf("ServeListener: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Error("unfinished POST server did not drain")
				}
				_ = listener.Close()
			})
			conn, err := net.DialTimeout("unix", path, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			body := `{"recipients":[]}`
			if name == "ack" {
				body = fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, id)
			}
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			// The JSON is complete, but one declared body byte never arrives.
			if _, err := fmt.Fprintf(conn, "POST /v1/notifications/%s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", name, len(body)+1, body); err != nil {
				t.Fatal(err)
			}
			response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
					t.Fatalf("unfinished POST did not receive a server response/close before the client deadline: %v", readErr)
				}
			} else {
				data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				_ = response.Body.Close()
				var failure struct {
					Error string `json:"error"`
				}
				if err != nil || response.StatusCode != http.StatusBadRequest || json.Unmarshal(data, &failure) != nil || failure.Error != "invalid request body" {
					t.Fatalf("unfinished POST response=%d/%q error=%v, want safe 400", response.StatusCode, data, err)
				}
			}
			if elapsed := time.Since(started); elapsed < 1500*time.Millisecond || elapsed > 3*time.Second {
				t.Fatalf("unfinished POST ended after %s, want the server's 2-second body deadline", elapsed)
			}
			if src.recipientsCalls.Load() != 0 || src.ackCalls.Load() != 0 {
				t.Fatalf("unfinished body reached a source mutation: recipients=%d ack=%d", src.recipientsCalls.Load(), src.ackCalls.Load())
			}
			after, err := s.Pending("")
			if err != nil || !reflect.DeepEqual(after, before) || s.Status() != statusBefore {
				t.Fatalf("unfinished body changed RAM pending/status: %+v, %v", after, err)
			}
			durableAfter, err := os.ReadFile(s.path)
			if err != nil || !bytes.Equal(durableAfter, durableBefore) {
				t.Fatalf("unfinished body changed durable recipients/ack progress: %v", err)
			}
			next := publishAPIEvent(t, s, "after unfinished POST")
			var want []watchdapi.Notification
			for _, chatID := range []int64{100, 200} {
				want = append(want,
					watchdapi.Notification{ChatID: chatID, EventID: id, At: s.at, Text: "before unfinished POST"},
					watchdapi.Notification{ChatID: chatID, EventID: next, At: s.at, Text: "after unfinished POST"},
				)
			}
			if got := readAPIPages(t, notificationHandler(s), ""); !reflect.DeepEqual(got, want) {
				t.Fatalf("unfinished POST altered recipient eligibility or per-chat progress: %+v", got)
			}
		})
	}
}

func TestPending_PaginationUnderMutation(t *testing.T) {
	s := newNotificationSource(t, 1000, 10, -2, 100, 2, -1000)
	h := notificationHandler(s)
	var ids []watchdapi.EventID
	for i := 1; i <= 19; i++ {
		ids = append(ids, publishAPIEvent(t, s, fmt.Sprintf("event %02d", i)))
	}
	var original []watchdapi.Notification
	for _, chatID := range []int64{-1000, -2, 2, 10, 100, 1000} {
		for i, id := range ids {
			original = append(original, watchdapi.Notification{ChatID: chatID, EventID: id, At: s.at, Text: fmt.Sprintf("event %02d", i+1)})
		}
	}
	first := decodeAPIPage(t, notificationRequest(h, http.MethodGet, "/v1/notifications/pending", ""))
	if len(first.Messages) == 0 || first.NextCursor == "" || !reflect.DeepEqual(first.Messages, original[:len(first.Messages)]) {
		t.Fatal("first page did not preserve numeric chat/sequence ordering")
	}
	if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, original) || s.Status().Pending != 114 {
		t.Fatal("reading pages changed or omitted the original 114 delivery entries")
	}
	again := decodeAPIPage(t, notificationRequest(h, http.MethodGet, "/v1/notifications/pending", ""))
	if !reflect.DeepEqual(again, first) {
		t.Fatal("unchanged first-page read consumed messages")
	}
	acked := first.Messages[0]
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", fmt.Sprintf(`{"chat_id":%d,"event_id":%q}`, acked.ChatID, acked.EventID)))
	var recipients []watchdapi.Recipient
	for _, chatID := range []int64{1000, -99, 10, -2, 100, 2, -1000} {
		recipients = append(recipients, watchdapi.Recipient{ChatID: chatID, FirstSeen: s.at})
	}
	body, err := json.Marshal(struct {
		Recipients []watchdapi.Recipient `json:"recipients"`
	}{recipients})
	if err != nil {
		t.Fatal(err)
	}
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/recipients", string(body)))
	inserted := publishAPIEvent(t, s, "between pages")
	var all []watchdapi.Notification
	for _, chatID := range []int64{-1000, -99, -2, 2, 10, 100, 1000} {
		for i, id := range ids {
			all = append(all, watchdapi.Notification{ChatID: chatID, EventID: id, At: s.at, Text: fmt.Sprintf("event %02d", i+1)})
		}
		all = append(all, watchdapi.Notification{ChatID: chatID, EventID: inserted, At: s.at, Text: "between pages"})
	}
	last := first.Messages[len(first.Messages)-1]
	boundary := -1
	var want []watchdapi.Notification
	for i, message := range all {
		if message.ChatID == last.ChatID && message.EventID == last.EventID {
			boundary = i
		}
		if message.ChatID != acked.ChatID || message.EventID != acked.EventID {
			want = append(want, message)
		}
	}
	if boundary < 0 {
		t.Fatal("fixture lost the cursor boundary")
	}
	if got := readAPIPages(t, h, first.NextCursor); !reflect.DeepEqual(got, all[boundary+1:]) {
		t.Fatal("ack/insertion shifted the cursor past remaining delivery entries")
	}
	if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, want) || len(got) != 139 || s.Status().Pending != 139 {
		t.Fatal("fresh empty-cursor pass missed events inserted before the previous cursor")
	}
	before := readAPIPages(t, h, "")
	for _, cursor := range []string{"not-a-cursor", first.NextCursor + "!", strings.Repeat("x", 256), strings.Repeat("x", 257)} {
		rec := notificationRequest(h, http.MethodGet, "/v1/notifications/pending?cursor="+url.QueryEscape(cursor), "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid %d-byte cursor status = %d, want 400", len(cursor), rec.Code)
		}
		if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, before) || s.Status().Pending != 139 {
			t.Error("invalid cursor mutated the pending queue")
		}
	}
}

func TestPending_EncodedResponseCap(t *testing.T) {
	s := newNotificationSource(t, -1000, -2, 2, 10, 100, 1000)
	text := strings.Repeat("<", 600<<10)
	id := publishAPIEvent(t, s, text)
	messages := readAPIPages(t, notificationHandler(s), "")
	if len(messages) != 6 || s.Status().Pending != 6 {
		t.Fatal("size-bounded pagination consumed or omitted messages")
	}
	for i, chatID := range []int64{-1000, -2, 2, 10, 100, 1000} {
		message := messages[i]
		if message.ChatID != chatID || message.EventID != id || !message.At.Equal(s.at) || message.Text != text {
			t.Fatalf("size-bounded message %d changed", i)
		}
	}
}

func TestNotificationsAPI_AckClosedUnknownAndPreviousEpoch(t *testing.T) {
	s := newNotificationSource(t, 100, 200)
	h := notificationHandler(s)
	var ids []watchdapi.EventID
	for i := 0; i < 21; i++ {
		ids = append(ids, publishAPIEvent(t, s, fmt.Sprintf("event %02d", i)))
	}
	before := readAPIPages(t, h, "")
	epoch, _, _ := strings.Cut(string(ids[0]), ":")
	previousEpoch := "0123456789abcdef0123456789abcdef"
	if previousEpoch == epoch {
		previousEpoch = "fedcba9876543210fedcba9876543210"
	}
	for _, ack := range []struct {
		chatID int64
		id     watchdapi.EventID
	}{
		{100, ids[0]},
		{900, ids[1]},
		{100, watchdapi.EventID(previousEpoch + ":18446744073709551615")},
	} {
		body := fmt.Sprintf(`{"chat_id":%d,"event_id":%q}`, ack.chatID, ack.id)
		assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", body))
		assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", body))
		if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, before) {
			t.Fatal("closed/unknown/previous-epoch ack changed the current queue")
		}
	}
	body := fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, ids[20])
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", body))
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", body))
	var want []watchdapi.Notification
	for _, message := range before {
		if message.ChatID != 100 || message.EventID != ids[20] {
			want = append(want, message)
		}
	}
	if got := readAPIPages(t, h, ""); !reflect.DeepEqual(got, want) || s.Status().Pending != 39 {
		t.Fatal("ack removed another chat or another event")
	}
	s.at = s.at.Add(12*time.Hour + time.Nanosecond)
	expired := fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, ids[1])
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", expired))
	assertMutationOK(t, notificationRequest(h, http.MethodPost, "/v1/notifications/ack", expired))
	if got := readAPIPages(t, h, ""); len(got) != 0 {
		t.Fatal("expired ack revived messages")
	}
}

type failedNotificationSource struct {
	*notificationSource
	failure error
}

func (s *failedNotificationSource) ReplaceRecipients([]watchdapi.Recipient) error { return s.failure }
func (s *failedNotificationSource) Ack(int64, watchdapi.EventID) error            { return s.failure }

func TestNotificationsAPI_StorageFailureIsSafe503(t *testing.T) {
	for _, target := range []string{"/v1/notifications/recipients", "/v1/notifications/ack"} {
		t.Run(target, func(t *testing.T) {
			s := newNotificationSource(t, 100)
			id := publishAPIEvent(t, s, "still pending")
			before := readAPIPages(t, notificationHandler(s), "")
			failure := errors.New("synthetic-provider-detail.example.test: private storage path")
			src := &failedNotificationSource{notificationSource: s, failure: failure}
			body := `{"recipients":[]}`
			if strings.HasSuffix(target, "/ack") {
				body = fmt.Sprintf(`{"chat_id":100,"event_id":%q}`, id)
			}
			rec := notificationRequest(notificationHandler(src), http.MethodPost, target, body)
			var response struct {
				Error string `json:"error"`
			}
			if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Error == "" {
				t.Fatalf("storage failure status = %d, want 503 with JSON error", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "synthetic-provider-detail") || strings.Contains(rec.Body.String(), "private storage path") {
				t.Fatal("storage response exposed the source error")
			}
			if got := readAPIPages(t, notificationHandler(src), ""); !reflect.DeepEqual(got, before) {
				t.Fatal("failed source mutation changed pending")
			}
		})
	}
}

func TestNotificationsAPI_MonitorCompatibilityAndWatchSnapshot(t *testing.T) {
	s := newNotificationSource(t, 100)
	publishAPIEvent(t, s, "watch event")
	monitor := &notificationMonitor{snapshot: watchdapi.Snapshot{
		State: watchdapi.StateOK, UpdatedAt: s.at, IntervalSeconds: 60,
		Endpoints: map[string]watchdapi.EndpointState{"fixture": {Status: watchdapi.StatusAlive, LatencyMS: 142}},
	}}
	for _, automation := range []bool{false, true} {
		t.Run(fmt.Sprintf("automation=%t", automation), func(t *testing.T) {
			h := watchdapi.NewHandler(monitor)
			if automation {
				h = watchdapi.NewHandler(monitor, s)
			}
			rec := notificationRequest(h, http.MethodGet, "/v1/monitor", "")
			var snapshot watchdapi.Snapshot
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snapshot) != nil || !reflect.DeepEqual(snapshot, monitor.snapshot) {
				t.Fatal("monitor DTO/status changed")
			}
			rec = notificationRequest(h, http.MethodPost, "/v1/monitor/check", `{"keys":["fixture"]}`)
			var queued struct {
				Queued int `json:"queued"`
			}
			if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &queued) != nil || queued.Queued != 1 {
				t.Fatal("monitor check lost its 202/queued contract")
			}
			if rec := notificationRequest(h, http.MethodPost, "/v1/monitor/check", `null`); rec.Code != http.StatusBadRequest {
				t.Fatalf("monitor null status = %d, want 400", rec.Code)
			}
			if automation {
				rec = notificationRequest(h, http.MethodGet, "/v1/watch", "")
				var watch watchdapi.WatchSnapshot
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &watch) != nil || !reflect.DeepEqual(watch, s.WatchSnapshot()) {
					t.Fatal("watch snapshot did not retain its action, restore and notification fields")
				}
			}
		})
	}
	stopped := &notificationMonitor{snapshot: watchdapi.Snapshot{State: watchdapi.StateStopped}}
	rec := notificationRequest(watchdapi.NewHandler(stopped, s), http.MethodPost, "/v1/monitor/check", `{}`)
	var response struct {
		State watchdapi.State `json:"state"`
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.State != watchdapi.StateStopped {
		t.Fatal("stopped monitor check lost its 409/state contract")
	}
}
