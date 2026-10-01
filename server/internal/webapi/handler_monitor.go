package webapi

import (
	"errors"
	"net/http"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// monitorServer is one server's status as the Servers tab shows it, matched
// with its row of GET /api/servers by index and fingerprint.
type monitorServer struct {
	Index       int    `json:"index"`
	Fingerprint string `json:"fingerprint"`
	watchdapi.ServerHealth
}

// monitorSubscription is one subscription's servers with how many are alive.
type monitorSubscription struct {
	ID      string          `json:"id"`
	Alive   int             `json:"alive"`
	Total   int             `json:"total"`
	Servers []monitorServer `json:"servers"`
}

type monitorResponse struct {
	State           watchdapi.State       `json:"state"`
	Message         string                `json:"message,omitempty"`
	IntervalSeconds int                   `json:"interval_seconds"`
	LagSeconds      int                   `json:"lag_seconds"`
	Subscriptions   []monitorSubscription `json:"subscriptions"`
}

// handleMonitor answers the status of every server: the daemon's endpoint
// states folded per server (watchdapi.Health). A daemon that does not answer
// is state not_running, every server unknown.
func handleMonitor(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		snap := watchdapi.Snapshot{State: watchdapi.StateNotRunning}
		if deps.Monitor != nil {
			if s, err := deps.Monitor.Monitor(r.Context()); err == nil {
				snap = s
			}
		}
		resp := monitorResponse{
			State: snap.State, Message: snap.Message,
			IntervalSeconds: snap.IntervalSeconds, LagSeconds: snap.LagSeconds,
			Subscriptions: make([]monitorSubscription, 0, len(subs)),
		}
		for _, sub := range subs {
			ms := monitorSubscription{ID: sub.ID, Total: len(sub.Servers), Servers: make([]monitorServer, 0, len(sub.Servers))}
			for i, s := range sub.Servers {
				s.Subscription = sub.ID
				h := watchdapi.Health(endpoint.Keys(s), snap)
				if h.Status == watchdapi.StatusAlive {
					ms.Alive++
				}
				ms.Servers = append(ms.Servers, monitorServer{Index: i, Fingerprint: vpnconfig.ServerFingerprint(s), ServerHealth: h})
			}
			resp.Subscriptions = append(resp.Subscriptions, ms)
		}
		jsonOK(w, resp)
	}
}

// monitorCheckRequest names one server as the page showed it, or nothing for
// every server.
type monitorCheckRequest struct {
	Subscription string `json:"subscription"`
	Index        *int   `json:"index"`
	Fingerprint  string `json:"fingerprint"`
}

// handleMonitorCheck queues a check of one server - every address of it - or
// of every server. 409 when the list changed since the page loaded it, or the
// monitor is stopped or disabled; 503 when the daemon does not answer.
func handleMonitorCheck(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req monitorCheckRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var keys []string
		if req.Index != nil {
			subs, err := deps.Config.LoadSubscriptions()
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to load servers")
				return
			}
			si := vpnconfig.FindSubscription(subs, req.Subscription)
			if si < 0 || *req.Index < 0 || *req.Index >= len(subs[si].Servers) {
				jsonError(w, http.StatusConflict, "server list changed")
				return
			}
			s := subs[si].Servers[*req.Index]
			s.Subscription = subs[si].ID
			if vpnconfig.ServerFingerprint(s) != req.Fingerprint {
				jsonError(w, http.StatusConflict, "server list changed")
				return
			}
			keys = endpoint.Keys(s)
		}
		if deps.Monitor == nil {
			jsonError(w, http.StatusServiceUnavailable, "the server monitor is not running")
			return
		}
		n, err := deps.Monitor.Check(r.Context(), keys)
		switch {
		case errors.Is(err, watchdapi.ErrNotActive):
			jsonError(w, http.StatusConflict, "the server monitor is stopped or disabled")
		case err != nil:
			jsonError(w, http.StatusServiceUnavailable, "the server monitor is not running")
		default:
			jsonOK(w, map[string]int{"queued": n})
		}
	}
}
