// internal/handler/xray.go
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// xrayPerPage is how many servers one /xray page lists, two a row. A message
// holds a limited number of buttons, and two subscriptions already bring 94
// servers.
const xrayPerPage = 30

// XrayHandler handles /xray: the subscription first, then its server.
type XrayHandler struct {
	deps *Deps
}

// NewXrayHandler creates a new XrayHandler
func NewXrayHandler(deps *Deps) *XrayHandler {
	return &XrayHandler{deps: deps}
}

// HandleXray shows the subscriptions to choose from - or, with one, its servers.
func (h *XrayHandler) HandleXray(msg *tgbotapi.Message) {
	text, kb, err := h.firstStep()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	if len(kb.InlineKeyboard) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, text)
		return
	}
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, kb)
}

// firstStep is what /xray opens with, and what « Back returns to.
func (h *XrayHandler) firstStep() (string, tgbotapi.InlineKeyboardMarkup, error) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if len(vpnconfig.AllServers(subs)) == 0 {
		return telegram.EscapeMarkdownV2("Серверы не найдены. Используйте /import для импорта"), tgbotapi.InlineKeyboardMarkup{}, nil
	}
	active := h.active()
	if len(subs) == 1 {
		text, kb := xrayServersPage(subs[0], 0, false, active)
		return text, kb, nil
	}
	text, kb := xraySubscriptions(subs, active)
	return text, kb, nil
}

func (h *XrayHandler) active() *vpnconfig.ActiveServer {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil || cfg == nil {
		return nil
	}
	return cfg.Xray.ActiveServer
}

// xraySubscriptions is the first step: a button per subscription with its
// server count, the running server's subscription checked.
func xraySubscriptions(subs []vpnconfig.Subscription, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
	kb := telegram.NewKeyboard()
	for _, s := range subs {
		if len(s.Servers) == 0 {
			continue
		}
		label := fmt.Sprintf("%s (%d)", s.Name, len(s.Servers))
		if active != nil && active.Subscription == s.ID {
			label = "✓ " + label
		}
		kb.Button(label, "xray:sub:"+s.ID+":0").Row()
	}
	return telegram.EscapeMarkdownV2("Выберите подписку:"), kb.Build()
}

// xrayServersPage is one page of a subscription's servers, two a row, with ◀ ▶
// between pages and « Back to the subscriptions when back is set.
func xrayServersPage(sub vpnconfig.Subscription, page int, back bool, active *vpnconfig.ActiveServer) (string, tgbotapi.InlineKeyboardMarkup) {
	pages := max(1, (len(sub.Servers)+xrayPerPage-1)/xrayPerPage)
	page = max(0, min(page, pages-1))
	start := page * xrayPerPage
	end := min(start+xrayPerPage, len(sub.Servers))
	kb := telegram.NewKeyboard()
	for i := start; i < end; i++ {
		s := sub.Servers[i]
		s.Subscription = sub.ID
		label := fmt.Sprintf("%d. %s", i+1, s.Name)
		if active != nil && active.Subscription == s.Subscription && active.Name == s.Name && active.Address == s.Address && active.Port == s.Port {
			label = "✓ " + label
		}
		kb.Button(label, fmt.Sprintf("xray:select:%s:%d:%s", sub.ID, i, serverFingerprint(s)))
	}
	kb.Columns(2)
	if page > 0 {
		kb.Button("◀", fmt.Sprintf("xray:sub:%s:%d", sub.ID, page-1))
	}
	if page < pages-1 {
		kb.Button("▶", fmt.Sprintf("xray:sub:%s:%d", sub.ID, page+1))
	}
	if back {
		kb.Button("« Back", "xray:subs")
	}
	kb.Row()
	text := fmt.Sprintf("%s: выберите сервер", sub.Name)
	if pages > 1 {
		text += fmt.Sprintf(" (стр. %d/%d)", page+1, pages)
	}
	return telegram.EscapeMarkdownV2(text), kb.Build()
}

// serverFingerprint names a server in a button: the first 8 hex digits of
// sha256("subscription|name|address|port"). The list can change between /xray
// and the tap - the subscription watch rotates endpoints, a refresh replaces a
// list - and the index alone then names another server.
func serverFingerprint(s vpnconfig.Server) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", s.Subscription, s.Name, s.Address, s.Port)))
	return hex.EncodeToString(sum[:4])
}

// HandleCallback handles xray:subs (back to the subscriptions),
// xray:sub:<id>:<page> (a page of servers) and
// xray:select:<id>:<index>:<fingerprint> (the switch). A button of a keyboard
// sent before subscriptions - xray:select:<index>[:<fingerprint>] - indexes a
// list that is gone and is answered as a changed one.
func (h *XrayHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil {
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	switch data := cb.Data; {
	case data == "xray:subs":
		text, kb, err := h.firstStep()
		if err != nil {
			h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
			return
		}
		if len(kb.InlineKeyboard) == 0 {
			// Every server went while the keyboard was open. An edit whose
			// keyboard is null is refused and would leave the old buttons.
			kb = noButtons()
		}
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "xray:sub:"):
		id, pageText, _ := strings.Cut(strings.TrimPrefix(data, "xray:sub:"), ":")
		page, _ := strconv.Atoi(pageText)
		subs, err := h.deps.Config.LoadSubscriptions()
		if err != nil {
			h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
			return
		}
		i := vpnconfig.FindSubscription(subs, id)
		if i < 0 {
			h.stale(chatID, msgID)
			return
		}
		text, kb := xrayServersPage(subs[i], page, len(subs) > 1, h.active())
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "xray:select:"):
		h.selectServer(chatID, msgID, strings.TrimPrefix(data, "xray:select:"))
	}
}

// stale replaces a keyboard whose server the list no longer has at that place.
func (h *XrayHandler) stale(chatID int64, msgID int) {
	text := telegram.EscapeMarkdownV2("The server list has changed since these buttons were sent; run /xray again")
	h.deps.Sender.EditMessage(chatID, msgID, text, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
}

// selectServer switches Xray to the server <id>:<index>:<fingerprint> names.
func (h *XrayHandler) selectServer(chatID int64, msgID int, arg string) {
	parts := strings.Split(arg, ":")
	if len(parts) != 3 || !vpnconfig.ValidSubscriptionID(parts[0]) {
		h.stale(chatID, msgID)
		return
	}
	idx, err := strconv.Atoi(parts[1])
	if err != nil {
		h.stale(chatID, msgID)
		return
	}
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	si := vpnconfig.FindSubscription(subs, parts[0])
	if si < 0 || idx < 0 || idx >= len(subs[si].Servers) {
		h.stale(chatID, msgID)
		return
	}
	server := subs[si].Servers[idx]
	server.Subscription = subs[si].ID
	if parts[2] != serverFingerprint(server) {
		h.stale(chatID, msgID)
		return
	}

	// The generated inbound has to listen where the TPROXY rules send traffic,
	// so the ports come from advanced.xray rather than from the template.
	var ports service.InboundPorts
	vpnCfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(vpnCfg)

	// Generate the Xray config and record which server it came from, both
	// under the config lock: the Web UI reads that record to name the running
	// server, and a switch made there at the same moment must not be able to
	// leave the two disagreeing.
	generated, err := service.GenerateAndRecordActiveServer(h.deps.Config, h.deps.Xray, server, ports)
	if !generated {
		// config.json is untouched, so there is nothing to restart into.
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}
	if err != nil {
		// The switch itself worked and the user has nothing to do about a
		// lock timeout, so this stays in the log.
		slog.Warn("Failed to record the active server", "server", server.Name, "error", err)
	}

	if err := h.deps.VPN.RestartXray(); err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка перезапуска: %v", err)))
		return
	}

	successText := telegram.EscapeMarkdownV2(fmt.Sprintf("✓ Переключено на %s / %s", subs[si].Name, server.Name))
	h.deps.Sender.EditMessage(chatID, msgID, successText, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
}
