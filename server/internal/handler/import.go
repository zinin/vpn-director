// internal/handler/import.go
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// importTimeout bounds one /import, or one refresh from /subs: the downloads
// and the resolution of every host in them.
const importTimeout = 3 * time.Minute

// ImportHandler handles /import. "/import <url> [name]" adds a subscription -
// or refreshes the one saved with that link - and "/import" alone refreshes
// every subscription that has a link.
type ImportHandler struct {
	deps       *Deps
	httpClient *http.Client
}

// NewImportHandler creates an ImportHandler that downloads through the
// SSRF-hardened client.
func NewImportHandler(deps *Deps) *ImportHandler {
	return &ImportHandler{deps: deps, httpClient: ssrf.NewClient(30 * time.Second)}
}

// HandleImport handles /import.
func (h *ImportHandler) HandleImport(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()

	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		h.refreshAll(ctx, chatID)
		return
	}
	// The name is the rest of the line: it may hold spaces.
	rawURL, name, _ := strings.Cut(args, " ")
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Loading the subscription..."))
	res := service.AddSubscription(ctx, h.deps.Config, h.httpClient, rawURL, strings.TrimSpace(name))
	if res.Err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(importFailure(res.Err)))
		return
	}
	h.deps.Sender.Send(chatID, importReport(res))
}

// refreshAll refreshes every subscription that has a link, a line each.
func (h *ImportHandler) refreshAll(ctx context.Context, chatID int64) {
	subs, err := h.deps.Config.LoadSubscriptions()
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	linked := 0
	for _, s := range subs {
		if !s.Static() {
			linked++
		}
	}
	if linked == 0 {
		h.deps.Sender.Send(chatID, "Usage: `/import <url> [name]` adds a subscription; `/import` alone refreshes them all")
		return
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(fmt.Sprintf("Refreshing %d subscriptions...", linked)))
	results, err := service.RefreshAllSubscriptions(ctx, h.deps.Config, h.httpClient)
	if err != nil {
		h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2("Error: "+err.Error()))
		return
	}
	lines := make([]string, 0, len(results))
	for _, r := range results {
		lines = append(lines, r.Line())
	}
	h.deps.Sender.Send(chatID, telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
}

// importFailure is what /import says when an add failed. Only a failure after
// the file was written says the subscription is saved; every other one saved
// nothing.
func importFailure(err error) string {
	if errors.Is(err, vpnconfig.ErrServersSaved) {
		return fmt.Sprintf("Warning: the subscription is saved, but xray.servers sync failed: %v", err)
	}
	return fmt.Sprintf("Import failed, nothing was imported: %v", err)
}

// importReport is the reply to an add: what came in, by country, and what was
// left out and why.
func importReport(res service.SubscriptionResult) string {
	imp := res.Import
	counts := imp.Counts()
	head := fmt.Sprintf("%s: Imported %d servers:", res.Name, len(imp.Servers))
	if counts != "" {
		head = fmt.Sprintf("%s: Imported %d of %d servers:", res.Name, len(imp.Servers), imp.Total)
	}
	var sb strings.Builder
	sb.WriteString(telegram.EscapeMarkdownV2(head) + "\n")
	sb.WriteString(telegram.EscapeMarkdownV2(groupServersByCountry(imp.Servers)))
	if counts != "" {
		lines := append([]string{counts}, imp.Details(3)...)
		sb.WriteString("\n\n" + telegram.EscapeMarkdownV2(strings.Join(lines, "\n")))
	}
	if res.Existed {
		sb.WriteString("\n\n" + telegram.EscapeMarkdownV2("The link was saved already; its list was refreshed."))
	}
	return sb.String()
}
