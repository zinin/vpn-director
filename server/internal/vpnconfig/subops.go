package vpnconfig

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ConfigUpdate is a locked update of vpn-director.json: lock, load, fn, save,
// unlock. The daemons pass service.ConfigStore.UpdateVPNConfig; the watch
// passes its own update, which also refuses a write once VPN Director is
// stopped.
type ConfigUpdate func(func(*VPNDirectorConfig) error) error

// SubscriptionFiles reaches the subscription files of one data directory. The
// operations below call it only inside the update they are given, so every
// write to the files happens under the config lock.
type SubscriptionFiles struct {
	Load   func() ([]Subscription, error)
	Save   func(Subscription) error
	Delete func(id string) error
}

var (
	// ErrSubscriptionGone is a write for a subscription that no longer exists
	// with the link the caller read: it was deleted, or deleted and its link
	// added again under another id.
	ErrSubscriptionGone = errors.New("the subscription was deleted or changed")
	// ErrSubscriptionLimit refuses an add past MaxSubscriptions.
	ErrSubscriptionLimit = fmt.Errorf("at most %d subscriptions", MaxSubscriptions)
	// ErrSubscriptionNameTaken refuses a name another subscription has.
	ErrSubscriptionNameTaken = errors.New("another subscription has that name")
	// ErrSubscriptionStatic refuses a refresh of a static list.
	ErrSubscriptionStatic = errors.New("a static list has no link to refresh")
	// ErrSaveSubscription marks a failure of the subscription file itself. The
	// config was not written either.
	ErrSaveSubscription = errors.New("save subscription")
)

// stamp is how the files keep time: UTC, whole seconds.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AddSubscription publishes servers, downloaded from rawURL, as a new
// subscription named name - its host when name is empty - under update's lock.
// A link already saved, compared as written, makes it a refresh of that
// subscription instead, and a rename too when name is given; existed says so.
// A name the rules refuse, or another subscription has, refuses the whole call.
//
// xray.servers is recomputed in the same update. A failure after the file was
// written carries ErrServersSaved: the list is out, the config beside it is not.
func AddSubscription(update ConfigUpdate, files SubscriptionFiles, rawURL, name string, servers []Server, now time.Time) (sub Subscription, existed bool, err error) {
	if rawURL == "" {
		return Subscription{}, false, errors.New("a subscription needs a link")
	}
	if name != "" {
		if name, err = CleanSubscriptionName(name); err != nil {
			return Subscription{}, false, err
		}
	}
	saved := false
	err = update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := -1
		for j, s := range subs {
			if s.URL == rawURL {
				i = j
				break
			}
		}
		if i >= 0 {
			existed = true
			sub = subs[i]
			if name != "" && name != sub.Name {
				if SubscriptionNameTaken(subs, name, sub.ID) {
					return ErrSubscriptionNameTaken
				}
				sub.Name = name
			}
		} else {
			if len(subs) >= MaxSubscriptions {
				return ErrSubscriptionLimit
			}
			n := name
			switch {
			case n == "":
				n = DefaultSubscriptionName(subs, hostOf(rawURL))
			case SubscriptionNameTaken(subs, n, ""):
				return ErrSubscriptionNameTaken
			}
			id, err := NewSubscriptionID(subs)
			if err != nil {
				return err
			}
			sub = Subscription{ID: id, Name: n, URL: rawURL, Added: stamp(now)}
			i = len(subs)
			subs = append(subs, sub)
		}
		sub.Servers = servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
		}
		return nil
	})
	switch {
	case err == nil:
		return sub, existed, nil
	case saved:
		return sub, existed, ServersSaved(err)
	default:
		return Subscription{}, existed, err
	}
}

// RefreshSubscription replaces the servers of subscription id with servers,
// downloaded from rawURL, under update's lock, and clears its error - only
// while that subscription still exists with that link: ErrSubscriptionGone
// otherwise, and nothing is written. xray.servers is recomputed in the same
// update.
func RefreshSubscription(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, servers []Server, now time.Time) (Subscription, error) {
	if rawURL == "" {
		return Subscription{}, ErrSubscriptionStatic
	}
	var sub Subscription
	saved := false
	err := update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		sub = subs[i]
		sub.Servers = servers
		sub.Refreshed = stamp(now)
		sub.Error = ""
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		saved = true
		subs[i] = sub
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(subs)
		}
		return nil
	})
	switch {
	case err == nil:
		return sub, nil
	case saved:
		return sub, ServersSaved(err)
	default:
		return Subscription{}, err
	}
}

// RecordSubscriptionError notes msg, why a refresh of subscription id from
// rawURL failed; the list stays. It writes nothing when the subscription is
// gone or has another link (ErrSubscriptionGone), and nothing when its
// Refreshed is no longer since, the time the caller read before it began: a
// refresh that succeeded meanwhile is not marked failed by an older one.
func RecordSubscriptionError(update ConfigUpdate, files SubscriptionFiles, id, rawURL string, since time.Time, msg string) error {
	return update(func(*VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 || subs[i].URL != rawURL {
			return ErrSubscriptionGone
		}
		if !subs[i].Refreshed.Equal(since) {
			return nil
		}
		sub := subs[i]
		sub.Error = msg
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		return nil
	})
}

// RenameSubscription gives subscription id the name name, under update's lock.
func RenameSubscription(update ConfigUpdate, files SubscriptionFiles, id, name string) (Subscription, error) {
	name, err := CleanSubscriptionName(name)
	if err != nil {
		return Subscription{}, err
	}
	var sub Subscription
	err = update(func(*VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 {
			return ErrSubscriptionGone
		}
		if SubscriptionNameTaken(subs, name, id) {
			return ErrSubscriptionNameTaken
		}
		sub = subs[i]
		sub.Name = name
		if err := files.Save(sub); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		return nil
	})
	return sub, err
}

// DeleteSubscription removes subscription id under update's lock, recomputes
// xray.servers without it and ends a preferred_server that names it. The
// running Xray is left alone: activeWasIn says active_server names the
// subscription, for the caller to tell the user to select another server. A
// failure after the file was removed carries ErrServersSaved.
func DeleteSubscription(update ConfigUpdate, files SubscriptionFiles, id string) (activeWasIn bool, err error) {
	deleted := false
	err = update(func(cfg *VPNDirectorConfig) error {
		subs, err := files.Load()
		if err != nil {
			return err
		}
		i := FindSubscription(subs, id)
		if i < 0 {
			return ErrSubscriptionGone
		}
		rest := append(append([]Subscription{}, subs[:i]...), subs[i+1:]...)
		if err := files.Delete(id); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveSubscription, err)
		}
		deleted = true
		if cfg != nil {
			cfg.Xray.Servers = SubscriptionIPs(rest)
			if p := cfg.Xray.PreferredServer; p != nil && p.Subscription == id {
				cfg.Xray.PreferredServer = nil
			}
			activeWasIn = cfg.Xray.ActiveServer != nil && cfg.Xray.ActiveServer.Subscription == id
		}
		return nil
	})
	if err != nil && deleted {
		return activeWasIn, ServersSaved(err)
	}
	return activeWasIn, err
}
