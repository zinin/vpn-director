package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

// ProbeURL is what a check fetches through an endpoint. Plain HTTP: the watch
// probes https://www.gstatic.com/generate_204, but a TLS handshake with that
// host costs 6.8 KB of the ~12 KB a check would take, and the two disagree
// only on a server that blocks outbound port 80.
const ProbeURL = "http://www.gstatic.com/generate_204"

// checkTimeout bounds one attempt. A var so a test can shorten it.
var checkTimeout = 10 * time.Second

// statusError is an answer other than 204.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// probeGet fetches url through the SOCKS5 proxy at socksAddr with the account
// user:pass and answers the time to the response headers: the full cost of a
// new connection, the server's handshake and the request both. A redirect is
// not followed; anything but 204 is a failure.
func probeGet(ctx context.Context, socksAddr, user, pass, url string) (time.Duration, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, &proxy.Auth{User: user, Password: pass}, &net.Dialer{Timeout: checkTimeout})
	if err != nil {
		return 0, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return 0, errors.New("socks dialer lacks DialContext")
	}
	tr := &http.Transport{DialContext: cd.DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: checkTimeout}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       checkTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	latency := time.Since(start)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 0, &statusError{code: resp.StatusCode}
	}
	return latency, nil
}

// classify names a failed check for the page: "HTTP <code>", "timeout" or
// "connection closed" - Xray drops the SOCKS connection when its outbound
// fails. Nothing of the error's own text: it can quote the proxy's address.
func classify(err error) string {
	var se *statusError
	var ne net.Error
	switch {
	case errors.As(err, &se):
		return se.Error()
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "connection closed"
	}
}
