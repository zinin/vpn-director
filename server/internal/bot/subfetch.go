package bot

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vless"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const maxSubscriptionBody = 1 << 20

func fetchSubscription(ctx context.Context, rawURL string, wan *http.Client, tunnel *http.Client) ([]byte, error) {
	return fetchWANThenOptionalTunnel(ctx, rawURL, wan, func() *http.Client { return tunnel })
}

// fetchWANThenOptionalTunnel GETs via wan; on failure it builds the tunnel
// client once and GETs through that only — it does not retry WAN.
func fetchWANThenOptionalTunnel(ctx context.Context, rawURL string, wan *http.Client, tunnel func() *http.Client) ([]byte, error) {
	body, err := getSubscription(ctx, wan, rawURL)
	if err == nil {
		return body, nil
	}
	// Even ssrf.ErrBlockedAddress falls back: a WAN resolver's private stub for a blocked domain is what the tunnel's own DNS gets around.
	if tunnel == nil {
		return nil, err
	}
	c := tunnel()
	if c == nil {
		return nil, err
	}
	// A *url.Error carries the whole subscription URL, token included.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	slog.Debug("Subscription fetch over WAN failed, trying the tunnel", "error", err)
	return getSubscription(ctx, c, rawURL)
}

func getSubscription(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("no http client")
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// One byte past the cap tells a truncated list from one that fits exactly.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSubscriptionBody {
		return nil, fmt.Errorf("subscription body exceeds 1 MiB")
	}
	return body, nil
}

func (b *Bot) fetchSub(ctx context.Context, rawURL string, cfgSvc service.ConfigStore, vpnSvc service.VPNDirector) ([]vpnconfig.Server, error) {
	if u, err := url.Parse(rawURL); err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("subscription URL must use https")
	}
	wan := ssrf.NewClient(10 * time.Second)
	p, tunnel := subscriptionTunnel(cfgSvc, vpnSvc)
	var tunnelLookup func(host string) ([]net.IP, error)
	if tunnel != nil {
		path := p
		tunnelLookup = func(host string) ([]net.IP, error) {
			return lookupIPv4OnPath(ctx, path, host)
		}
	}
	return fetchServers(ctx, rawURL, wan, tunnel, tunnelLookup)
}

// fetchServers GETs via wan, then tunnel. A WAN body is resolved with
// net.LookupIP; a tunneled body uses tunnelLookup so VLESS hostnames follow
// the same path that fetched the list.
func fetchServers(ctx context.Context, rawURL string, wan, tunnel *http.Client, tunnelLookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
	body, err := getSubscription(ctx, wan, rawURL)
	if err == nil {
		return serversFromSubscription(body)
	}
	if tunnel == nil {
		return nil, err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	slog.Debug("Subscription fetch over WAN failed, trying the tunnel", "error", err)
	body, err = getSubscription(ctx, tunnel, rawURL)
	if err != nil {
		return nil, err
	}
	if tunnelLookup != nil {
		return serversFromSubscriptionLookup(body, tunnelLookup)
	}
	return serversFromSubscription(body)
}

// serversFromSubscription decodes a fetched subscription body for the watch and
// keeps the servers whose addresses resolved.
func serversFromSubscription(body []byte) ([]vpnconfig.Server, error) {
	return serversFromSubscriptionLookup(body, nil)
}

func serversFromSubscriptionLookup(body []byte, lookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
	var result vless.Import
	if lookup == nil {
		result = vless.DecodeAndResolve(string(body))
	} else {
		result = vless.DecodeAndResolveLookup(string(body), lookup)
	}
	if result.Parsed == 0 {
		return nil, errors.New("no VLESS servers")
	}
	if len(result.Servers) == 0 {
		return nil, errors.New("could not resolve IP for any server")
	}
	return result.Servers, nil
}

func subscriptionTunnel(cfgSvc service.ConfigStore, vpnSvc service.VPNDirector) (Path, *http.Client) {
	if cfgSvc == nil || vpnSvc == nil {
		return Path{}, nil
	}
	cfg, err := cfgSvc.LoadVPNConfig()
	if err != nil || cfg == nil {
		return Path{}, nil
	}
	plat, platErr := vpnSvc.Platform()
	if platErr != nil {
		plat = vpnconfig.PlatformInfo{}
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		return Path{}, nil
	}
	p := subscriptionTunnelPath(cfg, plat, id)
	return p, newTunnelHTTPClient(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return refusePrivatePeer(DialPath(ctx, p, "tcp4", addr))
	})
}

// newTunnelHTTPClient gives the tunnel fetch what ssrf.NewClient gives the WAN
// one: no redirects, so a 3xx surfaces as "HTTP 3xx", and TLS 1.2 at least.
func newTunnelHTTPClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:       dial,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: false,
		},
	}
}

// refusePrivatePeer is the tunnel's stand-in for the ssrf dial guard, which
// DialPath has no hook for: it checks the peer once connected and closes a
// connection to a private or reserved address before a byte of HTTP is sent.
func refusePrivatePeer(conn net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	remote := conn.RemoteAddr()
	tcp, ok := remote.(*net.TCPAddr)
	if !ok || tcp == nil || tcp.IP == nil || ssrf.IsPrivateOrReserved(tcp.IP) {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %s", ssrf.ErrBlockedAddress, remote)
	}
	return conn, nil
}

func subscriptionTunnelPath(cfg *vpnconfig.VPNDirectorConfig, plat vpnconfig.PlatformInfo, id string) Path {
	var iface string
	for _, t := range plat.Tunnels {
		if t.ID == id {
			iface = t.Iface
			break
		}
	}
	idxByID := loadTunnelIdxFile(defaultTunnelTablesPath)
	var mark uint32
	if idx, ok := idxByID[id]; ok {
		mark = tunnelMark(idx, markShift(cfg))
	}
	return Path{kind: kindTunnel, id: id, iface: iface, mark: mark}
}
