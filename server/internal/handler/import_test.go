// internal/handler/import_test.go
package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockConfigStoreForImport extends mockConfigStore with tracking for SaveServers
type mockConfigStoreForImport struct {
	mockConfigStore
	savedServers []vpnconfig.Server
	dataDirVal   string
	cfg          *vpnconfig.VPNDirectorConfig
	updateErr    error // returned by UpdateVPNConfig before fn runs, as a lock or load failure is
	saveErr      error // returned by UpdateVPNConfig after fn ran, as a failed write of the config is
}

func (m *mockConfigStoreForImport) SaveServers(servers []vpnconfig.Server) error {
	m.savedServers = servers
	return m.mockConfigStore.err
}

func (m *mockConfigStoreForImport) DataDirOrDefault() string {
	if m.dataDirVal != "" {
		return m.dataDirVal
	}
	return "/data"
}

func (m *mockConfigStoreForImport) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	if m.cfg != nil {
		return m.cfg, nil
	}
	return m.mockConfigStore.LoadVPNConfig()
}

func (m *mockConfigStoreForImport) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	if m.cfg == nil {
		return m.mockConfigStore.UpdateVPNConfig(fn)
	}
	if err := fn(m.cfg); err != nil {
		return err
	}
	return m.saveErr
}

// allTextSender keeps every message, for a handler that sends more than one.
type allTextSender struct {
	mockSender
	texts []string
}

func (m *allTextSender) Send(chatID int64, text string) error {
	m.texts = append(m.texts, text)
	return m.mockSender.Send(chatID, text)
}

func importCommand(text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 123},
		Text:     text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 7}},
	}
}

// subscriptionServer answers every request with body.
func subscriptionServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

var osloSubscription = base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))

// A publication that never got the config lock wrote nothing, servers.json
// included. Reporting it as "servers imported" left the user believing the new
// list was in place.
func TestImportHandler_HandleImport_ALockTimeoutImportsNothing(t *testing.T) {
	server := subscriptionServer(t, osloSubscription)
	sender := &mockSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		cfg:        &vpnconfig.VPNDirectorConfig{},
		updateErr:  service.ErrConfigLockTimeout,
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	if config.savedServers != nil {
		t.Fatalf("saved %v without the config lock", config.savedServers)
	}
	if strings.Contains(sender.lastText, "Imported") || !strings.Contains(sender.lastText, "nothing was imported") {
		t.Fatalf("message %q; the user must learn the list was not saved", sender.lastText)
	}
}

// A re-import of the saved link publishes only against a config it can read: an
// unreadable one cannot say the link is still the saved one, and nothing is
// written. It was reported as imported all the same.
func TestImportHandler_HandleImport_AReImportThatCannotReadTheConfigImportsNothing(t *testing.T) {
	server := subscriptionServer(t, osloSubscription)
	sender := &mockSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		cfg:        &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{SubscriptionURL: server.URL}},
		updateErr:  fmt.Errorf("%w: %w", service.ErrConfigLoad, errors.New("invalid character 'x' looking for beginning of value")),
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import"))

	if config.savedServers != nil {
		t.Fatalf("saved %v", config.savedServers)
	}
	if strings.Contains(sender.lastText, "Imported") || !strings.Contains(sender.lastText, "nothing was imported") {
		t.Fatalf("message %q; the user must learn the list was not saved", sender.lastText)
	}
}

// servers.json is written and only the config beside it is not: the list is
// imported, and the user is told what is missing.
func TestImportHandler_HandleImport_AConfigWriteThatFailsAfterTheListWarns(t *testing.T) {
	server := subscriptionServer(t, osloSubscription)
	sender := &allTextSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		cfg:        &vpnconfig.VPNDirectorConfig{},
		saveErr:    errors.New("save config: no space left on device"),
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	if len(config.savedServers) != 1 {
		t.Fatalf("saved %v", config.savedServers)
	}
	warned := false
	for _, text := range sender.texts {
		if strings.Contains(text, "sync failed") {
			warned = true
		}
	}
	if !warned || !strings.Contains(sender.lastText, "Imported") {
		t.Fatalf("messages %q; want the import and a warning about the config", sender.texts)
	}
}

// Before the first configure there is no vpn-director.json to keep in step:
// the list is saved and reported as imported, with nothing to warn about.
func TestImportHandler_HandleImport_BeforeTheFirstConfigureSavesTheList(t *testing.T) {
	server := subscriptionServer(t, osloSubscription)
	sender := &allTextSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	if len(config.savedServers) != 1 {
		t.Fatalf("saved %v; the list must survive a missing config", config.savedServers)
	}
	for _, text := range sender.texts {
		if strings.Contains(text, "sync failed") || strings.Contains(text, "nothing was imported") {
			t.Fatalf("messages %q; a router before configure has nothing to warn about", sender.texts)
		}
	}
	if !strings.Contains(sender.lastText, "Imported") {
		t.Fatalf("message %q", sender.lastText)
	}
}

// A vpn-director.json that is there but does not load is not the router before
// its first configure. It was taken for one: the list was written, the link and
// the bypass list were not, and the answer was "Imported" without a warning.
func TestImportHandler_HandleImport_AConfigThatDoesNotLoadImportsNothing(t *testing.T) {
	server := subscriptionServer(t, osloSubscription)
	sender := &allTextSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		updateErr:  fmt.Errorf("%w: %w", service.ErrConfigLoad, errors.New("invalid character '}' looking for beginning of object key string")),
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	if config.savedServers != nil {
		t.Fatalf("saved %v beside a config that does not load", config.savedServers)
	}
	if strings.Contains(sender.lastText, "Imported") || !strings.Contains(sender.lastText, "nothing was imported") {
		t.Fatalf("messages %q; the user must learn the list was not saved", sender.texts)
	}
}

// A subscription over the 1 MiB cap was cut at the cap and decoded anyway:
// base64 cut there decodes to a shorter list, which the import then published
// as the subscription. The watch refuses such a body, and so does /import.
func TestImportHandler_HandleImport_RefusesASubscriptionOverTheCap(t *testing.T) {
	line := "vless://uuid-1@203.0.113.10:443?type=tcp#Oslo\n"
	// Base64 makes a MiB of lines a third longer again.
	server := subscriptionServer(t, base64.StdEncoding.EncodeToString([]byte(strings.Repeat(line, (1<<20)/len(line)))))
	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir(), cfg: &vpnconfig.VPNDirectorConfig{}}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	if config.savedServers != nil {
		t.Fatalf("saved %d servers out of a list cut at the cap", len(config.savedServers))
	}
	if !strings.Contains(sender.lastText, "exceeds 1 MiB") {
		t.Fatalf("message %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_NoURL(t *testing.T) {
	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		Text: "/import",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 123 {
		t.Errorf("expected chatID 123, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "Usage") {
		t.Errorf("expected usage message, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_NoArgsUsesSavedURL(t *testing.T) {
	vlessURL := "vless://test-uuid-1234@example.com:443?type=tcp#TestServer"
	encoded := base64.StdEncoding.EncodeToString([]byte(vlessURL))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	config.cfg = &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{SubscriptionURL: server.URL},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client()
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		Text: "/import",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)
	if len(config.savedServers) == 0 {
		t.Fatalf("expected import from saved URL, last message %q", sender.lastText)
	}
	if config.cfg.Xray.SubscriptionURL != server.URL {
		t.Fatalf("SubscriptionURL %q, want the saved %q kept", config.cfg.Xray.SubscriptionURL, server.URL)
	}
}

// A /import without a link re-downloads the saved one. If another importer saves
// a different subscription while that download runs, publishing would leave the
// old list beside the new link, and every later refresh would fetch the new link
// against a list it did not produce.
func TestImportHandler_HandleImport_SavedLinkChangedDuringDownload(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443?type=tcp#Oslo"))
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Another import saves a different subscription while this one downloads.
		config.cfg.Xray.SubscriptionURL = "https://cdn.example/s/other"
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config.cfg = &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{SubscriptionURL: server.URL}}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()
	msg := &tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 123},
		Text:     "/import",
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 7}},
	}

	h.HandleImport(msg)

	if config.savedServers != nil || len(config.cfg.Xray.Servers) != 0 {
		t.Fatalf("published a list the saved link no longer produces: saved %v, xray.servers %v; last message %q",
			config.savedServers, config.cfg.Xray.Servers, sender.lastText)
	}
	if !strings.Contains(sender.lastText, "changed while downloading") {
		t.Fatalf("message %q; the user must learn why nothing was imported", sender.lastText)
	}
}

// failingTransport fails every request the way an unreachable host does.
type failingTransport struct{ err error }

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func TestImportHandler_HandleImport_DownloadErrorHidesSavedURL(t *testing.T) {
	sender := &mockSender{}
	config := &mockConfigStoreForImport{
		cfg: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://cdn.example/s/SECRET-TOKEN"},
		},
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = &http.Client{Transport: failingTransport{err: errors.New("connection refused")}}
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		Text: "/import",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if !strings.Contains(sender.lastText, "Download error") {
		t.Fatalf("expected a download error, got %q", sender.lastText)
	}
	// The text is MarkdownV2-escaped, so a leaked token reads SECRET\-TOKEN.
	if strings.Contains(sender.lastText, "SECRET") {
		t.Fatalf("download error leaked the saved URL: %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_NoArgsConfigLoadError(t *testing.T) {
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 123},
		Text: "/import",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}

	sender := &mockSender{}
	config := &mockConfigStoreForImport{mockConfigStore: mockConfigStore{
		err: errors.New("invalid character 'x' looking for beginning of value"),
	}}
	NewImportHandler(&Deps{Sender: sender, Config: config}).HandleImport(msg)
	if !strings.Contains(sender.lastText, "Config load error") {
		t.Fatalf("expected the load error, got %q", sender.lastText)
	}

	// A missing vpn-director.json is not an error: /import works before configure.
	sender = &mockSender{}
	config = &mockConfigStoreForImport{mockConfigStore: mockConfigStore{
		err: fmt.Errorf("open vpn-director.json: %w", fs.ErrNotExist),
	}}
	NewImportHandler(&Deps{Sender: sender, Config: config}).HandleImport(msg)
	if !strings.Contains(sender.lastText, "Usage") {
		t.Fatalf("expected usage message for a missing config, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_SavesPostedURL(t *testing.T) {
	vlessURL := "vless://test-uuid-1234@example.com:443?type=tcp#TestServer"
	encoded := base64.StdEncoding.EncodeToString([]byte(vlessURL))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		cfg:        &vpnconfig.VPNDirectorConfig{},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client() // bypass SSRF guard to reach the loopback test server

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 789},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if config.cfg.Xray.SubscriptionURL != server.URL {
		t.Fatalf("expected SubscriptionURL %q, got %q, last message %q",
			server.URL, config.cfg.Xray.SubscriptionURL, sender.lastText)
	}
}

func TestImportHandler_HandleImport_InvalidScheme(t *testing.T) {
	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 456},
		Text: "/import ftp://example.com/servers",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 456 {
		t.Errorf("expected chatID 456, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "http") && !strings.Contains(sender.lastText, "https") {
		t.Errorf("expected error about http/https, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_RejectsHTTPURL(t *testing.T) {
	vlessURL := "vless://test-uuid-1234@example.com:443?type=tcp#TestServer"
	encoded := base64.StdEncoding.EncodeToString([]byte(vlessURL))
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{
		dataDirVal: t.TempDir(),
		cfg:        &vpnconfig.VPNDirectorConfig{},
	}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = &http.Client{} // bypass SSRF guard: only the scheme check may stop this import

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 789},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if !strings.Contains(sender.lastText, "https") {
		t.Errorf("expected a reply asking for https, got %q", sender.lastText)
	}
	if hits != 0 {
		t.Errorf("http URL downloaded %d times", hits)
	}
	if len(config.savedServers) != 0 {
		t.Errorf("expected nothing saved, got %v", config.savedServers)
	}
	if config.cfg.Xray.SubscriptionURL != "" {
		t.Errorf("SubscriptionURL %q written for a rejected URL", config.cfg.Xray.SubscriptionURL)
	}
}

func TestImportHandler_TimeoutConfiguration(t *testing.T) {
	h := NewImportHandler(&Deps{})

	if h.httpClient.Timeout != 30*time.Second {
		t.Errorf("expected 30s timeout, got %v", h.httpClient.Timeout)
	}
}

func TestImportHandler_MaxBodySize(t *testing.T) {
	h := NewImportHandler(&Deps{})

	expectedMaxSize := int64(1 << 20) // 1MB
	if h.maxBodySize != expectedMaxSize {
		t.Errorf("expected maxBodySize %d, got %d", expectedMaxSize, h.maxBodySize)
	}
}

func TestImportHandler_HandleImport_ValidSubscription(t *testing.T) {
	// Create a valid VLESS subscription (base64 encoded)
	vlessURL := "vless://test-uuid-1234@example.com:443?type=tcp#TestServer"
	encoded := base64.StdEncoding.EncodeToString([]byte(vlessURL))

	// Create mock HTTP server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client() // bypass SSRF guard to reach the loopback test server

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 789},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	// The handler should have sent at least one message
	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}

	// We expect either success or DNS resolution error (example.com won't resolve in test)
	// Either way, it should have tried to process the subscription
}

func TestImportHandler_HandleImport_HTTPError(t *testing.T) {
	// Create mock HTTP server that returns error
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client() // bypass SSRF guard to reach the loopback test server

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 111},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 111 {
		t.Errorf("expected chatID 111, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "404") && !strings.Contains(sender.lastText, "HTTP") {
		t.Errorf("expected HTTP error message, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_InvalidBase64(t *testing.T) {
	// Create mock HTTP server that returns invalid base64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid base64!!!"))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client() // bypass SSRF guard to reach the loopback test server

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 222},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 222 {
		t.Errorf("expected chatID 222, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "unrecognized subscription format") {
		t.Errorf("expected the unrecognized format error, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_EmptySubscription(t *testing.T) {
	// A base64 body with no link in it
	encoded := base64.StdEncoding.EncodeToString([]byte("just some text\nno vless here"))

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = server.Client() // bypass SSRF guard to reach the loopback test server

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 333},
		Text: "/import " + server.URL,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 333 {
		t.Errorf("expected chatID 333, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "unrecognized subscription format") {
		t.Errorf("expected the unrecognized format error, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_BlocksPrivateURL(t *testing.T) {
	// Uses the real SSRF-guarded client from NewImportHandler (no injection):
	// the dial-time guard must refuse to connect to a loopback address.
	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 999},
		Text: "/import https://127.0.0.1:9/sub",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 7},
		},
	}
	h.HandleImport(msg)

	if sender.lastChatID != 999 {
		t.Errorf("expected chatID 999, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "Download error") {
		t.Errorf("expected download error for blocked private address, got %q", sender.lastText)
	}
	if config.savedServers != nil {
		t.Error("expected no servers saved for a blocked private URL")
	}
}

// An import says what it left out, and why: the counts under the country
// list, then the entries a user can act on.
func TestImportHandler_HandleImport_ReportsWhatWasSkipped(t *testing.T) {
	server := subscriptionServer(t, strings.Join([]string{
		"vless://uuid-1@203.0.113.10:443?type=tcp#Oslo",
		"tuic://uuid:pw@203.0.113.11:443#TUIC",
		"vless://uuid-2@203.0.113.12:443?type=kcp#KCP",
	}, "\n"))
	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	for _, want := range []string{"Imported 1 of 3 servers:", "2 unsupported", "TUIC: tuic", "KCP: transport kcp"} {
		if !strings.Contains(sender.lastText, want) {
			t.Errorf("message %q lacks %q", sender.lastText, want)
		}
	}
	if len(config.savedServers) != 1 || config.savedServers[0].Name != "Oslo" {
		t.Fatalf("saved %+v", config.savedServers)
	}
}

func TestImportHandler_HandleImport_NoSupportedServers(t *testing.T) {
	server := subscriptionServer(t, "tuic://uuid:pw@203.0.113.11:443#TUIC")
	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	h := NewImportHandler(&Deps{Sender: sender, Config: config})
	h.httpClient = server.Client()

	h.HandleImport(importCommand("/import " + server.URL))

	for _, want := range []string{"No supported servers in subscription", "1 unsupported", "TUIC: tuic"} {
		if !strings.Contains(sender.lastText, want) {
			t.Errorf("message %q lacks %q", sender.lastText, want)
		}
	}
	if config.savedServers != nil {
		t.Fatalf("saved %+v", config.savedServers)
	}
}
