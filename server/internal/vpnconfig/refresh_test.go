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

func alphaWith(servers ...Server) Subscription {
	return Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: alphaLink, Added: t0, Refreshed: t0, Servers: servers}
}

// A download that differs from the file only in what the panel picked writes
// neither the file nor the config: saveErr and configErr would fail any write.
func TestPublishRefresh_NothingChangedWritesNothing(t *testing.T) {
	stored := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(stored)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

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

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10"), fr}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || res.Added != 1 || res.Count != 2 {
		t.Fatalf("result %+v, err %v", res, err)
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

func TestPublishRefresh_ClearsARecordedError(t *testing.T) {
	de := reality("DE", "www.example.com", "aa11", "203.0.113.10")
	sub := alphaWith(de)
	sub.Error = "download failed: HTTP 403"
	m := &memStore{subs: []Subscription{sub}}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || !res.Wrote || m.subs[0].Error != "" || !m.subs[0].Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
	if !reflect.DeepEqual(m.subs[0].Servers, []Server{de}) {
		t.Fatalf("servers %+v", m.subs[0].Servers)
	}
}

func TestPublishRefresh_ADeletedOrRelinkedSubscriptionIsNotPublished(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "www.example.com", "aa11", "203.0.113.10"))}}
	listed := []Server{{Name: "FR", Address: "fr.example", Port: 443, IPs: []string{"203.0.113.20"}, Outbound: trojanTo("fr.example")}}

	if _, err := PublishRefresh(m.update, m.files(), "1b2c3d4e", alphaLink, listed, t0); !errors.Is(err, ErrSubscriptionGone) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", "https://sub.example.com/s/other", listed, t0); !errors.Is(err, ErrSubscriptionGone) {
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

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{nl}, t0); !errors.Is(err, ErrNoServerResolved) {
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

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || len(res.Followed) != 3 {
		t.Fatalf("result %+v, err %v", res, err)
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

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE 9GB", "example.com", "bb22", "203.0.113.10")}, t0); err != nil {
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

	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{renamed}, t0); err != nil {
		t.Fatal(err)
	}
	if a := m.cfg.Xray.ActiveServer; a.Name != "DE 10GB" || a.Seq != 3 {
		t.Fatalf("active %+v, want it left as it was", a)
	}
}

// A manual refresh can write between a round's download and its publication.
// The merge reads the file as it is then: the manual refresh's copies are the
// stored ones, and a download that differs from them only in what the panel
// picked writes nothing.
func TestPublishRefresh_MergesWithTheFileAsItIsThen(t *testing.T) {
	manual := reality("DE", "b.example", "cc33", "203.0.113.10") // the round itself read aa11
	m := &memStore{subs: []Subscription{alphaWith(manual)}, saveErr: errors.New("the file was written"), configErr: errors.New("the config was written")}

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{reality("DE", "a.example", "bb22", "203.0.113.10")}, t0.Add(time.Hour))

	if err != nil || res.Wrote || !bytes.Equal(m.subs[0].Servers[0].Outbound, manual.Outbound) {
		t.Fatalf("result %+v, err %v, file %+v", res, err, m.subs[0])
	}
}

// An operator that rotates a REALITY key and keeps the name makes another
// server: it comes in fresh, and the record that named the old one is left
// alone - neither renamed nor cleared.
func TestPublishRefresh_ARotatedKeyIsANewServer(t *testing.T) {
	m := &memStore{subs: []Subscription{alphaWith(reality("DE", "s.example", "aa11", "203.0.113.10"))}}
	m.cfg.Xray.ActiveServer = &ActiveServer{Subscription: "0a1b2c3d", Name: "DE", Address: "de.example", Port: 443, Seq: 3}
	rotated := reality("DE 2", "s.example", "aa11", "203.0.113.10")
	rotated.Outbound = json.RawMessage(strings.Replace(string(rotated.Outbound), "pk-1", "pk-2", 1))

	res, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, []Server{rotated}, t0.Add(time.Hour))

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
