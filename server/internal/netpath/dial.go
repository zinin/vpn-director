package netpath

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

var errNoPath = errors.New("no network path")

type pathDialer struct {
	lookupIPv4  func(ctx context.Context, host string) ([]net.IP, error)
	bindControl func(iface string, mark uint32) func(network, address string, c syscall.RawConn) error
	socksDial   func(ctx context.Context, port int, network, addr string) (net.Conn, error)
	tcpDial     func(ctx context.Context, network, addr string, control func(network, address string, c syscall.RawConn) error) (net.Conn, error)
	dnsDial     func(ctx context.Context, network, address string, control func(network, address string, c syscall.RawConn) error) (net.Conn, error)
}

// Match http.DefaultTransport's dial timeout even with a replacement DialContext.
const productionDialTimeout = 30 * time.Second

func newProductionDialer(control func(network, address string, c syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: productionDialTimeout, Control: control}
}

// DialPath connects using the caller's path snapshot.
func DialPath(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
	var d pathDialer
	return d.dial(ctx, p, network, addr)
}

// LookupIPv4 uses system DNS directly, or 8.8.8.8 then 1.1.1.1 over the tunnel.
func LookupIPv4(ctx context.Context, p Path, host string) ([]net.IP, error) {
	var d pathDialer
	return d.lookupIPs(ctx, p, host)
}

func (d *pathDialer) lookupIPs(ctx context.Context, p Path, host string) ([]net.IP, error) {
	if p.Kind == KindTunnel {
		return d.lookupTunnel(ctx, p.Iface, p.Mark, host)
	}
	return d.lookupDirect(ctx, host)
}

func (d *pathDialer) dial(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, productionDialTimeout)
	defer cancel()
	switch p.Kind {
	case KindSOCKS:
		return d.dialSOCKS(ctx, p, addr)
	case KindTunnel:
		return d.dialTunnel(ctx, p, addr)
	case KindDirect:
		return d.dialDirect(ctx, addr)
	default:
		return nil, errNoPath
	}
}

func (d *pathDialer) dialSOCKS(ctx context.Context, p Path, addr string) (net.Conn, error) {
	fn := d.socksDial
	if fn == nil {
		fn = defaultSOCKSDial
	}
	return fn(ctx, p.SOCKSPort, "tcp", addr)
}

func defaultSOCKSDial(ctx context.Context, port int, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, productionDialTimeout)
	defer cancel()
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil, &net.Dialer{Timeout: productionDialTimeout})
	if err != nil {
		return nil, err
	}
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return dialer.Dial(network, addr)
}

func (d *pathDialer) dialDirect(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := d.lookupDirect(ctx, host)
	if err != nil {
		return nil, err
	}
	return d.dialIPv4s(ctx, ips, port, nil)
}

func (d *pathDialer) dialTunnel(ctx context.Context, p Path, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := d.lookupTunnel(ctx, p.Iface, p.Mark, host)
	if err != nil {
		return nil, err
	}
	return d.dialIPv4s(ctx, ips, port, d.control(p.Iface, p.Mark))
}

func (d *pathDialer) control(iface string, mark uint32) func(network, address string, c syscall.RawConn) error {
	if d.bindControl != nil {
		return d.bindControl(iface, mark)
	}
	return tunnelSocketControl(iface, mark)
}

func (d *pathDialer) lookupDirect(ctx context.Context, host string) ([]net.IP, error) {
	if ip := ipv4Literal(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if d.lookupIPv4 != nil {
		return d.lookupIPv4(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip4", host)
}

var tunnelDNSServers = []string{"8.8.8.8:53", "1.1.1.1:53"}

// A silent first nameserver must leave time for the second.
const tunnelDNSTimeout = 2 * time.Second

func (d *pathDialer) lookupTunnel(ctx context.Context, iface string, mark uint32, host string) ([]net.IP, error) {
	if ip := ipv4Literal(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if d.lookupIPv4 != nil {
		return d.lookupIPv4(ctx, host)
	}
	var lastErr error
	for _, dns := range tunnelDNSServers {
		dnsCtx, cancel := context.WithTimeout(ctx, tunnelDNSTimeout)
		deadline, _ := dnsCtx.Deadline()
		dial := d.tunnelDNSDial(iface, mark, dns)
		r := &net.Resolver{
			PreferGo: true, // cgo resolver ignores Dial
			Dial: func(dialCtx context.Context, network, address string) (net.Conn, error) {
				// Resolver may replace the lookup context; keep this server's deadline.
				dialCtx, cancelDial := context.WithDeadline(dialCtx, deadline)
				defer cancelDial()
				return dial(dialCtx, network, address)
			},
		}
		ips, err := r.LookupIP(dnsCtx, "ip4", host)
		cancel()
		if err == nil {
			return ips, nil
		}
		slog.Debug("Tunnel DNS lookup failed", "dns", dns, "iface", iface, "host", host)
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *pathDialer) tunnelDNSDial(iface string, mark uint32, dns string) func(ctx context.Context, network, address string) (net.Conn, error) {
	control := d.control(iface, mark)
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		// Ignore resolv.conf's address and force IPv4 on the tunnel.
		switch network {
		case "udp", "udp4", "udp6":
			network = "udp4"
		default:
			network = "tcp4"
		}
		if d.dnsDial != nil {
			return d.dnsDial(ctx, network, dns, control)
		}
		return newProductionDialer(control).DialContext(ctx, network, dns)
	}
}

func (d *pathDialer) dialIPv4s(ctx context.Context, ips []net.IP, port string, control func(network, address string, c syscall.RawConn) error) (net.Conn, error) {
	var addrs []net.IP
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			addrs = append(addrs, v4)
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("no IPv4 address")
	}
	var lastErr error
	for i, v4 := range addrs {
		addrCtx := ctx
		var cancel context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			partial, err := partialDeadline(time.Now(), deadline, len(addrs)-i)
			if err != nil {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, err
			}
			addrCtx, cancel = context.WithDeadline(ctx, partial)
		}
		conn, err := d.doTCP(addrCtx, "tcp4", net.JoinHostPort(v4.String(), port), control)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Split net.Dialer's shared deadline: remaining/n, at least 2s unless less is left.
func partialDeadline(now, deadline time.Time, addrsRemaining int) (time.Time, error) {
	if deadline.IsZero() {
		return deadline, nil
	}
	timeRemaining := deadline.Sub(now)
	if timeRemaining <= 0 {
		return time.Time{}, context.DeadlineExceeded
	}
	timeout := timeRemaining / time.Duration(addrsRemaining)
	const saneMinimum = 2 * time.Second
	if timeout < saneMinimum {
		if timeRemaining < saneMinimum {
			timeout = timeRemaining
		} else {
			timeout = saneMinimum
		}
	}
	return now.Add(timeout), nil
}

func (d *pathDialer) doTCP(ctx context.Context, network, addr string, control func(network, address string, c syscall.RawConn) error) (net.Conn, error) {
	if d.tcpDial != nil {
		return d.tcpDial(ctx, network, addr, control)
	}
	return newProductionDialer(control).DialContext(ctx, network, addr)
}

func ipv4Literal(host string) net.IP {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	return ip.To4()
}
