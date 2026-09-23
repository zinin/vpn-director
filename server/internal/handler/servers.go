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

// HandleServers handles /servers command - loads servers from config, builds paginated list
func (h *ServersHandler) HandleServers(msg *tgbotapi.Message) {
	servers, err := h.deps.Config.LoadServers()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Error: %v", err)))
		return
	}

	if len(servers) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("No servers. Use /import to add servers."))
		return
	}

	text, keyboard := buildServersPage(servers, 0)
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

	chatID := cb.Message.Chat.ID
	data := cb.Data

	// Parse page number from "servers:page:N"
	var page int
	if _, err := fmt.Sscanf(data, "servers:page:%d", &page); err != nil {
		// noop button clicked
		return
	}

	// Load servers
	servers, err := h.deps.Config.LoadServers()
	if err != nil || len(servers) == 0 {
		return
	}

	text, keyboard := buildServersPage(servers, page)
	h.deps.Sender.EditMessage(chatID, cb.Message.MessageID, text, keyboard)
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

// buildServersPage builds paginated server list with navigation keyboard
func buildServersPage(servers []vpnconfig.Server, page int) (string, tgbotapi.InlineKeyboardMarkup) {
	// Guard clause for empty servers list
	if len(servers) == 0 {
		return "No servers available\\.", tgbotapi.NewInlineKeyboardMarkup()
	}

	totalPages := (len(servers) + serversPerPage - 1) / serversPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * serversPerPage
	end := start + serversPerPage
	if end > len(servers) {
		end = len(servers)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🖥 *Servers* \\(%d\\), page %d/%d:\n",
		len(servers), page+1, totalPages))

	for i := start; i < end; i++ {
		s := servers[i]
		sb.WriteString(fmt.Sprintf("%d\\. %s — %s \\(%s\\) · %s\n",
			i+1,
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

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons)

	return sb.String(), keyboard
}
