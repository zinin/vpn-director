// internal/handler/import.go
package handler

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vless"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// ImportHandler handles /import command
type ImportHandler struct {
	deps        *Deps
	httpClient  *http.Client
	maxBodySize int64
}

// NewImportHandler creates a new ImportHandler with default timeout and size limits
func NewImportHandler(deps *Deps) *ImportHandler {
	return &ImportHandler{
		deps:        deps,
		httpClient:  ssrf.NewClient(30 * time.Second), // SSRF-hardened (validates resolved IP at dial time)
		maxBodySize: 1 << 20,                          // 1MB
	}
}

// HandleImport handles /import command - downloads and imports VLESS subscription
func (h *ImportHandler) HandleImport(msg *tgbotapi.Message) {
	args := msg.CommandArguments()
	fetchURL := args
	if fetchURL == "" {
		cfg, err := h.deps.Config.LoadVPNConfig()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Config load error: "+err.Error()))
			return
		}
		if err == nil && cfg != nil {
			fetchURL = cfg.Xray.SubscriptionURL
		}
	}
	if fetchURL == "" {
		h.deps.Sender.Send(msg.Chat.ID, "Usage: `/import [url]`")
		return
	}

	// Validate URL scheme
	parsedURL, err := url.Parse(fetchURL)
	if err != nil || parsedURL.Scheme != "https" {
		h.deps.Sender.Send(msg.Chat.ID, "Invalid URL\\. Use https://")
		return
	}

	h.deps.Sender.Send(msg.Chat.ID, "Loading server list\\.\\.\\.")

	// Download subscription
	resp, err := h.httpClient.Get(fetchURL)
	if err != nil {
		// A *url.Error carries the whole URL, and a saved link's token must
		// not reach the chat.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Download error: %v", err)))
		return
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		h.deps.Sender.Send(msg.Chat.ID, fmt.Sprintf("Error: HTTP %d", resp.StatusCode))
		return
	}

	// One byte past the cap tells a list that is too long from one that fits
	// exactly: cut at the cap, base64 decodes to a shorter list, and that would
	// be published as the subscription.
	body, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBodySize+1))
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Read error: %v", err)))
		return
	}
	if int64(len(body)) > h.maxBodySize {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2("Error: subscription exceeds 1 MiB; nothing was imported"))
		return
	}

	// Decode VLESS subscription and resolve IPs for each server
	result := vless.DecodeAndResolve(string(body))
	if result.Parsed == 0 {
		var sb strings.Builder
		sb.WriteString("No VLESS servers found")
		if len(result.ParseErrors) > 0 {
			sb.WriteString("\nErrors:\n")
			for _, e := range result.ParseErrors {
				sb.WriteString(fmt.Sprintf("- %s\n", e))
			}
		}
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(sb.String()))
		return
	}

	if len(result.Servers) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, "Could not resolve IP for any server")
		return
	}

	// servers.json and the xray.servers bypass list go out together, under the
	// config lock, so a Web UI import or the watch cannot leave one of ours
	// beside one of theirs. A missing vpn-director.json is not an error here:
	// /import works before the first configure, and the wizard writes
	// xray.servers itself.
	// "Imported" is said only for a list that was written. The lock, a config
	// a re-import cannot read and a refusal all fail before it.
	err = service.PublishImport(h.deps.Config, result.Servers, args, fetchURL)
	switch {
	case err == nil:
	case errors.Is(err, vpnconfig.ErrServersSaved):
		// Only the config beside the list is missing. With no config at all
		// there is nothing to keep in step with.
		if !errors.Is(err, service.ErrConfigLoad) {
			h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(
				fmt.Sprintf("Warning: servers imported but xray.servers sync failed: %v", err)))
		}
	case errors.Is(err, vpnconfig.ErrSubscriptionChanged):
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(
			"The saved subscription changed while downloading; nothing was imported. Run /import again."))
		return
	case errors.Is(err, vpnconfig.ErrSaveServers):
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Save error: %v", err)))
		return
	default:
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(
			fmt.Sprintf("Import failed, nothing was imported: %v", err)))
		return
	}

	// Build response with grouped stats
	var sb strings.Builder
	if result.ResolveErrors > 0 || len(result.ParseErrors) > 0 {
		sb.WriteString(fmt.Sprintf("Imported %d of %d servers:\n", len(result.Servers), result.Parsed))
	} else {
		sb.WriteString(fmt.Sprintf("Imported %d servers:\n", len(result.Servers)))
	}

	groupedStr := groupServersByCountry(result.Servers)
	sb.WriteString(telegram.EscapeMarkdownV2(groupedStr))

	if result.ResolveErrors > 0 || len(result.ParseErrors) > 0 {
		sb.WriteString("\n\n")
		if result.ResolveErrors > 0 {
			sb.WriteString(fmt.Sprintf("%d DNS errors", result.ResolveErrors))
		}
		if len(result.ParseErrors) > 0 {
			if result.ResolveErrors > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%d parse errors", len(result.ParseErrors)))
		}
	}

	h.deps.Sender.Send(msg.Chat.ID, sb.String())
}
