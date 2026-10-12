package vpnconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A server's group is its protocol and its security: the kind a replacement is
// picked from first. The transport is no part of it.
func TestServerGroup_ProtocolAndSecurity(t *testing.T) {
	cases := []struct {
		name string
		s    Server
		want string
	}{
		{"reality", reality("DE", "www.example.com", "aa11"), "vless+reality"},
		{"websocket over tls", webSocket("DE", "cdn.example", "/de"), "vless+tls"},
		{"trojan", Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"t.example","port":443,"password":"p"}]},"streamSettings":{"network":"tcp","security":"tls"}}`)}, "trojan+tls"},
		{"shadowsocks", Server{Outbound: json.RawMessage(`{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss.example","port":8388,"method":"aes-256-gcm","password":"p"}]}}`)}, "shadowsocks"},
		{"security none", Server{Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":80,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"ws","security":"none"}}`)}, "vless"},
		// Xray reads the protocol and the security without regard to case.
		{"security in capitals", Server{Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"TLS"}}`)}, "vless+tls"},
		{"protocol in capitals", Server{Outbound: json.RawMessage(`{"protocol":"VLESS","settings":{"vnext":[{"address":"a.example","port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"Reality"}}`)}, "vless+reality"},
		{"none in capitals", Server{Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":80,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"ws","security":"None"}}`)}, "vless"},
		{"legacy record", Server{Name: "Old", Address: "old.example", Port: 443}, ""},
		{"unreadable", Server{Outbound: json.RawMessage(`{"protocol":`)}, ""},
		{"null", Server{Outbound: json.RawMessage(`null`)}, ""},
		{"array", Server{Outbound: json.RawMessage(`[{"protocol":"vless"}]`)}, ""},
		{"no protocol", Server{Outbound: json.RawMessage(`{"settings":{}}`)}, ""},
	}
	for _, tc := range cases {
		if got := tc.s.Group(); got != tc.want {
			t.Errorf("%s: group %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The mark follows the server, not what a panel picks at random for each
// download or the name it gives the server.
func TestIdentityMark_FollowsTheServerNotThePanelsPicks(t *testing.T) {
	a := IdentityMark(reality("DE", "www.example.com", "aa11"))
	if len(a) != 16 {
		t.Fatalf("mark %q, want 16 hex digits", a)
	}
	if b := IdentityMark(reality("DE 9GB", "www.other.com", "bb22")); b != a {
		t.Fatalf("the REALITY picks and the name moved the mark: %q, %q", a, b)
	}
	if IdentityMark(webSocket("DE", "a.example", "/x1")) != IdentityMark(webSocket("DE", "b.example", "/y22")) {
		t.Fatal("a generated WebSocket Host and path moved the mark")
	}
	moved := reality("DE", "www.example.com", "aa11")
	moved.Outbound = realityOutbound("nl.example", "www.example.com", "aa11")
	if IdentityMark(moved) == a {
		t.Fatal("a new address kept the mark")
	}
	rekeyed := reality("DE", "www.example.com", "aa11")
	rekeyed.Outbound = json.RawMessage(strings.Replace(string(rekeyed.Outbound), `"u-1"`, `"u-2"`, 1))
	if IdentityMark(rekeyed) == a {
		t.Fatal("a new UUID kept the mark")
	}
	if m := IdentityMark(Server{Name: "Old", Address: "old.example", Port: 443}); m != "" {
		t.Fatalf("a legacy record has mark %q, want none", m)
	}
}

// Every writer of the selection goes through NewActiveServer: the record keeps
// the server's mark and group, and the walk's copy into preferred_server keeps
// them too. A record without them writes neither key.
func TestNewActiveServer_RecordsIdentityAndGroup(t *testing.T) {
	s := reality("DE", "www.example.com", "aa11")
	s.Subscription = "0a1b2c3d"
	a := NewActiveServer(s)
	if a.Identity != IdentityMark(s) || a.Group != "vless+reality" {
		t.Fatalf("record %+v", a)
	}
	r := RecordActiveServer(&ActiveServer{Seq: 4}, s)
	if r.Seq != 5 || r.Identity != a.Identity || r.Group != a.Group {
		t.Fatalf("record %+v", r)
	}
	cfg := &VPNDirectorConfig{}
	cfg.Xray.ActiveServer = a
	other := reality("NL", "www.example.com", "aa11")
	other.Subscription = "0a1b2c3d"
	other.Outbound = realityOutbound("nl.example", "www.example.com", "aa11")
	RecordWalkedServer(cfg, other)
	if p := cfg.Xray.PreferredServer; p == nil || p.Identity != a.Identity || p.Group != a.Group {
		t.Fatalf("preferred %+v, want the record of DE", p)
	}
	data, err := json.Marshal(&ActiveServer{Name: "DE", Address: "de.example", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "identity") || strings.Contains(string(data), "group") {
		t.Fatalf("a record without them writes them: %s", data)
	}
}

// A rename follows the server, whose outbound - and so its mark and group -
// did not change.
func TestPublishRefresh_ARenameKeepsTheRecordsMarkAndGroup(t *testing.T) {
	stored := reality("DE 10GB", "www.example.com", "aa11", "203.0.113.10")
	m := &memStore{subs: []Subscription{alphaWith(stored)}}
	stored.Subscription = "0a1b2c3d"
	m.cfg.Xray.ActiveServer = RecordActiveServer(nil, stored)
	want := *m.cfg.Xray.ActiveServer
	if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, []Server{reality("DE 9GB", "www.example.com", "aa11", "203.0.113.10")}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := m.cfg.Xray.ActiveServer
	if got == nil || got.Name != "DE 9GB" || got.Identity != want.Identity || got.Group != want.Group || got.Seq != want.Seq {
		t.Fatalf("record %+v, want %+v under the new name", got, want)
	}
}

// A panel lists twins of one name, address and port - a Trojan fallback
// listed before the REALITY Oslo the user runs - and a refresh renames one of
// them. The record follows the twin it was written from, by its kind and mark:
// the twin's rename leaves it as it was, and its own server's moves it.
func TestPublishRefresh_ARecordFollowsTheTwinItNames(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11", "203.0.113.10")
	twin := Server{Name: "Oslo", Address: "de.example", Port: 443, IPs: []string{"203.0.113.10"}, Outbound: trojanTo("de.example")}
	twinRenamed, osloRenamed := twin, oslo
	twinRenamed.Name, osloRenamed.Name = "Oslo Trojan", "Oslo REALITY"
	for _, tc := range []struct {
		name   string
		listed []Server
		want   string
	}{
		{"the twin renamed", []Server{twinRenamed, oslo}, "Oslo"},
		{"its own server renamed", []Server{twin, osloRenamed}, "Oslo REALITY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &memStore{subs: []Subscription{alphaWith(twin, oslo)}}
			chosen := oslo
			chosen.Subscription = "0a1b2c3d"
			m.cfg.Xray.ActiveServer = RecordActiveServer(nil, chosen)
			if _, err := PublishRefresh(m.update, m.files(), "0a1b2c3d", alphaLink, t0, tc.listed, t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if a := m.cfg.Xray.ActiveServer; a.Name != tc.want || a.Group != "vless+reality" {
				t.Fatalf("active %+v, want %q", a, tc.want)
			}
		})
	}
}

// The choice is a server's subscription, name and kind. A panel can give one
// name to several inbounds; a record without a group knows no kind, and its
// subscription and name decide.
func TestSameChoice(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11")
	oslo.Subscription = "0a1b2c3d"
	record := NewActiveServer(oslo)
	rotated := oslo
	rotated.Address, rotated.Outbound = "nl.example", realityOutbound("nl.example", "www.example.com", "aa11")
	trojan := Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443, Outbound: trojanTo("de.example")}
	legacy := Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	other := oslo
	other.Subscription = "1b2c3d4e"
	bergen := oslo
	bergen.Name = "Bergen"
	noGroup := &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	cases := []struct {
		name string
		a    *ActiveServer
		s    Server
		want bool
	}{
		{"the same server", record, oslo, true},
		{"the same kind at a new address", record, rotated, true},
		{"another kind at the same address and port", record, trojan, false},
		{"an entry without an outbound", record, legacy, false},
		{"another subscription", record, other, false},
		{"another name", record, bergen, false},
		{"a record without a group, another kind", noGroup, trojan, true},
		{"no record", nil, oslo, false},
	}
	for _, tc := range cases {
		if got := SameChoice(tc.a, tc.s); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// SameChoiceRecord holds the server another record names to the choice by that
// record's group. The first record is the choice: a second one without a group
// is not of its kind when the choice has one.
func TestSameChoiceRecord(t *testing.T) {
	oslo := reality("Oslo", "www.example.com", "aa11")
	oslo.Subscription = "0a1b2c3d"
	choice := NewActiveServer(oslo)
	trojan := NewActiveServer(Server{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443, Outbound: trojanTo("de.example")})
	rotated := *choice
	rotated.Address = "nl.example"
	noGroup := &ActiveServer{Subscription: "0a1b2c3d", Name: "Oslo", Address: "de.example", Port: 443}
	other := *choice
	other.Subscription = "1b2c3d4e"
	cases := []struct {
		name string
		a, b *ActiveServer
		want bool
	}{
		{"the same kind at a new address", choice, &rotated, true},
		{"another kind", choice, trojan, false},
		{"a record without a group for a choice with one", choice, noGroup, false},
		{"a choice without a group", noGroup, trojan, true},
		{"another subscription", choice, &other, false},
		{"no record", choice, nil, false},
		{"no choice", nil, choice, false},
	}
	for _, tc := range cases {
		if got := SameChoiceRecord(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
