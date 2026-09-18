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
	// FallbackCheck is how often a committed failover asks the platform about
	// its tunnel, and FallbackDownAfter how long that tunnel has to be gone
	// before the clients leave it: a reconnecting tunnel shows as down for a
	// moment.
	FallbackCheck     = time.Minute
	FallbackDownAfter = time.Minute
	defaultSOCKSPort  = 12346
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

	mu                sync.Mutex
	failSince         time.Time // zero => last probe succeeded
	lastImport        time.Time
	importRetry       time.Duration   // current wait between import waves; zero means ImportRetry
	lastRouteKind     noteKind        // noteMoved, noteNoTunnel
	lastImportKind    noteKind        // noteRefreshFailed, noteNoLive, noteRestored
	pendingApply      bool            // JSON mutated; Apply has not yet succeeded
	pendingRestore    *restoreAttempt // a restore whose last apply failed: what it removed, for the retry
	reconciled        bool            // the first armed Tick has checked for a failover left by an earlier process
	lastNoTunnelCheck time.Time       // last LoadPlatform while announcing no fallback
	lastTPROXYFail    time.Time       // last apply that found TPROXY not intercepting
	lastFallbackFail  time.Time       // last staged apply whose fallback was not ready
	fallbackTried     map[string]bool // exits this round of retargets has tried
	fallbackHold      time.Duration   // wait after a round that found no exit ready: 10, 20, then 30 minutes
	fallbackHoldUntil time.Time       // no retarget before this
	lastFallbackCheck time.Time       // last platform lookup for a committed failover's tunnel
	fallbackDownSince time.Time       // since when that tunnel is no exit; zero while it is one
	running           bool
}

// restoreAttempt is a restore whose failover record is gone and whose last
// apply has not succeeded yet: what the record said, so the retry can put it
// back if that apply loses TPROXY, and whether its clients had left Xray.
type restoreAttempt struct {
	removed   *vpnconfig.XrayFailover
	restored  []string
	committed bool
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
		// Nothing the outbound did while VPN Director is stopped counts: the
		// three minutes start again once it runs.
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
		if w.probeOK(ctx, cfg) {
			if !w.tproxyReady() && !w.lastTPROXYFail.IsZero() && w.Now().Sub(w.lastTPROXYFail) < ImportRetry {
				return
			}
			if done, committed := w.commitRestore(cfg); done && committed {
				w.announceRestored(activeName(cfg))
			}
			return
		}
		// A /stop can land while the probe waits. What follows drops Xray
		// membership and announces; a stopped tick does neither.
		if w.stopped() {
			return
		}
		// Xray is failing. Kept through the failover, so clients that go back
		// to a dead Xray - their tunnel gone, no other exit - are not given
		// three more minutes before the watch looks for another fallback.
		if w.failSince.IsZero() {
			w.failSince = w.Now()
		}
		cfg = w.extendFailover(cfg)
		staged := vpnconfig.FailoverStaged(cfg)
		wasPending := w.pendingApply || staged
		var ok bool
		cfg, ok = w.applyFailover(cfg)
		if w.stopped() {
			return
		}
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
		if ok {
			cfg = w.watchFallback(cfg)
			if w.stopped() {
				return
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
			// Keep probing: a dead outbound during a stuck restore-Apply
			// must still be able to fail over again.
		} else {
			w.pendingApply = false
			attempt := w.pendingRestore
			w.pendingRestore = nil
			if attempt != nil {
				// This retry is the apply that drops the tunnel membership,
				// the one that has to keep TPROXY up - checked here as after
				// the first try, or a soft-failed retry would be announced
				// with the clients on neither the proxy nor the tunnel.
				if !w.tproxyReady() {
					slog.Warn("TPROXY stopped intercepting during the restore; putting the clients back on the fallback tunnel")
					w.lastTPROXYFail = w.Now()
					w.reinstateFailover(attempt)
					return
				}
				w.settled()
				if attempt.committed {
					w.announceRestored(activeName(cfg))
				}
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
	if id != "" && len(vpnconfig.CarriableXrayClients(cfg)) == 0 {
		// Clients the tunnel cannot mark stay on Xray, so with none it can
		// there is nothing to move: no fallback, as far as they are concerned.
		if announce {
			slog.Warn("No Xray client is one Tunnel Director can carry", "tunnel", id, "clients", vpnconfig.EffectiveXrayClients(cfg))
		}
		id = ""
	}
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
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.StageXrayClientsToTunnel(current, id)
		if current.Xray.Failover == nil {
			return fmt.Errorf("tunnel %s no longer configured, or no client it can carry", id)
		}
		movedClients = len(current.Xray.Failover.Clients)
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to move Xray clients to Tunnel Director", "tunnel", id, "error", err)
		}
		return
	}
	// A new episode: the retry clock and the exits tried belong to the last one.
	w.resetFallbackState()
	if left := uncarried(cfg); len(left) > 0 {
		slog.Warn("Xray clients Tunnel Director cannot carry stay on Xray", "clients", left)
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
	// The commit inside can wait for the config lock; a stop that finished
	// meanwhile refused it, and nothing below may announce or retry it.
	if w.stopped() {
		return
	}
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
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			vpnconfig.CommitXrayFailover(current)
			return nil
		}); err != nil {
			if !errors.Is(err, errStopped) {
				slog.Warn("Failed to drop staged Xray clients after the tunnel apply", "error", err)
			}
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
	// The fallback carries them: what the retargets tried is done with.
	w.resetFallbackState()
	return cfg, true
}

// extendFailover brings the Xray clients that are not with the failover yet -
// added, re-added or resumed while Xray is down - onto its tunnel. The dead
// outbound would take them nowhere for the rest of the failover. They are
// staged the way the snapshot was, so the apply that installs their TUN_DIR
// rules runs before the one that drops them from Xray.
func (w *Watch) extendFailover(cfg *vpnconfig.VPNDirectorConfig) *vpnconfig.VPNDirectorConfig {
	if w.UpdateVPN == nil || len(vpnconfig.XrayClientsOutsideFailover(cfg)) == 0 {
		return cfg
	}
	var joined []string
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		joined = vpnconfig.XrayClientsOutsideFailover(current)
		vpnconfig.ExtendXrayFailover(current)
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to bring new Xray clients onto the failover tunnel", "error", err)
		}
		return cfg
	}
	if len(joined) > 0 {
		slog.Info("Xray clients join the failover", "tunnel", failoverTunnel(cfg), "clients", joined)
		w.pendingApply = true
	}
	if reloaded, err := w.LoadVPN(); err == nil {
		return reloaded
	}
	return cfg
}

// watchFallback looks at the tunnel of a committed failover while Xray stays
// down. Nothing else did once the clients were off Xray, and a tunnel that went
// down sent them out through the WAN until Xray came back. Gone for
// FallbackDownAfter, it is replaced by another exit; with none left the clients
// go back to Xray, where a dead outbound takes them nowhere - what a death with
// no fallback does too. An empty tunnel list is no answer (Keenetic prints one
// while RCI does not reply), and neither is a failed lookup.
func (w *Watch) watchFallback(cfg *vpnconfig.VPNDirectorConfig) *vpnconfig.VPNDirectorConfig {
	if w.LoadPlatform == nil || w.UpdateVPN == nil || !committedFailover(cfg) {
		return cfg
	}
	now := w.Now()
	if !w.lastFallbackCheck.IsZero() && now.Sub(w.lastFallbackCheck) < FallbackCheck {
		return cfg
	}
	w.lastFallbackCheck = now
	plat, err := w.LoadPlatform()
	if err != nil || len(plat.Tunnels) == 0 {
		return cfg
	}
	// LoadPlatform shells out and takes no lock; a /stop may have finished.
	if w.stopped() {
		return cfg
	}
	id := failoverTunnel(cfg)
	for _, exit := range vpnconfig.TDExits(cfg, plat) {
		if exit == id {
			w.fallbackDownSince = time.Time{}
			return cfg
		}
	}
	if w.fallbackDownSince.IsZero() {
		w.fallbackDownSince = now
		return cfg
	}
	if now.Sub(w.fallbackDownSince) < FallbackDownAfter {
		return cfg
	}
	next := vpnconfig.NextTDExit(cfg, plat, map[string]bool{id: true})
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.RestoreXrayClientsFromFailover(current)
		if next != "" {
			vpnconfig.StageXrayClientsToTunnel(current, next)
		}
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to move the Xray clients off a failover tunnel that is gone", "tunnel", id, "error", err)
		}
		return cfg
	}
	if next != "" {
		slog.Warn("Failover tunnel is no longer a Tunnel Director exit; moving the Xray clients", "from", id, "to", next)
		// They are told where they went once that tunnel carries them, and the
		// message is the one that told them about this tunnel.
		w.lastRouteKind = noteNone
	} else {
		slog.Warn("Failover tunnel is no longer a Tunnel Director exit and there is no other; the Xray clients go back to Xray", "tunnel", id)
	}
	w.resetFallbackState()
	if err := w.apply(); err != nil {
		slog.Warn("Apply after moving the Xray clients off the failover tunnel failed", "error", err)
		w.pendingApply = true
	}
	if reloaded, err := w.LoadVPN(); err == nil {
		return reloaded
	}
	return cfg
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

// walkGuard is the guard every write of the walk carries. It runs under the
// config lock Generate takes, after whatever wait that lock cost: a /stop that
// finished meanwhile refuses the write there - a new config.json and
// active_server on a stopped router would take effect on the next manual
// apply - and a newer selection does as before.
func (w *Watch) walkGuard(started, lastRecorded string, expectedSeq int) func(*vpnconfig.VPNDirectorConfig) error {
	superseded := supersededGuard(started, lastRecorded, expectedSeq)
	return func(cfg *vpnconfig.VPNDirectorConfig) error {
		if w.stopped() {
			return errStopped
		}
		return superseded(cfg)
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
	var update func(func(*vpnconfig.VPNDirectorConfig) error) error
	if w.UpdateVPN != nil {
		update = w.update
	}
	if err := vpnconfig.PublishServers(update, w.SaveServers, servers, "", vpnconfig.SubscriptionUnchanged(rawURL)); err != nil {
		if errors.Is(err, errStopped) {
			// Refused under the lock: the wave did not happen.
			w.lastImport = prevImport
			return
		}
		if errors.Is(err, vpnconfig.ErrSubscriptionChanged) {
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
		generated, seq, err := w.Generate(s, w.walkGuard(started, lastRecorded, lastSeq))
		if errors.Is(err, errStopped) {
			return
		}
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
		done, committed := w.commitRestore(cfg)
		if !done {
			return
		}
		if committed {
			w.announceRestored(s.Name)
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
		if w.returnToPreferred(*preferred, w.walkGuard(started, lastRecorded, lastSeq)) {
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
	if errors.Is(err, errStopped) {
		// Nothing was written; the caller ends the tick on the marker.
		return false
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

// update is UpdateVPN with the stop marker checked inside the locked callback,
// after whatever the wait for the config lock cost. The check made before that
// wait says nothing about the router after it, and a write that lands on a
// stopped router takes effect on its next manual apply.
func (w *Watch) update(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	return w.UpdateVPN(func(cfg *vpnconfig.VPNDirectorConfig) error {
		if w.stopped() {
			return errStopped
		}
		return fn(cfg)
	})
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
//
// done is a finished restore, or an applied pick without a failover. committed
// says the failover's clients had left Xray - taken from the record at the
// start, since this is what stages them back - and is what the caller
// announces as "back on Xray".
func (w *Watch) commitRestore(cfg *vpnconfig.VPNDirectorConfig) (done, committed bool) {
	if w.stopped() {
		return false, false
	}
	if failoverTunnel(cfg) == "" {
		if err := w.apply(); err != nil {
			slog.Warn("Apply after picking an Xray server failed", "error", err)
			return false, false
		}
		w.settled()
		return true, false
	}
	committed = vpnconfig.FailoverCommitted(cfg)
	// Staging the snapshot back into xray.clients is what hands these clients to
	// TPROXY, and the marker is the only thing that says TPROXY can carry them:
	// the PREROUTING jumps go in even when the platform's own rules do not - on
	// Keenetic a missing mangle INPUT accept drops proxied HTTPS in
	// _NDM_HTTP_INPUT_TLS_ - and Xray wins over TUN_DIR, so the tunnel they are
	// still on would carry nothing. Until the marker is there they stay where
	// they work, and only the apply that may yet install those rules is retried,
	// on the import cadence rather than every tick.
	if !w.tproxyReady() {
		if !w.lastTPROXYFail.IsZero() && w.Now().Sub(w.lastTPROXYFail) < ImportRetry {
			return false, committed
		}
		if err := w.apply(); err != nil {
			slog.Warn("Apply retry while TPROXY is not intercepting failed", "error", err)
			w.lastTPROXYFail = w.Now()
			return false, committed
		}
		if !w.tproxyReady() {
			slog.Warn("TPROXY is not intercepting LAN; keeping the clients on the fallback tunnel")
			w.lastTPROXYFail = w.Now()
			return false, committed
		}
		w.lastTPROXYFail = time.Time{}
	}
	if w.UpdateVPN != nil {
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			vpnconfig.EnsureFailoverStaged(current)
			return nil
		}); err != nil {
			if !errors.Is(err, errStopped) {
				slog.Warn("Failed to stage Xray clients for restore", "error", err)
			}
			return false, committed
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after staging Xray clients for restore failed", "error", err)
		w.pendingApply = true
		return false, committed
	}
	if !w.tproxyReady() {
		slog.Warn("TPROXY is not intercepting LAN; keeping fallback routing")
		w.lastTPROXYFail = w.Now()
		if committed {
			w.unstageRestore()
		}
		return false, committed
	}
	w.lastTPROXYFail = time.Time{}
	attempt := &restoreAttempt{committed: committed}
	if w.UpdateVPN != nil {
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			if current.Xray.Failover != nil {
				fo := *current.Xray.Failover
				attempt.removed = &fo
			}
			attempt.restored = vpnconfig.RestoreXrayClientsFromFailover(current)
			return nil
		}); err != nil {
			if !errors.Is(err, errStopped) {
				slog.Warn("Failed to restore Xray clients from the failover", "error", err)
			}
			return false, committed
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after dropping fallback membership failed", "error", err)
		w.pendingApply = true
		w.pendingRestore = attempt
		return false, committed
	}
	if !w.tproxyReady() {
		// The apply that dropped the fallback membership is also the one that
		// had to keep TPROXY up. It soft-failed - exit 0, marker gone - so these
		// clients have neither the proxy nor the tunnel, and a finished restore
		// would leave nothing to try again. They go back to the failover they
		// came from; the ready gate above restores them once the marker returns.
		slog.Warn("TPROXY stopped intercepting during the restore; putting the clients back on the fallback tunnel")
		w.lastTPROXYFail = w.Now()
		w.reinstateFailover(attempt)
		return false, committed
	}
	w.pendingApply = false
	w.pendingRestore = nil
	w.settled()
	w.resetFallbackState()
	return true, committed
}

// unstageRestore takes a committed failover's clients off Xray again after the
// apply that staged them for a restore lost the TPROXY marker. Staging is what
// hands them to TPROXY, and Xray wins over TUN_DIR: left there they meet a
// TPROXY that cannot carry them while the tunnel they are still on carries
// nothing. The next attempt stages them again once the marker is back.
func (w *Watch) unstageRestore() {
	if w.UpdateVPN == nil {
		return
	}
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.CommitXrayFailover(current)
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to take the Xray clients off Xray again", "error", err)
		}
		return
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after taking the Xray clients off Xray again failed", "error", err)
		w.pendingApply = true
	}
}

// reinstateFailover puts a restore that did not hold back the way it started:
// committed - the restored addresses leave xray.clients, where a TPROXY that
// cannot carry them would keep TUN_DIR from seeing them - or, for a stage that
// was never committed, back on the tunnel with the clients still on Xray, as
// they were all along. Only what the restore moved goes back, under the record
// that was removed: an address the user took off the tunnel meanwhile stays off.
func (w *Watch) reinstateFailover(attempt *restoreAttempt) {
	if attempt == nil || attempt.removed == nil || len(attempt.restored) == 0 || w.UpdateVPN == nil {
		return
	}
	removed, restored := attempt.removed, attempt.restored
	snapshot := &vpnconfig.XrayFailover{
		Tunnel:    removed.Tunnel,
		Clients:   restored,
		Added:     keepOnly(removed.Added, restored),
		Committed: attempt.committed,
	}
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.ApplyFailoverSnapshot(current, snapshot)
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to put the Xray clients back on the fallback tunnel", "error", err)
		}
		return
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after putting the Xray clients back on the fallback tunnel failed", "error", err)
		w.pendingApply = true
	}
}

// keepOnly is list without the entries keep does not name. A nil list stays
// nil: a failover record without Added is the older kind, whose restore drops
// every snapshot address from the tunnel, and an empty one would drop none.
func keepOnly(list, keep []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		for _, k := range keep {
			if s == k {
				out = append(out, s)
				break
			}
		}
	}
	return out
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
	if !w.fallbackHoldUntil.IsZero() && now.Before(w.fallbackHoldUntil) {
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
	if w.fallbackTried == nil {
		w.fallbackTried = map[string]bool{}
	}
	w.fallbackTried[skip] = true
	next := vpnconfig.NextTDExit(cfg, plat, w.fallbackTried)
	if next == "" {
		// Every exit has had its turn and none became ready. Starting over at
		// once traded the clients between the first two exits every five
		// minutes, each move a TUN_DIR rebuild for every client of every
		// tunnel. The apply is still retried; the next round waits 10, 20, then
		// 30 minutes.
		w.fallbackTried = nil
		if w.fallbackHold == 0 {
			w.fallbackHold = 2 * ImportRetry
		} else {
			w.fallbackHold = min(2*w.fallbackHold, ImportRetryMax)
		}
		w.fallbackHoldUntil = now.Add(w.fallbackHold)
		slog.Info("No Tunnel Director fallback became ready; the next round of exits waits", "after", w.fallbackHold)
		return cfg
	}
	if w.UpdateVPN == nil {
		return cfg
	}
	if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.RestoreXrayClientsFromFailover(current)
		vpnconfig.StageXrayClientsToTunnel(current, next)
		return nil
	}); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Failed to retarget the Xray failover", "from", skip, "to", next, "error", err)
		}
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

// settled is a working outbound: the three minutes start again from its next
// miss, and the next death refreshes the subscription at once.
func (w *Watch) settled() {
	w.failSince = time.Time{}
	w.lastImport = time.Time{}
	w.importRetry = 0
}

// resetFallbackState ends what one failover episode knew about its fallbacks:
// the retry clock, the exits tried and the wait between rounds, and the look at
// a committed failover's tunnel. The next episode starts from nothing.
func (w *Watch) resetFallbackState() {
	w.lastFallbackFail = time.Time{}
	w.fallbackTried = nil
	w.fallbackHold = 0
	w.fallbackHoldUntil = time.Time{}
	w.lastFallbackCheck = time.Time{}
	w.fallbackDownSince = time.Time{}
}

func (w *Watch) announceRestored(server string) {
	slog.Info("Xray clients restored", "server", server)
	w.notify(noteRestored, fmt.Sprintf(msgRestored, server))
}

func activeName(cfg *vpnconfig.VPNDirectorConfig) string {
	if cfg != nil && cfg.Xray.ActiveServer != nil {
		return cfg.Xray.ActiveServer.Name
	}
	return "unknown"
}

// uncarried is the effective Xray clients Tunnel Director cannot carry: they
// stay on Xray through a failover.
func uncarried(cfg *vpnconfig.VPNDirectorConfig) []string {
	var out []string
	for _, ip := range vpnconfig.EffectiveXrayClients(cfg) {
		if !vpnconfig.TDCarries(ip) {
			out = append(out, ip)
		}
	}
	return out
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
