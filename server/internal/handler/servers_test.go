// internal/handler/servers_test.go
package handler

import (
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

func TestBuildServersPage_HeadsEachSubscriptionAndNumbersWithinIt(t *testing.T) {
	subs := []vpnconfig.Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(14)},
		{ID: "1b2c3d4e", Name: "Beta", Servers: servers(3)},
	}

	text, kb := buildServersPage(serverLines(subs), len(subs), 0)

	if !strings.Contains(text, "*Alpha*") || !strings.Contains(text, "*Beta*") || !strings.Contains(text, "in 2 subscriptions, page 1/2") {
		t.Fatalf("page 1: %q", text)
	}
	// Beta's first server is number 1 of Beta, on the page after Alpha's 14.
	if !strings.Contains(text, "\n1\\. S1") || strings.Count(text, "S1 —") != 2 {
		t.Fatalf("page 1 numbering: %q", text)
	}
	if len(kb.InlineKeyboard) != 1 {
		t.Fatalf("navigation %v", kb.InlineKeyboard)
	}

	// A page that starts in the middle of a subscription names it again.
	text, _ = buildServersPage(serverLines(subs), len(subs), 1)
	if !strings.Contains(text, "*Beta*") || !strings.Contains(text, "3\\. S3") {
		t.Fatalf("page 2: %q", text)
	}
}

func TestServersHandler_NoServer(t *testing.T) {
	sender := &recordingSender{}
	h := NewServersHandler(&Deps{Sender: sender, Config: newSubsStore()})

	h.HandleServers(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}})

	if !strings.Contains(sender.last(), "/import") {
		t.Fatalf("reply %q", sender.last())
	}
}

func TestServersHandler_PageButtonsTurnThePage(t *testing.T) {
	store := newSubsStore(vpnconfig.Subscription{ID: "0a1b2c3d", Name: "Alpha", Servers: servers(20)})
	sender := &recordingSender{}
	h := NewServersHandler(&Deps{Sender: sender, Config: store})

	h.HandleCallback(&tgbotapi.CallbackQuery{ID: "cb", Data: "servers:page:1", Message: &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}}})

	if !strings.Contains(sender.last(), "page 2/2") || !strings.Contains(sender.last(), "16\\. S16") {
		t.Fatalf("page %q", sender.last())
	}
}
