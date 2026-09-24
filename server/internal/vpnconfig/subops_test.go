package vpnconfig

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// memStore is one data directory in memory: its subscription files and its
// config. update is the locked read-modify-write the daemons do - fn on a copy
// of the config, kept only when fn and the config write succeed.
type memStore struct {
	cfg           VPNDirectorConfig
	subs          []Subscription
	saveErr       error // the subscription file write fails
	configErr     error // the config write after fn fails
	inUpdate      bool
	writesOutside int // file writes made outside update: none may happen
}

func (m *memStore) update(fn func(*VPNDirectorConfig) error) error {
	m.inUpdate = true
	defer func() { m.inUpdate = false }()
	cfg := m.cfg
	if err := fn(&cfg); err != nil {
		return err
	}
	if m.configErr != nil {
		return m.configErr
	}
	m.cfg = cfg
	return nil
}

func (m *memStore) files() SubscriptionFiles {
	return SubscriptionFiles{
		Load: func() ([]Subscription, error) {
			out := make([]Subscription, len(m.subs))
			copy(out, m.subs)
			return out, nil
		},
		Save: func(s Subscription) error {
			if !m.inUpdate {
				m.writesOutside++
			}
			if m.saveErr != nil {
				return m.saveErr
			}
			for i := range m.subs {
				if m.subs[i].ID == s.ID {
					m.subs[i] = s
					return nil
				}
			}
			m.subs = append(m.subs, s)
			return nil
		},
		Delete: func(id string) error {
			if !m.inUpdate {
				m.writesOutside++
			}
			for i := range m.subs {
				if m.subs[i].ID == id {
					m.subs = append(m.subs[:i:i], m.subs[i+1:]...)
					return nil
				}
			}
			return nil
		},
	}
}

var t0 = time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)

func oslo() []Server {
	return []Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}}}
}

func TestAddSubscription_ANewLinkIsNamedAfterItsHost(t *testing.T) {
	m := &memStore{}

	sub, existed, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0.Add(1500*time.Millisecond))

	if err != nil || existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
	if !ValidSubscriptionID(sub.ID) || sub.Name != "sub.example.com" || sub.URL != "https://sub.example.com/s/t" {
		t.Fatalf("sub %+v", sub)
	}
	// UTC, whole seconds: the shell writes the same.
	if !sub.Added.Equal(t0.Add(time.Second)) || !sub.Refreshed.Equal(sub.Added) || sub.Added.Location() != time.UTC {
		t.Fatalf("added %v, refreshed %v", sub.Added, sub.Refreshed)
	}
	if len(m.subs) != 1 || m.subs[0].ID != sub.ID || len(m.subs[0].Servers) != 1 {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.10"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
	if m.writesOutside != 0 {
		t.Fatal("a file was written outside the config lock")
	}
}

func TestAddSubscription_ASavedLinkIsRefreshedAndRenamed(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: t0, Refreshed: t0, Error: "download failed: HTTP 403"}}}
	riga := []Server{{Name: "Riga", Address: "r.example.com", Port: 443, IPs: []string{"192.0.2.20"}}}

	sub, existed, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "Main", riga, t0.Add(time.Hour))

	if err != nil || !existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
	if sub.ID != "0a1b2c3d" || sub.Name != "Main" || sub.Error != "" || !sub.Added.Equal(t0) || !sub.Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("sub %+v", sub)
	}
	if len(m.subs) != 1 || m.subs[0].Servers[0].Name != "Riga" || m.subs[0].Name != "Main" {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
}

func TestAddSubscription_RefusesATakenName(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t"},
		{ID: "1b2c3d4e", Name: "Beta", URL: "https://other.example.net/s/u"},
	}}

	if _, _, err := AddSubscription(m.update, m.files(), "https://third.example.org/s/v", "alpha", oslo(), t0); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a new link under a taken name: %v", err)
	}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "BETA", oslo(), t0); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a saved link renamed to a taken name: %v", err)
	}
	if len(m.subs) != 2 || m.subs[0].Name != "Alpha" || m.subs[0].Servers != nil {
		t.Fatalf("a refused add wrote %+v", m.subs)
	}
}

func TestAddSubscription_RefusesANameTheRulesRefuse(t *testing.T) {
	m := &memStore{}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "\tAlpha", oslo(), t0); !errors.Is(err, ErrSubscriptionName) {
		t.Fatalf("err %v", err)
	}
	if len(m.subs) != 0 {
		t.Fatalf("wrote %+v", m.subs)
	}
}

func TestAddSubscription_ATakenDefaultGetsASuffix(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "sub.example.com", URL: "https://sub.example.com/s/one"}}}

	sub, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/two", "", oslo(), t0)

	if err != nil || sub.Name != "sub.example.com-2" {
		t.Fatalf("sub %+v, err %v", sub, err)
	}
}

func TestAddSubscription_RefusesAnEleventh(t *testing.T) {
	m := &memStore{}
	for i := 0; i < MaxSubscriptions; i++ {
		m.subs = append(m.subs, Subscription{ID: fmt.Sprintf("%08x", i), Name: fmt.Sprintf("s%d", i), URL: fmt.Sprintf("https://s%d.example.com/s", i)})
	}

	if _, _, err := AddSubscription(m.update, m.files(), "https://new.example.com/s", "", oslo(), t0); !errors.Is(err, ErrSubscriptionLimit) {
		t.Fatalf("err %v", err)
	}
	// A saved link is a refresh, which the limit does not stop.
	if _, existed, err := AddSubscription(m.update, m.files(), "https://s3.example.com/s", "", oslo(), t0); err != nil || !existed {
		t.Fatalf("existed %v, err %v", existed, err)
	}
}

// The file is out once the config write fails: the importers say "saved" for
// this failure only.
func TestAddSubscription_SaysWhichHalfLanded(t *testing.T) {
	m := &memStore{configErr: errors.New("disk full")}
	if _, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0); !errors.Is(err, ErrServersSaved) || len(m.subs) != 1 {
		t.Fatalf("err %v, files %d", err, len(m.subs))
	}

	m = &memStore{saveErr: errors.New("disk full")}
	_, _, err := AddSubscription(m.update, m.files(), "https://sub.example.com/s/t", "", oslo(), t0)
	if !errors.Is(err, ErrSaveSubscription) || errors.Is(err, ErrServersSaved) {
		t.Fatalf("err %v", err)
	}
}

// A download takes long enough for its subscription to be deleted meanwhile,
// or deleted and its link added again under a new id: publishing it then would
// bring back a file the user removed.
func TestRefreshSubscription_ADeletedSubscriptionStaysDeleted(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "1b2c3d4e", Name: "Alpha", URL: "https://sub.example.com/s/t"}}}

	if _, err := RefreshSubscription(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", oslo(), t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := RefreshSubscription(m.update, m.files(), "1b2c3d4e", "https://sub.example.com/s/other", oslo(), t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("another link: %v", err)
	}
	if len(m.subs) != 1 || m.subs[0].Servers != nil || m.cfg.Xray.Servers != nil {
		t.Fatalf("wrote %+v, %v", m.subs, m.cfg.Xray.Servers)
	}
}

func TestRefreshSubscription_ReplacesTheListAndClearsTheError(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: t0, Refreshed: t0, Error: "download failed: timeout"},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []Server{{Name: "Riga", IPs: []string{"192.0.2.20"}}}},
	}}

	sub, err := RefreshSubscription(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", oslo(), t0.Add(time.Hour))

	if err != nil || sub.Error != "" || !sub.Refreshed.Equal(t0.Add(time.Hour)) || !sub.Added.Equal(t0) {
		t.Fatalf("sub %+v, err %v", sub, err)
	}
	// xray.servers covers every subscription, not just the one refreshed.
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.10", "192.0.2.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
}

func TestRefreshSubscription_AStaticListHasNothingToRefresh(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "1b2c3d4e", Name: "Beta"}}}
	if _, err := RefreshSubscription(m.update, m.files(), "1b2c3d4e", "", oslo(), t0); !errors.Is(err, ErrSubscriptionStatic) {
		t.Fatalf("err %v", err)
	}
}

func TestRecordSubscriptionError_KeepsTheList(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", URL: "https://sub.example.com/s/t", Refreshed: t0, Servers: oslo()}}}

	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: HTTP 403"); err != nil {
		t.Fatal(err)
	}

	if m.subs[0].Error != "download failed: HTTP 403" || len(m.subs[0].Servers) != 1 || !m.subs[0].Refreshed.Equal(t0) {
		t.Fatalf("file %+v", m.subs[0])
	}
}

// Every wave of an outage fails the same way. The error is recorded once:
// neither the file nor the config is written for it again. Another error is.
func TestRecordSubscriptionError_TheSameErrorAgainWritesNothing(t *testing.T) {
	m := &memStore{
		subs:      []Subscription{{ID: "0a1b2c3d", URL: "https://sub.example.com/s/t", Refreshed: t0, Error: "download failed: HTTP 403", Servers: oslo()}},
		saveErr:   errors.New("the file was written"),
		configErr: errors.New("the config was written"),
	}

	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: HTTP 403"); err != nil {
		t.Fatal(err)
	}

	m.saveErr, m.configErr = nil, nil
	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: HTTP 502"); err != nil {
		t.Fatal(err)
	}
	if m.subs[0].Error != "download failed: HTTP 502" {
		t.Fatalf("error %q, want the new one recorded", m.subs[0].Error)
	}
}

// Two refreshes can overlap: the Web UI's that succeeded and the watch's that
// began before it and failed. The older one does not mark the newer list
// failed, and writes neither the file nor the config.
func TestRecordSubscriptionError_ANewerRefreshIsNotMarkedFailed(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", URL: "https://sub.example.com/s/t", Refreshed: t0.Add(time.Minute)}},
		configErr: errors.New("the config was written")}

	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "download failed: timeout"); err != nil {
		t.Fatal(err)
	}

	if m.subs[0].Error != "" {
		t.Fatalf("error %q written over a newer refresh", m.subs[0].Error)
	}
}

func TestRecordSubscriptionError_ADeletedSubscriptionStaysDeleted(t *testing.T) {
	m := &memStore{}
	if err := RecordSubscriptionError(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/t", t0, "x"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("err %v", err)
	}
	if len(m.subs) != 0 {
		t.Fatalf("wrote %+v", m.subs)
	}
}

func TestRenameSubscription(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha"}, {ID: "1b2c3d4e", Name: "Beta"}}}

	if sub, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", " ALPHA "); err != nil || sub.Name != "ALPHA" {
		t.Fatalf("its own name in capitals: %+v, %v", sub, err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", "beta"); !errors.Is(err, ErrSubscriptionNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "ffffffff", "Gamma"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("no such subscription: %v", err)
	}
	if _, err := RenameSubscription(m.update, m.files(), "0a1b2c3d", ""); !errors.Is(err, ErrSubscriptionName) {
		t.Fatalf("an empty name: %v", err)
	}
	if m.subs[0].Name != "ALPHA" || m.subs[1].Name != "Beta" {
		t.Fatalf("files %+v", m.subs)
	}
}

func TestDeleteSubscription_EndsTheChoiceKeptFromItAndSaysWhatRuns(t *testing.T) {
	m := &memStore{subs: []Subscription{
		{ID: "0a1b2c3d", Name: "Alpha", Servers: []Server{{Name: "Oslo", IPs: []string{"192.0.2.10"}}}},
		{ID: "1b2c3d4e", Name: "Beta", Servers: []Server{{Name: "Riga", IPs: []string{"192.0.2.20"}}}},
	}}
	m.cfg.Xray.Servers = []string{"192.0.2.10", "192.0.2.20"}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}
	m.cfg.Xray.PreferredServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo"}

	active, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d")

	if err != nil || !active {
		t.Fatalf("active %v, err %v", active, err)
	}
	if len(m.subs) != 1 || m.subs[0].ID != "1b2c3d4e" {
		t.Fatalf("files %+v", m.subs)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"192.0.2.20"}) || m.cfg.Xray.PreferredServer != nil {
		t.Fatalf("xray.servers %v, preferred %+v", m.cfg.Xray.Servers, m.cfg.Xray.PreferredServer)
	}
	// The running Xray is left alone, and so is its record.
	if a := m.cfg.Xray.ActiveServer; a == nil || a.Subscription != "0a1b2c3d" {
		t.Fatalf("active %+v", a)
	}
	if _, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d"); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("a second delete: %v", err)
	}
}

func TestDeleteSubscription_KeepsAChoiceFromAnotherSubscription(t *testing.T) {
	m := &memStore{subs: []Subscription{{ID: "0a1b2c3d", Name: "Alpha"}, {ID: "1b2c3d4e", Name: "Beta"}}}
	m.cfg.Xray.PreferredServer = &ActiveServer{Subscription: "1b2c3d4e", Name: "Riga"}

	active, err := DeleteSubscription(m.update, m.files(), "0a1b2c3d")

	if err != nil || active || m.cfg.Xray.PreferredServer == nil {
		t.Fatalf("active %v, err %v, preferred %+v", active, err, m.cfg.Xray.PreferredServer)
	}
}
