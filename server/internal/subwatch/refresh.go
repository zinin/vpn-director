package subwatch

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

const (
	// RefreshFirst is how long after watchd starts its first periodic refresh
	// begins: at boot the WAN may not be up yet.
	RefreshFirst = time.Minute
	// RefreshRetry is how soon a round that stood down looks again, and how
	// often a refresh turned off looks whether it still is.
	RefreshRetry = time.Minute
)

var (
	// errEpisode stands the periodic refresh down while the watch handles an
	// Xray failure: the wave refreshes the subscriptions then.
	errEpisode = errors.New("the watch is handling an Xray failure")
	// errNoLinks stands it down when no subscription has a link.
	errNoLinks = errors.New("no subscription has a link")
	// errNoRefresh is a watch built without what the refresh needs.
	errNoRefresh = errors.New("the periodic refresh is not configured")
)

// listed is one subscription's download in a round of the periodic refresh.
type listed struct {
	servers []vpnconfig.Server
	err     error
}

// StartRefresh downloads every subscription with a link, one round every
// RefreshInterval, until ctx ends, and publishes what changed. It downloads
// outside the watch's tick, publishes between ticks, and stands down while
// VPN Director is stopped, the gate is closed or the watch handles an Xray
// failure. The first round starts RefreshFirst after it does; each next one
// RefreshInterval after the last one ended, or RefreshRetry after one that
// stood down.
func (w *Watch) StartRefresh(ctx context.Context) {
	wait := RefreshFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.refreshAfter(wait):
		}
		wait = RefreshRetry
		var interval time.Duration
		if w.RefreshInterval != nil {
			interval = w.RefreshInterval()
		}
		if interval > 0 && w.refreshRound(ctx) {
			wait = interval
		}
	}
}

func (w *Watch) refreshAfter(d time.Duration) <-chan time.Time {
	if w.after != nil {
		return w.after(d)
	}
	return time.After(d)
}

// refreshRound runs one round and reports whether it published; one that
// stood down, whose downloads a stop or the gate cut short, or that a
// shutdown cut short, returns false.
func (w *Watch) refreshRound(ctx context.Context) bool {
	subs, err := w.refreshTargets()
	if err != nil {
		slog.Debug("Periodic subscription refresh stood down", "reason", err.Error())
		return false
	}
	results, cut := w.downloadLists(ctx, subs)
	if ctx.Err() != nil {
		return false
	}
	if cut != nil {
		// A stop or the gate ended the downloads: what they returned is not the
		// subscriptions' state, and recording it would mark them failed - also
		// once the stop or the gate has cleared again before the publication.
		slog.Info("Periodic subscription refresh dropped its downloads", "reason", cut.Error())
		return false
	}
	// Publish between ticks: a walk or a return compares the server records
	// whole, and a rename under it would read as a new selection.
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.refreshStandsDown(); err != nil {
		slog.Info("Periodic subscription refresh dropped its downloads", "reason", err.Error())
		return false
	}
	for i, s := range subs {
		if ctx.Err() != nil {
			return false
		}
		w.publishList(ctx, s, results[i])
	}
	return true
}

// refreshTargets is every subscription with a link, read while no tick runs,
// or why the round stands down.
func (w *Watch) refreshTargets() ([]vpnconfig.Subscription, error) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.refreshStandsDown(); err != nil {
		return nil, err
	}
	all, err := w.loadSubscriptions()
	if err != nil {
		return nil, err
	}
	var linked []vpnconfig.Subscription
	for _, s := range all {
		if !s.Static() {
			linked = append(linked, s)
		}
	}
	if len(linked) == 0 {
		return nil, errNoLinks
	}
	return linked, nil
}

// refreshStandsDown is why the periodic refresh may not download or publish
// now, or nil. Its caller holds tickMu and mu, so no tick runs meanwhile.
func (w *Watch) refreshStandsDown() error {
	w.applyDefaults()
	if w.FetchList == nil || w.LoadVPN == nil || w.UpdateVPN == nil {
		return errNoRefresh
	}
	if err := w.mutationRefused(); err != nil {
		return err
	}
	if !w.failSince.IsZero() || w.pendingApply {
		return errEpisode
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return err
	}
	if cfg != nil && (cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil) {
		return errEpisode
	}
	return nil
}

// downloadLists downloads every subscription of subs at once, each within
// FetchTimeout. A stop or a closed gate ends the downloads within stopPoll,
// as cancelOnStop ends a tick's waits, and cut is then that refusal; it is
// nil when the downloads ran to their end or ctx ended them.
func (w *Watch) downloadLists(ctx context.Context, subs []vpnconfig.Subscription) (results []listed, cut error) {
	ctx, end := w.refreshScope(ctx)
	results = make([]listed, len(subs))
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
			defer cancel()
			servers, err := w.FetchList(fetchCtx, s.URL)
			results[i] = listed{servers, err}
		}()
	}
	wg.Wait()
	return results, end()
}

// refreshScope is ctx, ended once a stop or the gate forbids mutation. end
// ends it too, joins its poller and returns the refusal that ended it, or nil
// when none did: the downloads ran to their end, or ctx ended first.
func (w *Watch) refreshScope(ctx context.Context) (context.Context, func() error) {
	ctx, cancel := context.WithCancelCause(ctx)
	if w.Stopped == nil && w.CanMutate == nil {
		return ctx, func() error {
			cancel(nil)
			return nil
		}
	}
	var refused error
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
				if ctx.Err() != nil {
					return
				}
				err := w.mutationRefused()
				// A context error is no refusal: watchd's CanMutate answers for
				// the running tick (Context), and a tick that ends is neither a
				// stop nor a closed gate. ctx itself carries the daemon's
				// shutdown, and the re-check under tickMu, with no tick
				// running, still catches a stop or a closed gate before
				// anything is written.
				if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				// A refusal that comes once the scope has ended - the downloads
				// are over, or ctx is - cut nothing short.
				if ctx.Err() != nil {
					return
				}
				refused = err
				cancel(err)
				return
			}
		}
	}()
	return ctx, func() error {
		cancel(nil)
		// The poller writes refused before it closes ended.
		<-ended
		return refused
	}
}

// publishList publishes one subscription's download, or records why it did
// not arrive (vpnconfig.RecordSubscriptionError writes an error once). Its
// caller holds tickMu and mu.
func (w *Watch) publishList(ctx context.Context, s vpnconfig.Subscription, l listed) {
	update, files := w.updateFor(ctx), w.files(ctx)
	err := l.err
	if err == nil {
		res, perr := vpnconfig.PublishRefresh(update, files, s.ID, s.URL, l.servers, w.Now())
		switch {
		case perr == nil:
			w.followActiveRename(s.ID, res.Followed)
			logPublished(s, res)
			return
		case errors.Is(perr, vpnconfig.ErrNoServerResolved):
			err = perr
		case errors.Is(perr, vpnconfig.ErrSubscriptionGone):
			slog.Info("Periodic refresh dropped; the subscription was deleted or relinked while it downloaded", "subscription", s.Name)
			return
		case errors.Is(perr, errStopped), errors.Is(perr, watchcompat.ErrIncompatible), ctx.Err() != nil:
			return
		default:
			slog.Warn("Failed to publish the periodically refreshed subscription", "subscription", s.Name, watchErrorAttr(perr))
			return
		}
	}
	// A *url.Error carries the whole subscription URL, token included.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if msg != s.Error {
		slog.Warn("Periodic subscription refresh failed", "subscription", s.Name, watchErrorAttr(err))
	}
	rerr := vpnconfig.RecordSubscriptionError(update, files, s.ID, s.URL, s.Refreshed, msg)
	// watchd's shutdown can end the record while it waits for the config lock,
	// as it ends a publication: that is no failure to record.
	if rerr != nil && !errors.Is(rerr, errStopped) && !errors.Is(rerr, watchcompat.ErrIncompatible) &&
		!errors.Is(rerr, vpnconfig.ErrSubscriptionGone) && ctx.Err() == nil {
		slog.Warn("Failed to record why the subscription did not refresh", "subscription", s.Name, watchErrorAttr(rerr))
	}
}

// followActiveRename moves what the watch remembers of the active server to
// the new name when a publication of subscription id renamed the server along
// with its active_server record. Left on the old name, the record the main
// probe last passed on (probeOKActive) no longer matches active_server, and
// the fast path does not know the server until the next probe passes; the copy
// the walk picked or a return proved (lastPicked) no longer leads the way back
// of a failed return. A panel that puts the traffic left into every name
// renames at nearly every round. lastPicked moves only when it names that
// server, as rollbackOrder matches it with the record; address and port stay,
// as the record's do. Its caller holds tickMu and mu, as every tick that reads
// them does.
func (w *Watch) followActiveRename(id string, followed []vpnconfig.RecordRename) {
	for _, f := range followed {
		if f.Record != "active_server" {
			continue
		}
		before := vpnconfig.ActiveServer{Subscription: id, Name: f.From, Address: f.Address, Port: f.Port}
		after := before
		after.Name = f.To
		if w.probeOKActive == activeID(&before) {
			w.probeOKActive = activeID(&after)
		}
		if p := w.lastPicked; p != nil && sameServer(*p, &before) {
			renamed := *p
			renamed.Name = f.To
			w.lastPicked = &renamed
		}
	}
}

// logPublished says what a published round did to subscription s. No link:
// names and counts only. The list is news at INFO when the round added,
// removed or readdressed a server: the monitor checks other addresses then. A
// round that only renamed or reordered the servers, or only cleared an error,
// logs it at DEBUG, and so does every record that followed a rename. A panel
// that puts the traffic left into every server name renames them at nearly
// every round, and watchd's log is cut at 200 KB: at INFO those lines would
// take the failover history with them.
func logPublished(s vpnconfig.Subscription, res vpnconfig.RefreshResult) {
	if !res.Wrote {
		slog.Debug("Periodic refresh found the subscription unchanged", "subscription", s.Name, "servers", res.Count)
		return
	}
	if s.Error != "" {
		slog.Info("Subscription downloads again", "subscription", s.Name)
	}
	level := slog.LevelDebug
	if res.Added > 0 || res.Removed > 0 || res.Readdressed > 0 {
		level = slog.LevelInfo
	}
	slog.Log(context.Background(), level, "Subscription list published", "subscription", s.Name, "servers", res.Count,
		"added", res.Added, "removed", res.Removed, "renamed", res.Renamed, "readdressed", res.Readdressed)
	for _, f := range res.Followed {
		slog.Debug("Server record follows its renamed server", "record", f.Record, "from", f.From, "to", f.To)
	}
}
