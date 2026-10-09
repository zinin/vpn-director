package bot

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
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

const defaultNonPollTimeout = 30 * time.Second

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
	dial  func(context.Context, Path, string, string) (net.Conn, error)
	// afterBoundSnapshot is for tests: runs after the RoundTrip snapshot,
	// before dispatch. Production is nil.
	afterBoundSnapshot func()
	// nonPollTimeout bounds getMe, setMyCommands, sendMessage. Zero means
	// defaultNonPollTimeout. getUpdates is not bounded (Client.Timeout is 0).
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
	return newPathClientWith(src, netpath.DialPath)
}

func newPathClientWith(src PathSource, dial func(context.Context, Path, string, string) (net.Conn, error)) *http.Client {
	if dial == nil {
		dial = netpath.DialPath
	}
	t := &pathTransport{src: src, bound: src.Current(), dial: dial}
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
			timeout = defaultNonPollTimeout
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
	c, err := t.dial(ctx, p, network, addr)
	if err != nil {
		if p.Kind == netpath.KindNone {
			return nil, errNoPath
		}
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
