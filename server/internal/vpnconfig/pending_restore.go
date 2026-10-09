package vpnconfig

// XrayPendingRestore survives the removal of failover until its routing apply finishes.
type XrayPendingRestore struct {
	Snapshot *XrayFailover `json:"snapshot"`
	Restored []string      `json:"restored"`
	Active   *ActiveServer `json:"active"`
}

// BeginXrayRestore records the intent in the mutation that removes failover.
// The caller holds the config lock and has checked the probed server's identity.
func BeginXrayRestore(cfg *VPNDirectorConfig) *XrayPendingRestore {
	if cfg == nil || cfg.Xray.Failover == nil || cfg.Xray.PendingRestore != nil {
		return nil
	}
	snapshot := *cfg.Xray.Failover
	snapshot.Clients = cloneRestoreList(snapshot.Clients)
	snapshot.Added = cloneRestoreList(snapshot.Added)
	snapshot.Committed = FailoverCommitted(cfg)
	var active *ActiveServer
	if cfg.Xray.ActiveServer != nil {
		a := *cfg.Xray.ActiveServer
		active = &a
	}
	pending := &XrayPendingRestore{
		Snapshot: &snapshot,
		Restored: RestoreXrayClientsFromFailover(cfg),
		Active:   active,
	}
	cfg.Xray.PendingRestore = pending
	return pending
}

// FailoverSnapshot keeps only restored addresses whose current assignment still
// belongs to this restore. A manual move, pause or deletion is never rolled back.
// The caller checks tunnel availability before reinstatement.
func (p *XrayPendingRestore) FailoverSnapshot(cfg *VPNDirectorConfig) *XrayFailover {
	if cfg == nil || p == nil || p.Snapshot == nil {
		return nil
	}
	original := p.Snapshot
	if original.Tunnel == "" {
		return nil
	}
	snapshot := &XrayFailover{
		Tunnel:    original.Tunnel,
		Clients:   []string{},
		Committed: original.Committed,
	}
	if original.Added != nil {
		snapshot.Added = []string{}
	}
	paused := make(map[string]bool, len(cfg.PausedClients))
	for _, addr := range cfg.PausedClients {
		paused[addrKey(addr)] = true
	}
	clients := CollectClients(cfg)
	for _, addr := range p.Restored {
		key := addrKey(addr)
		if paused[key] || !TDCarries(addr) || !restoreListContains(original.Clients, addr) || restoreListContains(snapshot.Clients, addr) {
			continue
		}
		appended := original.Added == nil || restoreListContains(original.Added, addr)
		stored := ""
		for _, client := range clients {
			if addrKey(client.IP) != key {
				continue
			}
			if client.Route == "xray" {
				stored = client.IP
				continue
			}
			if client.Route != original.Tunnel || appended {
				stored = ""
				break
			}
		}
		if stored == "" {
			continue
		}
		snapshot.Clients = append(snapshot.Clients, stored)
		if original.Added != nil && appended {
			snapshot.Added = append(snapshot.Added, stored)
		}
	}
	if len(snapshot.Clients) == 0 {
		return nil
	}
	return snapshot
}

func restoreListContains(list []string, addr string) bool {
	key := addrKey(addr)
	for _, entry := range list {
		if addrKey(entry) == key {
			return true
		}
	}
	return false
}

func cloneRestoreList(list []string) []string {
	if list == nil {
		return nil
	}
	return append([]string{}, list...)
}
