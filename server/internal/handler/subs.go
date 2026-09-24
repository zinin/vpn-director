// internal/handler/subs.go
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// renameWait is how long a rename waits for the new name.
const renameWait = 5 * time.Minute

// SubsHandler handles /subs: a line per subscription with buttons to refresh,
// rename and delete it. A rename takes the chat's next text message.
type SubsHandler struct {
	deps       *Deps
	httpClient *http.Client
	now        func() time.Time

	mu      sync.Mutex
	renames map[int64]pendingRename // chat -> the rename waiting for its name
}

type pendingRename struct {
	id, name string
	until    time.Time
}

// NewSubsHandler creates a SubsHandler that downloads through the
// SSRF-hardened client.
func NewSubsHandler(deps *Deps) *SubsHandler {
	return &SubsHandler{
		deps:       deps,
		httpClient: ssrf.NewClient(30 * time.Second),
		now:        time.Now,
		renames:    map[int64]pendingRename{},
	}
}

// noButtons is a keyboard without a button, the one an edited message ends with.
func noButtons() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
}

// HandleSubs sends the list.
func (h *SubsHandler) HandleSubs(msg *tgbotapi.Message) {
	text, kb, err := h.list()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, text)
		return
	}
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, kb)
}

// list is the /subs message: a line per subscription and a row of buttons each.
func (h *SubsHandler) list() (string, tgbotapi.InlineKeyboardMarkup, error) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if len(subs) == 0 {
		return telegram.EscapeMarkdownV2("No subscriptions. Add one with /import <url> [name]"), tgbotapi.InlineKeyboardMarkup{}, nil
	}
	var sb strings.Builder
	sb.WriteString(telegram.EscapeMarkdownV2("Subscriptions:"))
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		sb.WriteString("\n" + telegram.EscapeMarkdownV2(subLine(s, h.now())))
		if !s.Static() {
			kb.Button("⟳ "+s.Name, "subs:r:"+s.ID)
		}
		kb.Button("✎ "+s.Name, "subs:n:"+s.ID)
		kb.Button("🗑 "+s.Name, "subs:d:"+s.ID)
		kb.Row()
	}
	return sb.String(), kb.Build(), nil
}

// subLine is one subscription on the list:
// "Alpha — sub.example.com — 32 servers — 2 h ago — OK".
func subLine(s vpnconfig.Subscription, now time.Time) string {
	where := s.Host()
	if s.Static() {
		where = "static list"
	}
	status := "OK"
	if s.Error != "" {
		status = s.Error
	}
	return fmt.Sprintf("%s — %s — %d servers — %s — %s", s.Name, where, len(s.Servers), timeAgo(now, s.Refreshed), status)
}

// timeAgo is how long before now t was, the way a list says it: "2 h ago".
func timeAgo(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
	}
}

// HandleCallback handles subs:r:<id> (refresh), subs:n:<id> (rename),
// subs:d:<id> (delete; asks first), subs:dy:<id> (delete confirmed) and
// subs:dn (keep).
func (h *SubsHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.Message.Chat == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	action, id, _ := strings.Cut(strings.TrimPrefix(cb.Data, "subs:"), ":")
	switch action {
	case "r":
		h.refresh(chatID, msgID, id)
	case "n":
		h.askName(chatID, id)
	case "d":
		h.askDelete(chatID, msgID, id)
	case "dy":
		h.remove(chatID, msgID, id)
	case "dn":
		h.redraw(chatID, msgID)
	}
}

func (h *SubsHandler) find(id string) (vpnconfig.Subscription, bool) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return vpnconfig.Subscription{}, false
	}
	i := vpnconfig.FindSubscription(subs, id)
	if i < 0 {
		return vpnconfig.Subscription{}, false
	}
	return subs[i], true
}

func (h *SubsHandler) gone(chatID int64) {
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("That subscription is gone"))
}

// redraw puts the current list into the message that carries the buttons.
func (h *SubsHandler) redraw(chatID int64, msgID int) {
	text, kb, err := h.list()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		kb = noButtons()
	}
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

func (h *SubsHandler) refresh(chatID int64, msgID int, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()
	res := service.RefreshSubscription(ctx, h.deps.Config, h.httpClient, id)
	if errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) {
		h.gone(chatID)
	} else {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(res.Line()))
	}
	h.redraw(chatID, msgID)
}

func (h *SubsHandler) askName(chatID int64, id string) {
	sub, ok := h.find(id)
	if !ok {
		h.gone(chatID)
		return
	}
	h.mu.Lock()
	h.renames[chatID] = pendingRename{id: id, name: sub.Name, until: h.now().Add(renameWait)}
	h.mu.Unlock()
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Send the new name for %s (or /cancel)", sub.Name)))
}

func (h *SubsHandler) askDelete(chatID int64, msgID int, id string) {
	sub, ok := h.find(id)
	if !ok {
		h.gone(chatID)
		h.redraw(chatID, msgID)
		return
	}
	text := fmt.Sprintf("Delete %s and its %d servers?", sub.Name, len(sub.Servers))
	if h.runsFrom(id) {
		text += " The running Xray server comes from it; Xray keeps running it until you select another."
	}
	kb := telegram.NewKeyboard().Button("Yes, delete", "subs:dy:"+id).Button("No", "subs:dn").Row().Build()
	h.deps.Sender.EditMessage(chatID, msgID, telegram.EscapeMarkdownV2(text), kb)
}

// runsFrom reports whether active_server names subscription id.
func (h *SubsHandler) runsFrom(id string) bool {
	cfg, err := h.deps.Config.LoadVPNConfig()
	return err == nil && cfg != nil && cfg.Xray.ActiveServer != nil && cfg.Xray.ActiveServer.Subscription == id
}

// runningServerGone is what a delete adds when the running server came from
// the subscription it deleted.
const runningServerGone = "The running server came from it and is no longer in any subscription: select another with /xray"

func (h *SubsHandler) remove(chatID int64, msgID int, id string) {
	sub, _ := h.find(id)
	active, err := service.DeleteSubscription(h.deps.Config, id)
	var text string
	switch {
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		text = "That subscription is gone"
	case errors.Is(err, vpnconfig.ErrServersSaved):
		// The file is gone and only the config beside it is stale: the delete
		// must not read as failed, nor hide that the running server came from it.
		text = fmt.Sprintf("Deleted %s, but xray.servers sync failed: %v", sub.Name, err)
		if active {
			text += ". " + runningServerGone
		}
	case err != nil:
		text = fmt.Sprintf("Delete failed: %v", err)
	case active:
		text = fmt.Sprintf("Deleted %s. %s", sub.Name, runningServerGone)
	default:
		text = "Deleted " + sub.Name
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(text))
	h.redraw(chatID, msgID)
}

// HandleTextInput takes the new name a rename waits for, and reports whether
// msg was one: a pending rename comes before the wizard. An answer the rename
// refuses uses the prompt up all the same.
func (h *SubsHandler) HandleTextInput(msg *tgbotapi.Message) bool {
	chatID := msg.Chat.ID
	h.mu.Lock()
	p, ok := h.renames[chatID]
	delete(h.renames, chatID)
	h.mu.Unlock()
	if !ok || h.now().After(p.until) {
		return false
	}
	err := service.RenameSubscription(h.deps.Config, p.id, msg.Text)
	var text string
	switch {
	case err == nil:
		text = fmt.Sprintf("Renamed %s to %s", p.name, strings.Trim(msg.Text, " "))
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		text = "That subscription is gone"
	default:
		text = fmt.Sprintf("Not renamed: %v. Tap ✎ to try again", err)
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(text))
	return true
}

// HandleCancel ends a rename that waits for its name.
func (h *SubsHandler) HandleCancel(msg *tgbotapi.Message) {
	h.ClearState(msg.Chat.ID)
	h.deps.Sender.Send(msg.Chat.ID, "Cancelled")
}

// ClearState forgets the rename the chat was asked to name.
func (h *SubsHandler) ClearState(chatID int64) {
	h.mu.Lock()
	delete(h.renames, chatID)
	h.mu.Unlock()
}
