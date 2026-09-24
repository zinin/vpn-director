package webapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// serverView is a server as the Servers tab shows it. The record behind it
// also holds the outbound and its credentials - an id, a password - and the
// page needs none of them.
type serverView struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	Port     int      `json:"port"`
	IPs      []string `json:"ips"`
	Protocol string   `json:"protocol"`
}

// subscriptionServers is one subscription's servers as the Servers tab shows them.
type subscriptionServers struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Servers []serverView `json:"servers"`
}

// handleListServers answers the servers of every subscription, grouped, and
// the record of which one runs.
func handleListServers(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		// The list on its own cannot say which entry is running: a
		// subscription routinely puts many names behind one address:port, and
		// two subscriptions can use one name. The answer is the record a
		// selection leaves in vpn-director.json, and nil travels as null.
		cfg, err := deps.Config.LoadVPNConfig()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load vpn config")
			return
		}
		var active *vpnconfig.ActiveServer
		if cfg != nil {
			active = cfg.Xray.ActiveServer
		}
		groups := make([]subscriptionServers, 0, len(subs))
		for _, sub := range subs {
			views := make([]serverView, 0, len(sub.Servers))
			for _, s := range sub.Servers {
				ips := s.IPs
				if ips == nil {
					ips = []string{}
				}
				views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips, Protocol: s.Label()})
			}
			groups = append(groups, subscriptionServers{ID: sub.ID, Name: sub.Name, Servers: views})
		}
		jsonOK(w, map[string]interface{}{"subscriptions": groups, "active": active})
	}
}

// selectServerRequest is the expected JSON body for POST /api/servers/active.
type selectServerRequest struct {
	// Subscription is the id of the subscription the page showed the server
	// in; Index counts within that subscription's list.
	Subscription string `json:"subscription"`
	Index        *int   `json:"index"`
	// The server the page showed at that index. The list can change between the
	// page load and the click - the bot's subscription watch rotates endpoints,
	// an import in another tab replaces it - and the index then names another.
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// handleSelectServer returns a handler that selects a server by index,
// generates Xray config, updates vpn-director.json, and restarts Xray.
func handleSelectServer(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req selectServerRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if req.Index == nil {
			jsonError(w, http.StatusBadRequest, "index is required")
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		// Load the list after the lock so a concurrent import cannot change
		// which server this index names between the bounds check and the write.
		subs, err := deps.Config.LoadSubscriptions()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		si := vpnconfig.FindSubscription(subs, req.Subscription)
		if si < 0 {
			jsonError(w, http.StatusConflict, "server list changed")
			return
		}
		servers := subs[si].Servers
		if *req.Index < 0 || *req.Index >= len(servers) {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("index out of range: %d (have %d servers)", *req.Index, len(servers)))
			return
		}

		server := servers[*req.Index]
		server.Subscription = subs[si].ID
		if server.Name != req.Name || server.Address != req.Address || server.Port != req.Port {
			jsonError(w, http.StatusConflict, "server list changed")
			return
		}

		// Persist xray.servers before rewriting config.json. Generating first
		// left a new outbound on disk if the save then failed, and the next
		// xray restart would pick it up against the old vpn-director.json.
		var ports service.InboundPorts
		err = deps.Config.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
			// The union as it stands under the lock: a refresh, or a wave of
			// the bot's watch, may have published while this waited for it.
			all, err := deps.Config.LoadSubscriptions()
			if err != nil {
				return err
			}
			cfg.Xray.Servers = vpnconfig.SubscriptionIPs(all)
			// Read here, where the config is already in hand: the generated
			// inbound has to listen where the TPROXY rules send traffic.
			ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(cfg)
			return nil
		})
		if err != nil {
			if errors.Is(err, service.ErrConfigLoad) {
				jsonError(w, http.StatusInternalServerError, "failed to load vpn config")
			} else {
				jsonError(w, http.StatusInternalServerError, "failed to save vpn config")
			}
			return
		}

		// Generation and the record of it go under one lock: a switch from the
		// bot landing in between would otherwise leave config.json describing
		// its server while this one's name reaches the UI.
		generated, err := service.GenerateAndRecordActiveServer(deps.Config, deps.Xray, server, ports)
		if !generated {
			// Nothing was written, so nothing is worth restarting Xray for.
			if errors.Is(err, service.ErrConfigLoad) {
				jsonError(w, http.StatusInternalServerError, "failed to load vpn config")
			} else {
				// Xray rejects a config it cannot load, and its complaint is
				// the only thing that says which server to stop picking. The
				// bot and the wizard both pass it on; so does this.
				slog.Error("Failed to generate the Xray config", "server", server.Name, "error", err)
				jsonError(w, http.StatusInternalServerError,
					"failed to generate xray config: "+lastErrorLine(err))
			}
			return
		}
		if err != nil {
			// config.json is written and only the record is missing. The
			// server did change, and answering 500 would send the user back to
			// redo a switch that worked; the cost is a stale name in the UI
			// until the next selection.
			slog.Warn("Failed to record the active server", "server", server.Name, "error", err)
		}

		if err := deps.VPN.RestartXray(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to restart xray: "+lastErrorLine(err))
			return
		}

		jsonOK(w, map[string]bool{"ok": true})
	}
}
