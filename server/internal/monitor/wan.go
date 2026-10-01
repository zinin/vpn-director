package monitor

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// WANUp reports whether any of controls accepts a TCP connection within
// timeout; all are dialed at once.
func WANUp(ctx context.Context, controls []vpnconfig.Server, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	answers := make(chan bool, len(controls))
	for _, c := range controls {
		go func(addr string) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp4", addr)
			if err == nil {
				conn.Close()
			}
			answers <- err == nil
		}(net.JoinHostPort(c.Address, strconv.Itoa(c.Port)))
	}
	for range controls {
		if <-answers {
			return true
		}
	}
	return false
}
