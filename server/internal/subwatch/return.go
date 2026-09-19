package subwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	// ReturnCheck is how often a healthy watch looks whether the server the
	// user chose accepts TCP again while a walk has another one running.
	ReturnCheck = 5 * time.Minute
	// ReturnRetry is the wait after a return whose probe failed - an address
	// can accept TCP and still refuse the proxy - doubling up to ReturnRetryMax.
	ReturnRetry    = 10 * time.Minute
	ReturnRetryMax = 30 * time.Minute
)

// maybeReturn takes Xray back to the server the user chose once a walk has left
// another one running and the chosen one accepts TCP again. It runs after a
// probe that passed, with no failover and no restore apply outstanding, and
// looks every ReturnCheck - after a failed return, ReturnRetry and longer.
func (w *Watch) maybeReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.PreferredServer == nil {
		w.returnRetry = 0
		return
	}
	if w.LoadServers == nil || w.Reachable == nil || w.Generate == nil {
		return
	}
	if cfg.Xray.Failover != nil || w.pendingApply || w.pendingRestore != nil {
		return
	}
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	if active == nil || active.Name == preferred.Name {
		return
	}
	now := w.Now()
	if now.Before(w.returnNotBefore) {
		return
	}
	w.returnNotBefore = now.Add(ReturnCheck)
	servers, err := w.LoadServers()
	if err != nil {
		slog.Warn("Failed to load servers.json for the return to the preferred server", "error", err)
		return
	}
	i := chosenIndex(servers, preferred)
	if i < 0 {
		return
	}
	candidates := w.reachable(ctx, dialable(perAddress(servers[i:i+1])))
	if len(candidates) == 0 || ctx.Err() != nil || w.stopped() {
		return
	}
	w.tryReturn(ctx, cfg, servers, candidates)
}

// tryReturn switches Xray to each reachable copy of the preferred server in
// turn and keeps the first the probe finds live. With none it switches back to
// the server that ran before - the address the walk picked first, when this
// process remembers it - and holds the next attempt back. Every write carries
// the walk's guard: a stop, a newly saved link or a selection made meanwhile
// refuses it, and the attempt ends there.
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, servers, candidates []vpnconfig.Server) {
	before := cfg.Xray.ActiveServer
	sw := &switcher{
		w:       w,
		rawURL:  cfg.Xray.SubscriptionURL,
		started: activeID(before),
		seq:     vpnconfig.ActiveSeq(before),
		socks:   w.socksPort(cfg),
	}
	slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)
	for _, c := range candidates {
		live, ended := sw.to(ctx, c)
		if ended {
			return
		}
		if live {
			// A stop or a selection can land while the probe waits; the walk
			// makes the same look before it announces.
			if w.stopped() || endsWalk(w.walkOwnsNow(sw.rawURL, sw.started, sw.lastRecorded, sw.seq)) {
				return
			}
			slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			w.returnRetry = 0
			w.notify(noteReturned, fmt.Sprintf(msgReturned, c.Name))
			return
		}
	}
	w.backOffReturn()
	if !sw.wrote {
		return
	}
	j := chosenIndex(servers, before)
	if j < 0 {
		slog.Warn("Return to the preferred server failed and the previous server is no longer listed", "previous", before.Name)
		return
	}
	for _, c := range rollbackOrder(perAddress(servers[j:j+1]), w.lastPicked) {
		live, ended := sw.to(ctx, c)
		if ended {
			return
		}
		if live {
			slog.Info("Return to the preferred server failed; back on the previous server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			return
		}
	}
	slog.Warn("Return to the preferred server failed and the previous server did not come back", "previous", before.Name)
}

// switcher is one return attempt's run of switches. It carries the walk's
// bookkeeping from write to write - the record the last one left and the
// counter it moved to - which the guard of the next compares with the config.
type switcher struct {
	w            *Watch
	rawURL       string
	started      string
	lastRecorded string
	seq          int
	socks        int
	wrote        bool // a config.json has been written
}

// to writes c as the running server, restarts Xray and probes it. live is a
// probe that passed; ended is a write the guard refused, a restart a stop
// skipped, or a context or stop that ended the attempt, after which nothing
// more may be written.
func (s *switcher) to(ctx context.Context, c vpnconfig.Server) (live, ended bool) {
	w := s.w
	if ctx.Err() != nil || w.stopped() {
		return false, true
	}
	generated, seq, err := w.Generate(c, w.walkGuard(s.rawURL, s.started, s.lastRecorded, s.seq))
	if endsWalk(err) {
		return false, true
	}
	if !generated {
		slog.Warn("Generating Xray config for server failed", "server", c.Name, "error", err)
		return false, false
	}
	s.wrote = true
	if err == nil {
		s.lastRecorded = serverID(c)
	}
	s.seq = seq
	if ctx.Err() != nil {
		return false, true
	}
	if err := w.restartXray(); err != nil {
		if errors.Is(err, errStopped) {
			return false, true
		}
		slog.Warn("Xray restart failed", "server", c.Name, "error", err)
		return false, false
	}
	w.AfterRestart(SettleAfterRestart)
	if err := w.Probe(ctx, s.socks); err != nil {
		// A probe a cancelled context or a stop cut short says nothing about
		// the server.
		if ctx.Err() != nil || w.stopped() {
			return false, true
		}
		slog.Info("Server probe failed", "server", c.Name, "ips", c.IPs, "error", err)
		return false, false
	}
	return true, false
}

// rollbackOrder is the copies of the server that ran before, with the copy the
// walk picked or a return proved first: the address it ran on, when this
// process remembers it.
func rollbackOrder(copies []vpnconfig.Server, last *vpnconfig.Server) []vpnconfig.Server {
	if last == nil {
		return copies
	}
	for i, c := range copies {
		if serverID(c) != serverID(*last) || dialIP(c) != dialIP(*last) {
			continue
		}
		out := make([]vpnconfig.Server, 0, len(copies))
		out = append(out, c)
		out = append(out, copies[:i]...)
		return append(out, copies[i+1:]...)
	}
	return copies
}

// backOffReturn holds the next attempt back after a return that failed:
// ReturnRetry, then twice the last wait, ReturnRetryMax at most.
func (w *Watch) backOffReturn() {
	if w.returnRetry == 0 {
		w.returnRetry = ReturnRetry
	} else {
		w.returnRetry = min(2*w.returnRetry, ReturnRetryMax)
	}
	w.returnNotBefore = w.Now().Add(w.returnRetry)
	slog.Info("Next return attempt backed off", "after", w.returnRetry)
}
