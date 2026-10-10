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
// with stored, the list in the file. Each listed server pairs (pairServers)
// with the first stored server not yet paired of the same ServerIdentity or,
// failing that, of the same looseIdentity under the same name, and is then
// that stored record - its outbound, and so the stored REALITY picks and
// generated ws or httpupgrade Host and path - under the listed name: its
// endpoint keys, and so its monitor statuses, stay. Its
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

// ErrRefreshSuperseded is a periodic refresh whose subscription another
// refresh wrote while it downloaded - a manual refresh, an add of the saved
// link or the wave - and so moved its refreshed. That list is newer than the
// download, which would take back what it brought: the publication writes
// nothing, and the next round takes the newer list up.
var ErrRefreshSuperseded = errors.New("a newer refresh wrote the list while this one downloaded")

// RecordRename is a server record that followed its renamed server.
type RecordRename struct {
	Record   string // active_server, preferred_server or pending_restore.active
	From, To string
	// Address and Port are the record's, which the rename left as they were.
	// The watch names the active server by its subscription, name, address
	// and port, and needs all four to move what it remembers of it along.
	Address string
	Port    int
}

// RefreshResult is what one PublishRefresh came to, for the log.
type RefreshResult struct {
	// Wrote says the subscription file was written: its list changed, or its
	// error was cleared.
	Wrote bool
	// Cleared says the publication cleared an error the file recorded - the
	// file as it was under the lock, where a refresh that failed - a manual
	// one, the wave's or the shell's - may have recorded one since the caller
	// read the subscription.
	Cleared bool
	// Count is the servers of the merged list; the rest are MergeRefresh's.
	Count, Added, Removed, Renamed, Readdressed int
	// Followed is every record that followed a renamed server.
	Followed []RecordRename
}

// PublishRefresh publishes a periodic refresh of subscription id, downloaded
// from rawURL, under update's lock. since is the subscription's refreshed as
// the caller read it before downloading, and listed every server the download
// lists, in its order, without addresses where its host did not resolve. A
// file whose refreshed is no longer since holds a list another refresh wrote
// in between, newer than the download: nothing is written
// (ErrRefreshSuperseded), as RecordSubscriptionError writes no error then.
// Otherwise the merge (MergeRefresh) runs against the file as it is under the
// lock, not against what the caller read: an error recorded meanwhile, or a
// new name of the subscription, leaves refreshed as it was, and the
// publication clears that error and keeps that name. A merged list equal to
// the file's, with no error recorded, writes nothing at all - neither the file
// nor the config. Anything else writes the merged list, refreshed, a cleared
// error and xray.servers in one update, and the records that named a renamed
// server follow it (followRenames). ErrSubscriptionGone when the subscription
// was deleted or relinked meanwhile, ErrNoServerResolved when the merge comes
// out empty; none of the three writes. A config write that fails after the
// file was written puts the file back as it was (restoreSubscription): the
// next round finds the change again and writes the file and the config
// together. Only when the file cannot be put back - another writer wrote it
// since, or its own write fails - does the failure carry ErrServersSaved.
func PublishRefresh(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, since time.Time, listed []Server, now time.Time) (RefreshResult, error) {
	if rawURL == "" {
		return RefreshResult{}, ErrSubscriptionStatic
	}
	var res RefreshResult
	var before, written Subscription
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
		// Every writer of a list moves refreshed: one moved since the caller
		// read it is a list newer than this download.
		if !subs[i].Refreshed.Equal(since) {
			return ErrRefreshSuperseded
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
		res.Cleared = subs[i].Error != ""
		before = subs[i]
		sub := subs[i]
		sub.Servers = m.Servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		written = sub
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
		res.Followed = nil
		if restoreSubscription(update, files, before, written) {
			return res, err
		}
		res.Wrote = true
		return res, ServersSaved(err)
	default:
		res.Followed = nil
		return res, err
	}
}

// restoreSubscription puts before back in the file of the subscription a
// publication wrote as written, after the config write of that publication
// failed. Left as written, the file would read as unchanged to every later
// round, and xray.servers and the records that named a renamed server would
// stay behind for good. It takes the lock again through update, under the
// same guard, and writes no config. A file another writer has written since
// stands: the subscription is gone or relinked, or its name, refreshed or
// error is no longer the one written - a rename keeps refreshed and error. It
// reports whether before was saved.
func restoreSubscription(update ConfigUpdate, files SubscriptionFiles, before, written Subscription) bool {
	put := false
	_ = update(func(*VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, written.ID)
		if i < 0 || subs[i].URL != written.URL || subs[i].Name != written.Name ||
			!subs[i].Refreshed.Equal(written.Refreshed) || subs[i].Error != "" {
			return errNothingToWrite
		}
		if err := files.Save(before); err != nil {
			return err
		}
		put = true
		return errNothingToWrite
	})
	return put
}

// followRenames points every record that names a server of subscription id
// the refresh renamed at its new name: active_server, preferred_server and
// pending_restore.active, together - a restore compares active_server with
// pending_restore.active whole, and one that found them different would
// discard its intent as superseded by a new selection. A record names the
// first stored server with its name, address and port; pairs pairs fresh with
// stored (pairServers). Address and port belong to the identity, so only the
// name moves, and seq stays: a rename is no selection, and the watch must not
// take it for one. A record whose server left the list stays as it is.
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
				out = append(out, RecordRename{Record: record, From: a.Name, To: name, Address: a.Address, Port: a.Port})
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
