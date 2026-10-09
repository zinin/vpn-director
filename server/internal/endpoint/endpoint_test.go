package endpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestPerAddress_ACopyPerAddressAndServersWithoutOneAsTheyAre(t *testing.T) {
	got := PerAddress([]vpnconfig.Server{
		{Name: "Two", Address: "two.example", IPs: []string{"192.0.2.1", "", "192.0.2.2"}},
		{Name: "None", Address: "none.example"},
	})
	if len(got) != 3 || got[0].IPs[0] != "192.0.2.1" || got[1].IPs[0] != "192.0.2.2" || got[2].Name != "None" {
		t.Fatalf("copies %+v", got)
	}
	if len(got[0].IPs) != 1 {
		t.Fatalf("a copy keeps %v, want its one address", got[0].IPs)
	}
}

// Two names on one endpoint are one server to Xray and one line in the
// monitor's results; two addresses of one name are two. The key never carries
// the credential the outbound holds.
func TestKey_OneEndpointOneKeyWithoutItsCredential(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"secret-uuid","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	a := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: ob}
	b := a
	b.Name, b.Subscription = "Germany-2", "1b2c3d4e"
	c := a
	c.IPs = []string{"192.0.2.2"}

	if Key(a) != Key(b) {
		t.Fatal("two names on one endpoint have two keys")
	}
	if Key(a) == Key(c) {
		t.Fatal("two addresses share a key")
	}
	if k := Key(a); len(k) != 64 || strings.Contains(k, "secret") {
		t.Fatalf("key %q, want 64 hex digits", k)
	}
}

// A record from before outbounds were stored dials its flat fields; they make
// its key, so two such records with other credentials are two endpoints.
func TestKey_ALegacyRecordIsKeyedByItsFlatFields(t *testing.T) {
	a := vpnconfig.Server{Name: "Legacy", Address: "l.example", Port: 443, UUID: "u1", Security: "reality",
		PublicKey: "pk", SNI: "www.example.org", Fingerprint: "chrome", IPs: []string{"192.0.2.9"}}
	b := a
	b.UUID = "u2"
	if Key(a) == "" || Key(a) == Key(b) {
		t.Fatalf("keys %q and %q", Key(a), Key(b))
	}
	if DialKey(a) != "" {
		t.Fatal("a legacy record got a DialKey; the walk would take it for another")
	}
}

func TestKeys_OnePerAddressInPerAddressOrder(t *testing.T) {
	s := vpnconfig.Server{Name: "Two", Address: "two.example", Port: 443, IPs: []string{"192.0.2.1", "192.0.2.2"},
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"two.example","port":443,"password":"p"}]}}`)}
	copies := PerAddress([]vpnconfig.Server{s})
	if keys := Keys(s); len(keys) != 2 || keys[0] != Key(copies[0]) || keys[1] != Key(copies[1]) {
		t.Fatalf("keys %v", keys)
	}
	if keys := Keys(vpnconfig.Server{Name: "None", Address: "none.example", Port: 443}); len(keys) != 1 {
		t.Fatalf("a server without an address has keys %v, want one", keys)
	}
}

func TestServerForDial_UsesResolvedIPKeepsHostnameSNI(t *testing.T) {
	s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "tls"})
	if s.Address != "203.0.113.50" {
		t.Fatalf("address %q", s.Address)
	}
	if s.SNI != "oslo.example" {
		t.Fatalf("sni %q", s.SNI)
	}
	s = ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "tls", SNI: "cdn.example"})
	if s.SNI != "cdn.example" {
		t.Fatalf("explicit sni %q", s.SNI)
	}
}

// REALITY's server name is the site the handshake borrows, never the proxy's
// own host. An entry without one cannot connect - the Web UI and /xray refuse
// it - and the hostname must not make it look complete to the walk.
func TestServerForDial_LeavesARealitySNIEmpty(t *testing.T) {
	s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.50"}, Security: "reality"})
	if s.Address != "203.0.113.50" {
		t.Fatalf("address %q", s.Address)
	}
	if s.SNI != "" {
		t.Fatalf("sni %q; a REALITY entry without one must stay without one", s.SNI)
	}
}

// A stored outbound gets the IP in its own address slot, whatever the
// protocol keeps it in; the record and the outbound agree on the address.
func TestServerForDial_WritesTheIPIntoTheOutbound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outbound string
		path     []string
	}{
		{"vless vnext", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u"}]}]}}`, []string{"settings", "vnext", "0", "address"}},
		{"vless flat", `{"protocol":"vless","settings":{"address":"oslo.example","port":443,"id":"u"}}`, []string{"settings", "address"}},
		{"trojan", `{"protocol":"trojan","settings":{"servers":[{"address":"oslo.example","port":443,"password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
		{"shadowsocks", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"oslo.example","port":8388,"method":"aes-256-gcm","password":"p"}]}}`, []string{"settings", "servers", "0", "address"}},
		{"hysteria", `{"protocol":"hysteria","settings":{"version":2,"address":"oslo.example","port":443}}`, []string{"settings", "address"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ServerForDial(vpnconfig.Server{Address: "oslo.example", IPs: []string{"", "203.0.113.50"}, Outbound: json.RawMessage(tc.outbound)})
			if s.Address != "203.0.113.50" {
				t.Fatalf("address %q", s.Address)
			}
			var ob interface{}
			if err := json.Unmarshal(s.Outbound, &ob); err != nil {
				t.Fatal(err)
			}
			v := ob
			for _, key := range tc.path {
				switch node := v.(type) {
				case map[string]interface{}:
					v = node[key]
				case []interface{}:
					v = node[0]
				}
			}
			if v != "203.0.113.50" {
				t.Fatalf("outbound %s", s.Outbound)
			}
		})
	}
}

// Dialing an IP must not change the name the server is reached by: an empty
// TLS server name gets the hostname, and so does the Host of a transport
// without security; with TLS Xray takes that Host from the server name, and
// an explicit value stays.
func TestServerForDial_KeepsTheHostnameWhereTheSourceLeftItToTheAddress(t *testing.T) {
	dial := func(address, outbound string) map[string]interface{} {
		t.Helper()
		s := ServerForDial(vpnconfig.Server{Address: address, IPs: []string{"203.0.113.50"}, Outbound: json.RawMessage(outbound)})
		var ob map[string]interface{}
		if err := json.Unmarshal(s.Outbound, &ob); err != nil {
			t.Fatal(err)
		}
		return ob["streamSettings"].(map[string]interface{})
	}
	ss := dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"ws","security":"tls","wsSettings":{"path":"/ws"}}}`)
	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "cdn.example" {
		t.Fatalf("tls %v", tls)
	}
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; with TLS the Host follows the server name", ws)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"httpupgrade","security":"none"}}`)
	if hu := ss["httpupgradeSettings"].(map[string]interface{}); hu["host"] != "cdn.example" {
		t.Fatalf("httpupgrade %v", hu)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"ws","wsSettings":{"headers":{"Host":"front.example"}}}}`)
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; a Host header the source set stays the Host", ws)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"ws","wsSettings":{"headers":{"host":"front.example"}}}}`)
	if ws := ss["wsSettings"].(map[string]interface{}); ws["host"] != nil {
		t.Fatalf("ws %v; Xray takes a host header in any case, so it stays the Host", ws)
	}
	ss = dial("cdn.example", `{"protocol":"trojan","settings":{"servers":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"sni.example"}}}`)
	if tls := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "sni.example" {
		t.Fatalf("tls %v; an explicit server name stays", tls)
	}
	ss = dial("198.51.100.7", `{"protocol":"trojan","settings":{"servers":[{"address":"198.51.100.7","port":443}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	if _, ok := ss["tlsSettings"]; ok {
		t.Fatalf("stream %v; an IP source has no hostname to keep", ss)
	}
	ss = dial("oslo.example", `{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443}]},"streamSettings":{"network":"xhttp","security":"reality","realitySettings":{"serverName":"www.example.org"}}}`)
	if _, ok := ss["xhttpSettings"]; ok {
		t.Fatalf("stream %v; REALITY gives xhttp its Host", ss)
	}
	// Xray reads xhttpSettings over splithttpSettings and drops the other, so
	// the Host goes into the one the record has.
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"splithttp","splithttpSettings":{"path":"/secret","mode":"packet-up"}}}`)
	if splithttp, _ := ss["splithttpSettings"].(map[string]interface{}); splithttp["host"] != "cdn.example" || splithttp["path"] != "/secret" {
		t.Fatalf("splithttp %v", splithttp)
	}
	if _, ok := ss["xhttpSettings"]; ok {
		t.Fatalf("stream %v; a new xhttpSettings would replace the splithttpSettings", ss)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"xhttp","security":"none","xhttpSettings":{"path":"/a"},"splithttpSettings":{"path":"/b"}}}`)
	if xhttp, _ := ss["xhttpSettings"].(map[string]interface{}); xhttp["host"] != "cdn.example" {
		t.Fatalf("xhttp %v", xhttp)
	}
	if splithttp, _ := ss["splithttpSettings"].(map[string]interface{}); splithttp["host"] != nil {
		t.Fatalf("splithttp %v; Xray reads the xhttpSettings", splithttp)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"xhttp","security":"none"}}`)
	if xhttp, _ := ss["xhttpSettings"].(map[string]interface{}); xhttp["host"] != "cdn.example" {
		t.Fatalf("stream %v", ss)
	}
	// A cleartext gRPC stream takes its :authority from the address when
	// grpcSettings names none; with TLS, Xray takes the server name.
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none","grpcSettings":{"serviceName":"svc"}}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "cdn.example" || grpc["serviceName"] != "svc" {
		t.Fatalf("grpc %v", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none","grpcSettings":{"serviceName":"svc","authority":"front.example"}}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "front.example" {
		t.Fatalf("grpc %v; an explicit authority stays", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":443}]},"streamSettings":{"network":"grpc","security":"tls","grpcSettings":{"serviceName":"svc"}}}`)
	if tls, _ := ss["tlsSettings"].(map[string]interface{}); tls["serverName"] != "cdn.example" {
		t.Fatalf("tls %v", tls)
	}
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != nil {
		t.Fatalf("grpc %v; with TLS the authority follows the server name", grpc)
	}
	ss = dial("cdn.example", `{"protocol":"vless","settings":{"vnext":[{"address":"cdn.example","port":80}]},"streamSettings":{"network":"grpc","security":"none"}}`)
	if grpc, _ := ss["grpcSettings"].(map[string]interface{}); grpc["authority"] != "cdn.example" {
		t.Fatalf("stream %v", ss)
	}
}

func TestDialKey_OneEndpointUnderTwoNamesIsOneServer(t *testing.T) {
	ob := json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"de.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`)
	a := vpnconfig.Server{Name: "Germany-1", Address: "de.example", Port: 443, IPs: []string{"192.0.2.1"}, Outbound: ob}
	b := a
	b.Name = "Germany-2"
	c := a
	c.IPs = []string{"192.0.2.2"}

	if DialKey(a) != DialKey(b) {
		t.Fatal("two names on one endpoint dial differently")
	}
	if DialKey(a) == DialKey(c) {
		t.Fatal("two addresses dial alike")
	}
	if DialKey(vpnconfig.Server{Name: "Legacy", Address: "l.example", Port: 443}) != "" {
		t.Fatal("a record without an outbound has a key")
	}
}

// Shared substore records pin the outbound and key of every walk copy.
func TestEndpoints_TheSharedFixturesMatchTheWalk(t *testing.T) {
	subs, err := vpnconfig.LoadSubscriptions(filepath.Join("..", "..", "..", "testdata", "substore"))
	if err != nil {
		t.Fatal(err)
	}
	servers := vpnconfig.AllServers(subs)
	want := []struct {
		address  string
		outbound string
	}{
		{"198.51.100.20", `{"protocol":"trojan","settings":{"servers":[{"address":"198.51.100.20","password":"fixture-password","port":8443}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"beta.example.net"}}}`},
		{"192.0.2.10", `{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.10","port":443,"users":[{"encryption":"none","id":"00000000-0000-4000-8000-000000000001"}]}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"a.example.com"}}}`},
		{"192.0.2.11", `{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.11","port":443,"users":[{"encryption":"none","id":"00000000-0000-4000-8000-000000000002"}]}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"b.example.com"}}}`},
		{"203.0.113.30", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"203.0.113.30","method":"chacha20-ietf-poly1305","password":"fixture-password","port":443}]}}`},
	}
	copies := PerAddress(servers)
	if len(copies) != len(want) {
		t.Fatalf("%d fixture copies, want %d", len(copies), len(want))
	}
	if !reflect.DeepEqual(copies, servers) {
		t.Fatal("PerAddress changed the fixtures' one-address records")
	}
	for i, c := range copies {
		t.Run(c.Subscription+"/"+c.Name, func(t *testing.T) {
			wantDial := servers[i]
			wantDial.Address = want[i].address
			wantDial.Outbound = json.RawMessage(want[i].outbound)
			if got := ServerForDial(c); !reflect.DeepEqual(got, wantDial) {
				t.Fatal("ServerForDial differs from the fixture's walk outbound")
			}
			if got := DialKey(c); got != want[i].outbound {
				t.Fatal("DialKey differs from the fixture's walk outbound")
			}
			sum := sha256.Sum256([]byte(want[i].outbound))
			wantKey := hex.EncodeToString(sum[:])
			if got := Key(c); got != wantKey {
				t.Fatalf("Key %q, want %q", got, wantKey)
			}
			if got := Keys(servers[i]); !reflect.DeepEqual(got, []string{wantKey}) {
				t.Fatalf("Keys %v, want [%s]", got, wantKey)
			}
		})
	}
}
