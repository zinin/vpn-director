package subwatch

import "github.com/zinin/vpn-director/server/internal/vpnconfig"

// actuality is how the server active_server names stands against its
// subscription's list as it is now (spec 4.2).
type actuality uint8

const (
	// actualUnknown is no record, or one without a subscription or a name:
	// nothing to keep.
	actualUnknown actuality = iota
	// actualCurrent is an entry of the record's choice - its name and kind -
	// at its address and port with the record's mark, or any such entry when
	// the record has no mark; for a record without an address and a port, any
	// entry of its choice.
	actualCurrent
	// actualChanged is entries of the record's choice at its address and port,
	// all with another mark: new keys or a new UUID behind the same name and
	// address.
	actualChanged
	// actualMoved is no such entry, and exactly one entry of the choice
	// elsewhere.
	actualMoved
	// actualGone is neither: the subscription deleted, no entry of the choice,
	// or several - the name and the kind no longer tell which server the user
	// chose.
	actualGone
)

// classifyActive compares a with its subscription's list in subs, and returns
// the entry a is to follow when the server changed in place or moved. Only an
// entry of a's choice counts (vpnconfig.SameChoice: its subscription, name and
// kind): a panel can give one name to several inbounds, and one of another
// kind is another server, even at a's address and port. Several entries of the
// choice can share an address and port - a panel's inbounds behind one name -
// and the Web UI selects by index, so a can name any of them: the server
// changed only when none of them carries a's mark, and the first is then
// followed. A record without a mark - configure.sh's, an older daemon's - is
// current while an entry of its choice at its address and port is listed; one
// without an address and a port while an entry of its choice is. A record
// without a group knows no kind and goes by subscription and name.
func classifyActive(subs []vpnconfig.Subscription, a *vpnconfig.ActiveServer) (actuality, vpnconfig.Server) {
	if a == nil || a.Subscription == "" || a.Name == "" {
		return actualUnknown, vpnconfig.Server{}
	}
	i := vpnconfig.FindSubscription(subs, a.Subscription)
	if i < 0 {
		return actualGone, vpnconfig.Server{}
	}
	var named []vpnconfig.Server
	var changed *vpnconfig.Server
	for _, s := range subs[i].Servers {
		s.Subscription = subs[i].ID
		if !vpnconfig.SameChoice(a, s) {
			continue
		}
		if a.Address == "" && a.Port == 0 {
			return actualCurrent, s
		}
		if s.Address == a.Address && s.Port == a.Port {
			if a.Identity == "" || vpnconfig.IdentityMark(s) == a.Identity {
				return actualCurrent, s
			}
			// A twin further down can still carry the mark: the user may run it.
			if changed == nil {
				changed = &s
			}
			continue
		}
		named = append(named, s)
	}
	if changed != nil {
		return actualChanged, *changed
	}
	if len(named) == 1 {
		return actualMoved, named[0]
	}
	return actualGone, vpnconfig.Server{}
}
