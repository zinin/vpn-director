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
	msgMoved            = "Xray outbound is down; LAN clients moved to %s"
	msgNoTunnel         = "Xray outbound is down; no Tunnel Director fallback"
	msgFallbackNotReady = "Xray outbound is down; Tunnel Director fallback is not ready"
	msgRefreshFailed    = "Subscription refresh failed"
	msgRefreshFailedOn  = "Subscription refresh failed; still on tunnel:%s"
	msgNoLive           = "No live server in the subscription"
	msgNoLiveOn         = "No live server in the subscription; still on tunnel:%s"
	msgRestored         = "LAN clients back on Xray; server %s"
	msgPicked           = "Subscription refreshed; selected server %s"
)

type noteKind int

const (
	noteNone noteKind = iota
	noteMoved
	noteNoTunnel
	noteFallbackNotReady
	noteRefreshFailed
	noteNoLive
	noteRestored
)

type Watch struct {
	LoadVPN       func() (*vpnconfig.VPNDirectorConfig, error)
	LoadPlatform  func() (vpnconfig.PlatformInfo, error)
	UpdateVPN     func(func(*vpnconfig.VPNDirectorConfig) error) error
	Apply         func() error
	RestartXray   func() error
	SaveServers   func([]vpnconfig.Server) error
	Generate      func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (generated bool, seq int, err error)
	Probe         func(ctx context.Context, socksPort int) error
	Fetch         func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	Notify        func(msg string)
	Now           func() time.Time
	AfterRestart  func(time.Duration)
	FallbackReady func(tunnel string) bool // nil => ready; false keeps Xray membership
	TPROXYReady   func() bool              // nil => ready; false keeps fallback membership after restore
	Stopped       func() bool              // nil => not stopped; true skips apply/restart after /stop

	mu                   sync.Mutex
	failSince            time.Time // zero => last probe succeeded
	lastImport           time.Time
	importRetry          time.Duration // current wait between import waves; zero means ImportRetry
	lastRouteKind        noteKind      // noteMoved, noteNoTunnel
	lastImportKind       noteKind      // noteRefreshFailed, noteNoLive, noteRestored
	pendingApply         bool          // JSON mutated; Apply has not yet succeeded
	pendingRestoreNotify bool          // pendingApply is a committed restore, so notify when Apply succeeds
	reconciled           bool          // the first armed Tick has checked for a failover left by an earlier process
	lastNoTunnelCheck    time.Time     // last LoadPlatform while announcing no fallback
	lastTPROXYFail       time.Time     // last apply that found TPROXY not intercepting
	lastFallbackFail     time.Time     // last staged apply whose fallback was not ready
	running              bool
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
	if w.stopped() {
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
		staged := vpnconfig.FailoverStaged(cfg)
		if w.probeOK(ctx, cfg) {
			if !w.tproxyReady() && !w.lastTPROXYFail.IsZero() && w.Now().Sub(w.lastTPROXYFail) < ImportRetry {
				return
			}
			announceRestored := !staged || w.pendingRestoreNotify
			if !w.commitRestore(cfg) {
				return
			}
			if announceRestored {
				name := "unknown"
				if cfg.Xray.ActiveServer != nil {
					name = cfg.Xray.ActiveServer.Name
				}
				slog.Info("Xray clients restored", "server", name)
				w.notify(noteRestored, fmt.Sprintf(msgRestored, name))
			}
			return
		}
		// A /stop can land while the probe waits. What follows drops Xray
		// membership and announces; a stopped tick does neither.
		if w.stopped() {
			return
		}
		wasPending := w.pendingApply || staged
		var ok bool
		cfg, ok = w.applyFailover(cfg)
		if ok && wasPending {
			if id := failoverTunnel(cfg); id != "" {
				n := 0
				if cfg.Xray.Failover != nil {
					n = len(cfg.Xray.Failover.Clients)
				}
				slog.Info("Xray clients moved to Tunnel Director", "tunnel", id, "clients", n)
				w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
			}
		}
		if !ok && staged && !w.pendingApply {
			w.notify(noteFallbackNotReady, msgFallbackNotReady)
			cfg = w.retryOrSwitchFallback(ctx, cfg)
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
			// Keep probing: a dead outbound during a stuck restore-Apply
			// must still be able to fail over again.
		} else {
			w.pendingApply = false
			notifyRestore := w.pendingRestoreNotify
			w.pendingRestoreNotify = false
			w.failSince = time.Time{}
			w.lastImport = time.Time{}
			w.importRetry = 0
			if notifyRestore {
				name := "unknown"
				if cfg.Xray.ActiveServer != nil {
					name = cfg.Xray.ActiveServer.Name
				}
				slog.Info("Xray clients restored", "server", name)
				w.notify(noteRestored, fmt.Sprintf(msgRestored, name))
			}
		}
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
	// A /stop can land while the probe waits; the move is a write it rules out.
	if w.stopped() {
		return
	}
	if w.lastRouteKind == noteNoTunnel && !w.lastNoTunnelCheck.IsZero() && now.Sub(w.lastNoTunnelCheck) < ImportRetry {
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	// With no tunnel to move to, every later tick comes back here: log the
	// transition only until the user has been told there is no fallback.
	announce := w.lastRouteKind != noteNoTunnel
	if announce {
		slog.Info("Xray outbound declared dead", "socks_port", socks, "error", err)
	}

	var plat vpnconfig.PlatformInfo
	platErr := false
	if w.LoadPlatform != nil {
		if p, err := w.LoadPlatform(); err != nil {
			slog.Warn("Failed to read platform info for the Xray failover", "error", err)
			platErr = true
		} else {
			plat = p
		}
	}
	// LoadPlatform shells out and takes no lock: a /stop can finish while it
	// runs, and neither the stage write nor the message below may follow one.
	if w.stopped() {
		return
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		if platErr {
			w.maybeImportAndPick(ctx, cfg)
			return
		}
		w.lastNoTunnelCheck = now
		if announce {
			slog.Info("No Tunnel Director fallback for Xray clients")
		}
		w.notify(noteNoTunnel, msgNoTunnel)
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	w.lastNoTunnelCheck = time.Time{}
	if w.UpdateVPN == nil {
		return
	}
	movedClients := 0
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.StageXrayClientsToTunnel(current, id)
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
		if reloaded, err := w.LoadVPN(); err == nil {
			cfg = reloaded
		}
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	if reloaded, err := w.LoadVPN(); err == nil {
		cfg = reloaded
	}
	var ok bool
	cfg, ok = w.applyFailover(cfg)
	if !ok {
		if !w.pendingApply {
			w.notify(noteFallbackNotReady, msgFallbackNotReady)
			cfg = w.retryOrSwitchFallback(ctx, cfg)
		}
		w.maybeImportAndPick(ctx, cfg)
		return
	}
	slog.Info("Xray clients moved to Tunnel Director", "tunnel", id, "clients", movedClients)
	w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
	w.maybeImportAndPick(ctx, cfg)
}

// applyFailover installs remaining failover applies: a pending kernel apply,
// then dropping Xray membership once TUN_DIR already has the clients.
func (w *Watch) applyFailover(cfg *vpnconfig.VPNDirectorConfig) (*vpnconfig.VPNDirectorConfig, bool) {
	if w.pendingApply {
		if err := w.apply(); err != nil {
			slog.Warn("Apply retry after moving Xray clients failed", "error", err)
			return cfg, false
		}
		w.pendingApply = false
		if reloaded, err := w.LoadVPN(); err == nil {
			cfg = reloaded
		}
	}
	if !vpnconfig.FailoverStaged(cfg) {
		return cfg, true
	}
	if !w.fallbackReady(cfg) {
		slog.Warn("Failover tunnel is not applied yet; keeping Xray membership")
		return cfg, false
	}
	if w.UpdateVPN != nil {
		if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			vpnconfig.CommitXrayFailover(current)
			return nil
		}); err != nil {
			slog.Warn("Failed to drop staged Xray clients after the tunnel apply", "error", err)
			return cfg, false
		}
	}
	if reloaded, err := w.LoadVPN(); err == nil {
		cfg = reloaded
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after dropping staged Xray clients failed", "error", err)
		w.pendingApply = true
		return cfg, false
	}
	return cfg, true
}

func sameServer(s vpnconfig.Server, a *vpnconfig.ActiveServer) bool {
	if a == nil || s.Name != a.Name {
		return false
	}
	if a.Address == "" && a.Port == 0 {
		return true
	}
	return s.Address == a.Address && s.Port == a.Port
}

func activeID(a *vpnconfig.ActiveServer) string {
	if a == nil {
		return ""
	}
	if a.Address == "" && a.Port == 0 {
		return a.Name
	}
	return a.Name + "\x1f" + a.Address + "\x1f" + strconv.Itoa(a.Port)
}

func serverID(s vpnconfig.Server) string {
	return s.Name + "\x1f" + s.Address + "\x1f" + strconv.Itoa(s.Port)
}

func pickOrder(servers []vpnconfig.Server, active *vpnconfig.ActiveServer) []vpnconfig.Server {
	if active == nil || active.Name == "" {
		return servers
	}
	var first, rest []vpnconfig.Server
	seen := false
	for _, s := range servers {
		if !seen && sameServer(s, active) {
			first = append(first, s)
			seen = true
			continue
		}
		rest = append(rest, s)
	}
	return append(first, rest...)
}

func (w *Watch) fallbackReady(cfg *vpnconfig.VPNDirectorConfig) bool {
	if w.FallbackReady == nil {
		return true
	}
	id := failoverTunnel(cfg)
	if id == "" {
		return true
	}
	return w.FallbackReady(id)
}

func failoverTunnel(cfg *vpnconfig.VPNDirectorConfig) string {
	if cfg == nil || cfg.Xray.Failover == nil {
		return ""
	}
	return cfg.Xray.Failover.Tunnel
}

// errSuperseded is a Generate its guard refused: xray.active_server names a
// server the walk did not put there.
var errSuperseded = errors.New("a newer server was selected")

// errSubscriptionChanged is a publication its guard refused: the saved link is
// no longer the one this wave downloaded.
var errSubscriptionChanged = errors.New("the subscription URL changed")

func (w *Watch) walkSuperseded(started, lastRecorded string, expectedSeq int) bool {
	if w.LoadVPN == nil {
		return false
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return false
	}
	return superseded(cfg, started, lastRecorded, expectedSeq)
}

// supersededGuard is the guard the walk hands Generate. It runs under the config
// lock with the write, so a Web UI or /xray selection that commits after the
// walk last read the config is refused instead of written over.
func supersededGuard(started, lastRecorded string, expectedSeq int) func(*vpnconfig.VPNDirectorConfig) error {
	return func(cfg *vpnconfig.VPNDirectorConfig) error {
		if superseded(cfg, started, lastRecorded, expectedSeq) {
			return errSuperseded
		}
		return nil
	}
}

// superseded reports whether cfg names a server the walk did not record: one
// selected since the walk started (lastRecorded empty) or since the last
// Generate whose record saved.
func superseded(cfg *vpnconfig.VPNDirectorConfig, started, lastRecorded string, expectedSeq int) bool {
	if cfg == nil {
		return false
	}
	// Re-selecting the server that is already named changes nothing else, so
	// the write counter is the only thing that reports it. Every record moves
	// it on; a config whose records predate it keeps both sides at zero and
	// leaves the decision to the identity below.
	if vpnconfig.ActiveSeq(cfg.Xray.ActiveServer) != expectedSeq {
		return true
	}
	cur := activeID(cfg.Xray.ActiveServer)
	if lastRecorded == "" {
		return cur != "" && cur != started
	}
	if cur == "" || cur == lastRecorded {
		return false
	}
	// Same name with an empty recorded address is a test that did not fill
	// ActiveServer. A real Select of another host with the same name has
	// an address and is a newer selection, including re-selecting started.
	if cfg.Xray.ActiveServer != nil && cfg.Xray.ActiveServer.Address == "" && cfg.Xray.ActiveServer.Port == 0 {
		return false
	}
	return true
}

func (w *Watch) maybeImportAndPick(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.Fetch == nil || w.stopped() {
		return
	}
	now := w.Now()
	if !w.lastImport.IsZero() && now.Sub(w.lastImport) < w.importInterval() {
		return
	}
	prevImport := w.lastImport
	w.lastImport = now

	rawURL := ""
	if cfg != nil {
		rawURL = cfg.Xray.SubscriptionURL
	}
	servers, err := w.Fetch(ctx, rawURL)
	// The download blocks for as long as the subscription host takes. A /stop
	// that finished meanwhile ends the wave before servers.json is written, and
	// a wave that did not happen leaves its window to the next one.
	if w.stopped() {
		w.lastImport = prevImport
		return
	}
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
	// servers.json and xray.servers are published together, under the config
	// lock, so this wave cannot end up beside half of a Web UI or /import one.
	// The same lock is where the saved link is checked: a download takes longer
	// than it takes someone to paste another subscription, and publishing this
	// list then leaves it beside a link that did not produce it.
	sameURL := func(current *vpnconfig.VPNDirectorConfig) error {
		if current.Xray.SubscriptionURL != rawURL {
			return errSubscriptionChanged
		}
		return nil
	}
	if err := vpnconfig.PublishServers(w.UpdateVPN, w.SaveServers, servers, "", sameURL); err != nil {
		if errors.Is(err, errSubscriptionChanged) {
			slog.Info("Subscription refresh abandoned; the saved link is no longer the one that was downloaded")
			// The link that replaced it deserves a wave of its own rather than
			// the wait left over from the one thrown away.
			w.lastImport = prevImport
			return
		}
		if errors.Is(err, vpnconfig.ErrSaveServers) {
			slog.Warn("Failed to save the refreshed subscription servers", "error", err)
		} else {
			slog.Warn("Failed to sync xray.servers after the subscription refresh", "error", err)
		}
		w.notifyRefreshFailed(cfg)
		return
	}
	slog.Info("Subscription refreshed", "servers", len(servers))
	if w.Generate == nil {
		return
	}

	var active *vpnconfig.ActiveServer
	if cfg != nil {
		active = cfg.Xray.ActiveServer
	}
	started := activeID(active)
	startedSeq := vpnconfig.ActiveSeq(active)
	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	order := pickOrder(servers, active)
	var preferred *vpnconfig.Server
	if active != nil && len(order) > 0 && sameServer(order[0], active) {
		preferred = &order[0]
	}
	tried := 0
	lastGenerated := ""
	// lastRecorded is what active_server names: lastGenerated, unless the record
	// of that config.json failed to save. The checks for a newer selection
	// compare with it, or the walk's own unsaved write reads as someone else's.
	lastRecorded := ""
	// lastSeq is the counter of the walk's own last record, or the one it
	// started from: any other value in the config is someone else's write.
	lastSeq := startedSeq
	for _, s := range order {
		if ctx.Err() != nil || w.stopped() {
			return
		}
		generated, seq, err := w.Generate(s, supersededGuard(started, lastRecorded, lastSeq))
		if errors.Is(err, errSuperseded) {
			slog.Info("Subscription walk abandoned; a newer server was selected")
			return
		}
		if err != nil || !generated {
			slog.Debug("Generating Xray config for server failed", "server", s.Name, "generated", generated, "error", err)
		}
		if !generated {
			continue
		}
		lastGenerated = serverID(s)
		if err == nil {
			lastRecorded = lastGenerated
		}
		lastSeq = seq
		tried++
		if ctx.Err() != nil {
			return
		}
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
		if w.walkSuperseded(started, lastRecorded, lastSeq) {
			slog.Info("Subscription walk abandoned; a newer server was selected")
			return
		}
		// Only a committed failover left Xray. Staged clients never left, so
		// "back on Xray" would be a false message.
		announceRestored := failoverTunnel(cfg) != "" && (!vpnconfig.FailoverStaged(cfg) || w.pendingRestoreNotify)
		if !w.commitRestore(cfg) {
			return
		}
		if announceRestored {
			slog.Info("Xray clients restored", "server", s.Name)
			w.notify(noteRestored, fmt.Sprintf(msgRestored, s.Name))
		} else {
			w.notify(noteRestored, fmt.Sprintf(msgPicked, s.Name))
		}
		return
	}
	if w.stopped() {
		return
	}
	slog.Info("No live server in the subscription", "tried", tried)
	if w.walkSuperseded(started, lastRecorded, lastSeq) {
		slog.Info("Subscription walk abandoned; a newer server was selected")
		return
	}
	if preferred != nil && lastGenerated != "" && lastGenerated != serverID(*preferred) {
		if w.returnToPreferred(*preferred, supersededGuard(started, lastRecorded, lastSeq)) {
			slog.Info("Subscription walk abandoned; a newer server was selected")
			return
		}
		if w.stopped() {
			return
		}
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
// abandoned means guard found a newer selection and nothing was written.
func (w *Watch) returnToPreferred(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (abandoned bool) {
	generated, _, err := w.Generate(s, guard)
	if errors.Is(err, errSuperseded) {
		return true
	}
	if err != nil || !generated {
		slog.Warn("Failed to return the Xray config to the preferred server", "server", s.Name, "generated", generated, "error", err)
	}
	if !generated {
		return false
	}
	if w.RestartXray != nil {
		if err := w.RestartXray(); err != nil {
			slog.Warn("Xray restart on the preferred server failed", "server", s.Name, "error", err)
			return false
		}
	}
	if err == nil {
		slog.Info("Xray config returned to the preferred server", "server", s.Name)
	}
	return false
}

// errStopped is an apply the watch did not make, or the script skipped: VPN
// Director was stopped, and the tick that sees it writes, applies and announces
// nothing more.
var errStopped = errors.New("VPN Director is stopped")

// stopped reports the marker /stop leaves. A tick checks it first and again
// after every wait - a probe, a restart, an apply queued for the lock - because
// a stop that lands in between must not be undone by the rest of the tick.
func (w *Watch) stopped() bool {
	return w.Stopped != nil && w.Stopped()
}

func (w *Watch) apply() error {
	if w.Apply == nil {
		return nil
	}
	if w.stopped() {
		return errStopped
	}
	if err := w.Apply(); err != nil {
		return err
	}
	// Apply runs with --unless-stopped: the script skips it with exit 0 when a
	// stop took the lock first, and the marker that stop left is how to tell.
	if w.stopped() {
		return errStopped
	}
	return nil
}

func (w *Watch) socksPort(cfg *vpnconfig.VPNDirectorConfig) int {
	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		return defaultSOCKSPort
	}
	return socks
}

func (w *Watch) probeOK(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if w.Probe == nil {
		return false
	}
	return w.Probe(ctx, w.socksPort(cfg)) == nil
}

// ServerForDial uses a tunnel-resolved IPv4 for vnext so Xray does not go
// back to the system resolver. SNI keeps the hostname. Web UI /xray keep
// s.Address and let Xray resolve, so a CDN IP change still works there.
func ServerForDial(s vpnconfig.Server) vpnconfig.Server {
	host := s.Address
	for _, ip := range s.IPs {
		if ip == "" {
			continue
		}
		s.Address = ip
		if s.SNI == "" {
			s.SNI = host
		}
		break
	}
	return s
}

func (w *Watch) tproxyReady() bool {
	if w.TPROXYReady == nil {
		return true
	}
	return w.TPROXYReady()
}

// commitRestore puts snapshot addresses on Xray while they stay on the
// fallback tunnel, applies, and only then drops tunnel membership — after
// TPROXY is confirmed. A SOCKS-only success with tproxy_apply soft-fail must
// not strip kernel fallback routing.
func (w *Watch) commitRestore(cfg *vpnconfig.VPNDirectorConfig) bool {
	if w.stopped() {
		return false
	}
	if failoverTunnel(cfg) == "" {
		if err := w.apply(); err != nil {
			slog.Warn("Apply after picking an Xray server failed", "error", err)
			return false
		}
		return true
	}
	if !w.tproxyReady() && !w.lastTPROXYFail.IsZero() && w.Now().Sub(w.lastTPROXYFail) < ImportRetry {
		return false
	}
	startedCommitted := !vpnconfig.FailoverStaged(cfg)
	if w.UpdateVPN != nil {
		if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
			vpnconfig.EnsureFailoverStaged(current)
			return nil
		}); err != nil {
			slog.Warn("Failed to stage Xray clients for restore", "error", err)
			return false
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after staging Xray clients for restore failed", "error", err)
		w.pendingApply = true
		if startedCommitted {
			w.pendingRestoreNotify = true
		}
		return false
	}
	if !w.tproxyReady() {
		slog.Warn("TPROXY is not intercepting LAN; keeping fallback routing")
		w.lastTPROXYFail = w.Now()
		if startedCommitted {
			w.pendingRestoreNotify = true
		}
		return false
	}
	w.lastTPROXYFail = time.Time{}
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
		slog.Warn("Apply after dropping fallback membership failed", "error", err)
		w.pendingApply = true
		w.pendingRestoreNotify = true
		return false
	}
	w.pendingApply = false
	w.pendingRestoreNotify = false
	w.failSince = time.Time{}
	w.lastImport = time.Time{}
	w.importRetry = 0
	return true
}

func committedFailover(cfg *vpnconfig.VPNDirectorConfig) bool {
	return cfg != nil && cfg.Xray.Failover != nil && !vpnconfig.FailoverStaged(cfg)
}

func (w *Watch) notifyRefreshFailed(cfg *vpnconfig.VPNDirectorConfig) {
	if committedFailover(cfg) {
		w.notify(noteRefreshFailed, fmt.Sprintf(msgRefreshFailedOn, failoverTunnel(cfg)))
		return
	}
	w.notify(noteRefreshFailed, msgRefreshFailed)
}

func (w *Watch) notifyNoLive(cfg *vpnconfig.VPNDirectorConfig) {
	if committedFailover(cfg) {
		w.notify(noteNoLive, fmt.Sprintf(msgNoLiveOn, failoverTunnel(cfg)))
		return
	}
	w.notify(noteNoLive, msgNoLive)
}

func (w *Watch) retryOrSwitchFallback(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) *vpnconfig.VPNDirectorConfig {
	now := w.Now()
	if w.lastFallbackFail.IsZero() {
		w.lastFallbackFail = now
		return cfg
	}
	if now.Sub(w.lastFallbackFail) < ImportRetry {
		return cfg
	}
	w.lastFallbackFail = now
	if err := w.apply(); err != nil {
		if errors.Is(err, errStopped) {
			return cfg
		}
		slog.Warn("Apply retry while the failover tunnel is not ready failed", "error", err)
	} else if w.fallbackReady(cfg) {
		return cfg
	}
	skip := failoverTunnel(cfg)
	var plat vpnconfig.PlatformInfo
	if w.LoadPlatform != nil {
		if p, err := w.LoadPlatform(); err != nil {
			return cfg
		} else {
			plat = p
		}
	}
	// The lookup blocks too; a /stop during it rules out moving the failover.
	if w.stopped() {
		return cfg
	}
	next := vpnconfig.NextTDExit(cfg, plat, skip)
	if next == "" || w.UpdateVPN == nil {
		return cfg
	}
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.RestoreXrayClientsFromFailover(current)
		vpnconfig.StageXrayClientsToTunnel(current, next)
		return nil
	}); err != nil {
		slog.Warn("Failed to retarget the Xray failover", "from", skip, "to", next, "error", err)
		return cfg
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after retargeting the Xray failover failed", "tunnel", next, "error", err)
		w.pendingApply = true
	}
	if reloaded, err := w.LoadVPN(); err == nil {
		return reloaded
	}
	return cfg
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
	route := kind == noteMoved || kind == noteNoTunnel || kind == noteFallbackNotReady
	last := &w.lastImportKind
	if route {
		last = &w.lastRouteKind
	}
	if *last == kind {
		return
	}
	n := w.Notify
	if n == nil {
		*last = kind
		if route {
			w.lastImportKind = noteNone
		}
		return
	}
	w.mu.Unlock()
	n(msg)
	w.mu.Lock()
	if *last == kind {
		return
	}
	*last = kind
	if route {
		w.lastImportKind = noteNone
	}
}
