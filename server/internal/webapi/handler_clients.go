package webapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// handleListClients returns a handler that lists all VPN clients with their
// route assignment and pause status.
func handleListClients(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		cfg, err := deps.Config.LoadVPNConfig()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load configuration")
			return
		}
		clients := vpnconfig.CollectClients(cfg)
		jsonOK(w, map[string]interface{}{"clients": clients})
	}
}

// addClientRequest is the expected JSON body for POST /api/clients.
type addClientRequest struct {
	IP    string `json:"ip"`
	Route string `json:"route"`
}

// handleAddClient returns a handler that adds a client address to the
// specified route and applies the configuration. The address is normalized
// (IPv4 only, /32 stripped), must not already be configured in any route,
// and a newly created tunnel inherits xray.exclude_sets like the bot wizard.
func handleAddClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req addClientRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if req.IP == "" {
			jsonError(w, http.StatusBadRequest, "ip is required")
			return
		}
		ip, err := vpnconfig.NormalizeClientAddr(req.IP)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Route == "" {
			jsonError(w, http.StatusBadRequest, "route is required")
			return
		}
		if !checkClientRoute(w, deps, req.Route) {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		writeSaveApplyResult(w, updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			if existing, found := findClient(cfg, ip); found {
				return &httpError{status: http.StatusConflict, msg: fmt.Sprintf("client already configured for %s", existing.route)}
			}
			// Where the user puts an address during a failover is where it
			// goes: a restore must not take it back to Xray. One added as
			// xray while Xray is down joins the failover afresh.
			vpnconfig.DetachFailoverClient(cfg, ip)
			if req.Route == "xray" {
				cfg.Xray.Clients = append(cfg.Xray.Clients, ip)
				return nil
			}
			if cfg.TunnelDirector.Tunnels == nil {
				cfg.TunnelDirector.Tunnels = make(map[string]vpnconfig.TunnelConfig)
			}
			tunnel, ok := cfg.TunnelDirector.Tunnels[req.Route]
			if !ok {
				// A new tunnel inherits the Xray country exclusions, like the
				// bot's configure wizard. An empty exclude would route the
				// client's local-country traffic through the tunnel as well.
				tunnel = vpnconfig.TunnelConfig{
					Clients: []string{},
					Exclude: append([]string{}, cfg.Xray.ExcludeSets...),
				}
			}
			tunnel.Clients = append(tunnel.Clients, ip)
			cfg.TunnelDirector.Tunnels[req.Route] = tunnel
			return nil
		}))
	}
}

// checkClientRoute answers whether route may take a client, for an add and a
// move alike: xray, a tunnel already in the config, or a tunnel this router
// has, asked of the platform now - the list is the firmware's (Merlin
// wgcN/ovpncN, Keenetic OpenVPN0, Wireguard1, ...) and a tunnel can appear or
// go at any time. A configured tunnel is accepted without the platform, as the
// bot and the wizard accept it: ClientsTab offers exactly those when the
// platform cannot be asked, and a 503 here made that fallback unusable. An
// empty list is not an answer either: on Keenetic `vpn-director.sh platform`
// prints "tunnels": [] and exits 0 while RCI does not reply (spec 13). It
// writes the error response itself and returns false when the handler must
// stop.
func checkClientRoute(w http.ResponseWriter, deps *Deps, route string) bool {
	if route == "xray" {
		return true
	}
	cfg, err := deps.Config.LoadVPNConfig()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load configuration")
		return false
	}
	if _, configured := cfg.TunnelDirector.Tunnels[route]; configured {
		return true
	}
	extendWriteDeadline(w, statusDeadline)
	info, err := deps.VPN.Platform()
	if err != nil || len(info.Tunnels) == 0 {
		jsonError(w, http.StatusServiceUnavailable, "platform info unavailable")
		return false
	}
	if !info.HasTunnel(route) {
		jsonError(w, http.StatusBadRequest, "invalid route: must be xray or a tunnel this router has")
		return false
	}
	return true
}

// moveClientRequest is the expected JSON body for POST /api/clients/route.
type moveClientRequest struct {
	IP    string `json:"ip"`
	Route string `json:"route"`
}

// errClientUnchanged ends a move whose route already holds the address alone:
// the update writes nothing, and there is nothing to apply.
var errClientUnchanged = errors.New("client already on that route")

// handleMoveClient returns a handler that moves a client to another route in
// one config change and one apply (vpnconfig.MoveClient). The apply moves it
// make-before-break: the client stays on its old route until the new one
// carries it, so none of its packets leaves through the WAN meanwhile - a
// delete and an add left it there for as long as the user took between them.
func handleMoveClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req moveClientRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.IP == "" {
			jsonError(w, http.StatusBadRequest, "ip is required")
			return
		}
		ip, err := vpnconfig.NormalizeClientAddr(req.IP)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Route == "" {
			jsonError(w, http.StatusBadRequest, "route is required")
			return
		}
		if !checkClientRoute(w, deps, req.Route) {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		err = updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			switch vpnconfig.MoveClient(cfg, ip, req.Route) {
			case vpnconfig.ClientNotFound:
				return &httpError{status: http.StatusNotFound, msg: "client not found"}
			case vpnconfig.ClientAlreadyThere:
				return errClientUnchanged
			}
			return nil
		})
		if errors.Is(err, errClientUnchanged) {
			jsonOK(w, map[string]bool{"ok": true})
			return
		}
		writeSaveApplyResult(w, err)
	}
}

// handlePauseClient returns a handler that pauses a configured client.
// The paused entry keeps the stored spelling of the address because the
// shell subtracts paused_clients from the clients arrays by exact string.
func handlePauseClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, ok := clientAddrFromQuery(w, r)
		if !ok {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		writeSaveApplyResult(w, updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			stored := storedSpellings(cfg, ip)
			if len(stored) == 0 {
				return &httpError{status: http.StatusNotFound, msg: "client not found"}
			}
			// Drop every equivalent spelling and write back the ones actually in
			// use. lib/config.sh subtracts paused_clients from the clients arrays
			// by exact string, and CollectClients reports Paused by exact lookup,
			// so an entry spelled differently from the client pauses nothing while
			// reporting success. One address can also sit in two routes in two
			// spellings - 1.2.3.4 in xray, 1.2.3.4/32 in a tunnel - and keeping
			// only the first would silently resume the other.
			cfg.PausedClients = append(removeAddr(cfg.PausedClients, ip), stored...)
			return nil
		}))
	}
}

// handleResumeClient returns a handler that resumes a paused client.
func handleResumeClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, ok := clientAddrFromQuery(w, r)
		if !ok {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		writeSaveApplyResult(w, updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			if _, found := findClient(cfg, ip); !found {
				return &httpError{status: http.StatusNotFound, msg: "client not found"}
			}
			cfg.PausedClients = removeAddr(cfg.PausedClients, ip)
			return nil
		}))
	}
}

// handleDeleteClient returns a handler that removes a client from all routes
// and from the paused list, matching every stored spelling of the address.
func handleDeleteClient(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, ok := clientAddrFromQuery(w, r)
		if !ok {
			return
		}

		unlock, ok := lockLongOp(w, r, deps, applyDeadline)
		if !ok {
			return
		}
		defer unlock()

		writeSaveApplyResult(w, updateAndApply(deps, func(cfg *vpnconfig.VPNDirectorConfig) error {
			if _, found := findClient(cfg, ip); !found {
				return &httpError{status: http.StatusNotFound, msg: "client not found"}
			}
			cfg.Xray.Clients = removeAddr(cfg.Xray.Clients, ip)
			for name, tunnel := range cfg.TunnelDirector.Tunnels {
				tunnel.Clients = removeAddr(tunnel.Clients, ip)
				cfg.TunnelDirector.Tunnels[name] = tunnel
			}
			cfg.PausedClients = removeAddr(cfg.PausedClients, ip)
			// Gone is gone: the restore must not bring it back, and an
			// address added again later is a new client, not the snapshot.
			vpnconfig.DetachFailoverClient(cfg, ip)
			return nil
		}))
	}
}

// clientMatch describes a configured client found by normalized address.
type clientMatch struct {
	route  string // "xray" or the tunnel name
	stored string // the address exactly as stored in vpn-director.json
}

// findClient looks addr up across xray.clients and every tunnel, comparing
// normalized forms because older configs store both 1.2.3.4 and 1.2.3.4/32.
func findClient(cfg *vpnconfig.VPNDirectorConfig, addr string) (clientMatch, bool) {
	for _, c := range vpnconfig.CollectClients(cfg) {
		if sameAddr(c.IP, addr) {
			return clientMatch{route: c.Route, stored: c.IP}, true
		}
	}
	return clientMatch{}, false
}

// storedSpellings returns every stored form of addr, in config order and
// without repeats. Callers that write paused_clients need all of them: the
// shell matches those entries literally.
func storedSpellings(cfg *vpnconfig.VPNDirectorConfig, addr string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, c := range vpnconfig.CollectClients(cfg) {
		if !sameAddr(c.IP, addr) || seen[c.IP] {
			continue
		}
		seen[c.IP] = true
		out = append(out, c.IP)
	}
	return out
}

// clientAddrFromQuery reads and normalizes the ip query parameter. It writes
// the error response itself and returns ok=false when the handler must stop.
//
// An address that fails normalization is passed through verbatim instead of
// being rejected: older builds accepted IPv6 and a hand-edited config can hold
// anything, GET /api/clients still lists such an entry, and a 400 here would
// leave it impossible to pause or delete from the UI. findClient compares
// unparseable entries verbatim, so a value matching nothing still ends as
// 404 client not found rather than touching the config.
func clientAddrFromQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("ip"))
	if raw == "" {
		jsonError(w, http.StatusBadRequest, "ip query parameter is required")
		return "", false
	}
	if ip, err := vpnconfig.NormalizeClientAddr(raw); err == nil {
		return ip, true
	}
	return raw, true
}

// sameAddr reports whether stored denotes the same address as the normalized
// addr. Entries that fail normalization are compared verbatim.
func sameAddr(stored, addr string) bool {
	normalized, err := vpnconfig.NormalizeClientAddr(stored)
	if err != nil {
		normalized = stored
	}
	return normalized == addr
}

// containsAddr reports whether slice holds addr in any stored spelling.
func containsAddr(slice []string, addr string) bool {
	for _, s := range slice {
		if sameAddr(s, addr) {
			return true
		}
	}
	return false
}

// removeAddr returns a new slice without every entry that denotes addr,
// whatever its stored spelling.
func removeAddr(slice []string, addr string) []string {
	result := make([]string, 0, len(slice))
	for _, s := range slice {
		if !sameAddr(s, addr) {
			result = append(result, s)
		}
	}
	return result
}
