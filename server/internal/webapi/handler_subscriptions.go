package webapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// subscriptionView is a subscription as the page shows it: the host stands in
// for the link, whose path carries the subscription token.
type subscriptionView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Static    bool      `json:"static"`
	Servers   int       `json:"servers"`
	Added     time.Time `json:"added"`
	Refreshed time.Time `json:"refreshed"`
	Error     string    `json:"error,omitempty"`
}

func newSubscriptionView(s vpnconfig.Subscription) subscriptionView {
	return subscriptionView{ID: s.ID, Name: s.Name, Host: s.Host(), Static: s.Static(), Servers: len(s.Servers),
		Added: s.Added, Refreshed: s.Refreshed, Error: s.Error}
}

// importClient is the client subscription downloads go through: the
// SSRF-hardened one, unless a test put its own in Deps.
func importClient(deps *Deps) *http.Client {
	if deps.ImportClient != nil {
		return deps.ImportClient
	}
	return ssrf.NewClient(10 * time.Second)
}

// resultView is one add or refresh as the page reads it: the line it shows
// and, for a success, the counts that line is made of.
func resultView(r service.SubscriptionResult) map[string]interface{} {
	v := map[string]interface{}{"id": r.ID, "name": r.Name, "existed": r.Existed, "summary": r.Line()}
	if r.Err != nil {
		v["error"] = r.ErrorText()
		return v
	}
	v["count"] = len(r.Import.Servers)
	v["total"] = r.Import.Total
	v["skipped"] = r.Import.SkippedByReason()
	v["dns_errors"] = r.Import.ResolveErrors
	return v
}

// subscriptionErrorStatus is the status a failed subscription route answers
// with: 502 for a subscription that did not arrive, 400 for a request or a
// body the rules refuse, 404 for a subscription that is not there.
func subscriptionErrorStatus(err error) int {
	var de *service.DownloadError
	var be *service.BodyError
	switch {
	case errors.As(err, &de):
		return http.StatusBadGateway
	case errors.As(err, &be),
		errors.Is(err, service.ErrSubscriptionURL),
		errors.Is(err, vpnconfig.ErrSubscriptionName),
		errors.Is(err, vpnconfig.ErrSubscriptionNameTaken),
		errors.Is(err, vpnconfig.ErrSubscriptionLimit),
		errors.Is(err, vpnconfig.ErrSubscriptionStatic):
		return http.StatusBadRequest
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// subscriptionErrorText is what the page shows for err. A failure after the
// file was written says the subscription is saved: the user must not redo an
// add that happened. A delete words that case itself.
func subscriptionErrorText(err error) string {
	switch {
	case errors.Is(err, vpnconfig.ErrServersSaved):
		return "subscription saved, but xray.servers sync failed: " + err.Error()
	case errors.Is(err, service.ErrConfigLockTimeout):
		return "config is busy; nothing was saved"
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		return "no such subscription"
	}
	return err.Error()
}

// subscriptionID is the id an action names: ?id=, which must be there.
func subscriptionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonError(w, http.StatusBadRequest, "id is required")
		return "", false
	}
	return id, true
}

// handleListSubscriptions answers every subscription, in order, without links.
func handleListSubscriptions(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load subscriptions")
			return
		}
		views := make([]subscriptionView, 0, len(subs))
		for _, s := range subs {
			views = append(views, newSubscriptionView(s))
		}
		jsonOK(w, map[string]interface{}{"subscriptions": views})
	}
}

type addSubscriptionRequest struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

// handleAddSubscription downloads a link and saves it as a subscription; a
// link already saved is refreshed instead (service.AddSubscription).
func handleAddSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req addSubscriptionRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.URL = strings.TrimSpace(req.URL)
		if req.URL == "" {
			jsonError(w, http.StatusBadRequest, "url is required")
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		ctx, cancel := context.WithTimeout(context.Background(), subscriptionTimeout)
		defer cancel()
		res := service.AddSubscription(ctx, deps.Config, importClient(deps), req.URL, req.Name)
		if res.Err != nil {
			jsonError(w, subscriptionErrorStatus(res.Err), subscriptionErrorText(res.Err))
			return
		}
		v := resultView(res)
		v["ok"] = true
		jsonOK(w, v)
	}
}

// handleRefreshSubscriptions refreshes the subscription ?id= names, or every
// subscription with a link. A download that fails is a result, not an error:
// the answer is 200 and the result says why.
func handleRefreshSubscriptions(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		ctx, cancel := context.WithTimeout(context.Background(), subscriptionTimeout)
		defer cancel()
		var results []service.SubscriptionResult
		if id == "" {
			all, err := service.RefreshAllSubscriptions(ctx, deps.Config, importClient(deps))
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to load subscriptions")
				return
			}
			results = all
		} else {
			res := service.RefreshSubscription(ctx, deps.Config, importClient(deps), id)
			if errors.Is(res.Err, vpnconfig.ErrSubscriptionGone) || errors.Is(res.Err, vpnconfig.ErrSubscriptionStatic) {
				jsonError(w, subscriptionErrorStatus(res.Err), subscriptionErrorText(res.Err))
				return
			}
			results = []service.SubscriptionResult{res}
		}
		views := make([]map[string]interface{}, 0, len(results))
		for _, res := range results {
			views = append(views, resultView(res))
		}
		jsonOK(w, map[string]interface{}{"results": views})
	}
}

type renameSubscriptionRequest struct {
	Name string `json:"name"`
}

// handleRenameSubscription gives the subscription ?id= names a new name.
func handleRenameSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := subscriptionID(w, r)
		if !ok {
			return
		}
		var req renameSubscriptionRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		if err := service.RenameSubscription(deps.Config, id, req.Name); err != nil {
			jsonError(w, subscriptionErrorStatus(err), subscriptionErrorText(err))
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// handleDeleteSubscription removes the subscription ?id= names. The running
// Xray is left alone; active_removed says it came from that subscription.
func handleDeleteSubscription(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := subscriptionID(w, r)
		if !ok {
			return
		}
		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		active, err := service.DeleteSubscription(deps.Config, id)
		if errors.Is(err, vpnconfig.ErrServersSaved) {
			// The file is gone and only the config beside it is stale: the
			// user must not read the delete as failed, nor miss that the
			// running server came from it.
			msg := "subscription deleted, but xray.servers sync failed: " + err.Error()
			if active {
				msg += ". The running server came from it; select another server."
			}
			jsonError(w, http.StatusInternalServerError, msg)
			return
		}
		if err != nil {
			jsonError(w, subscriptionErrorStatus(err), subscriptionErrorText(err))
			return
		}
		jsonOK(w, map[string]bool{"ok": true, "active_removed": active})
	}
}
