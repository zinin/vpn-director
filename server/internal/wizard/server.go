package wizard

import (
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
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

// serverPerPage is how many servers one page of step 1 lists.
const serverPerPage = 30

// Render shows the subscriptions to choose from - or, with one, its servers.
func (s *ServerStep) Render(chatID int64, state *State) {
	subs, err := s.deps.Config.LoadSubscriptions()
	if err != nil || len(vpnconfig.AllServers(subs)) == 0 {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("No servers found. Use /import"))
		s.clearWizard(chatID)
		return
	}
	text, kb := firstServerPage(subs)
	s.deps.Sender.SendWithKeyboard(chatID, text, kb)
}

// firstServerPage is what step 1 opens with: the subscriptions, or with one
// subscription its servers.
func firstServerPage(subs []vpnconfig.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	if len(subs) == 1 {
		return serverPage(subs[0], 0, false)
	}
	return subscriptionChoice(subs)
}

// subscriptionChoice is a button per subscription that has servers.
func subscriptionChoice(subs []vpnconfig.Subscription) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	total := 0
	for _, sub := range subs {
		if len(sub.Servers) == 0 {
			continue
		}
		total += len(sub.Servers)
		kb.Button(fmt.Sprintf("%s (%d)", sub.Name, len(sub.Servers)), "server:sub:"+sub.ID+":0").Row()
	}
	kb.Button("Cancel", "cancel").Row()
	return telegram.EscapeMarkdownV2(fmt.Sprintf("Step 1/4: Select a subscription (%d servers)", total)), kb.Build()
}

// serverPage is one page of a subscription's servers, with ◀ ▶ between pages
// and « Back to the subscriptions when back is set.
func serverPage(sub vpnconfig.Subscription, page int, back bool) (string, tgbotapi.InlineKeyboardMarkup) {
	pages := max(1, (len(sub.Servers)+serverPerPage-1)/serverPerPage)
	page = max(0, min(page, pages-1))
	start := page * serverPerPage
	end := min(start+serverPerPage, len(sub.Servers))
	kb := telegram.NewKeyboard()
	for i := start; i < end; i++ {
		kb.Button(fmt.Sprintf("%d. %s", i+1, sub.Servers[i].Name), fmt.Sprintf("server:%s:%d", sub.ID, i))
	}
	kb.Columns(getServerGridColumns(end - start))
	if page > 0 {
		kb.Button("◀", fmt.Sprintf("server:sub:%s:%d", sub.ID, page-1))
	}
	if page < pages-1 {
		kb.Button("▶", fmt.Sprintf("server:sub:%s:%d", sub.ID, page+1))
	}
	if back {
		kb.Button("« Back", "server:subs")
	}
	kb.Row()
	kb.Button("Cancel", "cancel").Row()
	text := fmt.Sprintf("Step 1/4: Select Xray server of %s (%d available)", sub.Name, len(sub.Servers))
	if pages > 1 {
		text += fmt.Sprintf(", page %d/%d", page+1, pages)
	}
	return telegram.EscapeMarkdownV2(text), kb.Build()
}

// flatIndex is where subs[si].Servers[i] sits in the flattened list
// ConfigStore.LoadServers answers, the one steps 4 and the apply read.
func flatIndex(subs []vpnconfig.Subscription, si, i int) int {
	for _, sub := range subs[:si] {
		i += len(sub.Servers)
	}
	return i
}

// HandleCallback processes step 1's buttons: server:subs, server:sub:<id>:<page>
// and server:<id>:<index>, the pick. A button whose subscription or server is
// gone - server:<index> from before subscriptions among them - starts step 1
// again.
func (s *ServerStep) HandleCallback(cb *tgbotapi.CallbackQuery, state *State) {
	if !strings.HasPrefix(cb.Data, "server:") || cb.Message == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	subs, err := s.deps.Config.LoadSubscriptions()
	if err != nil {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Failed to load servers"))
		return
	}
	if len(vpnconfig.AllServers(subs)) == 0 {
		s.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("No servers found. Use /import"))
		s.clearWizard(chatID)
		return
	}
	switch rest := strings.TrimPrefix(cb.Data, "server:"); {
	case rest == "subs":
		text, kb := firstServerPage(subs)
		s.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	case strings.HasPrefix(rest, "sub:"):
		id, pageText, _ := strings.Cut(strings.TrimPrefix(rest, "sub:"), ":")
		page, _ := strconv.Atoi(pageText)
		if si := vpnconfig.FindSubscription(subs, id); si >= 0 {
			text, kb := serverPage(subs[si], page, len(subs) > 1)
			s.deps.Sender.EditMessage(chatID, msgID, text, kb)
			return
		}
	default:
		id, idxText, found := strings.Cut(rest, ":")
		idx, err := strconv.Atoi(idxText)
		si := vpnconfig.FindSubscription(subs, id)
		if found && err == nil && si >= 0 && idx >= 0 && idx < len(subs[si].Servers) {
			srv := subs[si].Servers[idx]
			srv.Subscription = subs[si].ID
			state.PickServer(flatIndex(subs, si, idx), srv)
			state.SetStep(StepExclusions)
			// Default: include ru in exclusions
			state.SetExclusion("ru", true)
			if s.next != nil {
				s.next(chatID, state)
			}
			return
		}
	}
	text, kb := firstServerPage(subs)
	s.deps.Sender.EditMessage(chatID, msgID, telegram.EscapeMarkdownV2("The server list has changed; select again.")+"\n\n"+text, kb)
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
