package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// subsStore is a ConfigStore over memory for the handlers that read and write
// subscriptions. UpdateVPNConfig serializes on upd as the flock does, and mu
// guards the data: /import refreshes every subscription at once.
type subsStore struct {
	upd  sync.Mutex
	mu   sync.Mutex
	cfg  *vpnconfig.VPNDirectorConfig
	subs []vpnconfig.Subscription
}

func newSubsStore(subs ...vpnconfig.Subscription) *subsStore {
	return &subsStore{cfg: &vpnconfig.VPNDirectorConfig{}, subs: subs}
}

func (m *subsStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg, nil
}
func (m *subsStore) LoadServers() ([]vpnconfig.Server, error) {
	subs, _ := m.LoadSubscriptions()
	return vpnconfig.AllServers(subs), nil
}
func (m *subsStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	m.upd.Lock()
	defer m.upd.Unlock()
	return fn(m.cfg)
}
func (m *subsStore) DataDir() (string, error) { return "/data", nil }
func (m *subsStore) ScriptsDir() string       { return "/scripts" }
func (m *subsStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vpnconfig.Subscription, len(m.subs))
	for i, s := range m.subs {
		servers := make([]vpnconfig.Server, len(s.Servers))
		for j, srv := range s.Servers {
			srv.Subscription = s.ID
			servers[j] = srv
		}
		s.Servers = servers
		out[i] = s
	}
	return out, nil
}
func (m *subsStore) SaveSubscription(sub vpnconfig.Subscription) error {
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
func (m *subsStore) DeleteSubscription(id string) error {
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

// configSaveFails is a subsStore whose config save fails after fn ran: the
// subscription files are written, vpn-director.json beside them is not.
type configSaveFails struct{ *subsStore }

func (m configSaveFails) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if err := m.subsStore.UpdateVPNConfig(fn); err != nil {
		return err
	}
	return errors.New("disk full")
}

// recordingSender keeps every text it is given, sent or edited, and the last keyboard.
type recordingSender struct {
	mu       sync.Mutex
	texts    []string
	keyboard tgbotapi.InlineKeyboardMarkup
	edits    int
}

func (s *recordingSender) add(text string, kb *tgbotapi.InlineKeyboardMarkup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	if kb != nil {
		s.keyboard = *kb
	}
}
func (s *recordingSender) Send(_ int64, text string) error      { s.add(text, nil); return nil }
func (s *recordingSender) SendPlain(_ int64, text string) error { s.add(text, nil); return nil }
func (s *recordingSender) SendLongPlain(_ int64, text string) error {
	s.add(text, nil)
	return nil
}
func (s *recordingSender) SendWithKeyboard(_ int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	s.add(text, &kb)
	return nil
}
func (s *recordingSender) SendCodeBlock(_ int64, header, content string) error {
	s.add(header+"\n"+content, nil)
	return nil
}
func (s *recordingSender) EditMessage(_ int64, _ int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	s.add(text, &kb)
	s.mu.Lock()
	s.edits++
	s.mu.Unlock()
	return nil
}
func (s *recordingSender) AckCallback(string) error { return nil }

func (s *recordingSender) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

func (s *recordingSender) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.texts, "\n")
}

// buttons is the callback data of every button of the last keyboard, in order.
func (s *recordingSender) buttons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, row := range s.keyboard.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != nil {
				out = append(out, *b.CallbackData)
			}
		}
	}
	return out
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// servingClient answers every request with h, whatever host the URL names: the
// checks in front of a download see the public address a test uses.
func servingClient(t *testing.T, h http.HandlerFunc) *http.Client {
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

func serveBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

var osloSubscription = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))
