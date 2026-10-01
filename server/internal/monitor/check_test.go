package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// socks5Server is a SOCKS5 proxy for these tests, standing in for the prober:
// one account, CONNECT only, and it dials the target itself.
func socks5Server(t *testing.T, user, pass string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(c, user, pass)
		}
	}()
	return l.Addr().String()
}

func serveSOCKS5(c net.Conn, user, pass string) {
	defer c.Close()
	read := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			return nil
		}
		return b
	}
	if h := read(2); h == nil || read(int(h[1])) == nil { // VER NMETHODS METHODS
		return
	}
	c.Write([]byte{5, 2}) // username/password
	h := read(2)          // VER ULEN
	if h == nil {
		return
	}
	u := read(int(h[1]))
	if u == nil {
		return
	}
	length := read(1)
	if length == nil {
		return
	}
	p := read(int(length[0]))
	if p == nil {
		return
	}
	if string(u) != user || string(p) != pass {
		c.Write([]byte{1, 1})
		return
	}
	c.Write([]byte{1, 0})
	req := read(4) // VER CMD RSV ATYP
	if req == nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := read(4)
		if ip == nil {
			return
		}
		host = net.IP(append([]byte(nil), ip...)).String()
	case 3:
		length := read(1)
		if length == nil {
			return
		}
		domain := read(int(length[0]))
		if domain == nil {
			return
		}
		host = string(domain)
	default:
		return
	}
	pb := read(2)
	if pb == nil {
		return
	}
	port := int(pb[0])<<8 | int(pb[1])
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(target, c)
	io.Copy(c, target)
}

func answering(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/generate_204"
}

func TestProbeGet_A204ThroughTheAccountIsASuccess(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	latency, err := probeGet(context.Background(), proxyAddr, "e0", "secret", url)
	if err != nil || latency <= 0 {
		t.Fatalf("probeGet() = %s, %v", latency, err)
	}
}

// A portal page, a block page, a redirect: anything but 204 is no internet.
func TestProbeGet_AnyOtherAnswerIsAFailure(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	for code, h := range map[int]http.HandlerFunc{
		403: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		302: func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		200: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>login</html>")) },
	} {
		_, err := probeGet(context.Background(), proxyAddr, "e0", "secret", answering(t, h))
		if got, want := classify(err), "HTTP "+strconv.Itoa(code); got != want {
			t.Errorf("answer %d classified %q (%v), want %q", code, got, err, want)
		}
	}
}

// The prober refuses another account's password, so a local process cannot
// borrow it.
func TestProbeGet_AnotherPasswordIsRefused(t *testing.T) {
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	if _, err := probeGet(context.Background(), proxyAddr, "e0", "guess", url); err == nil {
		t.Fatal("a wrong password got through")
	}
}

func TestClassify_ATimeoutIsATimeout(t *testing.T) {
	old := checkTimeout
	checkTimeout = 200 * time.Millisecond
	defer func() { checkTimeout = old }()
	proxyAddr := socks5Server(t, "e0", "secret")
	url := answering(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	_, err := probeGet(context.Background(), proxyAddr, "e0", "secret", url)
	if got := classify(err); got != "timeout" {
		t.Fatalf("classified %q (%v), want timeout", got, err)
	}
	if got := classify(errors.New("read: connection reset by peer")); got != "connection closed" {
		t.Fatalf("classified %q", got)
	}
}

type truncatedSOCKSConn struct {
	*bytes.Reader
	closed bool
}

func (c *truncatedSOCKSConn) Write(p []byte) (int, error)    { return len(p), nil }
func (c *truncatedSOCKSConn) Close() error                   { c.closed = true; return nil }
func (*truncatedSOCKSConn) LocalAddr() net.Addr              { return nil }
func (*truncatedSOCKSConn) RemoteAddr() net.Addr             { return nil }
func (*truncatedSOCKSConn) SetDeadline(time.Time) error      { return nil }
func (*truncatedSOCKSConn) SetReadDeadline(time.Time) error  { return nil }
func (*truncatedSOCKSConn) SetWriteDeadline(time.Time) error { return nil }

func TestSOCKS5Helper_TruncatedHandshakesCloseWithoutPanic(t *testing.T) {
	auth := append([]byte{5, 1, 2, 1, 2, 'e', '0', 6}, []byte("secret")...)
	for _, target := range [][]byte{{5, 1, 0, 1, 127, 0, 0, 1, 0, 80}, append(append([]byte{5, 1, 0, 3, 9}, []byte("localhost")...), 0, 80)} {
		full := append(append([]byte(nil), auth...), target...)
		for i := 0; i < len(full); i++ {
			t.Run(strconv.Itoa(int(target[3]))+"/"+strconv.Itoa(i), func(t *testing.T) {
				conn := &truncatedSOCKSConn{Reader: bytes.NewReader(full[:i])}
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("truncated handshake panicked: %v", r)
					}
					if !conn.closed {
						t.Error("truncated connection not closed")
					}
				}()
				serveSOCKS5(conn, "e0", "secret")
			})
		}
	}
}

func TestSOCKS5Helper_ACancelledHandshakeEndsItsWaiter(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		serveSOCKS5(server, "e0", "secret")
	}()
	t.Cleanup(func() { client.Close(); server.Close() })
	if _, err := client.Write([]byte{5, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte{1, 2, 'e', '0'}); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("cancelled handshake panicked: %v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled handshake left a waiter")
	}
}
