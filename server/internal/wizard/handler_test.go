package wizard

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestHandler_Start(t *testing.T) {
	t.Run("starts wizard and renders first step", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			}}},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)
		handler.Start(123)

		// Verify the server selection step was sent, not "No servers found"
		if len(sender.messages) == 0 || !strings.Contains(sender.messages[0], "Step 1/4") {
			t.Errorf("messages %q, want step 1", sender.messages)
		}

		// Verify state was created
		if handler.manager.Get(123) == nil {
			t.Error("expected state to be created")
		}
	})
}

func TestHandler_HandleCallback_Cancel(t *testing.T) {
	t.Run("clears state on cancel", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{{Name: "Server1", IPs: []string{"1.2.3.4"}}},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)
		handler.Start(123)

		// Clear messages from Start
		sender.messages = nil

		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "cancel",
			Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
		}
		handler.HandleCallback(cb)

		// Verify state was cleared
		if handler.manager.Get(123) != nil {
			t.Error("expected state to be cleared after cancel")
		}

		// Verify cancellation message
		found := false
		for _, msg := range sender.messages {
			if msg == "Configuration cancelled" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected 'Configuration cancelled' message, got: %v", sender.messages)
		}
	})
}

func TestHandler_HandleCallback_NoSession(t *testing.T) {
	t.Run("sends error message when no active session", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		// No Start() called - no session

		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "server:0",
			Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
		}
		handler.HandleCallback(cb)

		// Verify error message
		if len(sender.messages) == 0 {
			t.Error("expected error message to be sent")
		}
	})
}

func TestHandler_HandleCallback_Apply(t *testing.T) {
	t.Run("applies config and clears state on apply", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}, Address: "srv.example.com", Port: 443, UUID: "uuid-1"},
			},
			vpnConfig: &vpnconfig.VPNDirectorConfig{
				DataDir: "/opt/vpn-director/data",
				Xray:    vpnconfig.XrayConfig{},
				TunnelDirector: vpnconfig.TunnelDirectorConfig{
					Tunnels: make(map[string]vpnconfig.TunnelConfig),
				},
			},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)
		handler.Start(123)

		// Setup state for apply
		state := handler.manager.Get(123)
		state.SetServerIndex(0)
		state.SetExclusion("ru", true)
		state.AddClient(ClientRoute{IP: "192.168.1.10", Route: "xray"})
		state.SetStep(StepConfirm)

		// Clear messages from Start
		sender.messages = nil

		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "apply",
			Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
		}
		handler.HandleCallback(cb)

		// Verify state was cleared
		if handler.manager.Get(123) != nil {
			t.Error("expected state to be cleared after apply")
		}

		// Verify VPN apply was called
		if !vpnDirector.applyCalled {
			t.Error("expected VPN Apply to be called")
		}
	})
}

func TestHandler_HandleCallback_RoutesToStep(t *testing.T) {
	t.Run("routes callback to current step handler", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
				{Name: "Server2", IPs: []string{"5.6.7.8"}},
			}}},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)
		handler.Start(123)

		// State should be at StepSelectServer
		state := handler.manager.Get(123)
		if state.GetStep() != StepSelectServer {
			t.Errorf("expected step StepSelectServer, got %s", state.GetStep())
		}

		// Clear messages
		sender.messages = nil

		// Select server (this should route to ServerStep)
		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "server:0a1b2c3d:0",
			Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}, MessageID: 100},
		}
		handler.HandleCallback(cb)

		// Step should advance to StepExclusions
		if state.GetStep() != StepExclusions {
			t.Errorf("expected step StepExclusions, got %s", state.GetStep())
		}

		// Server index should be set
		if state.GetServerIndex() != 0 {
			t.Errorf("expected server index 0, got %d", state.GetServerIndex())
		}
	})
}

func TestHandler_HandleTextInput(t *testing.T) {
	t.Run("ignores text when no active session", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		// No Start() - no session

		msg := &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 123},
			Text: "192.168.1.100",
		}
		handler.HandleTextInput(msg)

		// No messages should be sent
		if len(sender.messages) != 0 {
			t.Errorf("expected no messages, got: %v", sender.messages)
		}
	})

	t.Run("routes text to step handler when awaiting IP", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)
		handler.Start(123)

		// Set step to StepClientIP (awaiting IP input)
		state := handler.manager.Get(123)
		state.SetStep(StepClientIP)

		// Clear messages
		sender.messages = nil

		msg := &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 123},
			Text: "192.168.1.100",
		}
		handler.HandleTextInput(msg)

		// PendingIP should be set
		if state.GetPendingIP() != "192.168.1.100" {
			t.Errorf("expected PendingIP '192.168.1.100', got '%s'", state.GetPendingIP())
		}

		// Step should advance to StepClientRoute
		if state.GetStep() != StepClientRoute {
			t.Errorf("expected step StepClientRoute, got %s", state.GetStep())
		}
	})
}

func TestHandler_GetManager(t *testing.T) {
	t.Run("exposes manager for external access", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		if handler.GetManager() == nil {
			t.Error("expected manager to be accessible")
		}
	})
}

func TestHandler_HandleCallback_NilMessage(t *testing.T) {
	t.Run("handles nil Message gracefully", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		// Callback with nil Message (can happen in inline mode)
		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "server:0",
			Message: nil,
		}

		// Should not panic
		handler.HandleCallback(cb)

		// No messages should be sent (except maybe AckCallback)
		// This just verifies no panic occurred
	})

	t.Run("handles nil Chat gracefully", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		// Callback with nil Chat
		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "server:0",
			Message: &tgbotapi.Message{Chat: nil},
		}

		// Should not panic
		handler.HandleCallback(cb)
	})
}

func TestHandler_HandleCallback_CancelWithoutSession(t *testing.T) {
	t.Run("cancel works even without active session", func(t *testing.T) {
		sender := &trackingSender{}
		configStore := &trackingConfigStore{
			servers:   []vpnconfig.Server{},
			vpnConfig: &vpnconfig.VPNDirectorConfig{},
		}
		vpnDirector := &mockVPNDirector{}
		xrayGen := &mockXrayGenerator{}

		handler := NewHandler(sender, configStore, vpnDirector, xrayGen)

		// No Start() called - no session exists

		cb := &tgbotapi.CallbackQuery{
			ID:      "cb1",
			Data:    "cancel",
			Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
		}

		// Should not panic and should send cancellation message
		handler.HandleCallback(cb)

		// Verify cancellation message was sent
		found := false
		for _, msg := range sender.messages {
			if msg == "Configuration cancelled" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected 'Configuration cancelled' message, got: %v", sender.messages)
		}
	})
}
