package webapi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subscription"
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

// handleListServers returns a handler that lists all imported servers.
func handleListServers(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		servers, err := deps.Config.LoadServers()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}
		// The list on its own cannot say which entry is running: a
		// subscription routinely puts many names behind one address:port. The
		// answer is the record a selection leaves in vpn-director.json, and
		// nil - "nothing has been selected" - travels as null.
		cfg, err := deps.Config.LoadVPNConfig()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load vpn config")
			return
		}
		var active *vpnconfig.ActiveServer
		if cfg != nil {
			active = cfg.Xray.ActiveServer
		}
		views := make([]serverView, 0, len(servers))
		for _, s := range servers {
			ips := s.IPs
			if ips == nil {
				ips = []string{}
			}
			views = append(views, serverView{Name: s.Name, Address: s.Address, Port: s.Port, IPs: ips, Protocol: s.Label()})
		}
		jsonOK(w, map[string]interface{}{
			"servers":            views,
			"active":             active,
			"subscription_saved": cfg != nil && cfg.Xray.SubscriptionURL != "",
		})
	}
}

// selectServerRequest is the expected JSON body for POST /api/servers/active.
type selectServerRequest struct {
	Index *int `json:"index"`
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
		servers, err := deps.Config.LoadServers()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to load servers")
			return
		}

		if *req.Index < 0 || *req.Index >= len(servers) {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("index out of range: %d (have %d servers)", *req.Index, len(servers)))
			return
		}

		server := servers[*req.Index]
		if server.Name != req.Name || server.Address != req.Address || server.Port != req.Port {
			jsonError(w, http.StatusConflict, "server list changed")
			return
		}

		// Persist xray.servers before rewriting config.json. Generating first
		// left a new outbound on disk if the save then failed, and the next
		// xray restart would pick it up against the old vpn-director.json.
		var ports service.InboundPorts
		err = deps.Config.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
			cfg.Xray.Servers = vpnconfig.ServerIPs(servers)
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

// importServersRequest is the expected JSON body for POST /api/servers/import.
type importServersRequest struct {
	URL string `json:"url"`
}

// handleImportServers returns a handler that imports servers from a VLESS
// subscription URL. It enforces HTTPS-only and SSRF protections.
func handleImportServers(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		extendWriteDeadline(w, importDeadline)

		var req importServersRequest
		if err := decodeJSON(r, &req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		cfg, _ := deps.Config.LoadVPNConfig()
		fetchURL, err := resolveSubscriptionURL(req.URL, cfg)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "url is required")
			return
		}

		parsed, err := url.Parse(fetchURL)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid URL")
			return
		}

		if parsed.Scheme != "https" {
			jsonError(w, http.StatusBadRequest, "only https URLs are allowed")
			return
		}

		// SSRF protection (pre-flight): reject obvious private/reserved hosts
		// early with a clear error. The dial-time guard in ssrf.NewClient is the
		// authoritative protection and also defeats DNS rebinding.
		host := parsed.Hostname()
		if ssrf.IsPrivateHost(host) {
			jsonError(w, http.StatusBadRequest, "URL must not point to private or loopback addresses")
			return
		}

		// Fetch the subscription with the SSRF-hardened client.
		client := deps.ImportClient
		if client == nil {
			client = ssrf.NewClient(10 * time.Second)
		}

		resp, err := client.Get(fetchURL)
		if err != nil {
			jsonError(w, http.StatusBadGateway, downloadErrMessage(err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			jsonError(w, http.StatusBadGateway, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
			return
		}

		// One byte past the cap tells a list that is too long from one that fits
		// exactly: cut at the cap, base64 decodes to a shorter list, and that
		// would be published as the subscription.
		const maxBody = 1 << 20 // 1MB
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			jsonError(w, http.StatusBadGateway, fmt.Sprintf("read body: %s", err))
			return
		}
		if len(body) > maxBody {
			jsonError(w, http.StatusBadGateway, "subscription exceeds 1 MiB")
			return
		}

		// Decode the subscription and resolve IPs. What was skipped, and why,
		// travels back to the user, as the bot's /import says it.
		result, err := subscription.DecodeAndResolve(string(body))
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if result.Parsed == 0 {
			jsonError(w, http.StatusBadRequest, result.NoServers())
			return
		}

		if len(result.Servers) == 0 {
			jsonError(w, http.StatusBadRequest, "could not resolve IP for any server")
			return
		}

		unlock, ok := lockLongOp(w, r, deps, importDeadline)
		if !ok {
			return
		}
		defer unlock()

		// One publication under the config lock: servers.json and the
		// xray.servers bypass list it is read against. Surface a persistence
		// failure instead of returning 200 with a stale xray.servers on disk,
		// and say which half landed - the client must not read a 500 that left
		// servers.json published as "nothing changed", nor one that published
		// nothing as "servers saved". Only vpnconfig.ErrServersSaved says the
		// list was written.
		if err := service.PublishImport(deps.Config, result.Servers, req.URL, fetchURL); err != nil {
			switch {
			case errors.Is(err, vpnconfig.ErrServersSaved):
				jsonError(w, http.StatusInternalServerError,
					fmt.Sprintf("servers saved, but xray.servers sync failed: %s", err))
			case errors.Is(err, vpnconfig.ErrSubscriptionChanged):
				jsonError(w, http.StatusConflict, "the saved subscription changed while downloading; nothing was imported")
			case errors.Is(err, vpnconfig.ErrSaveServers):
				jsonError(w, http.StatusInternalServerError, "failed to save servers")
			case errors.Is(err, service.ErrConfigLockTimeout):
				jsonError(w, http.StatusInternalServerError, "config is busy, servers not saved")
			default:
				jsonError(w, http.StatusInternalServerError, fmt.Sprintf("servers not saved: %s", err))
			}
			return
		}

		jsonOK(w, map[string]interface{}{
			"ok":         true,
			"count":      len(result.Servers),
			"total":      result.Total,
			"skipped":    result.SkippedByReason(),
			"dns_errors": result.ResolveErrors,
			"summary":    result.Summary(),
		})
	}
}

// downloadErrMessage builds the client-facing message for a subscription
// download failure. It echoes the underlying error for diagnostics EXCEPT when
// the SSRF dial guard blocked the connection: that error carries the resolved
// internal IP, which must not leak back to the caller. The *url.Error wrapper
// is dropped too: its text is the whole URL, and a re-import from the saved
// link must not hand its token to the browser.
func downloadErrMessage(err error) string {
	if errors.Is(err, ssrf.ErrBlockedAddress) {
		// The error carries the resolved internal IP; do not echo it back.
		return "download failed: URL resolved to a private or reserved address"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Sprintf("download failed: %s", err)
}

// resolveSubscriptionURL returns the posted URL, or the saved one when the
// client re-imports with an empty url. An empty post and nothing saved is an
// error: the import handler maps it to "url is required".
func resolveSubscriptionURL(reqURL string, cfg *vpnconfig.VPNDirectorConfig) (string, error) {
	if reqURL != "" {
		return reqURL, nil
	}
	if cfg != nil && cfg.Xray.SubscriptionURL != "" {
		return cfg.Xray.SubscriptionURL, nil
	}
	return "", errors.New("url is required")
}
