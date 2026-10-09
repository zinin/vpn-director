package webapi

import (
	"context"
	"net/http"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func handleWatch(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot := watchdapi.WatchSnapshot{
			State: watchdapi.WatchNotRunning, Message: "Subscription automation is not running",
		}
		if deps.Watch != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if value, err := deps.Watch.Watch(ctx); err == nil {
				switch value.State {
				case watchdapi.WatchStarting, watchdapi.WatchActive, watchdapi.WatchStopped,
					watchdapi.WatchIncompatible, watchdapi.WatchError, watchdapi.WatchNotRunning:
					snapshot = value
				}
			}
		}
		jsonOK(w, snapshot)
	}
}
