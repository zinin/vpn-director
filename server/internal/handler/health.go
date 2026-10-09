// internal/handler/health.go
package handler

import (
	"context"
	"fmt"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// health is what vpn-director-watchd says of every server, by subscription
// id and index. ok is false when it said nothing: no marks then.
type health struct {
	ok      bool
	note    string // the monitor's state when it is not ok, for the header
	servers map[string][]watchdapi.ServerHealth
	alive   map[string]int
}

// monitorHealth asks the daemon about subs; one that does not answer gives no
// marks.
func monitorHealth(api watchdapi.API, subs []vpnconfig.Subscription) health {
	if api == nil {
		return health{}
	}
	snap, err := api.Monitor(context.Background())
	if err != nil {
		return health{}
	}
	h := health{ok: true, note: stateNote(snap.State), servers: map[string][]watchdapi.ServerHealth{}, alive: map[string]int{}}
	for _, sub := range subs {
		list := make([]watchdapi.ServerHealth, len(sub.Servers))
		for i, s := range sub.Servers {
			s.Subscription = sub.ID
			list[i] = watchdapi.Health(endpoint.Keys(s), snap)
			if list[i].Status == watchdapi.StatusAlive {
				h.alive[sub.ID]++
			}
		}
		h.servers[sub.ID] = list
	}
	return h
}

// of is the status of the server at index in subscription sub.
func (h health) of(sub string, index int) (watchdapi.ServerHealth, bool) {
	list := h.servers[sub]
	if !h.ok || index < 0 || index >= len(list) {
		return watchdapi.ServerHealth{}, false
	}
	return list[index], true
}

// header is the line under a page's title: that the monitor is not running,
// or what it is doing when that is not checking.
func (h health) header() string {
	if !h.ok {
		return "мониторинг не запущен"
	}
	return h.note
}

func stateNote(s watchdapi.State) string {
	switch s {
	case watchdapi.StateStopped:
		return "мониторинг приостановлен: VPN Director остановлен"
	case watchdapi.StateDisabled:
		return "мониторинг выключен в настройках"
	case watchdapi.StateNoXray:
		return "мониторинг: xray не найден"
	case watchdapi.StateWANDown:
		return "мониторинг: WAN недоступен, статусы сохранены"
	case watchdapi.StateProberError:
		return "мониторинг: пробный Xray не запускается"
	}
	return ""
}

// mark is a server's status as a line or a button starts with it.
func mark(s watchdapi.ServerHealth) string {
	switch s.Status {
	case watchdapi.StatusAlive:
		return "🟢"
	case watchdapi.StatusDead:
		return "🔴"
	case watchdapi.StatusRejected:
		return "⛔"
	default:
		return "⚪"
	}
}

// latency is " · 142 ms" for a live server, nothing otherwise.
func latency(s watchdapi.ServerHealth) string {
	if s.Status != watchdapi.StatusAlive {
		return ""
	}
	return fmt.Sprintf(" · %d ms", s.LatencyMS)
}
