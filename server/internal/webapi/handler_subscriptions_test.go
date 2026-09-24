package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// alphaBeta is two subscriptions that both name a server Germany-1.
func alphaBeta() []vpnconfig.Subscription {
	return []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Servers: []vpnconfig.Server{
			{Name: "Oslo", Address: "a.example.com", Port: 443, UUID: "uuid-1", IPs: []string{"192.0.2.10"}},
			{Name: "Germany-1", Address: "b.example.com", Port: 443, UUID: "uuid-2", IPs: []string{"192.0.2.11"}},
		}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []vpnconfig.Server{
			{Name: "Germany-1", Address: "198.51.100.20", Port: 8443, UUID: "uuid-3", IPs: []string{"198.51.100.20"}},
		}},
	}
}

func subsDeps(t *testing.T, subs ...vpnconfig.Subscription) (*Deps, *mockConfig) {
	t.Helper()
	mc := &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
	deps := newTestDeps(t)
	deps.Config = mc
	return deps, mc
}

// call serves one request and decodes the JSON answer.
func call(t *testing.T, h http.HandlerFunc, method, target, body string) (int, map[string]interface{}) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, reader))
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func TestHandleListSubscriptions_ShowsTheHostAndNoLink(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	rec := httptest.NewRecorder()

	handleListSubscriptions(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/subscriptions", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "/s/t") || strings.Contains(body, "uuid-") {
		t.Fatalf("%d %s", rec.Code, body)
	}
	var resp struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	s := resp.Subscriptions
	if len(s) != 2 || s[0].Host != "sub.example.com" || s[0].Servers != 2 || s[0].Static || !s[1].Static || s[1].Host != "" {
		t.Fatalf("%+v", s)
	}
}

func TestHandleListSubscriptions_NoneIsAnEmptyList(t *testing.T) {
	deps, _ := subsDeps(t)
	rec := httptest.NewRecorder()

	handleListSubscriptions(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/subscriptions", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"subscriptions":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAddSubscription_SavesAndSummarizes(t *testing.T) {
	deps, mc := subsDeps(t)
	deps.ImportClient = subscriptionHost(t, osloSubscription)

	code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/token","name":"Alpha"}`)

	if code != http.StatusOK || resp["name"] != "Alpha" || resp["summary"] != "Alpha: Imported 1 servers" || resp["count"] != float64(1) || resp["existed"] != false {
		t.Fatalf("%d %v", code, resp)
	}
	if len(mc.subs) != 1 || mc.subs[0].URL != "https://93.184.216.34/s/token" {
		t.Fatalf("files %+v", mc.subs)
	}
	if !reflect.DeepEqual(mc.cfg.Xray.Servers, []string{"203.0.113.10"}) {
		t.Fatalf("xray.servers %v", mc.cfg.Xray.Servers)
	}
}

func TestHandleAddSubscription_Refusals(t *testing.T) {
	deps, mc := subsDeps(t, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"})
	deps.ImportClient = subscriptionHost(t, osloSubscription)
	for body, want := range map[string]int{
		`{"url":""}`:                                       http.StatusBadRequest,
		`{"url":"http://93.184.216.34/s"}`:                 http.StatusBadRequest,
		`{"url":"https://10.0.0.1/s"}`:                     http.StatusBadRequest,
		`{"url":"https://93.184.216.34/s","name":"ALPHA"}`: http.StatusBadRequest,
		`{"url":"https://93.184.216.34/s","name":"\tx"}`:   http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", body); code != want {
			t.Errorf("%s: %d %v, want %d", body, code, resp, want)
		}
	}
	if len(mc.subs) != 1 {
		t.Fatalf("a refused add wrote %+v", mc.subs)
	}
}

func TestHandleAddSubscription_ADownloadThatFailedIs502AndNamesNoLink(t *testing.T) {
	deps, _ := subsDeps(t)
	deps.ImportClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/secret-token"}`)

	if code != http.StatusBadGateway || strings.Contains(fmt.Sprint(resp), "secret-token") {
		t.Fatalf("%d %v", code, resp)
	}
}

// The answer says what the import left out and why: the counts beside the
// line the page shows.
func TestHandleAddSubscription_ReportsTheSkips(t *testing.T) {
	deps, mc := subsDeps(t)
	deps.ImportClient = subscriptionHost(t, strings.Join([]string{
		"vless://uuid-1@203.0.113.10:443?type=tcp#Oslo",
		"vless://uuid-2@203.0.113.11:443?type=kcp#KCP",
		"vless://uuid-3@127.0.0.1:1#Expired",
	}, "\n"))

	code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/token","name":"Alpha"}`)

	if code != http.StatusOK || resp["count"] != float64(1) || resp["total"] != float64(3) || resp["dns_errors"] != float64(0) {
		t.Fatalf("%d %v", code, resp)
	}
	skipped, _ := resp["skipped"].(map[string]interface{})
	if skipped["unsupported"] != float64(1) || skipped["placeholder"] != float64(1) || skipped["composite"] != float64(0) {
		t.Fatalf("skipped %v", skipped)
	}
	if resp["summary"] != "Alpha: Imported 1 of 3 servers: 1 unsupported, 1 placeholder" {
		t.Fatalf("summary %v", resp["summary"])
	}
	if len(mc.subs) != 1 || len(mc.subs[0].Servers) != 1 || mc.subs[0].Servers[0].Name != "Oslo" {
		t.Fatalf("%d files", len(mc.subs))
	}
}

// A body the router can use nothing of is refused with the reason, and no
// subscription is made of it.
func TestHandleAddSubscription_ABodyWithoutAServerIs400AndSaysWhy(t *testing.T) {
	for body, want := range map[string]string{
		"tuic://uuid:pw@203.0.113.10:443#TUIC\nssr://c29tZQ": "no supported servers in subscription: 2 unsupported; TUIC: tuic; #2: ssr",
		"<!doctype html><html>Open the app</html>":           "unrecognized subscription format",
	} {
		deps, mc := subsDeps(t)
		deps.ImportClient = subscriptionHost(t, body)

		code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/token"}`)

		if code != http.StatusBadRequest || resp["error"] != want {
			t.Errorf("got %d %v, want 400 %q", code, resp, want)
		}
		if len(mc.subs) != 0 {
			t.Errorf("a refused body wrote %d files", len(mc.subs))
		}
	}
}

// "subscription saved" is said only when the file was written: a user told so
// must not add it again, and one who was not must.
func TestHandleAddSubscription_SaysSavedOnlyWhenTheFileWasWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mc    *mockConfig
		want  string
		saved bool
	}{
		{
			name: "lock that cannot be opened",
			mc:   &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, updateErr: errors.New("open config lock: permission denied")},
			want: "open config lock: permission denied",
		},
		{
			name: "lock another writer holds",
			mc:   &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, updateErr: service.ErrConfigLockTimeout},
			want: "config is busy; nothing was saved",
		},
		{
			name:  "config write that fails after the file",
			mc:    &mockConfig{cfg: &vpnconfig.VPNDirectorConfig{}, saveVPNCfgErr: errors.New("disk full")},
			want:  "subscription saved, but xray.servers sync failed: disk full",
			saved: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := newTestDeps(t)
			deps.Config = tc.mc
			deps.ImportClient = subscriptionHost(t, osloSubscription)

			code, resp := call(t, handleAddSubscription(deps), "POST", "/api/subscriptions", `{"url":"https://93.184.216.34/s/token"}`)

			if code != http.StatusInternalServerError || resp["error"] != tc.want {
				t.Fatalf("got %d %v, want 500 %q", code, resp, tc.want)
			}
			if wrote := len(tc.mc.subs) == 1; wrote != tc.saved {
				t.Fatalf("file written: %v, want %v", wrote, tc.saved)
			}
		})
	}
}

func TestHandleRefreshSubscriptions(t *testing.T) {
	deps, mc := subsDeps(t,
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
	)
	deps.ImportClient = subscriptionHost(t, osloSubscription)
	h := handleRefreshSubscriptions(deps)

	code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=0a1b2c3d", "")
	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 1 || results[0].(map[string]interface{})["summary"] != "Alpha: Imported 1 servers" {
		t.Fatalf("one: %d %v", code, resp)
	}
	if len(mc.subs[0].Servers) != 1 {
		t.Fatalf("files %+v", mc.subs)
	}

	// All of them: the static list is left out.
	code, resp = call(t, h, "POST", "/api/subscriptions/refresh", "")
	if results, _ := resp["results"].([]interface{}); code != http.StatusOK || len(results) != 1 {
		t.Fatalf("all: %d %v", code, resp)
	}

	if code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=ffffffff", ""); code != http.StatusNotFound {
		t.Fatalf("unknown: %d %v", code, resp)
	}
	if code, resp := call(t, h, "POST", "/api/subscriptions/refresh?id=1b2c3d4e", ""); code != http.StatusBadRequest {
		t.Fatalf("static: %d %v", code, resp)
	}
}

// A refresh whose download fails answers 200: the result says why, and the
// subscription keeps its list and records the failure.
func TestHandleRefreshSubscriptions_AFailedDownloadIsAResult(t *testing.T) {
	deps, mc := subsDeps(t, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a",
		Servers: []vpnconfig.Server{{Name: "Old", IPs: []string{"192.0.2.1"}}}})
	deps.ImportClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	code, resp := call(t, handleRefreshSubscriptions(deps), "POST", "/api/subscriptions/refresh?id=0a1b2c3d", "")

	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 1 || results[0].(map[string]interface{})["error"] != "download failed: connection refused" {
		t.Fatalf("%d %v", code, resp)
	}
	if mc.subs[0].Error != "download failed: connection refused" || mc.subs[0].Servers[0].Name != "Old" {
		t.Fatalf("file %+v", mc.subs[0])
	}
}

// A refresh whose config write fails after the file says the list is saved:
// the new list is out, and only xray.servers beside it is stale.
func TestHandleRefreshSubscriptions_SaysTheListWasSavedWhenOnlyTheConfigFailed(t *testing.T) {
	deps, mc := subsDeps(t, vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a",
		Servers: []vpnconfig.Server{{Name: "Old", IPs: []string{"192.0.2.1"}}}})
	deps.ImportClient = subscriptionHost(t, osloSubscription)
	mc.saveVPNCfgErr = errors.New("disk full")

	code, resp := call(t, handleRefreshSubscriptions(deps), "POST", "/api/subscriptions/refresh?id=0a1b2c3d", "")

	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 1 {
		t.Fatalf("%d %v", code, resp)
	}
	r := results[0].(map[string]interface{})
	if r["error"] != "list saved, but xray.servers sync failed: disk full" || r["summary"] != "Alpha: list saved, but xray.servers sync failed: disk full" {
		t.Fatalf("result %v", r)
	}
	if s := mc.subs[0]; len(s.Servers) != 1 || s.Servers[0].Name != "Oslo" {
		t.Fatalf("file: %d servers", len(s.Servers))
	}
}

// Every subscription with a link refreshes at once, and the answer keeps the
// subscription order, which the page's table has too.
func TestHandleRefreshSubscriptions_AllAnswersEveryLinkInOrder(t *testing.T) {
	deps, mc := subsDeps(t,
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/s/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
		vpnconfig.Subscription{ID: "2c3d4e5f", Name: "Gamma", URL: "https://93.184.216.34/s/c"},
	)
	deps.ImportClient = subscriptionHost(t, osloSubscription)

	code, resp := call(t, handleRefreshSubscriptions(deps), "POST", "/api/subscriptions/refresh", "")

	results, _ := resp["results"].([]interface{})
	if code != http.StatusOK || len(results) != 2 {
		t.Fatalf("%d %v", code, resp)
	}
	for i, want := range []string{"Alpha: Imported 1 servers", "Gamma: Imported 1 servers"} {
		if got := results[i].(map[string]interface{})["summary"]; got != want {
			t.Errorf("result %d: %v, want %q", i, got, want)
		}
	}
	if len(mc.subs[0].Servers) != 1 || len(mc.subs[2].Servers) != 1 {
		t.Fatalf("files: %d and %d servers", len(mc.subs[0].Servers), len(mc.subs[2].Servers))
	}
}

func TestHandleRenameSubscription(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	h := handleRenameSubscription(deps)

	if code, resp := call(t, h, "POST", "/api/subscriptions/rename?id=0a1b2c3d", `{"name":"Main"}`); code != http.StatusOK || mc.subs[0].Name != "Main" {
		t.Fatalf("%d %v", code, resp)
	}
	for _, tc := range []struct {
		target, body string
		want         int
	}{
		{"/api/subscriptions/rename?id=0a1b2c3d", `{"name":"beta"}`, http.StatusBadRequest},
		{"/api/subscriptions/rename?id=0a1b2c3d", `{"name":""}`, http.StatusBadRequest},
		{"/api/subscriptions/rename", `{"name":"X"}`, http.StatusBadRequest},
		{"/api/subscriptions/rename?id=ffffffff", `{"name":"X"}`, http.StatusNotFound},
	} {
		if code, resp := call(t, h, "POST", tc.target, tc.body); code != tc.want {
			t.Errorf("%s %s: %d %v, want %d", tc.target, tc.body, code, resp, tc.want)
		}
	}
}

func TestHandleDeleteSubscription_SaysTheRunningServerCameFromIt(t *testing.T) {
	deps, mc := subsDeps(t, alphaBeta()...)
	mc.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "a.example.com", Port: 443}
	h := handleDeleteSubscription(deps)

	code, resp := call(t, h, "DELETE", "/api/subscriptions?id=0a1b2c3d", "")

	if code != http.StatusOK || resp["active_removed"] != true || len(mc.subs) != 1 {
		t.Fatalf("%d %v, files %d", code, resp, len(mc.subs))
	}
	if !reflect.DeepEqual(mc.cfg.Xray.Servers, []string{"198.51.100.20"}) {
		t.Fatalf("xray.servers %v", mc.cfg.Xray.Servers)
	}
	if code, resp := call(t, h, "DELETE", "/api/subscriptions?id=0a1b2c3d", ""); code != http.StatusNotFound {
		t.Fatalf("again: %d %v", code, resp)
	}
}

// A delete whose config write fails after the file is gone says it deleted:
// only xray.servers beside it is stale. The user still hears when the running
// server came from it.
func TestHandleDeleteSubscription_SaysDeletedWhenOnlyTheConfigFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active *vpnconfig.ActiveServer
		want   string
	}{
		{
			name:   "the running server came from it",
			active: &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "a.example.com", Port: 443},
			want:   "subscription deleted, but xray.servers sync failed: disk full. The running server came from it; select another server.",
		},
		{
			name:   "the running server came from another",
			active: &vpnconfig.ActiveServer{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "198.51.100.20", Port: 8443},
			want:   "subscription deleted, but xray.servers sync failed: disk full",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, mc := subsDeps(t, alphaBeta()...)
			mc.cfg.Xray.ActiveServer = tc.active
			mc.saveVPNCfgErr = errors.New("disk full")

			code, resp := call(t, handleDeleteSubscription(deps), "DELETE", "/api/subscriptions?id=0a1b2c3d", "")

			if code != http.StatusInternalServerError || resp["error"] != tc.want {
				t.Fatalf("got %d %v, want 500 %q", code, resp, tc.want)
			}
			if len(mc.subs) != 1 || mc.subs[0].ID != "1b2c3d4e" {
				t.Fatalf("%d files left", len(mc.subs))
			}
		})
	}
}

func TestSubscriptionRoutesAreRegistered(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	h := NewRouter(deps, nil)
	token := newTestToken(t, deps)
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve("GET", "/api/subscriptions", ""); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := serve("POST", "/api/subscriptions/rename?id=0a1b2c3d", `{"name":"Main"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := serve("DELETE", "/api/subscriptions?id=1b2c3d4e", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	// The import route of the single subscription is gone.
	if rec := serve("POST", "/api/servers/import", `{"url":""}`); rec.Code != http.StatusNotFound {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
}

// The add and refresh routes are there too: each answers a refusal of its own
// where a path the router does not know answers 404, and neither downloads.
func TestSubscriptionRoutesAreRegistered_AddAndRefresh(t *testing.T) {
	deps, _ := subsDeps(t, alphaBeta()...)
	h := NewRouter(deps, nil)
	token := newTestToken(t, deps)
	for _, tc := range []struct{ path, body, want string }{
		{"/api/subscriptions", `{"url":""}`, "url is required"},
		{"/api/subscriptions/refresh?id=1b2c3d4e", "", "a static list has no link to refresh"},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("POST %s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}
