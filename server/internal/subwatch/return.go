package subwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	// ReturnCheck is how often a healthy watch looks whether the server the
	// user chose, or a server of its group, can take Xray back while the
	// watch has another one running.
	ReturnCheck = 5 * time.Minute
	// ReturnRetry is the wait after a return whose probe failed - an address
	// can accept TCP and still refuse the proxy - doubling up to ReturnRetryMax.
	ReturnRetry    = 10 * time.Minute
	ReturnRetryMax = 30 * time.Minute
	// ReturnHold is how long the server a return went back to - the preferred
	// server, or a server of its group - has to keep working for the return to
	// count as held: a death that starts within it counts as a failed return,
	// and only a held return ends the backoff. A switch of the watch's own that
	// puts Xray back on the preferred server while returns to it have failed
	// counts as a return here (maybeReturn).
	ReturnHold = 30 * time.Minute
	// ReturnFailsMax is how many returns in a row may fail - a death within
	// ReturnHold of one included - before the watch stops trying. An address
	// that accepts TCP and keeps refusing the proxy, a blocked endpoint, would
	// otherwise cost every Xray client a few dead seconds every ReturnRetryMax
	// for as long as the block lasts. The count is the preferred server's: the
	// death of a server the watch switched to itself leaves it. A selection, a
	// held return or a restart of watchd starts the returns over.
	ReturnFailsMax = 4
	// ReturnAlive is how long the monitor has to hold a server alive before
	// Xray goes back to it - to the preferred server, or to the user's group:
	// a server that has just come back is often gone again within minutes.
	ReturnAlive = 5 * time.Minute
)

// maybeReturn takes Xray back to the server the user chose once the watch has
// left another one running and the chosen one is back: with fresh monitor
// evidence, once the monitor has held it alive for ReturnAlive; while the
// monitor is on and OK without evidence yet, at a later look; without the
// monitor, once it accepts TCP again - or, for a server no TCP dial can see,
// once its turn comes round. While it is not back and Xray runs a server of
// another group, it takes Xray to the best server of the user's group the
// monitor has held alive as long, and preferred_server stays. It runs after a
// probe that passed, with no failover and no restore apply outstanding, and
// looks every ReturnCheck - after a failed return, ReturnRetry and longer.
// After ReturnFailsMax failed attempts in a row the returns stop; a return of
// either kind that has held for ReturnHold ends that count and its wait. Xray
// back on the user's choice by a switch of the watch's own counts as a return
// while failed returns are counted.
func (w *Watch) maybeReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.mutationEnded(ctx) {
		return
	}
	// A return that has held for ReturnHold ends the backoff, whichever server
	// it went to: the preferred server, whose record cleared preferred_server,
	// or a server of its group, which kept it and the returns to it going.
	if !w.lastReturn.IsZero() && w.Now().Sub(w.lastReturn) >= ReturnHold {
		w.returnRetry = 0
		w.returnFails = 0
		w.lastReturn = time.Time{}
	}
	if cfg == nil || cfg.Xray.PreferredServer == nil {
		// Xray runs the user's choice, or nothing is left to return to. A switch
		// of the watch's own - the fast path, a replacement, the walk - can put
		// Xray back on the choice while returns to it have failed: that counts
		// as a return, and the log says so, so a choice that dies again soon
		// after is one more failed return, and one that holds for ReturnHold ends
		// the count above. A selection gets here with the count ended
		// (noteSelection), and with no count there is nothing to hold.
		if w.lastReturn.IsZero() && w.returnFails > 0 && cfg != nil && cfg.Xray.ActiveServer != nil {
			w.lastReturn = w.Now()
			w.returnSeq = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
			w.returnGroup = ""
			slog.Info("Xray is back on the preferred server without a return; counted as a return until it has held", "server", cfg.Xray.ActiveServer.Name, "failed", w.returnFails, "hold", ReturnHold)
		}
		return
	}
	if w.LoadSubscriptions == nil || w.Generate == nil {
		return
	}
	if w.returnFails >= ReturnFailsMax {
		return
	}
	if cfg.Xray.Failover != nil || w.pendingApply || w.pendingRestore != nil {
		return
	}
	active, preferred := cfg.Xray.ActiveServer, cfg.Xray.PreferredServer
	// A same-named server of another kind is not the preferred server
	// (vpnconfig.SameChoice): the return goes on looking.
	if active == nil || vpnconfig.SameChoiceRecord(preferred, active) {
		return
	}
	now := w.Now()
	if now.Before(w.returnNotBefore) {
		return
	}
	notBefore := w.returnNotBefore
	w.returnNotBefore = now.Add(ReturnCheck)
	subs, err := w.loadSubscriptions()
	if w.mutationEnded(ctx) {
		return
	}
	if err != nil {
		slog.Warn("Failed to read the subscriptions for the return to the preferred server", watchErrorAttr(err))
		return
	}
	servers := vpnconfig.AllServers(subs)
	// With fresh evidence the monitor's look is the look: alive for
	// ReturnAlive. While the monitor is on and OK without evidence for these
	// servers yet - a list publication gave it new endpoints or a new prober,
	// whose checks come in within half a minute - the look waits, and the next
	// tick looks again: the TCP look would skip ReturnAlive. Without the
	// monitor, the TCP look as before - a server no TCP dial can see is
	// switched to on its turn, the attempt the only check there is.
	ev, fresh, waiting := w.walkEvidence(servers, cfg)
	if waiting {
		w.returnNotBefore = notBefore
		slog.Debug("Return to the preferred server put off; the monitor has no evidence yet")
		return
	}
	var candidates []vpnconfig.Server
	if i := chosenIndex(servers, preferred); i >= 0 {
		copies := endpoint.PerAddress(servers[i : i+1])
		switch {
		case fresh:
			candidates = aliveFor(copies, ev, now, ReturnAlive)
		case w.Reachable == nil:
		case tcpChecked(servers[i]):
			candidates = w.reachable(ctx, dialable(copies))
		default:
			candidates = dialable(copies)
		}
	}
	if ctx.Err() != nil || w.mutationEnded(ctx) {
		return
	}
	if len(candidates) > 0 {
		w.tryReturn(ctx, cfg, subs, servers, candidates, "")
		return
	}
	// The preferred server is not back, but Xray may run another group than
	// the user's - preferred_server's, or its entry's for a record an older
	// writer left without one (spec 4.5): the best server of that group alive
	// for ReturnAlive is the nearer choice. preferred_server stays, and the
	// return to it still comes.
	ref := refFor(cfg, servers)
	activeGroup := active.Group
	if activeGroup == "" {
		if j := chosenIndex(servers, active); j >= 0 {
			activeGroup = servers[j].Group()
		}
	}
	if !fresh || ref.Group == "" || activeGroup == "" || activeGroup == ref.Group {
		return
	}
	// The group first, a server at a time: only its copies cost a key.
	var inGroup []vpnconfig.Server
	for _, s := range servers {
		if s.Group() == ref.Group {
			inGroup = append(inGroup, s)
		}
	}
	group := aliveFor(endpoint.PerAddress(inGroup), ev, now, ReturnAlive)
	if len(group) == 0 {
		return
	}
	// A look tries that one server (spec 4.6), its alive copies in the order
	// that chose it: each copy costs an Xray restart, and the other servers of
	// a group the probe refuses - twins on one endpoint among them - would cost
	// one each. Another server waits until a later look finds it the best.
	order := orderBest(group, ref, ev, true)
	var best []vpnconfig.Server
	for _, c := range order {
		if serverID(c) == serverID(order[0]) {
			best = append(best, c)
		}
	}
	w.tryReturn(ctx, cfg, subs, servers, best, ref.Group)
}

// aliveFor is the copies the monitor has held alive for at least d at now,
// fastest first, ties in the order given. Each copy's key, state and latency
// are read once, as orderBest reads them: a key costs a decode and a SHA-256
// of the outbound, and a comparator that took two keys a comparison spent
// 120 ms on 600 copies on a desktop CPU, many times that on a router's.
func aliveFor(copies []vpnconfig.Server, ev monitor.Evidence, now time.Time, d time.Duration) []vpnconfig.Server {
	type alive struct {
		s       vpnconfig.Server
		latency int64
	}
	var held []alive
	for _, c := range copies {
		st, ok := ev.Endpoints[endpoint.Key(c)]
		if ok && st.Status == watchdapi.StatusAlive && !st.Since.After(now.Add(-d)) {
			held = append(held, alive{s: c, latency: st.LatencyMS})
		}
	}
	sort.SliceStable(held, func(i, j int) bool { return held[i].latency < held[j].latency })
	out := make([]vpnconfig.Server, len(held))
	for i, a := range held {
		out[i] = a.s
	}
	return out
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
// has left it. With group set, the candidates are the copies of the best
// server of the user's group, not of the preferred server: the log and the
// message name the group, and preferred_server stays.
func (w *Watch) tryReturn(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, servers, candidates []vpnconfig.Server, group string) {
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
		if group == "" {
			slog.Info("Return to the preferred server put off; no way back to the server that runs now", "server", before.Name)
		} else {
			slog.Info("Return to a server of the preferred group put off; no way back to the server that runs now", "group", group, "server", before.Name)
		}
		return
	}
	w.setStatusAction("returning")
	if group == "" {
		slog.Info("Returning to the preferred server", "server", candidates[0].Name, "from", before.Name)
	} else {
		slog.Info("Returning to a server of the preferred group", "group", group, "server", candidates[0].Name, "from", before.Name)
	}
	for _, c := range candidates {
		live, ended, gone := sw.to(ctx, c, true)
		if ended {
			return
		}
		if gone {
			if group == "" {
				slog.Info("Return to the preferred server abandoned; its subscription was deleted", "server", c.Name)
			} else {
				slog.Info("Return to a server of the preferred group abandoned; its subscription was deleted", "group", group, "server", c.Name)
			}
			break
		}
		if live {
			// A stop or a selection can land while the probe waits; the walk
			// makes the same look before it announces.
			if w.mutationEnded(ctx) || w.endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq)) {
				return
			}
			w.lastPicked = &c
			w.lastReturn = w.Now()
			w.returnSeq = sw.seq
			w.returnGroup = group
			if group == "" {
				slog.Info("Xray returned to the preferred server", "server", c.Name, "ips", c.IPs)
				w.notify(noteReturned, fmt.Sprintf(msgReturned, label(names, c.Subscription, c.Name)))
			} else {
				slog.Info("Xray returned to a server of the preferred group", "group", group, "server", c.Name, "ips", c.IPs)
				w.say(fmt.Sprintf(msgBackToGroup, group, label(names, c.Subscription, c.Name)))
			}
			return
		}
	}
	if w.mutationEnded(ctx) {
		return
	}
	w.backOffReturn(group)
	if !sw.wrote {
		return
	}
	for _, c := range back {
		live, ended, _ := sw.to(ctx, c, false)
		if ended {
			return
		}
		if live {
			if group == "" {
				slog.Info("Return to the preferred server failed; back on the previous server", "server", c.Name, "ips", c.IPs)
			} else {
				slog.Info("Return to a server of the preferred group failed; back on the previous server", "group", group, "server", c.Name, "ips", c.IPs)
			}
			w.lastPicked = &c
			return
		}
	}
	if group == "" {
		slog.Warn("Return to the preferred server failed and the previous server did not come back", "previous", before.Name)
	} else {
		slog.Warn("Return to a server of the preferred group failed and the previous server did not come back", "group", group, "previous", before.Name)
	}
}

// switcher is one return attempt's run of switches, or one keeper action's. It
// carries the walk's bookkeeping from write to write - the record the last one
// left and the counter it moved to - which the guard of the next compares with
// the config.
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
// stop or a newer selection, a write or a restart a fallback's ownership
// refused, a restart a stop skipped, or a stop or a cancelled tick that ended
// the attempt, after which nothing more may be written. A timeout of the
// switch's own while the tick runs is a failed switch. It writes through
// generateWalked: under a fallback's ownership - the keeper's replacement
// after the fast path - the owner checks and records the write, as it does the
// walk's; without one that is Generate, and nothing is refused that way.
func (s *switcher) to(ctx context.Context, c vpnconfig.Server, holds bool) (live, ended, gone bool) {
	w := s.w
	if ctx.Err() != nil || w.mutationEnded(ctx) {
		return false, true, false
	}
	sub, link := "", ""
	if holds {
		sub, link = c.Subscription, s.links[c.Subscription]
	}
	generated, seq, err := w.generateWalked(c, w.walkGuard(sub, link, s.started, s.lastRecorded, s.seq, ctx))
	// Once a fallback's ownership refuses, nothing more may be written under
	// it: the attempt ends, as failOutbound ends on the same check.
	if w.endsWalk(err) || (err != nil && w.outboundAllowedNow() != nil) {
		return false, true, false
	}
	if errors.Is(err, vpnconfig.ErrSubscriptionGone) {
		return false, false, true
	}
	if !generated {
		slog.Warn("Generating Xray config for server failed", "server", c.Name, watchErrorAttr(err))
		return false, false, false
	}
	s.wrote = true
	if err == nil {
		s.lastRecorded = serverID(c)
	}
	s.seq = seq
	if w.mutationEnded(ctx) {
		return false, true, false
	}
	if err := w.restartXray(); err != nil {
		if w.endsWalk(err) || w.mutationEnded(ctx) || w.outboundAllowedNow() != nil {
			return false, true, false
		}
		slog.Warn("Xray restart failed", "server", c.Name, watchErrorAttr(err))
		return false, false, false
	}
	w.AfterRestart(SettleAfterRestart)
	if w.mutationEnded(ctx) {
		return false, true, false
	}
	err = w.Probe(ctx, s.socks)
	if w.mutationEnded(ctx) {
		return false, true, false
	}
	if err != nil {
		slog.Info("Server probe failed", "server", c.Name, "ips", c.IPs, watchErrorAttr(err))
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
		copies = endpoint.PerAddress(servers[j : j+1])
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

// returnAfterDeath settles the returns at a death. The count is the preferred
// server's: a death that starts within ReturnHold of a return, on the record
// that return wrote, is a failed return - the server it went back to, the
// preferred server or a server of its group, has failed again - so a server
// that flaps is not returned to every few minutes; the same record dying later
// held, and the returns start over. The death of any other record - one the
// watch switched to itself: a replacement's, the fast path's, the walk's, a
// follow's - says nothing about the preferred server and leaves the count and
// the wait as they are. A selection since the return wrote another record, so
// its death is not the return's either.
func (w *Watch) returnAfterDeath() {
	switch {
	case w.lastReturn.IsZero() || w.failSeq != w.returnSeq:
		// Not the record a return wrote: nothing about the preferred server.
	case w.failSince.Sub(w.lastReturn) < ReturnHold:
		if w.returnGroup == "" {
			slog.Info("The preferred server died soon after the return; counted as a failed return", "after", w.failSince.Sub(w.lastReturn))
		} else {
			slog.Info("The server of the preferred group died soon after the return; counted as a failed return", "group", w.returnGroup, "after", w.failSince.Sub(w.lastReturn))
		}
		w.backOffReturn(w.returnGroup)
	default:
		w.returnRetry = 0
		w.returnFails = 0
		w.returnNotBefore = time.Time{}
	}
	w.lastReturn = time.Time{}
}

// noteSelection starts the returns over at a record the watch did not write: a
// selection in the Web UI, /xray, the wizard or configure.sh - the same server
// selected again too, since every write moves the counter - or the record a
// previous process left, which the restart has started over anyway. What the
// returns counted is the old choice's, and the user's new choice starts with
// none: a selected server that never passes a probe would otherwise hand the
// old count on at its death. Once per record (selectedSeq), at the first tick
// that reads it, before the tick acts.
func (w *Watch) noteSelection(cfg *vpnconfig.VPNDirectorConfig) {
	seq := vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
	if seq == w.ownSeq || seq == w.selectedSeq {
		return
	}
	w.selectedSeq = seq
	w.returnFails, w.returnRetry = 0, 0
	w.returnNotBefore, w.lastReturn = time.Time{}, time.Time{}
}

// backOffReturn holds the next attempt back after a return that failed:
// ReturnRetry, then twice the last wait, ReturnRetryMax at most. After
// ReturnFailsMax failures in a row there is no next attempt: the returns stop,
// to the preferred server and to its group alike, and the log says so once. A
// death of the preferred server soon after Xray got back to it without a
// return still counts past the cap (maybeReturn), but it stops nothing new.
// group is the preferred group when the return that failed went to a server of
// it, and the log names it.
func (w *Watch) backOffReturn(group string) {
	w.returnFails++
	if w.returnRetry == 0 {
		w.returnRetry = ReturnRetry
	} else {
		w.returnRetry = min(2*w.returnRetry, ReturnRetryMax)
	}
	w.returnNotBefore = w.Now().Add(w.returnRetry)
	if w.returnFails >= ReturnFailsMax {
		if w.returnFails == ReturnFailsMax {
			if group == "" {
				slog.Info("Returns to the preferred server stopped after failed attempts; a selection, a watchd restart or Xray holding on it starts them again", "failed", w.returnFails, "hold", ReturnHold)
			} else {
				slog.Info("Returns to the preferred server and its group stopped after failed attempts; a selection, a watchd restart or Xray holding on the preferred server starts them again", "group", group, "failed", w.returnFails, "hold", ReturnHold)
			}
		}
		return
	}
	if group == "" {
		slog.Info("Next return attempt backed off", "after", w.returnRetry)
	} else {
		slog.Info("Next return attempt backed off after a failed return to a server of the preferred group", "group", group, "after", w.returnRetry)
	}
}
