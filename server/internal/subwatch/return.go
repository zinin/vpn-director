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
	// ReturnHold is how long the preferred server has to keep working after a
	// return for the return to count as held: a death that starts within it
	// counts as a failed return, and only a held return ends the backoff.
	ReturnHold = 30 * time.Minute
	// ReturnFailsMax is how many returns in a row may fail - a death within
	// ReturnHold of one included - before the watch stops trying. An address
	// that accepts TCP and keeps refusing the proxy, a blocked endpoint, would
	// otherwise cost every Xray client a few dead seconds every ReturnRetryMax
	// for as long as the block lasts. A new death, a selection or a restart of
	// the bot starts the returns over.
	ReturnFailsMax = 4
)

// maybeReturn takes Xray back to the server the user chose once a walk has left
// another one running and the chosen one accepts TCP again - or, for a server
// no TCP dial can see, once its turn comes round. It runs after a probe that
// passed, with no failover and no restore apply outstanding, and looks every
// ReturnCheck - after a failed return, ReturnRetry and longer. After
// ReturnFailsMax failed attempts in a row the returns stop.
func (w *Watch) maybeReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if cfg == nil || cfg.Xray.PreferredServer == nil {
		// A return clears preferred_server too; its backoff ends once it has held.
		if w.lastReturn.IsZero() || w.Now().Sub(w.lastReturn) >= ReturnHold {
			w.returnRetry = 0
			w.returnFails = 0
			w.lastReturn = time.Time{}
		}
		return
	}
	if w.LoadSubscriptions == nil || w.Reachable == nil || w.Generate == nil {
		return
	}
	if w.returnFails >= ReturnFailsMax {
		return
	}
	if cfg.Xray.Failover != nil || w.pendingApply || w.pendingRestore != nil {
		return
	}
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	if active == nil || (active.Subscription == preferred.Subscription && active.Name == preferred.Name) {
		return
	}
	now := w.Now()
	if now.Before(w.returnNotBefore) {
		return
	}
	w.returnNotBefore = now.Add(ReturnCheck)
	subs, err := w.loadSubscriptions()
	if err != nil {
		slog.Warn("Failed to read the subscriptions for the return to the preferred server", "error", err)
		return
	}
	servers := vpnconfig.AllServers(subs)
	i := chosenIndex(servers, preferred)
	if i < 0 {
		return
	}
	// A server no TCP dial can see is returned to without one: the attempt
	// itself is then the only check there is, and a failed one backs the next
	// off as any other does.
	candidates := dialable(perAddress(servers[i : i+1]))
	if tcpChecked(servers[i]) {
		candidates = w.reachable(ctx, candidates)
	}
	if len(candidates) == 0 || ctx.Err() != nil || w.stopped() {
		return
	}
	w.tryReturn(ctx, cfg, subs, servers, candidates)
}

// tryReturn switches Xray to each candidate copy of the preferred server in
// turn and keeps the first the probe finds live. With none it switches back to
// the server that ran before - the address the walk picked first, when this
// process remembers it - and holds the next attempt back. That way back is
// known before the first switch, and an attempt without one is put off. Every
// write carries the walk's guard: a stop or a selection made meanwhile refuses
// it, and the attempt ends there. A switch to the preferred server is refused
// too once its subscription is deleted; nobody else wrote, so the attempt
// fails as a dead one does, and goes back to the server that ran before if it
// has left it.
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, servers, candidates []vpnconfig.Server) {
	before := cfg.Xray.ActiveServer
	links := make(map[string]string, len(subs))
	for _, s := range subs {
		links[s.ID] = s.URL
	}
	names := subscriptionNames(subs)
	sw := &switcher{
		w:       w,
		links:   links,
		started: activeID(before),
		seq:     vpnconfig.ActiveSeq(before),
		socks:   w.socksPort(cfg),
	}
	back := rollbackOrder(servers, before, w.lastPicked)
	if len(back) == 0 {
		slog.Info("Return to the preferred server put off; no way back to the server that runs now", "server", before.Name)
		return
	}
	slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)
	for _, c := range candidates {
		live, ended, gone := sw.to(ctx, c, true)
		if ended {
			return
		}
		if gone {
			slog.Info("Return to the preferred server abandoned; its subscription was deleted", "server", c.Name)
			break
		}
		if live {
			// A stop or a selection can land while the probe waits; the walk
			// makes the same look before it announces.
			if w.stopped() || endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq)) {
				return
			}
			slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
			w.lastPicked = &c
			w.lastReturn = w.Now()
			w.notify(noteReturned, fmt.Sprintf(msgReturned, label(names, c.Subscription, c.Name)))
			return
		}
	}
	w.backOffReturn()
	if !sw.wrote {
		return
	}
	for _, c := range back {
		live, ended, _ := sw.to(ctx, c, false)
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
	links        map[string]string // subscription id -> the link the attempt read
	started      string
	lastRecorded string
	seq          int
	socks        int
	wrote        bool // a config.json has been written
}

// to writes c as the running server, restarts Xray and probes it. With holds,
// c's subscription must still exist with the link the attempt read: gone is a
// switch the guard refused because that subscription was deleted meanwhile.
// Nothing was written for it, and nobody else wrote either, so the way back
// stays open - and is not held to that check itself: it is the server that
// ran. live is a probe that passed; ended is a write the guard refused for a
// stop or a newer selection, a restart a stop skipped, or a context or stop
// that ended the attempt, after which nothing more may be written.
func (s *switcher) to(ctx context.Context, c vpnconfig.Server, holds bool) (live, ended, gone bool) {
	w := s.w
	if ctx.Err() != nil || w.stopped() {
		return false, true, false
	}
	sub, link := "", ""
	if holds {
		sub, link = c.Subscription, s.links[c.Subscription]
	}
	generated, seq, err := w.Generate(c, w.walkGuard(sub, link, s.started, s.lastRecorded, s.seq))
	if endsWalk(err) {
		return false, true, false
	}
	if errors.Is(err, vpnconfig.ErrSubscriptionGone) {
		return false, false, true
	}
	if !generated {
		slog.Warn("Generating Xray config for server failed", "server", c.Name, "error", err)
		return false, false, false
	}
	s.wrote = true
	if err == nil {
		s.lastRecorded = serverID(c)
	}
	s.seq = seq
	if ctx.Err() != nil {
		return false, true, false
	}
	if err := w.restartXray(); err != nil {
		if errors.Is(err, errStopped) {
			return false, true, false
		}
		slog.Warn("Xray restart failed", "server", c.Name, "error", err)
		return false, false, false
	}
	w.AfterRestart(SettleAfterRestart)
	if err := w.Probe(ctx, s.socks); err != nil {
		// A probe a cancelled context or a stop cut short says nothing about
		// the server.
		if ctx.Err() != nil || w.stopped() {
			return false, true, false
		}
		slog.Info("Server probe failed", "server", c.Name, "ips", c.IPs, "error", err)
		return false, false, false
	}
	return true, false, false
}

// rollbackOrder is where a failed return goes back to: the server that ran
// before, before - its copy the walk picked or a return proved first, the
// address it ran on, when this process remembers it, then every other address
// its entry in its subscription's list names. The remembered copy leads even
// when a list imported since no longer has its address: Xray passed its probe
// on it. Empty when its subscription's list no longer has that server and
// nothing is remembered: there is no way back.
func rollbackOrder(servers []vpnconfig.Server, before *vpnconfig.ActiveServer, last *vpnconfig.Server) []vpnconfig.Server {
	var copies []vpnconfig.Server
	if j := chosenIndex(servers, before); j >= 0 {
		copies = perAddress(servers[j : j+1])
	}
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
	if !sameServer(*last, before) {
		return copies
	}
	out := []vpnconfig.Server{*last}
	for _, c := range copies {
		if dialIP(c) != dialIP(*last) {
			out = append(out, c)
		}
	}
	return out
}

// returnAfterDeath settles the returns at a death. A death is a new episode,
// and whatever the returns backed off to is done with - unless it started
// within ReturnHold of a return: the preferred server has failed again, and
// the death counts as a failed return, so a server that flaps is not
// returned to every few minutes.
func (w *Watch) returnAfterDeath() {
	if !w.lastReturn.IsZero() && w.failSince.Sub(w.lastReturn) < ReturnHold {
		slog.Info("The preferred server died soon after the return; counted as a failed return", "after", w.failSince.Sub(w.lastReturn))
		w.backOffReturn()
	} else {
		w.returnRetry = 0
		w.returnFails = 0
		w.returnNotBefore = time.Time{}
	}
	w.lastReturn = time.Time{}
}

// backOffReturn holds the next attempt back after a return that failed:
// ReturnRetry, then twice the last wait, ReturnRetryMax at most. After
// ReturnFailsMax failures in a row there is no next attempt: the returns stop.
func (w *Watch) backOffReturn() {
	w.returnFails++
	if w.returnRetry == 0 {
		w.returnRetry = ReturnRetry
	} else {
		w.returnRetry = min(2*w.returnRetry, ReturnRetryMax)
	}
	w.returnNotBefore = w.Now().Add(w.returnRetry)
	if w.returnFails >= ReturnFailsMax {
		slog.Info("Returns to the preferred server stopped after failed attempts; a new death, a selection or a bot restart starts them again", "failed", w.returnFails)
		return
	}
	slog.Info("Next return attempt backed off", "after", w.returnRetry)
}
