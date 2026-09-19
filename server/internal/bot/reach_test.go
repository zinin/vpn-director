package bot

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestReachTCP4_AnAddressThatAcceptsIsReachable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	var network, dialed string
	reach := reachTCP4(func(ctx context.Context, n, addr string) (net.Conn, error) {
		network, dialed = n, addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", ln.Addr().String())
	})
	if !reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("an address that accepts is reachable")
	}
	if network != "tcp4" || dialed != "203.0.113.10:443" {
		t.Fatalf("dialed %s %s, want tcp4 203.0.113.10:443", network, dialed)
	}
}

func TestReachTCP4_AClosedPortIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	reach := reachTCP4(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	if reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("a closed port is unreachable")
	}
}

func TestReachTCP4_AddressesTheBotDoesNotDialCountAsReachable(t *testing.T) {
	dials := 0
	reach := reachTCP4(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	})
	for _, ip := range []string{"192.168.1.10", "127.0.0.1", "100.64.0.1", "2001:db8::1", "not-an-ip"} {
		if !reach(context.Background(), ip, 443) {
			t.Fatalf("%s: an address the bot does not dial counts as reachable", ip)
		}
	}
	if dials != 0 {
		t.Fatalf("dials %d, want none", dials)
	}
}
