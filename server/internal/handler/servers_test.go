// internal/handler/servers_test.go
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// mockSenderWithKeyboard tracks keyboard sent
type mockSenderWithKeyboard struct {
	lastChatID   int64
	lastText     string
	lastKeyboard tgbotapi.InlineKeyboardMarkup
	lastMsgID    int
	lastAckID    string
}

func (m *mockSenderWithKeyboard) Send(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return nil
}
func (m *mockSenderWithKeyboard) SendPlain(chatID int64, text string) error     { return nil }
func (m *mockSenderWithKeyboard) SendLongPlain(chatID int64, text string) error { return nil }
func (m *mockSenderWithKeyboard) SendWithKeyboard(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.lastChatID = chatID
	m.lastText = text
	m.lastKeyboard = kb
	return nil
}
func (m *mockSenderWithKeyboard) SendCodeBlock(chatID int64, header, content string) error {
	return nil
}
func (m *mockSenderWithKeyboard) EditMessage(chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	m.lastChatID = chatID
	m.lastMsgID = msgID
	m.lastText = text
	m.lastKeyboard = kb
	return nil
}
func (m *mockSenderWithKeyboard) AckCallback(callbackID string) error {
	m.lastAckID = callbackID
	return nil
}

// Tests for helper functions (migrated from handlers_test.go)

func TestExtractCountry(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "country and city",
			input:    "Чехия, Прага",
			expected: "Чехия",
		},
		{
			name:     "country and extra",
			input:    "Германия, Extra",
			expected: "Германия",
		},
		{
			name:     "english format",
			input:    "Germany, Berlin",
			expected: "Germany",
		},
		{
			name:     "with leading/trailing spaces",
			input:    "  Россия , Москва  ",
			expected: "Россия",
		},
		{
			name:     "no comma",
			input:    "Unknown Server",
			expected: "Unknown Server",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "Other",
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: "Other",
		},
		{
			name:     "comma at start",
			input:    ", City",
			expected: "Other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCountry(tt.input)
			if got != tt.expected {
				t.Errorf("extractCountry(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestGroupServersByCountry(t *testing.T) {
	tests := []struct {
		name     string
		servers  []vpnconfig.Server
		expected string
	}{
		{
			name:     "empty list",
			servers:  []vpnconfig.Server{},
			expected: "",
		},
		{
			name: "single country",
			servers: []vpnconfig.Server{
				{Name: "Германия, Берлин"},
				{Name: "Германия, Франкфурт"},
			},
			expected: "Германия (2)",
		},
		{
			name: "multiple countries sorted by count",
			servers: []vpnconfig.Server{
				{Name: "США, Нью-Йорк"},
				{Name: "Германия, Берлин"},
				{Name: "США, Майами"},
				{Name: "США, Лос-Анджелес"},
				{Name: "Германия, Франкфурт"},
			},
			expected: "США (3), Германия (2)",
		},
		{
			name: "same count sorted alphabetically",
			servers: []vpnconfig.Server{
				{Name: "Германия, Берлин"},
				{Name: "Австрия, Вена"},
			},
			expected: "Австрия (1), Германия (1)",
		},
		{
			name: "more than 10 countries",
			servers: []vpnconfig.Server{
				{Name: "A, City"}, {Name: "A, City"},
				{Name: "B, City"},
				{Name: "C, City"},
				{Name: "D, City"},
				{Name: "E, City"},
				{Name: "F, City"},
				{Name: "G, City"},
				{Name: "H, City"},
				{Name: "I, City"},
				{Name: "J, City"},
				{Name: "K, City"},
				{Name: "L, City"},
			},
			expected: "A (2), B (1), C (1), D (1), E (1), F (1), G (1), H (1), I (1), J (1), и ещё 2 стран",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupServersByCountry(tt.servers)
			if got != tt.expected {
				t.Errorf("groupServersByCountry() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestBuildServersPageEmpty(t *testing.T) {
	// Test guard clause for empty servers list - should not panic
	text, keyboard := buildServersPage([]vpnconfig.Server{}, 0)

	if text != "No servers available\\." {
		t.Errorf("expected 'No servers available\\.', got %q", text)
	}
	if len(keyboard.InlineKeyboard) != 0 {
		t.Errorf("expected empty keyboard, got %d rows", len(keyboard.InlineKeyboard))
	}
}

func TestBuildServersPageText(t *testing.T) {
	servers := make([]vpnconfig.Server, 47)
	for i := range servers {
		servers[i] = vpnconfig.Server{
			Name:    fmt.Sprintf("Server-%d, City", i+1),
			Address: fmt.Sprintf("srv%d.example.com", i+1),
			IPs:     []string{fmt.Sprintf("1.2.3.%d", i+1)},
		}
	}

	tests := []struct {
		name          string
		page          int
		expectHeader  string
		expectFirst   string
		expectLast    string
		expectButtons int // number of navigation buttons
	}{
		{
			name:          "first page",
			page:          0,
			expectHeader:  "🖥 *Servers* \\(47\\), page 1/4:",
			expectFirst:   "1\\. Server\\-1",
			expectLast:    "15\\. Server\\-15",
			expectButtons: 2, // [1/4] [Next →]
		},
		{
			name:          "middle page",
			page:          1,
			expectHeader:  "🖥 *Servers* \\(47\\), page 2/4:",
			expectFirst:   "16\\. Server\\-16",
			expectLast:    "30\\. Server\\-30",
			expectButtons: 3, // [← Prev] [2/4] [Next →]
		},
		{
			name:          "last page",
			page:          3,
			expectHeader:  "🖥 *Servers* \\(47\\), page 4/4:",
			expectFirst:   "46\\. Server\\-46",
			expectLast:    "47\\. Server\\-47",
			expectButtons: 2, // [← Prev] [4/4]
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, keyboard := buildServersPage(servers, tt.page)

			if !strings.Contains(text, tt.expectHeader) {
				t.Errorf("page text should contain %q, got:\n%s", tt.expectHeader, text)
			}
			if !strings.Contains(text, tt.expectFirst) {
				t.Errorf("page text should contain first server %q", tt.expectFirst)
			}
			if !strings.Contains(text, tt.expectLast) {
				t.Errorf("page text should contain last server %q", tt.expectLast)
			}

			if len(keyboard.InlineKeyboard) != 1 {
				t.Errorf("expected 1 keyboard row, got %d", len(keyboard.InlineKeyboard))
			}
			if len(keyboard.InlineKeyboard[0]) != tt.expectButtons {
				t.Errorf("expected %d buttons, got %d", tt.expectButtons, len(keyboard.InlineKeyboard[0]))
			}
		})
	}
}

func TestBuildServersPage_BoundaryPages(t *testing.T) {
	servers := []vpnconfig.Server{
		{Name: "Server-1, City", Address: "srv1.example.com", IPs: []string{"1.2.3.1"}},
	}

	// Negative page should be clamped to 0
	text, _ := buildServersPage(servers, -5)
	if !strings.Contains(text, "page 1/1") {
		t.Errorf("negative page should be clamped to 1, got: %s", text)
	}

	// Page beyond total should be clamped to last
	text, _ = buildServersPage(servers, 100)
	if !strings.Contains(text, "page 1/1") {
		t.Errorf("page beyond total should be clamped to last, got: %s", text)
	}
}

// Tests for HandleServers

func TestServersHandler_HandleServers_Empty(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{servers: []vpnconfig.Server{}}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}}
	h.HandleServers(msg)

	if sender.lastChatID != 123 {
		t.Errorf("expected chatID 123, got %d", sender.lastChatID)
	}
	// When no servers, we send a plain message (not with keyboard)
	if !strings.Contains(sender.lastText, "No servers") {
		t.Errorf("expected 'No servers' message, got %q", sender.lastText)
	}
}

func TestServersHandler_HandleServers_WithServers(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := []vpnconfig.Server{
		{Name: "Germany, Berlin", Address: "de.example.com", IPs: []string{"1.1.1.1"}},
		{Name: "USA, New York", Address: "us.example.com", IPs: []string{"2.2.2.2"}},
	}
	config := &mockConfigStore{servers: servers}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}}
	h.HandleServers(msg)

	if sender.lastChatID != 456 {
		t.Errorf("expected chatID 456, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "Servers") {
		t.Errorf("expected 'Servers' in text, got %q", sender.lastText)
	}
	if !strings.Contains(sender.lastText, "Germany") {
		t.Errorf("expected 'Germany' in text, got %q", sender.lastText)
	}
	// Should have keyboard buttons
	if len(sender.lastKeyboard.InlineKeyboard) == 0 {
		t.Error("expected keyboard to be set")
	}
}

func TestServersHandler_HandleServers_LoadError(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{err: errors.New("config error")}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleServers(msg)

	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "config error") {
		t.Errorf("expected error message, got %q", sender.lastText)
	}
}

// Tests for HandleCallback (pagination)

func TestServersHandler_HandleCallback_PageNavigation(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	servers := make([]vpnconfig.Server, 47)
	for i := range servers {
		servers[i] = vpnconfig.Server{
			Name:    fmt.Sprintf("Server-%d, City", i+1),
			Address: fmt.Sprintf("srv%d.example.com", i+1),
			IPs:     []string{fmt.Sprintf("1.2.3.%d", i+1)},
		}
	}
	config := &mockConfigStore{servers: servers}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	// Simulate callback for page 2
	cb := &tgbotapi.CallbackQuery{
		ID:   "callback123",
		Data: "servers:page:1",
		Message: &tgbotapi.Message{
			MessageID: 42,
			Chat:      &tgbotapi.Chat{ID: 100},
		},
	}
	h.HandleCallback(cb)

	// Should acknowledge callback
	if sender.lastAckID != "callback123" {
		t.Errorf("expected callback to be acknowledged, got %q", sender.lastAckID)
	}
	// Should edit message with page 2 content
	if sender.lastChatID != 100 {
		t.Errorf("expected chatID 100, got %d", sender.lastChatID)
	}
	if sender.lastMsgID != 42 {
		t.Errorf("expected msgID 42, got %d", sender.lastMsgID)
	}
	if !strings.Contains(sender.lastText, "page 2/4") {
		t.Errorf("expected page 2/4 in text, got %q", sender.lastText)
	}
}

func TestServersHandler_HandleCallback_NoopButton(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{servers: []vpnconfig.Server{}}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	// Simulate noop callback (clicking on page indicator)
	cb := &tgbotapi.CallbackQuery{
		ID:   "callback456",
		Data: "servers:noop",
		Message: &tgbotapi.Message{
			MessageID: 10,
			Chat:      &tgbotapi.Chat{ID: 200},
		},
	}
	h.HandleCallback(cb)

	// Should acknowledge callback
	if sender.lastAckID != "callback456" {
		t.Errorf("expected callback to be acknowledged, got %q", sender.lastAckID)
	}
	// Should NOT edit message (lastMsgID stays 0)
	if sender.lastMsgID != 0 {
		t.Errorf("expected no message edit for noop, got msgID %d", sender.lastMsgID)
	}
}

func TestServersHandler_HandleCallback_LoadError(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{err: errors.New("load error")}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	cb := &tgbotapi.CallbackQuery{
		ID:   "callback789",
		Data: "servers:page:0",
		Message: &tgbotapi.Message{
			MessageID: 20,
			Chat:      &tgbotapi.Chat{ID: 300},
		},
	}
	h.HandleCallback(cb)

	// Should acknowledge callback
	if sender.lastAckID != "callback789" {
		t.Errorf("expected callback to be acknowledged")
	}
	// On error, we don't edit the message
	if sender.lastMsgID != 0 {
		t.Errorf("expected no message edit on error")
	}
}

func TestServersHandler_HandleCallback_NilMessage(t *testing.T) {
	sender := &mockSenderWithKeyboard{}
	config := &mockConfigStore{}

	deps := &Deps{Sender: sender, Config: config}
	h := NewServersHandler(deps)

	// Inline callbacks may have nil Message
	cb := &tgbotapi.CallbackQuery{
		ID:      "callback_nil",
		Data:    "servers:page:0",
		Message: nil,
	}

	// Should not panic
	h.HandleCallback(cb)

	// Should still acknowledge callback
	if sender.lastAckID != "callback_nil" {
		t.Errorf("expected callback to be acknowledged even with nil Message")
	}
}

// A subscription now mixes protocols; the list says which one each server
// runs on.
func TestBuildServersPage_ShowsTheProtocol(t *testing.T) {
	servers := []vpnconfig.Server{
		{Name: "Oslo", Address: "oslo.example.com", IPs: []string{"1.2.3.4"},
			Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"ws","security":"tls"}}`)},
		{Name: "Canada SS", Address: "ss.example.com", IPs: []string{"5.6.7.8"},
			Outbound: json.RawMessage(`{"protocol":"shadowsocks"}`)},
	}
	text, _ := buildServersPage(servers, 0)
	if !strings.Contains(text, "1\\. Oslo — oslo\\.example\\.com \\(1\\.2\\.3\\.4\\) · vless·ws·tls") {
		t.Errorf("text %q", text)
	}
	if !strings.Contains(text, "2\\. Canada SS — ss\\.example\\.com \\(5\\.6\\.7\\.8\\) · ss") {
		t.Errorf("text %q", text)
	}
}
