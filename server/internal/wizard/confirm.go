package wizard

import (
	"fmt"
	"sort"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

// ConfirmStep handles Step 4: confirmation before applying
type ConfirmStep struct {
	deps *StepDeps
}

// NewConfirmStep creates a new ConfirmStep handler
func NewConfirmStep(deps *StepDeps) *ConfirmStep {
	return &ConfirmStep{
		deps: deps,
	}
}

// Render displays the confirmation UI with summary of selections
func (s *ConfirmStep) Render(chatID int64, state *State) {
	servers, err := s.deps.Config.LoadServers()
	if err != nil {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Failed to load servers"))
		return
	}
	// The subscription names, for "Beta / Germany-1": two subscriptions can
	// share a server name. A store that cannot list them leaves the name alone.
	subNames := map[string]string{}
	if subs, err := s.deps.Config.LoadSubscriptions(); err == nil {
		for _, sub := range subs {
			subNames[sub.ID] = sub.Name
		}
	}

	serverIndex := state.PickedIndex(servers)
	exclusions := state.GetExclusions()
	clients := state.GetClients()

	var sb strings.Builder
	sb.WriteString(telegram.EscapeMarkdownV2("Step 4/4: Confirmation") + "\n\n")

	// Show selected server
	if serverIndex >= 0 {
		srv := servers[serverIndex]
		label := srv.Name
		if n := subNames[srv.Subscription]; n != "" {
			label = n + " / " + srv.Name
		}
		sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("Xray server: %s (%s)", label, strings.Join(srv.IPs, ", "))) + "\n")
	} else if name := state.PickedName(); name != "" {
		// A refresh dropped it: the apply will leave the running server alone.
		sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("Xray server: %s (no longer in the server list; not switched)", name)) + "\n")
	}

	// Show exclusions (sorted alphabetically)
	var selected []string
	for k, v := range exclusions {
		if v {
			selected = append(selected, k)
		}
	}
	if len(selected) > 0 {
		sort.Strings(selected)
		sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("Exclusions: %s", strings.Join(selected, ", "))) + "\n")
	}

	// Show clients
	sb.WriteString("\n" + telegram.EscapeMarkdownV2("Clients:") + "\n")
	if len(clients) == 0 {
		sb.WriteString(telegram.EscapeMarkdownV2("(none)") + "\n")
	} else {
		for _, c := range clients {
			sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("* %s -> %s", c.IP, c.Route)) + "\n")
		}
	}

	// Build keyboard with Apply and Cancel
	kb := telegram.NewKeyboard()
	kb.Button("Apply", "apply").Button("Cancel", "cancel").Row()

	s.deps.Sender.SendWithKeyboard(chatID, sb.String(), kb.Build())
}

// HandleCallback is a no-op for ConfirmStep.
// The "apply" and "cancel" callbacks are handled by wizard/handler.go
func (s *ConfirmStep) HandleCallback(cb *tgbotapi.CallbackQuery, state *State) {
	// No-op: apply and cancel are handled at the Handler level
}

// HandleMessage returns false as confirm step doesn't handle text input
func (s *ConfirmStep) HandleMessage(msg *tgbotapi.Message, state *State) bool {
	return false
}
