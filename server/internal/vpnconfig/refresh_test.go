package vpnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMergeRefresh_WhatAPanelPicksAtRandomKeepsTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want the stored list as it is", m)
	}
}

// A panel that generates the WebSocket Host and path anew for every download
// lists the same server each time: under the same name it pairs with the
// stored copy, which stays as it is.
func TestMergeRefresh_AGeneratedWebSocketHostAndPathKeepTheStoredCopy(t *testing.T) {
	stored := []Server{webSocket("NL", "a1.example", "/x7f", "203.0.113.30")}

	m := MergeRefresh(stored, []Server{webSocket("NL", "q9z.example", "/kd83jd", "203.0.113.30")})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, stored[0].Outbound) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want the stored copy as it is", m)
	}
}

// One front often routes servers by the path alone - one UUID for every
// inbound, /de to one country and /nl to another - and a panel can reorder
// them: each pairs with its own stored record, never with its twin.
func TestMergeRefresh_PathRoutedTwinsKeepTheirOwnOutbounds(t *testing.T) {
	de := webSocket("DE", "front.example", "/de", "203.0.113.30")
	nl := webSocket("NL", "front.example", "/nl", "203.0.113.30")

	m := MergeRefresh([]Server{de, nl}, []Server{nl, de})

	if !reflect.DeepEqual(m.Servers, []Server{nl, de}) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want NL on /nl and DE on /de, nothing renamed", m)
	}
}

// When the panel generates the paths anew as well, only the names tell such
// twins apart: each pairs with the stored server of its name.
func TestMergeRefresh_TwinsWithGeneratedPathsPairByName(t *testing.T) {
	de := webSocket("DE", "a1.example", "/x7f", "203.0.113.30")
	nl := webSocket("NL", "b2.example", "/p4q", "203.0.113.30")

	m := MergeRefresh([]Server{de, nl}, []Server{webSocket("NL", "q9z.example", "/kd83jd", "203.0.113.30"), webSocket("DE", "r5.example", "/m2", "203.0.113.30")})

	if !reflect.DeepEqual(m.Servers, []Server{nl, de}) || m.Added+m.Removed+m.Renamed+m.Readdressed != 0 {
		t.Fatalf("merge %+v, want each stored copy under its own name", m)
	}
}

// A name two stored servers share tells neither apart. The front drops /de
// and adds /fr under that name: /nl pairs by its identity, /fr comes in
// fresh and /de leaves - /fr never dials /de's outbound.
func TestMergeRefresh_ANameTwinsShareDoesNotPairByTheLooseIdentity(t *testing.T) {
	de := webSocket("S", "front.example", "/de", "203.0.113.30")
	nl := webSocket("S", "front.example", "/nl", "203.0.113.30")
	fr := webSocket("S", "front.example", "/fr", "203.0.113.30")

	m := MergeRefresh([]Server{de, nl}, []Server{nl, fr})

	if !reflect.DeepEqual(m.Servers, []Server{nl, fr}) || m.Added != 1 || m.Removed != 1 || m.Renamed != 0 {
		t.Fatalf("merge %+v, want /nl kept and /fr fresh", m)
	}
}

// Under a new name, a server whose Host and path were generated anew is
// another server: the fresh copy comes in, and the stored one leaves.
func TestMergeRefresh_AGeneratedHostAndPathUnderANewNameIsANewServer(t *testing.T) {
	stored := webSocket("NL 10GB", "a1.example", "/x7f", "203.0.113.30")
	fresh := webSocket("NL 9GB", "q9z.example", "/kd83jd", "203.0.113.30")

	m := MergeRefresh([]Server{stored}, []Server{fresh})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, fresh.Outbound) || m.Added != 1 || m.Removed != 1 || m.Renamed != 0 {
		t.Fatalf("merge %+v, want the fresh copy in place of the stored one", m)
	}
}

// 3x-ui's default remark puts the traffic left into every name.
func TestMergeRefresh_TheFreshNameOnTheStoredCopy(t *testing.T) {
	stored := []Server{reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")})

	if len(m.Servers) != 1 || m.Servers[0].Name != "DE 9GB" || !bytes.Equal(m.Servers[0].Outbound, stored[0].Outbound) || m.Renamed != 1 {
		t.Fatalf("merge %+v", m)
	}
}

func TestMergeRefresh_Addresses(t *testing.T) {
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10", "203.0.113.11")}
	for _, tc := range []struct {
		name  string
		fresh []string
		want  []string
		moved int
	}{
		{"the same set in another order", []string{"203.0.113.11", "203.0.113.10"}, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a host that did not resolve this time", nil, []string{"203.0.113.10", "203.0.113.11"}, 0},
		{"a new set", []string{"203.0.113.12"}, []string{"203.0.113.12"}, 1},
	} {
		m := MergeRefresh(stored, []Server{reality("DE", "t.example", "bb22", tc.fresh...)})
		if len(m.Servers) != 1 || !reflect.DeepEqual(m.Servers[0].IPs, tc.want) || m.Readdressed != tc.moved {
			t.Errorf("%s: merge %+v", tc.name, m)
		}
	}
}

func TestMergeRefresh_NewChangedAndGoneServers(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	stored := []Server{reality("DE", "s.example", "aa11", "203.0.113.10"), fr}
	rotated := reality("DE", "s.example", "bb22", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))
	nl := Server{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")} // did not resolve

	m := MergeRefresh(stored, []Server{rotated, nl})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, rotated.Outbound) {
		t.Fatalf("servers %+v, want the rotated server alone", m.Servers)
	}
	if m.Added != 1 || m.Removed != 2 {
		t.Fatalf("added %d, removed %d, want 1 and 2", m.Added, m.Removed)
	}
}

func TestMergeRefresh_TwinsPairInOrder(t *testing.T) {
	stored := []Server{reality("DE-1", "a.example", "aa11", "203.0.113.10"), reality("DE-2", "a.example", "bb22", "203.0.113.10")}

	m := MergeRefresh(stored, []Server{reality("DE-1", "b.example", "cc33", "203.0.113.10"), reality("DE-2", "b.example", "dd44", "203.0.113.10")})

	if !reflect.DeepEqual(m.Servers, stored) {
		t.Fatalf("merge %+v, want each twin on its own stored copy", m.Servers)
	}
}

func TestMergeRefresh_TheFreshOrder(t *testing.T) {
	de := reality("DE", "a.example", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}

	m := MergeRefresh([]Server{de, fr}, []Server{fr, reality("DE", "b.example", "bb22", "203.0.113.10")})

	if len(m.Servers) != 2 || m.Servers[0].Name != "FR" || !bytes.Equal(m.Servers[1].Outbound, de.Outbound) {
		t.Fatalf("merge %+v, want FR, then the stored DE", m.Servers)
	}
}

// A record stored before outbounds has no identity; the first refresh puts a
// fresh copy in its place.
func TestMergeRefresh_ALegacyRecordMakesWayForAFreshCopy(t *testing.T) {
	legacy := Server{Name: "DE", Address: "de.example", Port: 443, UUID: "u-1", Security: "reality", IPs: []string{"203.0.113.10"}}
	fresh := reality("DE", "a.example", "aa11", "203.0.113.10")

	m := MergeRefresh([]Server{legacy}, []Server{fresh})

	if len(m.Servers) != 1 || !bytes.Equal(m.Servers[0].Outbound, fresh.Outbound) || m.Added != 1 || m.Removed != 1 {
		t.Fatalf("merge %+v", m)
	}
}

const alphaLink = "https://sub.example.com/s/t"

// alphaWith is subscription Alpha holding servers, refreshed at t0: the calls
// below pass t0 as since, the refreshed a round read before it downloaded.
func alphaWith(servers ...Server) Subscription {
	return Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: alphaLink, Added: t0, Refreshed: t0, Servers: servers}
}

// A download that differs from the file only in what the panel picked writes
// neither the file nor the config: saveErr and configErr would fail any write.
func TestPublishRefresh_NothingChangedWritesNothing(t *testing.T) {
	stored := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(stored)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || res.Wrote {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{stored}) || !m.subs[0].Refreshed.Equal(t0) {
		t.Fatalf("file %+v", m.subs[0])
	}
}

// Nor does one that differs, under the same name, in the WebSocket Host and
// path some panels generate for every download.
func TestPublishRefresh_AGeneratedWebSocketHostAndPathWriteNothing(t *testing.T) {
	stored := webSocket("NL", "a1.example", "/x7f", "203.0.113.30")
	m := &memStore{subs: []Subscription{alphaWith(stored)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{webSocket("NL", "q9z.example", "/kd83jd", "203.0.113.30")}, t0.Add(time.Hour))

	if err != nil || res.Wrote {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{stored}) || !m.subs[0].Refreshed.Equal(t0) {
		t.Fatalf("file %+v", m.subs[0])
	}
}

func TestPublishRefresh_AChangeIsWrittenOnce(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	m := &memStore{subs: []Subscription{alphaWith(de)}}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE", "example.com", "bb22", "203.0.113.10"), fr}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || res.Added != 1 || res.Count != 2 || res.Cleared {
		t.Fatalf("result %+v, err %v; the file recorded no error to clear", res, err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de, fr}) || !m.subs[0].Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("file %+v", m.subs[0])
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"203.0.113.10", "203.0.113.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
	if m.writesOutside != 0 {
		t.Fatal("a file was written outside the config lock")
	}
}

// A recorded error leaves refreshed as it was: the round still publishes over
// an error a failed manual refresh recorded while it downloaded, and clears it.
func TestPublishRefresh_ClearsARecordedError(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	sub := alphaWith(de)
	sub.Error = "download failed: HTTP 403"
	m := &memStore{subs: []Subscription{sub}}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || !res.Cleared || m.subs[0].Error != "" || !m.subs[0].Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de}) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
}

// A manual refresh, an add of the saved link or the wave can write the list
// while the round downloads, and moves refreshed. That list is newer than the
// round's download, which would take back what it brought - a server added, a
// rename - until the next round: the round writes nothing at all.
func TestPublishRefresh_ANewerRefreshSupersedesTheRound(t *testing.T) {
	de := reality("DE 9GB", "www.example.com", "aa11", "203.0.113.10")
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	newer := alphaWith(de, fr)
	newer.Refreshed = t0.Add(30 * time.Minute) // the manual refresh, after the round read t0
	newer.Error = "download failed: HTTP 403"  // and a manual refresh that failed after it
	m := &memStore{subs: []Subscription{newer}}
	m.cfg.Xray.Servers = []string{"203.0.113.10", "203.0.113.20"}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 9GB", Address: "de.example", Port: 443, Seq: 3}
	// The round's older download: DE under its old name, FR not listed yet.
	listed := []Server{reality("DE 10GB", "example.com", "bb22", "203.0.113.10")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, listed, t0.Add(time.Hour))

	if !errors.Is(err, ErrRefreshSuperseded) || res.Wrote || len(res.Followed) != 0 {
		t.Fatalf("result %+v, err %v; want the round superseded", res, err)
	}
	if !reflect.DeepEqual(m.subs[0], newer) {
		t.Fatalf("file %+v, want the newer refresh's left as it is", m.subs[0])
	}
	if a := m.cfg.Xray.ActiveServer; !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"203.0.113.10", "203.0.113.20"}) || a.Name != "DE 9GB" || a.Seq != 3 {
		t.Fatalf("xray.servers %v, active %+v; want the config untouched", m.cfg.Xray.Servers, a)
	}
}

func TestPublishRefresh_ADeletedOrRelinkedSubscriptionIsNotPublished(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "www.example.com", "aa11", "203.0.113.10"))}}
	listed := []Server{{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}}

	if _, err := PublishRefresh(m.update, m.files(), "1b2c3d4e", alphaLink, t0, listed, t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/other", t0, listed, t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("relinked: %v", err)
	}
	if m.subs[0].Servers[0].Name != "DE" || m.cfg.Xray.Servers != nil {
		t.Fatalf("wrote %+v, %v", m.subs, m.cfg.Xray.Servers)
	}
}

func TestPublishRefresh_AnEmptyMergeKeepsTheList(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(de)}}
	nl := Server{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")} // did not resolve

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{nl}, t0); !errors.Is(err, ErrNoServerResolved) {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de}) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
}

func TestPublishRefresh_TheRecordsFollowARename(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	named := ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443}
	active, preferred := named, named
	active.Seq = 7
	pending := active
	m.cfg.Xray.ActiveServer, m.cfg.Xray.PreferredServer = &active, &preferred
	m.cfg.Xray.PendingRestore = &XrayPendingRestore{Active: &pending}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || len(res.Followed) != 3 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	// The watch moves what it remembers of the active server along with the
	// record: it needs the record's address and port, which stay.
	for _, f := range res.Followed {
		if f.From != "DE 10GB" || f.To != "DE 9GB" || f.Address != "de.example" || f.Port != 443 {
			t.Errorf("followed %+v", f)
		}
	}
	for record, a := range map[string]*ActiveServer{
		"active_server":          m.cfg.Xray.ActiveServer,
		"preferred_server":       m.cfg.Xray.PreferredServer,
		"pending_restore.active": m.cfg.Xray.PendingRestore.Active,
	} {
		if a.Name != "DE 9GB" || a.Address != "de.example" || a.Port != 443 {
			t.Errorf("%s %+v", record, a)
		}
	}
	// A rename is no selection: the write counter stays.
	if m.cfg.Xray.ActiveServer.Seq != 7 || m.cfg.Xray.PendingRestore.Active.Seq != 7 {
		t.Fatalf("seq %d and %d, want 7", m.cfg.Xray.ActiveServer.Seq, m.cfg.Xray.PendingRestore.Active.Seq)
	}
}

func TestPublishRefresh_OnlyItsOwnSubscriptionsRecordsFollow(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "1b2c3d4e", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 3}

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Xray.ActiveServer.Name != "DE 10GB" {
		t.Fatalf("another subscription's record followed: %+v", m.cfg.Xray.ActiveServer)
	}
}

func TestPublishRefresh_ARecordWhoseServerLeftStays(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}
	m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"), fr)}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 3}
	renamed := fr
	renamed.Name = "FR 2"

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{renamed}, t0); err != nil {
		t.Fatal(err)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE 10GB" || a.Seq != 3 {
		t.Fatalf("active %+v, want it left as it was", a)
	}
}

// The merge reads the file as it is under the lock, not the list the round
// read before downloading. A refresh that wrote in between moved refreshed and
// supersedes the round (TestPublishRefresh_ANewerRefreshSupersedesTheRound); a
// file still refreshed when the round read it is merged as it stands: its
// copies are the stored ones, and a download that differs from them only in
// what the panel picked writes nothing.
func TestPublishRefresh_MergesWithTheFileAsItIsThen(t *testing.T) {
	inFile := reality("DE", "b.example", "cc33", "203.0.113.10") // the round itself read aa11
	m := &memStore{subs: []Subscription{alphaWith(inFile)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE", "a.example", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || res.Wrote || !bytes.Equal(m.subs[0].Servers[0].Outbound, inFile.Outbound) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
}

// An operator that rotates a REALITY key and keeps the name makes another
// server: it comes in fresh, and the record that named the old one is left
// alone - neither renamed nor cleared.
func TestPublishRefresh_ARotatedKeyIsANewServer(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "s.example", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE", Address: "de.example", Port: 443, Seq: 3}
	rotated := reality("DE", "s.example", "aa11", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{rotated}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || res.Added != 1 || res.Removed != 1 || len(res.Followed) != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if !bytes.Equal(m.subs[0].Servers[0].Outbound, rotated.Outbound) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE" || a.Seq != 3 {
		t.Fatalf("active %+v, want it left as it was", a)
	}
}

// apart is m.update on a config whose active_server is fn's own copy: memStore
// copies the config shallowly, and a config write that fails must leave the
// record as it was, as it does on disk.
func (m *memStore) apart(fn func(*VPNDirectorConfig) error) error {
	return m.update(func(cfg *VPNDirectorConfig) error {
		if a := cfg.Xray.ActiveServer; a != nil {
			own := *a
			cfg.Xray.ActiveServer = &own
		}
		return fn(cfg)
	})
}

// A config write that fails after the file was written puts the file back:
// left as written, it would read as unchanged to every later round, and
// xray.servers and the record would stay behind for good. The next round
// writes them all together.
func TestPublishRefresh_AFailedConfigWritePutsTheFileBack(t *testing.T) {
	stored := alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))
	m := &memStore{subs: []Subscription{stored}, configErr: errors.New("disk full")}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE 10GB", Address: "de.example", Port: 443, Seq: 3}
	listed := []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.20")}

	res, err := PublishRefresh(m.apart, m.files(), "0a1b2c3d", alphaLink, t0, listed, t0.Add(time.Hour))

	if err == nil || errors.Is(err, ErrServersSaved) || res.Wrote {
		t.Fatalf("result %+v, err %v; want the config's own error and nothing written", res, err)
	}
	if !reflect.DeepEqual(m.subs[0], stored) {
		t.Fatalf("file %+v, want it put back as it was", m.subs[0])
	}
	if a := m.cfg.Xray.ActiveServer; m.cfg.Xray.Servers != nil || a.Name != "DE 10GB" || a.Seq != 3 {
		t.Fatalf("xray.servers %v, active %+v; want the config untouched", m.cfg.Xray.Servers, a)
	}

	m.configErr = nil
	res, err = PublishRefresh(m.apart, m.files(), "0a1b2c3d", alphaLink, t0, listed, t0.Add(time.Hour))

	if err != nil || !res.Wrote {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if s := m.subs[0].Servers; len(s) != 1 || s[0].Name != "DE 9GB" || !reflect.DeepEqual(s[0].IPs, []string{"203.0.113.20"}) {
		t.Fatalf("servers %+v", s)
	}
	if !reflect.DeepEqual(m.cfg.Xray.Servers, []string{"203.0.113.20"}) {
		t.Fatalf("xray.servers %v", m.cfg.Xray.Servers)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE 9GB" || a.Seq != 3 {
		t.Fatalf("active %+v", a)
	}
	if f := res.Followed; len(f) != 1 || f[0].Record != "active_server" || f[0].From != "DE 10GB" || f[0].To != "DE 9GB" {
		t.Fatalf("followed %+v", f)
	}
}

// A file another writer wrote between the publication and the put-back stands,
// and the failure says the list is out: a manual refresh moves refreshed, and
// a rename keeps refreshed and error.
func TestPublishRefresh_AFileAnotherWriterWroteStays(t *testing.T) {
	for name, write := range map[string]func(*Subscription){
		"a refresh": func(s *Subscription) { s.Refreshed = t0.Add(2 * time.Hour) },
		"a rename":  func(s *Subscription) { s.Name = "Beta" },
	} {
		m := &memStore{subs: []Subscription{alphaWith(reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10"))}, configErr: errors.New("disk full")}
		var theirs Subscription
		calls := 0
		update := func(fn func(*VPNDirectorConfig) error) error {
			if calls++; calls == 2 {
				write(&m.subs[0]) // the other writer lands between the two updates
				theirs = m.subs[0]
			}
			return m.update(fn)
		}

		res, err := PublishRefresh(update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.20")}, t0.Add(time.Hour))

		if !errors.Is(err, ErrServersSaved) || !res.Wrote {
			t.Errorf("%s: result %+v, err %v; want the list out", name, res, err)
		}
		if calls != 2 || !reflect.DeepEqual(m.subs[0], theirs) {
			t.Errorf("%s: %d updates, file %+v; want the other writer's file left as it is", name, calls, m.subs[0])
		}
	}
}

// A record never moves onto a twin: active_server names DE through every
// refresh that follows renames, the periodic and the manual one, whether the
// front's paths are stable or generated anew, and the panel reorders them.
func TestRefresh_ARecordNeverMovesOntoATwin(t *testing.T) {
	const ip = "203.0.113.30"
	for name, tc := range map[string]struct{ stored, listed []Server }{
		"stable paths": {
			[]Server{webSocket("DE", "front.example", "/de", ip), webSocket("NL", "front.example", "/nl", ip)},
			[]Server{webSocket("NL", "front.example", "/nl", ip), webSocket("DE", "front.example", "/de", ip)},
		},
		"generated paths": {
			[]Server{webSocket("DE", "a1.example", "/x7f", ip), webSocket("NL", "b2.example", "/p4q", ip)},
			[]Server{webSocket("NL", "q9z.example", "/kd83jd", ip), webSocket("DE", "r5.example", "/m2", ip)},
		},
	} {
		for how, refresh := range map[string]func(*memStore) ([]RecordRename, error){
			"periodic": func(m *memStore) ([]RecordRename, error) {
				res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, tc.listed, t0.Add(time.Hour))
				return res.Followed, err
			},
			"manual": func(m *memStore) ([]RecordRename, error) {
				_, err := RefreshSubscriptionFollowingRenames(m.update, m.files(), "0a1b2c3d", alphaLink, tc.listed, t0.Add(time.Hour))
				return nil, err
			},
		} {
			m := &memStore{subs: []Subscription{alphaWith(tc.stored...)}}
			m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE", Address: "front.example", Port: 443, Seq: 3}

			followed, err := refresh(m)

			if a := m.cfg.Xray.ActiveServer; err != nil || len(followed) != 0 || a.Name != "DE" || a.Seq != 3 {
				t.Errorf("%s, %s refresh: active %+v, followed %+v, err %v; want DE as it was", name, how, a, followed, err)
			}
		}
	}
}
