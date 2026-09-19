package subscription

import (
	"context"
	"fmt"
	"net"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Import is a decoded subscription after resolution. Servers keeps
// subscription order and holds only the servers whose address resolved;
// Parsed counts the servers Decode returned.
type Import struct {
	Servers       []vpnconfig.Server
	Total         int
	Parsed        int
	Skipped       []Skip
	ResolveErrors int
}

// LookupIPv4 resolves over IPv4 only, through ctx. The router's own resolver is
// reached straight over the WAN, and an AF_UNSPEC lookup there waits out the
// AAAA half that often goes unanswered - glibc's full five seconds per host,
// in a loop over the whole subscription (.claude/rules/shell-conventions.md
// has the measurement). Anything that is not IPv4 is discarded below anyway,
// so the second family is pure waiting.
func LookupIPv4(ctx context.Context) func(host string) ([]net.IP, error) {
	return func(host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
}

// DecodeAndResolve decodes a subscription body and resolves every server
// through the default resolver. The bot's /import and the Web UI import use
// this; a tunneled watch fetch uses DecodeAndResolveLookup.
func DecodeAndResolve(body string) (Import, error) {
	return DecodeAndResolveLookup(body, LookupIPv4(context.Background()))
}

// DecodeAndResolveLookup is DecodeAndResolve with a caller-supplied lookup
// (the watch uses the tunnel DNS path after a tunneled GET). Decode's error
// comes back as is.
func DecodeAndResolveLookup(body string, lookup func(host string) ([]net.IP, error)) (Import, error) {
	decoded, err := Decode(body)
	if err != nil {
		return Import{}, err
	}
	if lookup == nil {
		lookup = LookupIPv4(context.Background())
	}
	imp := Import{Total: decoded.Total, Parsed: len(decoded.Servers), Skipped: decoded.Skipped}
	for _, s := range decoded.Servers {
		ips, err := resolveIPv4(lookup, s.Address)
		if err != nil {
			imp.ResolveErrors++
			continue
		}
		s.IPs = ips
		imp.Servers = append(imp.Servers, s)
	}
	return imp, nil
}

func resolveIPv4(lookup func(host string) ([]net.IP, error), host string) ([]string, error) {
	ips, err := lookup(host)
	if err != nil {
		return nil, err
	}
	var resolved []string
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			resolved = append(resolved, ipv4.String())
		}
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found for %s", host)
	}
	return resolved, nil
}
