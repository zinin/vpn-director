package webapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/updater"
)

// Response deadlines for handlers that run shell commands. Each is the
// command's own timeout plus deadlineSlack, so the handler can still write
// its response after the service layer has cut the command off. The
// server-wide WriteTimeout (30 s, server.go) stays in force for every other
// route.
const (
	// deadlineSlack is the margin on top of a command's own timeout. Spec 5.3
	// says 30 s, which turned out to be negative margin in two places: a shell
	// command can hold its pipes for another shell.waitDelay (10 s) after the
	// timeout fires, and a mutation waits up to service.ConfigLockTimeout
	// (30 s) for the config lock before the command even starts.
	deadlineSlack  = 60 * time.Second
	applyDeadline  = service.ApplyTimeout + deadlineSlack
	updateDeadline = service.UpdateTimeout + deadlineSlack
	// statusDeadline covers `vpn-director.sh status`, whose own StatusTimeout
	// equals WriteTimeout: without the extension the connection is torn down
	// exactly when the router is slow enough to need diagnostics.
	statusDeadline = service.StatusTimeout + deadlineSlack
	// ipDeadline covers `curl ifconfig.me`. The route's own worst case -
	// ExternalIPTimeout (15 s) plus shell.waitDelay (10 s) - fits inside the
	// 30 s WriteTimeout with five seconds to spare; the extension means
	// raising ExternalIPTimeout cannot silently put the route back over.
	ipDeadline = service.ExternalIPTimeout + deadlineSlack
	// subscriptionTimeout bounds the downloads of one subscription route and
	// the resolution of every host in them.
	subscriptionTimeout = 90 * time.Second
	// importDeadline covers a subscription route, which runs no shell command.
	// Once subscriptionTimeout has ended its downloads, the publication - or
	// the record of why a download failed - can still wait
	// service.ConfigLockTimeout for the config lock, which the downloads'
	// context does not bound. deadlineSlack is the margin on top, as above.
	importDeadline = subscriptionTimeout + service.ConfigLockTimeout + deadlineSlack
	// githubDeadline covers the synchronous part of an update route: one
	// GitHub API call under updater.APITimeout. POST /api/update answers 202
	// as soon as the download goroutine is under way, so the script's own
	// minutes never sit on the connection.
	githubDeadline = updater.APITimeout + deadlineSlack
)

// logsDeadline covers the all-sources branch of /api/logs, which tails every
// file in deps.LogPaths sequentially under TailTimeout each. The count comes
// from the map rather than a literal, so wiring a fifth source in
// cmd/webui/main.go cannot leave the deadline behind.
func logsDeadline(deps *Deps) time.Duration {
	return time.Duration(len(deps.LogPaths))*service.TailTimeout + deadlineSlack
}

// extendWriteDeadline pushes the response deadline past the server-wide
// WriteTimeout for handlers that run long shell commands. Writers that do
// not support deadlines (httptest.ResponseRecorder) are ignored; any other
// failure is logged, because a silently lost deadline reproduces the torn
// connection this helper exists to prevent.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("failed to extend write deadline", "error", err)
	}
}

// lockLongOp serializes a shell-running handler on deps.OpMutex and extends
// the write deadline both before and after the wait, so the deadline covers
// the command itself and not the time spent queued behind another one.
// ok is false when the client went away while queued: the mutex is already
// released and the handler must return without starting the mutation.
func lockLongOp(w http.ResponseWriter, r *http.Request, deps *Deps, d time.Duration) (unlock func(), ok bool) {
	extendWriteDeadline(w, d)
	deps.OpMutex.Lock()
	if r != nil && r.Context().Err() != nil {
		deps.OpMutex.Unlock()
		return func() {}, false
	}
	extendWriteDeadline(w, d)
	return deps.OpMutex.Unlock, true
}
