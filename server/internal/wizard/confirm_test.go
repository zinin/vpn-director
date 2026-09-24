package wizard

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestConfirmStep_Render(t *testing.T) {
	t.Run("shows server name, exclusions, and clients", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
				{Name: "Server2", IPs: []string{"5.6.7.8"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 1,
			Exclusions:  map[string]bool{"ru": true, "by": true, "ua": false},
			Clients: []ClientRoute{
				{IP: "192.168.1.10", Route: "xray"},
				{IP: "192.168.1.20", Route: "wgc1"},
			},
		}

		step.Render(123, state)

		if sender.lastChatID != 123 {
			t.Errorf("expected chatID 123, got %d", sender.lastChatID)
		}

		// Should contain step header
		if !strings.Contains(sender.lastText, "Step 4/4") {
			t.Errorf("expected 'Step 4/4' in text, got: %s", sender.lastText)
		}

		// Should contain server info (Server2 since index=1)
		// Note: text is MarkdownV2 escaped, so special chars like . and ( are escaped
		if !strings.Contains(sender.lastText, "Server2") {
			t.Errorf("expected 'Server2' in text, got: %s", sender.lastText)
		}
		if !strings.Contains(sender.lastText, "5\\.6\\.7\\.8") {
			t.Errorf("expected server IP '5.6.7.8' (escaped) in text, got: %s", sender.lastText)
		}

		// Should contain exclusions (sorted alphabetically: by, ru)
		if !strings.Contains(sender.lastText, "by") || !strings.Contains(sender.lastText, "ru") {
			t.Errorf("expected exclusions 'by, ru' in text, got: %s", sender.lastText)
		}

		// Should contain clients (IPs have escaped dots)
		if !strings.Contains(sender.lastText, "192\\.168\\.1\\.10") || !strings.Contains(sender.lastText, "xray") {
			t.Errorf("expected client '192.168.1.10 -> xray' in text, got: %s", sender.lastText)
		}
		if !strings.Contains(sender.lastText, "192\\.168\\.1\\.20") || !strings.Contains(sender.lastText, "wgc1") {
			t.Errorf("expected client '192.168.1.20 -> wgc1' in text, got: %s", sender.lastText)
		}

		// Should have keyboard with Apply and Cancel
		if sender.lastKeyboard == nil {
			t.Fatal("expected keyboard to be sent")
		}

		if len(sender.lastKeyboard.InlineKeyboard) != 1 {
			t.Errorf("expected 1 row of buttons, got %d", len(sender.lastKeyboard.InlineKeyboard))
		}

		row := sender.lastKeyboard.InlineKeyboard[0]
		if len(row) != 2 {
			t.Errorf("expected 2 buttons in row, got %d", len(row))
		}

		if row[0].Text != "Apply" || *row[0].CallbackData != "apply" {
			t.Errorf("expected Apply button, got: %s / %s", row[0].Text, *row[0].CallbackData)
		}
		if row[1].Text != "Cancel" || *row[1].CallbackData != "cancel" {
			t.Errorf("expected Cancel button, got: %s / %s", row[1].Text, *row[1].CallbackData)
		}
	})
}

func TestConfirmStep_Render_NoClients(t *testing.T) {
	t.Run("shows (none) when no clients configured", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 0,
			Exclusions:  map[string]bool{"ru": true},
			Clients:     []ClientRoute{}, // No clients
		}

		step.Render(123, state)

		// Should contain "(none)" for clients (escaped as \(none\))
		if !strings.Contains(sender.lastText, "\\(none\\)") {
			t.Errorf("expected '(none)' (escaped) for empty clients, got: %s", sender.lastText)
		}
	})
}

func TestConfirmStep_Render_NoExclusions(t *testing.T) {
	t.Run("does not show exclusions line when none selected", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 0,
			Exclusions:  map[string]bool{}, // No exclusions
			Clients: []ClientRoute{
				{IP: "192.168.1.10", Route: "xray"},
			},
		}

		step.Render(123, state)

		// Should NOT contain "Exclusions:" when none selected
		if strings.Contains(sender.lastText, "Exclusions:") {
			t.Errorf("expected no 'Exclusions:' line when empty, got: %s", sender.lastText)
		}

		// Should still contain other elements
		if !strings.Contains(sender.lastText, "Server1") {
			t.Errorf("expected 'Server1' in text, got: %s", sender.lastText)
		}
		if !strings.Contains(sender.lastText, "192\\.168\\.1\\.10") {
			t.Errorf("expected client in text (escaped), got: %s", sender.lastText)
		}
	})
}

func TestConfirmStep_Render_AllExclusionsFalse(t *testing.T) {
	t.Run("does not show exclusions line when all are false", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 0,
			Exclusions:  map[string]bool{"ru": false, "by": false},
			Clients: []ClientRoute{
				{IP: "192.168.1.10", Route: "xray"},
			},
		}

		step.Render(123, state)

		// Should NOT contain "Exclusions:" when all are false
		if strings.Contains(sender.lastText, "Exclusions:") {
			t.Errorf("expected no 'Exclusions:' line when all false, got: %s", sender.lastText)
		}
	})
}

func TestConfirmStep_Render_InvalidServerIndex(t *testing.T) {
	t.Run("handles server index out of bounds gracefully", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 5, // Out of bounds
			Exclusions:  map[string]bool{},
			Clients:     []ClientRoute{},
		}

		step.Render(123, state)

		// Should still send a message (no crash)
		if sender.lastChatID != 123 {
			t.Errorf("expected message to be sent, chatID=%d", sender.lastChatID)
		}

		// Should NOT contain server info
		if strings.Contains(sender.lastText, "Xray server:") {
			t.Errorf("expected no server info for invalid index, got: %s", sender.lastText)
		}
	})
}

func TestConfirmStep_HandleCallback(t *testing.T) {
	t.Run("returns without action for apply callback", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:     123,
			Step:       StepConfirm,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "apply",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		// Should not panic or change state
		step.HandleCallback(cb, state)

		// State should remain unchanged
		if state.GetStep() != StepConfirm {
			t.Errorf("expected step to remain %s, got %s", StepConfirm, state.GetStep())
		}
	})

	t.Run("returns without action for cancel callback", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:     123,
			Step:       StepConfirm,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "cancel",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		// Should not panic or change state
		step.HandleCallback(cb, state)

		// State should remain unchanged
		if state.GetStep() != StepConfirm {
			t.Errorf("expected step to remain %s, got %s", StepConfirm, state.GetStep())
		}
	})

	t.Run("ignores other callbacks", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:     123,
			Step:       StepConfirm,
			Exclusions: make(map[string]bool),
		}

		cb := &tgbotapi.CallbackQuery{
			Data: "other:data",
			Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: 123},
			},
		}

		step.HandleCallback(cb, state)

		// State should remain unchanged
		if state.GetStep() != StepConfirm {
			t.Errorf("expected step to remain %s, got %s", StepConfirm, state.GetStep())
		}
	})
}

func TestConfirmStep_HandleMessage(t *testing.T) {
	t.Run("returns false for any text input", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:     123,
			Step:       StepConfirm,
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

func TestConfirmStep_ExclusionsSorted(t *testing.T) {
	t.Run("exclusions are displayed in alphabetical order", func(t *testing.T) {
		sender := &mockSender{}
		configStore := &mockConfigStore{
			servers: []vpnconfig.Server{
				{Name: "Server1", IPs: []string{"1.2.3.4"}},
			},
		}

		deps := &StepDeps{
			Sender: sender,
			Config: configStore,
		}

		step := NewConfirmStep(deps)

		state := &State{
			ChatID:      123,
			Step:        StepConfirm,
			ServerIndex: 0,
			Exclusions:  map[string]bool{"ua": true, "by": true, "ru": true},
			Clients:     []ClientRoute{},
		}

		step.Render(123, state)

		// The exclusions should be sorted: by, ru, ua
		// Find the position of each in the text
		byPos := strings.Index(sender.lastText, "by")
		ruPos := strings.Index(sender.lastText, "ru")
		uaPos := strings.Index(sender.lastText, "ua")

		if byPos > ruPos || ruPos > uaPos {
			t.Errorf("expected exclusions in alphabetical order (by, ru, ua), got text: %s", sender.lastText)
		}
	})
}

// Step 4 is where the user checks what is about to be applied, and it must name
// the server step 1 picked, wherever a refresh has moved it since.
func TestConfirmStep_Render_NamesThePickedServerAfterTheListMoved(t *testing.T) {
	store := &mockConfigStore{subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: wizardServers("Oslo", "Paris")}}}
	state := pickedServer(t, store, 1)
	store.subs = []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: wizardServers("Berlin", "Oslo", "Paris")}}
	sender := &mockSender{}

	NewConfirmStep(&StepDeps{Sender: sender, Config: store}).Render(123, state)

	if !strings.Contains(sender.lastText, "Xray server: Main / Paris") {
		t.Fatalf("confirmation %q, want the server picked in step 1", sender.lastText)
	}
}

// Two subscriptions can name a server alike: step 4 names the one step 1
// picked by its subscription, even after a refresh moved it next to the other.
func TestConfirmStep_Render_NamesTheSubscriptionOfThePickedServer(t *testing.T) {
	store := &mockConfigStore{subs: twoSubs()}
	state := NewManager().Start(123)
	NewServerStep(&StepDeps{Sender: &mockSender{}, Config: store}, nil).HandleCallback(&tgbotapi.CallbackQuery{
		Data:    "server:1b2c3d4e:0",
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 123}},
	}, state)
	store.subs = twoSubs()
	store.subs[0].Servers = store.subs[0].Servers[1:] // Alpha drops Oslo: Beta's Germany-1 moves from 2 to 1
	sender := &mockSender{}

	NewConfirmStep(&StepDeps{Sender: sender, Config: store}).Render(123, state)

	if !strings.Contains(sender.lastText, `Xray server: Beta / Germany\-1`) {
		t.Fatalf("confirmation %q, want Beta's Germany-1, the one picked in step 1", sender.lastText)
	}
}

// And one a refresh dropped is named as gone, so the user is not surprised when
// the apply leaves the running server alone.
func TestConfirmStep_Render_SaysThePickedServerIsGone(t *testing.T) {
	store := &mockConfigStore{subs: []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: wizardServers("Oslo", "Paris")}}}
	state := pickedServer(t, store, 1)
	store.subs = []vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Main", Servers: wizardServers("Oslo", "Berlin")}}
	sender := &mockSender{}

	NewConfirmStep(&StepDeps{Sender: sender, Config: store}).Render(123, state)

	if strings.Contains(sender.lastText, "Berlin") {
		t.Fatalf("confirmation %q names a server the user never picked", sender.lastText)
	}
	if !strings.Contains(sender.lastText, "Paris") || !strings.Contains(sender.lastText, "no longer in the server list") {
		t.Fatalf("confirmation %q, want Paris named as gone", sender.lastText)
	}
}
