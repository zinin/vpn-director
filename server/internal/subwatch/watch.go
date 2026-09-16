package subwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	ProbeInterval      = 30 * time.Second
	DeadAfter          = 3 * time.Minute
	ImportRetry        = 5 * time.Minute
	ImportRetryMax     = 30 * time.Minute
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

	mu             sync.Mutex
	failSince      time.Time // zero => last probe succeeded
	lastImport     time.Time
	importRetry    time.Duration // current wait between import waves; zero means ImportRetry
	lastRouteKind  noteKind      // noteMoved, noteNoTunnel
	lastImportKind noteKind      // noteRefreshFailed, noteNoLive, noteRestored
	pendingApply   bool          // JSON mutated; Apply has not yet succeeded
	reconciled     bool          // the first armed Tick has checked for a failover left by an earlier process
	running        bool
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
	if !w.reconciled {
		w.reconciled = true
		// The move is written before Apply, and a process that stopped in
		// between left the kernel without the fallback routing. apply is
		// idempotent and queues with --wait, so re-running it is safe.
		if cfg.Xray.Failover != nil {
			w.pendingApply = true
		}
	}
	if cfg.Xray.Failover != nil {
		if w.pendingApply {
			if err := w.apply(); err != nil {
				slog.Warn("Apply retry after moving Xray clients failed", "error", err)
				// No walk until the move is applied: its restart of Xray would
				// rebuild TPROXY from a JSON that no longer lists the moved
				// clients while Tunnel Director has not been applied, sending
				// them straight out through the WAN.
				return
			}
			w.pendingApply = false
			if id := failoverTunnel(cfg); id != "" {
				slog.Info("Xray clients moved to Tunnel Director", "tunnel", id, "clients", len(cfg.Xray.Failover.Clients))
				w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
			}
		}
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	if w.pendingApply {
		// The JSON says restored, but the restore Apply never succeeded (the
		// write-back failed), so the kernel may still route the clients
		// through the tunnel.
		if err := w.apply(); err != nil {
			slog.Warn("Apply retry after restoring Xray clients failed", "error", err)
			return
		}
		w.pendingApply = false
		w.failSince = time.Time{}
		w.lastImport = time.Time{}
		w.importRetry = 0
		name := "unknown"
		if cfg.Xray.ActiveServer != nil {
			name = cfg.Xray.ActiveServer.Name
		}
		slog.Info("Xray clients restored", "server", name)
		w.notify(noteRestored, fmt.Sprintf(msgRestored, name))
	}

	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	err = w.Probe(ctx, socks)
	if err == nil {
		w.failSince = time.Time{}
		w.importRetry = 0
		w.lastRouteKind = noteNone
		w.lastImportKind = noteNone
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
	announce := w.lastRouteKind != noteNoTunnel
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
	if !w.lastImport.IsZero() && now.Sub(w.lastImport) < w.importInterval() {
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
		// An all-dead walk may have backed this off to 10/20/30m. A failed
		// download retries every ImportRetry; keep that, not the walk backoff.
		w.importRetry = 0
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
			current.Xray.Servers = vpnconfig.ServerIPs(servers)
			return nil
		}); err != nil {
			slog.Warn("Failed to sync xray.servers after the subscription refresh", "error", err)
			w.notifyRefreshFailed(cfg)
			return
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
	order := pickOrder(servers, activeName)
	var preferred *vpnconfig.Server
	if activeName != "" && len(order) > 0 && order[0].Name == activeName {
		preferred = &order[0]
	}
	tried := 0
	lastGenerated := ""
	for _, s := range order {
		generated, err := w.Generate(s)
		if err != nil || !generated {
			slog.Debug("Generating Xray config for server failed", "server", s.Name, "generated", generated, "error", err)
		}
		if !generated {
			continue
		}
		lastGenerated = s.Name
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
	if preferred != nil && lastGenerated != "" && lastGenerated != activeName {
		w.returnToPreferred(*preferred)
	}
	if tried > 0 {
		// Every tried server cost an Xray restart and a config write; on a large
		// all-dead subscription back-to-back waves would never stop doing that.
		w.lastImport = w.Now()
		w.importRetry = min(2*w.importInterval(), ImportRetryMax)
		slog.Info("Next subscription refresh backed off", "after", w.importRetry)
	}
	w.notifyNoLive(cfg)
}

// importInterval is the current wait between import waves.
func (w *Watch) importInterval() time.Duration {
	if w.importRetry != 0 {
		return w.importRetry
	}
	return ImportRetry
}

// returnToPreferred generates the preferred server back after a walk found
// nothing live. Generate records every server it writes as xray.active_server,
// so without this the next wave would start from the last server tried rather
// than the user's. No probe and no restore: the walk has just found it down.
func (w *Watch) returnToPreferred(s vpnconfig.Server) {
	generated, err := w.Generate(s)
	if err != nil || !generated {
		slog.Warn("Failed to return the Xray config to the preferred server", "server", s.Name, "generated", generated, "error", err)
	}
	if !generated {
		return
	}
	if w.RestartXray != nil {
		if err := w.RestartXray(); err != nil {
			slog.Warn("Xray restart on the preferred server failed", "server", s.Name, "error", err)
			return
		}
	}
	if err == nil {
		slog.Info("Xray config returned to the preferred server", "server", s.Name)
	}
}

func (w *Watch) apply() error {
	if w.Apply == nil {
		return nil
	}
	return w.Apply()
}

func (w *Watch) writeBackFailover(fo *vpnconfig.XrayFailover) {
	if fo == nil || fo.Tunnel == "" || w.UpdateVPN == nil {
		return
	}
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.ApplyFailoverSnapshot(current, fo)
		return nil
	}); err != nil {
		slog.Warn("Failed to write the Xray failover back", "tunnel", fo.Tunnel, "error", err)
	}
}

// commitRestore persists Restore+Apply as one transaction. On Apply error the
// failover record is written back so a later Tick can retry instead of
// guessing that clients are already on Xray. failSince and lastImport clear
// only after Apply succeeds. The write-back is the addresses this restore
// moved, not a fresh Move of every current Xray client.
func (w *Watch) commitRestore(cfg *vpnconfig.VPNDirectorConfig) bool {
	tunnel := failoverTunnel(cfg)
	var restored []string
	if w.UpdateVPN != nil {
		if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			restored = vpnconfig.RestoreXrayClientsFromFailover(current)
			return nil
		}); err != nil {
			slog.Warn("Failed to restore Xray clients from the failover", "error", err)
			return false
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after restoring Xray clients failed", "error", err)
		w.writeBackFailover(&vpnconfig.XrayFailover{Tunnel: tunnel, Clients: restored})
		// Only a restored failover record changed the JSON routing; without one
		// there is nothing to re-apply and the next Tick's health probe decides.
		if tunnel != "" {
			w.pendingApply = true
		}
		return false
	}
	w.pendingApply = false
	w.failSince = time.Time{}
	w.lastImport = time.Time{}
	w.importRetry = 0
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

// notify de-duplicates two channels separately: where the clients are routed
// (moved, no tunnel) and what the last import came to (refresh failed, no live
// server, restored). One channel shared by both let a steady no-tunnel outage
// alternate kinds and repeat the same messages on every import wave. A routing
// message that is sent starts a new episode and clears the import channel, so
// that episode's import outcome is news again. A suppressed one leaves the
// import channel alone: the no-tunnel branch notifies on every tick.
func (w *Watch) notify(kind noteKind, msg string) {
	if kind == noteRestored {
		// The clients are back, or Xray works again: the next episode's moved
		// or no-tunnel message is news even without a healthy probe between.
		w.lastRouteKind = noteNone
	}
	route := kind == noteMoved || kind == noteNoTunnel
	last := &w.lastImportKind
	if route {
		last = &w.lastRouteKind
	}
	if *last == kind {
		return
	}
	*last = kind
	if route {
		w.lastImportKind = noteNone
	}
	if w.Notify != nil {
		w.Notify(msg)
	}
}
