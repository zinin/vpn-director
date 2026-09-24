// internal/handler/status_test.go
package handler

import (
	"errors"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
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
