package handler

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const automationSecret = "synthetic-automation-credential-13"

type automationWatchRead func(context.Context) (watchdapi.WatchSnapshot, error)

func (f automationWatchRead) Watch(ctx context.Context) (watchdapi.WatchSnapshot, error) {
	return f(ctx)
}

type automationMonitorRead func(context.Context) (watchdapi.Snapshot, error)

func (f automationMonitorRead) Monitor(ctx context.Context) (watchdapi.Snapshot, error) {
	return f(ctx)
}

func (automationMonitorRead) Check(context.Context, []string) (int, error) { return 0, nil }

func automationStateLine(t *testing.T, text, component, state string) string {
	t.Helper()
	var line string
	for _, candidate := range strings.Split(text, "\n") {
		lower := strings.ToLower(candidate)
		labelled := strings.Contains(lower, component)
		if component == "monitor" {
			labelled = labelled || strings.Contains(lower, "монитор")
		} else {
			labelled = labelled || strings.Contains(lower, "автомат")
		}
		if labelled {
			line = candidate
			break
		}
	}
	if line == "" {
		t.Fatalf("no independent %s line in automation status: %q", component, text)
	}
	lower := strings.ToLower(line)
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if word == state {
			return line
		}
	}
	// The bot's existing monitor notes are localized; wording is not the contract.
	phrases := map[string][]string{
		"ok":           {"работает", "активен", "проверяет"},
		"disabled":     {"выключен", "отключен", "отключён"},
		"active":       {"активна", "работает"},
		"starting":     {"запускается", "запуск"},
		"stopped":      {"остановлен", "остановлена"},
		"incompatible": {"несовместим", "совместимость не подтверждена"},
		"error":        {"ошибка", "недоступна конфигурация"},
		"not_running":  {"не запущен", "не запущена", "недоступен", "недоступна", "не работает"},
	}
	for _, phrase := range phrases[state] {
		if strings.Contains(lower, phrase) {
			if (state == "active" || state == "ok") && strings.Contains(lower, "не работает") {
				continue
			}
			return line
		}
	}
	t.Fatalf("%s line %q does not explain %s", component, line, state)
	return ""
}

func assertIndependentAutomationStates(t *testing.T, text, monitor, watch string) {
	t.Helper()
	monitorLine := automationStateLine(t, text, "monitor", monitor)
	watchLine := automationStateLine(t, text, "watch", watch)
	if monitorLine == watchLine {
		t.Fatalf("monitor and watch were collapsed into one line: %q", text)
	}
	for _, forbidden := range []string{automationSecret, "https://provider.example/private/", "vless://", "bot_token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("automation status exposes %q: %q", forbidden, text)
		}
	}
}

func assertAutomationQueueHealth(t *testing.T, text, pending, storageError string) {
	t.Helper()
	found := false
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "pending") && !strings.Contains(lower, "уведом") && !strings.Contains(lower, "достав") {
			continue
		}
		for _, word := range strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsDigit(r) }) {
			found = found || word == pending
		}
	}
	if !found || !strings.Contains(text, storageError) {
		t.Fatalf("queue pending=%s/storage health is missing: %q", pending, text)
	}
}

func TestStatus_MonitorAndWatchAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		monitor    watchdapi.API
		watchState watchdapi.WatchState
		watchErr   error
		wantMon    string
		wantWatch  string
	}{
		{"disabled monitor active watch", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}, watchdapi.WatchActive, nil, "disabled", "active"},
		{"incompatible watch leaves monitor available", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}, watchdapi.WatchIncompatible, nil, "ok", "incompatible"},
		{"unavailable watch leaves disabled monitor visible", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}, "", errors.New("old daemon: " + automationSecret), "disabled", "not_running"},
		{"unavailable monitor leaves watch active", &fakeMonitor{err: errors.New("provider error: " + automationSecret)}, watchdapi.WatchActive, nil, "not_running", "active"},
		{"both unavailable", nil, "", errors.New("https://provider.example/private/" + automationSecret), "not_running", "not_running"},
		{"starting watch", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}, watchdapi.WatchStarting, nil, "ok", "starting"},
		{"stopped watch", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}, watchdapi.WatchStopped, nil, "disabled", "stopped"},
		{"watch configuration error", &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}, watchdapi.WatchError, nil, "ok", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watch := automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
				return watchdapi.WatchSnapshot{State: tc.watchState, UpdatedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}, tc.watchErr
			})
			text := automationStatus(tc.monitor, watch)
			assertIndependentAutomationStates(t, text, tc.wantMon, tc.wantWatch)
		})
	}

	t.Run("nil watch is unavailable rather than an active monitor alias", func(t *testing.T) {
		text := automationStatus(&fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}, nil)
		assertIndependentAutomationStates(t, text, "ok", "not_running")
	})

	t.Run("queue storage failure does not hide active automation", func(t *testing.T) {
		watch := automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
			return watchdapi.WatchSnapshot{
				State: watchdapi.WatchActive, Action: "walking", CommittedFailover: true,
				Notifications: watchdapi.NotificationsStatus{Pending: 2, StorageError: "cannot write notification storage"},
			}, nil
		})
		text := automationStatus(&fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}, watch)
		assertIndependentAutomationStates(t, text, "disabled", "active")
		assertAutomationQueueHealth(t, text, "2", "cannot write notification storage")
		if !strings.Contains(strings.ToLower(text), "walking") && !strings.Contains(strings.ToLower(text), "перебор") {
			t.Fatalf("the active watch's walk action is missing: %q", text)
		}
	})

	t.Run("committed failover and pending restore are separate recovery intents", func(t *testing.T) {
		monitor := &fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateDisabled}}
		read := func(snapshot watchdapi.WatchSnapshot) string {
			return automationStatus(monitor, automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
				return snapshot, nil
			}))
		}
		idle := read(watchdapi.WatchSnapshot{State: watchdapi.WatchActive})
		for _, tc := range []struct {
			name      string
			committed bool
			pending   bool
		}{
			{"committed failover", true, false},
			{"pending restore", false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				text := read(watchdapi.WatchSnapshot{State: watchdapi.WatchActive, CommittedFailover: tc.committed, PendingRestore: tc.pending})
				assertIndependentAutomationStates(t, text, "disabled", "active")
				if text == idle {
					t.Fatalf("%s is invisible in automation status: %q", tc.name, text)
				}
				lower := strings.ToLower(strings.ReplaceAll(text, "_", " "))
				committedOn := regexp.MustCompile(`committed(?: failover)?\s*[:=]\s*(?:true|yes|да)`)
				committedOff := regexp.MustCompile(`committed(?: failover)?\s*[:=]\s*(?:false|no|нет)|not committed`)
				pendingOn := regexp.MustCompile(`(?:pending restore|restore pending)\s*[:=]\s*(?:true|yes|да)`)
				pendingOff := regexp.MustCompile(`(?:pending restore|restore pending)\s*[:=]\s*(?:false|no|нет)|not pending`)
				if (tc.committed && committedOff.MatchString(lower)) || (!tc.committed && committedOn.MatchString(lower)) ||
					(tc.pending && pendingOff.MatchString(lower)) || (!tc.pending && pendingOn.MatchString(lower)) {
					t.Fatalf("recovery intent booleans are inverted or conflated: committed=%t pending=%t text=%q", tc.committed, tc.pending, text)
				}
				if tc.committed && !strings.Contains(lower, "committed") && !strings.Contains(lower, "зафиксирован") && !strings.Contains(lower, "на резервном") {
					t.Fatalf("committed fallback was not explained: %q", text)
				}
				if tc.pending && !(strings.Contains(lower, "pending") && strings.Contains(lower, "restore")) && !(strings.Contains(lower, "ожида") && strings.Contains(lower, "восстанов")) {
					t.Fatalf("pending restore was not explained: %q", text)
				}
			})
		}
	})

	t.Run("safe runtime explanation is retained", func(t *testing.T) {
		watch := automationWatchRead(func(context.Context) (watchdapi.WatchSnapshot, error) {
			return watchdapi.WatchSnapshot{State: watchdapi.WatchIncompatible, Message: "Bot compatibility is unconfirmed"}, nil
		})
		text := automationStatus(&fakeMonitor{snap: watchdapi.Snapshot{State: watchdapi.StateOK}}, watch)
		assertIndependentAutomationStates(t, text, "ok", "incompatible")
		if !strings.Contains(text, "Bot compatibility is unconfirmed") {
			t.Fatalf("the safe availability explanation was dropped: %q", text)
		}
	})
}

func TestStatus_MonitorAndWatchShareParallelDeadline(t *testing.T) {
	type read struct {
		component string
		ctx       context.Context
		deadline  time.Time
		bounded   bool
	}
	reads := make(chan read, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	done := make(chan string, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("automation status did not drain its IPC reads")
		}
	})
	record := func(component string, ctx context.Context) {
		deadline, bounded := ctx.Deadline()
		reads <- read{component, ctx, deadline, bounded}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	monitor := automationMonitorRead(func(ctx context.Context) (watchdapi.Snapshot, error) {
		record("monitor", ctx)
		return watchdapi.Snapshot{State: watchdapi.StateDisabled}, nil
	})
	watch := automationWatchRead(func(ctx context.Context) (watchdapi.WatchSnapshot, error) {
		record("watch", ctx)
		return watchdapi.WatchSnapshot{State: watchdapi.WatchActive}, nil
	})
	started := time.Now()
	go func() {
		defer close(finished)
		done <- automationStatus(monitor, watch)
	}()
	var observed []read
	for len(observed) < 2 {
		select {
		case r := <-reads:
			observed = append(observed, r)
		case <-time.After(time.Second):
			t.Fatal("the second IPC read did not start while the first was blocked; reads must be parallel")
		}
	}
	first, second := observed[0], observed[1]
	if first.component == second.component || !first.bounded || !second.bounded || first.ctx != second.ctx || !first.deadline.Equal(second.deadline) {
		t.Fatalf("IPC reads do not share one context/deadline: %+v %+v", first, second)
	}
	if budget := first.deadline.Sub(started); budget < 1900*time.Millisecond || budget > 2100*time.Millisecond {
		t.Fatalf("shared IPC budget=%s, want 2 seconds", budget)
	}
	unblock()
	select {
	case text := <-done:
		assertIndependentAutomationStates(t, text, "disabled", "active")
	case <-time.After(time.Second):
		t.Fatal("completed parallel IPC reads did not yield status")
	}
	if first.ctx.Err() == nil || second.ctx.Err() == nil {
		t.Fatal("automation status did not cancel its shared context after the reads")
	}
}

func TestStatus_MonitorAndWatchHungReadsUseOneTwoSecondBudget(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	done := make(chan string, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("hung automation reads did not drain")
		}
	})
	wait := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return errors.New("IPC timeout: " + automationSecret)
		case <-release:
			return errors.New("fixture released: " + automationSecret)
		}
	}
	monitor := automationMonitorRead(func(ctx context.Context) (watchdapi.Snapshot, error) {
		return watchdapi.Snapshot{}, wait(ctx)
	})
	watch := automationWatchRead(func(ctx context.Context) (watchdapi.WatchSnapshot, error) {
		return watchdapi.WatchSnapshot{}, wait(ctx)
	})
	started := time.Now()
	go func() {
		defer close(finished)
		done <- automationStatus(monitor, watch)
	}()
	select {
	case text := <-done:
		elapsed := time.Since(started)
		if elapsed < 1500*time.Millisecond || elapsed > 3*time.Second {
			t.Errorf("two hung reads took %s, want one 2-second budget rather than sequential budgets", elapsed)
		}
		assertIndependentAutomationStates(t, text, "not_running", "not_running")
	case <-time.After(3 * time.Second):
		t.Fatal("two hung IPC reads exceeded the shared 2-second status budget")
	}
}
