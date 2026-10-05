package bot

import (
	"reflect"
	"testing"

	"github.com/zinin/vpn-director/server/internal/netpath"
)

func TestPathAlias(t *testing.T) {
	if reflect.TypeOf(Path{}) != reflect.TypeOf(netpath.Path{}) {
		t.Fatal("bot.Path must be an alias of netpath.Path")
	}
	want := netpath.Path{Kind: netpath.KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}
	var p Path = want
	var got netpath.Path = p
	if got != want {
		t.Fatalf("shared path %+v, want %+v", got, want)
	}
}

func TestSelectPath(t *testing.T) {
	direct := Path{Kind: netpath.KindDirect}
	socks := Path{Kind: netpath.KindSOCKS}
	tun := Path{Kind: netpath.KindTunnel, ID: "ovpnc2"}
	tunB := Path{Kind: netpath.KindTunnel, ID: "OpenVPN0"}

	if p := selectPath(tun, true, true, socks); !p.Same(direct) {
		t.Fatalf("direct wins over live tunnel: %s", p)
	}
	if p := selectPath(socks, false, true, tun); !p.Same(socks) {
		t.Fatalf("keep live socks while tunnel also live: %s", p)
	}
	if p := selectPath(tun, false, true, socks); !p.Same(tun) {
		t.Fatalf("keep live tunnel: %s", p)
	}
	if p := selectPath(Path{}, false, false, socks); !p.Same(socks) {
		t.Fatalf("no current, socks first: %s", p)
	}
	if p := selectPath(socks, false, false, tun); !p.Same(tun) {
		t.Fatalf("dead socks, take replacement: %s", p)
	}
	if p := selectPath(direct, false, false, Path{}); p.String() != "none" {
		t.Fatalf("all dead: %s", p)
	}
	if p := selectPath(tunB, false, false, socks); !p.Same(socks) {
		t.Fatalf("dead current, socks replacement: %s", p)
	}
}
