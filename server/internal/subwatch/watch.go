package subwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
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
	msgPicked          = "Subscription refreshed; selected server %s"
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
		slog.Warn("Failed to load VPN Director config for the subscription watch", "error", err)
		return
	}
	if !vpnconfig.Armed(cfg) {
		w.failSince = time.Time{}
		return
	}
	if cfg.Xray.Failover != nil {
		if w.pendingApply {
			if err := w.apply(); err != nil {
				slog.Warn("Apply retry after moving Xray clients failed", "error", err)
			} else {
				w.pendingApply = false
				if id := failoverTunnel(cfg); id != "" {
					slog.Info("Xray clients moved to Tunnel Director", "tunnel", id, "clients", len(cfg.Xray.Failover.Clients))
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
	err = w.Probe(ctx, socks)
	if err == nil {
		w.failSince = time.Time{}
		w.lastNoteKind = noteNone
		return
	}

	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	if now.Sub(w.failSince) < DeadAfter {
		slog.Debug("Xray SOCKS probe failed", "socks_port", socks, "error", err)
		return
	}
	// With no tunnel to move to, every later tick comes back here: log the
	// transition only until the user has been told there is no fallback.
	announce := w.lastNoteKind != noteNoTunnel
	if announce {
		slog.Info("Xray outbound declared dead", "socks_port", socks, "error", err)
	}

	var plat vpnconfig.PlatformInfo
	if w.LoadPlatform != nil {
		if p, err := w.LoadPlatform(); err != nil {
			slog.Warn("Failed to read platform info for the Xray failover", "error", err)
		} else {
			plat = p
		}
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		if announce {
			slog.Info("No Tunnel Director fallback for Xray clients")
		}
		w.notify(noteNoTunnel, msgNoTunnel)
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	if w.UpdateVPN == nil {
		return
	}
	movedClients := 0
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.MoveXrayClientsToTunnel(current, id)
		if current.Xray.Failover == nil {
			return fmt.Errorf("tunnel %s no longer configured", id)
		}
		movedClients = len(current.Xray.Failover.Clients)
		return nil
	}); err != nil {
		slog.Warn("Failed to move Xray clients to Tunnel Director", "tunnel", id, "error", err)
		return
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after moving Xray clients failed", "tunnel", id, "error", err)
		w.pendingApply = true
		return
	}
	w.pendingApply = false
	slog.Info("Xray clients moved to Tunnel Director", "tunnel", id, "clients", movedClients)
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

	rawURL := ""
	if cfg != nil {
		rawURL = cfg.Xray.SubscriptionURL
	}
	servers, err := w.Fetch(ctx, rawURL)
	if err != nil || len(servers) == 0 {
		// A *url.Error carries the whole subscription URL, token included.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		slog.Warn("Subscription refresh failed", "servers", len(servers), "error", err)
		w.notifyRefreshFailed(cfg)
		return
	}
	if w.SaveServers != nil {
		if err := w.SaveServers(servers); err != nil {
			slog.Warn("Failed to save the refreshed subscription servers", "error", err)
			w.notifyRefreshFailed(cfg)
			return
		}
	}
	slog.Info("Subscription refreshed", "servers", len(servers))
	if w.UpdateVPN != nil {
		if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			current.Xray.Servers = uniqueServerIPs(servers)
			return nil
		}); err != nil {
			slog.Warn("Failed to sync xray.servers after the subscription refresh", "error", err)
		}
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
	tried := 0
	for _, s := range pickOrder(servers, activeName) {
		generated, err := w.Generate(s)
		if err != nil || !generated {
			slog.Debug("Generating Xray config for server failed", "server", s.Name, "generated", generated, "error", err)
		}
		if !generated {
			continue
		}
		tried++
		if w.RestartXray != nil {
			if err := w.RestartXray(); err != nil {
				slog.Debug("Xray restart failed", "server", s.Name, "error", err)
				continue
			}
		}
		w.AfterRestart(SettleAfterRestart)
		if err := w.Probe(ctx, socks); err != nil {
			slog.Debug("Subscription server probe failed", "server", s.Name, "error", err)
			continue
		}
		slog.Info("Subscription server picked", "server", s.Name)
		// Only a watch that moved clients has anything to bring back to Xray.
		moved := failoverTunnel(cfg) != ""
		if !w.commitRestore(cfg) {
			return
		}
		if moved {
			slog.Info("Xray clients restored", "server", s.Name)
			w.notify(noteRestored, fmt.Sprintf(msgRestored, s.Name))
		} else {
			w.notify(noteRestored, fmt.Sprintf(msgPicked, s.Name))
		}
		return
	}
	slog.Info("No live server in the subscription", "tried", tried)
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
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.MoveXrayClientsToTunnel(current, tunnel)
		return nil
	}); err != nil {
		slog.Warn("Failed to write the Xray failover back", "tunnel", tunnel, "error", err)
	}
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
			slog.Warn("Failed to restore Xray clients from the failover", "error", err)
			return false
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after restoring Xray clients failed", "error", err)
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
	if w.Notify != nil {
		w.Notify(msg)
	}
}
