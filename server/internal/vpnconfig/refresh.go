package vpnconfig

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"
)

// RefreshMerge is a downloaded list merged with the stored one (MergeRefresh).
type RefreshMerge struct {
	Servers []Server
	// Added counts the listed servers without a pair, Removed the stored
	// servers without one, Renamed the pairs whose name changed and
	// Readdressed the pairs whose addresses did.
	Added, Removed, Renamed, Readdressed int
	// pairs[i] is the index in the stored list of listed server i's pair, or -1.
	pairs []int
}

// MergeRefresh merges listed, every server a periodic refresh downloaded in
// subscription order - without addresses where its host did not resolve -
// with stored, the list in the file. Each listed server pairs with the first
// stored server of the same ServerIdentity not yet paired and is then that
// stored record - its outbound, and so the stored REALITY picks - under the
// listed name: its endpoint keys, and so its monitor statuses, stay. Its
// addresses are the stored ones when the listed ones are the same set or
// there are none, the listed ones otherwise. A listed server without a pair
// comes in as it is, unless it has no address; a stored one without a pair
// leaves. The servers take the listed order.
func MergeRefresh(stored, listed []Server) RefreshMerge {
	m := RefreshMerge{pairs: pairServers(stored, listed)}
	paired := make([]bool, len(stored))
	for i, l := range listed {
		si := m.pairs[i]
		if si < 0 {
			if len(l.IPs) > 0 {
				m.Servers = append(m.Servers, l)
				m.Added++
			}
			continue
		}
		paired[si] = true
		s := stored[si]
		if l.Name != s.Name {
			s.Name = l.Name
			m.Renamed++
		}
		if len(l.IPs) > 0 && !sameIPSet(l.IPs, s.IPs) {
			s.IPs = l.IPs
			m.Readdressed++
		}
		m.Servers = append(m.Servers, s)
	}
	for _, p := range paired {
		if !p {
			m.Removed++
		}
	}
	return m
}

// sameIPSet reports whether a and b hold the same addresses, whatever their
// order: a resolver can answer the same set in another order every time.
func sameIPSet(a, b []string) bool {
	set := func(ips []string) []string {
		out := append([]string(nil), ips...)
		sort.Strings(out)
		return slices.Compact(out)
	}
	return slices.Equal(set(a), set(b))
}

// ErrNoServerResolved is a periodic refresh whose merge came out empty: the
// download held servers, and none of them resolved or paired with one the
// file has. The list stays.
var ErrNoServerResolved = errors.New("could not resolve IP for any server")

// RecordRename is a server record that followed its renamed server.
type RecordRename struct {
	Record   string // active_server, preferred_server or pending_restore.active
	From, To string
}

// RefreshResult is what one PublishRefresh came to, for the log.
type RefreshResult struct {
	// Wrote says the subscription file was written: its list changed, or its
	// error was cleared.
	Wrote bool
	// Count is the servers of the merged list; the rest are MergeRefresh's.
	Count, Added, Removed, Renamed, Readdressed int
	// Followed is every record that followed a renamed server.
	Followed []RecordRename
}

// PublishRefresh publishes a periodic refresh of subscription id, downloaded
// from rawURL, under update's lock. listed is every server the download
// lists, in its order, without addresses where its host did not resolve. The
// merge (MergeRefresh) runs against the file as it is under the lock, not
// against what the caller read before downloading: a manual refresh may have
// written in between. A merged list equal to the file's, with no error
// recorded, writes nothing at all - neither the file nor the config. Anything
// else writes the merged list, refreshed, a cleared error and xray.servers in
// one update, and the records that named a renamed server follow it
// (followRenames). ErrSubscriptionGone when the subscription was deleted or
// relinked meanwhile, ErrNoServerResolved when the merge comes out empty;
// neither writes. A failure after the file was written carries
// ErrServersSaved.
func PublishRefresh(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, listed []Server, now time.Time) (RefreshResult, error) {
	if rawURL == "" {
		return RefreshResult{}, ErrSubscriptionStatic
	}
	var res RefreshResult
	saved := false
	err := update(func(cfg *VPNDirectorConfig) error {
		res = RefreshResult{}
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		stored := subs[i].Servers
		m := MergeRefresh(stored, listed)
		res.Count, res.Added, res.Removed, res.Renamed, res.Readdressed = len(m.Servers), m.Added, m.Removed, m.Renamed, m.Readdressed
		if len(m.Servers) == 0 {
			return ErrNoServerResolved
		}
		if subs[i].Error == "" && reflect.DeepEqual(m.Servers, stored) {
			return errNothingToWrite
		}
		sub := subs[i]
		sub.Servers = m.Servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
			res.Followed = followRenames(cfg, id, stored, listed, m.pairs)
		}
		return nil
	})
	switch {
	case err == nil:
		res.Wrote = true
		return res, nil
	case errors.Is(err, errNothingToWrite):
		return res, nil
	case saved:
		res.Wrote, res.Followed = true, nil
		return res, ServersSaved(err)
	default:
		res.Followed = nil
		return res, err
	}
}

// followRenames points every record that names a server of subscription id
// the refresh renamed at its new name: active_server, preferred_server and
// pending_restore.active, together - a restore compares the first and the
// last whole, and found them different, would discard its intent as
// superseded by a new selection. A record names the first stored server with
// its name, address and port; pairs pairs fresh with stored (pairServers).
// Address and port belong to the identity, so only the name moves, and seq
// stays: a rename is no selection, and the watch must not take it for one. A
// record whose server left the list stays as it is.
func followRenames(cfg *VPNDirectorConfig, id string, stored, fresh []Server, pairs []int) []RecordRename {
	renamed := map[int]string{}
	for fi, si := range pairs {
		if si >= 0 && fresh[fi].Name != stored[si].Name {
			renamed[si] = fresh[fi].Name
		}
	}
	if len(renamed) == 0 {
		return nil
	}
	var out []RecordRename
	follow := func(record string, a *ActiveServer) {
		if a == nil || a.Subscription != id {
			return
		}
		for si, s := range stored {
			if s.Name != a.Name || s.Address != a.Address || s.Port != a.Port {
				continue
			}
			if name, ok := renamed[si]; ok {
				out = append(out, RecordRename{Record: record, From: a.Name, To: name})
				a.Name = name
			}
			return
		}
	}
	follow("active_server", cfg.Xray.ActiveServer)
	follow("preferred_server", cfg.Xray.PreferredServer)
	if p := cfg.Xray.PendingRestore; p != nil {
		follow("pending_restore.active", p.Active)
	}
	return out
}
