// internal/handler/servers.go
package handler

import (
	"fmt"
	"sort"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const serversPerPage = 15

// ServersHandler handles /servers command
type ServersHandler struct {
	deps *Deps
}

// NewServersHandler creates a new ServersHandler
func NewServersHandler(deps *Deps) *ServersHandler {
	return &ServersHandler{deps: deps}
}

// serverLine is one server on a /servers page: its subscription's name and its
// number within that subscription.
type serverLine struct {
	sub    string
	number int
	server vpnconfig.Server
}

// serverLines is every server of subs, in subscription order.
func serverLines(subs []vpnconfig.Subscription) []serverLine {
	var lines []serverLine
	for _, sub := range subs {
		for i, s := range sub.Servers {
			lines = append(lines, serverLine{sub: sub.Name, number: i + 1, server: s})
		}
	}
	return lines
}

// HandleServers handles /servers: every server, a header per subscription, in pages.
func (h *ServersHandler) HandleServers(msg *tgbotapi.Message) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Error: %v", err)))
		return
	}
	lines := serverLines(subs)
	if len(lines) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("No servers. Use /import to add a subscription."))
		return
	}
	text, keyboard := buildServersPage(lines, len(subs), 0)
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, keyboard)
}

// HandleCallback handles servers pagination callbacks (servers:page:N)
func (h *ServersHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	// Acknowledge callback
	h.deps.Sender.AckCallback(cb.ID)

	// Guard against nil Message (inline mode callbacks)
	if cb.Message == nil {
		return
	}

	var page int
	if _, err := fmt.Sscanf(cb.Data, "servers:page:%d", &page); err != nil {
		// noop button clicked
		return
	}
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		return
	}
	lines := serverLines(subs)
	if len(lines) == 0 {
		return
	}
	text, keyboard := buildServersPage(lines, len(subs), page)
	h.deps.Sender.EditMessage(cb.Message.Chat.ID, cb.Message.MessageID, text, keyboard)
}

// extractCountry extracts country name from server name format "Country, City"
func extractCountry(name string) string {
	parts := strings.SplitN(name, ",", 2)
	country := strings.TrimSpace(parts[0])
	if country == "" {
		return "Other"
	}
	return country
}

// groupServersByCountry groups servers by country and returns formatted string
func groupServersByCountry(servers []vpnconfig.Server) string {
	if len(servers) == 0 {
		return ""
	}

	counts := make(map[string]int)
	for _, s := range servers {
		country := extractCountry(s.Name)
		counts[country]++
	}

	type countryCount struct {
		country string
		count   int
	}
	var sorted []countryCount
	for c, n := range counts {
		sorted = append(sorted, countryCount{c, n})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count > sorted[j].count
		}
		return sorted[i].country < sorted[j].country
	})

	var parts []string
	maxShow := 10
	for i, cc := range sorted {
		if i >= maxShow {
			parts = append(parts, fmt.Sprintf("и ещё %d стран", len(sorted)-maxShow))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", cc.country, cc.count))
	}
	return strings.Join(parts, ", ")
}

// buildServersPage builds one page of the list, a header wherever a
// subscription starts and at the top of the page, with the navigation keyboard.
func buildServersPage(lines []serverLine, subCount, page int) (string, tgbotapi.InlineKeyboardMarkup) {
	if len(lines) == 0 {
		return "No servers available\\.", tgbotapi.NewInlineKeyboardMarkup()
	}

	totalPages := (len(lines) + serversPerPage - 1) / serversPerPage
	page = max(0, min(page, totalPages-1))
	start := page * serversPerPage
	end := min(start+serversPerPage, len(lines))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🖥 *Servers* \\(%d\\) in %d subscriptions, page %d/%d:\n",
		len(lines), subCount, page+1, totalPages))

	for i := start; i < end; i++ {
		l := lines[i]
		if i == start || l.sub != lines[i-1].sub {
			sb.WriteString("\n*" + telegram.EscapeMarkdownV2(l.sub) + "*\n")
		}
		s := l.server
		sb.WriteString(fmt.Sprintf("%d\\. %s — %s \\(%s\\) · %s\n",
			l.number,
			telegram.EscapeMarkdownV2(s.Name),
			telegram.EscapeMarkdownV2(s.Address),
			telegram.EscapeMarkdownV2(strings.Join(s.IPs, ", ")),
			telegram.EscapeMarkdownV2(s.Label())))
	}

	// Navigation buttons
	var buttons []tgbotapi.InlineKeyboardButton
	if page > 0 {
		buttons = append(buttons,
			tgbotapi.NewInlineKeyboardButtonData("← Prev", fmt.Sprintf("servers:page:%d", page-1)))
	}
	buttons = append(buttons,
		tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, totalPages), "servers:noop"))
	if page < totalPages-1 {
		buttons = append(buttons,
			tgbotapi.NewInlineKeyboardButtonData("Next →", fmt.Sprintf("servers:page:%d", page+1)))
	}

	return sb.String(), tgbotapi.NewInlineKeyboardMarkup(buttons)
}
