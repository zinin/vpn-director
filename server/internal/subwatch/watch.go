package subwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
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
	// FetchTimeout bounds one subscription download together with the resolution
	// of every host in it: a resolver that answers nothing costs seconds per
	// host, and the tick - every probe behind it - would wait all of them out.
	FetchTimeout = 3 * time.Minute
	// FastDeadAfter is how long the probe has to fail before an outbound whose
	// server accepts no TCP connection while the WAN works counts as dead;
	// every other failure, a WAN outage included, waits DeadAfter. ReachTimeout
	// bounds one look at a server's addresses.
	FastDeadAfter = time.Minute
	ReachTimeout  = 3 * time.Second
	// FallbackCheck is how often a committed failover asks the platform about
	// its tunnel and looks for failover_ready, and FallbackDownAfter how long
	// that tunnel has to be gone, or not carry the clients, before they leave
	// it: a reconnecting tunnel shows as down for a moment.
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
	msgReturned         = "Xray back on the preferred server %s"
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
	noteReturned
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
	LoadServers   func() ([]vpnconfig.Server, error)
	Reachable     func(ctx context.Context, ip string, port int) bool // nil => no TCP checks: no fast death, no return
	Notify        func(msg string)
	Now           func() time.Time
	AfterRestart  func(time.Duration)
	FallbackReady func(tunnel string) bool // nil => ready; false keeps Xray membership
	TPROXYReady   func() bool              // nil => ready; false keeps fallback membership after restore
	Stopped       func() bool              // nil => not stopped; true skips apply/restart after /stop

	mu                sync.Mutex
	failSince         time.Time // zero => last probe succeeded
	downChecks        int       // checks since failSince that found the active server down; -1 once one did not
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
	returnNotBefore   time.Time         // no look for the preferred server before this
	returnRetry       time.Duration     // wait after the last failed return; zero before any
	returnFails       int               // returns in a row that failed; at ReturnFailsMax the returns stop
	lastReturn        time.Time         // when the last return proved live; zero once it held for ReturnHold or a death followed it
	returnDeath       time.Time         // failSince of the last death the returns were settled at
	lastPicked        *vpnconfig.Server // the copy the walk picked or a return proved, with the address it ran on
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
	// A restore whose last apply failed is work the watch has started, as a
	// failover is: neither waits for a saved link or for Xray clients.
	if !vpnconfig.Armed(cfg) && !w.pendingApply {
		w.resetFail()
		return
	}
	if w.stopped() {
		// Nothing the outbound did while VPN Director is stopped counts: the
		// three minutes start again once it runs.
		w.resetFail()
		return
	}
	// A download, the resolution behind it or a probe can hold the tick for
	// minutes; a stop ends what it is waiting on rather than waiting with it.
	ctx, endWatch := w.cancelOnStop(ctx)
	defer endWatch()
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
			done, committed, refused := w.commitRestore(cfg, probedServer(cfg))
			if done && committed {
				w.announceRestored(activeName(cfg))
			} else if errors.Is(refused, errSuperseded) {
				slog.Info("Restore put off; a newer server was selected after the probe")
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
	// A pending restore is all an unarmed watch finishes: a failover of its own
	// needs a link to refresh and Xray clients to move.
	if !vpnconfig.Armed(cfg) {
		w.resetFail()
		return
	}

	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	err = w.Probe(ctx, socks)
	if err == nil {
		w.resetFail()
		w.importRetry = 0
		w.lastRouteKind = noteNone
		w.lastImportKind = noteNone
		w.maybeReturn(ctx, cfg)
		return
	}

	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	// Past DeadAfter the outbound is dead whatever a look finds.
	if now.Sub(w.failSince) < DeadAfter {
		w.checkReach(ctx, cfg)
	}
	reason := w.deadReason(now)
	if reason == "" {
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
	// Later ticks of the same death come back here - no tunnel, a failed
	// platform lookup, a failed stage - and the returns settle once per death.
	if !w.returnDeath.Equal(w.failSince) {
		w.returnDeath = w.failSince
		w.returnAfterDeath()
	}
	// With no tunnel to move to, every later tick comes back here: log the
	// transition only until the user has been told there is no fallback.
	announce := w.lastRouteKind != noteNoTunnel
	if announce {
		slog.Info("Xray outbound declared dead", "socks_port", socks, "reason", reason, "error", err)
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
// down sent them out through the WAN until Xray came back. So did one the
// platform still listed while Tunnel Director no longer sent the clients into
// it - a firewall restart took the rules, and the apply after it could not put
// them back and withheld failover_ready - which counts as gone too once an
// apply of the watch's own has not brought it back. Gone for
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
	gone := "is no longer a Tunnel Director exit"
	for _, exit := range vpnconfig.TDExits(cfg, plat) {
		if exit != id {
			continue
		}
		if w.fallbackCarries(cfg) {
			w.fallbackDownSince = time.Time{}
			return cfg
		}
		gone = "does not carry its clients"
		break
	}
	// The apply fallbackCarries runs can wait for the script lock.
	if w.stopped() {
		return cfg
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
		slog.Warn("Failover tunnel "+gone+"; moving the Xray clients to another exit", "from", id, "to", next)
		// They are told where they went once that tunnel carries them, and the
		// message is the one that told them about this tunnel.
		w.lastRouteKind = noteNone
	} else {
		slog.Warn("Failover tunnel "+gone+" and there is no other exit; the Xray clients go back to Xray", "tunnel", id)
		// Announced here, not by the death path on the next tick: a watch
		// whose link is gone does not reach it.
		w.notify(noteNoTunnel, msgNoTunnel)
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

// fallbackCarries reports whether Tunnel Director carries a committed
// failover's clients: tunnel_apply wrote failover_ready for its tunnel, or the
// failover has nobody left to carry. The marker says what the last apply found,
// and an apply is what puts back the rules a firewall restart took, so one runs
// before the tunnel counts as not carrying them.
func (w *Watch) fallbackCarries(cfg *vpnconfig.VPNDirectorConfig) bool {
	if !vpnconfig.FailoverCarries(cfg) || w.fallbackReady(cfg) {
		return true
	}
	if err := w.apply(); err != nil {
		if !errors.Is(err, errStopped) {
			slog.Warn("Apply retry while the failover tunnel does not carry the Xray clients failed", "error", err)
		}
		return false
	}
	return w.fallbackReady(cfg)
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

// chosenIndex is where servers has the server a names: the entry with its name,
// address and port, or else the first entry with its name - a subscription that
// rotates endpoints gives a name a new address every day, and the name is what
// the user chose. -1 when the list has neither.
func chosenIndex(servers []vpnconfig.Server, a *vpnconfig.ActiveServer) int {
	if a == nil || a.Name == "" {
		return -1
	}
	byName := -1
	for i, s := range servers {
		if s.Name != a.Name {
			continue
		}
		if sameServer(s, a) {
			return i
		}
		if byName < 0 {
			byName = i
		}
	}
	return byName
}

// pickOrder is servers with the one chosen names moved to the front.
func pickOrder(servers []vpnconfig.Server, chosen *vpnconfig.ActiveServer) []vpnconfig.Server {
	i := chosenIndex(servers, chosen)
	if i < 0 {
		return servers
	}
	order := make([]vpnconfig.Server, 0, len(servers))
	order = append(order, servers[i])
	order = append(order, servers[:i]...)
	return append(order, servers[i+1:]...)
}

// perAddress lists each server once for every address it resolved to, each copy
// with that address alone, so the walk dials them one after another; a server
// with none is listed as it is. An endpoint ban takes an address, not the name:
// a host can resolve to one the router cannot reach and another it can, and
// dialing only the first rejected the whole server.
func perAddress(servers []vpnconfig.Server) []vpnconfig.Server {
	out := make([]vpnconfig.Server, 0, len(servers))
	for _, s := range servers {
		n := len(out)
		for _, ip := range s.IPs {
			if ip == "" {
				continue
			}
			c := s
			c.IPs = []string{ip}
			out = append(out, c)
		}
		if len(out) == n {
			out = append(out, s)
		}
	}
	return out
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

// walkOwns is nil while the walk may still write, and otherwise why not: the
// saved link is no longer the one its list came from - a server of the old
// list may be gone from the new one, and the new link deserves a wave of its
// own - or active_server names a server the walk did not record.
func walkOwns(cfg *vpnconfig.VPNDirectorConfig, rawURL, started, lastRecorded string, expectedSeq int) error {
	if err := vpnconfig.SubscriptionUnchanged(rawURL)(cfg); err != nil {
		return err
	}
	if superseded(cfg, started, lastRecorded, expectedSeq) {
		return errSuperseded
	}
	return nil
}

// walkOwnsNow is walkOwns on a fresh read of the config, for the checks of
// the walk that write nothing themselves.
func (w *Watch) walkOwnsNow(rawURL, started, lastRecorded string, expectedSeq int) error {
	if w.LoadVPN == nil {
		return nil
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return nil
	}
	return walkOwns(cfg, rawURL, started, lastRecorded, expectedSeq)
}

// walkGuard is the guard every write of the walk carries. It runs under the
// config lock Generate takes, after whatever wait that lock cost: a /stop that
// finished meanwhile refuses the write there - a new config.json and
// active_server on a stopped router would take effect on the next manual
// apply - and so do a newly saved link and a Web UI or /xray selection that
// committed after the walk last read the config, instead of being written over.
func (w *Watch) walkGuard(rawURL, started, lastRecorded string, expectedSeq int) func(*vpnconfig.VPNDirectorConfig) error {
	return func(cfg *vpnconfig.VPNDirectorConfig) error {
		if w.stopped() {
			return errStopped
		}
		return walkOwns(cfg, rawURL, started, lastRecorded, expectedSeq)
	}
}

// endsWalk is an error after which the walk writes nothing more: a stop, a newer
// selection, or a link saved since the wave downloaded its own.
func endsWalk(err error) bool {
	return errors.Is(err, errStopped) || errors.Is(err, errSuperseded) || errors.Is(err, vpnconfig.ErrSubscriptionChanged)
}

// walkEnded reports whether err ends the walk, and settles what that leaves: a
// newly saved link gets its wave at once rather than the window this one spent.
func (w *Watch) walkEnded(err error, prevImport time.Time) bool {
	if !endsWalk(err) {
		return false
	}
	switch {
	case errors.Is(err, errSuperseded):
		slog.Info("Subscription walk abandoned; a newer server was selected")
	case errors.Is(err, vpnconfig.ErrSubscriptionChanged):
		slog.Info("Subscription walk abandoned; the saved link changed")
		w.lastImport = prevImport
	}
	return true
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
	rawURL := ""
	if cfg != nil {
		rawURL = cfg.Xray.SubscriptionURL
	}
	if rawURL == "" {
		// A failover outlives its link - import_server_list.sh clears it for a
		// list from a file - and is still seen through, but nothing refreshes.
		return
	}
	now := w.Now()
	if !w.lastImport.IsZero() && now.Sub(w.lastImport) < w.importInterval() {
		return
	}
	prevImport := w.lastImport
	w.lastImport = now

	fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	servers, err := w.Fetch(fetchCtx, rawURL)
	cancel()
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

	var active, chosen *vpnconfig.ActiveServer
	if cfg != nil {
		active = cfg.Xray.ActiveServer
		chosen = cfg.Xray.PreferredServer
	}
	// A walk cut short leaves active_server on a server it was only trying, and
	// preferred_server then keeps the one the user chose.
	if chosen == nil {
		chosen = active
	}
	started := activeID(active)
	startedSeq := vpnconfig.ActiveSeq(active)
	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	order := pickOrder(servers, chosen)
	var preferred *vpnconfig.Server
	if chosenIndex(servers, chosen) >= 0 {
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
	for _, s := range perAddress(order) {
		if ctx.Err() != nil || w.stopped() {
			return
		}
		generated, seq, err := w.Generate(s, w.walkGuard(rawURL, started, lastRecorded, lastSeq))
		if w.walkEnded(err, prevImport) {
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
		if err := w.restartXray(); err != nil {
			if errors.Is(err, errStopped) {
				return
			}
			slog.Debug("Xray restart failed", "server", s.Name, "error", err)
			continue
		}
		w.AfterRestart(SettleAfterRestart)
		if err := w.Probe(ctx, socks); err != nil {
			slog.Debug("Subscription server probe failed", "server", s.Name, "ips", s.IPs, "error", err)
			continue
		}
		slog.Info("Subscription server picked", "server", s.Name, "ips", s.IPs)
		w.lastPicked = &s
		if w.walkEnded(w.walkOwnsNow(rawURL, started, lastRecorded, lastSeq), prevImport) {
			return
		}
		// Only a committed failover left Xray. Staged clients never left, so
		// "back on Xray" would be a false message. The walk's guard goes with
		// the restore's writes: a selection that commits after the look above
		// puts another server in place of the one just probed.
		done, committed, refused := w.commitRestore(cfg, w.walkGuard(rawURL, started, lastRecorded, lastSeq))
		if w.walkEnded(refused, prevImport) || !done {
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
	if w.walkEnded(w.walkOwnsNow(rawURL, started, lastRecorded, lastSeq), prevImport) {
		return
	}
	if preferred != nil && lastGenerated != "" && lastGenerated != serverID(*preferred) {
		if w.walkEnded(w.returnToPreferred(*preferred, w.walkGuard(rawURL, started, lastRecorded, lastSeq)), prevImport) {
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
// The error is one that ends the walk - the guard refused and nothing was
// written, or a stop skipped the restart - and nil otherwise.
func (w *Watch) returnToPreferred(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) error {
	generated, _, err := w.Generate(s, guard)
	if endsWalk(err) {
		return err
	}
	if err != nil || !generated {
		slog.Warn("Failed to return the Xray config to the preferred server", "server", s.Name, "generated", generated, "error", err)
	}
	if !generated {
		return nil
	}
	if rerr := w.restartXray(); rerr != nil {
		if errors.Is(rerr, errStopped) {
			return rerr
		}
		slog.Warn("Xray restart on the preferred server failed", "server", s.Name, "error", rerr)
		return nil
	}
	if err == nil {
		slog.Info("Xray config returned to the preferred server", "server", s.Name)
	}
	return nil
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

// restartXray is RestartXray the way apply is Apply: the restart runs with
// --unless-stopped too, and a probe after one the script skipped would try a
// server that was never started.
func (w *Watch) restartXray() error {
	if w.RestartXray == nil {
		return nil
	}
	if w.stopped() {
		return errStopped
	}
	if err := w.RestartXray(); err != nil {
		return err
	}
	if w.stopped() {
		return errStopped
	}
	return nil
}

// stopPoll is how often a tick looks for the stop marker while it waits.
var stopPoll = time.Second

// cancelOnStop is ctx cancelled once the stop marker appears, so whatever the
// tick is waiting on ends with a /stop instead of running its course. end stops
// the look and returns once it has ended: nothing of it outlives the tick.
func (w *Watch) cancelOnStop(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	if w.Stopped == nil {
		return ctx, cancel
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(stopPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if w.Stopped() {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-ended
	}
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
// back to the system resolver. A TLS server name keeps the hostname. A REALITY
// one is the site the handshake borrows, never the proxy's own host, so an
// entry without one stays without one and is refused as the Web UI refuses it.
// Web UI /xray keep s.Address and let Xray resolve, so a CDN IP change still
// works there.
//
// A server whose import stored its outbound gets the IP in the outbound's own
// address slot (vpnconfig.OutboundTarget). Where the source left the name to
// the address, dialing an IP would change it, so the hostname goes there
// instead: an empty tlsSettings.serverName, and for a stream without security
// an empty Host of ws or httpupgrade, an empty Host of the xhttpSettings or
// splithttpSettings the record has - Xray reads the former over the latter
// and drops the other - or an empty grpcSettings.authority, which a
// cleartext gRPC stream otherwise takes from the address. With TLS, Xray
// takes that Host, and gRPC's authority, from the server name.
//
// The download host of an xhttp extra (downloadSettings.address) keeps its
// name: the record's IPs are the main address's, and Xray resolves that host
// itself through the system resolver. So with the WAN resolver silent, a
// server whose download host is another name is judged dead although its main
// address resolved; looking that host up over the tunnel is a separate task.
func ServerForDial(s vpnconfig.Server) vpnconfig.Server {
	ip := ""
	for _, v := range s.IPs {
		if v != "" {
			ip = v
			break
		}
	}
	if ip == "" {
		return s
	}
	host := s.Address
	if len(s.Outbound) == 0 {
		s.Address = ip
		if s.SNI == "" && s.Security != "reality" {
			s.SNI = host
		}
		return s
	}
	ob, err := vpnconfig.DecodeOutbound(s.Outbound)
	if err != nil {
		return s
	}
	target := vpnconfig.OutboundTarget(ob)
	if target == nil {
		return s
	}
	target["address"] = ip
	if net.ParseIP(host) == nil {
		keepHostname(ob, host)
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return s
	}
	s.Outbound = raw
	s.Address = ip
	return s
}

// keepHostname writes host where the stream would otherwise take the name
// from an address that is now an IP. The xhttp Host goes into the
// xhttpSettings or splithttpSettings the record has: Xray reads xhttpSettings
// over splithttpSettings and drops the other, so a new xhttpSettings beside a
// splithttpSettings would dial without its path, mode and extra. A cleartext
// gRPC stream takes its :authority from the address when
// grpcSettings.authority is empty, so the hostname goes there.
func keepHostname(ob map[string]interface{}, host string) {
	ss, _ := ob["streamSettings"].(map[string]interface{})
	if ss == nil {
		return
	}
	switch security, _ := ss["security"].(string); security {
	case "tls":
		tls, _ := ss["tlsSettings"].(map[string]interface{})
		if tls == nil {
			tls = map[string]interface{}{}
			ss["tlsSettings"] = tls
		}
		if name, _ := tls["serverName"].(string); name == "" {
			tls["serverName"] = host
		}
	case "", "none":
		key := ""
		switch ss["network"] {
		case "ws", "websocket":
			key = "wsSettings"
		case "httpupgrade":
			key = "httpupgradeSettings"
		case "xhttp", "splithttp":
			key = "xhttpSettings"
			if _, ok := ss[key].(map[string]interface{}); !ok {
				if _, ok := ss["splithttpSettings"].(map[string]interface{}); ok {
					key = "splithttpSettings"
				}
			}
		case "grpc":
			grpc, _ := ss["grpcSettings"].(map[string]interface{})
			if grpc == nil {
				grpc = map[string]interface{}{}
				ss["grpcSettings"] = grpc
			}
			if authority, _ := grpc["authority"].(string); authority == "" {
				grpc["authority"] = host
			}
			return
		}
		if key == "" {
			return
		}
		transport, _ := ss[key].(map[string]interface{})
		if transport == nil {
			transport = map[string]interface{}{}
			ss[key] = transport
		}
		headers, _ := transport["headers"].(map[string]interface{})
		if h, _ := transport["host"].(string); h != "" {
			return
		}
		// Xray's ws builder takes a host header in any case.
		for key, value := range headers {
			if h, _ := value.(string); strings.EqualFold(key, "host") && h != "" {
				return
			}
		}
		transport["host"] = host
	}
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
//
// guard runs under the config lock in both writes, before either changes
// anything. The restore rests on a probe of one server, and a Web UI or /xray
// selection that committed after it - while a write waited for the lock, or
// between the two - put another server in its place, one still starting or
// dead. refused is the error with which the guard, or a stop, ended the
// restore; a stage it ended after is taken back for clients that had left Xray.
func (w *Watch) commitRestore(cfg *vpnconfig.VPNDirectorConfig, guard func(*vpnconfig.VPNDirectorConfig) error) (done, committed bool, refused error) {
	if w.stopped() {
		return false, false, nil
	}
	if failoverTunnel(cfg) == "" {
		if err := w.apply(); err != nil {
			slog.Warn("Apply after picking an Xray server failed", "error", err)
			return false, false, nil
		}
		w.settled()
		return true, false, nil
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
			return false, committed, nil
		}
		if err := w.apply(); err != nil {
			slog.Warn("Apply retry while TPROXY is not intercepting failed", "error", err)
			w.lastTPROXYFail = w.Now()
			return false, committed, nil
		}
		if !w.tproxyReady() {
			slog.Warn("TPROXY is not intercepting LAN; keeping the clients on the fallback tunnel")
			w.lastTPROXYFail = w.Now()
			return false, committed, nil
		}
		w.lastTPROXYFail = time.Time{}
	}
	if w.UpdateVPN != nil {
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			if guard != nil {
				if err := guard(current); err != nil {
					return err
				}
			}
			vpnconfig.EnsureFailoverStaged(current)
			return nil
		}); err != nil {
			if endsWalk(err) {
				return false, committed, err
			}
			slog.Warn("Failed to stage Xray clients for restore", "error", err)
			return false, committed, nil
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after staging Xray clients for restore failed", "error", err)
		w.pendingApply = true
		return false, committed, nil
	}
	if !w.tproxyReady() {
		slog.Warn("TPROXY is not intercepting LAN; keeping fallback routing")
		w.lastTPROXYFail = w.Now()
		if committed {
			w.unstageRestore()
		}
		return false, committed, nil
	}
	w.lastTPROXYFail = time.Time{}
	attempt := &restoreAttempt{committed: committed}
	if w.UpdateVPN != nil {
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			if guard != nil {
				if err := guard(current); err != nil {
					return err
				}
			}
			if current.Xray.Failover != nil {
				fo := *current.Xray.Failover
				attempt.removed = &fo
			}
			attempt.restored = vpnconfig.RestoreXrayClientsFromFailover(current)
			return nil
		}); err != nil {
			if endsWalk(err) {
				// The stage handed the clients to a server that is no longer
				// the one probed. Those that had left Xray leave it again, and
				// the next probe is of the server running now.
				if committed && !errors.Is(err, errStopped) {
					w.unstageRestore()
				}
				return false, committed, err
			}
			slog.Warn("Failed to restore Xray clients from the failover", "error", err)
			return false, committed, nil
		}
	}
	if err := w.apply(); err != nil {
		slog.Warn("Apply after dropping fallback membership failed", "error", err)
		w.pendingApply = true
		w.pendingRestore = attempt
		return false, committed, nil
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
		return false, committed, nil
	}
	w.pendingApply = false
	w.pendingRestore = nil
	w.settled()
	w.resetFallbackState()
	return true, committed, nil
}

// probedServer is the guard of a restore the tick's own probe decided. That
// probe tested the server active_server named when the tick read cfg; a Web UI
// or /xray selection since then put another in its place, one still starting
// or dead, and it is the next tick's to probe.
func probedServer(cfg *vpnconfig.VPNDirectorConfig) func(*vpnconfig.VPNDirectorConfig) error {
	var probed *vpnconfig.ActiveServer
	if cfg != nil {
		probed = cfg.Xray.ActiveServer
	}
	id, seq := activeID(probed), vpnconfig.ActiveSeq(probed)
	return func(current *vpnconfig.VPNDirectorConfig) error {
		if superseded(current, id, "", seq) {
			return errSuperseded
		}
		return nil
	}
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

// resetFail forgets a failing outbound: the three minutes - and the streak of
// unreachable checks that can shorten them - start again from its next miss.
func (w *Watch) resetFail() {
	w.failSince = time.Time{}
	w.downChecks = 0
}

// deadReason is why an outbound failing since failSince counts as dead at now,
// and "" while it does not yet. A server that every check since the first miss
// found down - two at least, each with the WAN reaching a control address -
// dies after FastDeadAfter; every other failure after DeadAfter.
func (w *Watch) deadReason(now time.Time) string {
	failing := now.Sub(w.failSince)
	switch {
	case failing >= FastDeadAfter && w.downChecks >= 2:
		return "unreachable"
	case failing >= DeadAfter:
		return "probe"
	}
	return ""
}

// settled is a working outbound: the three minutes start again from its next
// miss, the next death refreshes the subscription at once, and the first look
// for the preferred server waits ReturnCheck at least.
func (w *Watch) settled() {
	w.resetFail()
	w.lastImport = time.Time{}
	w.importRetry = 0
	// The preferred server failed minutes ago: the first look at it waits -
	// longer when the death counted as a failed return (returnAfterDeath).
	if hold := w.Now().Add(ReturnCheck); hold.After(w.returnNotBefore) {
		w.returnNotBefore = hold
	}
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
