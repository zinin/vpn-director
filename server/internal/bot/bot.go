// internal/bot/bot.go
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/config"
	"github.com/zinin/vpn-director/server/internal/handler"
	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/startup"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/updateflow"
	"github.com/zinin/vpn-director/server/internal/updater"
	"github.com/zinin/vpn-director/server/internal/vless"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/wizard"
)

// Bot is the main Telegram bot struct with DI
type Bot struct {
	api         *tgbotapi.BotAPI
	auth        *Auth
	router      *Router
	sender      telegram.MessageSender
	devMode     bool
	executor    service.ShellExecutor
	updater     updater.Updater
	chatStore   *chatstore.Store
	pathManager *PathManager
	subWatch    *subwatch.Watch
	// apiBase is empty in production and set only by tests, where one local
	// server answers both the path probe and the Telegram API, as one host
	// does in production.
	apiBase string
	// version the bot is running. The startup notifier compares it with the
	// version a failed update left behind.
	version string
}

// Option configures the Bot.
type Option func(*Bot)

// WithDevMode enables development mode with custom executor.
func WithDevMode(executor service.ShellExecutor) Option {
	return func(b *Bot) {
		b.devMode = true
		b.executor = executor
	}
}

// WithUpdater sets the updater service.
func WithUpdater(u updater.Updater) Option {
	return func(b *Bot) {
		b.updater = u
	}
}

// WithChatStore sets the chat store for recording user interactions.
func WithChatStore(store *chatstore.Store) Option {
	return func(b *Bot) {
		b.chatStore = store
	}
}

// withAPIBase points the path probe and the Telegram API at base. Tests only.
func withAPIBase(base string) Option {
	return func(b *Bot) { b.apiBase = base }
}

// New creates a new Bot with full dependency injection.
// Use WithDevMode() and WithUpdater() options to configure the bot.
func New(ctx context.Context, cfg *config.Config, p paths.Paths, version, versionFull, commit, buildDate string, opts ...Option) (*Bot, error) {
	b := &Bot{version: version}

	// Apply options
	for _, opt := range opts {
		opt(b)
	}

	// Create services (executor may be set by WithDevMode option)
	configSvc := service.NewConfigService(p.ScriptsDir, p.DefaultDataDir)
	vpnSvc := service.NewVPNDirectorService(p.ScriptsDir, b.executor)
	xraySvc := service.NewXrayService(p.XrayTemplate, p.XrayConfig)
	networkSvc := service.NewNetworkService(b.executor)
	logSvc := service.NewLogService(b.executor)

	var httpClient *http.Client
	var stopMonitor context.CancelFunc
	var monitorCtx context.Context
	if b.devMode {
		httpClient = &http.Client{}
	} else {
		pm := NewPathManager(PathManagerConfig{
			Token:        cfg.BotToken,
			LoadVPN:      configSvc.LoadVPNConfig,
			LoadPlatform: vpnSvc.Platform,
			APIBase:      b.apiBase,
		})
		pm.SelectOnce(ctx)
		b.pathManager = pm
		httpClient = NewPathClient(pm)
		var stop context.CancelFunc
		monitorCtx, stop = context.WithCancel(ctx)
		stopMonitor = stop
		go pm.Start(monitorCtx)
	}

	endpoint := tgbotapi.APIEndpoint
	if b.apiBase != "" {
		endpoint = b.apiBase + "/bot%s/%s"
	}
	api, err := tgbotapi.NewBotAPIWithClient(cfg.BotToken, endpoint, httpClient)
	if err != nil {
		if stopMonitor != nil {
			stopMonitor()
		}
		var apiErr *tgbotapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 401 {
			return nil, &PermanentError{Err: fmt.Errorf("invalid bot token: %w", err)}
		}
		return nil, err
	}

	slog.Info("Authorized", "username", api.Self.UserName)

	sender := telegram.NewSender(api)
	b.api = api
	b.auth = NewAuth(cfg.AllowedUsers)
	b.sender = sender

	if !b.devMode {
		sw := &subwatch.Watch{
			LoadVPN:      configSvc.LoadVPNConfig,
			LoadPlatform: vpnSvc.Platform,
			UpdateVPN:    configSvc.UpdateVPNConfig,
			Apply:        vpnSvc.Apply,
			RestartXray:  vpnSvc.RestartXray,
			SaveServers:  configSvc.SaveServers,
			Generate: func(s vpnconfig.Server) (bool, error) {
				cfg, err := configSvc.LoadVPNConfig()
				if err != nil {
					return false, err
				}
				ports := service.InboundPorts{}
				ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(cfg)
				return service.GenerateAndRecordActiveServer(configSvc, xraySvc, s, ports)
			},
			Probe: func(ctx context.Context, port int) error {
				return subwatch.ProbeSOCKS(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), subwatch.ProbeURL)
			},
			Fetch: func(ctx context.Context, rawURL string) ([]vpnconfig.Server, error) {
				body, err := b.fetchSub(ctx, rawURL, configSvc, vpnSvc)
				if err != nil {
					return nil, err
				}
				parsed, _ := vless.DecodeSubscription(string(body))
				if len(parsed) == 0 {
					return nil, errors.New("no VLESS servers")
				}
				var resolved []vpnconfig.Server
				for _, s := range parsed {
					if err := s.ResolveIPs(); err != nil {
						continue
					}
					resolved = append(resolved, s.ToVPNConfig())
				}
				if len(resolved) == 0 {
					return nil, errors.New("could not resolve IP for any server")
				}
				return resolved, nil
			},
			Notify: func(msg string) {
				if b.chatStore == nil || b.sender == nil {
					return
				}
				users, err := b.chatStore.GetActiveUsers()
				if err != nil {
					return
				}
				for _, u := range users {
					b.sender.SendPlain(u.ChatID, msg)
				}
			},
		}
		b.subWatch = sw
		go sw.Start(monitorCtx)
	}

	// Create handler dependencies
	deps := &handler.Deps{
		Sender:      sender,
		Config:      configSvc,
		VPN:         vpnSvc,
		Xray:        xraySvc,
		Network:     networkSvc,
		Logs:        logSvc,
		Paths:       p,
		Version:     version,
		VersionFull: versionFull,
		Commit:      commit,
		BuildDate:   buildDate,
		DevMode:     b.devMode,
	}
	if pm := b.pathManager; pm != nil {
		deps.TelegramPath = func() string { return pm.Current().String() }
	}

	// Create handlers
	statusHandler := handler.NewStatusHandler(deps)
	serversHandler := handler.NewServersHandler(deps)
	importHandler := handler.NewImportHandler(deps)
	miscHandler := handler.NewMiscHandler(deps)
	// updateflow owns every decision behind /update; the handler is an adapter.
	updateFlow := updateflow.New(b.updater, version, b.devMode)
	updateHandler := handler.NewUpdateHandler(sender, updateFlow, version)
	wizardHandler := wizard.NewHandler(sender, configSvc, vpnSvc, xraySvc)
	xrayHandler := handler.NewXrayHandler(deps)
	excludeHandler := handler.NewExcludeHandler(deps)
	clientsHandler := handler.NewClientsHandler(deps)

	// Create router
	router := NewRouter(statusHandler, serversHandler, importHandler, miscHandler, updateHandler, wizardHandler, xrayHandler, excludeHandler, clientsHandler)
	b.router = router

	return b, nil
}

// RegisterCommands registers bot commands with Telegram
func (b *Bot) RegisterCommands() error {
	commands := []tgbotapi.BotCommand{
		{Command: "status", Description: "Xray status"},
		{Command: "xray", Description: "Switch Xray server"},
		{Command: "servers", Description: "Server list"},
		{Command: "import", Description: "Import servers from URL"},
		{Command: "configure", Description: "Configuration wizard"},
		{Command: "exclude", Description: "Manage excluded IPs"},
		{Command: "clients", Description: "Manage VPN clients"},
		{Command: "restart", Description: "Restart VPN Director"},
		{Command: "stop", Description: "Stop VPN Director"},
		{Command: "logs", Description: "Recent logs"},
		{Command: "ip", Description: "External IP"},
		{Command: "update", Description: "Update VPN Director to latest release"},
		{Command: "version", Description: "Bot version"},
	}

	cfg := tgbotapi.NewSetMyCommands(commands...)
	_, err := b.api.Request(cfg)
	if err != nil {
		return err
	}

	slog.Info("Registered bot commands", "count", len(commands))
	return nil
}

// Run starts the bot and processes updates until context is cancelled
func (b *Bot) Run(ctx context.Context) {
	if b.pathManager != nil {
		go b.pathManager.Start(ctx)
	}
	if b.subWatch != nil {
		go b.subWatch.Start(ctx)
	}
	// b.chatStore is a typed nil in dev mode; assigning it straight into the
	// interface would hand CheckAndSendNotify a non-nil interface over a nil
	// pointer and panic on the first call.
	var store startup.ChatStore
	if b.chatStore != nil {
		store = b.chatStore
	}
	// Check for pending update notification before starting polling
	if err := startup.CheckAndSendNotify(b.sender, store, startup.DefaultNotifyFile, startup.DefaultUpdateDir, b.version); err != nil {
		slog.Warn("Failed to send update notification", "error", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	slog.Info("Bot started, waiting for messages")

	for {
		select {
		case <-ctx.Done():
			slog.Info("Shutting down bot")
			b.api.StopReceivingUpdates()
			return
		case update, ok := <-updates:
			if !ok {
				slog.Warn("Updates channel closed, stopping bot")
				return
			}
			if msg := update.Message; msg != nil {
				// Skip messages without sender (channel posts, service messages)
				if msg.From == nil {
					continue
				}
				username := msg.From.UserName
				if !b.auth.IsAuthorized(username) {
					slog.Warn("Unauthorized access attempt", "username", username)
					b.sender.SendPlain(msg.Chat.ID, "Access denied")
					continue
				}
				// Record interaction for update notifications
				if b.chatStore != nil {
					_ = b.chatStore.RecordInteraction(username, msg.Chat.ID)
				}
				// Log command without arguments for sensitive commands (import may contain tokens)
				slog.Info("Command received", "username", username, "command", sanitizeLogMessage(msg))
				b.router.RouteMessage(msg)
			}
			if cb := update.CallbackQuery; cb != nil {
				// Skip callbacks without sender (should not happen, but be defensive)
				if cb.From == nil {
					continue
				}
				// Acknowledge callback to prevent UI spinner hanging
				b.sender.AckCallback(cb.ID)
				username := cb.From.UserName
				if !b.auth.IsAuthorized(username) {
					slog.Warn("Unauthorized callback", "username", username)
					continue
				}
				// Record interaction for update notifications
				// Note: cb.Message can be nil for inline callbacks, so check before accessing
				if b.chatStore != nil && cb.Message != nil {
					_ = b.chatStore.RecordInteraction(username, cb.Message.Chat.ID)
				}
				slog.Info("Callback received", "username", username, "data", cb.Data)
				b.router.RouteCallback(cb)
			}
		}
	}
}

// sanitizeLogMessage returns a safe-to-log representation of the message.
// Sensitive commands (like /import) have their arguments redacted.
func sanitizeLogMessage(msg *tgbotapi.Message) string {
	if msg.IsCommand() {
		cmd := msg.Command()
		// Redact arguments for commands that may contain sensitive data (URLs with tokens)
		switch cmd {
		case "import":
			return "/" + cmd + " [REDACTED]"
		}
	}
	return msg.Text
}

// Auth returns the authorization handler (for update checker).
func (b *Bot) Auth() *Auth {
	return b.auth
}

// Sender returns the message sender (for update checker).
func (b *Bot) Sender() telegram.MessageSender {
	return b.sender
}
