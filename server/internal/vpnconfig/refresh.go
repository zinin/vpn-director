package vpnconfig

import (
	"slices"
	"sort"
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
