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

// XrayHandler handles /xray command for quick server switching
type XrayHandler struct {
	deps *Deps
}

// NewXrayHandler creates a new XrayHandler
func NewXrayHandler(deps *Deps) *XrayHandler {
	return &XrayHandler{deps: deps}
}

// HandleXray handles /xray command - shows server selection keyboard
func (h *XrayHandler) HandleXray(msg *tgbotapi.Message) {
	servers, err := h.deps.Config.LoadServers()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}

	if len(servers) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Серверы не найдены. Используйте /import для импорта"))
		return
	}

	kb := telegram.NewKeyboard()
	for i, srv := range servers {
		btnText := fmt.Sprintf("%d. %s", i+1, srv.Name)
		kb.Button(btnText, fmt.Sprintf("xray:select:%d:%s", i, serverFingerprint(srv)))
	}
	kb.Columns(2)

	text := telegram.EscapeMarkdownV2("Выберите сервер:")
	h.deps.Sender.SendWithKeyboard(msg.Chat.ID, text, kb.Build())
}

// serverFingerprint names a server in a button: the first 8 hex digits of
// sha256("name|address|port"). The list can change between /xray and the tap -
// the subscription watch rotates endpoints, an import replaces it - and the
// index alone then names another server.
func serverFingerprint(s vpnconfig.Server) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", s.Name, s.Address, s.Port)))
	return hex.EncodeToString(sum[:4])
}

// HandleCallback handles xray:select:{index}:{fingerprint} callbacks, and the
// xray:select:{index} of a keyboard sent before buttons carried a fingerprint.
func (h *XrayHandler) HandleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil {
		return
	}

	chatID := cb.Message.Chat.ID
	data := cb.Data

	// Parse server index from "xray:select:N"
	if !strings.HasPrefix(data, "xray:select:") {
		return
	}

	idxStr, fingerprint, _ := strings.Cut(strings.TrimPrefix(data, "xray:select:"), ":")
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Ошибка: неверный индекс"))
		return
	}

	// Load servers
	servers, err := h.deps.Config.LoadServers()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка: %v", err)))
		return
	}

	if idx < 0 || idx >= len(servers) {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Ошибка: сервер не найден"))
		return
	}

	server := servers[idx]
	if fingerprint != "" && fingerprint != serverFingerprint(server) {
		// The keyboard is stale: replace it rather than leave more taps on it.
		text := telegram.EscapeMarkdownV2("The server list has changed since these buttons were sent; run /xray again")
		empty := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		h.deps.Sender.EditMessage(chatID, cb.Message.MessageID, text, empty)
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

	// Restart Xray
	if err := h.deps.VPN.RestartXray(); err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Ошибка перезапуска: %v", err)))
		return
	}

	// Edit original message to show result (removes keyboard)
	successText := telegram.EscapeMarkdownV2(fmt.Sprintf("✓ Переключено на %s", server.Name))
	emptyKeyboard := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
	h.deps.Sender.EditMessage(chatID, cb.Message.MessageID, successText, emptyKeyboard)
}
