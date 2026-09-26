package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

type mockSenderClients struct {
	lastChatID   int64
	lastText     string
	lastKeyboard tgbotapi.InlineKeyboardMarkup
	editChatID   int64
	editMsgID    int
	editText     string
	editKeyboard tgbotapi.InlineKeyboardMarkup
	plainTexts   []string
}

func (m *mockSenderClients) Send(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return nil
}
func (m *mockSenderClients) SendPlain(chatID int64, text string) error {
	m.plainTexts = append(m.plainTexts, text)
	m.lastChatID = chatID
	return nil
}
func (m *mockSenderClients) SendLongPlain(chatID int64, text string) error { return nil }
func (m *mockSenderClients) SendWithKeyboard(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.lastChatID = chatID
	m.lastText = text
	m.lastKeyboard = kb
	return nil
}
func (m *mockSenderClients) SendCodeBlock(chatID int64, header, content string) error { return nil }
func (m *mockSenderClients) EditMessage(chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.editChatID = chatID
	m.editMsgID = msgID
	m.editText = text
	m.editKeyboard = kb
	return nil
}
func (m *mockSenderClients) AckCallback(callbackID string) error { return nil }

type mockConfigClients struct {
	vpnConfig   *vpnconfig.VPNDirectorConfig
	loadErr     error
	savedConfig *vpnconfig.VPNDirectorConfig
	saveErr     error
}

func (m *mockConfigClients) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	return m.vpnConfig, m.loadErr
}

func (m *mockConfigClients) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if m.loadErr != nil {
		return fmt.Errorf("%w: %w", service.ErrConfigLoad, m.loadErr)
	}
	if err := fn(m.vpnConfig); err != nil {
		return err
	}
	m.savedConfig = m.vpnConfig
	return m.saveErr
}

func (m *mockConfigClients) LoadServers() ([]vpnconfig.Server, error) { return nil, nil }
func (m *mockConfigClients) DataDir() (string, error)                 { return "/data", nil }
func (m *mockConfigClients) ScriptsDir() string                       { return "/scripts" }

func (m *mockConfigClients) LoadSubscriptions() ([]vpnconfig.Subscription, error) { return nil, nil }
func (m *mockConfigClients) SaveSubscription(vpnconfig.Subscription) error        { return nil }
func (m *mockConfigClients) DeleteSubscription(string) error                      { return nil }

type mockVPNClients struct {
	applyErr      error
	applyCalls    int
	platform      vpnconfig.PlatformInfo
	platformErr   error
	platformCalls int
}

func (m *mockVPNClients) Status() (string, error) { return "", nil }
func (m *mockVPNClients) Apply() error            { m.applyCalls++; return m.applyErr }
func (m *mockVPNClients) Restart() error          { return nil }
func (m *mockVPNClients) RestartXray() error      { return nil }
func (m *mockVPNClients) Stop() error             { return nil }
func (m *mockVPNClients) Update() error           { return nil }
func (m *mockVPNClients) Platform() (vpnconfig.PlatformInfo, error) {
	m.platformCalls++
	return m.platform, m.platformErr
}

func TestClientsHandler_HandleClients_WithClients(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{
				Clients: []string{"192.168.50.10"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{"192.168.50.20/32"}},
				},
			},
			PausedClients: []string{"192.168.50.20/32"},
		},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}}
	h.HandleClients(msg)

	if sender.lastChatID != 100 {
		t.Errorf("expected chatID 100, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "192\\.168\\.50\\.10") {
		t.Errorf("expected message to contain escaped 192.168.50.10, got: %s", sender.lastText)
	}
	if !strings.Contains(sender.lastText, "192\\.168\\.50\\.20") {
		t.Errorf("expected message to contain escaped 192.168.50.20/32, got: %s", sender.lastText)
	}
	// A row per client (pause or resume, move, remove) + the Add/Close row = 3 rows
	if len(sender.lastKeyboard.InlineKeyboard) != 3 {
		t.Errorf("expected 3 keyboard rows, got %d", len(sender.lastKeyboard.InlineKeyboard))
	}
}

func TestClientsHandler_HandleClients_Empty(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{
			Xray:           vpnconfig.XrayConfig{},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{},
		},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}}
	h.HandleClients(msg)

	if len(sender.lastKeyboard.InlineKeyboard) != 1 {
		t.Errorf("expected 1 keyboard row (Add), got %d", len(sender.lastKeyboard.InlineKeyboard))
	}
}

func TestClientsHandler_HandlePause(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:pause:192.168.50.10",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.PausedClients) != 1 || config.savedConfig.PausedClients[0] != "192.168.50.10" {
		t.Errorf("expected paused_clients=[192.168.50.10], got %v", config.savedConfig.PausedClients)
	}
	if sender.editMsgID != 42 {
		t.Errorf("expected message 42 to be edited, got %d", sender.editMsgID)
	}
}

func TestClientsHandler_HandleResume(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
		PausedClients: []string{"192.168.50.10"},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:resume:192.168.50.10",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.PausedClients) != 0 {
		t.Errorf("expected empty paused_clients, got %v", config.savedConfig.PausedClients)
	}
}

func TestClientsHandler_HandleRemoveConfirm(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:remove:192.168.50.10",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if sender.editMsgID != 42 {
		t.Errorf("expected message 42 to be edited, got %d", sender.editMsgID)
	}
	if len(sender.editKeyboard.InlineKeyboard) != 1 {
		t.Errorf("expected 1 keyboard row, got %d", len(sender.editKeyboard.InlineKeyboard))
	}
}

func TestClientsHandler_HandleRemoveYes_Xray(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		Xray:          vpnconfig.XrayConfig{Clients: []string{"192.168.50.10", "192.168.50.20"}},
		PausedClients: []string{"192.168.50.10"},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:rm_yes:192.168.50.10",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.Xray.Clients) != 1 || config.savedConfig.Xray.Clients[0] != "192.168.50.20" {
		t.Errorf("expected xray.clients=[192.168.50.20], got %v", config.savedConfig.Xray.Clients)
	}
	if len(config.savedConfig.PausedClients) != 0 {
		t.Errorf("expected empty paused_clients, got %v", config.savedConfig.PausedClients)
	}
}

func TestClientsHandler_HandleRemoveYes_Tunnel(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{"192.168.50.30/32", "192.168.50.40/32"}, Exclude: []string{"ru"}},
			},
		},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:rm_yes:192.168.50.30/32",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	wgc1 := config.savedConfig.TunnelDirector.Tunnels["wgc1"]
	if len(wgc1.Clients) != 1 || wgc1.Clients[0] != "192.168.50.40/32" {
		t.Errorf("expected wgc1.clients=[192.168.50.40/32], got %v", wgc1.Clients)
	}
}

func TestClientsHandler_HandleAdd_SetsState(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{
			Xray:           vpnconfig.XrayConfig{Clients: []string{}},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{}},
		},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:add",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if len(sender.plainTexts) == 0 {
		t.Fatal("expected a plain text message")
	}
}

func TestClientsHandler_HandleTextInput_ValidIP(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{Clients: []string{}},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{
					"wgc1": {Clients: []string{}, Exclude: []string{"ru"}},
				},
			},
		},
	}
	deps := &Deps{Sender: sender, Config: config, VPN: &mockVPNClients{}}
	h := NewClientsHandler(deps)

	h.mu.Lock()
	h.addState[100] = ""
	h.mu.Unlock()

	msg := &tgbotapi.Message{
		Text: "192.168.50.10",
		Chat: &tgbotapi.Chat{ID: 100},
	}
	h.HandleTextInput(msg)

	if sender.lastChatID != 100 {
		t.Errorf("expected chatID 100, got %d", sender.lastChatID)
	}
	if len(sender.lastKeyboard.InlineKeyboard) == 0 {
		t.Error("expected route selection keyboard")
	}
}

func TestClientsHandler_HandleTextInput_InvalidIP(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	h.mu.Lock()
	h.addState[100] = ""
	h.mu.Unlock()

	msg := &tgbotapi.Message{
		Text: "not-an-ip",
		Chat: &tgbotapi.Chat{ID: 100},
	}
	h.HandleTextInput(msg)

	if len(sender.plainTexts) == 0 {
		t.Fatal("expected error message")
	}
}

func TestClientsHandler_HandleTextInput_DuplicateIP(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{
			Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}},
		},
	}
	deps := &Deps{Sender: sender, Config: config}
	h := NewClientsHandler(deps)

	h.mu.Lock()
	h.addState[100] = ""
	h.mu.Unlock()

	msg := &tgbotapi.Message{
		Text: "192.168.50.10",
		Chat: &tgbotapi.Chat{ID: 100},
	}
	h.HandleTextInput(msg)

	if len(sender.plainTexts) == 0 {
		t.Fatal("expected duplicate error message")
	}
}

func TestClientsHandler_HandleAddRoute_Xray(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{}},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	h.mu.Lock()
	h.addState[100] = "192.168.50.10"
	h.mu.Unlock()

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:route:xray",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.Xray.Clients) != 1 || config.savedConfig.Xray.Clients[0] != "192.168.50.10" {
		t.Errorf("expected xray.clients=[192.168.50.10], got %v", config.savedConfig.Xray.Clients)
	}
}

func TestClientsHandler_HandleAddRoute_Tunnel(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{}, Exclude: []string{"ru"}},
			},
		},
	}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{}
	deps := &Deps{Sender: sender, Config: config, VPN: vpn}
	h := NewClientsHandler(deps)

	h.mu.Lock()
	// Seeded with the /32 spelling so the assertion below actually exercises
	// the normalization handleAddRoute performs.
	h.addState[100] = "192.168.50.30/32"
	h.mu.Unlock()

	cb := &tgbotapi.CallbackQuery{
		Data:    "clients:route:wgc1",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
	h.HandleCallback(cb)

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	wgc1 := config.savedConfig.TunnelDirector.Tunnels["wgc1"]
	// Should be normalized (no /32 suffix)
	if len(wgc1.Clients) != 1 || wgc1.Clients[0] != "192.168.50.30" {
		t.Errorf("expected wgc1.clients=[192.168.50.30], got %v", wgc1.Clients)
	}
}

func TestClientsHandler_HandleTextInput_NotInAddState(t *testing.T) {
	sender := &mockSenderClients{}
	deps := &Deps{Sender: sender}
	h := NewClientsHandler(deps)

	msg := &tgbotapi.Message{
		Text: "192.168.50.10",
		Chat: &tgbotapi.Chat{ID: 100},
	}
	h.HandleTextInput(msg)

	if sender.lastChatID != 0 {
		t.Error("expected no message sent when not in add state")
	}
}

func TestClientsHandler_Pause_SaveErrorIsReported(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{
		vpnConfig: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}}},
		saveErr:   errors.New("disk full"),
	}
	vpn := &mockVPNClients{}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:pause:192.168.50.10",
		Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if n := len(sender.plainTexts); n == 0 || !strings.Contains(sender.plainTexts[n-1], "Save error: disk full") {
		t.Errorf("expected the save error to reach the user, got %v", sender.plainTexts)
	}
}

func TestClientsHandler_RouteKeyboardListsPlatformTunnelsThenConfiguredOnes(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{TunnelDirector: vpnconfig.TunnelDirectorConfig{
		Tunnels: map[string]vpnconfig.TunnelConfig{"wgc1": {Clients: []string{}}},
	}}
	vpn := &mockVPNClients{platform: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "OpenVPN0", Connected: true, Description: "office"},
	}}}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: cfg}, VPN: vpn})

	h.showRouteSelection(100, "192.168.1.5", cfg)

	rows := sender.lastKeyboard.InlineKeyboard
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want xray, OpenVPN0, wgc1, Cancel", len(rows))
	}
	if *rows[1][0].CallbackData != "clients:route:OpenVPN0" || rows[1][0].Text != "OpenVPN0 office" {
		t.Errorf("row 1 = %+v", rows[1][0])
	}
	if *rows[2][0].CallbackData != "clients:route:wgc1" || rows[2][0].Text != "wgc1 (unknown)" {
		t.Errorf("row 2 = %+v: a configured tunnel the platform does not list stays reachable, marked unknown", rows[2][0])
	}
}

func TestClientsHandler_RouteKeyboardFallsBackToTheConfigWhenThePlatformIsDown(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{TunnelDirector: vpnconfig.TunnelDirectorConfig{
		Tunnels: map[string]vpnconfig.TunnelConfig{"wgc1": {Clients: []string{}}},
	}}
	vpn := &mockVPNClients{platformErr: errors.New("down")}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: cfg}, VPN: vpn})

	h.showRouteSelection(100, "192.168.1.5", cfg)

	rows := sender.lastKeyboard.InlineKeyboard
	if len(rows) != 3 || *rows[1][0].CallbackData != "clients:route:wgc1" {
		t.Errorf("rows = %+v, want xray, wgc1, Cancel", rows)
	}
	// The platform is what is unknown here, not the tunnel, so no "(unknown)" label.
	if rows[1][0].Text != "wgc1" {
		t.Errorf("row 1 text = %q, want the bare tunnel name", rows[1][0].Text)
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "platform info unavailable") {
		t.Errorf("plain texts = %v", sender.plainTexts)
	}
}

func TestClientsHandler_HandleAddRoute_NewPlatformTunnelInheritsTheXrayExclusions(t *testing.T) {
	sender := &mockSenderClients{}
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{ExcludeSets: []string{"ru"}}}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "OpenVPN0"}}}}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})
	h.mu.Lock()
	h.addState[100] = "192.168.1.5"
	h.mu.Unlock()

	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:route:OpenVPN0",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	tunnel := config.savedConfig.TunnelDirector.Tunnels["OpenVPN0"]
	if len(tunnel.Clients) != 1 || tunnel.Clients[0] != "192.168.1.5" || len(tunnel.Exclude) != 1 || tunnel.Exclude[0] != "ru" {
		t.Errorf("OpenVPN0 = %+v", tunnel)
	}
}

func TestClientsHandler_HandleAddRoute_UnknownRouteIsStale(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: &vpnconfig.VPNDirectorConfig{}}
	vpn := &mockVPNClients{platform: vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{{ID: "OpenVPN0"}}}}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})
	h.mu.Lock()
	h.addState[100] = "192.168.1.5"
	h.mu.Unlock()

	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:route:wgc1",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if config.savedConfig != nil {
		t.Error("a route the router does not have must not be saved")
	}
	// A list redrawn without the new row reads as a bug; the user is told why.
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "route wgc1 is no longer available") {
		t.Errorf("plain texts = %v, want the vanished route named", sender.plainTexts)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not refreshed: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_HandleAddRoute_PlatformErrorIsReported(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: &vpnconfig.VPNDirectorConfig{}}
	vpn := &mockVPNClients{platformErr: errors.New("down")}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})
	h.mu.Lock()
	h.addState[100] = "192.168.1.5"
	h.mu.Unlock()

	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:route:wgc1",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})

	if config.savedConfig != nil {
		t.Error("a route that could not be checked must not be saved")
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "platform info unavailable, try again") {
		t.Errorf("plain texts = %v, want the platform reported as unavailable", sender.plainTexts)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not refreshed: editMsgID = %d", sender.editMsgID)
	}
}

// failedOverClientsCfg is a committed failover of 192.168.50.8 onto wgc1.
func failedOverClientsCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{
			Failover: &vpnconfig.XrayFailover{
				Tunnel: "wgc1", Clients: []string{"192.168.50.8"}, Added: []string{"192.168.50.8"}, Committed: true,
			},
		},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{
			Tunnels: map[string]vpnconfig.TunnelConfig{
				"wgc1": {Clients: []string{"192.168.50.3", "192.168.50.8"}, Exclude: []string{"ru"}},
			},
		},
	}
}

// Removing an address during a failover is a decision about it: the restore
// must not bring it back.
func TestClientsHandler_HandleRemoveYes_DetachesTheAddressFromTheFailover(t *testing.T) {
	config := &mockConfigClients{vpnConfig: failedOverClientsCfg()}
	h := NewClientsHandler(&Deps{Sender: &mockSenderClients{}, Config: config, VPN: &mockVPNClients{}})
	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:rm_yes:192.168.50.8",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})
	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if fo := config.savedConfig.Xray.Failover; fo == nil || len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Fatalf("failover %+v; the removed address is still one the restore brings back", fo)
	}
}

// Putting an address on a tunnel during a failover is where the user wants it;
// the restore used to take it back to Xray.
func TestClientsHandler_HandleAddRoute_DetachesTheAddressFromTheFailover(t *testing.T) {
	cfg := failedOverClientsCfg()
	cfg.TunnelDirector.Tunnels["wgc1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.50.3"}, Exclude: []string{"ru"}}
	config := &mockConfigClients{vpnConfig: cfg}
	h := NewClientsHandler(&Deps{Sender: &mockSenderClients{}, Config: config, VPN: &mockVPNClients{}})
	h.mu.Lock()
	h.addState[100] = "192.168.50.8"
	h.mu.Unlock()
	h.HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "clients:route:wgc1",
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	})
	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if fo := config.savedConfig.Xray.Failover; fo == nil || len(fo.Clients) != 0 || len(fo.Added) != 0 {
		t.Fatalf("failover %+v; the restore would take the address off the tunnel it was just put on", fo)
	}
}

// moveCfg is 192.168.50.10 on Xray, which excludes ru, and 192.168.50.30 on wgc1.
func moveCfg() *vpnconfig.VPNDirectorConfig {
	return &vpnconfig.VPNDirectorConfig{
		Xray: vpnconfig.XrayConfig{Clients: []string{"192.168.50.10"}, ExcludeSets: []string{"ru"}},
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"wgc1": {Clients: []string{"192.168.50.30"}, Exclude: []string{"ru"}},
		}},
	}
}

// movePlatform lists OpenVPN0 up and Wireguard1 down.
func movePlatform() vpnconfig.PlatformInfo {
	return vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "OpenVPN0", Connected: true, Description: "office"},
		{ID: "Wireguard1", Connected: false},
	}}
}

func moveCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{
		Data:    data,
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 100}},
	}
}

func TestClientsHandler_ListOffersAMoveButtonPerClient(t *testing.T) {
	sender := &mockSenderClients{}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: moveCfg()}})
	h.HandleClients(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}})

	row := sender.lastKeyboard.InlineKeyboard[0]
	if len(row) != 3 || *row[1].CallbackData != "clients:move:192.168.50.10" || row[1].Text != "\U0001f500 192.168.50.10" {
		t.Fatalf("row 0 = %+v, want pause, move, remove", row)
	}
}

// An entry an older build saved as IPv6 goes on no route: it can be paused or
// removed, not moved.
func TestClientsHandler_NoMoveButtonForAnAddressNoRouteTakes(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{Clients: []string{"fd00::10"}}}
	sender := &mockSenderClients{}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: cfg}})
	h.HandleClients(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}})

	if row := sender.lastKeyboard.InlineKeyboard[0]; len(row) != 2 {
		t.Fatalf("row 0 = %+v, want pause and remove only", row)
	}
}

func TestClientsHandler_MoveKeyboardMarksTheCurrentRoute(t *testing.T) {
	sender := &mockSenderClients{}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: &mockConfigClients{vpnConfig: moveCfg()}, VPN: vpn})

	h.HandleCallback(moveCallback("clients:move:192.168.50.10"))

	rows := sender.editKeyboard.InlineKeyboard
	want := []struct{ text, data string }{
		{"✓ xray", "clients:to:xray:192.168.50.10"},
		{"OpenVPN0 office", "clients:to:OpenVPN0:192.168.50.10"},
		{"Wireguard1 (down)", "clients:to:Wireguard1:192.168.50.10"},
		{"wgc1 (unknown)", "clients:to:wgc1:192.168.50.10"},
		{"« Back", "clients:rm_no"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i, w := range want {
		if rows[i][0].Text != w.text || *rows[i][0].CallbackData != w.data {
			t.Errorf("row %d = %q %q, want %q %q", i, rows[i][0].Text, *rows[i][0].CallbackData, w.text, w.data)
		}
	}
	if sender.editMsgID != 42 {
		t.Errorf("editMsgID = %d, want the list replaced", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToATunnel(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.10"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if len(config.savedConfig.Xray.Clients) != 0 {
		t.Errorf("xray.clients = %v", config.savedConfig.Xray.Clients)
	}
	tun := config.savedConfig.TunnelDirector.Tunnels["OpenVPN0"]
	if strings.Join(tun.Clients, ",") != "192.168.50.10" || strings.Join(tun.Exclude, ",") != "ru" {
		t.Errorf("OpenVPN0 = %+v", tun)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("applies = %d, want 1", vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToADownTunnelAsksFirst(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:Wireguard1:192.168.50.10"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Fatal("a move to a tunnel that is down happens only once confirmed")
	}
	if !strings.Contains(sender.editText, "Wireguard1 is down") {
		t.Errorf("edit text = %q", sender.editText)
	}
	row := sender.editKeyboard.InlineKeyboard[0]
	if len(row) != 2 || *row[0].CallbackData != "clients:toyes:Wireguard1:192.168.50.10" || *row[1].CallbackData != "clients:rm_no" {
		t.Errorf("confirmation row = %+v", row)
	}
}

func TestClientsHandler_MoveConfirmedToADownTunnel(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:toyes:Wireguard1:192.168.50.10"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if got := config.savedConfig.TunnelDirector.Tunnels["Wireguard1"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("Wireguard1 = %v", got)
	}
	if vpn.applyCalls != 1 {
		t.Errorf("applies = %d, want 1", vpn.applyCalls)
	}
}

func TestClientsHandler_MoveToTheRouteItIsOnChangesNothing(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:xray:192.168.50.10"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveOfAClientThatIsGoneRedrawsTheList(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.99"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_MoveToARouteNoLongerAvailable(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:Wireguard9:192.168.50.10"))

	if config.savedConfig != nil {
		t.Error("a route the router does not have must not be saved")
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "route Wireguard9 is no longer available") {
		t.Errorf("plain texts = %v", sender.plainTexts)
	}
}

// The buttons carry the stored spelling; the paused entry follows the address
// to the spelling it is stored under now.
func TestClientsHandler_MoveKeepsThePausedState(t *testing.T) {
	cfg := moveCfg()
	cfg.Xray.Clients = []string{"192.168.50.10/32"}
	cfg.PausedClients = []string{"192.168.50.10/32"}
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:OpenVPN0:192.168.50.10/32"))

	if config.savedConfig == nil {
		t.Fatal("expected config to be saved")
	}
	if got := config.savedConfig.TunnelDirector.Tunnels["OpenVPN0"].Clients; strings.Join(got, ",") != "192.168.50.10" {
		t.Errorf("OpenVPN0 = %v", got)
	}
	if strings.Join(config.savedConfig.PausedClients, ",") != "192.168.50.10" {
		t.Errorf("paused_clients = %v", config.savedConfig.PausedClients)
	}
}

// Tunnel Director marks private IPv4 addresses and networks only
// (vpnconfig.TDCarries): on a tunnel route any other address is on no route at
// all, out through the WAN. The API answers 400; the bot says why and redraws.
func TestClientsHandler_MoveToATunnelRefusesAnAddressTunnelDirectorCannotCarry(t *testing.T) {
	cfg := moveCfg()
	cfg.Xray.Clients = append(cfg.Xray.Clients, "100.64.0.8")
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:toyes:OpenVPN0:100.64.0.8"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Fatalf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "Tunnel Director routes private IPv4 addresses only; 100.64.0.8 can go on xray") {
		t.Errorf("plain texts = %v", sender.plainTexts)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

func TestClientsHandler_HandleAddRoute_RefusesATunnelForAnAddressTunnelDirectorCannotCarry(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})
	h.mu.Lock()
	h.addState[100] = "100.64.0.8"
	h.mu.Unlock()

	h.HandleCallback(moveCallback("clients:route:wgc1"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Fatalf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if len(sender.plainTexts) == 0 || !strings.Contains(sender.plainTexts[len(sender.plainTexts)-1], "Tunnel Director routes private IPv4 addresses only; 100.64.0.8 can go on xray") {
		t.Errorf("plain texts = %v", sender.plainTexts)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

// A configured tunnel the platform does not list - "(unknown)" on the keyboard:
// a typo, a connection deleted in the router, RCI silent - is one Tunnel
// Director skips. The move asks first, as for a tunnel that is down.
func TestClientsHandler_MoveToAnUnknownTunnelAsksFirst(t *testing.T) {
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:wgc1:192.168.50.10"))

	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Fatal("a move to a tunnel the router does not list happens only once confirmed")
	}
	want := "wgc1 is not on the router's tunnel list: until it is, Tunnel Director does not route 192.168.50.10 through it. Move anyway?"
	if !strings.Contains(sender.editText, telegramEscaped(want)) {
		t.Errorf("edit text = %q", sender.editText)
	}
	row := sender.editKeyboard.InlineKeyboard[0]
	if len(row) != 2 || *row[0].CallbackData != "clients:toyes:wgc1:192.168.50.10" || *row[1].CallbackData != "clients:rm_no" {
		t.Errorf("confirmation row = %+v", row)
	}
}

func TestClientsHandler_MoveConfirmedToAnUnknownTunnel(t *testing.T) {
	config := &mockConfigClients{vpnConfig: moveCfg()}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: &mockSenderClients{}, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:toyes:wgc1:192.168.50.10"))

	if config.savedConfig == nil || vpn.applyCalls != 1 {
		t.Fatalf("saved %v, applies %d: the confirmed move goes ahead", config.savedConfig, vpn.applyCalls)
	}
	if got := config.savedConfig.TunnelDirector.Tunnels["wgc1"].Clients; strings.Join(got, ",") != "192.168.50.30,192.168.50.10" {
		t.Errorf("wgc1 = %v", got)
	}
}

// The ✓ route of a client on a tunnel that is down: tapping it moves nothing,
// so it asks nothing either.
func TestClientsHandler_TappingTheCurrentRouteAsksNothing(t *testing.T) {
	cfg := moveCfg()
	cfg.Xray.Clients = nil
	cfg.TunnelDirector.Tunnels["Wireguard1"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.50.10"}}
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:to:Wireguard1:192.168.50.10"))

	if strings.Contains(sender.editText, "Move anyway") {
		t.Errorf("edit text = %q: a tap on the current route asked", sender.editText)
	}
	if config.savedConfig != nil || vpn.applyCalls != 0 {
		t.Errorf("saved %v, applies %d: nothing may happen", config.savedConfig, vpn.applyCalls)
	}
	if sender.editMsgID != 42 {
		t.Errorf("the list was not redrawn: editMsgID = %d", sender.editMsgID)
	}
}

// main is Tunnel Director's own route, which no platform lists: never
// "(unknown)", and a move to it asks nothing - the platform included, once
// the config has main.
func TestClientsHandler_MainIsNeverUnknown(t *testing.T) {
	cfg := moveCfg()
	cfg.TunnelDirector.Tunnels["main"] = vpnconfig.TunnelConfig{Clients: []string{"192.168.50.40"}}
	sender := &mockSenderClients{}
	config := &mockConfigClients{vpnConfig: cfg}
	vpn := &mockVPNClients{platform: movePlatform()}
	h := NewClientsHandler(&Deps{Sender: sender, Config: config, VPN: vpn})

	h.HandleCallback(moveCallback("clients:move:192.168.50.10"))
	found := false
	for _, row := range sender.editKeyboard.InlineKeyboard {
		if *row[0].CallbackData == "clients:to:main:192.168.50.10" {
			found = true
			if row[0].Text != "main" {
				t.Errorf("main's button = %q", row[0].Text)
			}
		}
	}
	if !found {
		t.Fatalf("no main button: %+v", sender.editKeyboard.InlineKeyboard)
	}

	asked := vpn.platformCalls
	h.HandleCallback(moveCallback("clients:to:main:192.168.50.10"))
	if config.savedConfig == nil || vpn.applyCalls != 1 {
		t.Fatalf("saved %v, applies %d: a move to main asks nothing", config.savedConfig, vpn.applyCalls)
	}
	if vpn.platformCalls != asked {
		t.Errorf("platform calls %d: a move to a configured main asks the platform nothing", vpn.platformCalls-asked)
	}
}

// telegramEscaped is s as the bot sends it in MarkdownV2.
func telegramEscaped(s string) string {
	return telegram.EscapeMarkdownV2(s)
}
