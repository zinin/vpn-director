package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// hostClient sends every request to srv, ignoring the request URL host so a
// WAN miss can fall through to the tunnel server on the same rawURL.
func hostClient(srv *httptest.Server) *http.Client {
	base := srv.Client()
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			u, err := url.Parse(srv.URL)
			if err != nil {
				return nil, err
			}
			clone.URL.Scheme = u.Scheme
			clone.URL.Host = u.Host
			return base.Transport.RoundTrip(clone)
		}),
	}
}

func TestGetSubscription_WANSuccess(t *testing.T) {
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "wan-body")
	}))
	t.Cleanup(wan.Close)

	body, err := getSubscription(context.Background(), hostClient(wan), wan.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "wan-body" {
		t.Fatalf("body %q, want wan-body", body)
	}
}

func TestGetSubscription_Non200DoesNotReturnBody(t *testing.T) {
	payload := "not-a-subscription"
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(wan.Close)

	body, err := getSubscription(context.Background(), hostClient(wan), wan.URL)
	if err == nil {
		t.Fatal("expected error on HTTP non-200")
	}
	if body != nil {
		t.Fatalf("non-200 must not return a body to decode, got %q", body)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %v should mention the HTTP status", err)
	}
}

func TestGetSubscription_CapsBody(t *testing.T) {
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("A"), (1<<20)+64))
	}))
	t.Cleanup(wan.Close)

	body, err := getSubscription(context.Background(), hostClient(wan), wan.URL)
	if err == nil || !strings.Contains(err.Error(), "subscription body exceeds 1 MiB") {
		t.Fatalf("err %v, want the 1 MiB cap", err)
	}
	if body != nil {
		t.Fatalf("an oversized list must not come back truncated, got %d bytes", len(body))
	}
}

func TestGetSubscription_DialError(t *testing.T) {
	wanErr := errors.New("wan down")
	wan := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wanErr
	})}
	body, err := getSubscription(context.Background(), wan, "http://subscription.example/list")
	if !errors.Is(err, wanErr) {
		t.Fatalf("err %v, want WAN error", err)
	}
	if body != nil {
		t.Fatalf("body %q", body)
	}
}

func TestFetchSub_RejectsNonHTTPS(t *testing.T) {
	servers, err := (&Bot{}).fetchSub(context.Background(), "http://cdn.example/s/token", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err %v, want an https error", err)
	}
	if strings.Contains(err.Error(), "cdn.example") {
		t.Fatalf("error %q carries the subscription URL", err)
	}
	if servers != nil {
		t.Fatalf("servers %v", servers)
	}
}

// configOnly and platformOnly answer the two calls subscriptionTunnel makes;
// any other method hits the nil embedded interface.
type configOnly struct {
	service.ConfigStore
	cfg *vpnconfig.VPNDirectorConfig
}

func (c configOnly) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) { return c.cfg, nil }

type platformOnly struct {
	service.VPNDirector
	plat vpnconfig.PlatformInfo
}

func (p platformOnly) Platform() (vpnconfig.PlatformInfo, error) { return p.plat, nil }

// The watch moves the clients off a first exit whose fallback never became
// ready. A refresh through that first exit again goes through the tunnel the
// watch has just given up on.
func TestSubscriptionTunnel_DialsTheTunnelTheClientsWereMovedTo(t *testing.T) {
	cfg := &vpnconfig.VPNDirectorConfig{
		TunnelDirector: vpnconfig.TunnelDirectorConfig{Tunnels: map[string]vpnconfig.TunnelConfig{
			"ovpnc2": {Clients: []string{"192.168.1.3"}},
			"wgc1":   {Clients: []string{"192.168.1.4", "192.168.1.8"}},
		}},
		Xray: vpnconfig.XrayConfig{Failover: &vpnconfig.XrayFailover{
			Tunnel: "wgc1", Clients: []string{"192.168.1.8"}, Added: []string{"192.168.1.8"},
		}},
	}
	plat := vpnconfig.PlatformInfo{Tunnels: []vpnconfig.PlatformTunnel{
		{ID: "ovpnc2", Iface: "tun12", Connected: true},
		{ID: "wgc1", Iface: "wgc1", Connected: true},
	}}

	p, client := subscriptionTunnel(configOnly{cfg: cfg}, platformOnly{plat: plat})

	if client == nil {
		t.Fatal("no tunnel client")
	}
	if p.id != "wgc1" || p.iface != "wgc1" {
		t.Fatalf("path %s on %s, want wgc1, the tunnel the clients were moved to", p.id, p.iface)
	}
}

// remoteConn is a net.Conn that only answers RemoteAddr.
type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

func TestRefusePrivatePeer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	raw, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	conn, err := refusePrivatePeer(raw, nil)
	if !errors.Is(err, ssrf.ErrBlockedAddress) {
		t.Fatalf("loopback peer: err %v, want ssrf.ErrBlockedAddress", err)
	}
	if conn != nil {
		t.Fatal("loopback peer: connection returned")
	}
	if _, werr := raw.Write([]byte("GET")); !errors.Is(werr, net.ErrClosed) {
		t.Fatalf("loopback peer: connection left open, write err %v", werr)
	}

	public := remoteConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 443}}
	conn, err = refusePrivatePeer(public, nil)
	if err != nil {
		t.Fatalf("public peer: %v", err)
	}
	if conn != public {
		t.Fatalf("public peer: got %v, want the dialled connection", conn)
	}

	dialErr := errors.New("tunnel down")
	conn, err = refusePrivatePeer(nil, dialErr)
	if err != dialErr {
		t.Fatalf("dial error: err %v, want it unchanged", err)
	}
	if conn != nil {
		t.Fatal("dial error: connection returned")
	}
}

func TestNewTunnelHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_, _ = io.WriteString(w, "internal")
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	dialer := &net.Dialer{}
	body, err := getSubscription(context.Background(), newTunnelHTTPClient(dialer.DialContext), redirect.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("err %v, want HTTP 302", err)
	}
	if body != nil {
		t.Fatalf("body %q", body)
	}
	if targetHits != 0 {
		t.Fatalf("redirect followed: target hits %d", targetHits)
	}
}

func TestServersFromSubscription(t *testing.T) {
	if _, err := serversFromSubscription([]byte("not base64 !!!")); err == nil || err.Error() != "no VLESS servers" {
		t.Fatalf("err %v, want no VLESS servers", err)
	}

	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443#Oslo"))
	servers, err := serversFromSubscription([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "Oslo" || !reflect.DeepEqual(servers[0].IPs, []string{"203.0.113.10"}) {
		t.Fatalf("servers %+v, want Oslo on 203.0.113.10", servers)
	}
}

func TestFetchServers_WANSuccessDoesNotUseTunnelLookup(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@203.0.113.10:443#Oslo"))
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(wan.Close)
	looked := 0
	servers, err := fetchServers(context.Background(), wan.URL, hostClient(wan), nil, func(host string) ([]net.IP, error) {
		looked++
		return nil, errors.New("tunnel lookup must not run")
	})
	if err != nil {
		t.Fatal(err)
	}
	if looked != 0 {
		t.Fatalf("WAN success must resolve on the system resolver, lookups %d", looked)
	}
	if len(servers) != 1 || servers[0].Name != "Oslo" || !reflect.DeepEqual(servers[0].IPs, []string{"203.0.113.10"}) {
		t.Fatalf("servers %+v", servers)
	}
}

func TestFetchServers_TunnelFetchUsesTunnelLookup(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte("vless://uuid-1@oslo.example.invalid:443#Oslo"))
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wan-down", http.StatusBadGateway)
	}))
	t.Cleanup(wan.Close)
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(tun.Close)

	looked := []string{}
	servers, err := fetchServers(context.Background(), "https://cdn.example/s/token", hostClient(wan), hostClient(tun), func(host string) ([]net.IP, error) {
		looked = append(looked, host)
		return []net.IP{net.ParseIP("203.0.113.50")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(looked, []string{"oslo.example.invalid"}) {
		t.Fatalf("tunnel lookup %v", looked)
	}
	if len(servers) != 1 || servers[0].Name != "Oslo" || !reflect.DeepEqual(servers[0].IPs, []string{"203.0.113.50"}) {
		t.Fatalf("servers %+v", servers)
	}
}
