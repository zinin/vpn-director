// internal/handler/misc_test.go
package handler

import (
	"errors"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

type mockSender struct {
	lastChatID      int64
	lastText        string
	lastCodeHeader  string
	lastCodeContent string
}

func (m *mockSender) Send(chatID int64, text string) error {
	m.lastChatID = chatID
	m.lastText = text
	return nil
}
func (m *mockSender) SendPlain(chatID int64, text string) error     { return nil }
func (m *mockSender) SendLongPlain(chatID int64, text string) error { return nil }
func (m *mockSender) SendWithKeyboard(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	return nil
}
func (m *mockSender) SendCodeBlock(chatID int64, header, content string) error {
	m.lastChatID = chatID
	m.lastCodeHeader = header
	m.lastCodeContent = content
	return nil
}
func (m *mockSender) EditMessage(chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) error {
	return nil
}
func (m *mockSender) AckCallback(callbackID string) error { return nil }

type mockNetworkInfo struct {
	ip  string
	err error
}

func (m *mockNetworkInfo) GetExternalIP() (string, error) {
	return m.ip, m.err
}

type mockLogReader struct {
	output string
	err    error
	calls  []logReadCall
}

type logReadCall struct {
	path  string
	lines int
}

func (m *mockLogReader) Read(path string, lines int) (string, error) {
	m.calls = append(m.calls, logReadCall{path: path, lines: lines})
	return m.output, m.err
}

func TestMiscHandler_HandleVersion(t *testing.T) {
	sender := &mockSender{}
	deps := &Deps{
		Sender:      sender,
		Version:     "v1.0.0",
		VersionFull: "v1.0.0-5-gabc1234",
		Commit:      "abc1234",
		BuildDate:   "2026-01-30",
	}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}}
	h.HandleVersion(msg)

	if sender.lastChatID != 123 {
		t.Errorf("expected chatID 123, got %d", sender.lastChatID)
	}
	expected := telegram.EscapeMarkdownV2("v1.0.0-5-gabc1234 (abc1234, 2026-01-30)")
	if sender.lastText != expected {
		t.Errorf("expected %q, got %q", expected, sender.lastText)
	}
}

// With a path manager behind it, /version names the path the bot is using.
func TestMiscHandler_HandleVersionShowsTelegramPath(t *testing.T) {
	sender := &mockSender{}
	deps := &Deps{
		Sender:       sender,
		Version:      "v1.0.0",
		VersionFull:  "v1.0.0-5-gabc1234",
		Commit:       "abc1234",
		BuildDate:    "2026-01-30",
		TelegramPath: func() string { return "tunnel:ovpnc2" },
	}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}}
	h.HandleVersion(msg)

	expected := telegram.EscapeMarkdownV2("v1.0.0-5-gabc1234 (abc1234, 2026-01-30)\nTelegram API path: tunnel:ovpnc2")
	if sender.lastText != expected {
		t.Errorf("expected %q, got %q", expected, sender.lastText)
	}
}

func TestMiscHandler_HandleStart(t *testing.T) {
	sender := &mockSender{}
	deps := &Deps{Sender: sender}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}}
	h.HandleStart(msg)

	if sender.lastChatID != 456 {
		t.Errorf("expected chatID 456, got %d", sender.lastChatID)
	}
	if sender.lastText == "" {
		t.Error("expected non-empty start message")
	}
}

func TestMiscHandler_HandleIP_Success(t *testing.T) {
	sender := &mockSender{}
	network := &mockNetworkInfo{ip: "1.2.3.4"}
	deps := &Deps{Sender: sender, Network: network}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleIP(msg)

	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}
	expected := "\U0001F310 External IP: `1.2.3.4`"
	if sender.lastText != expected {
		t.Errorf("expected %q, got %q", expected, sender.lastText)
	}
}

func TestMiscHandler_HandleIP_Error(t *testing.T) {
	sender := &mockSender{}
	network := &mockNetworkInfo{err: errors.New("network error")}
	deps := &Deps{Sender: sender, Network: network}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleIP(msg)

	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}
	expected := telegram.EscapeMarkdownV2("network error")
	if sender.lastText != expected {
		t.Errorf("expected %q, got %q", expected, sender.lastText)
	}
}

func TestMiscHandler_HandleLogs_DefaultArgs(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log line 1\nlog line 2"}
	testPaths := paths.Paths{
		BotLogPath:    "/tmp/bot.log",
		VPNLogPath:    "/tmp/vpn.log",
		XrayLogPath:   "/tmp/xray-error.log",
		WebUILogPath:  "/tmp/webui.log",
		WatchdLogPath: "/tmp/vpn-director-watchd.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	// Default is "all" which reads bot, vpn, xray, webui and watchd logs
	if len(logReader.calls) != 5 {
		t.Fatalf("expected 5 log read calls, got %d", len(logReader.calls))
	}
	// Check first call (bot logs)
	if logReader.calls[0].path != "/tmp/bot.log" {
		t.Errorf("expected bot log path, got %q", logReader.calls[0].path)
	}
	if logReader.calls[0].lines != 20 {
		t.Errorf("expected 20 lines, got %d", logReader.calls[0].lines)
	}
	// Check second call (vpn logs)
	if logReader.calls[1].path != "/tmp/vpn.log" {
		t.Errorf("expected vpn log path, got %q", logReader.calls[1].path)
	}
	// Check third call (xray error log)
	if logReader.calls[2].path != "/tmp/xray-error.log" {
		t.Errorf("expected xray log path, got %q", logReader.calls[2].path)
	}
	// Check fourth call (webui log)
	if logReader.calls[3].path != "/tmp/webui.log" {
		t.Errorf("expected webui log path, got %q", logReader.calls[3].path)
	}
	// Check fifth call (the server monitor's log)
	if logReader.calls[4].path != "/tmp/vpn-director-watchd.log" {
		t.Errorf("expected watchd log path, got %q", logReader.calls[4].path)
	}
}

func TestMiscHandler_HandleLogs_SourceBot(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "bot log"}
	testPaths := paths.Paths{
		BotLogPath: "/tmp/bot.log",
		VPNLogPath: "/tmp/vpn.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs bot",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	if logReader.calls[0].path != "/tmp/bot.log" {
		t.Errorf("expected bot log path, got %q", logReader.calls[0].path)
	}
}

func TestMiscHandler_HandleLogs_SourceVPN(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "vpn log"}
	testPaths := paths.Paths{
		BotLogPath: "/tmp/bot.log",
		VPNLogPath: "/tmp/vpn.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs vpn",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	if logReader.calls[0].path != "/tmp/vpn.log" {
		t.Errorf("expected vpn log path, got %q", logReader.calls[0].path)
	}
}

func TestMiscHandler_HandleLogs_SourceXray(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "xray warning"}
	testPaths := paths.Paths{
		BotLogPath:  "/tmp/bot.log",
		VPNLogPath:  "/tmp/vpn.log",
		XrayLogPath: "/tmp/xray-error.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs xray",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	if logReader.calls[0].path != "/tmp/xray-error.log" {
		t.Errorf("expected xray log path, got %q", logReader.calls[0].path)
	}
}

func TestMiscHandler_HandleLogs_SourceWebUI(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	testPaths := paths.Paths{
		BotLogPath:   "/tmp/bot.log",
		VPNLogPath:   "/tmp/vpn.log",
		XrayLogPath:  "/tmp/xray-error.log",
		WebUILogPath: "/tmp/webui.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs webui",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	if logReader.calls[0].path != "/tmp/webui.log" {
		t.Errorf("expected webui log path, got %q", logReader.calls[0].path)
	}
}

func TestMiscHandler_HandleLogs_WithLines(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	testPaths := paths.Paths{
		BotLogPath: "/tmp/bot.log",
		VPNLogPath: "/tmp/vpn.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs bot 50",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	if logReader.calls[0].lines != 50 {
		t.Errorf("expected 50 lines, got %d", logReader.calls[0].lines)
	}
}

func TestMiscHandler_HandleLogs_LinesOnly(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	testPaths := paths.Paths{
		BotLogPath:   "/tmp/bot.log",
		VPNLogPath:   "/tmp/vpn.log",
		XrayLogPath:  "/tmp/xray-error.log",
		WebUILogPath: "/tmp/webui.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs 30",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	// When only a number is given, source defaults to "all"
	if len(logReader.calls) != 5 {
		t.Fatalf("expected 5 log read calls, got %d", len(logReader.calls))
	}
	if logReader.calls[0].lines != 30 {
		t.Errorf("expected 30 lines, got %d", logReader.calls[0].lines)
	}
}

func TestMiscHandler_HandleLogs_MaxLinesLimit(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	testPaths := paths.Paths{
		BotLogPath: "/tmp/bot.log",
		VPNLogPath: "/tmp/vpn.log",
	}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: testPaths}
	h := NewMiscHandler(deps)

	// Request more than maxLogLines (500)
	msg := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 100},
		Text: "/logs bot 99999",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 5},
		},
	}
	h.HandleLogs(msg)

	if len(logReader.calls) != 1 {
		t.Fatalf("expected 1 log read call, got %d", len(logReader.calls))
	}
	// Should be capped at maxLogLines (500)
	if logReader.calls[0].lines != 500 {
		t.Errorf("expected 500 lines (maxLogLines), got %d", logReader.calls[0].lines)
	}
}

func TestMiscHandler_HandleLogs_SourceWatchd(t *testing.T) {
	sender := &mockSender{}
	logReader := &mockLogReader{output: "log"}
	deps := &Deps{Sender: sender, Logs: logReader, Paths: paths.Paths{WatchdLogPath: "/tmp/vpn-director-watchd.log"}}
	h := NewMiscHandler(deps)

	h.HandleLogs(&tgbotapi.Message{
		Chat:     &tgbotapi.Chat{ID: 100},
		Text:     "/logs watchd",
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}},
	})

	if len(logReader.calls) != 1 || logReader.calls[0].path != "/tmp/vpn-director-watchd.log" {
		t.Fatalf("log reads %+v, want the monitor's log", logReader.calls)
	}
}

func TestMiscHandler_HandleLogs_WatchdGolden(t *testing.T) {
	for _, tc := range []struct {
		command string
		lines   int
		content string
		want    string
	}{
		{"/logs watchd", 20, "monitor started\nstate=ok", "📋 *Server monitor logs* \\(last 20 lines\\):\nmonitor started\nstate=ok"},
		{"/logs watchd 50", 50, "monitor started", "📋 *Server monitor logs* \\(last 50 lines\\):\nmonitor started"},
		{"/logs watchd 99999", 500, "monitor started", "📋 *Server monitor logs* \\(last 500 lines\\):\nmonitor started"},
		{"/logs watchd", 20, "", "📋 *Server monitor logs* \\(last 20 lines\\):\n(empty)"},
	} {
		t.Run(tc.command+tc.content, func(t *testing.T) {
			sender := &recordingSender{}
			reader := &mockLogReader{output: tc.content}
			h := NewMiscHandler(&Deps{Sender: sender, Logs: reader, Paths: paths.Paths{WatchdLogPath: "/tmp/vpn-director-watchd.log"}})
			h.HandleLogs(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}, Text: tc.command, Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}}})
			if got := sender.last(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if len(reader.calls) != 1 || reader.calls[0] != (logReadCall{path: "/tmp/vpn-director-watchd.log", lines: tc.lines}) {
				t.Fatalf("log reads %+v", reader.calls)
			}
		})
	}
}

func TestMiscHandler_HandleLogs_AllGolden(t *testing.T) {
	for _, command := range []string{"/logs", "/logs all"} {
		t.Run(command, func(t *testing.T) {
			sender := &recordingSender{}
			reader := &mockLogReader{output: "log"}
			h := NewMiscHandler(&Deps{Sender: sender, Logs: reader, Paths: paths.Paths{
				BotLogPath: "/tmp/bot.log", VPNLogPath: "/tmp/vpn.log", XrayLogPath: "/tmp/xray-error.log", WebUILogPath: "/tmp/webui.log", WatchdLogPath: "/tmp/vpn-director-watchd.log",
			}})
			h.HandleLogs(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}, Text: command, Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}}})
			const want = `📋 *Bot logs* \(last 20 lines\):
log
📋 *VPN Director logs* \(last 20 lines\):
log
📋 *Xray logs* \(last 20 lines\):
log
📋 *Web UI logs* \(last 20 lines\):
log
📋 *Server monitor logs* \(last 20 lines\):
log`
			if got := sender.all(); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			wantPaths := []string{"/tmp/bot.log", "/tmp/vpn.log", "/tmp/xray-error.log", "/tmp/webui.log", "/tmp/vpn-director-watchd.log"}
			if len(reader.calls) != len(wantPaths) {
				t.Fatalf("log reads %+v", reader.calls)
			}
			for i, path := range wantPaths {
				if reader.calls[i] != (logReadCall{path: path, lines: 20}) {
					t.Errorf("log read %d: %+v", i, reader.calls[i])
				}
			}
		})
	}
}

func TestMiscHandler_HandleLogs_WatchdReadError(t *testing.T) {
	sender := &mockSender{}
	reader := &mockLogReader{err: errors.New("disk [offline]")}
	h := NewMiscHandler(&Deps{Sender: sender, Logs: reader, Paths: paths.Paths{WatchdLogPath: "/tmp/vpn-director-watchd.log"}})
	h.HandleLogs(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}, Text: "/logs watchd", Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}}})
	if want := `Error reading Server monitor logs: disk \[offline\]`; sender.lastText != want {
		t.Fatalf("got %q, want %q", sender.lastText, want)
	}
}

func TestMiscHandler_HandleLogs_UsageIncludesWatchd(t *testing.T) {
	sender := &mockSender{}
	h := NewMiscHandler(&Deps{Sender: sender})
	h.HandleLogs(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 100}, Text: "/logs invalid", Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}}})
	if want := "Usage: `/logs [bot|vpn|xray|webui|watchd|all] [lines]`"; sender.lastText != want {
		t.Fatalf("got %q, want %q", sender.lastText, want)
	}
}
