package subwatch

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type watchStatus struct {
	context  context.Context
	snapshot watchdapi.WatchSnapshot
}

// Snapshot never waits for the Tick's probes, shell commands or config lock.
func (w *Watch) Snapshot() watchdapi.WatchSnapshot {
	w.statusMu.RLock()
	snapshot := w.status.snapshot
	w.statusMu.RUnlock()
	if snapshot.State == "" {
		snapshot.State = watchdapi.WatchStarting
	}
	return snapshot
}

// Context is the current Tick's cancellation scope, or nil outside a Tick.
func (w *Watch) Context() context.Context {
	w.statusMu.RLock()
	defer w.statusMu.RUnlock()
	return w.status.context
}

func (w *Watch) setContext(ctx context.Context) {
	w.statusMu.Lock()
	w.status.context = ctx
	w.statusMu.Unlock()
}

func (w *Watch) setStatus(err error) {
	state, message := watchdapi.WatchActive, ""
	switch {
	case errors.Is(err, errStopped):
		state, message = watchdapi.WatchStopped, "VPN Director is stopped"
	case errors.Is(err, watchcompat.ErrIncompatible):
		state, message = watchdapi.WatchIncompatible, "Bot compatibility is unconfirmed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		state, message = watchdapi.WatchNotRunning, "Subscription automation is not running"
	case err != nil:
		state, message = watchdapi.WatchError, "Subscription automation configuration is unavailable"
	}
	now := w.Now()
	w.statusMu.Lock()
	w.status.snapshot.State = state
	w.status.snapshot.Message = message
	w.status.snapshot.UpdatedAt = now
	w.statusMu.Unlock()
}

func statusFlags(cfg *vpnconfig.VPNDirectorConfig) (bool, bool) {
	return vpnconfig.FailoverCommitted(cfg), cfg != nil && cfg.Xray.PendingRestore != nil
}

func (w *Watch) setStatusConfig(cfg *vpnconfig.VPNDirectorConfig) {
	committed, pending := statusFlags(cfg)
	w.setStatusFlags(committed, pending)
}

func (w *Watch) setStatusFlags(committed, pending bool) {
	w.statusMu.Lock()
	w.status.snapshot.CommittedFailover = committed
	w.status.snapshot.PendingRestore = pending
	w.statusMu.Unlock()
}

func (w *Watch) setStatusAction(action string) {
	switch action {
	case "probe", "confirm":
		action = "checking"
	case "switch":
		action = "switching"
	case "restore":
		action = "restoring"
	case "", "checking", "switching", "fallback", "refreshing", "walking", "returning", "restoring":
	default:
		action = ""
	}
	w.statusMu.Lock()
	w.status.snapshot.Action = action
	w.statusMu.Unlock()
}

// Provider and transport errors can embed subscription URLs or credentials.
func watchErrorAttr(err error) slog.Attr {
	kind := "operation"
	switch {
	case err == nil:
		kind = "unavailable"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, errStopped):
		kind = "stopped"
	case errors.Is(err, watchcompat.ErrIncompatible):
		kind = "incompatible"
	case errors.Is(err, errSuperseded):
		kind = "superseded"
	case errors.Is(err, vpnconfig.ErrSubscriptionGone):
		kind = "subscription_gone"
	}
	return slog.Group("error", "kind", kind)
}
