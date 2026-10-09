package telegram

import (
	"errors"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// BotAPI is the interface for Telegram bot API operations
type BotAPI interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
}

// MessageSender defines the interface for sending Telegram messages
type MessageSender interface {
	Send(chatID int64, text string) error
	SendPlain(chatID int64, text string) error
	SendLongPlain(chatID int64, text string) error
	SendWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) error
	SendCodeBlock(chatID int64, header, content string) error
	EditMessage(chatID int64, msgID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error
	AckCallback(callbackID string) error
}

// Sender implements MessageSender using Telegram Bot API
type Sender struct {
	api BotAPI
}

// NewSender creates a new Sender
func NewSender(api BotAPI) *Sender {
	return &Sender{api: api}
}

func senderErrorAttr(err error) slog.Attr {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) && apiErr != nil {
		return slog.Group("error", "kind", "telegram", "code", apiErr.Code)
	}
	return slog.Group("error", "kind", "transport")
}

// Send sends a MarkdownV2 formatted message
func (s *Sender) Send(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	_, err := s.api.Send(msg)
	if err != nil {
		slog.Error("Failed to send message", "chat_id", chatID, senderErrorAttr(err))
	}
	return err
}

// SendPlain sends a plain text message without formatting
func (s *Sender) SendPlain(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := s.api.Send(msg)
	if err != nil {
		slog.Error("Failed to send message", "chat_id", chatID, senderErrorAttr(err))
	}
	return err
}

// SendLongPlain sends a long plain-text message, splitting into chunks if needed.
// Uses rune-safe chunking to avoid breaking UTF-8 characters.
func (s *Sender) SendLongPlain(chatID int64, text string) error {
	runes := []rune(text)
	for len(runes) > 0 {
		// Calculate chunk size in runes
		chunkSize := len(runes)
		if chunkSize > MaxMessageLength {
			chunkSize = MaxMessageLength
		}

		chunk := string(runes[:chunkSize])

		// If we're splitting, try to break at newline for readability
		if chunkSize < len(runes) {
			if idx := lastIndexRune(runes[:chunkSize], '\n'); idx > chunkSize/2 {
				chunk = string(runes[:idx+1])
				chunkSize = idx + 1
			}
		}

		if err := s.SendPlain(chatID, chunk); err != nil {
			return err
		}
		runes = runes[chunkSize:]
	}
	return nil
}

func lastIndexRune(runes []rune, r rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// SendWithKeyboard sends a message with inline keyboard
func (s *Sender) SendWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	msg.ReplyMarkup = keyboard
	_, err := s.api.Send(msg)
	if err != nil {
		slog.Error("Failed to send message with keyboard", "chat_id", chatID, senderErrorAttr(err))
	}
	return err
}

// SendCodeBlock sends a message with code block formatting
func (s *Sender) SendCodeBlock(chatID int64, header, content string) error {
	text := BuildCodeBlockMessage(header, content, MaxMessageLength)
	return s.Send(chatID, text)
}

// EditMessage edits an existing message
func (s *Sender) EditMessage(chatID int64, msgID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, msgID, text, keyboard)
	edit.ParseMode = "MarkdownV2"
	_, err := s.api.Send(edit)
	if err != nil {
		slog.Error("Failed to edit message", "msg_id", msgID, senderErrorAttr(err))
	}
	return err
}

// AckCallback acknowledges a callback query
func (s *Sender) AckCallback(callbackID string) error {
	_, err := s.api.Request(tgbotapi.NewCallback(callbackID, ""))
	if err != nil {
		slog.Error("Failed to acknowledge callback", senderErrorAttr(err))
	}
	return err
}
