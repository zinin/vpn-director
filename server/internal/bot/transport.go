package bot

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

// PermanentError wraps errors that should not be retried (config errors, auth failures).
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// PathSource is the current Telegram API path and the sink for dial/TLS failures.
type PathSource interface {
	Current() Path
	ReportFailure(Path)
}

// IdleCloser is implemented by a PathSource that must close keep-alives on a path change.
// The callback receives the path the new transport generation is bound to.
type IdleCloser interface {
	RegisterIdleCloser(func(Path))
}

var errNoPath = errors.New("telegram API unreachable on every path")

var errRetiredPath = errors.New("telegram API path retired")

type pathDialer struct {
	lookupIPv4  func(ctx context.Context, host string) ([]net.IP, error)
	bindControl func(iface string, mark uint32) func(network, address string, c syscall.RawConn) error
	socksDial   func(ctx context.Context, port int, network, addr string) (net.Conn, error)
	tcpDial     func(ctx context.Context, network, addr string, control func(network, address string, c syscall.RawConn) error) (net.Conn, error)
	dnsDial     func(ctx context.Context, network, address string) (net.Conn, error)
}

// productionDialer is written only at initialisation; tests inject a dialer
// through newPathClientWith instead of writing here.
var productionDialer pathDialer

// Matches http.DefaultTransport's net.Dialer.Timeout. NewPathClient overwrites
// DialContext, so this must be set on the replacement dialer or a SYN-drop
// hangs getUpdates until the kernel retry budget (~minutes).
const productionDialTimeout = 30 * time.Second

func newProductionDialer(control func(network, address string, c syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: productionDialTimeout, Control: control}
}

// DialPath connects using p. The caller snapshots p; this function does not.
func DialPath(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
	return productionDialer.dial(ctx, p, network, addr)
}

// lookupIPv4OnPath resolves host the way DialPath would for p: system DNS on
// a direct path, 8.8.8.8 then 1.1.1.1 over the bound device on a tunnel.
func lookupIPv4OnPath(ctx context.Context, p Path, host string) ([]net.IP, error) {
	return productionDialer.lookupIPs(ctx, p, host)
}

func (d *pathDialer) lookupIPs(ctx context.Context, p Path, host string) ([]net.IP, error) {
	if p.kind == kindTunnel {
		return d.lookupTunnel(ctx, p.iface, p.mark, host)
	}
	return d.lookupDirect(ctx, host)
}

func (d *pathDialer) dial(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, productionDialTimeout)
	defer cancel()
	switch p.kind {
	case kindSOCKS:
		return d.dialSOCKS(ctx, p, addr)
	case kindTunnel:
		return d.dialTunnel(ctx, p, addr)
	case kindDirect:
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
	return fn(ctx, p.socksPort, "tcp", addr)
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
	ips, err := d.lookupTunnel(ctx, p.iface, p.mark, host)
	if err != nil {
		return nil, err
	}
	// Call bindControl even when tcpDial is injected so tests can record iface.
	return d.dialIPv4s(ctx, ips, port, d.control(p.iface, p.mark))
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

// Cap per nameserver so a silent 8.8.8.8 cannot consume the whole probe budget
// and skip 1.1.1.1.
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
		r := &net.Resolver{
			PreferGo: true, // cgo resolver ignores Dial
			Dial:     d.tunnelDNSDial(iface, mark, dns),
		}
		ips, err := r.LookupIP(dnsCtx, "ip4", host)
		cancel()
		if err == nil {
			return ips, nil
		}
		slog.Debug("Tunnel DNS lookup failed", "dns", dns, "iface", iface, "host", host, "error", err)
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (d *pathDialer) tunnelDNSDial(iface string, mark uint32, dns string) func(ctx context.Context, network, address string) (net.Conn, error) {
	if d.dnsDial != nil {
		return func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.dnsDial(ctx, network, dns)
		}
	}
	control := d.control(iface, mark)
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		// Ignore the resolver's address so resolv.conf is not used.
		switch network {
		case "udp", "udp4", "udp6":
			network = "udp4"
		default:
			network = "tcp4"
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

// partialDeadline is net.Dialer's per-address slice of a shared deadline:
// remaining/n, but at least 2s unless less time is left.
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

type pathCtxKey struct{}

type pollCtxKey struct{}

type pollWait struct {
	cancel context.CancelFunc
}

func isTelegramPoll(req *http.Request) bool {
	return strings.Contains(req.URL.Path, "/getUpdates")
}

type pathTransport struct {
	src   PathSource
	mu    sync.Mutex
	bound Path
	base  *http.Transport
	gen   *connGen
	polls []*pollWait
	// dialer nil means the package's production dialer. The field exists so a
	// test injects a dialer instead of writing to that package variable.
	dialer *pathDialer
	// afterBoundSnapshot is for tests: runs after the RoundTrip snapshot,
	// before dispatch. Production is nil.
	afterBoundSnapshot func()
	// nonPollTimeout bounds getMe, setMyCommands, sendMessage. Zero means
	// productionDialTimeout. getUpdates is not bounded (Client.Timeout is 0).
	nonPollTimeout time.Duration
}

// connGen is one Transport generation. Retired generations reject new
// getUpdates dials; they do not close sockets already used for sendMessage.
type connGen struct {
	mu      sync.Mutex
	retired bool
}

func newConnGen() *connGen {
	return &connGen{}
}

func (g *connGen) allow(poll bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !(g.retired && poll)
}

func (g *connGen) retire() {
	g.mu.Lock()
	g.retired = true
	g.mu.Unlock()
}

type pollBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *pollBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *pollBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *pollBody) finish() {
	b.once.Do(b.done)
}

func newPathBase(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	base.DialContext = dial
	base.ForceAttemptHTTP2 = false
	tlsCfg := base.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	} else {
		tlsCfg = tlsCfg.Clone()
	}
	tlsCfg.NextProtos = []string{"http/1.1"}
	base.TLSClientConfig = tlsCfg
	return base
}

// NewPathClient returns an HTTP client that dials through src.Current().
// Timeout is left at zero so getUpdates can long-poll.
func NewPathClient(src PathSource) *http.Client {
	return newPathClientWith(src, nil)
}

// newPathClientWith is NewPathClient with an injected dialer, so a test can
// replace one dial without writing to productionDialer.
func newPathClientWith(src PathSource, d *pathDialer) *http.Client {
	t := &pathTransport{src: src, bound: src.Current(), dialer: d}
	t.gen = newConnGen()
	t.base = t.attach(t.gen)
	if ic, ok := src.(IdleCloser); ok {
		ic.RegisterIdleCloser(t.retireBase)
	}
	return &http.Client{Transport: t}
}

func (t *pathTransport) attach(gen *connGen) *http.Transport {
	return newPathBase(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return t.dialOn(ctx, network, addr, gen)
	})
}

// retireBase installs a new Transport bound to next so keep-alives from the
// previous path cannot be reused, and cancels in-flight getUpdates so polling
// is not pinned to a blackholed path. Other Bot API calls on the old generation
// are left to finish.
func (t *pathTransport) retireBase(next Path) {
	t.mu.Lock()
	old := t.base
	oldGen := t.gen
	polls := t.polls
	t.polls = nil
	t.bound = next
	t.gen = newConnGen()
	t.base = t.attach(t.gen)
	t.mu.Unlock()
	old.CloseIdleConnections()
	oldGen.retire()
	for _, p := range polls {
		p.cancel()
	}
}

func (t *pathTransport) unregisterPoll(p *pollWait) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.polls {
		if x == p {
			t.polls = append(t.polls[:i], t.polls[i+1:]...)
			return
		}
	}
}

func (t *pathTransport) CloseIdleConnections() {
	t.mu.Lock()
	base := t.base
	t.mu.Unlock()
	base.CloseIdleConnections()
}

func (t *pathTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var pw *pollWait
	if isTelegramPoll(req) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		ctx = context.WithValue(ctx, pollCtxKey{}, true)
		pw = &pollWait{cancel: cancel}
	}
	t.mu.Lock()
	p := t.bound
	base := t.base
	if pw != nil {
		t.polls = append(t.polls, pw)
	}
	t.mu.Unlock()
	if t.afterBoundSnapshot != nil && isTelegramPoll(req) {
		t.afterBoundSnapshot()
	}
	var stopTimeout context.CancelFunc
	if pw == nil {
		timeout := t.nonPollTimeout
		if timeout <= 0 {
			timeout = productionDialTimeout
		}
		ctx, stopTimeout = context.WithTimeout(ctx, timeout)
	}
	ctx = context.WithValue(ctx, pathCtxKey{}, p)
	req = req.WithContext(ctx)
	var handshakeMu sync.Mutex
	var handshakeErr error
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				handshakeMu.Lock()
				handshakeErr = err
				handshakeMu.Unlock()
			}
		},
	}))
	resp, err := base.RoundTrip(req)
	handshakeMu.Lock()
	hs := handshakeErr
	handshakeMu.Unlock()
	if err != nil && (isPathFailure(err) || hs != nil) {
		t.src.ReportFailure(p)
	}
	var onBody []func()
	if pw != nil {
		if err != nil || resp == nil {
			t.unregisterPoll(pw)
		} else {
			onBody = append(onBody, func() { t.unregisterPoll(pw) })
		}
	}
	if stopTimeout != nil {
		if err != nil || resp == nil {
			stopTimeout()
		} else {
			onBody = append(onBody, stopTimeout)
		}
	}
	if len(onBody) > 0 && resp != nil {
		resp.Body = &pollBody{ReadCloser: resp.Body, done: func() {
			for _, f := range onBody {
				f()
			}
		}}
	}
	return resp, err
}

func (t *pathTransport) dialOn(ctx context.Context, network, addr string, gen *connGen) (net.Conn, error) {
	p, _ := ctx.Value(pathCtxKey{}).(Path)
	// Set once at construction and never mutated, so it needs no lock.
	d := t.dialer
	if d == nil {
		d = &productionDialer
	}
	c, err := d.dial(ctx, p, network, addr)
	if err != nil {
		return nil, err
	}
	poll, _ := ctx.Value(pollCtxKey{}).(bool)
	if !gen.allow(poll) {
		_ = c.Close()
		return nil, errRetiredPath
	}
	return c, nil
}

func socksListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func isPathFailure(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op)
}
