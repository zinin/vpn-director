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
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subscription"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const maxSubscriptionBody = 1 << 20

// errNoResolved is a subscription that decoded and whose hostnames went
// unanswered - the one download failure worth retrying over the other path.
var errNoResolved = errors.New("could not resolve IP for any server")

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
	// IPv4 only and bound to ctx: an AF_UNSPEC lookup of every hostname in the
	// subscription can hold a watch tick for minutes on this router, and a stop
	// has to be able to end it.
	wanLookup := subscription.LookupIPv4(ctx)
	tunnel, tunnelLookup := lazyTunnel(ctx, cfgSvc, vpnSvc)
	return fetchServers(ctx, rawURL, wan, tunnel, wanLookup, tunnelLookup)
}

// errNoTunnel is a lookup over a tunnel that is not there.
var errNoTunnel = errors.New("no tunnel to look the host up over")

// lazyTunnel is the tunnel one fetch falls back to, found when the fetch first
// asks for it, and once: finding it runs vpn-director.sh platform, and a wave
// of the watch fetches every subscription at once, mostly over a WAN that
// serves them all. client answers nil, and lookup an error, without a tunnel.
func lazyTunnel(ctx context.Context, cfgSvc service.ConfigStore, vpnSvc service.VPNDirector) (client func() *http.Client, lookup func(host string) ([]net.IP, error)) {
	find := sync.OnceValues(func() (Path, *http.Client) { return subscriptionTunnel(cfgSvc, vpnSvc) })
	client = func() *http.Client {
		_, c := find()
		return c
	}
	lookup = func(host string) ([]net.IP, error) {
		p, c := find()
		if c == nil {
			return nil, errNoTunnel
		}
		return lookupIPv4OnPath(ctx, p, host)
	}
	return client, lookup
}

// fetchServers GETs via wan, then tunnel. A body the tunnel fetched resolves
// over the tunnel. One the WAN fetched resolves each host with the WAN lookup,
// and with the tunnel's for a host the WAN resolver does not answer: one WAN
// answer used to make the whole list count as resolved, and the servers only
// the tunnel's resolver knew were dropped from it. A context that ends during
// the resolution fails every lookup after it at once, and what resolved before
// that is not the subscription: the context's error comes back instead of a
// list cut short. A download that failed reads as the daemons' own
// (service.DownloadError): the watch records it in the subscription's error,
// which the Web UI and /subs show. tunnel is asked for the tunnel's client only
// once the WAN falls short, and answers nil when there is none.
func fetchServers(ctx context.Context, rawURL string, wan *http.Client, tunnel func() *http.Client, wanLookup, tunnelLookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
	body, err := getSubscription(ctx, wan, rawURL)
	if err == nil {
		servers, rerr := serversFromSubscriptionLookup(body, eitherLookup(ctx, wanLookup, tunnelLookup))
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if rerr == nil {
			return servers, nil
		}
		// A body none of whose hostnames answered, on the WAN resolver or the
		// tunnel's, is the resolvers' failure rather than the subscription's:
		// the tunnel's own download gets the last try.
		if !errors.Is(rerr, errNoResolved) || tunnel() == nil {
			return nil, rerr
		}
		slog.Debug("Subscription hostnames did not resolve over the WAN, trying the tunnel", "error", rerr)
	} else {
		err = service.NewDownloadError(err)
		if tunnel() == nil {
			return nil, err
		}
		slog.Debug("Subscription fetch over WAN failed, trying the tunnel", "error", err)
	}
	body, err = getSubscription(ctx, tunnel(), rawURL)
	if err != nil {
		return nil, service.NewDownloadError(err)
	}
	servers, err := serversFromSubscriptionLookup(body, tunnelLookup)
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	return servers, err
}

// eitherLookup asks first, and second for a host first does not answer with an
// IPv4 address. A nil first is the default resolver, bound to ctx.
func eitherLookup(ctx context.Context, first, second func(host string) ([]net.IP, error)) func(host string) ([]net.IP, error) {
	if first == nil {
		first = subscription.LookupIPv4(ctx)
	}
	if second == nil {
		return first
	}
	return func(host string) ([]net.IP, error) {
		if ips, err := first(host); err == nil && hasIPv4(ips) {
			return ips, nil
		}
		return second(host)
	}
}

func hasIPv4(ips []net.IP) bool {
	for _, ip := range ips {
		if ip.To4() != nil {
			return true
		}
	}
	return false
}

// serversFromSubscriptionLookup decodes a fetched subscription body for the
// watch and keeps the servers whose addresses resolved through lookup, the
// default resolver when it is nil.
func serversFromSubscriptionLookup(body []byte, lookup func(host string) ([]net.IP, error)) ([]vpnconfig.Server, error) {
	result, err := subscription.DecodeAndResolveLookup(string(body), lookup)
	if err != nil {
		return nil, err
	}
	if result.Parsed == 0 {
		return nil, errors.New("no supported servers")
	}
	if len(result.Servers) == 0 {
		return nil, errNoResolved
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
	// The tunnel the clients are on: after a retarget the first exit is the
	// one the watch gave up on.
	id := vpnconfig.FailoverTDExit(cfg, plat)
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
