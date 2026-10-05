package netpath

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
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
	reach := ReachTCP4(func(ctx context.Context, n, addr string) (net.Conn, error) {
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
	reach := ReachTCP4(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	if reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("a closed port is unreachable")
	}
}

func TestReachTCP4_AddressesWeDoNotDialCountAsReachable(t *testing.T) {
	dials := 0
	reach := ReachTCP4(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	})
	for _, ip := range []string{"192.168.1.10", "127.0.0.1", "100.64.0.1", "2001:db8::1", "not-an-ip"} {
		if !reach(context.Background(), ip, 443) {
			t.Fatalf("%s: an address we do not dial counts as reachable", ip)
		}
	}
	if dials != 0 {
		t.Fatalf("dials %d, want none", dials)
	}
}

func TestReachTCP4_PassesCancellationAndClosesSuccessfulConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen context.Context
	reach := ReachTCP4(func(got context.Context, _, _ string) (net.Conn, error) {
		seen = got
		return nil, got.Err()
	})
	if reach(ctx, "203.0.113.10", 443) {
		t.Fatal("a cancelled dial must be unreachable")
	}
	if seen != ctx {
		t.Fatal("the dial must receive the caller's context")
	}

	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	reach = ReachTCP4(func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	})
	if !reach(context.Background(), "203.0.113.10", 443) {
		t.Fatal("a successful dial must be reachable")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("successful reachability connection was not closed: %v", err)
	}
}
