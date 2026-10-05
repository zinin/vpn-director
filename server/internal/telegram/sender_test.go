package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// MockBotAPI implements the minimal interface needed for testing
type MockBotAPI struct {
	SentMessages []tgbotapi.Chattable
	LastError    error
}

func (m *MockBotAPI) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	m.SentMessages = append(m.SentMessages, c)
	return tgbotapi.Message{}, m.LastError
}

func (m *MockBotAPI) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	m.SentMessages = append(m.SentMessages, c)
	return &tgbotapi.APIResponse{Ok: true}, m.LastError
}

func TestSender_Send(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	err := sender.Send(123, "Hello \\*world\\*")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(mock.SentMessages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.SentMessages))
	}

	msg, ok := mock.SentMessages[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("expected MessageConfig, got %T", mock.SentMessages[0])
	}
	if msg.ChatID != 123 {
		t.Errorf("expected chatID 123, got %d", msg.ChatID)
	}
	if msg.ParseMode != "MarkdownV2" {
		t.Errorf("expected MarkdownV2, got %s", msg.ParseMode)
	}
}

func TestSender_SendPlain(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	err := sender.SendPlain(123, "Plain text")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	msg, ok := mock.SentMessages[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("expected MessageConfig, got %T", mock.SentMessages[0])
	}
	if msg.ParseMode != "" {
		t.Errorf("expected empty parse mode for plain, got %s", msg.ParseMode)
	}
}

func TestSender_SendCodeBlock(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	err := sender.SendCodeBlock(456, "Status:", "running\nok")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	msg, ok := mock.SentMessages[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("expected MessageConfig, got %T", mock.SentMessages[0])
	}
	if msg.ChatID != 456 {
		t.Errorf("expected chatID 456, got %d", msg.ChatID)
	}
	// Should contain code block markers
	if !strings.Contains(msg.Text, "```") {
		t.Errorf("expected code block, got %s", msg.Text)
	}
}

func TestSender_SendWithKeyboard(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	kb := NewKeyboard().Button("Test", "test").Row().Build()
	err := sender.SendWithKeyboard(456, "Pick one", kb)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	msg, ok := mock.SentMessages[0].(tgbotapi.MessageConfig)
	if !ok {
		t.Fatalf("expected MessageConfig, got %T", mock.SentMessages[0])
	}
	if msg.ReplyMarkup == nil {
		t.Error("expected keyboard, got nil")
	}
}

func TestSender_SendLongPlain(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	// Create a message longer than MaxMessageLength
	longText := strings.Repeat("a", MaxMessageLength+100)

	err := sender.SendLongPlain(123, longText)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should be split into 2 messages
	if len(mock.SentMessages) != 2 {
		t.Errorf("expected 2 messages for long text, got %d", len(mock.SentMessages))
	}
}

func TestSender_SendLongPlain_BreaksAtNewline(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	// Create text with newline in the middle
	part1 := strings.Repeat("a", MaxMessageLength-100)
	part2 := strings.Repeat("b", 200)
	longText := part1 + "\n" + part2

	err := sender.SendLongPlain(123, longText)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should be split into 2 messages, breaking at newline
	if len(mock.SentMessages) < 2 {
		t.Errorf("expected at least 2 messages, got %d", len(mock.SentMessages))
	}
}

func TestSender_EditMessage(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	kb := NewKeyboard().Button("OK", "ok").Build()
	err := sender.EditMessage(123, 456, "Updated", kb)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(mock.SentMessages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.SentMessages))
	}
}

func TestSender_AckCallback(t *testing.T) {
	mock := &MockBotAPI{}
	sender := NewSender(mock)

	err := sender.AckCallback("callback123")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(mock.SentMessages) != 1 {
		t.Fatalf("expected 1 request, got %d", len(mock.SentMessages))
	}
}

func TestSender_SendPlainSafeErrorLogging(t *testing.T) {
	const token = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"
	const providerPayload = "synthetic provider payload: https://provider.example.test/subscription?secret=fixture-provider-secret"
	transportError := &url.Error{
		Op:  "Post",
		URL: "https://api.example.test/bot" + token + "/sendMessage",
		Err: errors.New("synthetic transport payload"),
	}
	forbidden := &tgbotapi.Error{Code: 403, Message: providerPayload}
	rateLimit := &tgbotapi.Error{Code: 429, Message: providerPayload}
	serverError := &tgbotapi.Error{Code: 502, Message: providerPayload}
	for _, tc := range []struct {
		name      string
		err       error
		transport *url.Error
		provider  *tgbotapi.Error
	}{
		{"token_bearing_url", transportError, transportError, nil},
		{"permanent_provider_payload", forbidden, nil, forbidden},
		{"retryable_provider_payload", rateLimit, nil, rateLimit},
		{"wrapped_provider_payload", fmt.Errorf("synthetic wrapper payload: %w", serverError), nil, serverError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&captured, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			sender := NewSender(&MockBotAPI{LastError: tc.err})
			err := sender.SendPlain(123, "safe notification")
			if err != tc.err || !errors.Is(err, tc.err) {
				t.Fatal("SendPlain changed the original returned error identity")
			}
			if tc.transport != nil {
				var got *url.Error
				if !errors.As(err, &got) || got != tc.transport || !errors.Is(err, tc.transport.Err) {
					t.Error("SendPlain lost transport error matching or its cause")
				}
			}
			if tc.provider != nil {
				var got *tgbotapi.Error
				if !errors.As(err, &got) || got != tc.provider || !errors.Is(err, tc.provider) {
					t.Error("SendPlain changed Telegram error matching or retry/permanent classification")
				}
			}
			var record struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
				ChatID  int64  `json:"chat_id"`
			}
			if err := json.Unmarshal(captured.Bytes(), &record); err != nil {
				t.Fatal("SendPlain did not retain a structured error diagnostic")
			}
			if record.Level != "ERROR" || record.Message == "" || record.ChatID != 123 {
				t.Fatalf("error diagnostic lost level/operation/chat metadata: %+v", record)
			}
			for _, private := range []struct {
				name string
				text string
			}{
				{"Telegram token", token},
				{"token-bearing URL", transportError.URL},
				{"transport payload", "synthetic transport payload"},
				{"provider payload", providerPayload},
				{"provider URL", "https://provider.example.test/subscription"},
				{"provider credential", "fixture-provider-secret"},
				{"wrapper payload", "synthetic wrapper payload"},
			} {
				if strings.Contains(captured.String(), private.text) {
					t.Errorf("SendPlain error log exposes the synthetic %s", private.name)
				}
			}
		})
	}
}
