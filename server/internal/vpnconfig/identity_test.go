package vpnconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// realityOutbound is a VLESS REALITY outbound on address, with the server
// name and the short id a panel picked for one download.
func realityOutbound(address, sni, sid string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":%q,"fingerprint":"chrome","publicKey":"pk-1","shortId":%q,"spiderX":"/"}}}`, address, sni, sid))
}

// reality is a server on de.example as an import stores it.
func reality(name, sni, sid string, ips ...string) Server {
	return Server{Name: name, Address: "de.example", Port: 443, IPs: ips, Outbound: realityOutbound("de.example", sni, sid)}
}

// trojanTo is a Trojan outbound on address.
func trojanTo(address string) json.RawMessage {
	return json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"` + address + `","port":443,"password":"p"}]}}`)
}

// 3x-ui and Marzban pick the REALITY server name and short id at random for
// every download: two downloads of one server are one server.
func TestServerIdentity_WhatAPanelPicksAtRandomDoesNotCount(t *testing.T) {
	a := ServerIdentity(reality("DE", "www.example.com", "aa11"))
	b := ServerIdentity(reality("DE 9GB", "example.com", "bb22"))
	if a == "" || a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

func TestServerIdentity_EverythingElseCounts(t *testing.T) {
	base := ServerIdentity(reality("DE", "www.example.com", "aa11"))
	key := reality("DE", "www.example.com", "aa11")
	key.Outbound = json.RawMessage(strings.Replace(string(key.Outbound), "pk-1", "pk-2", 1))
	moved := Server{Name: "DE", Outbound: realityOutbound("fr.example", "www.example.com", "aa11")}
	for what, got := range map[string]string{"public key": ServerIdentity(key), "address": ServerIdentity(moved)} {
		if got == base {
			t.Errorf("a server with another %s has the same identity", what)
		}
	}
	// On a CDN the TLS server name picks the backend: two names are two servers.
	tls := func(sni string) Server {
		return Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"cdn.example","port":443,"password":"p"}]},"streamSettings":{"security":"tls","tlsSettings":{"serverName":"` + sni + `"}}}`)}
	}
	if ServerIdentity(tls("a.example")) == ServerIdentity(tls("b.example")) {
		t.Error("two TLS server names have one identity")
	}
}

// Xray reads its config with encoding/json, which folds case: a pick spelled
// another way is still the pick, and the identity drops it all the same.
func TestServerIdentity_MatchesTheKeysAsXrayDoes(t *testing.T) {
	canonical := Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"realitySettings":{"publicKey":"pk-1","shortId":"aa11","serverName":"a.example","spiderX":"/a"}}}`)}
	folded := Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"realitySettings":{"publicKey":"pk-1","ShortID":"bb22","SERVERNAME":"b.example","Spiderx":"/b"}}}`)}
	if a, b := ServerIdentity(canonical), ServerIdentity(folded); a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

// An xhttp extra carries a whole download stream, REALITY picks and all.
func TestServerIdentity_LooksIntoTheDownloadStream(t *testing.T) {
	xhttp := func(sni, sid string) Server {
		return Server{Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"vless","streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"publicKey":"pk-1","serverName":"a.example","shortId":"aa11"},"xhttpSettings":{"extra":{"downloadSettings":{"address":"dl.example","port":443,"network":"xhttp","security":"reality","realitySettings":{"publicKey":"pk-2","serverName":%q,"shortId":%q}}}}}}`, sni, sid))}
	}
	if a, b := ServerIdentity(xhttp("x.example", "cc33")), ServerIdentity(xhttp("y.example", "dd44")); a == "" || a != b {
		t.Fatalf("identities %q and %q, want one", a, b)
	}
}

func TestServerIdentity_KeyOrderDoesNotCount(t *testing.T) {
	a := Server{Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"fr.example","port":443,"password":"p"}]}}`)}
	b := Server{Outbound: json.RawMessage(`{ "settings": {"servers": [{"password": "p", "port": 443, "address": "fr.example"}]}, "protocol": "trojan" }`)}
	if ServerIdentity(a) != ServerIdentity(b) {
		t.Fatal("key order changed the identity")
	}
}

func TestServerIdentity_ARecordWithoutAnOutboundHasNone(t *testing.T) {
	legacy := Server{Name: "DE", Address: "de.example", Port: 443, UUID: "u-1", Security: "reality", ShortID: "aa11"}
	if id := ServerIdentity(legacy); id != "" {
		t.Fatalf("legacy record identity %q", id)
	}
	if id := ServerIdentity(Server{Outbound: json.RawMessage(`not json`)}); id != "" {
		t.Fatalf("unreadable outbound identity %q", id)
	}
}

func TestPairServers_EachTakesTheFirstFreeStoredTwin(t *testing.T) {
	fr := Server{Name: "FR", Address: "fr.example", Port: 443, Outbound: trojanTo("fr.example")}
	stored := []Server{reality("DE-1", "a.example", "aa11"), reality("DE-2", "a.example", "bb22"), fr}
	fresh := []Server{
		reality("DE-1", "b.example", "cc33"),
		reality("DE-2", "b.example", "dd44"),
		{Name: "NL", Address: "nl.example", Port: 443, Outbound: trojanTo("nl.example")},
		{Name: "Legacy", Address: "l.example", Port: 443},
	}
	if got := pairServers(stored, fresh); !reflect.DeepEqual(got, []int{0, 1, -1, -1}) {
		t.Fatalf("pairs %v, want [0 1 -1 -1]", got)
	}
}
