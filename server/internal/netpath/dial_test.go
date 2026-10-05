package netpath

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDialPath_None(t *testing.T) {
	conn, err := DialPath(context.Background(), Path{}, "tcp", "service.example:443")
	if !errors.Is(err, errNoPath) || conn != nil {
		t.Fatalf("conn=%v err=%v", conn, err)
	}
}

func TestDialPath_DirectTCP4(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()
	conn, err := DialPath(context.Background(), Path{Kind: KindDirect}, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
	}
}

func TestPathDialer_SOCKSKeepsHostname(t *testing.T) {
	var gotAddr string
	d := pathDialer{
		lookupIPv4: func(context.Context, string) ([]net.IP, error) {
			t.Error("SOCKS must not resolve the target locally")
			return nil, errors.New("unexpected lookup")
		},
		socksDial: func(ctx context.Context, port int, network, addr string) (net.Conn, error) {
			gotAddr = addr
			if port != 12346 || network != "tcp" {
				t.Errorf("port=%d network=%s", port, network)
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	p := Path{Kind: KindSOCKS, SOCKSPort: 12346}
	conn, err := d.dial(context.Background(), p, "tcp", "service.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if gotAddr != "service.example:443" {
		t.Fatalf("SOCKS target %q", gotAddr)
	}
}

func TestDialDirect_LookupHasDeadline(t *testing.T) {
	var had bool
	var budget time.Duration
	d := pathDialer{
		lookupIPv4: func(ctx context.Context, host string) ([]net.IP, error) {
			if deadline, ok := ctx.Deadline(); ok {
				had = true
				budget = time.Until(deadline)
			}
			return nil, errors.New("stop")
		},
	}
	_, _ = d.dial(context.Background(), Path{Kind: KindDirect}, "tcp", "service.example:443")
	if !had {
		t.Fatal("direct DNS lookup has no dial timeout")
	}
	if budget <= 29*time.Second || budget > 30*time.Second {
		t.Fatalf("direct DNS budget %s, want the 30s dial timeout", budget)
	}
}

func TestDialIPv4s_ReservesTimeForLaterAddresses(t *testing.T) {
	var mu sync.Mutex
	var secondRemain time.Duration
	sawSecond := false
	d := pathDialer{
		tcpDial: func(ctx context.Context, network, addr string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if host == "203.0.113.20" {
				mu.Lock()
				sawSecond = true
				if dl, ok := ctx.Deadline(); ok {
					secondRemain = time.Until(dl)
				}
				mu.Unlock()
				c1, c2 := net.Pipe()
				_ = c2.Close()
				return c1, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := d.dialIPv4s(ctx, []net.IP{net.IPv4(203, 0, 113, 10), net.IPv4(203, 0, 113, 20)}, "443", nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second address: %v", err)
	}
	_ = conn.Close()
	if elapsed > 4*time.Second {
		t.Fatalf("first address consumed the budget: %s", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawSecond {
		t.Fatal("second address not tried")
	}
	if secondRemain < time.Second {
		t.Fatalf("second address remaining %s", secondRemain)
	}
}

func TestDialIPv4s_SplitsCallerDeadline(t *testing.T) {
	var firstBudget time.Duration
	var sawSecond bool
	d := pathDialer{
		tcpDial: func(ctx context.Context, network, addr string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if host == "203.0.113.10" {
				if deadline, ok := ctx.Deadline(); ok {
					firstBudget = time.Until(deadline)
				}
				return nil, errors.New("first address unavailable")
			}
			sawSecond = true
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := d.dialIPv4s(ctx, []net.IP{net.IPv4(203, 0, 113, 10), net.IPv4(203, 0, 113, 20)}, "443", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if firstBudget <= 0 || firstBudget > 6*time.Second || !sawSecond {
		t.Fatalf("first budget=%s second=%t; IPv4 attempts must split the caller's 8s deadline", firstBudget, sawSecond)
	}
}

func TestPathDialer_TunnelBindsIface(t *testing.T) {
	var gotIface string
	var gotMark uint32
	d := pathDialer{
		lookupIPv4: func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.IPv4(203, 0, 113, 10)}, nil
		},
		bindControl: func(iface string, mark uint32) func(network, address string, c syscall.RawConn) error {
			gotIface = iface
			gotMark = mark
			return func(network, address string, c syscall.RawConn) error { return nil }
		},
		tcpDial: func(ctx context.Context, network, addr string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			if network != "tcp4" {
				t.Errorf("network %s", network)
			}
			if control == nil {
				t.Error("tunnel TCP dial has no socket control")
			}
			return nil, errors.New("dial skipped")
		},
	}
	p := Path{Kind: KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}
	_, _ = d.dial(context.Background(), p, "tcp", "service.example:443")
	if gotIface != "tun12" {
		t.Fatalf("iface %q", gotIface)
	}
	if gotMark != 0x10000 {
		t.Fatalf("mark 0x%x", gotMark)
	}
}

func TestLookupTunnel_FallsBackAfterDNSTimeout(t *testing.T) {
	silent, good := dnsFixtures(t)
	d := pathDialer{
		dnsDial: func(ctx context.Context, network, address string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			return dialDNSFixture(ctx, network, address, silent, good)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := d.lookupTunnel(ctx, "tun0", 0, "vpn-director-dns-test.example.")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(203, 0, 113, 10)) {
		t.Fatalf("ips %v", ips)
	}
}

func TestDial_TunnelDNSAndTCPShareMark(t *testing.T) {
	silent, good := dnsFixtures(t)
	p := Path{Kind: KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}
	type controlCall struct {
		network string
		address string
		iface   string
		mark    uint32
	}
	var mu sync.Mutex
	var calls []controlCall
	var firstDNSBudget time.Duration
	var tcpBudget time.Duration
	d := pathDialer{
		bindControl: func(iface string, mark uint32) func(string, string, syscall.RawConn) error {
			return func(network, address string, _ syscall.RawConn) error {
				mu.Lock()
				calls = append(calls, controlCall{network, address, iface, mark})
				mu.Unlock()
				return nil
			}
		},
		dnsDial: func(ctx context.Context, network, address string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			if control == nil {
				return nil, errors.New("tunnel DNS dial has no socket control")
			}
			if err := control(network, address, nil); err != nil {
				return nil, err
			}
			if address == "8.8.8.8:53" {
				if deadline, ok := ctx.Deadline(); ok {
					mu.Lock()
					if budget := time.Until(deadline); budget > firstDNSBudget {
						firstDNSBudget = budget
					}
					mu.Unlock()
				}
			}
			return dialDNSFixture(ctx, network, address, silent, good)
		},
		tcpDial: func(ctx context.Context, network, address string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
			if control == nil {
				return nil, errors.New("tunnel TCP dial has no socket control")
			}
			if err := control(network, address, nil); err != nil {
				return nil, err
			}
			if deadline, ok := ctx.Deadline(); ok {
				mu.Lock()
				tcpBudget = time.Until(deadline)
				mu.Unlock()
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.dial(ctx, p, "tcp", "vpn-director-dns-test.example.:443")
	if err != nil {
		t.Fatalf("dial after the first DNS timed out: %v", err)
	}
	_ = conn.Close()

	mu.Lock()
	defer mu.Unlock()
	firstDNS, secondDNS, tcp := false, false, false
	for _, call := range calls {
		if call.iface != p.Iface || call.mark != p.Mark {
			t.Fatalf("control %+v, want iface %q and mark 0x%x", call, p.Iface, p.Mark)
		}
		switch call.address {
		case "8.8.8.8:53":
			firstDNS = true
		case "1.1.1.1:53":
			secondDNS = true
		case "203.0.113.10:443":
			tcp = true
			if call.network != "tcp4" {
				t.Fatalf("TCP network %q, want tcp4", call.network)
			}
		default:
			t.Fatalf("unexpected control target %+v", call)
		}
	}
	if !firstDNS || !secondDNS || !tcp {
		t.Fatalf("both DNS controls and TCP must run: %+v", calls)
	}
	if firstDNSBudget < 1500*time.Millisecond || firstDNSBudget > 2*time.Second {
		t.Fatalf("first DNS budget %s, want the 2s per-server cap", firstDNSBudget)
	}
	if tcpBudget < time.Second {
		t.Fatalf("first DNS consumed the TCP budget: %s", tcpBudget)
	}
}

func TestLookupIPv4_IPv4Literal(t *testing.T) {
	for _, p := range []Path{{Kind: KindDirect}, {Kind: KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}} {
		ips, err := LookupIPv4(context.Background(), p, "203.0.113.10")
		if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(203, 0, 113, 10)) {
			t.Fatalf("%s: ips=%v err=%v", p, ips, err)
		}
	}
}

func TestLookupIPv4_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, p := range []Path{{Kind: KindDirect}, {Kind: KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}} {
		if _, err := LookupIPv4(ctx, p, "cancelled-lookup.example."); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: err=%v, want context.Canceled", p, err)
		}
	}
}

func TestProductionDialer_Timeout(t *testing.T) {
	d := newProductionDialer(nil)
	if d.Timeout != 30*time.Second {
		t.Fatalf("Timeout=%s; DefaultTransport uses 30s", d.Timeout)
	}
}

func dnsFixtures(t *testing.T) (net.PacketConn, net.PacketConn) {
	t.Helper()
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, err := silent.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	good, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = good.Close() })
	go serveDNSA(good, net.IPv4(203, 0, 113, 10))
	return silent, good
}

func dialDNSFixture(ctx context.Context, network, address string, silent, good net.PacketConn) (net.Conn, error) {
	switch network {
	case "udp", "udp4", "udp6":
		network = "udp4"
	case "tcp", "tcp4", "tcp6":
		network = "tcp4"
	}
	var target string
	switch address {
	case "8.8.8.8:53":
		target = silent.LocalAddr().String()
	case "1.1.1.1:53":
		target = good.LocalAddr().String()
	default:
		return nil, errors.New("unexpected DNS server")
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}

func serveDNSA(pc net.PacketConn, ip net.IP) {
	v4 := ip.To4()
	if v4 == nil {
		return
	}
	buf := make([]byte, 2048)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		hdr, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		hdr.Response = true
		hdr.RecursionAvailable = true
		b := dnsmessage.NewBuilder(make([]byte, 0, 512), hdr)
		b.EnableCompression()
		if err := b.StartQuestions(); err != nil {
			continue
		}
		if err := b.Question(q); err != nil {
			continue
		}
		if err := b.StartAnswers(); err != nil {
			continue
		}
		if err := b.AResource(dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		}, dnsmessage.AResource{A: [4]byte{v4[0], v4[1], v4[2], v4[3]}}); err != nil {
			continue
		}
		msg, err := b.Finish()
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(msg, addr)
	}
}
