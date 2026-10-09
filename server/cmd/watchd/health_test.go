package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var _ func(context.Context, func() ([]vpnconfig.Subscription, error), watchdapi.Source, *notifications.Store) = publishSubscriptionHealth

func publisherTestSubscription() vpnconfig.Subscription {
	return vpnconfig.Subscription{
		ID: "0a1b2c3d", Name: "North", Added: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
		URL: "https://subscription.example.test/list?token=SUBSCRIPTION_URL_SECRET",
		Servers: []vpnconfig.Server{{
			Subscription: "0a1b2c3d", Name: "Primary", Address: "health.example.test", Port: 443,
			IPs: []string{"192.0.2.10", "192.0.2.11"}, UUID: "00000000-0000-0000-0000-000000000001",
			Security: "tls", SNI: "health.example.test",
		}},
	}
}

func publisherTestSnapshot(sub vpnconfig.Subscription, status watchdapi.Status) watchdapi.Snapshot {
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	snapshot := watchdapi.Snapshot{
		State: watchdapi.StateOK, UpdatedAt: at, IntervalSeconds: 60,
		Message: "RAW_MONITOR_ERROR_SECRET", Endpoints: make(map[string]watchdapi.EndpointState),
	}
	for _, server := range sub.Servers {
		for _, key := range endpoint.Keys(server) {
			snapshot.Endpoints[key] = watchdapi.EndpointState{
				Status: status, CheckedAt: at, Since: at, NextAt: at.Add(time.Minute),
				Error: "RAW_ENDPOINT_ERROR_SECRET",
			}
		}
	}
	return snapshot
}

type publisherHealthSource struct {
	mu       sync.Mutex
	snapshot watchdapi.Snapshot
}

func (s *publisherHealthSource) Snapshot() watchdapi.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.snapshot
	snapshot.Endpoints = make(map[string]watchdapi.EndpointState, len(s.snapshot.Endpoints))
	for key, state := range s.snapshot.Endpoints {
		snapshot.Endpoints[key] = state
	}
	return snapshot
}

func (*publisherHealthSource) Request([]string) (int, error) { return 0, watchdapi.ErrNotActive }

func (s *publisherHealthSource) set(snapshot watchdapi.Snapshot) {
	s.mu.Lock()
	s.snapshot = snapshot
	s.mu.Unlock()
}

func startHealthPublisher(t *testing.T, ctx context.Context, cancel context.CancelFunc, load func() ([]vpnconfig.Subscription, error), source watchdapi.Source, q *notifications.Store, release ...func()) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishSubscriptionHealth(ctx, load, source, q)
	}()
	t.Cleanup(func() {
		cancel()
		for _, finish := range release {
			finish()
		}
		await(t, done)
	})
	return done
}

func awaitHealthMessages(t *testing.T, q *notifications.Store, count int, wait time.Duration) []watchdapi.Notification {
	t.Helper()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := q.Pending("")
		if err != nil {
			t.Fatal("health Pending:", err)
		}
		if len(page.Messages) == count {
			return page.Messages
		}
		if len(page.Messages) > count {
			t.Fatalf("publisher duplicated a transition: %+v", page.Messages)
		}
		select {
		case <-deadline.C:
			t.Fatalf("publisher did not deliver %d messages within %s: %+v", count, wait, page.Messages)
		case <-ticker.C:
		}
	}
}

func publisherHealthRecords(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Health map[string]any `json:"health"`
	}
	if err := json.Unmarshal(raw, &document); err != nil || document.Health == nil {
		t.Fatalf("missing/invalid durable health section: %v", err)
	}
	return document.Health
}

func TestPublishSubscriptionHealth_ImmediateEveryTenSecondsAndCancellation(t *testing.T) {
	for _, failedSave := range []bool{false, true} {
		name := "durable"
		if failedSave {
			name = "failed_save"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "queue")
			path := filepath.Join(dir, "watchd-notifications.json")
			q := runtimeQueue(t, path)
			if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			if failedSave {
				if err := os.Rename(dir, dir+"-held"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, []byte("temporarily unavailable queue directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var logs runtimeLog
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			sub := publisherTestSubscription()
			source := &publisherHealthSource{snapshot: publisherTestSnapshot(sub, watchdapi.StatusDead)}
			var calls atomic.Int64
			loadedAt := make(chan time.Time, 8)
			load := func() ([]vpnconfig.Subscription, error) {
				calls.Add(1)
				select {
				case loadedAt <- time.Now():
				default:
				}
				return []vpnconfig.Subscription{sub}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			started := time.Now()
			done := startHealthPublisher(t, ctx, cancel, load, source, q)
			first := awaitHealthMessages(t, q, 1, 2*time.Second)[0]
			if first.Text != "Subscription North has no live servers" || first.At.Before(started) || first.At.After(time.Now()) {
				t.Fatalf("immediate health event = %+v", first)
			}
			source.set(publisherTestSnapshot(sub, watchdapi.StatusAlive))
			messages := awaitHealthMessages(t, q, 2, 12*time.Second)
			if messages[0].EventID != first.EventID || messages[1].EventID == first.EventID || messages[1].Text != "Subscription North has a live server again" {
				t.Fatalf("next health cycle did not produce one safe recovery: %+v", messages)
			}
			cancel()
			await(t, done)
			if calls.Load() != 2 {
				t.Fatalf("publisher loaded %d times for two scheduled cycles", calls.Load())
			}
			firstLoad, secondLoad := <-loadedAt, <-loadedAt
			if elapsed := secondLoad.Sub(firstLoad); elapsed < 9*time.Second || elapsed > 12*time.Second {
				t.Fatalf("health reload cadence = %s, want 10 seconds", elapsed)
			}
			if failedSave {
				if status := q.Status(); status.Pending != 2 || status.StorageError == "" {
					t.Fatalf("save error stopped publication or lost its diagnostic: %+v", status)
				}
				if !strings.Contains(strings.ToLower(logs.String()), "storage") {
					t.Fatal("publisher did not diagnose the failed health save")
				}
			} else {
				reopened := runtimeQueue(t, path)
				page, err := reopened.Pending("")
				if err != nil || len(page.Messages) != 2 {
					t.Fatalf("publisher events did not survive restart: page=%+v error=%v", page, err)
				}
				for i, message := range page.Messages {
					if message.ChatID != 100 || message.EventID != messages[i].EventID || message.Text != messages[i].Text || !message.At.Equal(messages[i].At) {
						t.Fatalf("restart changed health message %d: %+v, want %+v", i, message, messages[i])
					}
				}
			}
			for _, secret := range []string{"SUBSCRIPTION_URL_SECRET", "RAW_MONITOR_ERROR_SECRET", "RAW_ENDPOINT_ERROR_SECRET", sub.Servers[0].UUID} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("publisher diagnostic exposed %q", secret)
				}
			}
		})
	}
}

func TestPublishSubscriptionHealth_LoadFailureKeepsPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchd-notifications.json")
	q := runtimeQueue(t, path)
	if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	sub := publisherTestSubscription()
	if err := q.ObserveSubscriptions([]vpnconfig.Subscription{sub}, publisherTestSnapshot(sub, watchdapi.StatusDead)); err != nil {
		t.Fatal(err)
	}
	first := awaitHealthMessages(t, q, 1, time.Second)[0]
	before := publisherHealthRecords(t, path)
	source := &publisherHealthSource{snapshot: publisherTestSnapshot(sub, watchdapi.StatusAlive)}
	failedRead, recoveredRead, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	var calls atomic.Int64
	load := func() ([]vpnconfig.Subscription, error) {
		switch calls.Add(1) {
		case 1:
			close(failedRead)
			return []vpnconfig.Subscription{sub}, errors.New("https://subscription.example.test/private?token=LOAD_URL_SECRET RAW_PROVIDER_ERROR_SECRET")
		case 2:
			close(recoveredRead)
			<-gate
		}
		return []vpnconfig.Subscription{sub}, nil
	}
	var logs runtimeLog
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	ctx, cancel := context.WithCancel(context.Background())
	done := startHealthPublisher(t, ctx, cancel, load, source, q, release)
	await(t, failedRead)
	select {
	case <-recoveredRead:
	case <-time.After(12 * time.Second):
		t.Fatal("transient load error ended the publisher instead of retrying")
	}
	// The next load barrier proves the failed cycle has finished.
	page, err := q.Pending("")
	if err != nil || len(page.Messages) != 1 || page.Messages[0].EventID != first.EventID {
		t.Fatalf("failed load produced new events: page=%+v error=%v", page, err)
	}
	if after := publisherHealthRecords(t, path); !reflect.DeepEqual(after, before) {
		t.Fatalf("partial failed load changed previous health: before=%+v after=%+v", before, after)
	}
	release()
	messages := awaitHealthMessages(t, q, 2, 2*time.Second)
	if messages[0].EventID != first.EventID || messages[1].Text != "Subscription North has a live server again" {
		t.Fatalf("next successful load did not recover the preserved zero state: %+v", messages)
	}
	cancel()
	await(t, done)
	if after := publisherHealthRecords(t, path); reflect.DeepEqual(after, before) {
		t.Fatal("successful recovery did not persist the new health")
	}
	if diagnostic := logs.String(); diagnostic == "" || strings.Contains(diagnostic, "LOAD_URL_SECRET") || strings.Contains(diagnostic, "RAW_PROVIDER_ERROR_SECRET") {
		t.Fatalf("missing/unsafe load-failure diagnostic: %q", diagnostic)
	}
}

func TestPublishSubscriptionHealth_CancellationPreventsLatePublication(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "already_cancelled"
		if blocked {
			name = "cancel_during_load"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			q := runtimeQueue(t, path)
			if err := q.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			sub := publisherTestSubscription()
			source := &publisherHealthSource{snapshot: publisherTestSnapshot(sub, watchdapi.StatusDead)}
			entered, gate := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate) }) }
			var calls atomic.Int64
			load := func() ([]vpnconfig.Subscription, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				if blocked {
					<-gate
				}
				return []vpnconfig.Subscription{sub}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			if !blocked {
				cancel()
			}
			done := startHealthPublisher(t, ctx, cancel, load, source, q, release)
			if blocked {
				await(t, entered)
				cancel()
				release()
			}
			await(t, done)
			page, err := q.Pending("")
			if err != nil || len(page.Messages) != 0 || len(publisherHealthRecords(t, path)) != 0 {
				t.Fatalf("cancelled publisher created a late health/event intent: page=%+v error=%v", page, err)
			}
			if !blocked && calls.Load() != 0 {
				t.Fatal("already-cancelled publisher performed subscription I/O")
			}
		})
	}
}
