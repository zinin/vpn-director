// internal/bot/router.go
package bot

import (
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// StatusRouterHandler defines methods for status-related commands
type StatusRouterHandler interface {
	HandleStatus(msg *tgbotapi.Message)
	HandleRestart(msg *tgbotapi.Message)
	HandleStop(msg *tgbotapi.Message)
}

// ServersRouterHandler defines methods for server-related commands
type ServersRouterHandler interface {
	HandleServers(msg *tgbotapi.Message)
	HandleCallback(cb *tgbotapi.CallbackQuery)
}

// ImportRouterHandler defines methods for import command
type ImportRouterHandler interface {
	HandleImport(msg *tgbotapi.Message)
}

// MiscRouterHandler defines methods for misc commands
type MiscRouterHandler interface {
	HandleStart(msg *tgbotapi.Message)
	HandleIP(msg *tgbotapi.Message)
	HandleVersion(msg *tgbotapi.Message)
	HandleLogs(msg *tgbotapi.Message)
}

// UpdateRouterHandler defines methods for update command
type UpdateRouterHandler interface {
	HandleUpdate(msg *tgbotapi.Message)
	HandleCallback(cb *tgbotapi.CallbackQuery)
}

// WizardRouterHandler defines methods for wizard
type WizardRouterHandler interface {
	Start(chatID int64)
	ClearState(chatID int64)
	HandleCallback(cb *tgbotapi.CallbackQuery)
	HandleTextInput(msg *tgbotapi.Message)
}

// XrayRouterHandler defines methods for xray command
type XrayRouterHandler interface {
	HandleXray(msg *tgbotapi.Message)
	HandleCallback(cb *tgbotapi.CallbackQuery)
}

// ExcludeRouterHandler defines methods for exclude command
type ExcludeRouterHandler interface {
	HandleExclude(msg *tgbotapi.Message)
	ClearState(chatID int64)
	HandleCallback(cb *tgbotapi.CallbackQuery)
	HandleTextInput(msg *tgbotapi.Message)
}

// ClientsRouterHandler defines methods for clients command
type ClientsRouterHandler interface {
	HandleClients(msg *tgbotapi.Message)
	ClearState(chatID int64)
	HandleCallback(cb *tgbotapi.CallbackQuery)
	HandleTextInput(msg *tgbotapi.Message)
}

// SubsRouterHandler defines methods for /subs and its rename prompt.
type SubsRouterHandler interface {
	HandleSubs(msg *tgbotapi.Message)
	HandleCancel(msg *tgbotapi.Message)
	HandleCallback(cb *tgbotapi.CallbackQuery)
	// HandleTextInput takes the name a rename waits for and reports whether it did.
	HandleTextInput(msg *tgbotapi.Message) bool
	ClearState(chatID int64)
}

// Router routes messages and callbacks to appropriate handlers
type Router struct {
	status  StatusRouterHandler
	servers ServersRouterHandler
	import_ ImportRouterHandler
	misc    MiscRouterHandler
	update  UpdateRouterHandler
	wizard  WizardRouterHandler
	xray    XrayRouterHandler
	exclude ExcludeRouterHandler
	clients ClientsRouterHandler
	subs    SubsRouterHandler
}

// NewRouter creates a new Router with all handlers
func NewRouter(
	status StatusRouterHandler,
	servers ServersRouterHandler,
	import_ ImportRouterHandler,
	misc MiscRouterHandler,
	update UpdateRouterHandler,
	wizard WizardRouterHandler,
	xray XrayRouterHandler,
	exclude ExcludeRouterHandler,
	clients ClientsRouterHandler,
	subs SubsRouterHandler,
) *Router {
	return &Router{
		status:  status,
		servers: servers,
		import_: import_,
		misc:    misc,
		update:  update,
		wizard:  wizard,
		xray:    xray,
		exclude: exclude,
		clients: clients,
		subs:    subs,
	}
}

// RouteMessage routes a message to the appropriate handler based on command
func (r *Router) RouteMessage(msg *tgbotapi.Message) {
	// Any command ends a rename that waits for its name: the next text is no
	// longer an answer to it.
	if r.subs != nil && msg.IsCommand() {
		r.subs.ClearState(msg.Chat.ID)
	}
	switch msg.Command() {
	case "start":
		r.misc.HandleStart(msg)
	case "status":
		r.status.HandleStatus(msg)
	case "restart":
		r.status.HandleRestart(msg)
	case "stop":
		r.status.HandleStop(msg)
	case "servers":
		r.servers.HandleServers(msg)
	case "import":
		r.import_.HandleImport(msg)
	case "logs":
		r.misc.HandleLogs(msg)
	case "ip":
		r.misc.HandleIP(msg)
	case "version":
		r.misc.HandleVersion(msg)
	case "update":
		r.update.HandleUpdate(msg)
	case "configure":
		r.exclude.ClearState(msg.Chat.ID)
		r.clients.ClearState(msg.Chat.ID)
		r.wizard.Start(msg.Chat.ID)
	case "xray":
		r.xray.HandleXray(msg)
	case "exclude":
		r.wizard.ClearState(msg.Chat.ID)
		r.clients.ClearState(msg.Chat.ID)
		r.exclude.HandleExclude(msg)
	case "clients":
		r.exclude.ClearState(msg.Chat.ID)
		r.wizard.ClearState(msg.Chat.ID)
		r.clients.HandleClients(msg)
	case "subs":
		r.subs.HandleSubs(msg)
	case "cancel":
		r.subs.HandleCancel(msg)
	default:
		// A rename waiting for its name takes the text first: its prompt is
		// the last question the user was asked.
		if r.subs != nil && r.subs.HandleTextInput(msg) {
			return
		}
		// Non-command messages go to clients, exclude, and wizard text handlers.
		// All handlers check their own manager state, so multi-dispatch
		// is safe — only one will have active state.
		r.clients.HandleTextInput(msg)
		r.exclude.HandleTextInput(msg)
		r.wizard.HandleTextInput(msg)
	}
}

// RouteCallback routes a callback query to the appropriate handler
func (r *Router) RouteCallback(cb *tgbotapi.CallbackQuery) {
	// A button outside /subs ends a rename that waits for its name, as a
	// command does: it can ask a question of its own (clients:add,
	// exclip:add, the wizard's text steps), and the next text answers that.
	if r.subs != nil && cb.Message != nil && cb.Message.Chat != nil && !strings.HasPrefix(cb.Data, "subs:") {
		r.subs.ClearState(cb.Message.Chat.ID)
	}
	if strings.HasPrefix(cb.Data, "servers:") {
		r.servers.HandleCallback(cb)
		return
	}
	if strings.HasPrefix(cb.Data, "update:") {
		r.update.HandleCallback(cb)
		return
	}
	if strings.HasPrefix(cb.Data, "xray:") {
		r.xray.HandleCallback(cb)
		return
	}
	if strings.HasPrefix(cb.Data, "exclip:") {
		r.exclude.HandleCallback(cb)
		return
	}
	if strings.HasPrefix(cb.Data, "clients:") {
		r.clients.HandleCallback(cb)
		return
	}
	if strings.HasPrefix(cb.Data, "subs:") {
		r.subs.HandleCallback(cb)
		return
	}
	r.wizard.HandleCallback(cb)
}
