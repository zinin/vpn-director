// internal/handler/misc.go
package handler

import (
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/telegram"
)

const (
	defaultLogLines = 20
	maxLogLines     = 500
)

// MiscHandler handles miscellaneous commands
type MiscHandler struct {
	deps *Deps
}

// NewMiscHandler creates a new MiscHandler
func NewMiscHandler(deps *Deps) *MiscHandler {
	return &MiscHandler{deps: deps}
}

// HandleStart handles /start command
func (h *MiscHandler) HandleStart(msg *tgbotapi.Message) {
	text := `*VPN Director Bot*

Commands:
/status \- VPN Director status
/xray \- quick server switch
/servers \- server list
/import \[url\] \- import servers
/configure \- configuration
/restart \- restart VPN Director
/stop \- stop VPN Director
/logs \- recent logs
/ip \- external IP
/update \- update to latest release
/version \- bot version`

	h.deps.Sender.Send(msg.Chat.ID, text)
}

// HandleVersion handles /version command
func (h *MiscHandler) HandleVersion(msg *tgbotapi.Message) {
	text := h.deps.VersionFull + " (" + h.deps.Commit + ", " + h.deps.BuildDate + ")"
	if h.deps.TelegramPath != nil {
		if path := h.deps.TelegramPath(); path != "" {
			text += "\nTelegram API path: " + path
		}
	}
	h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(text))
}

// HandleIP handles /ip command
func (h *MiscHandler) HandleIP(msg *tgbotapi.Message) {
	ip, err := h.deps.Network.GetExternalIP()
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(err.Error()))
		return
	}
	h.deps.Sender.Send(msg.Chat.ID, "\U0001F310 External IP: `"+ip+"`")
}

// HandleLogs handles /logs command
func (h *MiscHandler) HandleLogs(msg *tgbotapi.Message) {
	args := strings.Fields(msg.CommandArguments())

	source := "all"
	lines := defaultLogLines

	// Parse arguments: /logs [source] [lines]
	if len(args) >= 1 {
		switch args[0] {
		case "bot", "vpn", "xray", "webui", "all":
			source = args[0]
		default:
			// Maybe it's a number
			if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
				lines = n
			} else {
				h.deps.Sender.Send(msg.Chat.ID, "Usage: `/logs [bot|vpn|xray|webui|all] [lines]`")
				return
			}
		}
	}

	if len(args) >= 2 {
		if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
			lines = n
		}
	}

	// Limit maximum lines to prevent resource exhaustion
	if lines > maxLogLines {
		lines = maxLogLines
	}

	if source == "bot" || source == "all" {
		h.sendLogFile(msg.Chat.ID, h.deps.Paths.BotLogPath, "Bot", lines)
	}

	if source == "vpn" || source == "all" {
		h.sendLogFile(msg.Chat.ID, h.deps.Paths.VPNLogPath, "VPN Director", lines)
	}

	if source == "xray" || source == "all" {
		h.sendLogFile(msg.Chat.ID, h.deps.Paths.XrayLogPath, "Xray", lines)
	}

	if source == "webui" || source == "all" {
		h.sendLogFile(msg.Chat.ID, h.deps.Paths.WebUILogPath, "Web UI", lines)
	}
}

func (h *MiscHandler) sendLogFile(chatID int64, path, name string, lines int) {
	output, err := h.deps.Logs.Read(path, lines)
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error reading "+name+" logs: "+err.Error()))
		return
	}

	if output == "" {
		output = "(empty)"
	}

	header := "\U0001F4CB *" + telegram.EscapeMarkdownV2(name) + " logs* \\(last " + strconv.Itoa(lines) + " lines\\):"
	h.deps.Sender.SendCodeBlock(chatID, header, output)
}
