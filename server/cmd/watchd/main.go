// vpn-director-watchd checks, minute by minute, whether every server of every
// subscription carries traffic, and serves what it finds on a unix socket to
// the Web UI and the bot (internal/monitor, internal/watchdapi).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/devmode"
	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/logging"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/platform"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updater"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func main() {
	// Step 2 of a self-update (internal/updater/selfupdate.go): every binary
	// of the daemon table implements it, and nothing a daemon does at startup
	// may run first.
	if len(os.Args) > 1 && os.Args[1] == updater.SelfUpdateCommand {
		os.Exit(updater.RunSelfUpdate(os.Args[2:], updater.DaemonWatchd, Version, os.Stdout, os.Stderr))
	}
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "/opt/vpn-director/vpn-director.json", "path to vpn-director.json")
	devFlag := flag.Bool("dev", false, "run in development mode (testdata paths, a fake prober)")
	platformFlag := flag.String("platform", "", "platform this router runs (merlin|keenetic); detected when empty")
	flag.Parse()

	plat, err := platform.Resolve(*platformFlag, *devFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if err := plat.Export(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	var p paths.Paths
	var detachErr error
	if *devFlag {
		p = paths.DevPaths()
		if *configPath == "/opt/vpn-director/vpn-director.json" {
			*configPath = p.ScriptsDir + "/vpn-director.json"
		}
		if _, err := os.Stat(p.ScriptsDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: %s not found\n", p.ScriptsDir)
			fmt.Fprintf(os.Stderr, "Run from server/ directory: cd server && go run ./cmd/watchd --dev\n")
			return 1
		}
	} else {
		p = paths.Default()
		// The update script starts this daemon from a directory the bot
		// deletes moments later (paths.DetachFromCallerDirectory).
		detachErr = paths.DetachFromCallerDirectory(configPath)
	}

	slogger, logger, err := logging.NewSlogLogger(p.WatchdLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		return 1
	}
	defer logger.Close()
	slog.SetDefault(slogger)
	slog.Info("starting vpn-director-watchd", "version", Version, "commit", Commit, "dev", *devFlag)
	if detachErr != nil {
		slog.Warn("could not leave the directory this process was started in", "error", detachErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runRuntime(ctx, p.WatchdSocket, func() (runtimeDeps, error) {
		logger.StartRotation(ctx, p.RotatedLogs(), logging.DefaultMaxSize, time.Minute)
		scriptsDir := filepath.Dir(*configPath)
		configSvc := service.NewConfigService(scriptsDir, filepath.Join(scriptsDir, "data"), *configPath)
		queuePath := ""
		dataDir, pathErr := configSvc.DataDir()
		if pathErr == nil && dataDir != "" {
			queuePath = filepath.Join(dataDir, "watchd-notifications.json")
		} else {
			pathErr = errors.New("notification data path is unavailable")
		}
		queue, storageErr := notifications.NewStore(queuePath, nil)
		var executor service.ShellExecutor = service.DefaultExecutor()
		var launcher monitor.Launcher
		var gate *watchcompat.Gate
		if *devFlag {
			executor = devmode.NewExecutor()
			launcher = monitor.FakeLauncher{}
		} else {
			gate = &watchcompat.Gate{BotPath: p.BotBinary}
			monitor.KillLeftovers(p.ProbeBinary)
			launcher = &monitor.XrayLauncher{ProbeBinary: p.ProbeBinary, ConfigDir: p.ProbeDir}
		}
		vpnSvc := service.NewVPNDirectorService(p.ScriptsDir, service.WithContext(ctx, executor))
		xraySvc := service.NewXrayServiceForContext(ctx, p.XrayTemplate, p.XrayConfig)
		watch := newWatch(ctx, p, configSvc, vpnSvc, xraySvc, queue, gate)
		if *devFlag {
			// Dev has no installed or running router bot to attest.
			watch.CanMutate = func() error {
				if current := watch.Context(); current != nil {
					return current.Err()
				}
				return ctx.Err()
			}
		}
		readiness := netpath.Readiness{StoppedPath: p.StoppedMarker}
		m := monitor.New(monitor.Deps{
			Settings:  settingsReader(configSvc),
			Endpoints: endpointsReader(configSvc),
			Launcher:  launcher,
			Stopped:   readiness.Stopped,
			WANUp: func(ctx context.Context) bool {
				return monitor.WANUp(ctx, endpoint.WANControls, 3*time.Second)
			},
			StatePath:  p.WatchdState,
			OnSettings: levelSetter(logger),
		})
		return runtimeDeps{Monitor: m, Watch: watch, Queue: queue}, errors.Join(pathErr, storageErr)
	}); err != nil {
		slog.Error("the monitor's socket stopped", "path", p.WatchdSocket, "error", err)
		return 1
	}
	slog.Info("vpn-director-watchd stopped")
	return 0
}

type daemonMonitor interface {
	watchdapi.Source
	Run(context.Context)
}

// Instance ownership precedes every prober side effect and outlives shutdown.
func runMonitor(ctx context.Context, path string, build func() daemonMonitor) error {
	listener, err := watchdapi.Listen(ctx, path)
	if err != nil {
		return &socketError{cause: err}
	}
	defer listener.Close()
	if err := ctx.Err(); err != nil {
		return &socketError{cause: err}
	}
	return runDaemon(ctx, path, build(), func(ctx context.Context, _ string, src watchdapi.Source) error {
		return watchdapi.ServeListener(ctx, listener, src)
	})
}

type socketError struct{ cause error }

func (*socketError) Error() string   { return "serve the monitor's socket" }
func (e *socketError) Unwrap() error { return e.cause }

// runDaemon waits for both the monitor's prober/state shutdown and the socket.
// A listener failure cancels the monitor and retains a safe failure cause.
func runDaemon(ctx context.Context, path string, m daemonMonitor, serve func(context.Context, string, watchdapi.Source) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		err := serve(ctx, path, m)
		if err != nil {
			cancel()
		}
		served <- err
	}()
	m.Run(ctx)
	cancel()
	if err := <-served; err != nil {
		return &socketError{cause: err}
	}
	return nil
}

// settingsReader reads the monitor section at every refresh and warns once
// about each distinct set of values out of bounds.
func settingsReader(configSvc *service.ConfigService) func() (monitor.Settings, error) {
	var lastWarns string
	return func() (monitor.Settings, error) {
		cfg, err := configSvc.LoadVPNConfig()
		if err != nil {
			return monitor.Settings{}, err
		}
		s, warns := monitor.SettingsFrom(cfg.Monitor)
		if joined := strings.Join(warns, "\n"); joined != lastWarns {
			lastWarns = joined
			for _, w := range warns {
				slog.Warn(w)
			}
		}
		return s, nil
	}
}

// endpointsReader rebuilds only when a subscription or config changed
// (monitor.Stamp), reusing unchanged subscription files from the shared cache.
func endpointsReader(configSvc *service.ConfigService) func() ([]monitor.Endpoint, map[string]string, error) {
	return endpointsReaderWithLoader(configSvc, vpnconfig.NewSubscriptionCache().Load)
}

func endpointsReaderWithLoader(configSvc *service.ConfigService, load func(string) ([]vpnconfig.Subscription, error)) func() ([]monitor.Endpoint, map[string]string, error) {
	var lastStamp string
	var lastEps []monitor.Endpoint
	var lastRefused map[string]string
	return func() ([]monitor.Endpoint, map[string]string, error) {
		dir, err := configSvc.SubscriptionsDir()
		if err != nil {
			return nil, nil, err
		}
		stamp := monitor.Stamp(dir, configSvc.ConfigPath())
		if stamp != "" && stamp == lastStamp {
			return lastEps, lastRefused, nil
		}
		subs, err := load(dir)
		if err != nil {
			return nil, nil, err
		}
		var active *vpnconfig.ActiveServer
		if cfg, err := configSvc.LoadVPNConfig(); err == nil {
			active = cfg.Xray.ActiveServer
		}
		eps, refused := monitor.Build(subs, active, func(s vpnconfig.Server) (json.RawMessage, error) {
			return service.OutboundJSON(s, "")
		})
		lastStamp, lastEps, lastRefused = stamp, eps, refused
		return eps, refused, nil
	}
}

// levelSetter follows monitor.log_level, and only when it changes: SetLevel
// warns about a level it does not know.
func levelSetter(logger *logging.Logger) func(monitor.Settings) {
	last := "\x00"
	return func(s monitor.Settings) {
		if s.LogLevel != last {
			last = s.LogLevel
			logger.SetLevel(s.LogLevel)
		}
	}
}
