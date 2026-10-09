package notifications

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

var _ interface {
	ObserveSubscriptions([]vpnconfig.Subscription, watchdapi.Snapshot) error
} = (*Store)(nil)

func healthTestSubscription() vpnconfig.Subscription {
	primary := vpnconfig.Server{
		Subscription: "0a1b2c3d", Name: "Primary", Address: "health.example.test", Port: 443,
		IPs: []string{"192.0.2.10", "192.0.2.11"}, UUID: "00000000-0000-0000-0000-000000000001",
		Security: "tls", SNI: "health.example.test",
	}
	alias := primary
	alias.Name = "Alias"
	return vpnconfig.Subscription{
		ID: "0a1b2c3d", Name: "North", Added: notificationTestTime(), Refreshed: notificationTestTime(),
		URL:   "https://subscription.example.test/list?token=SUBSCRIPTION_URL_SECRET",
		Error: "RAW_PROVIDER_ERROR_SECRET",
		Servers: []vpnconfig.Server{primary, alias, {
			Subscription: "0a1b2c3d", Name: "Other", Address: "other.example.test", Port: 8443,
			IPs: []string{"198.51.100.20"}, UUID: "00000000-0000-0000-0000-000000000002",
			Security: "tls", SNI: "other.example.test",
		}},
	}
}

func healthTestSnapshot(sub vpnconfig.Subscription, status watchdapi.Status) watchdapi.Snapshot {
	at := notificationTestTime()
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

type observedSubscriptionHealth struct {
	sequence uint64
	health   map[string]any
	recent   []storedEvent
}

func captureSubscriptionHealth(t *testing.T, s *Store) observedSubscriptionHealth {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	state := observedSubscriptionHealth{
		sequence: s.sequence, health: make(map[string]any, len(s.health)),
		recent: append([]storedEvent(nil), s.recent...),
	}
	for id, raw := range s.health {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("invalid health record for %q: %v", id, err)
		}
		state.health[id] = value
	}
	return state
}

func observeSubscriptionHealth(t *testing.T, s *Store, subs []vpnconfig.Subscription, snapshot watchdapi.Snapshot) {
	t.Helper()
	if err := s.ObserveSubscriptions(subs, snapshot); err != nil {
		t.Fatalf("ObserveSubscriptions: %v", err)
	}
}

func assertSubscriptionHealthMessages(t *testing.T, s *Store, want []string) []watchdapi.Notification {
	t.Helper()
	messages := notificationsForChat(pendingNotifications(t, s), 100)
	if len(messages) != len(want) {
		t.Fatalf("health messages = %+v, want texts %v", messages, want)
	}
	for i, message := range messages {
		if message.Text != want[i] || !message.At.Equal(notificationTestTime()) {
			t.Errorf("health message %d = %+v, want text %q and original timestamp", i, message, want[i])
		}
		splitNotificationID(t, message.EventID)
	}
	return messages
}

func TestSubscriptionHealth_CompleteTransitionsOnce(t *testing.T) {
	for _, status := range []watchdapi.Status{watchdapi.StatusDead, watchdapi.StatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			at := notificationTestTime()
			now := func() time.Time { return at }
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
			sub := healthTestSubscription()
			keys := endpoint.Keys(sub.Servers[0])
			if len(keys) != 2 || keys[0] == keys[1] || !reflect.DeepEqual(keys, endpoint.Keys(sub.Servers[1])) {
				t.Fatal("fixture must contain two addresses and duplicate endpoint aliases")
			}
			complete := healthTestSnapshot(sub, status)
			otherKey := endpoint.Keys(sub.Servers[2])[0]
			complete.Endpoints[otherKey] = watchdapi.EndpointState{Status: watchdapi.StatusRejected}
			incomplete := healthTestSnapshot(sub, status)
			delete(incomplete.Endpoints, keys[1])
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, incomplete)
			assertSubscriptionHealthMessages(t, s, nil)
			if state := captureSubscriptionHealth(t, s); len(state.health) != 0 {
				t.Fatalf("incomplete first scan established health: %+v", state.health)
			}

			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, complete)
			first := assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"})
			if state := captureSubscriptionHealth(t, s); len(state.health) != 1 || state.health[sub.ID] == nil {
				t.Fatalf("complete scan did not establish subscription-ID health: %+v", state.health)
			}
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, complete)
			sub.Name = "Renamed"
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, complete)
			s = newNotificationStore(t, path, now)
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, complete)
			assertNotificationIDs(t, assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"}), []watchdapi.EventID{first[0].EventID})

			alive := healthTestSnapshot(sub, watchdapi.StatusUnknown)
			delete(alive.Endpoints, keys[1])
			alive.Endpoints[keys[0]] = watchdapi.EndpointState{Status: watchdapi.StatusAlive, LatencyMS: 12}
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, alive)
			messages := assertSubscriptionHealthMessages(t, s, []string{
				"Subscription North has no live servers", "Subscription Renamed has a live server again",
			})
			firstEpoch, firstSequence := splitNotificationID(t, first[0].EventID)
			nextEpoch, nextSequence := splitNotificationID(t, messages[1].EventID)
			if nextEpoch != firstEpoch || nextSequence <= firstSequence {
				t.Fatal("recovery reset or reused the persisted event identity")
			}
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, alive)
			s = newNotificationStore(t, path, now)
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, alive)
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{messages[0].EventID, messages[1].EventID})
		})
	}

	t.Run("initial_alive_is_silent_and_refresh_uses_current_keys", func(t *testing.T) {
		at := notificationTestTime()
		s := newNotificationStore(t, filepath.Join(t.TempDir(), "watchd-notifications.json"), func() time.Time { return at })
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		sub := healthTestSubscription()
		old := healthTestSnapshot(sub, watchdapi.StatusAlive)
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, old)
		assertSubscriptionHealthMessages(t, s, nil)
		refreshed := sub
		refreshed.Servers = []vpnconfig.Server{sub.Servers[0]}
		refreshed.Servers[0].IPs = []string{"203.0.113.30", "203.0.113.31"}
		current := healthTestSnapshot(refreshed, watchdapi.StatusDead)
		for key, state := range old.Endpoints {
			current.Endpoints[key] = state
		}
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{refreshed}, current)
		assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"})
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{refreshed}, old)
		assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"})
		current = healthTestSnapshot(refreshed, watchdapi.StatusUnknown)
		current.Endpoints[endpoint.Keys(refreshed.Servers[0])[1]] = watchdapi.EndpointState{Status: watchdapi.StatusAlive}
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{refreshed}, current)
		assertSubscriptionHealthMessages(t, s, []string{
			"Subscription North has no live servers", "Subscription North has a live server again",
		})
	})

	t.Run("concurrent_complete_scans_share_one_transition", func(t *testing.T) {
		at := notificationTestTime()
		s := newNotificationStore(t, filepath.Join(t.TempDir(), "watchd-notifications.json"), func() time.Time { return at })
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		sub := healthTestSubscription()
		snapshot := healthTestSnapshot(sub, watchdapi.StatusRejected)
		before := captureSubscriptionHealth(t, s)
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.ObserveSubscriptions([]vpnconfig.Subscription{sub}, snapshot)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error("concurrent ObserveSubscriptions:", err)
			}
		}
		assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"})
		if after := captureSubscriptionHealth(t, s); after.sequence != before.sequence+1 || len(after.recent) != 1 {
			t.Fatalf("concurrent scans advanced more than one intent: %+v", after)
		}
	})
}

func TestSubscriptionHealth_UnknownAndInfrastructureFreeze(t *testing.T) {
	type freezeCase struct {
		name   string
		change func(*vpnconfig.Subscription, *watchdapi.Snapshot)
		global bool
	}
	var cases []freezeCase
	for _, state := range []watchdapi.State{
		watchdapi.StateStopped, watchdapi.StateDisabled, watchdapi.StateNoXray,
		watchdapi.StateWANDown, watchdapi.StateProberError, watchdapi.StateNotRunning,
		"", "future_state",
	} {
		cases = append(cases, freezeCase{
			name: "monitor_" + string(state), global: true,
			change: func(_ *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) { snapshot.State = state },
		})
	}
	cases = append(cases,
		freezeCase{name: "empty_subscription", change: func(sub *vpnconfig.Subscription, _ *watchdapi.Snapshot) { sub.Servers = nil }},
		freezeCase{name: "unknown_address", change: func(sub *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) {
			snapshot.Endpoints[endpoint.Keys(sub.Servers[0])[1]] = watchdapi.EndpointState{Status: watchdapi.StatusUnknown}
		}},
		freezeCase{name: "unchecked_address", change: func(sub *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) {
			snapshot.Endpoints[endpoint.Keys(sub.Servers[0])[1]] = watchdapi.EndpointState{}
		}},
		freezeCase{name: "unrecognized_address_status", change: func(sub *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) {
			snapshot.Endpoints[endpoint.Keys(sub.Servers[0])[1]] = watchdapi.EndpointState{Status: "future_status"}
		}},
		freezeCase{name: "missing_address", change: func(sub *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) {
			delete(snapshot.Endpoints, endpoint.Keys(sub.Servers[0])[1])
		}},
		freezeCase{name: "missing_snapshot", change: func(_ *vpnconfig.Subscription, snapshot *watchdapi.Snapshot) { snapshot.Endpoints = nil }},
		freezeCase{name: "refresh_new_unknown_key", change: func(sub *vpnconfig.Subscription, _ *watchdapi.Snapshot) {
			sub.Servers = append(append([]vpnconfig.Server(nil), sub.Servers...), vpnconfig.Server{
				Subscription: sub.ID, Name: "New", Address: "new.example.test", Port: 443, IPs: []string{"203.0.113.30"},
			})
		}},
	)
	for _, previousZero := range []bool{false, true} {
		for _, test := range cases {
			t.Run(fmt.Sprintf("previous_zero_%v/%s", previousZero, test.name), func(t *testing.T) {
				at := notificationTestTime()
				now := func() time.Time { return at }
				path := filepath.Join(t.TempDir(), "watchd-notifications.json")
				s := newNotificationStore(t, path, now)
				replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
				sub := healthTestSubscription()
				initial := watchdapi.StatusAlive
				var want []string
				if previousZero {
					initial = watchdapi.StatusDead
					want = []string{"Subscription North has no live servers"}
				}
				observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, healthTestSnapshot(sub, initial))
				before := captureSubscriptionHealth(t, s)
				if len(before.health) != 1 || before.health[sub.ID] == nil {
					t.Fatal("fixture must establish a determined previous health record")
				}
				frozenSub := sub
				frozen := healthTestSnapshot(sub, watchdapi.StatusDead)
				if test.global && previousZero {
					frozen = healthTestSnapshot(sub, watchdapi.StatusAlive)
				}
				test.change(&frozenSub, &frozen)
				for i := 0; i < 2; i++ {
					observeSubscriptionHealth(t, s, []vpnconfig.Subscription{frozenSub}, frozen)
				}
				after := captureSubscriptionHealth(t, s)
				if after.sequence != before.sequence || !reflect.DeepEqual(after.health, before.health) || !reflect.DeepEqual(after.recent, before.recent) {
					t.Fatalf("indeterminate observation changed the previous intent: before=%+v after=%+v", before, after)
				}
				assertSubscriptionHealthMessages(t, s, want)
				s = newNotificationStore(t, path, now)
				observeSubscriptionHealth(t, s, []vpnconfig.Subscription{frozenSub}, frozen)
				if restored := captureSubscriptionHealth(t, s); !reflect.DeepEqual(restored.health, before.health) {
					t.Fatalf("freeze did not survive restart: %+v", restored.health)
				}
				assertSubscriptionHealthMessages(t, s, want)
				next := watchdapi.StatusDead
				if previousZero {
					next = watchdapi.StatusAlive
					want = append(want, "Subscription North has a live server again")
				} else {
					want = []string{"Subscription North has no live servers"}
				}
				observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, healthTestSnapshot(sub, next))
				assertSubscriptionHealthMessages(t, s, want)
			})
		}
	}
}

func TestSubscriptionHealth_WholeBatchLeaseBoundary(t *testing.T) {
	for _, stage := range []string{"success", "write", "file_sync", "rename", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			at := notificationTestTime()
			now := func() time.Time { return at }
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
			north := healthTestSubscription()
			south := north
			south.ID, south.Name = "1b2c3d4e", "South"
			south.Servers = append([]vpnconfig.Server(nil), north.Servers...)
			for i := range south.Servers {
				south.Servers[i].Subscription = south.ID
			}
			subs := []vpnconfig.Subscription{north, south}
			observeSubscriptionHealth(t, s, subs, healthTestSnapshot(north, watchdapi.StatusAlive))
			var initial savedState
			if err := json.Unmarshal(readNotificationFile(t, path), &initial); err != nil {
				t.Fatal(err)
			}
			// The private store fixture has one leased ID for a two-event batch.
			initial.ReservedThrough = initial.Sequence + 1
			data, err := json.Marshal(initial)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.reservedThrough = initial.ReservedThrough
			beforeRevision, beforeSaved := s.revision, s.savedRevision
			s.mu.Unlock()
			before := captureSubscriptionHealth(t, s)
			durableBefore := readNotificationFile(t, path)
			if before.sequence != 0 || len(before.health) != 2 || len(before.recent) != 0 || beforeRevision != beforeSaved {
				t.Fatalf("lease boundary fixture is not a clean, event-free pair of live subscriptions: %+v", before)
			}
			assertReservation := func(state savedState) {
				t.Helper()
				if state.Version != 1 || state.Epoch != initial.Epoch || state.Sequence != initial.Sequence ||
					state.ReservedThrough < initial.Sequence+2 || len(state.Recent) != 0 || len(state.Pending) != 0 ||
					!reflect.DeepEqual(state.Health, initial.Health) || !reflect.DeepEqual(state.Recipients, initial.Recipients) ||
					!reflect.DeepEqual(state.Closed, initial.Closed) {
					t.Fatalf("preliminary lease save contains partial health/event intent: %+v", state)
				}
			}
			originalIO := s.io
			t.Cleanup(func() { s.io = originalIO })
			var writes []savedState
			recordWrites := func() {
				write := s.io.write
				s.io.write = func(file *os.File, data []byte) (int, error) {
					var state savedState
					if err := json.Unmarshal(data, &state); err != nil {
						t.Errorf("invalid staged lease/batch document: %v", err)
					}
					writes = append(writes, state)
					return write(file, data)
				}
			}
			restore := func() {}
			if stage != "success" {
				restore = failNotificationIO(t, s, stage)
			}
			recordWrites()
			dead := healthTestSnapshot(north, watchdapi.StatusDead)

			err = s.ObserveSubscriptions(subs, dead)

			if stage != "success" {
				if err == nil || len(writes) != 1 {
					t.Fatalf("reservation %s failure error=%v saves=%d, want an error and only one reservation attempt", stage, err, len(writes))
				}
				assertReservation(writes[0])
				if after := captureSubscriptionHealth(t, s); !reflect.DeepEqual(after, before) {
					t.Fatalf("failed reservation composed partial RAM health/event/sequence: before=%+v after=%+v", before, after)
				}
				s.mu.Lock()
				reserved, revision, saved := s.reservedThrough, s.revision, s.savedRevision
				s.mu.Unlock()
				if reserved != initial.ReservedThrough || revision != beforeRevision || saved != beforeSaved {
					t.Fatalf("failed reservation committed a RAM lease/revision: lease=%d revision=%d saved=%d", reserved, revision, saved)
				}
				if status := s.Status(); status.Pending != 0 || status.StorageError == "" || len(pendingNotifications(t, s)) != 0 {
					t.Fatalf("failed reservation lost its diagnostic or queued a partial batch: %+v", status)
				}
				if stage != "directory_sync" {
					if !bytes.Equal(readNotificationFile(t, path), durableBefore) {
						t.Fatal("pre-rename reservation failure changed durable bytes")
					}
				} else {
					var reservedOnly savedState
					if err := json.Unmarshal(readNotificationFile(t, path), &reservedOnly); err != nil {
						t.Fatal(err)
					}
					assertReservation(reservedOnly)
					if !reflect.DeepEqual(reservedOnly, writes[0]) {
						t.Fatal("post-rename sync failure exposed something other than the complete reservation-only document")
					}
				}
				assertNoNotificationTempFiles(t, path)
				restore()
				recordWrites()
				err = s.ObserveSubscriptions(subs, dead)
			}
			if err != nil {
				t.Fatal("whole-batch lease/retry:", err)
			}
			offset := 0
			if stage != "success" {
				offset = 1
			}
			if len(writes) != offset+2 {
				t.Fatalf("successful boundary/retry wrote %d documents, want reservation-only followed by one full batch after %d failed attempt", len(writes), offset)
			}
			assertReservation(writes[offset])
			full := writes[offset+1]
			wantHealth := map[string]json.RawMessage{
				north.ID: json.RawMessage(`{"available":false}`), south.ID: json.RawMessage(`{"available":false}`),
			}
			if full.Sequence != initial.Sequence+2 || full.ReservedThrough < full.Sequence || len(full.Recent) != 2 ||
				len(full.Pending) != 1 || len(full.Pending[100]) != 2 || !reflect.DeepEqual(full.Health, wantHealth) ||
				!reflect.DeepEqual(full.Recipients, initial.Recipients) || !reflect.DeepEqual(full.Closed, initial.Closed) {
				t.Fatalf("full save did not atomically compose both health transitions and deliveries: %+v", full)
			}
			messages := assertSubscriptionHealthMessages(t, s, []string{
				"Subscription North has no live servers", "Subscription South has no live servers",
			})
			var ids []watchdapi.EventID
			for i, message := range messages {
				epoch, sequence := splitNotificationID(t, message.EventID)
				if epoch != initial.Epoch || sequence != initial.Sequence+uint64(i)+1 || full.Recent[i].EventID != message.EventID ||
					full.Recent[i].Text != message.Text || !full.Recent[i].At.Equal(at) || !reflect.DeepEqual(full.Pending[100][i], full.Recent[i]) {
					t.Fatalf("full batch event %d lost its unique sequence, timestamp or per-chat delivery: %+v", i, message)
				}
				ids = append(ids, message.EventID)
			}
			if ids[0] == ids[1] || s.Status().StorageError != "" {
				t.Fatal("recovered batch reused an ID or retained a failed-save diagnostic")
			}
			var durable savedState
			if err := json.Unmarshal(readNotificationFile(t, path), &durable); err != nil || !reflect.DeepEqual(durable, full) {
				t.Fatalf("durable store does not contain the whole final batch: %v", err)
			}
			observeSubscriptionHealth(t, s, subs, dead)
			if len(writes) != offset+2 {
				t.Fatal("repeated successful observation rewrote or duplicated the confirmed batch")
			}
			assertNoNotificationTempFiles(t, path)
			reopened := newNotificationStore(t, path, now)
			observeSubscriptionHealth(t, reopened, subs, dead)
			assertNotificationIDs(t, pendingNotifications(t, reopened), ids)
			if after := captureSubscriptionHealth(t, reopened); !reflect.DeepEqual(after.health, captureSubscriptionHealth(t, s).health) || reopened.Status().StorageError != "" {
				t.Fatalf("restart lost confirmed batch health or revived the diagnostic: %+v", after)
			}
		})
	}
}

func TestSubscriptionHealth_AtomicFailureAndDelete(t *testing.T) {
	for _, stage := range []string{"write", "file_sync", "rename", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			at := notificationTestTime()
			now := func() time.Time { return at }
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s,
				watchdapi.Recipient{ChatID: 100, FirstSeen: at}, watchdapi.Recipient{ChatID: 200, FirstSeen: at},
			)
			sub := healthTestSubscription()
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, healthTestSnapshot(sub, watchdapi.StatusAlive))
			before := captureSubscriptionHealth(t, s)
			durableBefore := readNotificationFile(t, path)
			restore := failNotificationIO(t, s, stage)
			dead := healthTestSnapshot(sub, watchdapi.StatusDead)
			if err := s.ObserveSubscriptions([]vpnconfig.Subscription{sub}, dead); err == nil {
				t.Fatalf("ObserveSubscriptions hid %s failure", stage)
			}
			dirty := captureSubscriptionHealth(t, s)
			if dirty.sequence != before.sequence+1 || len(dirty.recent) != 1 || reflect.DeepEqual(dirty.health, before.health) || dirty.health[sub.ID] == nil {
				t.Fatalf("failed save did not retain one RAM health/event/sequence intent: before=%+v after=%+v", before, dirty)
			}
			first := assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"})[0]
			assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 200), []watchdapi.EventID{first.EventID})
			if status := s.Status(); status.Pending != 2 || status.StorageError == "" {
				t.Fatalf("failed health save lost RAM delivery or diagnostic: %+v", status)
			}
			if stage != "directory_sync" && !bytes.Equal(readNotificationFile(t, path), durableBefore) {
				t.Fatal("pre-rename failure replaced the previous durable intent")
			}
			if stage == "directory_sync" {
				var document savedState
				if err := json.Unmarshal(readNotificationFile(t, path), &document); err != nil {
					t.Fatal(err)
				}
				if document.Sequence != dirty.sequence || len(document.Health) != 1 || len(document.Recent) != 1 || len(document.Pending[100]) != 1 || document.Pending[100][0].EventID != first.EventID {
					t.Fatalf("rename exposed only part of the health/event/sequence intent: %+v", document)
				}
			}
			_ = s.ObserveSubscriptions([]vpnconfig.Subscription{sub}, dead)
			if again := captureSubscriptionHealth(t, s); !reflect.DeepEqual(again, dirty) {
				t.Fatalf("repeat of unsaved health duplicated or rolled back RAM intent: %+v", again)
			}
			if err := s.Flush(); err == nil || s.Status().StorageError == "" {
				t.Fatal("failed retry cleared health intent's dirty diagnostic")
			}
			assertNoNotificationTempFiles(t, path)
			restore()
			if err := s.Flush(); err != nil {
				t.Fatal("recovered health flush:", err)
			}
			s = newNotificationStore(t, path, now)
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, dead)
			assertNotificationIDs(t, assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers"}), []watchdapi.EventID{first.EventID})
			if recovered := captureSubscriptionHealth(t, s); !reflect.DeepEqual(recovered.health, dirty.health) || s.Status().StorageError != "" {
				t.Fatalf("successful flush/restart lost the confirmed health: %+v", recovered)
			}
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub}, healthTestSnapshot(sub, watchdapi.StatusAlive))
			assertSubscriptionHealthMessages(t, s, []string{
				"Subscription North has no live servers", "Subscription North has a live server again",
			})
		})
	}

	t.Run("one_save_contains_all_health_events_and_sequence", func(t *testing.T) {
		at := notificationTestTime()
		now := func() time.Time { return at }
		path := filepath.Join(t.TempDir(), "watchd-notifications.json")
		s := newNotificationStore(t, path, now)
		replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 100, FirstSeen: at})
		sub := healthTestSubscription()
		other := sub
		other.ID = "1b2c3d4e"
		other.Servers = append([]vpnconfig.Server(nil), sub.Servers...)
		for i := range other.Servers {
			other.Servers[i].Subscription = other.ID
		}
		before := captureSubscriptionHealth(t, s)
		original := s
		write := original.io.write
		var writes []savedState
		original.io.write = func(file *os.File, data []byte) (int, error) {
			var state savedState
			if err := json.Unmarshal(data, &state); err != nil {
				t.Error("invalid staged health document:", err)
			}
			writes = append(writes, state)
			return write(file, data)
		}
		t.Cleanup(func() { original.io.write = write })
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub, other}, healthTestSnapshot(sub, watchdapi.StatusDead))
		if len(writes) != 1 {
			t.Fatalf("one observation wrote %d documents, want one atomic intent", len(writes))
		}
		state := writes[0]
		if state.Sequence != before.sequence+2 || len(state.Health) != 2 || len(state.Health[sub.ID]) == 0 || len(state.Health[other.ID]) == 0 || len(state.Recent) != 2 || len(state.Pending[100]) != 2 {
			t.Fatalf("one save did not compose both subscription IDs and queues: %+v", state)
		}
		messages := assertSubscriptionHealthMessages(t, s, []string{"Subscription North has no live servers", "Subscription North has no live servers"})
		if messages[0].EventID == messages[1].EventID {
			t.Fatal("different subscription IDs sharing keys/name reused a transition ID")
		}
		s = newNotificationStore(t, path, now)
		observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub, other}, healthTestSnapshot(sub, watchdapi.StatusDead))
		assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{messages[0].EventID, messages[1].EventID})
	})

	for _, finish := range []string{"ack", "ttl"} {
		t.Run("delete_keeps_history_until_"+finish, func(t *testing.T) {
			at := notificationTestTime()
			clock := at
			now := func() time.Time { return clock }
			path := filepath.Join(t.TempDir(), "watchd-notifications.json")
			s := newNotificationStore(t, path, now)
			replaceNotificationRecipients(t, s,
				watchdapi.Recipient{ChatID: 100, FirstSeen: at}, watchdapi.Recipient{ChatID: 200, FirstSeen: at},
			)
			sub := healthTestSubscription()
			keep := sub
			keep.ID, keep.Name = "1b2c3d4e", "Kept"
			keep.Servers = append([]vpnconfig.Server(nil), sub.Servers...)
			for i := range keep.Servers {
				keep.Servers[i].Subscription = keep.ID
			}
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{sub, keep}, healthTestSnapshot(sub, watchdapi.StatusDead))
			messages := notificationsForChat(pendingNotifications(t, s), 100)
			if len(messages) != 2 {
				t.Fatalf("two subscription IDs did not publish their own events: %+v", messages)
			}
			var deletedID, retainedID watchdapi.EventID
			for _, message := range messages {
				splitNotificationID(t, message.EventID)
				if !message.At.Equal(at) {
					t.Fatalf("health message lost its original timestamp: %+v", message)
				}
				switch message.Text {
				case "Subscription North has no live servers":
					deletedID = message.EventID
				case "Subscription Kept has no live servers":
					retainedID = message.EventID
				default:
					t.Fatalf("unsafe/unexpected health message: %+v", message)
				}
			}
			if deletedID == "" || retainedID == "" || deletedID == retainedID {
				t.Fatalf("distinct subscription histories were merged: %+v", messages)
			}
			ids := []watchdapi.EventID{messages[0].EventID, messages[1].EventID}
			keptHealth := captureSubscriptionHealth(t, s).health[keep.ID]
			observeSubscriptionHealth(t, s, []vpnconfig.Subscription{keep}, healthTestSnapshot(keep, watchdapi.StatusDead))
			if state := captureSubscriptionHealth(t, s); len(state.health) != 1 || !reflect.DeepEqual(state.health[keep.ID], keptHealth) {
				t.Fatalf("deletion did not remove only the deleted ID: %+v", state.health)
			}
			observeSubscriptionHealth(t, s, nil, watchdapi.Snapshot{State: watchdapi.StateOK})
			s = newNotificationStore(t, path, now)
			if state := captureSubscriptionHealth(t, s); len(state.health) != 0 {
				t.Fatalf("deleted health returned on restart: %+v", state.health)
			}
			assertNotificationIDs(t, pendingNotifications(t, s), []watchdapi.EventID{ids[0], ids[1], ids[0], ids[1]})
			if historical := notificationsForChat(pendingNotifications(t, s), 100); !reflect.DeepEqual(historical, messages) {
				t.Fatalf("deletion/restart rewrote safe historical messages: %+v", historical)
			}
			if finish == "ack" {
				if err := s.Ack(100, deletedID); err != nil {
					t.Fatal(err)
				}
				s = newNotificationStore(t, path, now)
				assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 100), []watchdapi.EventID{retainedID})
				assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 200), ids)
			} else {
				clock = at.Add(12 * time.Hour)
				assertNotificationIDs(t, notificationsForChat(pendingNotifications(t, s), 100), ids)
				clock = clock.Add(time.Nanosecond)
				if messages := pendingNotifications(t, s); len(messages) != 0 {
					t.Fatalf("deleted subscription history exceeded 12-hour TTL: %+v", messages)
				}
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
				s = newNotificationStore(t, path, now)
				replaceNotificationRecipients(t, s, watchdapi.Recipient{ChatID: 300, FirstSeen: at})
				if messages := pendingNotifications(t, s); len(messages) != 0 {
					t.Fatalf("expired deleted history returned through recent backfill: %+v", messages)
				}
			}
		})
	}
}
