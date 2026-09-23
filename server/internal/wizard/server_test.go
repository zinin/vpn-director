package wizard

import (
	"errors"
	"fmt"
	"os"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockSender for testing
type mockSender struct {
	lastChatID   int64
	lastText     string
	lastKeyboard *tgbotapi.InlineKeyboardMarkup
	sendError    error
}

func (m *mockSender) Send(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return m.sendError
}

func (m *mockSender) SendPlain(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return m.sendError
}

func (m *mockSender) SendLongPlain(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return m.sendError
}

func (m *mockSender) SendWithKeyboard(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.lastChatID = chatID
	m.lastText = text
	m.lastKeyboard = &kb
	return m.sendError
}

func (m *mockSender) SendCodeBlock(chatID int64, header, content string) error {
	m.lastChatID = chatID
	m.lastText = header + "\n" + content
	return m.sendError
}

func (m *mockSender) EditMessage(chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.lastChatID = chatID
	m.lastText = text
	m.lastKeyboard = &kb
	return m.sendError
}

func (m *mockSender) AckCallback(callbackID string) error {
	return nil
}

// mockConfigStore for testing
type mockConfigStore struct {
	servers   []vpnconfig.Server
	subs      []vpnconfig.Subscription
	vpnConfig *vpnconfig.VPNDirectorConfig
	err       error
}

func (m *mockConfigStore) LoadServers() ([]vpnconfig.Server, error) {
	return m.servers, m.err
}

func (m *mockConfigStore) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	return m.vpnConfig, m.err
}

func (m *mockConfigStore) SaveServers([]vpnconfig.Server) error {
	return m.err
}

func (m *mockConfigStore) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	return m.subs, m.err
}
func (m *mockConfigStore) SaveSubscription(vpnconfig.Subscription) error { return m.err }
func (m *mockConfigStore) DeleteSubscription(string) error               { return m.err }

func (m *mockConfigStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	if m.err != nil {
		return fmt.Errorf("%w: %w", service.ErrConfigLoad, m.err)
	}
	if m.vpnConfig == nil {
		return fmt.Errorf("%w: %w", service.ErrConfigLoad, os.ErrNotExist)
	}
	return fn(m.vpnConfig)
}

func (m *mockConfigStore) DataDir() (string, error) {
	return "/opt/vpn-director/data", m.err
}

func (m *mockConfigStore) DataDirOrDefault() string {
	return "/opt/vpn-director/data"
}

func (m *mockConfigStore) ScriptsDir() string {
	return "/opt/vpn-director"
}

// mockManager to track wizard state clearing
type mockManager struct {
	clearedChatID int64
}

func (m *mockManager) Clear(chatID int64) {
	m.clearedChatID = chatID
}

func TestGetServerGridColumns(t *testing.T) {
	tests := []struct {
		name     string
		count    int
		expected int
	}{
		{"zero servers", 0, 1},
		{"one server", 1, 1},
		{"five servers", 5, 1},
		{"ten servers", 10, 1},
		{"eleven servers", 11, 2},
		{"twenty servers", 20, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getServerGridColumns(tt.count)
			if result != tt.expected {
				t.Errorf("getServerGridColumns(%d) = %d, want %d", tt.count, result, tt.expected)
			}
		})
	}
}

func TestServerStep_Render(t *testing.T) {
	t.Run("renders server selection with keyboard", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1"},
				{Name: "Server2"},
				{Name: "Server3"},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		nextCalled := false
		step := NewServerStep(deps, func(chatID int64, state *State) {
			nextCalled = true
		})

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		step.Render(123, state)

		if sender.lastChatID != 123 {
			t.Errorf("expected chatID 123, got %d", sender.lastChatID)
		}

		if sender.lastKeyboard == nil {
			t.Fatal("expected keyboard to be sent")
		}

		// Should have 3 server buttons + 1 cancel row = 4 rows total
		if len(sender.lastKeyboard.InlineKeyboard) != 4 {
			t.Errorf("expected 4 rows, got %d", len(sender.lastKeyboard.InlineKeyboard))
		}

		// Verify server buttons
		if sender.lastKeyboard.InlineKeyboard[0][0].Text != "1. Server1" {
			t.Errorf("expected first button '1. Server1', got '%s'", sender.lastKeyboard.InlineKeyboard[0][0].Text)
		}

		// Verify cancel button
		lastRow := sender.lastKeyboard.InlineKeyboard[len(sender.lastKeyboard.InlineKeyboard)-1]
		if lastRow[0].Text != "Cancel" {
			t.Errorf("expected cancel button, got '%s'", lastRow[0].Text)
		}

		if nextCalled {
			t.Error("next callback should not be called on render")
		}
	})

	t.Run("sends error and clears wizard on no servers", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{},
		}

		var clearedChatID int64
		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewServerStep(deps, nil)
		step.onClear = func(chatID int64) {
			clearedChatID = chatID
		}

		state := &State{
			ChatID:     456,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		step.Render(456, state)

		// Should send error message
		if sender.lastText == "" {
			t.Error("expected error message to be sent")
		}

		// Should clear wizard
		if clearedChatID != 456 {
			t.Errorf("expected wizard to be cleared for chatID 456, got %d", clearedChatID)
		}
	})

	t.Run("sends error and clears wizard on load error", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			err: errors.New("load error"),
		}

		var clearedChatID int64
		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewServerStep(deps, nil)
		step.onClear = func(chatID int64) {
			clearedChatID = chatID
		}

		state := &State{
			ChatID:     789,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		step.Render(789, state)

		// Should clear wizard
		if clearedChatID != 789 {
			t.Errorf("expected wizard to be cleared for chatID 789, got %d", clearedChatID)
		}
	})
}

func TestServerStep_HandleCallback(t *testing.T) {
	t.Run("selects server and advances to exclusions step", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1"},
				{Name: "Server2"},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		var nextChatID int64
		var nextState *State
		step := NewServerStep(deps, func(chatID int64, state *State) {
			nextChatID = chatID
			nextState = state
		})

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "server:1",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		step.HandleCallback(cb, state)

		// Should set server index
		if state.GetServerIndex() != 1 {
			t.Errorf("expected server index 1, got %d", state.GetServerIndex())
		}

		// Should advance to exclusions step
		if state.GetStep() != StepExclusions {
			t.Errorf("expected step %s, got %s", StepExclusions, state.GetStep())
		}

		// Should set default exclusion for "ru"
		exclusions := state.GetExclusions()
		if !exclusions["ru"] {
			t.Error("expected 'ru' exclusion to be set")
		}

		// Should call next callback
		if nextChatID != 123 {
			t.Errorf("expected next callback with chatID 123, got %d", nextChatID)
		}

		if nextState != state {
			t.Error("expected next callback with same state")
		}
	})

	t.Run("ignores invalid server index", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1"},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		nextCalled := false
		step := NewServerStep(deps, func(chatID int64, state *State) {
			nextCalled = true
		})

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "server:5", // Invalid index
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		step.HandleCallback(cb, state)

		// Should not advance step
		if state.GetStep() != StepSelectServer {
			t.Errorf("expected step to remain %s, got %s", StepSelectServer, state.GetStep())
		}

		// Should not call next
		if nextCalled {
			t.Error("next callback should not be called for invalid index")
		}
	})

	t.Run("ignores negative server index", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1"},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		nextCalled := false
		step := NewServerStep(deps, func(chatID int64, state *State) {
			nextCalled = true
		})

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "server:-1",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		step.HandleCallback(cb, state)

		if nextCalled {
			t.Error("next callback should not be called for negative index")
		}
	})

	t.Run("ignores non-server callbacks", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1"},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		nextCalled := false
		step := NewServerStep(deps, func(chatID int64, state *State) {
			nextCalled = true
		})

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "other:data",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		step.HandleCallback(cb, state)

		if nextCalled {
			t.Error("next callback should not be called for non-server callback")
		}
	})
}

func TestServerStep_HandleMessage(t *testing.T) {
	t.Run("returns false for any text input", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewServerStep(deps, nil)

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		msg := &tgbotapi.Message{
			Text: "some text",
			Chat: &tgbotapi.Chat{ID: 123},
		}

		handled := step.HandleMessage(msg, state)

		if handled {
			t.Error("expected HandleMessage to return false")
		}
	})
}

func TestServerStep_GridLayoutWithManyServers(t *testing.T) {
	t.Run("uses 2 columns for more than 10 servers", func(t *testing.T) {
		sender := &mockSender{}
		servers := make([]vpnconfig.Server, 12)
		for i := range servers {
			servers[i] = vpnconfig.Server{Name: "Server"}
		}
		configStore := &mockConfigStore{servers: servers}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewServerStep(deps, nil)

		state := &State{
			ChatID:     123,
			Step:       StepSelectServer,
			Exclusions: make(map[string]bool),
		}

		step.Render(123, state)

		if sender.lastKeyboard == nil {
			t.Fatal("expected keyboard to be sent")
		}

		// With 12 servers and 2 columns, we should have 6 server rows + 1 cancel row = 7 rows
		if len(sender.lastKeyboard.InlineKeyboard) != 7 {
			t.Errorf("expected 7 rows, got %d", len(sender.lastKeyboard.InlineKeyboard))
		}

		// First row should have 2 buttons (2 columns)
		if len(sender.lastKeyboard.InlineKeyboard[0]) != 2 {
			t.Errorf("expected 2 columns, got %d", len(sender.lastKeyboard.InlineKeyboard[0]))
		}
	})
}
