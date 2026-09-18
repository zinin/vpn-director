package webapi

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/auth"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updateflow"
)

// UpdateFlow is the subset of updateflow.Flow the API handlers use. Declared
// here so the handler tests can drive every outcome without a fake GitHub.
type UpdateFlow interface {
	Check(ctx context.Context, force bool) (updateflow.CheckResult, error)
	Start(ctx context.Context, initiator string, chatID int64, progress func(string)) (updateflow.StartResult, error)
	InProgress() bool
}

// Deps holds all dependencies required by the HTTP API handlers.
type Deps struct {
	Config       service.ConfigStore
	VPN          service.VPNDirector
	Xray         service.XrayGenerator
	Network      service.NetworkInfo
	Logs         service.LogReader
	LogPaths     map[string]string // log source name -> file path, built by main from paths.Paths
	Update       UpdateFlow        // self-update orchestration, shared with the bot
	Shadow       *auth.ShadowAuth
	JWT          *auth.JWTService
	Version      string
	Commit       string
	OpMutex      *sync.Mutex  // serializes mutating shell operations
	ImportClient *http.Client // subscription downloads; nil means ssrf.NewClient
	loginLimiter *rateLimiter // rate limiter for login endpoint
}

// NewRouter creates the top-level HTTP handler with all routes registered.
// staticFS provides the embedded Vue SPA assets; pass nil to disable SPA serving.
// The variadic ctx stops the login limiter's cleanup goroutine; tests that do
// not care leave it out, as newRateLimiter does.
func NewRouter(deps *Deps, staticFS fs.FS, ctx ...context.Context) http.Handler {
	mux := http.NewServeMux()

	deps.loginLimiter = newRateLimiter(5, 1*time.Minute, 30*time.Second, ctx...)

	// Public routes (no auth required).
	mux.HandleFunc("POST /api/login", handleLogin(deps))

	// Protected routes (require valid JWT).
	protectedMux := http.NewServeMux()
	registerProtectedRoutes(protectedMux, deps)

	authMW := authMiddleware(deps.JWT, deps.Shadow)
	mux.Handle("/api/", authMW(protectedMux))

	// SPA fallback: serve static files and fall back to index.html.
	if staticFS != nil {
		mux.Handle("/", spaHandler(staticFS))
	}

	return loggingMiddleware(mux)
}

// registerProtectedRoutes adds all authenticated API endpoints to the mux.
func registerProtectedRoutes(mux *http.ServeMux, deps *Deps) {
	// Auth
	mux.HandleFunc("POST /api/logout", handleLogout)

	// Status & control
	mux.HandleFunc("GET /api/status", handleStatus(deps))
	mux.HandleFunc("POST /api/apply", handleApply(deps))
	mux.HandleFunc("POST /api/restart", handleRestart(deps))
	mux.HandleFunc("POST /api/stop", handleStop(deps))

	// IPSets
	mux.HandleFunc("POST /api/ipsets/update", handleUpdateIPsets(deps))

	// Info
	mux.HandleFunc("GET /api/ip", handleIP(deps))
	mux.HandleFunc("GET /api/version", handleVersion(deps))
	mux.HandleFunc("GET /api/platform", handlePlatform(deps))

	// Servers
	mux.HandleFunc("GET /api/servers", handleListServers(deps))
	mux.HandleFunc("POST /api/servers/active", handleSelectServer(deps))
	mux.HandleFunc("POST /api/servers/import", handleImportServers(deps))

	// Clients
	mux.HandleFunc("GET /api/clients", handleListClients(deps))
	mux.HandleFunc("POST /api/clients", handleAddClient(deps))
	mux.HandleFunc("POST /api/clients/pause", handlePauseClient(deps))
	mux.HandleFunc("POST /api/clients/resume", handleResumeClient(deps))
	mux.HandleFunc("DELETE /api/clients", handleDeleteClient(deps))

	// Exclusions — sets
	mux.HandleFunc("GET /api/excludes/sets", handleListExcludeSets(deps))
	mux.HandleFunc("POST /api/excludes/sets", handleUpdateExcludeSets(deps))

	// Exclusions — IPs
	mux.HandleFunc("GET /api/excludes/ips", handleListExcludeIPs(deps))
	mux.HandleFunc("POST /api/excludes/ips", handleAddExcludeIP(deps))
	mux.HandleFunc("DELETE /api/excludes/ips", handleDeleteExcludeIP(deps))

	// Logs & config
	mux.HandleFunc("GET /api/logs", handleLogs(deps))
	mux.HandleFunc("GET /api/config", handleConfig(deps))

	// Self-update
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck(deps))
	mux.HandleFunc("POST /api/update", handleUpdateStart(deps))
	mux.HandleFunc("GET /api/update/status", handleUpdateStatus(deps))

	// Fallback for unknown API paths: JSON 404 instead of the mux's text/plain.
	// Auth still runs first because this mux sits behind authMiddleware.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w, http.StatusNotFound, "not found")
	})
}

// spaHandler serves static files from the embedded filesystem. If a file is
// not found and the request path does not start with "/api/", it falls back to
// index.html so the Vue SPA router can handle the path. Hashed bundles under
// assets/ are immutable; index.html must be revalidated so a new binary's
// bundle names are picked up after an update.
func spaHandler(staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if f, err := staticFS.Open(path); err == nil {
			f.Close()
		} else {
			// File not found — serve index.html for SPA routing.
			path = "index.html"
			r.URL.Path = "/"
		}

		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
