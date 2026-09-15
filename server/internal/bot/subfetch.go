package bot

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/ssrf"
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
	if tunnel == nil {
		return nil, err
	}
	c := tunnel()
	if c == nil {
		return nil, err
	}
	return getSubscription(ctx, c, rawURL)
}

func getSubscription(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("no http client")
	}
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
	return io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBody))
}

func (b *Bot) fetchSub(ctx context.Context, rawURL string, cfgSvc service.ConfigStore, vpnSvc service.VPNDirector) ([]byte, error) {
	wan := ssrf.NewClient(10 * time.Second)
	return fetchWANThenOptionalTunnel(ctx, rawURL, wan, func() *http.Client {
		return subscriptionTunnelClient(cfgSvc, vpnSvc)
	})
}

func subscriptionTunnelClient(cfgSvc service.ConfigStore, vpnSvc service.VPNDirector) *http.Client {
	if cfgSvc == nil || vpnSvc == nil {
		return nil
	}
	cfg, err := cfgSvc.LoadVPNConfig()
	if err != nil || cfg == nil {
		return nil
	}
	plat, platErr := vpnSvc.Platform()
	if platErr != nil {
		plat = vpnconfig.PlatformInfo{}
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		return nil
	}
	p := subscriptionTunnelPath(cfg, plat, id)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return DialPath(ctx, p, "tcp4", addr)
			},
			ForceAttemptHTTP2: false,
		},
	}
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
