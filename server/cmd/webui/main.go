package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/auth"
	"github.com/zinin/vpn-director/server/internal/devmode"
	"github.com/zinin/vpn-director/server/internal/logging"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/platform"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updateflow"
	"github.com/zinin/vpn-director/server/internal/updater"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
	"github.com/zinin/vpn-director/server/internal/webapi"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func main() {
	// Step 2 of a self-update (internal/updater/selfupdate.go): the version
	// being replaced runs this binary with these arguments before installing
	// it. Nothing a daemon does at startup may run first.
	if len(os.Args) > 1 && os.Args[1] == updater.SelfUpdateCommand {
		os.Exit(updater.RunSelfUpdate(os.Args[2:], updater.DaemonWebUI, Version, os.Stdout, os.Stderr))
	}

	configPath := flag.String("config", "/opt/vpn-director/vpn-director.json", "path to vpn-director.json")
	platformFlag := flag.String("platform", "", "platform this router runs (merlin|keenetic); detected when empty")
	shadowPath := flag.String("shadow", "", "password file (default: the platform's - /etc/shadow on Merlin, /opt/etc/passwd on Keenetic)")
	devFlag := flag.Bool("dev", false, "run in development mode (HTTP, mock executor, testdata paths)")
	flag.Parse()

	// The platform decides the password file and which release files the
	// updater installs. Neither can be guessed: a wrong guess is a Web UI
	// nobody can log into, or another firmware's hooks on this router.
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

	// In dev mode, override defaults with testdata paths.
	var p paths.Paths
	// Reported once the logger exists: this runs before it.
	var detachErr error
	if *devFlag {
		p = paths.DevPaths()
		if *configPath == "/opt/vpn-director/vpn-director.json" {
			*configPath = p.ScriptsDir + "/vpn-director.json"
		}
		if *shadowPath == "" {
			*shadowPath = p.ScriptsDir + "/shadow"
		}
		// Validate testdata/dev exists before proceeding.
		if _, err := os.Stat(p.ScriptsDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: %s not found\n", p.ScriptsDir)
			fmt.Fprintf(os.Stderr, "Run from server/ directory: cd server && go run ./cmd/webui --dev\n")
			os.Exit(1)
		}
		ensureDevFiles(*configPath, *shadowPath, p.DefaultDataDir)
	} else {
		p = paths.Default()
		if *shadowPath == "" {
			*shadowPath = plat.PasswordFile
		}
		// Before anything here spawns a shell, and with the flags resolved
		// first so a relative --config keeps naming the same file. The update
		// script starts this daemon from the update directory, which the bot
		// deletes moments later, once it has reported the update - and a
		// working directory that is gone prints getcwd errors into the output
		// the Status page puts on screen.
		detachErr = paths.DetachFromCallerDirectory(configPath, shadowPath)
	}

	// Log file first, like the bot, so a config load failure is logged too.
	slogger, logger, err := logging.NewSlogLogger(p.WebUILogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()
	slog.SetDefault(slogger)

	slog.Info("starting VPN Director Web UI", "version", Version, "commit", Commit, "dev", *devFlag, "platform", plat.Name)

	if detachErr != nil {
		slog.Warn("could not leave the directory this process was started in", "error", detachErr)
	}

	// Derive scripts directory from --config path so runtime reads/writes
	// honour the flag instead of hardcoding /opt/vpn-director.
	scriptsDir := filepath.Dir(*configPath)
	defaultDataDir := filepath.Join(scriptsDir, "data")
	// The flag names the file, not its directory: --config /tmp/custom.json
	// must read that file, not /tmp/vpn-director.json.
	configSvc := service.NewConfigService(scriptsDir, defaultDataDir, *configPath)

	// Load config
	vpnCfg, err := configSvc.LoadVPNConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if vpnCfg.WebUI.Port == 0 {
		vpnCfg.WebUI.Port = 8444
	}
	if vpnCfg.WebUI.CertFile == "" {
		vpnCfg.WebUI.CertFile = "/opt/vpn-director/certs/server.crt"
	}
	if vpnCfg.WebUI.KeyFile == "" {
		vpnCfg.WebUI.KeyFile = "/opt/vpn-director/certs/server.key"
	}
	// Read against the config, like data_dir: this process has left the
	// directory it was started in, and TLS that fails to come up is how a
	// relative certificate path would report it.
	vpnCfg.WebUI.CertFile = paths.Resolve(scriptsDir, vpnCfg.WebUI.CertFile)
	vpnCfg.WebUI.KeyFile = paths.Resolve(scriptsDir, vpnCfg.WebUI.KeyFile)

	logger.SetLevel(vpnCfg.WebUI.LogLevel) // "" keeps info

	// Auto-generate JWT secret if empty. The write goes through the config
	// lock like every other writer; if another process filled the secret in
	// the meantime, that one wins and is used here as well.
	if vpnCfg.WebUI.JWTSecret == "" {
		slog.Warn("jwt_secret not set, generating random secret")
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			slog.Error("failed to generate jwt secret", "error", err)
			os.Exit(1)
		}
		vpnCfg.WebUI.JWTSecret = base64.StdEncoding.EncodeToString(secret)
		err := configSvc.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
			if cfg.WebUI.JWTSecret == "" {
				cfg.WebUI.JWTSecret = vpnCfg.WebUI.JWTSecret
			}
			vpnCfg.WebUI.JWTSecret = cfg.WebUI.JWTSecret
			return nil
		})
		if err != nil {
			slog.Warn("failed to save auto-generated jwt_secret", "error", err)
			// Continue anyway — secret is in memory for this session
		}
	}

	// Services — in dev mode use devmode executor for mock shell responses.
	var executor service.ShellExecutor
	if *devFlag {
		executor = devmode.NewExecutor()
	}

	vpnSvc := service.NewVPNDirectorService(scriptsDir, executor)
	xraySvc := service.NewXrayService(p.XrayTemplate, p.XrayConfig)
	networkSvc := service.NewNetworkService(executor)
	logSvc := service.NewLogService(executor)

	// Auth
	shadowAuth := auth.NewShadowAuth(*shadowPath)
	jwtSvc := auth.NewJWTService(vpnCfg.WebUI.JWTSecret, 24*time.Hour)

	// The Web UI updates both daemons through the same flow as the bot; the
	// progress lines go to the Web UI log, since there is no chat to answer in.
	upd := updater.NewForDaemon(updater.DaemonWebUI)
	upd.SetPlatform(plat.Name)
	updateFlow := updateflow.New(upd, Version, *devFlag)

	deps := &webapi.Deps{
		Config:  configSvc,
		VPN:     vpnSvc,
		Xray:    xraySvc,
		Network: networkSvc,
		Logs:    logSvc,
		LogPaths: map[string]string{
			"bot":    p.BotLogPath,
			"vpn":    p.VPNLogPath,
			"xray":   p.XrayLogPath,
			"webui":  p.WebUILogPath,
			"watchd": p.WatchdLogPath,
		},
		Update:  updateFlow,
		Monitor: watchdapi.NewClient(p.WatchdSocket),
		Shadow:  shadowAuth,
		JWT:     jwtSvc,
		Version: Version,
		Commit:  Commit,
		OpMutex: &sync.Mutex{},
	}

	// Embedded SPA files
	var staticFS fs.FS
	sub, err := fs.Sub(staticFiles, "web/dist")
	if err != nil {
		slog.Warn("no embedded static files", "error", err)
	} else {
		staticFS = sub
	}

	// Start server
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.StartRotation(ctx, p.RotatedLogs(), logging.DefaultMaxSize, time.Minute)

	serverCfg := webapi.ServerConfig{
		Port:     vpnCfg.WebUI.Port,
		CertFile: vpnCfg.WebUI.CertFile,
		KeyFile:  vpnCfg.WebUI.KeyFile,
		DevMode:  *devFlag,
	}

	if *devFlag {
		slog.Info("dev mode: HTTP server, mock executor, testdata paths",
			"config", *configPath,
			"shadow", *shadowPath,
			"port", vpnCfg.WebUI.Port,
		)
	}

	if err := webapi.ListenAndServe(ctx, serverCfg, deps, staticFS); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// ensureDevFiles creates default dev config and shadow files if they don't exist,
// so that `go run ./cmd/webui --dev` works out of the box.
func ensureDevFiles(configPath, shadowPath, dataDir string) {
	// Create data directory if needed.
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		slog.Warn("failed to create data dir", "path", dataDir, "error", err)
	}

	// Shadow file with admin:admin (SHA-256 crypt).
	if _, err := os.Stat(shadowPath); os.IsNotExist(err) {
		// Hash generated with: openssl passwd -5 -salt devsalt admin
		const devShadow = "admin:$5$devsalt$LMFogNzwzA8X4bCMYZf22bdOkeaX6VqsdOAtuYDFXYB:19000:0:99999:7:::\n"
		if err := os.WriteFile(shadowPath, []byte(devShadow), 0600); err != nil {
			slog.Warn("failed to create dev shadow file", "error", err)
		} else {
			slog.Info("created dev shadow file (admin:admin)", "path", shadowPath)
		}
	}

	// VPN Director config with webui section.
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		devConfig := &vpnconfig.VPNDirectorConfig{
			DataDir: "data", // relative to this config file
			WebUI: vpnconfig.WebUIConfig{
				Port:      8444,
				JWTSecret: "dev-secret-not-for-production-use!!",
				LogLevel:  "debug",
			},
			Xray: vpnconfig.XrayConfig{
				Clients:     []string{"192.168.50.0/24"},
				Servers:     []string{},
				ExcludeIPs:  []string{},
				ExcludeSets: []string{"ru"},
			},
			TunnelDirector: vpnconfig.TunnelDirectorConfig{
				Tunnels: map[string]vpnconfig.TunnelConfig{},
			},
		}
		if err := vpnconfig.SaveVPNDirectorConfig(configPath, devConfig); err != nil {
			slog.Warn("failed to create dev config", "error", err)
		} else {
			slog.Info("created dev vpn-director.json", "path", configPath)
		}
	}
}
