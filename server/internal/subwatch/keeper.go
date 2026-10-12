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
// lists as they are. Once the probe fails, a server that moved or changed is
// followed, and one that left its subscription is replaced, at once
// (followActive); one still listed is replaced once the armed tick's rules
// confirm its death (spec 4.4): the fast path's evidence, the unreachable
// streak, or DeadAfter.
func (w *Watch) keep(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) {
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
	if w.followActive(ctx, cfg) {
		return
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
		w.replaceDead(ctx, attempt.Config, cfg.Xray.ActiveServer, attempt.Tried, "monitor", attempt.Guard)
		return
	}
	// The death is decided on the record this tick read; an attempt that wrote
	// a candidate before it ended leaves attempt.Config naming that one.
	died := cfg.Xray.ActiveServer
	cfg = attempt.Config
	if now.Sub(w.failSince) < DeadAfter {
		w.checkReach(ctx, cfg)
	}
	if reason := w.deadReason(now); reason != "" {
		w.replaceDead(ctx, cfg, died, nil, reason, attempt.Guard)
	}
}

// followActive is the actuality check (spec 4.2, as owner decision C1 amends
// it): in both modes, a tick whose probe failed compares the selected server
// with its list before the death rules - on every such tick, so a server the
// periodic refresh moves while Xray is already failing is followed too. A
// server that changed in place or moved is followed; one whose new entry does
// not answer either, and one that left its subscription, are replaced. The
// whole action has one fence, the record the check classified: a replacement
// after a follow that failed carries on from the follow's write, and a
// selection the user commits meanwhile ends it. It reports whether it acted -
// switched, tried and backed off, or was ended - and the tick then ends.
//
// While the probe passes it does nothing: a working Xray is left alone. A
// panel that picks a host's address or SNI at random for each download -
// Marzban draws both from a list, or salts a '*' - reads as a server that
// moved or changed at every refresh, and acting on it would restart Xray, cut
// the open connections of every client on it and send a message each round.
// So would a refresh that does not follow renames, which reads a renamed
// server as gone, and the deletion of the running server's subscription,
// after which the Web UI promises that Xray keeps running it. A server that
// really moved stops answering at its old address, and the next probe brings
// the check in. Nor does it act while xray.failover or xray.pending_restore
// is recorded, or an apply of the watch's own is pending: that episode's own
// logic owns the record then - the walk picks the server, a restore finishes
// on the one its probe passed - and a switch of the check's own would cut
// across it.
func (w *Watch) followActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) bool {
	if cfg == nil || w.failSince.IsZero() || w.pendingApply || cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil {
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
		// A replacement that waits for the WAN does nothing: the tick goes
		// on to the death rules, which with clients on Xray may still move
		// them into a tunnel.
		if w.replacementWaits(ctx) {
			return false
		}
		slog.Info("The Xray server left its subscription; replacing it", "server", was)
		w.replaceActive(ctx, cfg, subs, sw, nil, nil, msgReplacedGone, was)
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
	if w.replacementWaits(ctx) {
		return true
	}
	w.replaceActive(ctx, cfg, subs, sw, &entry, nil, msgReplacedDead, was)
	return true
}

// replaceDead replaces died - the server active_server named when the tick's
// probe failed, the record its death was decided on - once that death is
// confirmed: nobody to move, so no tunnel and no wave. The message and the log
// name died, and the replacement leaves it out together with tried, the
// candidates the fast path is done with (fastAttempt.Tried). The action's
// fence is cfg: the tick's record, or after the fast path a read its ownership
// validated, which names the last candidate the fast path wrote, if it wrote
// one - and never a later read: a selection committed since refuses the first
// write. guard is the fast path's ownership when it got that far
// (fastAttempt.Guard), and the replacement writes under it as failOutbound's
// fallback does: the owner checks and records each write, its port snapshot
// stays guarded, and a refusal ends the action - nothing more is written, said
// or backed off. While the WAN is down it waits (replacementWaits), and the
// returns are not settled either: a WAN outage is no death of the server a
// return went back to.
func (w *Watch) replaceDead(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, died *vpnconfig.ActiveServer, tried []vpnconfig.Server, reason string, guard func(*vpnconfig.VPNDirectorConfig) error) {
	if cfg == nil || cfg.Xray.ActiveServer == nil || died == nil || w.replacementWaits(ctx) {
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
	was := label(subscriptionNames(subs), died.Subscription, died.Name)
	slog.Info("Xray outbound declared dead; replacing the server", "server", was, "reason", reason)
	var dead *vpnconfig.Server
	if s, ok := exactFastServer(subs, died); ok {
		dead = &s
	}
	w.replaceActive(ctx, cfg, subs, w.newSwitcher(cfg, subs), dead, tried, msgReplacedDead, was)
}

// replaceActive switches Xray to the best working server (spec 4.3) through
// sw, the action's fence. cfg is the record the action started from, and it
// orders the candidates too: a follow that failed recorded the same choice,
// which walkOrder still finds by its name, and the write after it keeps the
// record it left in preferred_server (RecordWalkedServer), as does a write of
// the fast path's. It leaves out dead - every copy of it, and every copy that
// dials the same connection (endpoint.DialKey), which dies with it - and each
// copy in tried with the copies that dial its connection: spec 4.5 leaves out
// the server that died and the servers already tried in the attempt. It sends
// msg with was, the server replaced, and the server found. With none working
// it says so once a run and backs the next attempt off. Every caller has asked
// replacementWaits first: with the WAN down none would work.
func (w *Watch) replaceActive(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig, subs []vpnconfig.Subscription, sw *switcher, dead *vpnconfig.Server, tried []vpnconfig.Server, msg, was string) {
	if w.mutationEnded(ctx) {
		return
	}
	chosen := cfg.Xray.PreferredServer
	if chosen == nil {
		chosen = cfg.Xray.ActiveServer
	}
	order, _ := walkOrder(subs, chosen)
	left := map[string]bool{}
	if dead != nil {
		for _, c := range endpoint.PerAddress([]vpnconfig.Server{*dead}) {
			left[leftKey(c)] = true
		}
	}
	for _, c := range tried {
		left[leftKey(c)] = true
	}
	var copies []vpnconfig.Server
	for _, c := range endpoint.PerAddress(order) {
		if (dead != nil && serverID(c) == serverID(*dead)) || left[leftKey(c)] {
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

// leftKey tells apart the copies a replacement leaves out: by what a copy
// dials (endpoint.DialKey), which every copy that dies with it shares, or -
// for a legacy record, which has no DialKey and is never taken for another -
// by the copy itself, its server and address. The NUL keeps the two kinds
// apart: a DialKey is an outbound's JSON.
func leftKey(c vpnconfig.Server) string {
	if key := endpoint.DialKey(c); key != "" {
		return key
	}
	return "\x00" + serverID(c) + "\x1f" + dialIP(c)
}

// replacementWaits reports whether a keeper replacement has to wait for the
// WAN (Ruling F-R1): with the WAN down every candidate fails its probe, each
// after an Xray restart, and the run ends in a "No working Xray server found"
// that blames the servers - the armed watch walks nothing when every download
// failed, for the same reason. Waiting switches nothing, backs nothing off and
// says nothing; the log says so once an episode, since the death rules come
// back here on every tick past DeadAfter. The fast path and the unreachable
// rule confirm a death only with the WAN working, and the check passes there.
func (w *Watch) replacementWaits(ctx context.Context) bool {
	if !w.wanDown(ctx) {
		return false
	}
	if !w.mutationEnded(ctx) && !w.keeperWANNoted.Equal(w.failSince) {
		w.keeperWANNoted = w.failSince
		slog.Info("The replacement of the Xray server waits for the WAN")
	}
	return true
}

// wanDown reports whether the WAN is down as far as the watch can tell:
// WANUp says so, or, without WANUp, no control address accepts a TCP
// connection (reachControls), as in the unreachable rule's look. With neither
// it cannot tell, and says it is not.
func (w *Watch) wanDown(ctx context.Context) bool {
	switch {
	case w.WANUp != nil:
		return !w.WANUp(ctx)
	case w.Reachable != nil:
		return len(w.reachable(ctx, reachControls)) == 0
	}
	return false
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
