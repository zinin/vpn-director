package subwatch

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	ProbeInterval      = 30 * time.Second
	DeadAfter          = 3 * time.Minute
	ImportRetry        = 5 * time.Minute
	SettleAfterRestart = 3 * time.Second
	defaultSOCKSPort   = 12346
)

const (
	msgMoved           = "Xray outbound is down; LAN clients moved to %s"
	msgNoTunnel        = "Xray outbound is down; no Tunnel Director fallback"
	msgRefreshFailed   = "Subscription refresh failed"
	msgRefreshFailedOn = "Subscription refresh failed; still on tunnel:%s"
	msgNoLive          = "No live server in the subscription"
	msgNoLiveOn        = "No live server in the subscription; still on tunnel:%s"
	msgRestored        = "LAN clients back on Xray; server %s"
)

type noteKind int

const (
	noteNone noteKind = iota
	noteMoved
	noteNoTunnel
	noteRefreshFailed
	noteNoLive
	noteRestored
)

type Watch struct {
	LoadVPN      func() (*vpnconfig.VPNDirectorConfig, error)
	LoadPlatform func() (vpnconfig.PlatformInfo, error)
	UpdateVPN    func(func(*vpnconfig.VPNDirectorConfig) error) error
	Apply        func() error
	RestartXray  func() error
	SaveServers  func([]vpnconfig.Server) error
	Generate     func(vpnconfig.Server) (generated bool, err error)
	Probe        func(ctx context.Context, socksPort int) error
	Fetch        func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	Notify       func(msg string)
	Now          func() time.Time
	AfterRestart func(time.Duration)

	mu           sync.Mutex
	failSince    time.Time // zero => last probe succeeded
	lastImport   time.Time
	lastNote     string
	lastNoteKind noteKind
	pendingApply bool // JSON mutated; Apply has not yet succeeded
	running      bool
}

func (w *Watch) Start(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	ticker := time.NewTicker(ProbeInterval)
	defer ticker.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

func (w *Watch) Tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.applyDefaults()

	if w.LoadVPN == nil {
		return
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return
	}
	if !vpnconfig.Armed(cfg) {
		return
	}
	if cfg.Xray.Failover != nil {
		if w.pendingApply {
			if err := w.apply(); err == nil {
				w.pendingApply = false
				if id := failoverTunnel(cfg); id != "" {
					w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
				}
			}
		}
		w.maybeImportAndPick(ctx, cfg)
		return
	}

	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	if err := w.Probe(ctx, socks); err == nil {
		w.failSince = time.Time{}
		w.lastNote = ""
		w.lastNoteKind = noteNone
		return
	}

	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	if now.Sub(w.failSince) < DeadAfter {
		return
	}

	var plat vpnconfig.PlatformInfo
	if w.LoadPlatform != nil {
		if p, err := w.LoadPlatform(); err == nil {
			plat = p
		}
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		w.notify(noteNoTunnel, msgNoTunnel)
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	if w.UpdateVPN == nil {
		return
	}
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.MoveXrayClientsToTunnel(current, id)
		return nil
	}); err != nil {
		return
	}
	if err := w.apply(); err != nil {
		w.pendingApply = true
		return
	}
	w.pendingApply = false
	w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
	if reloaded, err := w.LoadVPN(); err == nil {
		cfg = reloaded
	}
	w.maybeImportAndPick(ctx, cfg)
}

func pickOrder(servers []vpnconfig.Server, activeName string) []vpnconfig.Server {
	if activeName == "" {
		return servers
	}
	var first, rest []vpnconfig.Server
	seen := false
	for _, s := range servers {
		if !seen && s.Name == activeName {
			first = append(first, s)
			seen = true
			continue
		}
		rest = append(rest, s)
	}
	return append(first, rest...)
}

func failoverTunnel(cfg *vpnconfig.VPNDirectorConfig) string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return ""
	}
	return cfg.Xray.Failover.Tunnel
}

func (w *Watch) maybeImportAndPick(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.Fetch == nil {
		return
	}
	now := w.Now()
	if !w.lastImport.IsZero() && now.Sub(w.lastImport) < ImportRetry {
		return
	}
	w.lastImport = now

	url := ""
	if cfg != nil {
		url = cfg.Xray.SubscriptionURL
	}
	servers, err := w.Fetch(ctx, url)
	if err != nil || len(servers) == 0 {
		w.notifyRefreshFailed(cfg)
		return
	}
	if w.SaveServers != nil {
		if err := w.SaveServers(servers); err != nil {
			w.notifyRefreshFailed(cfg)
			return
		}
	}
	if w.UpdateVPN != nil {
		_ = w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			current.Xray.Servers = uniqueServerIPs(servers)
			return nil
		})
	}
	if w.Generate == nil {
		return
	}

	activeName := ""
	if cfg != nil && cfg.Xray.ActiveServer != nil {
		activeName = cfg.Xray.ActiveServer.Name
	}
	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	for _, s := range pickOrder(servers, activeName) {
		generated, _ := w.Generate(s)
		if !generated {
			continue
		}
		if w.RestartXray != nil {
			if err := w.RestartXray(); err != nil {
				continue
			}
		}
		w.AfterRestart(SettleAfterRestart)
		if err := w.Probe(ctx, socks); err != nil {
			continue
		}
		if !w.commitRestore(cfg) {
			return
		}
		w.notify(noteRestored, fmt.Sprintf(msgRestored, s.Name))
		return
	}
	w.notifyNoLive(cfg)
}

// uniqueServerIPs is the same de-dupe as handler/import.go and webapi.collectServerIPs:
// xray.servers feeds TPROXY_BYPASS, so every imported endpoint must be present.
func uniqueServerIPs(servers []vpnconfig.Server) []string {
	seen := make(map[string]bool)
	ips := make([]string, 0)
	for _, s := range servers {
		for _, ip := range s.IPs {
			if ip != "" && !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	sort.Strings(ips)
	return ips
}

func (w *Watch) apply() error {
	if w.Apply == nil {
		return nil
	}
	return w.Apply()
}

func (w *Watch) writeBackFailover(tunnel string) {
	if tunnel == "" || w.UpdateVPN == nil {
		return
	}
	_ = w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.MoveXrayClientsToTunnel(current, tunnel)
		return nil
	})
}

// commitRestore persists Restore+Apply as one transaction. On Apply error the
// failover record is written back so a later Tick can retry instead of
// guessing that clients are already on Xray. failSince and lastImport clear
// only after Apply succeeds.
func (w *Watch) commitRestore(cfg *vpnconfig.VPNDirectorConfig) bool {
	tunnel := failoverTunnel(cfg)
	if w.UpdateVPN != nil {
		if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			vpnconfig.RestoreXrayClientsFromFailover(current)
			return nil
		}); err != nil {
			return false
		}
	}
	if err := w.apply(); err != nil {
		w.writeBackFailover(tunnel)
		return false
	}
	w.pendingApply = false
	w.failSince = time.Time{}
	w.lastImport = time.Time{}
	return true
}

func (w *Watch) notifyRefreshFailed(cfg *vpnconfig.VPNDirectorConfig) {
	if id := failoverTunnel(cfg); id != "" {
		w.notify(noteRefreshFailed, fmt.Sprintf(msgRefreshFailedOn, id))
		return
	}
	w.notify(noteRefreshFailed, msgRefreshFailed)
}

func (w *Watch) notifyNoLive(cfg *vpnconfig.VPNDirectorConfig) {
	if id := failoverTunnel(cfg); id != "" {
		w.notify(noteNoLive, fmt.Sprintf(msgNoLiveOn, id))
		return
	}
	w.notify(noteNoLive, msgNoLive)
}

func (w *Watch) applyDefaults() {
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.AfterRestart == nil {
		w.AfterRestart = time.Sleep
	}
	if w.Probe == nil {
		w.Probe = func(ctx context.Context, port int) error {
			return ProbeSOCKS(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ProbeURL)
		}
	}
}

func (w *Watch) notify(kind noteKind, msg string) {
	if w.lastNoteKind == kind {
		return
	}
	w.lastNoteKind = kind
	w.lastNote = msg
	if w.Notify != nil {
		w.Notify(msg)
	}
}
