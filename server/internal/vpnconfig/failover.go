package vpnconfig

import "sort"

type XrayFailover struct {
	Tunnel  string   `json:"tunnel"`
	Clients []string `json:"clients"`
	// Addresses appended to the tunnel at stage time. Restore removes only
	// these, so an address that was already a tunnel client stays there.
	// nil means a record from before this field existed: restore then
	// removes every restored address, as it used to.
	Added []string `json:"added"`
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

// NextTDExit is the first connected tunnel that is not skip, so a staged
// failover whose fallback never becomes ready can move to another exit.
func NextTDExit(cfg *VPNDirectorConfig, plat PlatformInfo, skip string) string {
	for _, id := range TDExits(cfg, plat) {
		if id != skip {
			return id
		}
	}
	return ""
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
	StageXrayClientsToTunnel(cfg, tunnel)
	CommitXrayFailover(cfg)
}

// StageXrayClientsToTunnel appends unpaused Xray clients to the tunnel and
// records failover, but leaves them in xray.clients so TPROXY still matches
// until TUN_DIR is applied.
func StageXrayClientsToTunnel(cfg *VPNDirectorConfig, tunnel string) {
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
	dropped := make([]string, 0)
	added := make([]string, 0)
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			continue
		}
		dropped = append(dropped, ip)
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
			added = append(added, ip)
		}
	}
	cfg.TunnelDirector.Tunnels[tunnel] = tun
	cfg.Xray.Failover = &XrayFailover{Tunnel: tunnel, Clients: dropped, Added: added}
	// TUN_DIR is first-match: tunnel.sh emits these snapshot IPs first so a
	// covering earlier rule (often main) does not send them to WAN.
}

// CommitXrayFailover drops staged clients from xray.clients: snapshot
// addresses still on the failover tunnel. Paused clients stay. An address
// the Web UI deleted and re-added as xray is not on the tunnel, so it stays.
func CommitXrayFailover(cfg *VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	paused := pausedSet(cfg)
	drop := make(map[string]struct{})
	for _, ip := range snapshotOnTunnel(cfg) {
		drop[ip] = struct{}{}
	}
	kept := make([]string, 0, len(cfg.Xray.Clients))
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			kept = append(kept, ip)
			continue
		}
		if _, gone := drop[ip]; gone {
			continue
		}
		kept = append(kept, ip)
	}
	cfg.Xray.Clients = kept
}

// FailoverStaged reports that failover is recorded but TPROXY still matches
// those clients, so the drop Apply has not run. Only snapshot addresses that
// are still on the fallback tunnel count: DELETE /api/clients strips the
// tunnel and leaves xray.failover, and a later xray add of the same address
// must not look staged.
func FailoverStaged(cfg *VPNDirectorConfig) bool {
	if cfg == nil || cfg.Xray.Failover == nil {
		return false
	}
	paused := pausedSet(cfg)
	onTunnel := snapshotOnTunnel(cfg)
	for _, ip := range cfg.Xray.Clients {
		if _, skip := paused[ip]; skip {
			continue
		}
		if contains(onTunnel, ip) {
			return true
		}
	}
	return false
}

// snapshotOnTunnel is the failover snapshot still assigned to the fallback
// tunnel. Restore, Commit and FailoverStaged share this so a deleted address
// is not treated as still in the move.
func snapshotOnTunnel(cfg *VPNDirectorConfig) []string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return nil
	}
	fo := cfg.Xray.Failover
	tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(fo.Clients))
	for _, ip := range fo.Clients {
		if contains(tun.Clients, ip) {
			out = append(out, ip)
		}
	}
	return out
}

func RestoreXrayClientsFromFailover(cfg *VPNDirectorConfig) []string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return nil
	}
	fo := cfg.Xray.Failover
	// Only addresses still on the fallback tunnel come back to Xray. A
	// missing key is the wizard dropping that tunnel (and possibly moving
	// the clients elsewhere); restoring the whole snapshot would put them
	// on Xray and override the new assignment.
	restore := snapshotOnTunnel(cfg)
	tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	for _, ip := range restore {
		if !contains(cfg.Xray.Clients, ip) {
			cfg.Xray.Clients = append(cfg.Xray.Clients, ip)
		}
	}
	if ok {
		kept := make([]string, 0, len(tun.Clients))
		drop := make(map[string]struct{})
		if fo.Added == nil {
			for _, ip := range restore {
				drop[ip] = struct{}{}
			}
		} else {
			for _, ip := range fo.Added {
				drop[ip] = struct{}{}
			}
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
	cfg.Xray.Failover = &XrayFailover{Tunnel: fo.Tunnel, Clients: clients, Added: fo.Added}
}

// EnsureFailoverStaged puts snapshot addresses that are still on the fallback
// tunnel onto xray.clients without clearing failover. It does not put deleted
// or wizard-moved addresses back on the tunnel. Restore then drops tunnel
// membership only after TPROXY is confirmed.
func EnsureFailoverStaged(cfg *VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	paused := pausedSet(cfg)
	for _, ip := range snapshotOnTunnel(cfg) {
		if _, skip := paused[ip]; skip {
			continue
		}
		if !contains(cfg.Xray.Clients, ip) {
			cfg.Xray.Clients = append(cfg.Xray.Clients, ip)
		}
	}
}
