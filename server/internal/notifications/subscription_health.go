package notifications

import (
	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type subscriptionHealthObservation struct {
	id        string
	name      string
	available bool
	known     bool
}

type subscriptionHealthUpdate struct {
	id        string
	available bool
	text      string
}

// ObserveSubscriptions persists confirmed health and its events as one intent.
func (s *Store) ObserveSubscriptions(subs []vpnconfig.Subscription, snapshot watchdapi.Snapshot) error {
	if err := s.ensureEpoch(); err != nil {
		return err
	}
	current := make(map[string]bool, len(subs))
	observations := make([]subscriptionHealthObservation, 0, len(subs))
	for _, sub := range subs {
		if sub.ID == "" || current[sub.ID] {
			continue
		}
		current[sub.ID] = true
		available, known := confirmedSubscriptionHealth(sub, snapshot)
		observations = append(observations, subscriptionHealthObservation{
			id: sub.ID, name: sub.Name, available: available, known: known,
		})
	}
	at := s.now()
	for {
		s.mu.Lock()
		updates := make([]subscriptionHealthUpdate, 0, len(observations))
		var events uint64
		for _, observation := range observations {
			if !observation.known {
				continue
			}
			previous, known := decodeSubscriptionHealth(s.health[observation.id])
			if known && previous == observation.available {
				continue
			}
			update := subscriptionHealthUpdate{id: observation.id, available: observation.available}
			if !observation.available {
				update.text = "Subscription " + observation.name + " has no live servers"
			} else if known {
				update.text = "Subscription " + observation.name + " has a live server again"
			}
			if update.text != "" {
				events++
			}
			updates = append(updates, update)
		}
		if events > ^uint64(0)-s.sequence {
			s.mu.Unlock()
			return errSequence
		}
		// Lease the whole batch before changing health or appending events.
		if events > 0 && (s.sequence >= s.reservedThrough || events > s.reservedThrough-s.sequence) {
			s.mu.Unlock()
			if err := s.flushForEvents(events); err != nil {
				return err
			}
			continue
		}
		for id := range s.health {
			if !current[id] {
				delete(s.health, id)
				s.revision++
			}
		}
		for _, update := range updates {
			s.health[update.id] = encodeSubscriptionHealth(update.available)
			s.revision++
			if update.text != "" {
				s.appendEventLocked(update.text, at)
			}
		}
		s.mu.Unlock()
		return s.Flush()
	}
}

func confirmedSubscriptionHealth(sub vpnconfig.Subscription, snapshot watchdapi.Snapshot) (available, known bool) {
	if snapshot.State != watchdapi.StateOK || len(sub.Servers) == 0 {
		return false, false
	}
	complete := true
	for _, server := range sub.Servers {
		switch watchdapi.Health(endpoint.Keys(server), snapshot).Status {
		case watchdapi.StatusAlive:
			return true, true
		case watchdapi.StatusDead, watchdapi.StatusRejected:
		default:
			complete = false
		}
	}
	return false, complete
}
