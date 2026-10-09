package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type runtimeDeps struct {
	Monitor daemonMonitor
	Watch   *subwatch.Watch
	Queue   *notifications.Store
	// HealthSubscriptions feeds the subscription-health publisher; nil reads
	// through Watch.LoadSubscriptions.
	HealthSubscriptions func() ([]vpnconfig.Subscription, error)
}

// serveListener is watchdapi.ServeListener; tests fail the socket through it.
var serveListener = watchdapi.ServeListener

type runtimeError struct{ cause error }

func (*runtimeError) Error() string   { return "initialize the watchd runtime" }
func (e *runtimeError) Unwrap() error { return e.cause }

// runRuntime serves the socket listen acquires: watchdapi.Listen, or
// watchdapi.ListenDev in --dev.
func runRuntime(ctx context.Context, listen func(context.Context, string) (net.Listener, error), socket string, build func() (runtimeDeps, error)) error {
	listener, err := listen(ctx, socket)
	if err != nil {
		return &socketError{cause: err}
	}
	defer listener.Close()
	if err := ctx.Err(); err != nil {
		return &socketError{cause: err}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deps, initErr := build()
	if deps.Monitor == nil || deps.Watch == nil || deps.Queue == nil {
		if initErr == nil {
			initErr = errors.New("runtime dependencies are unavailable")
		}
		return &runtimeError{cause: initErr}
	}
	if initErr != nil {
		slog.Warn("Notification storage initialization failed; using the available queue")
	}
	source := &runtimeSource{Source: deps.Monitor, ctx: ctx, watch: deps.Watch, queue: deps.Queue}
	var running sync.WaitGroup
	start := func(run func(context.Context)) {
		running.Add(1)
		go func() {
			defer running.Done()
			defer cancel()
			run(ctx)
		}()
	}
	healthSubscriptions := deps.HealthSubscriptions
	if healthSubscriptions == nil {
		healthSubscriptions = deps.Watch.LoadSubscriptions
	}
	start(deps.Monitor.Run)
	start(deps.Watch.Start)
	start(deps.Queue.Run)
	start(func(ctx context.Context) {
		publishSubscriptionHealth(ctx, healthSubscriptions, deps.Monitor, deps.Queue)
	})
	err = serveListener(ctx, listener, source, source)
	cancel()
	// HTTP handlers may still be finishing a durable write after socket close.
	source.close()
	running.Wait()
	if err := deps.Queue.Flush(); err != nil {
		slog.Warn("Notification storage flush failed during shutdown")
	}
	if err != nil {
		return &socketError{cause: err}
	}
	return nil
}

type runtimeSource struct {
	watchdapi.Source
	ctx      context.Context
	watch    *subwatch.Watch
	queue    *notifications.Store
	requests sync.RWMutex
	closed   bool
}

var _ watchdapi.AutomationSource = (*runtimeSource)(nil)

func (s *runtimeSource) close() {
	s.requests.Lock()
	s.closed = true
	s.requests.Unlock()
}

func (s *runtimeSource) WatchSnapshot() watchdapi.WatchSnapshot {
	s.requests.RLock()
	defer s.requests.RUnlock()
	if s.closed {
		return watchdapi.WatchSnapshot{State: watchdapi.WatchNotRunning}
	}
	snapshot := s.watch.Snapshot()
	snapshot.Notifications = s.queue.Status()
	return snapshot
}

func (s *runtimeSource) ReplaceRecipients(recipients []watchdapi.Recipient) error {
	s.requests.RLock()
	defer s.requests.RUnlock()
	if err := s.available(); err != nil {
		return err
	}
	return s.queue.ReplaceRecipients(recipients)
}

func (s *runtimeSource) Pending(cursor string) (watchdapi.NotificationPage, error) {
	s.requests.RLock()
	defer s.requests.RUnlock()
	if err := s.available(); err != nil {
		return watchdapi.NotificationPage{}, err
	}
	return s.queue.Pending(cursor)
}

func (s *runtimeSource) Ack(chatID int64, eventID watchdapi.EventID) error {
	s.requests.RLock()
	defer s.requests.RUnlock()
	if err := s.available(); err != nil {
		return err
	}
	return s.queue.Ack(chatID, eventID)
}

func (s *runtimeSource) available() error {
	if s.closed {
		return context.Canceled
	}
	return s.ctx.Err()
}
