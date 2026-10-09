package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// fakeMonitor is vpn-director-watchd for the handler tests.
type fakeMonitor struct {
	snap     watchdapi.Snapshot
	err      error
	checkErr error
	checked  [][]string
}

func (f *fakeMonitor) Monitor(context.Context) (watchdapi.Snapshot, error) { return f.snap, f.err }

func (f *fakeMonitor) Check(_ context.Context, keys []string) (int, error) {
	f.checked = append(f.checked, keys)
	return len(keys), f.checkErr
}

func trojanServer(name, ip string) vpnconfig.Server {
	return vpnconfig.Server{Name: name, Address: name + ".example", Port: 443, IPs: []string{ip},
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + name + `.example","port":443,"password":"secret"}]}}`)}
}

func monitorDeps(t *testing.T, mon watchdapi.API) (*Deps, []vpnconfig.Server) {
	t.Helper()
	servers := []vpnconfig.Server{trojanServer("oslo", "192.0.2.1"), trojanServer("riga", "192.0.2.2")}
	deps := newTestDeps(t)
	deps.Config = &mockConfig{subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Alpha", Servers: servers}}}
	deps.Monitor = mon
	for i := range servers {
		servers[i].Subscription = "0a1b2c3d"
	}
	return deps, servers
}

func TestHandleMonitor_FoldsTheEndpointsOfEveryServer(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	mon.snap = watchdapi.Snapshot{State: watchdapi.StateOK, IntervalSeconds: 60, Endpoints: map[string]watchdapi.EndpointState{
		endpoint.Keys(servers[0])[0]: {Status: watchdapi.StatusAlive, LatencyMS: 142},
		endpoint.Keys(servers[1])[0]: {Status: watchdapi.StatusDead, Error: "timeout"},
	}}

	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))

	var resp monitorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.State != watchdapi.StateOK || resp.IntervalSeconds != 60 || len(resp.Subscriptions) != 1 {
		t.Fatalf("response %+v", resp)
	}
	sub := resp.Subscriptions[0]
	if sub.ID != "0a1b2c3d" || sub.Alive != 1 || sub.Total != 2 || len(sub.Servers) != 2 {
		t.Fatalf("subscription %+v", sub)
	}
	if s := sub.Servers[0]; s.Index != 0 || s.Fingerprint != vpnconfig.ServerFingerprint(servers[0]) || s.Status != watchdapi.StatusAlive || s.LatencyMS != 142 {
		t.Fatalf("server 0 %+v", s)
	}
	if s := sub.Servers[1]; s.Status != watchdapi.StatusDead || s.Error != "timeout" {
		t.Fatalf("server 1 %+v", s)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("secret")) {
		t.Fatalf("the response carries a credential: %s", rec.Body)
	}
}

// Without the daemon the page still gets every server, as not checked.
func TestHandleMonitor_NoDaemonIsNotRunning(t *testing.T) {
	deps, _ := monitorDeps(t, &fakeMonitor{err: errors.New("dial unix: no such file")})

	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))

	var resp monitorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if resp.State != watchdapi.StateNotRunning || resp.Subscriptions[0].Servers[0].Status != watchdapi.StatusUnknown {
		t.Fatalf("response %+v", resp)
	}
}

func post(t *testing.T, deps *Deps, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handleMonitorCheck(deps).ServeHTTP(rec, httptest.NewRequest("POST", "/api/monitor/check", bytes.NewBufferString(body)))
	return rec
}

// SHA-256 of the independently written, IP-addressed Trojan outbounds.
const (
	monitorRigaFirstKey  = "923e3214354ec258610997ff601094591672414dfbcc9c107fd498649e0d3e22"
	monitorRigaSecondKey = "6452ed5028413a19d002a2ef9be523c3bd5f10fbeb5923de03b1546fc8642e24"
)

func TestHandleMonitorCheck_QueuesEveryAddressOfTheServer(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	servers[1].IPs = []string{"192.0.2.2", "198.51.100.2"}
	body := `{"subscription":"0a1b2c3d","index":1,"fingerprint":"` + vpnconfig.ServerFingerprint(servers[1]) + `"}`

	rec := post(t, deps, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var queued struct {
		Queued int `json:"queued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queued); err != nil || queued.Queued != 2 {
		t.Fatalf("queued: %s, error %v", rec.Body, err)
	}
	if len(mon.checked) != 1 || !reflect.DeepEqual(mon.checked[0], endpoint.Keys(servers[1])) {
		t.Fatalf("checked %v", mon.checked)
	}
	if want := []string{monitorRigaFirstKey, monitorRigaSecondKey}; !reflect.DeepEqual(mon.checked[0], want) {
		t.Fatalf("checked %v, want %v", mon.checked[0], want)
	}
	if rec := post(t, deps, `{}`); rec.Code != http.StatusOK || len(mon.checked[1]) != 0 {
		t.Fatalf("check all: %d, keys %v", rec.Code, mon.checked)
	}
}

// Review focus: the list changed between the page load and the tap - the
// fingerprint at that index is another server's - and nothing is checked.
func TestHandleMonitorCheck_AChangedListIs409(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	for _, body := range []string{
		`{"subscription":"0a1b2c3d","index":0,"fingerprint":"` + vpnconfig.ServerFingerprint(servers[1]) + `"}`,
		`{"subscription":"0a1b2c3d","index":5,"fingerprint":"x"}`,
		`{"subscription":"ffffffff","index":0,"fingerprint":"x"}`,
	} {
		if rec := post(t, deps, body); rec.Code != http.StatusConflict {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if len(mon.checked) != 0 {
		t.Fatalf("checked %v", mon.checked)
	}
}

func TestHandleMonitorCheck_AStoppedMonitorIs409AndNoDaemon503(t *testing.T) {
	deps, _ := monitorDeps(t, &fakeMonitor{checkErr: watchdapi.ErrNotActive})
	if rec := post(t, deps, `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("stopped: %d %s", rec.Code, rec.Body)
	}
	deps, _ = monitorDeps(t, &fakeMonitor{checkErr: errors.New("dial unix: no such file")})
	if rec := post(t, deps, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no daemon: %d %s", rec.Code, rec.Body)
	}
	deps, _ = monitorDeps(t, nil)
	if rec := post(t, deps, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client: %d %s", rec.Code, rec.Body)
	}
}

// The Servers tab matches its rows with the monitor's by fingerprint.
func TestHandleListServers_CarriesTheFingerprint(t *testing.T) {
	deps, servers := monitorDeps(t, nil)

	rec := httptest.NewRecorder()
	handleListServers(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/servers", nil))

	var resp struct {
		Subscriptions []struct {
			Servers []struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"servers"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Subscriptions[0].Servers[1].Fingerprint; got != vpnconfig.ServerFingerprint(servers[1]) {
		t.Fatalf("fingerprint %q", got)
	}
}

func TestHandleMonitor_FoldsAllAddressesAndPreservesMetadata(t *testing.T) {
	mon := &fakeMonitor{}
	deps, servers := monitorDeps(t, mon)
	servers[1].IPs = []string{"192.0.2.2", "198.51.100.2"}
	checked := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	since, next := checked.Add(-time.Hour), checked.Add(time.Minute)
	for _, tc := range []struct {
		name   string
		first  watchdapi.Status
		second watchdapi.Status
		want   watchdapi.Status
		alive  int
	}{
		{"second address alive", watchdapi.StatusDead, watchdapi.StatusAlive, watchdapi.StatusAlive, 1},
		{"best latency", watchdapi.StatusAlive, watchdapi.StatusAlive, watchdapi.StatusAlive, 1},
		{"one unchecked", watchdapi.StatusDead, watchdapi.StatusUnknown, watchdapi.StatusUnknown, 0},
		{"dead beats rejected", watchdapi.StatusRejected, watchdapi.StatusDead, watchdapi.StatusDead, 0},
		{"all rejected", watchdapi.StatusRejected, watchdapi.StatusRejected, watchdapi.StatusRejected, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mon.snap = watchdapi.Snapshot{State: watchdapi.StateProberError, Message: "the prober cannot start",
				IntervalSeconds: 60, LagSeconds: 7, Endpoints: map[string]watchdapi.EndpointState{
					monitorRigaFirstKey:  {Status: tc.first, LatencyMS: 250},
					monitorRigaSecondKey: {Status: tc.second, LatencyMS: 42, CheckedAt: checked, Since: since, NextAt: next},
				}}
			rec := httptest.NewRecorder()
			handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))
			var resp monitorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("%d %s, error %v", rec.Code, rec.Body, err)
			}
			if resp.State != watchdapi.StateProberError || resp.Message != mon.snap.Message || resp.LagSeconds != 7 || resp.IntervalSeconds != 60 {
				t.Fatalf("response %+v", resp)
			}
			sub := resp.Subscriptions[0]
			if sub.Total != 2 || sub.Alive != tc.alive || sub.Servers[1].Status != tc.want {
				t.Fatalf("subscription %+v", sub)
			}
			if tc.want == watchdapi.StatusAlive {
				s := sub.Servers[1]
				if s.LatencyMS != 42 || !s.CheckedAt.Equal(checked) || !s.Since.Equal(since) || !s.NextAt.Equal(next) {
					t.Fatalf("server %+v", s)
				}
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("secret")) || bytes.Contains(rec.Body.Bytes(), []byte("outbound")) {
				t.Fatalf("response carries an outbound: %s", rec.Body)
			}
		})
	}
}

func TestHandleMonitor_NilClientIsNotRunning(t *testing.T) {
	deps, _ := monitorDeps(t, nil)
	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))
	var resp monitorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s, error %v", rec.Code, rec.Body, err)
	}
	if resp.State != watchdapi.StateNotRunning || resp.Subscriptions[0].Alive != 0 {
		t.Fatalf("response %+v", resp)
	}
	for _, s := range resp.Subscriptions[0].Servers {
		if s.Status != watchdapi.StatusUnknown {
			t.Fatalf("server %+v", s)
		}
	}
}

func TestHandleMonitor_EmptyListsAreArrays(t *testing.T) {
	for _, subs := range [][]vpnconfig.Subscription{nil, {{ID: "0a1b2c3d"}}} {
		deps, _ := monitorDeps(t, nil)
		deps.Config = &mockConfig{subs: subs}
		rec := httptest.NewRecorder()
		handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))
		field := `"subscriptions":[]`
		if len(subs) != 0 {
			field = `"servers":[]`
		}
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(field)) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
}

func TestHandleMonitor_LoadFailuresAndMalformedChecksQueueNothing(t *testing.T) {
	mon := &fakeMonitor{}
	deps, _ := monitorDeps(t, mon)
	deps.Config.(*mockConfig).subsErr = errors.New("subscription unavailable")
	rec := httptest.NewRecorder()
	handleMonitor(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/monitor", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("load: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, deps, `{"subscription":"0a1b2c3d","index":0,"fingerprint":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("check load: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{`, `{"index":"zero"}`} {
		if rec := post(t, deps, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed: %d %s", rec.Code, rec.Body)
		}
	}
	if len(mon.checked) != 0 {
		t.Fatalf("checked %v", mon.checked)
	}
}

func TestMonitorRoutes_AreRegisteredAndAuthenticated(t *testing.T) {
	mon := &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}
	deps, _ := monitorDeps(t, mon)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := NewRouter(deps, nil, ctx)
	token := newTestToken(t, deps)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/monitor", ""},
		{"POST", "/api/monitor/check", `{}`},
	} {
		for _, authorized := range []bool{false, true} {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			if authorized {
				req.AddCookie(&http.Cookie{Name: "token", Value: token})
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			want := http.StatusUnauthorized
			if authorized {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("%s %s, authorized %t: %d %s", tc.method, tc.path, authorized, rec.Code, rec.Body)
			}
		}
	}
	if len(mon.checked) != 1 {
		t.Fatalf("checked %v", mon.checked)
	}
}

func TestHandleMonitorCheck_PartialTargetsNeverCheckAll(t *testing.T) {
	for _, body := range []string{
		`{"subscription":"0a1b2c3d","fingerprint":"x"}`, `{"subscription":"0a1b2c3d","index":null,"fingerprint":"x"}`,
		`{"subscription":"0a1b2c3d"}`, `{"fingerprint":"x"}`, `{"index":0}`, `{"index":null}`,
		`{"subscription":"","index":0,"fingerprint":"x"}`, `{"subscription":"0a1b2c3d","index":0}`, `{"subscription":null}`, `null`, `{"subscription":" ","index":0,"fingerprint":"x"}`, `{"subscription":"0a1b2c3d","index":0,"fingerprint":" "}`,
	} {
		t.Run(body, func(t *testing.T) {
			mon := &fakeMonitor{}
			deps, _ := monitorDeps(t, mon)
			rec := post(t, deps, body)
			if rec.Code != http.StatusBadRequest || len(mon.checked) != 0 {
				t.Fatalf("status=%d checked=%d; want 400 without Check", rec.Code, len(mon.checked))
			}
		})
	}
}
