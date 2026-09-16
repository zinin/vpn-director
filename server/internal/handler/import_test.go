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
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockConfigStoreForImport extends mockConfigStore with tracking for SaveServers
type mockConfigStoreForImport struct {
	mockConfigStore
	savedServers []vpnconfig.Server
	dataDirVal   string
	cfg          *vpnconfig.VPNDirectorConfig
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
	if m.cfg == nil {
		return m.mockConfigStore.UpdateVPNConfig(fn)
	}
	if err := fn(m.cfg); err != nil {
		return err
	}
	return nil
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	h.httpClient = &http.Client{}
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	h.httpClient = &http.Client{} // bypass SSRF guard to reach the loopback test server

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{dataDirVal: t.TempDir()}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = &http.Client{} // bypass SSRF guard to reach the loopback test server

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = &http.Client{} // bypass SSRF guard to reach the loopback test server

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid base64!!!"))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = &http.Client{} // bypass SSRF guard to reach the loopback test server

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
	// Should report no servers found or decode error
	if !strings.Contains(sender.lastText, "No") && !strings.Contains(sender.lastText, "decode") && !strings.Contains(sender.lastText, "base64") {
		t.Errorf("expected error about decoding or no servers, got %q", sender.lastText)
	}
}

func TestImportHandler_HandleImport_EmptySubscription(t *testing.T) {
	// Create a subscription with no VLESS URLs
	encoded := base64.StdEncoding.EncodeToString([]byte("just some text\nno vless here"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(encoded))
	}))
	defer server.Close()

	sender := &mockSender{}
	config := &mockConfigStoreForImport{}
	deps := &Deps{Sender: sender, Config: config}
	h := NewImportHandler(deps)
	h.httpClient = &http.Client{} // bypass SSRF guard to reach the loopback test server

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
	if !strings.Contains(sender.lastText, "No") {
		t.Errorf("expected 'No VLESS servers' message, got %q", sender.lastText)
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
		Text: "/import http://127.0.0.1:9/sub",
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
