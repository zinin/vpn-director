package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func publishSubscriptionHealth(ctx context.Context, load func() ([]vpnconfig.Subscription, error), source watchdapi.Source, queue *notifications.Store) {
	if load == nil {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		subs, err := load()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("Subscription health load failed; keeping the previous health")
		} else {
			snapshot := source.Snapshot()
			if ctx.Err() != nil {
				return
			}
			if err := queue.ObserveSubscriptions(subs, snapshot); err != nil {
				slog.Warn("Subscription health notification storage update failed; delivery will retry")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
