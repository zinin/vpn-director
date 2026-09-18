// internal/handler/xray_test.go
package handler

import (
	"errors"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockXrayGenerator for testing
type mockXrayGenerator struct {
	lastServer vpnconfig.Server
	err        error
}

func (m *mockXrayGenerator) GenerateConfig(server vpnconfig.Server, _ ...service.InboundPorts) error {
	m.lastServer = server
	return m.err
}

func TestXrayHandler_HandleXray_WithServers(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{
		{Name: "Germany, Berlin", Address: "de.example.com", IPs: []string{"1.1.1.1"}},
		{Name: "USA, New York", Address: "us.example.com", IPs: []string{"2.2.2.2"}},
	}
	config := &mockConfigStore{servers: servers}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}}
	h.HandleXray(msg)

	if sender.lastChatID != 123 {
		t.Errorf("expected chatID 123, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "сервер") {
		t.Errorf("expected 'сервер' in text, got %q", sender.lastText)
	}
	// Should have 1 row with 2 buttons (2 servers, 2 columns)
	if len(sender.lastKeyboard.InlineKeyboard) < 1 {
		t.Error("expected keyboard to have rows")
	}
	// First row should have server buttons
	if len(sender.lastKeyboard.InlineKeyboard[0]) == 0 {
		t.Error("expected buttons in first row")
	}
}

func TestXrayHandler_HandleXray_Empty(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{servers: []vpnconfig.Server{}}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}}
	h.HandleXray(msg)

	if !strings.Contains(sender.lastText, "не найден") || !strings.Contains(sender.lastText, "/import") {
		t.Errorf("expected 'не найдены' and '/import' in text, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleXray_LoadError(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{err: errors.New("config load failed")}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleXray(msg)

	if !strings.Contains(sender.lastText, "config load failed") {
		t.Errorf("expected error message, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_Success(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{
		{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1", IPs: []string{"1.1.1.1"}},
		{Name: "USA, New York", Address: "us.example.com", Port: 443, UUID: "uuid2", IPs: []string{"2.2.2.2"}},
	}
	// A configured router: the generation runs inside UpdateVPNConfig, so a
	// store with no vpn-director.json would never reach it.
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: servers},
		cfg:             &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{}
	vpn := &mockVPNDirectorWithXray{}

	deps := &Deps{Sender: sender, Config: config, Xray: xray, VPN: vpn}
	h := NewXrayHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "cb123",
		Data: "xray:select:1",
		Message: &tgbotapi.Message{
			MessageID: 42,
			Chat:      &tgbotapi.Chat{ID: 100},
		},
	}
	h.HandleCallback(cb)

	// Should generate config for server index 1 (USA)
	if xray.lastServer.Name != "USA, New York" {
		t.Errorf("expected server 'USA, New York', got %q", xray.lastServer.Name)
	}
	// Should edit the original message (not send new one)
	if sender.lastMsgID != 42 {
		t.Errorf("expected message ID 42 to be edited, got %d", sender.lastMsgID)
	}
	// Should show success message
	if !strings.Contains(sender.lastText, "Переключено") || !strings.Contains(sender.lastText, "USA") {
		t.Errorf("expected success message with server name, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_InvalidIndex(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{
		{Name: "Germany, Berlin"},
	}
	config := &mockConfigStore{servers: servers}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "cb456",
		Data: "xray:select:99", // invalid index
		Message: &tgbotapi.Message{
			MessageID: 10,
			Chat:      &tgbotapi.Chat{ID: 200},
		},
	}
	h.HandleCallback(cb)

	if !strings.Contains(sender.lastText, "Ошибка") {
		t.Errorf("expected error message, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_GenerateError(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{{Name: "Server1"}}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: servers},
		cfg:             &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{err: errors.New("template not found")}
	// No VPN on purpose: a generation that failed must not reach RestartXray,
	// and a nil dependency is the loudest way to prove it does not.
	deps := &Deps{Sender: sender, Config: config, Xray: xray}
	h := NewXrayHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "cb789",
		Data: "xray:select:0",
		Message: &tgbotapi.Message{
			MessageID: 5,
			Chat:      &tgbotapi.Chat{ID: 300},
		},
	}
	h.HandleCallback(cb)

	if !strings.Contains(sender.lastText, "template not found") {
		t.Errorf("expected error message, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_RestartError(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{{Name: "Server1"}}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: servers},
		cfg:             &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{}
	vpn := &mockVPNDirectorWithXray{restartXrayErr: errors.New("xray not running")}

	deps := &Deps{Sender: sender, Config: config, Xray: xray, VPN: vpn}
	h := NewXrayHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "cb_restart",
		Data: "xray:select:0",
		Message: &tgbotapi.Message{
			MessageID: 7,
			Chat:      &tgbotapi.Chat{ID: 400},
		},
	}
	h.HandleCallback(cb)

	if !strings.Contains(sender.lastText, "xray not running") {
		t.Errorf("expected restart error message, got %q", sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_NilMessage(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	// Inline callbacks may have nil Message
	cb := &tgbotapi.CallbackQuery{
		ID:      "cb_nil",
		Data:    "xray:select:0",
		Message: nil,
	}

	// Should not panic
	h.HandleCallback(cb)

	// No message should be sent (because we returned early)
	if sender.lastChatID != 0 {
		t.Error("expected no message to be sent for nil Message")
	}
}

func TestXrayHandler_HandleCallback_InvalidData(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{}

	deps := &Deps{Sender: sender, Config: config}
	h := NewXrayHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "cb_invalid",
		Data: "xray:select:abc", // non-numeric index
		Message: &tgbotapi.Message{
			MessageID: 1,
			Chat:      &tgbotapi.Chat{ID: 500},
		},
	}
	h.HandleCallback(cb)

	if !strings.Contains(sender.lastText, "неверный индекс") {
		t.Errorf("expected 'неверный индекс' error, got %q", sender.lastText)
	}
}

func TestXrayHandler_FullFlow(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{
		{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1", IPs: []string{"1.1.1.1"}},
		{Name: "USA, New York", Address: "us.example.com", Port: 443, UUID: "uuid2", IPs: []string{"2.2.2.2"}},
		{Name: "Japan, Tokyo", Address: "jp.example.com", Port: 443, UUID: "uuid3", IPs: []string{"3.3.3.3"}},
	}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: servers},
		cfg:             &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{}
	vpn := &mockVPNDirectorWithXray{}

	deps := &Deps{Sender: sender, Config: config, Xray: xray, VPN: vpn}
	h := NewXrayHandler(deps)

	// Step 1: User sends /xray
	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}}
	h.HandleXray(msg)

	// Should show keyboard with 3 servers in 2 columns (2 rows)
	if len(sender.lastKeyboard.InlineKeyboard) != 2 { // 2 rows (2+1)
		t.Errorf("expected 2 keyboard rows, got %d", len(sender.lastKeyboard.InlineKeyboard))
	}
	// First row should have 2 buttons
	if len(sender.lastKeyboard.InlineKeyboard[0]) != 2 {
		t.Errorf("expected 2 buttons in first row, got %d", len(sender.lastKeyboard.InlineKeyboard[0]))
	}
	// Verify callback data format: the index, and the first 8 hex digits of
	// sha256("Germany, Berlin|de.example.com|443")
	btn := sender.lastKeyboard.InlineKeyboard[0][0]
	if btn.CallbackData == nil || *btn.CallbackData != "xray:select:0:f3dbd26d" {
		got := "<nil>"
		if btn.CallbackData != nil {
			got = *btn.CallbackData
		}
		t.Errorf("expected callback data 'xray:select:0:f3dbd26d', got %q", got)
	}

	// Step 2: User clicks on USA server (index 1)
	cb := &tgbotapi.CallbackQuery{
		ID:   "cb",
		Data: "xray:select:1",
		Message: &tgbotapi.Message{
			MessageID: 42,
			Chat:      &tgbotapi.Chat{ID: 100},
		},
	}
	h.HandleCallback(cb)

	// Verify correct server was used
	if xray.lastServer.Address != "us.example.com" {
		t.Errorf("expected address 'us.example.com', got %q", xray.lastServer.Address)
	}
	// Verify original message was edited (keyboard removed)
	if sender.lastMsgID != 42 {
		t.Errorf("expected message ID 42 to be edited, got %d", sender.lastMsgID)
	}
	// Verify success message
	if !strings.Contains(sender.lastText, "USA, New York") {
		t.Errorf("expected success message with 'USA, New York', got %q", sender.lastText)
	}
}

// mockVPNDirectorWithXray extends mockVPNDirector with restartXrayErr support
type mockVPNDirectorWithXray struct {
	statusOutput   string
	statusErr      error
	restartErr     error
	restartXrayErr error
	stopErr        error
}

func (m *mockVPNDirectorWithXray) Status() (string, error) { return m.statusOutput, m.statusErr }
func (m *mockVPNDirectorWithXray) Apply() error            { return nil }
func (m *mockVPNDirectorWithXray) Restart() error          { return m.restartErr }
func (m *mockVPNDirectorWithXray) RestartXray() error      { return m.restartXrayErr }
func (m *mockVPNDirectorWithXray) Stop() error             { return m.stopErr }
func (m *mockVPNDirectorWithXray) Update() error           { return nil }
func (m *mockVPNDirectorWithXray) Platform() (vpnconfig.PlatformInfo, error) {
	return vpnconfig.PlatformInfo{}, nil
}

// trackingXrayConfigStore holds a config the way a configured router does, so
// a test can read back what the switch wrote. The plain mockConfigStore stands
// in for a router before its first configure and fails every update.
type trackingXrayConfigStore struct {
	mockConfigStore
	cfg *vpnconfig.VPNDirectorConfig
}

func (m *trackingXrayConfigStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	return m.cfg, m.err
}

func (m *trackingXrayConfigStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	return fn(m.cfg)
}

// The bot switches servers without going through the Web UI, so it owes the
// same record: otherwise the Status tab keeps naming whatever was chosen last
// time a browser did it.
func TestXrayHandler_HandleCallback_RecordsTheSelection(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: []vpnconfig.Server{
			{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1", IPs: []string{"1.1.1.1"}},
			{Name: "USA, New York", Address: "us.example.com", Port: 8443, UUID: "uuid2", IPs: []string{"2.2.2.2"}},
		}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := &Deps{Sender: sender, Config: config, Xray: &mockXrayGenerator{}, VPN: &mockVPNDirectorWithXray{}}
	h := NewXrayHandler(deps)

	h.HandleCallback(&tgbotapi.CallbackQuery{
		ID:      "cb123",
		Data:    "xray:select:1",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	active := config.cfg.Xray.ActiveServer
	if active == nil {
		t.Fatal("the bot switched servers without recording which one")
	}
	if active.Name != "USA, New York" || active.Address != "us.example.com" || active.Port != 8443 {
		t.Errorf("recorded %+v, want the server at index 1", *active)
	}
}

// A generation failure leaves the previous config.json in place, so the record
// has to stay on the server that is still running.
func TestXrayHandler_HandleCallback_RecordsNothingWhenGenerationFails(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: []vpnconfig.Server{
			{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1"},
		}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	deps := &Deps{
		Sender: sender,
		Config: config,
		Xray:   &mockXrayGenerator{err: errors.New("reality requires a public key")},
		VPN:    &mockVPNDirectorWithXray{},
	}
	h := NewXrayHandler(deps)

	h.HandleCallback(&tgbotapi.CallbackQuery{
		ID:      "cb124",
		Data:    "xray:select:0",
		Message: &tgbotapi.Message{MessageID: 43, Chat: &tgbotapi.Chat{ID: 101}},
	})

	if active := config.cfg.Xray.ActiveServer; active != nil {
		t.Errorf("recorded %+v after the config failed to generate", *active)
	}
}

// A button names its server as well as its place in the list. The list can
// change between /xray and the tap - the subscription watch rotates endpoints,
// an import replaces it - and the index then points at another server.
func TestXrayHandler_HandleCallback_SwitchesToTheServerItsButtonNamed(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: []vpnconfig.Server{
			{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1"},
			{Name: "USA, New York", Address: "us.example.com", Port: 443, UUID: "uuid2"},
		}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{}
	h := NewXrayHandler(&Deps{Sender: sender, Config: config, Xray: xray, VPN: &mockVPNDirectorWithXray{}})

	// b41c75e3: sha256("USA, New York|us.example.com|443")
	h.HandleCallback(&tgbotapi.CallbackQuery{
		ID:      "cb",
		Data:    "xray:select:1:b41c75e3",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if xray.lastServer.Name != "USA, New York" {
		t.Fatalf("generated %q, want the server the button named; last message %q", xray.lastServer.Name, sender.lastText)
	}
}

func TestXrayHandler_HandleCallback_RefusesAButtonTheListNoLongerMatches(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &trackingXrayConfigStore{
		mockConfigStore: mockConfigStore{servers: []vpnconfig.Server{
			{Name: "Germany, Berlin", Address: "de.example.com", Port: 443, UUID: "uuid1"},
			{Name: "Japan, Tokyo", Address: "jp.example.com", Port: 443, UUID: "uuid3"},
		}},
		cfg: &vpnconfig.VPNDirectorConfig{},
	}
	xray := &mockXrayGenerator{}
	// No VPN on purpose: a refused switch must not reach RestartXray.
	h := NewXrayHandler(&Deps{Sender: sender, Config: config, Xray: xray})

	// The keyboard was drawn when index 1 was USA, New York.
	h.HandleCallback(&tgbotapi.CallbackQuery{
		ID:      "cb",
		Data:    "xray:select:1:b41c75e3",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if xray.lastServer.Name != "" {
		t.Fatalf("generated %q; the button named a server the list no longer has there", xray.lastServer.Name)
	}
	if config.cfg.Xray.ActiveServer != nil {
		t.Fatalf("recorded %+v", *config.cfg.Xray.ActiveServer)
	}
	if !strings.Contains(sender.lastText, "changed") {
		t.Fatalf("message %q; the user must learn why nothing switched", sender.lastText)
	}
}
