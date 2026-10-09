package wizard

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

// Handler routes wizard callbacks and text input to step handlers
type Handler struct {
	manager *Manager
	steps   map[Step]StepHandler
	sender  telegram.MessageSender
	applier *Applier
}

// NewHandler creates a new wizard Handler
func NewHandler(
	sender telegram.MessageSender,
	config service.ConfigStore,
	vpn service.VPNDirector,
	xray service.XrayGenerator,
) *Handler {
	deps := &StepDeps{Sender: sender, Config: config, VPN: vpn}
	manager := NewManager()

	// Create step handlers with next callbacks
	var serverStep, exclusionsStep, excludeIPsStep, clientsStep, confirmStep StepHandler

	// ServerStep -> ExclusionsStep
	serverStep = NewServerStep(deps, func(chatID int64, state *State) {
		exclusionsStep.Render(chatID, state)
	})

	// ExclusionsStep -> ExcludeIPsStep
	exclusionsStep = NewExclusionsStep(deps, func(chatID int64, state *State) {
		// Pre-populate ExcludeIPs from existing config
		cfg, err := config.LoadVPNConfig()
		if err == nil && len(cfg.Xray.ExcludeIPs) > 0 {
			state.SetExcludeIPs(cfg.Xray.ExcludeIPs)
		}
		excludeIPsStep.Render(chatID, state)
	})

	// ExcludeIPsStep -> ClientsStep
	excludeIPsStep = NewExcludeIPsStep(deps, func(chatID int64, state *State) {
		clientsStep.Render(chatID, state)
	})

	// ClientsStep -> ConfirmStep
	clientsStep = NewClientsStep(deps, func(chatID int64, state *State) {
		confirmStep.Render(chatID, state)
	})

	// ConfirmStep has no next callback (apply is handled by Handler)
	confirmStep = NewConfirmStep(deps)

	return &Handler{
		manager: manager,
		steps: map[Step]StepHandler{
			StepSelectServer: serverStep,
			StepExclusions:   exclusionsStep,
			StepExcludeIPs:   excludeIPsStep,
			StepClients:      clientsStep,
			StepClientIP:     clientsStep, // ClientsStep handles IP input
			StepClientRoute:  clientsStep, // ClientsStep handles route selection
			StepConfirm:      confirmStep,
		},
		sender:  sender,
		applier: NewApplier(manager, sender, config, vpn, xray),
	}
}

// GetManager returns the wizard state manager
func (h *Handler) GetManager() *Manager {
	return h.manager
}

// Start begins a new wizard session
func (h *Handler) Start(chatID int64) {
	state := h.manager.Start(chatID)
	h.steps[StepSelectServer].Render(chatID, state)
}

// HandleCallback processes callback button presses
// Note: AckCallback is called by Bot.Run() before routing, no need to ack here
func (h *Handler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	// Guard against nil Message (inline mode or channel callbacks)
	if cb.Message == nil || cb.Message.Chat == nil {
		return
	}

	chatID := cb.Message.Chat.ID
	data := cb.Data

	if data == "cancel" {
		h.manager.Clear(chatID)
		h.sender.SendPlain(chatID, "Configuration cancelled")
		return
	}

	state := h.manager.Get(chatID)
	if state == nil {
		h.sender.Send(chatID, telegram.EscapeMarkdownV2("No active session. Use /configure"))
		return
	}

	if data == "apply" {
		h.applier.Apply(chatID, state)
		return
	}

	// Route to appropriate step handler
	currentStep := state.GetStep()
	if step, ok := h.steps[currentStep]; ok {
		step.HandleCallback(cb, state)
	}
}

// ClearState clears any active /configure wizard session for the given chat
func (h *Handler) ClearState(chatID int64) {
	h.manager.Clear(chatID)
}

// HandleTextInput processes text messages for wizard steps
func (h *Handler) HandleTextInput(msg *tgbotapi.Message) {
	state := h.manager.Get(msg.Chat.ID)
	if state == nil {
		return
	}

	// Route to current step handler
	currentStep := state.GetStep()
	if step, ok := h.steps[currentStep]; ok {
		step.HandleMessage(msg, state)
	}
}
