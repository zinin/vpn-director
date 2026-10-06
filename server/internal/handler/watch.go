package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func automationStatus(m watchdapi.API, w watchdapi.WatchAPI) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Buffered replies let canceled readers finish after the status deadline.
	monitorReply := make(chan watchdapi.Snapshot, 1)
	watchReply := make(chan watchdapi.WatchSnapshot, 1)
	go func(reply chan<- watchdapi.Snapshot) {
		snapshot := watchdapi.Snapshot{State: watchdapi.StateNotRunning}
		if m != nil {
			if value, err := m.Monitor(ctx); err == nil {
				switch value.State {
				case watchdapi.StateOK, watchdapi.StateStopped, watchdapi.StateDisabled,
					watchdapi.StateNoXray, watchdapi.StateWANDown, watchdapi.StateProberError, watchdapi.StateNotRunning:
					snapshot = value
				}
			}
		}
		reply <- snapshot
	}(monitorReply)
	go func(reply chan<- watchdapi.WatchSnapshot) {
		snapshot := watchdapi.WatchSnapshot{
			State: watchdapi.WatchNotRunning, Message: "Subscription automation is not running",
		}
		if w != nil {
			if value, err := w.Watch(ctx); err == nil {
				switch value.State {
				case watchdapi.WatchStarting, watchdapi.WatchActive, watchdapi.WatchStopped,
					watchdapi.WatchIncompatible, watchdapi.WatchError, watchdapi.WatchNotRunning:
					snapshot = value
				}
			}
		}
		reply <- snapshot
	}(watchReply)

	monitor := watchdapi.Snapshot{State: watchdapi.StateNotRunning}
	watch := watchdapi.WatchSnapshot{State: watchdapi.WatchNotRunning}
	for remaining := 2; remaining > 0; remaining-- {
		select {
		case monitor = <-monitorReply:
			monitorReply = nil
		case watch = <-watchReply:
			watchReply = nil
		case <-ctx.Done():
			// Retain any completed read even when the other one exhausted the budget.
			select {
			case monitor = <-monitorReply:
			default:
			}
			select {
			case watch = <-watchReply:
			default:
			}
			return formatAutomationStatus(monitor, watch)
		}
	}
	return formatAutomationStatus(monitor, watch)
}

func formatAutomationStatus(m watchdapi.Snapshot, w watchdapi.WatchSnapshot) string {
	monitor := fmt.Sprintf("Monitor: %s", m.State)
	if note := stateNote(m.State); note != "" {
		monitor += " — " + note
	}
	lines := []string{
		monitor,
		fmt.Sprintf("Watch: %s", w.State),
		fmt.Sprintf("Committed failover: %t", w.CommittedFailover),
		fmt.Sprintf("Pending restore: %t", w.PendingRestore),
		fmt.Sprintf("Notifications pending: %d", w.Notifications.Pending),
	}
	if w.Action != "" {
		lines = append(lines, "Action: "+automationText(w.Action))
	}
	if w.Message != "" {
		lines = append(lines, "Watch message: "+automationText(w.Message))
	}
	if w.Notifications.StorageError != "" {
		lines = append(lines, "Storage error: "+automationText(w.Notifications.StorageError))
	}
	return strings.Join(lines, "\n")
}

func automationText(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > 512 {
		return string(runes[:512]) + "…"
	}
	return string(runes)
}
