package wizard

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// StateClearer is the interface for clearing wizard state
type StateClearer interface {
	Clear(chatID int64)
}

// Applier applies wizard configuration to the system
type Applier struct {
	manager StateClearer
	sender  telegram.MessageSender
	config  service.ConfigStore
	vpn     service.VPNDirector
	xray    service.XrayGenerator
}

// NewApplier creates a new Applier
func NewApplier(
	manager StateClearer,
	sender telegram.MessageSender,
	config service.ConfigStore,
	vpn service.VPNDirector,
	xray service.XrayGenerator,
) *Applier {
	return &Applier{
		manager: manager,
		sender:  sender,
		config:  config,
		vpn:     vpn,
		xray:    xray,
	}
}

// Apply applies the wizard configuration.
// IMPORTANT: State is ALWAYS cleared, even on error.
// This is intentional: router apply failures are usually config issues
// that require user to reconsider settings, not just retry.
func (a *Applier) Apply(chatID int64, state *State) error {
	// Always clear state at the end, regardless of success or failure
	defer a.manager.Clear(chatID)

	a.sender.SendPlain(chatID, "Applying configuration...")

	// Load servers
	servers, err := a.config.LoadServers()
	if err != nil {
		a.sender.SendPlain(chatID, fmt.Sprintf("Server load error: %v", err))
		return err
	}

	// Get state data with thread-safe getters
	clients := state.GetClients()
	exclusions := state.GetExclusions()
	serverIndex := state.PickedIndex(servers)

	// Build exclusion list (sorted for deterministic config)
	var excl []string
	for k, v := range exclusions {
		if v {
			excl = append(excl, k)
		}
	}
	sort.Strings(excl)
	if len(excl) == 0 {
		excl = []string{"ru"}
	}

	// A route is valid if it is xray, a tunnel the platform lists, or one
	// already in the loaded config (so a tunnel the firmware dropped still
	// gets rewritten rather than silently dropped from the wizard apply).
	validRoutes := map[string]bool{"xray": true}
	if loaded, err := a.config.LoadVPNConfig(); err == nil && loaded != nil {
		for id := range loaded.TunnelDirector.Tunnels {
			validRoutes[id] = true
		}
	}
	platformOK := false
	if info, err := a.vpn.Platform(); err == nil {
		for _, t := range info.Tunnels {
			validRoutes[t.ID] = true
		}
		// An empty list is not an answer: on Keenetic `vpn-director.sh
		// platform` prints "tunnels": [] and exits 0 while RCI does not reply
		// (spec 13), so a router with no tunnels and an RCI outage arrive
		// here as the same document.
		platformOK = len(info.Tunnels) > 0
	}

	// A route that is neither xray nor already in the config can only be
	// checked against the platform. With the platform silent, or listing no
	// tunnel at all, the loop below
	// would drop that client from the save without a word and the wizard would
	// still report success - and a transient RCI failure between picking the
	// tunnel and applying is exactly the NDM-rebuild window. Refuse the save
	// instead, so a confirmed VPN assignment is never lost quietly. A route the
	// platform does answer about and does not know stays a skip below.
	if !platformOK {
		for _, c := range clients {
			if !validRoutes[c.Route] {
				a.sender.SendPlain(chatID, "platform info unavailable, try again")
				return fmt.Errorf("platform info unavailable: cannot validate route %q for %s", c.Route, c.IP)
			}
		}
	}

	// Build new configuration
	var xrayClients []string
	tunnels := make(map[string]vpnconfig.TunnelConfig)

	for _, c := range clients {
		// Skip clients with invalid routes
		if !validRoutes[c.Route] {
			continue
		}
		if c.Route == "xray" {
			xrayClients = append(xrayClients, c.IP)
		} else {
			// Store the address as entered, with no /32 appended. The Web UI
			// stores what vpnconfig.NormalizeClientAddr returns, which strips
			// /32, and lib/config.sh subtracts paused_clients by exact string:
			// a second spelling here orphans entries paused from the Web UI.
			if existing, ok := tunnels[c.Route]; ok {
				existing.Clients = append(existing.Clients, c.IP)
				tunnels[c.Route] = existing
			} else {
				tunnels[c.Route] = vpnconfig.TunnelConfig{
					Clients: []string{c.IP},
					Exclude: excl,
				}
			}
		}
	}

	// Exclude IPs from wizard state
	excludeIPs := state.GetExcludeIPs()

	// Update config under the cross-process lock
	var ports service.InboundPorts
	err = a.config.UpdateVPNConfig(func(vpnCfg *vpnconfig.VPNDirectorConfig) error {
		// Every IP of every server, read again under the lock: a refresh, or a
		// wave of the subscription watch, may have published since the read
		// above, and the platform call and the wait for the lock lie between.
		all, err := a.config.LoadServers()
		if err != nil {
			return err
		}
		vpnCfg.Xray.Clients = xrayClients
		vpnCfg.Xray.ExcludeSets = excl
		vpnCfg.Xray.ExcludeIPs = excludeIPs
		vpnCfg.Xray.Servers = vpnconfig.ServerIPs(all)
		for name, tunnel := range tunnels {
			if existing, ok := vpnCfg.TunnelDirector.Tunnels[name]; ok {
				tunnel.Gateway = existing.Gateway
				tunnels[name] = tunnel
			}
		}
		vpnCfg.TunnelDirector.Tunnels = tunnels
		// The save is the user's whole assignment. During a failover an
		// address put on a tunnel is where the user wants it - a restore used
		// to take it off that tunnel and back to Xray - and one left out is
		// gone. Only what stays on Xray stays with the failover.
		if fo := vpnCfg.Xray.Failover; fo != nil {
			for _, addr := range append([]string(nil), fo.Clients...) {
				if !slices.Contains(xrayClients, addr) {
					vpnconfig.DetachFailoverClient(vpnCfg, addr)
				}
			}
		}
		// The wizard stores addresses as entered, so a client the old wizard
		// wrote as 1.2.3.4/32 comes back as 1.2.3.4. paused_clients is matched
		// literally, so its entry has to follow or the client resumes on its
		// own. Runs after the clients are in place, on the new spellings.
		vpnCfg.PausedClients = vpnconfig.RepointPausedClients(
			vpnCfg.PausedClients, vpnconfig.CollectClients(vpnCfg))
		// The generated inbound has to listen where the TPROXY rules send
		// traffic, so read the ports while the config is in hand.
		ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(vpnCfg)
		return nil
	})
	if err != nil {
		if errors.Is(err, service.ErrConfigLoad) {
			a.sender.SendPlain(chatID, fmt.Sprintf("Config load error: %v", err))
		} else {
			a.sender.SendPlain(chatID, fmt.Sprintf("Save error: %v", err))
		}
		return err
	}
	a.sender.SendPlain(chatID, "vpn-director.json updated")

	// Generate Xray config for the server step 1 picked, wherever it is now
	if serverIndex >= 0 {
		s := servers[serverIndex]
		// Generation and the record of it under one lock, so a switch from the
		// Web UI or /xray cannot land between them; the record follows only a
		// generation that succeeded, since a failure leaves the previous
		// config.json running and that is the server still to be named.
		generated, err := service.GenerateAndRecordActiveServer(a.config, a.xray, s, ports)
		if !generated {
			a.sender.SendPlain(chatID, fmt.Sprintf("Xray config generation error: %v", err))
			// Continue anyway - vpn-director.json is already saved
		} else {
			if err != nil {
				// config.json is written; only the record of it is missing.
				slog.Warn("Failed to record the active server", "server", s.Name, "error", err)
			}
			a.sender.SendPlain(chatID, "xray/config.json updated")
		}
	} else if name := state.PickedName(); name != "" {
		a.sender.SendPlain(chatID, fmt.Sprintf("Warning: %s is no longer in the server list, Xray config not updated", name))
	} else {
		a.sender.SendPlain(chatID, "Warning: Invalid server selection, Xray config not updated")
	}

	// Apply configuration via vpn-director
	if err := a.vpn.Apply(); err != nil {
		a.sender.SendPlain(chatID, fmt.Sprintf("vpn-director apply error: %v", err))
		return err
	}
	a.sender.SendPlain(chatID, "VPN Director applied")

	// Restart Xray to apply new config
	if err := a.vpn.RestartXray(); err != nil {
		a.sender.SendPlain(chatID, fmt.Sprintf("Xray restart error: %v", err))
		return err
	}
	a.sender.SendPlain(chatID, "Xray restarted")

	a.sender.SendPlain(chatID, "Done!")
	return nil
}
