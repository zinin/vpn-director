package notifications

import (
	"sort"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

func (s *Store) ReplaceRecipients(recipients []watchdapi.Recipient) error {
	next := make(map[int64]watchdapi.Recipient, len(recipients))
	for _, recipient := range recipients {
		previous, exists := next[recipient.ChatID]
		if !exists || recipient.FirstSeen.Before(previous.FirstSeen) {
			next[recipient.ChatID] = recipient
		}
	}
	at := s.now()
	s.mu.Lock()
	s.pruneLocked(at)
	pending := make(map[int64][]storedEvent, len(next))
	for chatID, recipient := range next {
		seen := make(map[watchdapi.EventID]bool)
		merged := make([]storedEvent, 0, len(s.pending[chatID])+len(s.recent))
		for _, events := range [][]storedEvent{s.pending[chatID], s.recent} {
			for _, event := range events {
				_, closed := s.closed[chatID][event.EventID]
				if seen[event.EventID] || closed || event.At.Before(recipient.FirstSeen) {
					continue
				}
				seen[event.EventID] = true
				merged = append(merged, event)
			}
		}
		sort.Slice(merged, func(i, j int) bool {
			return eventSequence(merged[i].EventID) < eventSequence(merged[j].EventID)
		})
		queue := boundedEvents(merged, at, maxPerChat, nil)
		if len(queue) > 0 {
			pending[chatID] = queue
		}
	}
	changed := !sameRecipients(s.recipients, next) || !samePending(s.pending, pending)
	s.recipients, s.pending = next, pending
	if changed {
		s.revision++
	}
	s.mu.Unlock()
	return s.Flush()
}

func sameRecipients(a, b map[int64]watchdapi.Recipient) bool {
	if len(a) != len(b) {
		return false
	}
	for chatID, recipient := range a {
		other, exists := b[chatID]
		if !exists || recipient.ChatID != other.ChatID || !recipient.FirstSeen.Equal(other.FirstSeen) {
			return false
		}
	}
	return true
}

func samePending(a, b map[int64][]storedEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for chatID, events := range a {
		other, exists := b[chatID]
		if !exists || !sameEvents(events, other) {
			return false
		}
	}
	return true
}
