package netpath

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/zinin/vpn-director/server/internal/ssrf"
)

const defaultReachTimeout = 3 * time.Second

// ReachTCP4 checks whether ip accepts TCP on port. Addresses that are not safe
// IPv4 dial targets count as reachable without a dial. A nil dial uses net.Dialer.
func ReachTCP4(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, int) bool {
	if dial == nil {
		d := &net.Dialer{Timeout: defaultReachTimeout}
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
