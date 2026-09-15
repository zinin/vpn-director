// internal/handler/import.go
package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
		if cfg, err := h.deps.Config.LoadVPNConfig(); err == nil && cfg != nil {
			fetchURL = cfg.Xray.SubscriptionURL
		}
	}
	if fetchURL == "" {
		h.deps.Sender.Send(msg.Chat.ID, "Usage: `/import [url]`")
		return
	}

	// Validate URL scheme
	parsedURL, err := url.Parse(fetchURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		h.deps.Sender.Send(msg.Chat.ID, "Invalid URL\\. Use http:// or https://")
		return
	}

	h.deps.Sender.Send(msg.Chat.ID, "Loading server list\\.\\.\\.")

	// Download subscription
	resp, err := h.httpClient.Get(fetchURL)
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Download error: %v", err)))
		return
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		h.deps.Sender.Send(msg.Chat.ID, fmt.Sprintf("Error: HTTP %d", resp.StatusCode))
		return
	}

	// Limit body size
	body, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBodySize))
	if err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Read error: %v", err)))
		return
	}

	// Decode VLESS subscription
	servers, parseErrors := vless.DecodeSubscription(string(body))
	if len(servers) == 0 {
		var sb strings.Builder
		sb.WriteString("No VLESS servers found")
		if len(parseErrors) > 0 {
			sb.WriteString("\nErrors:\n")
			for _, e := range parseErrors {
				sb.WriteString(fmt.Sprintf("- %s\n", e))
			}
		}
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(sb.String()))
		return
	}

	// Resolve IPs for each server
	var resolved []vpnconfig.Server
	var resolveErrors int
	totalParsed := len(servers)

	for _, s := range servers {
		if err := s.ResolveIPs(); err != nil {
			resolveErrors++
			continue
		}
		resolved = append(resolved, s.ToVPNConfig())
	}

	if len(resolved) == 0 {
		h.deps.Sender.Send(msg.Chat.ID, "Could not resolve IP for any server")
		return
	}

	// Save servers (SaveServers creates directory if needed)
	if err := h.deps.Config.SaveServers(resolved); err != nil {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(fmt.Sprintf("Save error: %v", err)))
		return
	}

	seen := make(map[string]bool)
	var serverIPs []string
	for _, s := range resolved {
		for _, ip := range s.IPs {
			if ip != "" && !seen[ip] {
				seen[ip] = true
				serverIPs = append(serverIPs, ip)
			}
		}
	}
	sort.Strings(serverIPs)

	// Auto-sync xray.servers with IPs from all imported servers. A missing
	// vpn-director.json is not an error here: /import works before the first
	// configure, and the wizard writes xray.servers itself.
	err = h.deps.Config.UpdateVPNConfig(func(vpnCfg *vpnconfig.VPNDirectorConfig) error {
		vpnCfg.Xray.Servers = serverIPs
		if args != "" {
			vpnCfg.Xray.SubscriptionURL = args
		}
		return nil
	})
	if err != nil && !errors.Is(err, service.ErrConfigLoad) {
		h.deps.Sender.Send(msg.Chat.ID, telegram.EscapeMarkdownV2(
			fmt.Sprintf("Warning: servers imported but xray.servers sync failed: %v", err)))
	}

	// Build response with grouped stats
	var sb strings.Builder
	if resolveErrors > 0 || len(parseErrors) > 0 {
		sb.WriteString(fmt.Sprintf("Imported %d of %d servers:\n", len(resolved), totalParsed))
	} else {
		sb.WriteString(fmt.Sprintf("Imported %d servers:\n", len(resolved)))
	}

	groupedStr := groupServersByCountry(resolved)
	sb.WriteString(telegram.EscapeMarkdownV2(groupedStr))

	if resolveErrors > 0 || len(parseErrors) > 0 {
		sb.WriteString("\n\n")
		if resolveErrors > 0 {
			sb.WriteString(fmt.Sprintf("%d DNS errors", resolveErrors))
		}
		if len(parseErrors) > 0 {
			if resolveErrors > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%d parse errors", len(parseErrors)))
		}
	}

	h.deps.Sender.Send(msg.Chat.ID, sb.String())
}
