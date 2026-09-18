package handler

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

type ClientsHandler struct {
	deps     *Deps
	mu       sync.Mutex
	addState map[int64]string
}

func NewClientsHandler(deps *Deps) *ClientsHandler {
	return &ClientsHandler{
		deps:     deps,
		addState: make(map[int64]string),
	}
}

func (h *ClientsHandler) ClearState(chatID int64) {
	h.mu.Lock()
	delete(h.addState, chatID)
	h.mu.Unlock()
}

func (h *ClientsHandler) HandleClients(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	h.ClearState(chatID)

	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Config load error: %v", err)))
		return
	}

	text, kb := h.buildClientList(cfg)
	h.deps.Sender.SendWithKeyboard(chatID, text, kb)
}

func (h *ClientsHandler) buildClientList(cfg *vpnconfig.VPNDirectorConfig) (string, tgbotapi.InlineKeyboardMarkup) {
	clients := vpnconfig.CollectClients(cfg)
	kb := telegram.NewKeyboard()

	var sb strings.Builder
	if len(clients) == 0 {
		sb.WriteString(telegram.EscapeMarkdownV2("No clients configured."))
	} else {
		sb.WriteString(telegram.EscapeMarkdownV2("Clients:") + "\n\n")
		for _, c := range clients {
			status := "\u25b6"
			if c.Paused {
				status = "\u23f8"
			}
			sb.WriteString(telegram.EscapeMarkdownV2(fmt.Sprintf("%s  %s \u2192 %s", status, c.IP, c.Route)) + "\n")

			if c.Paused {
				kb.Button(fmt.Sprintf("\u25b6 %s", c.IP), fmt.Sprintf("clients:resume:%s", c.IP))
			} else {
				kb.Button(fmt.Sprintf("\u23f8 %s", c.IP), fmt.Sprintf("clients:pause:%s", c.IP))
			}
			kb.Button(fmt.Sprintf("\U0001f5d1 %s", c.IP), fmt.Sprintf("clients:remove:%s", c.IP))
			kb.Row()
		}
	}

	kb.Button("\u2795 Add client", "clients:add")
	kb.Button("\u2716 Close", "clients:close")
	kb.Row()

	return sb.String(), kb.Build()
}

// HandleCallback handles all clients: callback queries.
func (h *ClientsHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	data := cb.Data
	if !strings.HasPrefix(data, "clients:") {
		return
	}

	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	action := strings.TrimPrefix(data, "clients:")

	switch {
	case strings.HasPrefix(action, "pause:"):
		ip := strings.TrimPrefix(action, "pause:")
		h.handlePauseResume(chatID, msgID, ip, true)
	case strings.HasPrefix(action, "resume:"):
		ip := strings.TrimPrefix(action, "resume:")
		h.handlePauseResume(chatID, msgID, ip, false)
	case strings.HasPrefix(action, "remove:"):
		ip := strings.TrimPrefix(action, "remove:")
		h.handleRemoveConfirm(chatID, msgID, ip)
	case strings.HasPrefix(action, "rm_yes:"):
		ip := strings.TrimPrefix(action, "rm_yes:")
		h.handleRemove(chatID, msgID, ip)
	case action == "rm_no":
		h.handleRefreshList(chatID, msgID)
	case action == "add":
		h.handleAddStart(chatID)
	case action == "close":
		h.handleClose(chatID, msgID)
	case strings.HasPrefix(action, "route:"):
		route := strings.TrimPrefix(action, "route:")
		h.handleAddRoute(chatID, msgID, route)
	}
}

func (h *ClientsHandler) handlePauseResume(chatID int64, msgID int, ip string, pause bool) {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}

	// Verify IP still exists in config (stale keyboard protection)
	clients := vpnconfig.CollectClients(cfg)
	exists := false
	for _, c := range clients {
		if c.IP == ip {
			exists = true
			break
		}
	}
	if !exists {
		text, kb := h.buildClientList(cfg)
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	}

	err = h.deps.Config.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
		if pause {
			found := false
			for _, p := range c.PausedClients {
				if p == ip {
					found = true
					break
				}
			}
			if !found {
				c.PausedClients = append(c.PausedClients, ip)
			}
		} else {
			c.PausedClients = removeString(c.PausedClients, ip)
		}
		cfg = c // render the list from what was actually saved
		return nil
	})
	if err != nil {
		h.deps.Sender.SendPlain(chatID, configUpdateError(err))
		return
	}

	if err := h.deps.VPN.Apply(); err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Apply error: %v", err))
		return
	}

	text, kb := h.buildClientList(cfg)
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

func (h *ClientsHandler) handleRefreshList(chatID int64, msgID int) {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}
	text, kb := h.buildClientList(cfg)
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

func (h *ClientsHandler) handleClose(chatID int64, msgID int) {
	h.ClearState(chatID)
	emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
	h.deps.Sender.EditMessage(chatID, msgID, telegram.EscapeMarkdownV2("Clients menu closed."), emptyKb)
}

func (h *ClientsHandler) handleRemoveConfirm(chatID int64, msgID int, ip string) {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}

	clients := vpnconfig.CollectClients(cfg)
	route := ""
	for _, c := range clients {
		if c.IP == ip {
			route = c.Route
			break
		}
	}
	if route == "" {
		h.handleRefreshList(chatID, msgID)
		return
	}

	text := telegram.EscapeMarkdownV2(fmt.Sprintf("Remove %s from %s?", ip, route))
	kb := telegram.NewKeyboard()
	kb.Button("Yes, remove", fmt.Sprintf("clients:rm_yes:%s", ip))
	kb.Button("Cancel", "clients:rm_no")
	kb.Row()

	h.deps.Sender.EditMessage(chatID, msgID, text, kb.Build())
}

func (h *ClientsHandler) handleRemove(chatID int64, msgID int, ip string) {
	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}

	// Find which route this IP belongs to, remove only from that route
	clients := vpnconfig.CollectClients(cfg)
	route := ""
	for _, c := range clients {
		if c.IP == ip {
			route = c.Route
			break
		}
	}
	if route == "" {
		// IP already gone — refresh list
		text, kb := h.buildClientList(cfg)
		h.deps.Sender.EditMessage(chatID, msgID, text, kb)
		return
	}

	err = h.deps.Config.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
		if route == "xray" {
			c.Xray.Clients = removeString(c.Xray.Clients, ip)
		} else if tunnel, ok := c.TunnelDirector.Tunnels[route]; ok {
			tunnel.Clients = removeString(tunnel.Clients, ip)
			c.TunnelDirector.Tunnels[route] = tunnel
		}
		c.PausedClients = removeString(c.PausedClients, ip)
		// Gone is gone: a failover restore must not bring it back.
		vpnconfig.DetachFailoverClient(c, ip)
		cfg = c
		return nil
	})
	if err != nil {
		h.deps.Sender.SendPlain(chatID, configUpdateError(err))
		return
	}

	if err := h.deps.VPN.Apply(); err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Apply error: %v", err))
		return
	}

	text, kb := h.buildClientList(cfg)
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}
func (h *ClientsHandler) handleAddStart(chatID int64) {
	h.mu.Lock()
	h.addState[chatID] = ""
	h.mu.Unlock()

	h.deps.Sender.SendPlain(chatID, "Enter client IP address (e.g. 192.168.50.10 or 192.168.50.0/24):")
}

// HandleTextInput handles text messages for the add-client flow.
func (h *ClientsHandler) HandleTextInput(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	h.mu.Lock()
	pendingIP, inAddState := h.addState[chatID]
	h.mu.Unlock()

	if !inAddState || pendingIP != "" {
		return
	}

	input := strings.TrimSpace(msg.Text)
	if input == "" {
		return
	}

	normalized, err := vpnconfig.NormalizeClientAddr(input)
	if err != nil {
		h.deps.Sender.SendPlain(chatID, "Invalid format. Enter IPv4 (192.168.50.10) or CIDR (192.168.50.0/24):")
		return
	}

	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}

	// Compare normalized forms: older configs store both 1.2.3.4 and 1.2.3.4/32.
	clients := vpnconfig.CollectClients(cfg)
	for _, c := range clients {
		existing, err := vpnconfig.NormalizeClientAddr(c.IP)
		if err != nil {
			existing = c.IP
		}
		if existing == normalized {
			h.deps.Sender.SendPlain(chatID, fmt.Sprintf("This IP is already configured for %s", c.Route))
			return
		}
	}

	// Save pending IP (normalized) and show route selection
	h.mu.Lock()
	h.addState[chatID] = normalized
	h.mu.Unlock()

	h.showRouteSelection(chatID, normalized, cfg)
}

// showRouteSelection offers xray, every tunnel the platform lists, and the
// tunnels already in the config the platform does not list (so an existing
// route stays reachable), marked "(unknown)" the way the Web UI marks them.
// When the platform cannot be asked the config's tunnels are all there is, and
// the user is told: the tunnel is not unknown there, the platform is, so those
// keep their bare names.
func (h *ClientsHandler) showRouteSelection(chatID int64, ip string, cfg *vpnconfig.VPNDirectorConfig) {
	kb := telegram.NewKeyboard()

	kb.Button("xray", "clients:route:xray").Row()

	listed := map[string]bool{}
	platformAnswered := true
	info, err := h.deps.VPN.Platform()
	if err != nil {
		platformAnswered = false
		h.deps.Sender.SendPlain(chatID, "platform info unavailable; offering the configured tunnels only")
	} else {
		for _, t := range info.Tunnels {
			label := t.ID
			if t.Description != "" {
				label += " " + t.Description
			}
			if !t.Connected {
				label += " (down)"
			}
			kb.Button(label, fmt.Sprintf("clients:route:%s", t.ID)).Row()
			listed[t.ID] = true
		}
	}

	tunnelNames := make([]string, 0, len(cfg.TunnelDirector.Tunnels))
	for name := range cfg.TunnelDirector.Tunnels {
		if !listed[name] {
			tunnelNames = append(tunnelNames, name)
		}
	}
	sort.Strings(tunnelNames)
	for _, name := range tunnelNames {
		label := name
		if platformAnswered {
			label += " (unknown)"
		}
		kb.Button(label, fmt.Sprintf("clients:route:%s", name)).Row()
	}

	kb.Button("Cancel", "clients:route:cancel").Row()

	text := telegram.EscapeMarkdownV2(fmt.Sprintf("Select route for %s:", ip))
	h.deps.Sender.SendWithKeyboard(chatID, text, kb.Build())
}

func (h *ClientsHandler) handleAddRoute(chatID int64, msgID int, route string) {
	if route == "cancel" {
		h.ClearState(chatID)
		h.handleRefreshList(chatID, msgID)
		return
	}

	h.mu.Lock()
	ip, ok := h.addState[chatID]
	delete(h.addState, chatID)
	h.mu.Unlock()

	if !ok || ip == "" {
		return
	}

	cfg, err := h.deps.Config.LoadVPNConfig()
	if err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		return
	}

	// Normalize IP: strip /32 for consistent storage. The pending state already
	// holds the normalized form; this keeps older pending entries consistent.
	if n, err := vpnconfig.NormalizeClientAddr(ip); err == nil {
		ip = n
	}

	if route != "xray" {
		if _, ok := cfg.TunnelDirector.Tunnels[route]; !ok {
			// Not configured yet: fine when the router has the tunnel (the
			// keyboard listed it from the platform), stale otherwise. Either way
			// the client is not added, so say why - a redrawn list that simply
			// lacks the new row reads as a bug.
			info, err := h.deps.VPN.Platform()
			if err != nil {
				h.deps.Sender.SendPlain(chatID, "platform info unavailable, try again")
				text, kb := h.buildClientList(cfg)
				h.deps.Sender.EditMessage(chatID, msgID, text, kb)
				return
			}
			if !info.HasTunnel(route) {
				h.deps.Sender.SendPlain(chatID, fmt.Sprintf("route %s is no longer available", route))
				text, kb := h.buildClientList(cfg)
				h.deps.Sender.EditMessage(chatID, msgID, text, kb)
				return
			}
		}
	}

	err = h.deps.Config.UpdateVPNConfig(func(c *vpnconfig.VPNDirectorConfig) error {
		// Where the user puts an address during a failover is where it goes:
		// a restore must not take it back to Xray. One added as xray while
		// Xray is down joins the failover afresh.
		vpnconfig.DetachFailoverClient(c, ip)
		if route == "xray" {
			c.Xray.Clients = append(c.Xray.Clients, ip)
			cfg = c
			return nil
		}
		if c.TunnelDirector.Tunnels == nil {
			c.TunnelDirector.Tunnels = make(map[string]vpnconfig.TunnelConfig)
		}
		tunnel, ok := c.TunnelDirector.Tunnels[route]
		if !ok {
			// A new tunnel inherits the Xray country exclusions, like the
			// Web UI and the configure wizard: an empty exclude would route
			// the client's local-country traffic through the tunnel as well.
			tunnel = vpnconfig.TunnelConfig{
				Clients: []string{},
				Exclude: append([]string{}, c.Xray.ExcludeSets...),
			}
		}
		tunnel.Clients = append(tunnel.Clients, ip)
		c.TunnelDirector.Tunnels[route] = tunnel
		cfg = c
		return nil
	})
	if err != nil {
		h.deps.Sender.SendPlain(chatID, configUpdateError(err))
		return
	}

	if err := h.deps.VPN.Apply(); err != nil {
		h.deps.Sender.SendPlain(chatID, fmt.Sprintf("Apply error: %v", err))
		return
	}

	text, kb := h.buildClientList(cfg)
	h.deps.Sender.EditMessage(chatID, msgID, text, kb)
}
