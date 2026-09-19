package vpnconfig

import (
	"encoding/json"
	"testing"
)

func TestDecodeOutbound_KeepsNumbersAsWritten(t *testing.T) {
	ob, err := DecodeOutbound(json.RawMessage(`{"protocol":"vless","settings":{"port":443,"level":1.0}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(ob)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"protocol":"vless","settings":{"level":1.0,"port":443}}` {
		t.Fatalf("round trip %s", out)
	}
	if _, err := DecodeOutbound(json.RawMessage(`null`)); err == nil {
		t.Fatal("null is no outbound")
	}
	if _, err := DecodeOutbound(json.RawMessage(`[1]`)); err == nil {
		t.Fatal("an array is no outbound")
	}
}

func TestOutboundTarget(t *testing.T) {
	for _, tc := range []struct {
		name, outbound, want string
	}{
		{"vless vnext", `{"protocol":"vless","settings":{"vnext":[{"address":"a.example"}]}}`, "a.example"},
		{"vmess vnext", `{"protocol":"vmess","settings":{"vnext":[{"address":"b.example"}]}}`, "b.example"},
		{"trojan servers", `{"protocol":"trojan","settings":{"servers":[{"address":"c.example"}]}}`, "c.example"},
		{"shadowsocks servers", `{"protocol":"shadowsocks","settings":{"servers":[{"address":"d.example"}]}}`, "d.example"},
		{"vless flat", `{"protocol":"vless","settings":{"address":"e.example"}}`, "e.example"},
		{"trojan flat", `{"protocol":"trojan","settings":{"address":"f.example"}}`, "f.example"},
		{"hysteria", `{"protocol":"hysteria","settings":{"address":"g.example","vnext":[{"address":"not-this"}]}}`, "g.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ob, err := DecodeOutbound(json.RawMessage(tc.outbound))
			if err != nil {
				t.Fatal(err)
			}
			target := OutboundTarget(ob)
			if target == nil || target["address"] != tc.want {
				t.Fatalf("target %v, want address %q", target, tc.want)
			}
		})
	}
	if target := OutboundTarget(map[string]interface{}{"protocol": "vless"}); target != nil {
		t.Fatalf("target %v of an outbound without settings", target)
	}
}

func TestServerLabel(t *testing.T) {
	for _, tc := range []struct {
		server Server
		want   string
	}{
		{Server{Security: "reality", Network: "tcp"}, "vless·reality"},
		{Server{}, "vless·tls"},
		{Server{Security: "tls", Network: "ws"}, "vless·ws·tls"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"xhttp","security":"reality"}}`)}, "vless·xhttp·reality"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vless","streamSettings":{"network":"raw","security":"none"}}`)}, "vless"},
		{Server{Outbound: json.RawMessage(`{"protocol":"vmess","streamSettings":{"network":"ws"}}`)}, "vmess·ws"},
		{Server{Outbound: json.RawMessage(`{"protocol":"trojan","streamSettings":{"network":"tcp","security":"tls"}}`)}, "trojan·tls"},
		{Server{Outbound: json.RawMessage(`{"protocol":"shadowsocks","streamSettings":{"network":"tcp","security":"none"}}`)}, "ss"},
		{Server{Outbound: json.RawMessage(`{"protocol":"hysteria","streamSettings":{"network":"hysteria","security":"tls"}}`)}, "hysteria2"},
	} {
		if got := tc.server.Label(); got != tc.want {
			t.Errorf("Label() of %+v = %q, want %q", tc.server, got, tc.want)
		}
	}
}

// A record an import writes now carries no UUID of its own; the empty field
// must not appear in servers.json.
func TestServer_NoEmptyUUIDInTheFile(t *testing.T) {
	out, err := json.Marshal(Server{Name: "Oslo", Address: "oslo.example", Port: 443, Outbound: json.RawMessage(`{"protocol":"trojan"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"address":"oslo.example","port":443,"name":"Oslo","ips":null,"outbound":{"protocol":"trojan"}}` {
		t.Fatalf("marshal %s", out)
	}
}
