package main

import (
	"context"
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

func newWatch(ctx context.Context, p paths.Paths, cfg *service.ConfigService, vpn *service.VPNDirectorService, xray *service.XrayService, q *notifications.Store, gate *watchcompat.Gate, health subwatch.HealthMonitor, wanUp func(context.Context) bool) *subwatch.Watch {
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
		if err := current.Err(); err != nil {
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
	w.Apply = func() error {
		return privateWatchError("VPN Director apply failed", vpn.ForContext(operationContext()).ApplyUnlessStopped())
	}
	w.RestartXray = func() error {
		return privateWatchError("Xray process restart failed", vpn.ForContext(operationContext()).RestartXrayProcessUnlessStopped())
	}
	w.Generate = func(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		current := operationContext()
		if err := current.Err(); err != nil {
			return false, 0, err
		}
		generator := watchXrayGenerator{GuardedXrayGenerator: xray.ForContext(current), config: cfg}
		return service.GenerateAndRecordGuardedWalkedServer(cfg, generator, endpoint.ServerForDial(s), s, service.InboundPorts{}, guard)
	}
	w.Fetch = func(ctx context.Context, url string) ([]vpnconfig.Server, error) {
		fetcher := service.SubscriptionFetcher{
			Store:      cfg,
			VPN:        vpn.ForContext(ctx),
			TablesPath: p.TunnelTables,
		}
		return fetcher.Fetch(ctx, url)
	}
	w.Notify = func(text string) {
		if operationContext().Err() != nil {
			return
		}
		if _, err := q.Publish(text); err != nil {
			slog.Warn("Notification storage publish failed; delivery will retry")
		}
	}
	return w
}

type watchXrayGenerator struct {
	service.GuardedXrayGenerator
	config *service.ConfigService
}

func (g watchXrayGenerator) GenerateConfigGuarded(s vpnconfig.Server, _ service.InboundPorts, guard func() error) error {
	// The guarded transaction holds the config lock here.
	config, err := g.config.LoadVPNConfig()
	if err != nil {
		return err
	}
	ports := service.InboundPorts{}
	ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(config)
	return g.GuardedXrayGenerator.GenerateConfigGuarded(s, ports, guard)
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
