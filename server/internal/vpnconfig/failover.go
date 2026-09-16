package vpnconfig

import "sort"

type XrayFailover struct {
	Tunnel  string   `json:"tunnel"`
	Clients []string `json:"clients"`
}

func pausedSet(cfg *VPNDirectorConfig) map[string]struct{} {
	s := make(map[string]struct{}, len(cfg.PausedClients))
	for _, ip := range cfg.PausedClients {
		s[ip] = struct{}{}
	}
	return s
}

func EffectiveXrayClients(cfg *VPNDirectorConfig) []string {
	if cfg == nil {
		return nil
	}
	paused := pausedSet(cfg)
	out := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func Armed(cfg *VPNDirectorConfig) bool {
	if cfg == nil || cfg.Xray.SubscriptionURL == "" {
		return false
	}
	if cfg.Xray.Failover != nil {
		return true
	}
	return len(EffectiveXrayClients(cfg)) > 0
}

func TDExits(cfg *VPNDirectorConfig, plat PlatformInfo) []string {
	if cfg == nil {
		return nil
	}
	byID := make(map[string]PlatformTunnel, len(plat.Tunnels))
	for _, t := range plat.Tunnels {
		byID[t.ID] = t
	}
	ids := make([]string, 0, len(cfg.TunnelDirector.Tunnels))
	for id, tun := range cfg.TunnelDirector.Tunnels {
		if id == "main" || len(tun.Clients) == 0 {
			continue
		}
		pt, ok := byID[id]
		if !ok || !pt.Connected || pt.Iface == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func FirstTDExit(cfg *VPNDirectorConfig, plat PlatformInfo) string {
	ids := TDExits(cfg, plat)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func MoveXrayClientsToTunnel(cfg *VPNDirectorConfig, tunnel string) {
	if cfg == nil || tunnel == "" {
		return
	}
	if cfg.Xray.Failover != nil {
		return
	}
	tun, ok := cfg.TunnelDirector.Tunnels[tunnel]
	if !ok {
		return
	}
	paused := pausedSet(cfg)
	added := make([]string, 0)
	keptXray := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			keptXray = append(keptXray, ip)
			continue
		}
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
			added = append(added, ip)
		}
		// unpaused Xray clients leave xray.clients even if they already sat on the tunnel
	}
	cfg.TunnelDirector.Tunnels[tunnel] = tun
	cfg.Xray.Clients = keptXray
	cfg.Xray.Failover = &XrayFailover{Tunnel: tunnel, Clients: added}
	// TUN_DIR is first-match: config.sh puts this tunnel first while failover is set,
	// so a covering earlier rule (often main) does not send these clients to WAN.
}

func RestoreXrayClientsFromFailover(cfg *VPNDirectorConfig) []string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return nil
	}
	fo := cfg.Xray.Failover
	restore := fo.Clients
	tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	if ok {
		// A client deleted from the tunnel, or moved to another one, during
		// failover stays where the user put it instead of coming back on Xray.
		restore = make([]string, 0, len(fo.Clients))
		for _, ip := range fo.Clients {
			if contains(tun.Clients, ip) {
				restore = append(restore, ip)
			}
		}
	}
	for _, ip := range restore {
		if !contains(cfg.Xray.Clients, ip) {
			cfg.Xray.Clients = append(cfg.Xray.Clients, ip)
		}
	}
	if ok {
		kept := make([]string, 0, len(tun.Clients))
		drop := make(map[string]struct{}, len(restore))
		for _, ip := range restore {
			drop[ip] = struct{}{}
		}
		for _, ip := range tun.Clients {
			if _, ok := drop[ip]; !ok {
				kept = append(kept, ip)
			}
		}
		tun.Clients = kept
		cfg.TunnelDirector.Tunnels[fo.Tunnel] = tun
	}
	cfg.Xray.Failover = nil
	return restore
}

// ApplyFailoverSnapshot puts only fo.Clients onto fo.Tunnel and records
// failover. Unlike MoveXrayClientsToTunnel it does not scoop up other
// addresses that landed on xray.clients while we were failed over.
func ApplyFailoverSnapshot(cfg *VPNDirectorConfig, fo *XrayFailover) {
	if cfg == nil || fo == nil || fo.Tunnel == "" {
		return
	}
	tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	if !ok {
		return
	}
	drop := make(map[string]struct{}, len(fo.Clients))
	clients := make([]string, 0, len(fo.Clients))
	for _, ip := range fo.Clients {
		if ip == "" {
			continue
		}
		drop[ip] = struct{}{}
		clients = append(clients, ip)
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
		}
	}
	keptXray := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := drop[ip]; skip {
			continue
		}
		keptXray = append(keptXray, ip)
	}
	cfg.TunnelDirector.Tunnels[fo.Tunnel] = tun
	cfg.Xray.Clients = keptXray
	cfg.Xray.Failover = &XrayFailover{Tunnel: fo.Tunnel, Clients: clients}
}
