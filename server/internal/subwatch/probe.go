package subwatch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

const (
	ProbeURL     = "https://www.gstatic.com/generate_204"
	ProbeSuccess = http.StatusNoContent
	probeTimeout = 10 * time.Second
)

func probeThrough(ctx context.Context, client *http.Client, probeURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != ProbeSuccess {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}

func socksClient(socksAddr string) (*http.Client, error) {
	base := &net.Dialer{Timeout: probeTimeout}
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, base)
	if err != nil {
		return nil, err
	}
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks dialer lacks DialContext")
	}
	tr := &http.Transport{
		DialContext:         ctxDialer.DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: probeTimeout,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{Transport: tr, Timeout: probeTimeout}, nil
}

func ProbeSOCKS(ctx context.Context, socksAddr string, probeURL string) error {
	if probeURL == "" {
		probeURL = ProbeURL
	}
	client, err := socksClient(socksAddr)
	if err != nil {
		return err
	}
	return probeThrough(ctx, client, probeURL)
}
