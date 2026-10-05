package bot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
)

type fakeSource struct {
	p      Path
	failed []Path
	idle   []func(Path)
}

func (f *fakeSource) Current() Path                    { return f.p }
func (f *fakeSource) ReportFailure(p Path)             { f.failed = append(f.failed, p) }
func (f *fakeSource) RegisterIdleCloser(fn func(Path)) { f.idle = append(f.idle, fn) }

func TestNewPathClient_ReportsDialFailure(t *testing.T) {
	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	// Nothing listens here; DialPath to this host:port fails.
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/", nil)
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if len(src.failed) != 1 || !src.failed[0].Same(src.p) {
		t.Fatalf("failures %+v", src.failed)
	}
}

func TestNewPathClientWith_DialInjectionIsPerClient(t *testing.T) {
	paths := []Path{
		{Kind: netpath.KindSOCKS, SOCKSPort: 23456},
		{Kind: netpath.KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000},
	}
	var mu sync.Mutex
	dials := make([]int, len(paths))
	sources := make([]*fakeSource, len(paths))
	clients := make([]*http.Client, len(paths))
	dialErrs := make([]*net.OpError, len(paths))
	for i, p := range paths {
		sources[i] = &fakeSource{p: p}
		dialErrs[i] = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("injected " + p.String() + " failure")}
		dial := func(ctx context.Context, got Path, network, addr string) (net.Conn, error) {
			mu.Lock()
			dials[i]++
			mu.Unlock()
			if !got.ParamsEqual(p) || network != "tcp" || addr != "api.example:80" {
				t.Errorf("dial path=%+v network=%q addr=%q", got, network, addr)
			}
			return nil, dialErrs[i]
		}
		clients[i] = newPathClientWith(sources[i], dial)
		defer clients[i].CloseIdleConnections()
	}
	for i, client := range clients {
		if _, err := client.Get("http://api.example/botTOKEN/getMe"); !errors.Is(err, dialErrs[i]) {
			t.Fatalf("client %d: err=%v, want its injected failure", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, p := range paths {
		if dials[i] != 1 || len(sources[i].failed) != 1 || !sources[i].failed[0].ParamsEqual(p) {
			t.Fatalf("client %d: dials=%d failures=%+v", i, dials[i], sources[i].failed)
		}
	}
}

// A peer that accepts TCP and closes after ClientHello makes crypto/tls
// return plain io.EOF, which is not a net.Error. RoundTrip must still
// ReportFailure so PathManager reselects instead of waiting for the 30s probe.
func TestNewPathClient_ReportsTLSCloseAfterClientHello(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 2048)
			_, _ = c.Read(buf)
			_ = c.Close()
		}
	}()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	_, err = client.Get("https://" + ln.Addr().String() + "/")
	if err == nil {
		t.Fatal("expected TLS handshake error")
	}
	if len(src.failed) != 1 || !src.failed[0].Same(src.p) {
		t.Fatalf("ReportFailure skipped for %v; failures %+v", err, src.failed)
	}
}

// WithClientTrace already composes with a previous ClientTrace. Copying those
// hooks and chaining them ourselves makes TLSHandshakeStart run twice.
// The test exercises the late TLS-handshake write and detects the race only
// under -race, so it is intentionally assertion-free.
func TestNewPathClient_HandshakeErrNoRace(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		time.Sleep(80 * time.Millisecond)
		_ = c.Close()
	}()
	client := NewPathClient(&fakeSource{p: Path{Kind: netpath.KindDirect}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Do(req)
	time.Sleep(120 * time.Millisecond)
}

func TestNewPathClient_ExistingTraceHooksRunOnce(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 2048)
		_, _ = c.Read(buf)
		_ = c.Close()
	}()

	start := make(chan struct{})
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		TLSHandshakeStart: func() { close(start) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := NewPathClient(&fakeSource{p: Path{Kind: netpath.KindDirect}})
	_, _ = client.Do(req)
	select {
	case <-start:
	case <-time.After(2 * time.Second):
		t.Fatal("TLSHandshakeStart not called")
	}
}

func TestNewPathClient_RegistersIdleCloser(t *testing.T) {
	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	_ = NewPathClient(src)
	if len(src.idle) != 1 {
		t.Fatalf("idle closers %d", len(src.idle))
	}
}

// CloseIdleConnections does not interrupt an in-use connection. getUpdates
// long-polls with Client.Timeout=0, so a blackholed path would pin the sole
// polling goroutine until the kernel TCP timeout. Retire must close that conn.
func TestNewPathClient_PathChangeAbortsInFlight(t *testing.T) {
	arrived := make(chan struct{})
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-hold
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(hold)

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	if len(src.idle) != 1 {
		t.Fatalf("idle closers %d", len(src.idle))
	}

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			errc <- resp.Body.Close()
			return
		}
		errc <- err
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the server")
	}

	src.idle[0](src.p)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("in-flight getUpdates must not survive a path change")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request still blocked after path change")
	}
}

func TestNewPathClient_PathChangeDoesNotAbortSend(t *testing.T) {
	arrived := make(chan struct{})
	hold := make(chan struct{})
	var holdOnce sync.Once
	release := func() { holdOnce.Do(func() { close(hold) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-hold
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer release()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/sendMessage")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			errc <- resp.Body.Close()
			return
		}
		errc <- err
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach the server")
	}

	src.idle[0](src.p)

	select {
	case err := <-errc:
		t.Fatalf("send aborted on path change: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not finish after release")
	}
}

func TestNewPathBase_AdvertisesOnlyHTTP11(t *testing.T) {
	tr := newPathBase(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no dial")
	})
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 still set")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil; cloned DefaultTransport still advertises h2")
	}
	found11 := false
	for _, p := range tr.TLSClientConfig.NextProtos {
		if p == "h2" || p == "h2c" {
			t.Fatalf("ALPN still has %q: %v", p, tr.TLSClientConfig.NextProtos)
		}
		if p == "http/1.1" {
			found11 = true
		}
	}
	if !found11 {
		t.Fatalf("ALPN %v", tr.TLSClientConfig.NextProtos)
	}
}

func TestNewPathClient_SendOnReusedPollConnSurvivesPathChange(t *testing.T) {
	sendArrived := make(chan struct{})
	holdSend := make(chan struct{})
	var holdOnce sync.Once
	release := func() { holdOnce.Do(func() { close(holdSend) }) }
	var arrivedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/botTOKEN/getUpdates" {
			w.WriteHeader(http.StatusOK)
			return
		}
		arrivedOnce.Do(func() { close(sendArrived) })
		<-holdSend
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer release()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/sendMessage")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			errc <- resp.Body.Close()
			return
		}
		errc <- err
	}()
	select {
	case <-sendArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach the server")
	}

	src.idle[0](src.p)
	select {
	case err := <-errc:
		t.Fatalf("send on reused poll conn aborted: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not finish after release")
	}
}

func TestNewPathClient_PollSeesRetireAcrossSnapshot(t *testing.T) {
	sendArrived := make(chan struct{})
	holdSend := make(chan struct{})
	pollArrived := make(chan struct{})
	holdPoll := make(chan struct{})
	var sendArrivedOnce, sendHoldOnce, pollArrivedOnce, pollHoldOnce sync.Once
	releaseSend := func() { sendHoldOnce.Do(func() { close(holdSend) }) }
	releasePoll := func() { pollHoldOnce.Do(func() { close(holdPoll) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getUpdates") {
			pollArrivedOnce.Do(func() { close(pollArrived) })
			<-holdPoll
			w.WriteHeader(http.StatusOK)
			return
		}
		sendArrivedOnce.Do(func() { close(sendArrived) })
		<-holdSend
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer releaseSend()
	defer releasePoll()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	pt := client.Transport.(*pathTransport)
	pt.base.MaxConnsPerHost = 1

	sendErr := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/sendMessage")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			sendErr <- resp.Body.Close()
			return
		}
		sendErr <- err
	}()
	select {
	case <-sendArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach the server")
	}

	pt.afterBoundSnapshot = func() { src.idle[0](src.p) }

	pollErr := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			pollErr <- resp.Body.Close()
			return
		}
		pollErr <- err
	}()

	releaseSend()
	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not finish")
	}

	select {
	case err := <-pollErr:
		if err == nil {
			t.Fatal("getUpdates completed on the retired path")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("getUpdates remains blocked on the retired path")
	}
}

func TestNewPathClient_PathChangeAbortsPollAfterHeaders(t *testing.T) {
	arrived := make(chan struct{})
	hold := make(chan struct{})
	var holdOnce sync.Once
	release := func() { holdOnce.Do(func() { close(hold) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/botTOKEN/getMe" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(arrived)
		<-hold
	}))
	defer srv.Close()
	defer release()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	resp, err := client.Get(srv.URL + "/botTOKEN/getMe")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
		if err != nil {
			errc <- err
			return
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		errc <- err
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("getUpdates headers did not arrive")
	}

	src.idle[0](src.p)
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("getUpdates body survived path change after headers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getUpdates still blocked after path change")
	}
}

// A dial that started before retire must not return a socket on the retired path.
func TestNewPathClient_RetireRejectsLateDial(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	dial := func(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return net.Dial(network, addr)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := newPathClientWith(src, dial)
	if len(src.idle) != 1 {
		t.Fatalf("idle closers %d", len(src.idle))
	}

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			errc <- resp.Body.Close()
			return
		}
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not start")
	}

	src.idle[0](src.p)
	unblock()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("late dial on a retired generation served a request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request still blocked after late dial")
	}
}

// CloseIdleConnections on a shared Transport is undone by the next RoundTrip
// (queueForIdleConn clears closeIdle). An in-flight getUpdates can then return
// its conn to the pool after a path switch. Retiring the Transport on the idle
// closer is what stops that reuse.
func TestNewPathClient_PathChangeDoesNotReuseOldConn(t *testing.T) {
	var mu sync.Mutex
	var addrs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		addrs = append(addrs, r.RemoteAddr)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	if len(src.idle) != 1 {
		t.Fatalf("idle closers %d", len(src.idle))
	}

	do := func() error {
		resp, err := client.Get(srv.URL)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}
	if err := do(); err != nil {
		t.Fatal(err)
	}
	src.idle[0](src.p)
	if err := do(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addrs) != 2 {
		t.Fatalf("requests %d addrs=%v", len(addrs), addrs)
	}
	if addrs[1] == addrs[0] {
		t.Fatalf("reused the pre-switch connection %s", addrs[0])
	}
}

// retireBase installs a new pool before PathManager stores current. A request
// in that window must dial the path passed to the closer, not the stale Current(),
// or the new pool keeps an old-path connection forever.
func TestNewPathClient_SwitchBindsNewPathBeforeCurrentUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	socks := Path{Kind: netpath.KindSOCKS, SOCKSPort: 12346}
	direct := Path{Kind: netpath.KindDirect}
	src := &fakeSource{p: socks}

	var mu sync.Mutex
	var dialed []Path
	dial := func(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, p)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	client := newPathClientWith(src, dial)
	if len(src.idle) != 1 {
		t.Fatalf("idle closers %d", len(src.idle))
	}
	src.idle[0](direct)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || !dialed[0].ParamsEqual(direct) {
		t.Fatalf("new pool dialed stale Current()=%s: %+v", src.Current(), dialed)
	}
}

func TestNewPathClient_NonPollRequestTimesOut(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(hold)

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	client.Transport.(*pathTransport).nonPollTimeout = 50 * time.Millisecond

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getMe")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-poll request has no timeout")
	}
}

func TestNewPathClient_GetUpdatesIgnoresNonPollTimeout(t *testing.T) {
	arrived := make(chan struct{})
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-hold
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(hold)

	src := &fakeSource{p: Path{Kind: netpath.KindDirect}}
	client := NewPathClient(src)
	client.Transport.(*pathTransport).nonPollTimeout = 50 * time.Millisecond

	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(srv.URL + "/botTOKEN/getUpdates")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			errc <- resp.Body.Close()
			return
		}
		errc <- err
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("getUpdates did not reach the server")
	}
	select {
	case err := <-errc:
		t.Fatalf("getUpdates timed out: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNewPathClient_NoClientTimeout(t *testing.T) {
	c := NewPathClient(&fakeSource{p: Path{Kind: netpath.KindDirect}})
	if c.Timeout != 0 {
		t.Fatalf("Timeout=%s; getUpdates long-polls", c.Timeout)
	}
}

func TestSocksListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if !socksListening(port) {
		t.Fatal("expected listening")
	}
	if socksListening(1) {
		t.Fatal("port 1 should be down")
	}
}

func TestIsPathFailure(t *testing.T) {
	if isPathFailure(nil) {
		t.Fatal("nil")
	}
	op := &net.OpError{Op: "dial", Err: errors.New("refused")}
	if !isPathFailure(op) {
		t.Fatal("op")
	}
	// Nothing listens here; the dial error must read as a path failure.
	_, err := http.Get("http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !isPathFailure(err) {
		t.Fatalf("dial to a closed port must be a path failure: %v", err)
	}
}
