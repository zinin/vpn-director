package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

func newWatch(ctx context.Context, p paths.Paths, cfg *service.ConfigService, vpn, mutating *service.VPNDirectorService, xray *service.XrayService, q *notifications.Store, gate *watchcompat.Gate, health subwatch.HealthMonitor, wanUp func(context.Context) bool) *subwatch.Watch {
	readiness := netpath.Readiness{
		TablesPath:   p.TunnelTables,
		FailoverPath: p.FailoverReady,
		TPROXYPath:   p.TPROXYReady,
		StoppedPath:  p.StoppedMarker,
	}
	w := &subwatch.Watch{
		LoadVPN:           cfg.LoadVPNConfig,
		UpdateVPN:         cfg.UpdateVPNConfig,
		LoadSubscriptions: cfg.LoadSubscriptions,
		SaveSubscription:  cfg.SaveSubscription,
		Reachable:         netpath.ReachTCP4(nil),
		Health:            health,
		WANUp:             wanUp,
		FallbackReady:     readiness.FallbackReady,
		TPROXYReady:       readiness.TPROXYReady,
		Stopped:           readiness.Stopped,
	}
	operationContext := func() context.Context {
		if current := w.Context(); current != nil {
			return current
		}
		return ctx
	}
	w.CanMutate = func() error {
		current := operationContext()
		// The cause, not the bare cancellation: a stop poll that found the gate
		// closed ends the operation with ErrIncompatible, and the log says so.
		if err := context.Cause(current); err != nil {
			return err
		}
		if gate == nil {
			return watchcompat.ErrIncompatible
		}
		return gate.Check(current)
	}
	w.LoadPlatform = func() (vpnconfig.PlatformInfo, error) {
		info, err := vpn.ForContext(operationContext()).Platform()
		return info, privateWatchError("Platform lookup failed", err)
	}
	// An automatic apply or Xray process restart runs to its end, bounded by
	// ApplyTimeout, whatever ends the tick or the daemon: cancelling it would
	// signal the script's whole process group, a main Xray just started there
	// included, and leave routing half-changed. mutating is not scoped to ctx.
	w.Apply = func() error {
		return privateWatchError("VPN Director apply failed", mutating.ApplyUnlessStopped())
	}
	w.RestartXray = func() error {
		return privateWatchError("Xray process restart failed", mutating.RestartXrayProcessUnlessStopped())
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		current := operationContext()
		if err := current.Err(); err != nil {
			return false, 0, err
		}
		ports, bound := w.BoundGenerationPorts()
		if !bound {
			// Legacy consumers expect ports read before the config-lock wait.
			config, err := cfg.LoadVPNConfig()
			if err != nil {
				return false, 0, err
			}
			ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(config)
		}
		return service.GenerateAndRecordGuardedWalkedServer(cfg, xray.ForContext(current), endpoint.ServerForDial(s), s, ports, guard)
	}
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		fetcher := service.SubscriptionFetcher{
			Store:      cfg,
			VPN:        vpn.ForContext(ctx),
			TablesPath: p.TunnelTables,
		}
		return fetcher.Fetch(ctx, url)
	}
	// The watch notifies a change it has made and records the message as sent;
	// its own checks decide what a stop silences, so an ended tick drops nothing.
	w.Notify = func(text string) {
		if _, err := q.Publish(text); errors.Is(err, notifications.ErrDeferred) {
			slog.Warn("Notification storage cannot save yet; the event waits in memory until it can")
		} else if err != nil {
			slog.Warn("Notification storage publish failed; delivery will retry")
		}
	}
	return w
}

type watchOperationError struct {
	message string
	cause   error
}

func (e *watchOperationError) Error() string { return e.message }
func (e *watchOperationError) Unwrap() error { return e.cause }

func privateWatchError(message string, err error) error {
	if err == nil {
		return nil
	}
	return &watchOperationError{message: message, cause: err}
}
