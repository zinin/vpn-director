package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"errors"

	"github.com/zinin/vpn-director/server/internal/bot"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/config"
	"github.com/zinin/vpn-director/server/internal/devmode"
	"github.com/zinin/vpn-director/server/internal/logging"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/platform"
	"github.com/zinin/vpn-director/server/internal/updatechecker"
	"github.com/zinin/vpn-director/server/internal/updater"
)

var (
	Version     = "dev"
	VersionFull = "dev"
	Commit      = "unknown"
	BuildDate   = "unknown"
)

func versionString() string {
	return fmt.Sprintf("%s (%s, %s)", VersionFull, Commit, BuildDate)
}

func main() {
	// Step 2 of a self-update (internal/updater/selfupdate.go): the version
	// being replaced runs this binary with these arguments before installing
	// it. Nothing a daemon does at startup may run first.
	if len(os.Args) > 1 && os.Args[1] == updater.SelfUpdateCommand {
		os.Exit(updater.RunSelfUpdate(os.Args[2:], updater.DaemonBot, Version, os.Stdout, os.Stderr))
	}

	devFlag := flag.Bool("dev", false, "Run in development mode (local testing)")
	platformFlag := flag.String("platform", "", "platform this router runs (merlin|keenetic); detected when empty")
	flag.Parse()

	plat, err := platform.Resolve(*platformFlag, *devFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// Every shell this daemon runs takes the platform from the environment, so
	// the answer above has to reach them: vpn-director.sh would otherwise keep
	// detecting on its own, and a --platform would hold for the Go side alone.
	if err := plat.Export(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var p paths.Paths
	var opts []bot.Option
	// Reported once the logger exists: this runs before it.
	var detachErr error

	if *devFlag {
		p = paths.DevPaths()
		opts = append(opts, bot.WithDevMode(devmode.NewExecutor()))
		// Validate testdata/dev exists before proceeding
		if _, err := os.Stat(p.ScriptsDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: %s not found\n", p.ScriptsDir)
			fmt.Fprintf(os.Stderr, "Run from server/ directory: cd server && go run ./cmd/bot --dev\n")
			os.Exit(1)
		}
	} else {
		p = paths.Default()
		// Before anything here spawns a shell. The update script starts this
		// daemon from the update directory, which the bot deletes moments
		// later, once it has reported the update - and a working directory
		// that is gone breaks monit and prints getcwd errors into the output
		// of every command run from here.
		detachErr = paths.DetachFromCallerDirectory()
	}

	// Always add updater service
	upd := updater.NewForDaemon(updater.DaemonBot)
	upd.SetPlatform(plat.Name)
	opts = append(opts, bot.WithUpdater(upd))

	// Initialize logger BEFORE config load (default INFO level)
	slogger, logger, err := logging.NewSlogLogger(p.BotLogPath)
	if err != nil {
		// Can't write to log file - fall back to stderr and exit
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()

	slog.SetDefault(slogger)

	if detachErr != nil {
		slog.Warn("Could not leave the directory this process was started in", "error", detachErr)
	}

	if *devFlag {
		slog.Info("Running in DEVELOPMENT mode", "config", p.BotConfigPath)
	}

	// Now load config - errors will be logged to file
	cfg, err := config.Load(p.BotConfigPath)
	if os.IsNotExist(err) {
		if *devFlag {
			slog.Info("Config not found", "hint", "copy testdata/dev/telegram-bot.json.example to testdata/dev/telegram-bot.json")
		} else {
			slog.Info("Config not found, run setup_telegram_bot.sh first")
		}
		os.Exit(0)
	}
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Update log level from config
	if cfg.LogLevel != "" {
		logger.SetLevel(cfg.LogLevel)
		slog.Debug("Log level set from config", "level", cfg.LogLevel)
	}

	if strings.TrimSpace(cfg.BotToken) == "" {
		slog.Info("Bot token not configured, skipping")
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.StartRotation(ctx, p.RotatedLogs(), logging.DefaultMaxSize, time.Minute)

	// Create chat store for update notifications (not in dev mode)
	var store *chatstore.Store
	if !*devFlag {
		store = chatstore.New(p.DefaultDataDir + "/chats.json")
		opts = append(opts, bot.WithChatStore(store))
	}

	var b *bot.Bot
	{
		backoffs := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
		for attempt := 0; ; attempt++ {
			if b == nil {
				b, err = bot.New(ctx, cfg, p, Version, VersionFull, Commit, BuildDate, opts...)
			} else {
				err = b.Connect(cfg)
			}
			if err == nil {
				break
			}
			// Fail fast on permanent errors (invalid config, invalid bot token)
			var permErr *bot.PermanentError
			if errors.As(err, &permErr) {
				slog.Error("Fatal configuration error", "error", err)
				os.Exit(1)
			}
			delay := backoffs[len(backoffs)-1]
			if attempt < len(backoffs) {
				delay = backoffs[attempt]
			}
			slog.Warn("Failed to connect to Telegram API, retrying...",
				"error", err, "attempt", attempt+1, "retry_in", delay)
			select {
			case <-ctx.Done():
				slog.Error("Shutdown requested during startup retry", "error", err)
				os.Exit(1)
			case <-time.After(delay):
			}
		}
	}

	if err := b.RegisterCommands(); err != nil {
		slog.Warn("Failed to register commands", "error", err)
	}

	// Start update checker if configured (not in dev mode, not dev version)
	if cfg.UpdateCheckInterval > 0 && !*devFlag && Version != "dev" {
		checker := updatechecker.New(
			updater.New(),
			store,
			b.Sender(),
			b.Auth(),
			Version,
		)
		go checker.Run(ctx, cfg.UpdateCheckInterval)
	}

	slog.Info("Telegram Bot started", "version", versionString(), "platform", plat.Name)
	b.Run(ctx)
	slog.Info("Bot stopped")
}
