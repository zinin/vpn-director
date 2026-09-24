package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// memConfigStore is a ConfigStore over memory. UpdateVPNConfig serializes on
// upd as the flock does, and mu guards the data: RefreshAll refreshes every
// subscription at once.
type memConfigStore struct {
	upd  sync.Mutex
	mu   sync.Mutex
	cfg  *vpnconfig.VPNDirectorConfig
	subs []vpnconfig.Subscription
}

func newMemConfigStore(subs ...vpnconfig.Subscription) *memConfigStore {
	return &memConfigStore{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
}

func (m *memConfigStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := *m.cfg
	return &cfg, nil
}
func (m *memConfigStore) LoadServers() ([]vpnconfig.Server, error) {
	subs, _ := m.LoadSubscriptions()
	return vpnconfig.AllServers(subs), nil
}
func (m *memConfigStore) SaveServers([]vpnconfig.Server) error { return errors.New("no servers.json") }
func (m *memConfigStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	cfg, _ := m.LoadVPNConfig()
	if err := fn(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return nil
}
func (m *memConfigStore) DataDir() (string, error) { return "/tmp/test-data", nil }
func (m *memConfigStore) DataDirOrDefault() string { return "/tmp/test-data" }
func (m *memConfigStore) ScriptsDir() string       { return "/tmp/test-scripts" }
func (m *memConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vpnconfig.Subscription, len(m.subs))
	copy(out, m.subs)
	return out, nil
}
func (m *memConfigStore) SaveSubscription(sub vpnconfig.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == sub.ID {
			m.subs[i] = sub
			return nil
		}
	}
	m.subs = append(m.subs, sub)
	return nil
}
func (m *memConfigStore) DeleteSubscription(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.subs {
		if m.subs[i].ID == id {
			m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
			return nil
		}
	}
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// subscriptionHost answers every request with the handler's answer, whatever
// host the URL names: the checks in front of the download see the public
// address a test uses.
func subscriptionHost(t *testing.T, h http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})}
}

func serve(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

var osloBody = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))

const publicLink = "https://93.184.216.34/s/token"

func TestAddSubscription_SavesTheListUnderItsHost(t *testing.T) {
	store := newMemConfigStore()

	res := AddSubscription(context.Background(), store, subscriptionHost(t, serve(osloBody)), publicLink, "")

	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Name != "93.184.216.34" || res.Existed || len(res.Import.Servers) != 1 || !vpnconfig.ValidSubscriptionID(res.ID) {
		t.Fatalf("result %+v", res)
	}
	if got := res.Line(); got != "93.184.216.34: Imported 1 servers" {
		t.Fatalf("line %q", got)
	}
	if len(store.subs) != 1 || store.subs[0].URL != publicLink {
		t.Fatalf("files %+v", store.subs)
	}
	if !reflect.DeepEqual(store.cfg.Xray.Servers, []string{"203.0.113.10"}) {
		t.Fatalf("xray.servers %v", store.cfg.Xray.Servers)
	}
}

func TestAddSubscription_RefusesALinkTheDaemonsDoNotDownload(t *testing.T) {
	store := newMemConfigStore()
	for _, raw := range []string{"http://sub.example.com/s", "https://127.0.0.1/s", "https://192.168.1.1/s", "ftp://sub.example.com/s", "no link", "https:///s"} {
		if res := AddSubscription(context.Background(), store, nil, raw, ""); !errors.Is(res.Err, ErrSubscriptionURL) {
			t.Errorf("%q: %v", raw, res.Err)
		}
	}
	if len(store.subs) != 0 {
		t.Fatalf("wrote %+v", store.subs)
	}
}

func TestAddSubscription_ATakenNameIsRefused(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"})

	res := AddSubscription(context.Background(), store, subscriptionHost(t, serve(osloBody)), publicLink, "alpha")

	if !errors.Is(res.Err, vpnconfig.ErrSubscriptionNameTaken) || len(store.subs) != 1 {
		t.Fatalf("err %v, files %d", res.Err, len(store.subs))
	}
}

// A body over the cap is refused, never cut: cut, a base64 list decodes to a
// shorter one, which would be published as the subscription.
func TestDownloadSubscription_ABodyOverTheCapDidNotArrive(t *testing.T) {
	line := "vless://uuid-1@203.0.113.10:443?type=tcp#Oslo\n"
	body := base64.StdEncoding.EncodeToString([]byte(strings.Repeat(line, MaxSubscriptionBody/len(line))))

	_, err := DownloadSubscription(context.Background(), subscriptionHost(t, serve(body)), publicLink)

	var de *DownloadError
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("err %v", err)
	}
}

func TestDownloadSubscription_AnHTTPErrorDidNotArrive(t *testing.T) {
	forbidden := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }

	_, err := DownloadSubscription(context.Background(), subscriptionHost(t, forbidden), publicLink)

	var de *DownloadError
	if !errors.As(err, &de) || err.Error() != "download failed: HTTP 403" {
		t.Fatalf("err %v", err)
	}
}

func TestDownloadSubscription_ABodyWithoutAServerSaysWhy(t *testing.T) {
	for body, want := range map[string]string{
		"tuic://u:p@203.0.113.9:443#TUIC":     "no supported servers",
		"vless://u@[2001:db8::1]:443#SixOnly": "could not resolve IP for any server",
	} {
		_, err := DownloadSubscription(context.Background(), subscriptionHost(t, serve(body)), publicLink)
		var be *BodyError
		if !errors.As(err, &be) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// cutShortText is what a download reads as when its context ended while the
// hosts resolved.
const cutShortText = "download failed: resolving the servers took longer than the deadline"

// cutShortClient serves osloBody and ends the context it returns with the
// body, so the resolution runs on a context that is over and every lookup
// fails at once. The one host is an IP literal, which resolves without a
// lookup: the list comes out whole, and must still not be taken.
func cutShortClient(t *testing.T) (context.Context, *http.Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	base := subscriptionHost(t, serve(osloBody)).Transport
	return ctx, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		if err == nil {
			resp.Body = cancelAtEOF{resp.Body, cancel}
		}
		return resp, err
	})}
}

// cancelAtEOF ends a context when the body it wraps ends.
type cancelAtEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelAtEOF) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.cancel()
	}
	return n, err
}

// A context that ends while the hosts resolve fails every lookup after it at
// once, and what resolved before it is not the subscription: it did not
// arrive in time.
func TestDownloadSubscription_AResolutionTheDeadlineCutShortDidNotArrive(t *testing.T) {
	ctx, client := cutShortClient(t)

	imp, err := DownloadSubscription(ctx, client, publicLink)

	var de *DownloadError
	if !errors.As(err, &de) || err.Error() != cutShortText || len(imp.Servers) != 0 {
		t.Fatalf("servers %d, err %v", len(imp.Servers), err)
	}
}

func TestAddSubscription_AResolutionCutShortSavesNothing(t *testing.T) {
	store := newMemConfigStore()
	ctx, client := cutShortClient(t)

	res := AddSubscription(ctx, store, client, publicLink, "")

	var de *DownloadError
	if !errors.As(res.Err, &de) || len(store.subs) != 0 || len(store.cfg.Xray.Servers) != 0 {
		t.Fatalf("err %v, files %d, xray.servers %v", res.Err, len(store.subs), store.cfg.Xray.Servers)
	}
}

func TestRefreshSubscription_AResolutionCutShortRecordsWhyAndKeepsTheList(t *testing.T) {
	old := []vpnconfig.Server{{Name: "Old", Address: "old.example.com", Port: 443, IPs: []string{"192.0.2.1"}}}
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: publicLink, Servers: old})
	ctx, client := cutShortClient(t)

	res := RefreshSubscription(ctx, store, client, "0a1b2c3d")

	var de *DownloadError
	if !errors.As(res.Err, &de) || res.Line() != "Alpha: "+cutShortText {
		t.Fatalf("err %v, line %q", res.Err, res.Line())
	}
	s := store.subs[0]
	if s.Error != cutShortText {
		t.Fatalf("recorded error %q", s.Error)
	}
	if len(s.Servers) != 1 || s.Servers[0].Name != "Old" {
		t.Fatalf("the list was replaced: %d servers", len(s.Servers))
	}
}

// A *url.Error's text is the whole URL, and the subscription token sits in its path.
func TestDownloadError_NamesNoLink(t *testing.T) {
	err := downloadError(&url.Error{Op: "Get", URL: "https://sub.example.com/s/secret-token", Err: errors.New("i/o timeout")})
	if strings.Contains(err.Error(), "secret-token") || err.Error() != "download failed: i/o timeout" {
		t.Fatalf("%q", err)
	}
	blocked := downloadError(&url.Error{Op: "Get", URL: "https://x/s/t", Err: fmt.Errorf("dial: %w: 10.0.0.1", ssrf.ErrBlockedAddress)})
	if strings.Contains(blocked.Error(), "10.0.0.1") {
		t.Fatalf("the blocked address reached the text: %q", blocked)
	}
}

func TestRefreshSubscription_AFailedDownloadRecordsWhyAndKeepsTheList(t *testing.T) {
	old := []vpnconfig.Server{{Name: "Old", Address: "old.example.com", Port: 443, IPs: []string{"192.0.2.1"}}}
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: publicLink, Servers: old})
	forbidden := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }

	res := RefreshSubscription(context.Background(), store, subscriptionHost(t, forbidden), "0a1b2c3d")

	if res.Err == nil || res.Line() != "Alpha: download failed: HTTP 403" {
		t.Fatalf("result %+v, line %q", res, res.Line())
	}
	if s := store.subs[0]; s.Error != "download failed: HTTP 403" || len(s.Servers) != 1 || s.Servers[0].Name != "Old" {
		t.Fatalf("file %+v", s)
	}
}

func TestRefreshSubscription_UnknownAndStatic(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	if res := RefreshSubscription(context.Background(), store, nil, "0a1b2c3d"); !errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) {
		t.Fatalf("unknown: %v", res.Err)
	}
	if res := RefreshSubscription(context.Background(), store, nil, "1b2c3d4e"); !errors.Is(res.Err, vpnconfig.ErrSubscriptionStatic) {
		t.Fatalf("static: %v", res.Err)
	}
}

func TestRefreshAllSubscriptions_OneResultPerLinkInOrder(t *testing.T) {
	store := newMemConfigStore(
		vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://93.184.216.34/a"},
		vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"},
		vpnconfig.Subscription{ID: "2c3d4e5f", Name: "Gamma", URL: "https://93.184.216.34/b"},
	)
	client := subscriptionHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(osloBody))
	})

	results, err := RefreshAllSubscriptions(context.Background(), store, client)

	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	want := []string{"Alpha: Imported 1 servers", "Gamma: download failed: HTTP 403"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

func TestDeleteSubscription_SaysTheRunningServerCameFromIt(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"})
	store.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}

	active, err := DeleteSubscription(store, "0a1b2c3d")

	if err != nil || !active || len(store.subs) != 0 {
		t.Fatalf("active %v, err %v, files %d", active, err, len(store.subs))
	}
}

func TestRenameSubscription_Refusals(t *testing.T) {
	store := newMemConfigStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha"}, vpnconfig.Subscription{ID: "1b2c3d4e", Name: "Beta"})
	if err := RenameSubscription(store, "0a1b2c3d", "BETA"); !errors.Is(err, vpnconfig.ErrSubscriptionNameTaken) {
		t.Fatalf("taken: %v", err)
	}
	if err := RenameSubscription(store, "0a1b2c3d", "Main"); err != nil || store.subs[0].Name != "Main" {
		t.Fatalf("err %v, files %+v", err, store.subs)
	}
}
