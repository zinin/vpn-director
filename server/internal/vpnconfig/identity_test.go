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

// upgradeOutbound is a VLESS outbound over TLS on address and port whose
// stream holds transport under the key settings - wsSettings or
// httpupgradeSettings, however spelled - and runs the network that key names.
func upgradeOutbound(settings, address string, port int, transport string) json.RawMessage {
	network := strings.ToLower(strings.TrimSuffix(settings, "Settings"))
	return json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":%d,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"network":%q,"security":"tls","tlsSettings":{"serverName":"nl.example","fingerprint":"chrome"},%q:%s}}`, address, port, network, settings, transport))
}

// webSocket is a server on nl.example over WebSocket as an import stores it,
// with the Host and the path a panel generated for one download.
func webSocket(name, host, path string, ips ...string) Server {
	return Server{Name: name, Address: "nl.example", Port: 443, IPs: ips, Outbound: upgradeOutbound("wsSettings", "nl.example", 443, fmt.Sprintf(`{"host":%q,"path":%q}`, host, path))}
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

// Some panels generate the WebSocket and HTTPUpgrade Host and path anew for
// every download, to mask the traffic: two downloads of one server are one
// server, whether the Host comes as host or in the headers, and however Xray's
// folding lets the keys be spelled.
func TestServerIdentity_AnUpgradeHostAndPathAPanelGeneratesDoNotCount(t *testing.T) {
	downloads := map[string][2]string{
		"host and path": {`{"host":"a1.example","path":"/x7f"}`, `{"host":"q9z.example","path":"/kd83jd?ed=2048"}`},
		"headers Host":  {`{"path":"/p","headers":{"Host":"a1.example","User-Agent":"ua"}}`, `{"path":"/p","headers":{"Host":"q9z.example","User-Agent":"ua"}}`},
		"folded keys":   {`{"host":"a1.example","path":"/x7f","Headers":{"Host":"a1.example"}}`, `{"HOST":"q9z.example","Path":"/kd83jd","Headers":{"hOsT":"q9z.example"}}`},
	}
	for _, settings := range []string{"wsSettings", "httpupgradeSettings", "WSSettings", "HttpUpgradeSettings"} {
		for what, d := range downloads {
			a := ServerIdentity(Server{Outbound: upgradeOutbound(settings, "nl.example", 443, d[0])})
			b := ServerIdentity(Server{Outbound: upgradeOutbound(settings, "nl.example", 443, d[1])})
			if a == "" || a != b {
				t.Errorf("%s, %s: identities %q and %q, want one", settings, what, a, b)
			}
		}
	}
}

// Only the ws and httpupgrade Host and path are dropped: the xhttp Host and
// path, the gRPC service, the TLS server name, which picks the backend on a
// CDN, any other header and the address still tell two servers apart.
func TestServerIdentity_WhatNoPanelGeneratesStillCounts(t *testing.T) {
	vless := func(stream string) Server {
		return Server{Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"nl.example","port":443,"users":[{"id":"u-1","encryption":"none"}]}]},"streamSettings":{"security":"tls",` + stream + `}}`)}
	}
	download := func(host, path string) string {
		return fmt.Sprintf(`"network":"xhttp","xhttpSettings":{"path":"/up","extra":{"downloadSettings":{"address":"dl.example","port":443,"network":"xhttp","security":"tls","xhttpSettings":{"host":%q,"path":%q}}}}`, host, path)
	}
	ws := `{"host":"a1.example","path":"/x7f"}`
	for what, apart := range map[string][2]Server{
		"xhttp host and path":          {vless(`"network":"xhttp","xhttpSettings":{"host":"a.example","path":"/a"}`), vless(`"network":"xhttp","xhttpSettings":{"host":"b.example","path":"/b"}`)},
		"splithttp host and path":      {vless(`"network":"splithttp","splithttpSettings":{"host":"a.example","path":"/a"}`), vless(`"network":"splithttp","splithttpSettings":{"host":"b.example","path":"/b"}`)},
		"xhttp download host and path": {vless(download("a.example", "/a")), vless(download("b.example", "/b"))},
		"gRPC service name":            {vless(`"network":"grpc","grpcSettings":{"serviceName":"a"}`), vless(`"network":"grpc","grpcSettings":{"serviceName":"b"}`)},
		"gRPC authority":               {vless(`"network":"grpc","grpcSettings":{"serviceName":"s","authority":"a.example"}`), vless(`"network":"grpc","grpcSettings":{"serviceName":"s","authority":"b.example"}`)},
		"TLS server name":              {vless(`"network":"ws","tlsSettings":{"serverName":"a.example"},"wsSettings":` + ws), vless(`"network":"ws","tlsSettings":{"serverName":"b.example"},"wsSettings":` + ws)},
		"ws header other than Host":    {vless(`"network":"ws","wsSettings":{"headers":{"User-Agent":"a"}}`), vless(`"network":"ws","wsSettings":{"headers":{"User-Agent":"b"}}`)},
		"ws address":                   {{Outbound: upgradeOutbound("wsSettings", "nl.example", 443, ws)}, {Outbound: upgradeOutbound("wsSettings", "fr.example", 443, ws)}},
		"ws port":                      {{Outbound: upgradeOutbound("wsSettings", "nl.example", 443, ws)}, {Outbound: upgradeOutbound("wsSettings", "nl.example", 8443, ws)}},
	} {
		if a, b := ServerIdentity(apart[0]), ServerIdentity(apart[1]); a == "" || a == b {
			t.Errorf("%s: identities %q and %q, want two", what, a, b)
		}
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
	// A file can hold "outbound": null, and an outbound that is no object is
	// none: DecodeOutbound refuses it too.
	for _, raw := range []string{`null`, `"x"`, `7`, `[]`} {
		if id := ServerIdentity(Server{Outbound: json.RawMessage(raw)}); id != "" {
			t.Errorf("outbound %s identity %q", raw, id)
		}
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
