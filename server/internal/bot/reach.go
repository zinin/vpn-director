package bot

import (
	"context"
	"net"
	"strconv"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subwatch"
)

// dialFunc is the dial reachTCP4 makes; tests hand it one of their own.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// reachTCP4 is the subscription watch's reachability check: does ip accept a
// TCP connection on port within subwatch.ReachTimeout. An address the bot does
// not dial on a subscription's say-so - not IPv4, private, loopback, reserved -
// counts as reachable without a dial, so the watch neither declares a server
// behind it dead early nor holds a return back for it. A nil dial uses a
// net.Dialer.
func reachTCP4(dial dialFunc) func(ctx context.Context, ip string, port int) bool {
	if dial == nil {
		d := &net.Dialer{Timeout: subwatch.ReachTimeout}
		dial = d.DialContext
	}
	return func(ctx context.Context, ip string, port int) bool {
		addr := net.ParseIP(ip).To4()
		if addr == nil || ssrf.IsPrivateOrReserved(addr) {
			return true
		}
		conn, err := dial(ctx, "tcp4", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
}
