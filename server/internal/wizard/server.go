package wizard

import (
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

// ServerStep handles Step 1: server selection
type ServerStep struct {
	deps    *StepDeps
	next    func(chatID int64, state *State) // callback to render next step
	onClear func(chatID int64)               // callback to clear wizard state (for testing)
}

// NewServerStep creates a new ServerStep handler
func NewServerStep(deps *StepDeps, next func(chatID int64, state *State)) *ServerStep {
	return &ServerStep{
		deps: deps,
		next: next,
	}
}

// Render displays the server selection UI
func (s *ServerStep) Render(chatID int64, state *State) {
	servers, err := s.deps.Config.LoadServers()
	if err != nil || len(servers) == 0 {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("No servers found. Use /import"))
		s.clearWizard(chatID)
		return
	}

	cols := getServerGridColumns(len(servers))

	kb := telegram.NewKeyboard()
	for i, srv := range servers {
		btnText := fmt.Sprintf("%d. %s", i+1, srv.Name)
		kb.Button(btnText, fmt.Sprintf("server:%d", i))
	}
	kb.Columns(cols)
	kb.Button("Cancel", "cancel").Row()

	text := telegram.EscapeMarkdownV2(fmt.Sprintf("Step 1/4: Select Xray server (%d available)", len(servers)))
	s.deps.Sender.SendWithKeyboard(chatID, text, kb.Build())
}

// HandleCallback processes callback button presses for server selection
func (s *ServerStep) HandleCallback(cb *tgbotapi.CallbackQuery, state *State) {
	data := cb.Data

	if !strings.HasPrefix(data, "server:") {
		return
	}

	idxStr := strings.TrimPrefix(data, "server:")
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		s.deps.Sender.Send(cb.Message.Chat.ID, telegram.EscapeMarkdownV2("Invalid server index"))
		return
	}

	// Validate server index bounds
	servers, err := s.deps.Config.LoadServers()
	if err != nil {
		s.deps.Sender.Send(cb.Message.Chat.ID, telegram.EscapeMarkdownV2("Failed to load servers"))
		return
	}

	if idx < 0 || idx >= len(servers) {
		s.deps.Sender.Send(cb.Message.Chat.ID, telegram.EscapeMarkdownV2("Invalid server index"))
		return
	}

	state.PickServer(idx, servers[idx])
	state.SetStep(StepExclusions)
	// Default: include ru in exclusions
	state.SetExclusion("ru", true)

	if s.next != nil {
		s.next(cb.Message.Chat.ID, state)
	}
}

// HandleMessage processes text input - server step doesn't handle text input
func (s *ServerStep) HandleMessage(msg *tgbotapi.Message, state *State) bool {
	return false
}

// clearWizard clears the wizard state for the given chat
func (s *ServerStep) clearWizard(chatID int64) {
	if s.onClear != nil {
		s.onClear(chatID)
	}
}

// getServerGridColumns determines the number of columns for the server grid
func getServerGridColumns(count int) int {
	if count <= 10 {
		return 1
	}
	return 2
}
