package vpnconfig

import (
	"net"
	"sort"
	"strings"
)

type XrayFailover struct {
	Tunnel  string   `json:"tunnel"`
	Clients []string `json:"clients"`
	// Addresses appended to the tunnel at stage time. Restore removes only
	// these, so an address that was already a tunnel client stays there.
	// nil means a record from before this field existed: restore then
	// removes every restored address, as it used to.
	Added []string `json:"added"`
	// Committed says the clients have left Xray: CommitXrayFailover sets it.
	// A restore stages a committed failover back onto Xray before it drops
	// the tunnel, so the config alone cannot tell the two apart afterwards -
	// and what a restore announces, and what one that did not hold goes back
	// to, depends on it.
	Committed bool `json:"committed,omitempty"`
}

// TDCarries reports whether Tunnel Director marks addr: an IPv4 address or
// CIDR written the way iptables and ipset read it - net.ParseIP refuses a
// leading zero, as lib/common.sh is_ipv4_net does - whose address lies in
// RFC1918, where is_lan_ip looks. tunnel.sh skips everything else, and a
// failover client it skips is on neither TPROXY nor the tunnel once it has
// been dropped from Xray: it leaves through the WAN.
func TDCarries(addr string) bool {
	if addr == "" || addr != strings.TrimSpace(addr) {
		return false
	}
	if _, err := NormalizeClientAddr(addr); err != nil {
		return false
	}
	host, prefix, isCIDR := strings.Cut(addr, "/")
	if isCIDR && len(prefix) > 1 && prefix[0] == '0' {
		return false
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return false
	}
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	}
	return false
}

// CarriableXrayClients is EffectiveXrayClients that Tunnel Director can carry
// (TDCarries): what a failover moves.
func CarriableXrayClients(cfg *VPNDirectorConfig) []string {
	var out []string
	for _, ip := range EffectiveXrayClients(cfg) {
		if TDCarries(ip) {
			out = append(out, ip)
		}
	}
	return out
}

// FailoverCommitted reports whether the failover's clients have left Xray. The
// record says so; one from before the Committed field is committed when its
// snapshot is on the tunnel and off Xray. A snapshot deleted from the tunnel
// says nothing, and does not count.
func FailoverCommitted(cfg *VPNDirectorConfig) bool {
	if cfg == nil || cfg.Xray.Failover == nil {
		return false
	}
	if cfg.Xray.Failover.Committed {
		return true
	}
	return len(snapshotOnTunnel(cfg)) > 0 && !FailoverStaged(cfg)
}

// XrayClientsOutsideFailover is the Xray clients a failover would carry but
// does not: added, re-added or resumed since the snapshot was taken. A client
// is inside once it is both in the snapshot and on the fallback tunnel.
func XrayClientsOutsideFailover(cfg *VPNDirectorConfig) []string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return nil
	}
	fo := cfg.Xray.Failover
	tun, ok := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	if !ok {
		return nil
	}
	var out []string
	for _, ip := range CarriableXrayClients(cfg) {
		if contains(fo.Clients, ip) && contains(tun.Clients, ip) {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// ExtendXrayFailover stages XrayClientsOutsideFailover the way the snapshot was
// staged: onto the fallback tunnel - recorded in Added when this put them
// there - and into the snapshot, while they stay in xray.clients until the
// tunnel apply has them. On a dead outbound they would otherwise go nowhere
// for the rest of the failover. Reports whether anything changed.
func ExtendXrayFailover(cfg *VPNDirectorConfig) bool {
	outside := XrayClientsOutsideFailover(cfg)
	if len(outside) == 0 {
		return false
	}
	fo := cfg.Xray.Failover
	tun := cfg.TunnelDirector.Tunnels[fo.Tunnel]
	for _, ip := range outside {
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
			// A record without Added restores by taking every restored
			// address off the tunnel, this one included.
			if fo.Added != nil && !contains(fo.Added, ip) {
				fo.Added = append(fo.Added, ip)
			}
		}
		if !contains(fo.Clients, ip) {
			fo.Clients = append(fo.Clients, ip)
		}
	}
	cfg.TunnelDirector.Tunnels[fo.Tunnel] = tun
	return true
}

// DetachFailoverClient takes addr, in any spelling, out of the failover record:
// an address the user has just deleted, or put on a route of their own, is no
// longer one the restore brings back to Xray. The record stays, even empty, so
// the restore that clears it still runs; Added stays a list, because nil is
// the older kind of record, whose restore takes every restored address off the
// tunnel.
func DetachFailoverClient(cfg *VPNDirectorConfig, addr string) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	fo := cfg.Xray.Failover
	fo.Clients = withoutAddr(fo.Clients, addr)
	if fo.Added != nil {
		fo.Added = withoutAddr(fo.Added, addr)
	}
}

func withoutAddr(list []string, addr string) []string {
	want := addrKey(addr)
	out := make([]string, 0, len(list))
	for _, s := range list {
		if addrKey(s) != want {
			out = append(out, s)
		}
	}
	return out
}

// addrKey is the form two spellings of one address share: 1.2.3.4 and
// 1.2.3.4/32 are one client. Anything NormalizeClientAddr refuses compares as
// written.
func addrKey(s string) string {
	if n, err := NormalizeClientAddr(s); err == nil {
		return n
	}
	return s
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

// Armed reports whether the subscription watch has work. A saved link and Xray
// clients to protect arm it for a failover of its own. A failover record arms it
// with or without a link: import_server_list.sh clears the link for a list from
// a file or a plain-http link, and the clients the record took off Xray still
// have to come back.
func Armed(cfg *VPNDirectorConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.Xray.Failover != nil {
		return true
	}
	return cfg.Xray.SubscriptionURL != "" && len(EffectiveXrayClients(cfg)) > 0
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

// FailoverTDExit is the exit the Xray clients are on while failed over: the
// recorded failover tunnel while it is still an exit - the watch may have moved
// them off the first one - and otherwise the first exit.
func FailoverTDExit(cfg *VPNDirectorConfig, plat PlatformInfo) string {
	ids := TDExits(cfg, plat)
	if cfg != nil && cfg.Xray.Failover != nil && contains(ids, cfg.Xray.Failover.Tunnel) {
		return cfg.Xray.Failover.Tunnel
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// NextTDExit is the first exit not in tried, so a failover whose fallback
// never becomes ready moves through every exit rather than trading its
// clients between the first two.
func NextTDExit(cfg *VPNDirectorConfig, plat PlatformInfo, tried map[string]bool) string {
	for _, id := range TDExits(cfg, plat) {
		if !tried[id] {
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
// until TUN_DIR is applied. A client Tunnel Director cannot carry (TDCarries)
// stays on Xray: moved here and dropped from Xray it would leave through the
// WAN, while a dead outbound takes it nowhere. With nothing to carry there is
// no failover to record.
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
		if !TDCarries(ip) {
			continue
		}
		dropped = append(dropped, ip)
		if !contains(tun.Clients, ip) {
			tun.Clients = append(tun.Clients, ip)
			added = append(added, ip)
		}
	}
	if len(dropped) == 0 {
		return
	}
	cfg.TunnelDirector.Tunnels[tunnel] = tun
	cfg.Xray.Failover = &XrayFailover{Tunnel: tunnel, Clients: dropped, Added: added}
	// TUN_DIR is first-match: tunnel.sh emits these snapshot IPs first so a
	// covering earlier rule (often main) does not send them to WAN.
}

// CommitXrayFailover drops staged clients from xray.clients: snapshot
// addresses still on the failover tunnel. Paused clients stay. An address
// the Web UI deleted and re-added as xray is not on the tunnel, so it stays.
// The record says from now on that the clients left Xray.
func CommitXrayFailover(cfg *VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	cfg.Xray.Failover.Committed = true
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
// addresses that landed on xray.clients while we were failed over. A committed
// snapshot takes its clients off Xray; a staged one leaves them there, as the
// stage it records did.
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
	if fo.Committed {
		keptXray := make([]string, 0, len(cfg.Xray.Clients))
		for _, ip := range cfg.Xray.Clients {
			if _, skip := drop[ip]; skip {
				continue
			}
			keptXray = append(keptXray, ip)
		}
		cfg.Xray.Clients = keptXray
	}
	cfg.TunnelDirector.Tunnels[fo.Tunnel] = tun
	cfg.Xray.Failover = &XrayFailover{Tunnel: fo.Tunnel, Clients: clients, Added: fo.Added, Committed: fo.Committed}
}

// EnsureFailoverStaged puts snapshot addresses that are still on the fallback
// tunnel onto xray.clients without clearing failover. It does not put deleted
// or wizard-moved addresses back on the tunnel. Restore then drops tunnel
// membership only after TPROXY is confirmed.
func EnsureFailoverStaged(cfg *VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.Failover == nil {
		return
	}
	// A record from before the Committed field is committed only while its
	// snapshot is off Xray, which this is about to end: past it, a restore
	// whose last apply failed would read a stage that never committed.
	if FailoverCommitted(cfg) {
		cfg.Xray.Failover.Committed = true
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
