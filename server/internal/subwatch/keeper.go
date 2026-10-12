package subwatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	msgFollowedMoved   = "Xray server %s moved; Xray follows it"
	msgFollowedChanged = "Xray server %s changed; Xray follows it"
	msgReplacedGone    = "Xray server %s left its subscription; Xray switched to %s"
	msgReplacedDead    = "Xray server %s is not answering; Xray switched to %s"
	msgNoWorking       = "No working Xray server found; Xray stays on %s"
	msgBackToGroup     = "Xray back on a %s server %s"
)

const (
	// KeeperRetry is the wait after a run of switches none of which worked,
	// doubling up to KeeperRetryMax: every candidate costs an Xray restart, and
	// back-to-back runs would restart Xray every tick.
	KeeperRetry    = 10 * time.Minute
	KeeperRetryMax = 30 * time.Minute
)

// keeps reports whether a tick of a watch that is not armed keeps Xray on a
// working server (spec 4.1): active_server names a server of a subscription,
// the watch can switch, and xray is installed. Nobody need be on Xray: a
// client back from a pause, or the bot's SOCKS path to Telegram, finds it
// working.
func (w *Watch) keeps(cfg *vpnconfig.VPNDirectorConfig, subs int) bool {
	if subs == 0 || cfg == nil || cfg.Xray.ActiveServer == nil || cfg.Xray.ActiveServer.Subscription == "" ||
		w.Generate == nil || w.Probe == nil {
		return false
	}
	return w.Health == nil || w.Health.Evidence().State != watchdapi.StateNoXray
}

// keep is a tick in keeper mode. There is nobody to move into a tunnel and no
// wave to run: the periodic refresh keeps the lists current, and keeps running
// while the probe fails (refreshStandsDown), so each attempt works from the
// lists as they are. A server that moved or changed is followed, and one that
// left its subscription or died is replaced, at once. A death is confirmed by
// the armed tick's rules (spec 4.4): the fast path's evidence, the unreachable
// streak, or DeadAfter.
func (w *Watch) keep(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
	if w.followActive(ctx, cfg) {
		return
	}
	w.setStatusAction("probe")
	err := w.Probe(ctx, w.socksPort(cfg))
	if w.mutationEnded(ctx) {
		return
	}
	if err == nil {
		w.probeOKAt = w.Now()
		w.probeOKActive = activeID(cfg.Xray.ActiveServer)
		w.resetFail()
		w.resetKeeper()
		w.lastRouteKind, w.lastImportKind = noteNone, noteNone
		w.maybeReturn(ctx, cfg)
		return
	}
	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
		w.failSeq = vpnconfig.ActiveSeq(cfg.Xray.ActiveServer)
	}
	if now.Before(w.keeperNotBefore) {
		return
	}
	attempt := w.fastFailover(ctx, cfg)
	switch attempt.Outcome {
	case fastSwitched:
		return
	case fastCanceled:
		w.resetFail()
		return
	case fastFallback:
		w.replaceDead(ctx, attempt.Config, "monitor", attempt.Guard)
		return
	}
	cfg = attempt.Config
	if now.Sub(w.failSince) < DeadAfter {
		w.checkReach(ctx, cfg)
	}
	if reason := w.deadReason(now); reason != "" {
		w.replaceDead(ctx, cfg, reason, attempt.Guard)
	}
}

// followActive is the actuality check (spec 4.2), made between episodes in
// both modes. A server that changed in place or moved is followed; one whose
// new entry does not answer either, and one that left its subscription, are
// replaced. The whole action has one fence, the record the check classified: a
// replacement after a follow that failed carries on from the follow's write,
// and a selection the user commits meanwhile ends it. It reports whether it
// acted - switched, tried and backed off, or was ended - and the tick then
// ends.
//
// During an episode it does nothing: while the probe fails, xray.failover or
// xray.pending_restore is recorded, or an apply of the watch's own is pending
// (spec 4.2). The episode's own logic owns the record then - the death rules
// and the walk pick the server, a restore finishes on the one its probe
// passed - and a switch of the check's own would cut across it.
func (w *Watch) followActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if cfg == nil || !w.failSince.IsZero() || w.pendingApply || cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil {
		return false
	}
	if cfg.Xray.ActiveServer == nil || w.Generate == nil || w.Probe == nil ||
		w.Now().Before(w.keeperNotBefore) || w.mutationEnded(ctx) {
		return false
	}
	subs, err := w.loadSubscriptions()
	if err != nil || len(subs) == 0 || w.mutationEnded(ctx) {
		return false
	}
	state, entry := classifyActive(subs, cfg.Xray.ActiveServer)
	if state == actualUnknown || state == actualCurrent {
		return false
	}
	a := cfg.Xray.ActiveServer
	was := label(subscriptionNames(subs), a.Subscription, a.Name)
	sw := w.newSwitcher(cfg, subs)
	if state == actualGone {
		slog.Info("The Xray server left its subscription; replacing it", "server", was)
		w.replaceActive(ctx, cfg, subs, sw, nil, msgReplacedGone, was)
		return true
	}
	msg := msgFollowedMoved
	if state == actualChanged {
		msg = msgFollowedChanged
	}
	slog.Info("The Xray server moved or changed; following it", "server", was, "changed", state == actualChanged)
	switched, ended := w.switchAmong(ctx, sw, endpoint.PerAddress([]vpnconfig.Server{entry}), func(vpnconfig.Server) string {
		return fmt.Sprintf(msg, was)
	})
	if switched || ended {
		return true
	}
	// The entry it moved to does not answer either: as far as Xray goes, the
	// server died. The follow's write is the action's own, and sw knows it.
	w.replaceActive(ctx, cfg, subs, sw, &entry, msgReplacedDead, was)
	return true
}

// replaceDead replaces the server active_server names once its death is
// confirmed: nobody to move, so no tunnel and no wave. The action's fence is
// cfg, the record the death was confirmed on - after the fast path, a read its
// ownership validated - and never a later read: a selection committed since
// refuses the first write. guard is the fast path's ownership when it got that
// far (fastAttempt.Guard), and the replacement writes under it as
// failOutbound's fallback does: the owner checks and records each write, its
// port snapshot stays guarded, and a refusal ends the action - nothing more is
// written, said or backed off.
func (w *Watch) replaceDead(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, reason string, guard func(*vpnconfig.VPNDirectorConfig) error) {
	if cfg == nil || cfg.Xray.ActiveServer == nil {
		return
	}
	previousGuard, previousRefused := w.outboundGuard, w.outboundRefused
	w.outboundGuard, w.outboundRefused = guard, nil
	defer func() { w.outboundGuard, w.outboundRefused = previousGuard, previousRefused }()
	if w.outboundGuard != nil && w.fastOwned != nil {
		// The replacement acts on the death the proof established, as the
		// armed walk does once a publication has replaced the proof's inputs:
		// ownership fences it from here, and each candidate's own probe proves
		// it. A proof kept on would refuse the switch at the monitor's next
		// check of a proof key.
		w.fastOwned.proofConsumed()
	}
	subs, err := w.loadSubscriptions()
	if err != nil || len(subs) == 0 || w.mutationEnded(ctx) || w.outboundAllowedNow() != nil {
		return
	}
	// The returns settle once per death, as failOutbound settles them.
	if !w.returnDeath.Equal(w.failSince) {
		w.returnDeath = w.failSince
		w.returnAfterDeath()
	}
	a := cfg.Xray.ActiveServer
	was := label(subscriptionNames(subs), a.Subscription, a.Name)
	slog.Info("Xray outbound declared dead; replacing the server", "server", was, "reason", reason)
	var dead *vpnconfig.Server
	if s, ok := exactFastServer(subs, a); ok {
		dead = &s
	}
	w.replaceActive(ctx, cfg, subs, w.newSwitcher(cfg, subs), dead, msgReplacedDead, was)
}

// replaceActive switches Xray to the best working server (spec 4.3) through
// sw, the action's fence. cfg is the record the action started from, and it
// orders the candidates too: a follow that failed recorded the same choice,
// which walkOrder still finds by its name, and the write after it keeps the
// record it left in preferred_server (RecordWalkedServer). It leaves out dead -
// every copy of it, and every copy that dials the same connection
// (endpoint.DialKey), which dies with it. It sends msg with was, the server
// replaced, and the server found. With none working it says so once a run and
// backs the next attempt off.
func (w *Watch) replaceActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, sw *switcher, dead *vpnconfig.Server, msg, was string) {
	if w.mutationEnded(ctx) {
		return
	}
	chosen := cfg.Xray.PreferredServer
	if chosen == nil {
		chosen = cfg.Xray.ActiveServer
	}
	order, _ := walkOrder(subs, chosen)
	deadKeys := map[string]bool{}
	if dead != nil {
		for _, c := range endpoint.PerAddress([]vpnconfig.Server{*dead}) {
			if key := endpoint.DialKey(c); key != "" {
				deadKeys[key] = true
			}
		}
	}
	var copies []vpnconfig.Server
	for _, c := range endpoint.PerAddress(order) {
		if dead != nil && serverID(c) == serverID(*dead) {
			continue
		}
		if key := endpoint.DialKey(c); key != "" && deadKeys[key] {
			continue
		}
		copies = append(copies, c)
	}
	names := subscriptionNames(subs)
	ref := refFor(cfg, vpnconfig.AllServers(subs))
	switched, ended := w.switchAmong(ctx, sw, w.bestWalkOrder(copies, cfg, ref), func(c vpnconfig.Server) string {
		return fmt.Sprintf(msg, was, label(names, c.Subscription, c.Name))
	})
	if switched || ended {
		return
	}
	w.keeperBackOff()
	if w.keeperFailNoted {
		return
	}
	w.keeperFailNoted = true
	// Read for the message alone: the server the action's last write left.
	stays := was
	if w.LoadVPN != nil {
		if latest, err := w.LoadVPN(); err == nil && latest != nil && latest.Xray.ActiveServer != nil {
			stays = label(names, latest.Xray.ActiveServer.Subscription, latest.Xray.ActiveServer.Name)
		}
	}
	slog.Warn("No working Xray server found", "stays", stays)
	w.say(fmt.Sprintf(msgNoWorking, stays))
}

// newSwitcher is the fence of one keeper action, set at cfg: the record the
// action starts from, and the links of subs it was read with. Every write of
// the action goes through it (switcher.to), so a selection the user commits
// after cfg was read refuses the next write and ends the action.
func (w *Watch) newSwitcher(cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription) *switcher {
	links := make(map[string]string, len(subs))
	for _, s := range subs {
		links[s.ID] = s.URL
	}
	before := cfg.Xray.ActiveServer
	return &switcher{w: w, links: links, started: activeID(before), seq: vpnconfig.ActiveSeq(before), socks: w.socksPort(cfg)}
}

// switchAmong switches Xray to the first of candidates the SOCKS probe finds
// working, through sw - the action's fence, carried from write to write - and
// sends message for it. It skips a copy whose endpoint.DialKey it tried
// already, and the copies of a subscription deleted meanwhile. ended is a
// stop, a closed gate, a selection the user made since the action began, or a
// fallback's ownership that refused (outboundAllowedNow), as failOutbound ends
// on it: nothing more may be written, said or backed off.
func (w *Watch) switchAmong(ctx context.Context, sw *switcher, candidates []vpnconfig.Server, message func(vpnconfig.Server) string) (switched, ended bool) {
	seen, gone := map[string]bool{}, map[string]bool{}
	w.setStatusAction("switch")
	for _, c := range candidates {
		if gone[c.Subscription] {
			continue
		}
		if key := endpoint.DialKey(c); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		live, stop, isGone := sw.to(ctx, c, true)
		if stop {
			return false, true
		}
		if isGone {
			gone[c.Subscription] = true
			continue
		}
		if !live {
			continue
		}
		// A stop or a selection can land while the probe waits; the walk makes
		// the same look before it announces.
		if w.mutationEnded(ctx) || w.endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq)) {
			return false, true
		}
		picked := c
		w.lastPicked = &picked
		w.settled()
		w.lastRouteKind, w.lastImportKind = noteNone, noteNone
		w.resetKeeper()
		slog.Info("Xray switched to a working server", "server", c.Name, "ips", c.IPs)
		w.say(message(c))
		return true, false
	}
	// A selection committed during the last probe ends the action as well, as
	// the walk looks before it reports no live server: the run did not fail
	// on the user's server. So does an ownership that stopped holding then.
	return false, w.mutationEnded(ctx) || w.outboundAllowedNow() != nil ||
		w.endsWalk(w.walkOwnsNow(sw.started, sw.lastRecorded, sw.seq))
}

// resetKeeper ends a run of keeper attempts: Xray works, and the next failure
// is a new run. Its first attempt is not held back, a run that finds nothing
// working backs off KeeperRetry again, and its "No working Xray server found"
// is news.
func (w *Watch) resetKeeper() {
	w.keeperRetry, w.keeperNotBefore, w.keeperFailNoted = 0, time.Time{}, false
}

// keeperBackOff holds the next attempt back after a run of switches none of
// which worked: KeeperRetry, then twice the last wait, KeeperRetryMax at most.
func (w *Watch) keeperBackOff() {
	if w.keeperRetry == 0 {
		w.keeperRetry = KeeperRetry
	} else {
		w.keeperRetry = min(2*w.keeperRetry, KeeperRetryMax)
	}
	w.keeperNotBefore = w.Now().Add(w.keeperRetry)
	slog.Info("Next attempt to find a working Xray server backed off", "after", w.keeperRetry)
}

// say sends msg as it stands. Each keeper switch is news: notify's
// one-per-kind bookkeeping would swallow the second of two switches alike.
// Like notify it lets go of mu for the send.
func (w *Watch) say(msg string) {
	if w.mutationAllowed() != nil || w.outboundAllowedNow() != nil || w.Notify == nil {
		return
	}
	n := w.Notify
	w.mu.Unlock()
	n(msg)
	w.mu.Lock()
}
