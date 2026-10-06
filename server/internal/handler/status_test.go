// internal/handler/status_test.go
package handler

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type mockVPNDirector struct {
	statusOutput  string
	statusErr     error
	restartErr    error
	stopErr       error
	restartCalled bool
}

func (m *mockVPNDirector) Status() (string, error) { return m.statusOutput, m.statusErr }
func (m *mockVPNDirector) Apply() error            { return nil }
func (m *mockVPNDirector) Restart() error          { return m.restartErr }
func (m *mockVPNDirector) RestartXray() error      { m.restartCalled = true; return nil }
func (m *mockVPNDirector) Stop() error             { return m.stopErr }
func (m *mockVPNDirector) Update() error           { return nil }
func (m *mockVPNDirector) Platform() (vpnconfig.PlatformInfo, error) {
	return vpnconfig.PlatformInfo{}, nil
}

func TestStatusHandler_HandleStatus(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{statusOutput: "Xray: running\nTunnel: active"}

	deps := &Deps{
		Sender: sender,
		VPN:    vpn,
	}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}}
	h.HandleStatus(msg)

	if sender.lastChatID != 456 {
		t.Errorf("expected chatID 456, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastCodeHeader, "VPN Director Status") {
		t.Errorf("expected header to contain 'VPN Director Status', got %q", sender.lastCodeHeader)
	}
	if sender.lastCodeContent != "Xray: running\nTunnel: active" {
		t.Errorf("expected code content to match mock output, got %q", sender.lastCodeContent)
	}
}

func TestStatusHandler_HandleStatus_Error(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{statusErr: errors.New("exec failed")}

	deps := &Deps{
		Sender: sender,
		VPN:    vpn,
	}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}}
	h.HandleStatus(msg)

	if sender.lastChatID != 123 {
		t.Errorf("expected chatID 123, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "exec failed") {
		t.Errorf("expected error message to contain 'exec failed', got %q", sender.lastText)
	}
}

func TestStatusHandler_HandleRestart(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{}

	deps := &Deps{Sender: sender, VPN: vpn}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleRestart(msg)

	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "VPN Director restarted") {
		t.Errorf("expected success message to contain 'VPN Director restarted', got %q", sender.lastText)
	}
}

func TestStatusHandler_HandleRestart_Error(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{restartErr: errors.New("restart failed")}

	deps := &Deps{Sender: sender, VPN: vpn}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 789}}
	h.HandleRestart(msg)

	if sender.lastChatID != 789 {
		t.Errorf("expected chatID 789, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "restart failed") {
		t.Errorf("expected error message to contain 'restart failed', got %q", sender.lastText)
	}
}

func TestStatusHandler_HandleStop(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{}

	deps := &Deps{Sender: sender, VPN: vpn}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 111}}
	h.HandleStop(msg)

	if sender.lastChatID != 111 {
		t.Errorf("expected chatID 111, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "VPN Director stopped") {
		t.Errorf("expected success message to contain 'VPN Director stopped', got %q", sender.lastText)
	}
}

func TestStatusHandler_HandleStop_Error(t *testing.T) {
	sender := &mockSender{}
	vpn := &mockVPNDirector{stopErr: errors.New("stop failed")}

	deps := &Deps{Sender: sender, VPN: vpn}
	h := NewStatusHandler(deps)

	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 222}}
	h.HandleStop(msg)

	if sender.lastChatID != 222 {
		t.Errorf("expected chatID 222, got %d", sender.lastChatID)
	}
	if !strings.Contains(sender.lastText, "stop failed") {
		t.Errorf("expected error message to contain 'stop failed', got %q", sender.lastText)
	}
}

type statusMessageAPI struct {
	messages []tgbotapi.MessageConfig
}

func (a *statusMessageAPI) Send(chattable tgbotapi.Chattable) (tgbotapi.Message, error) {
	message, ok := chattable.(tgbotapi.MessageConfig)
	if !ok {
		return tgbotapi.Message{}, errors.New("unexpected Telegram request in status fixture")
	}
	a.messages = append(a.messages, message)
	return tgbotapi.Message{}, nil
}

func (*statusMessageAPI) Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func plainAutomationMessages(t *testing.T, api *statusMessageAPI, chatID int64) string {
	t.Helper()
	var plain []string
	for _, message := range api.messages {
		if message.ChatID != chatID {
			t.Fatalf("status sent to chat %d, want %d", message.ChatID, chatID)
		}
		if message.ParseMode == "" {
			if !utf8.ValidString(message.Text) || utf8.RuneCountInString(message.Text) > telegram.MaxMessageLength {
				t.Fatalf("automation message is not bounded UTF-8: bytes=%d runes=%d", len(message.Text), utf8.RuneCountInString(message.Text))
			}
			plain = append(plain, message.Text)
		}
	}
	if len(plain) == 0 {
		t.Fatal("/status sent no separate plain automation section")
	}
	return strings.Join(plain, "\n")
}

func TestStatus_MonitorAndWatchPresentation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shellErr error
	}{
		{"successful shell status", nil},
		{"failed shell status still shows automation", errors.New("exec failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &statusMessageAPI{}
			deps := &Deps{
				Sender:  telegram.NewSender(api),
				VPN:     &mockVPNDirector{statusOutput: "Xray: running\nTunnel: active", statusErr: tc.shellErr},
				Monitor: &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}},
				Watch: automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
					return watchdapi.WatchSnapshot{
						State: watchdapi.WatchActive, Action: "walking",
						Notifications: watchdapi.NotificationsStatus{Pending: 2, StorageError: "cannot write notification storage"},
					}, nil
				}),
			}
			NewStatusHandler(deps).HandleStatus(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}})
			plain := plainAutomationMessages(t, api, 456)
			assertIndependentAutomationStates(t, plain, "disabled", "active")
			assertAutomationQueueHealth(t, plain, "2", "cannot write notification storage")
			var codeBlocks, shellErrors int
			for _, message := range api.messages {
				if strings.Contains(message.Text, "```\nXray: running\nTunnel: active```") {
					codeBlocks++
					if message.ParseMode != "MarkdownV2" || !strings.Contains(message.Text, "VPN Director Status") || strings.Contains(message.Text, "cannot write notification storage") {
						t.Fatalf("shell status block was replaced or mixed with automation: %+v", message)
					}
				}
				if strings.Contains(message.Text, "exec failed") {
					shellErrors++
				}
			}
			if tc.shellErr == nil && codeBlocks != 1 {
				t.Fatalf("/status retained %d shell code blocks, want exactly one", codeBlocks)
			}
			if tc.shellErr != nil && shellErrors != 1 {
				t.Fatalf("/status dropped or repeated the shell failure: %+v", api.messages)
			}
		})
	}

	t.Run("plain automation messages are bounded without breaking UTF-8", func(t *testing.T) {
		api := &statusMessageAPI{}
		deps := &Deps{
			Sender: telegram.NewSender(api), VPN: &mockVPNDirector{statusOutput: "Xray: running"},
			Monitor: &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}},
			Watch: automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
				return watchdapi.WatchSnapshot{State: watchdapi.WatchActive, Message: "Safe diagnostic [_plain_]: " + strings.Repeat("Ж", 5000)}, nil
			}),
		}
		NewStatusHandler(deps).HandleStatus(&tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}})
		plain := plainAutomationMessages(t, api, 456)
		assertIndependentAutomationStates(t, plain, "ok", "active")
		if strings.Contains(plain, `\[_plain_\]`) || strings.Contains(plain, `\_plain\_`) || strings.Contains(plain, "```") {
			t.Fatalf("automation text was treated as Markdown instead of plain text: %q", plain)
		}
	})
}

type statusManualVPN struct {
	mockVPNDirector
	restarts int
	stops    int
}

func (v *statusManualVPN) Restart() error { v.restarts++; return v.restartErr }
func (v *statusManualVPN) Stop() error    { v.stops++; return v.stopErr }

type statusLogFiles struct {
	contents map[string]string
	calls    []logReadCall
}

func (l *statusLogFiles) Read(path string, lines int) (string, error) {
	l.calls = append(l.calls, logReadCall{path: path, lines: lines})
	if content, ok := l.contents[path]; ok {
		return content, nil
	}
	return "", errors.New("unknown log source")
}

func TestStatus_MonitorAndWatchIPCFailureKeepsManualAndLogs(t *testing.T) {
	t.Run("status restart and stop", func(t *testing.T) {
		api := &statusMessageAPI{}
		vpn := &statusManualVPN{mockVPNDirector: mockVPNDirector{statusOutput: "Xray: running\nTunnel: active"}}
		client := watchdapi.NewClient(filepath.Join(t.TempDir(), "missing.sock"))
		deps := &Deps{Sender: telegram.NewSender(api), VPN: vpn, Monitor: client, Watch: client}
		handler := NewStatusHandler(deps)
		message := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 456}}
		handler.HandleStatus(message)
		assertIndependentAutomationStates(t, plainAutomationMessages(t, api, 456), "not_running", "not_running")
		foundShell := false
		for _, sent := range api.messages {
			foundShell = foundShell || strings.Contains(sent.Text, "```\nXray: running\nTunnel: active```")
		}
		if !foundShell {
			t.Fatal("IPC failure removed the existing shell status")
		}
		api.messages = nil
		handler.HandleRestart(message)
		handler.HandleStop(message)
		if vpn.restarts != 1 || vpn.stops != 1 {
			t.Fatalf("IPC failure blocked manual operations: restart=%d stop=%d", vpn.restarts, vpn.stops)
		}
		var responses string
		for _, sent := range api.messages {
			responses += sent.Text + "\n"
		}
		if !strings.Contains(responses, "VPN Director restarted") || !strings.Contains(responses, "VPN Director stopped") {
			t.Fatalf("manual commands lost their success responses: %q", responses)
		}
	})

	for _, tc := range []struct {
		command string
		paths   []string
	}{
		{"/logs watchd", []string{"/tmp/status-watchd.log"}},
		{"/logs all", []string{"/tmp/status-bot.log", "/tmp/status-vpn.log", "/tmp/status-xray.log", "/tmp/status-webui.log", "/tmp/status-watchd.log"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			api := &statusMessageAPI{}
			logs := &statusLogFiles{contents: map[string]string{
				"/tmp/status-bot.log":    "bot fixture log",
				"/tmp/status-vpn.log":    "vpn fixture log",
				"/tmp/status-xray.log":   "xray fixture log",
				"/tmp/status-webui.log":  "webui fixture log",
				"/tmp/status-watchd.log": `level=INFO msg="Subscription refreshed" subscription=North`,
			}}
			client := watchdapi.NewClient(filepath.Join(t.TempDir(), "missing.sock"))
			deps := &Deps{
				Sender: telegram.NewSender(api), Logs: logs, Monitor: client, Watch: client,
				Paths: paths.Paths{
					BotLogPath: "/tmp/status-bot.log", VPNLogPath: "/tmp/status-vpn.log", XrayLogPath: "/tmp/status-xray.log",
					WebUILogPath: "/tmp/status-webui.log", WatchdLogPath: "/tmp/status-watchd.log",
				},
			}
			NewMiscHandler(deps).HandleLogs(&tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 456}, Text: tc.command,
				Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 5}},
			})
			var readPaths []string
			for _, call := range logs.calls {
				readPaths = append(readPaths, call.path)
				if call.lines != 20 {
					t.Fatalf("log source %q lost the default line bound: %d", call.path, call.lines)
				}
			}
			if !reflect.DeepEqual(readPaths, tc.paths) || len(api.messages) != len(tc.paths) {
				t.Fatalf("IPC failure changed log sources: paths=%v messages=%d, want %v", readPaths, len(api.messages), tc.paths)
			}
			for i, path := range tc.paths {
				message := api.messages[i]
				if message.ChatID != 456 || message.ParseMode != "MarkdownV2" || !strings.Contains(message.Text, "```\n"+logs.contents[path]+"```") {
					t.Fatalf("source %q lost its code block or automation log content: %+v", path, message)
				}
			}
		})
	}
}
