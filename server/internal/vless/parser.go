package vless

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// cleanName removes emoji flags and keeps only allowed characters:
// letters (latin + cyrillic), digits, spaces, and basic punctuation
func cleanName(s string) string {
	var result strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			unicode.Is(unicode.Cyrillic, r),
			r == ' ', r == '.', r == ',', r == ';', r == ':',
			r == '!', r == '?', r == '(', r == ')', r == '-':
			result.WriteRune(r)
		}
	}
	return strings.TrimSpace(result.String())
}

type Server struct {
	Address     string   `json:"address"`
	Port        int      `json:"port"`
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	IPs         []string `json:"ips"`
	Security    string   `json:"security,omitempty"`
	Network     string   `json:"network,omitempty"`
	Flow        string   `json:"flow,omitempty"`
	SNI         string   `json:"sni,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	PublicKey   string   `json:"public_key,omitempty"`
	ShortID     string   `json:"short_id,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
}

// ToVPNConfig converts a parsed vless.Server into a vpnconfig.Server,
// carrying all stream parameters. Call ResolveIPs first to populate IPs.
func (s *Server) ToVPNConfig() vpnconfig.Server {
	return vpnconfig.Server{
		Address:     s.Address,
		Port:        s.Port,
		UUID:        s.UUID,
		Name:        s.Name,
		IPs:         s.IPs,
		Security:    s.Security,
		Network:     s.Network,
		Flow:        s.Flow,
		SNI:         s.SNI,
		Fingerprint: s.Fingerprint,
		PublicKey:   s.PublicKey,
		ShortID:     s.ShortID,
		ALPN:        s.ALPN,
	}
}

func ParseURI(uri string) (*Server, error) {
	if !strings.HasPrefix(uri, "vless://") {
		return nil, errors.New("not a vless URI")
	}

	rest := strings.TrimPrefix(uri, "vless://")

	// Extract name (after #)
	name := ""
	if idx := strings.LastIndex(rest, "#"); idx != -1 {
		name, _ = url.QueryUnescape(rest[idx+1:])
		name = cleanName(name)
		rest = rest[:idx]
	}

	// Extract and parse query params
	var params url.Values
	if idx := strings.Index(rest, "?"); idx != -1 {
		var err error
		params, err = url.ParseQuery(rest[idx+1:])
		if err != nil {
			// A malformed query (e.g. a bad %-escape) would otherwise silently
			// drop stream params (security/pbk/sid/...) and emit a broken
			// outbound. Fail loudly; DecodeSubscription skips this URI and keeps
			// the rest of the subscription.
			return nil, fmt.Errorf("invalid query: %w", err)
		}
		rest = rest[:idx]
	}

	// Extract UUID (before @)
	atIdx := strings.Index(rest, "@")
	if atIdx == -1 {
		return nil, errors.New("missing @ in URI")
	}
	uuid := rest[:atIdx]
	rest = rest[atIdx+1:]

	// Extract server:port
	// Handle IPv6 addresses in brackets. NOTE: the stored Address keeps the
	// brackets ([2001:db8::1]); the shell importer (import_server_list.sh) drops
	// them (2001:db8::1). Both are equivalent to Xray — its ParseAddress strips
	// brackets for the standalone address field — so the two paths agree.
	var address string
	var portStr string

	if strings.HasPrefix(rest, "[") {
		// IPv6 address
		closeBracket := strings.Index(rest, "]")
		if closeBracket == -1 {
			return nil, errors.New("invalid IPv6 address format")
		}
		address = rest[:closeBracket+1]
		rest = rest[closeBracket+1:]
		if !strings.HasPrefix(rest, ":") {
			return nil, errors.New("missing port in URI")
		}
		portStr = rest[1:]
	} else {
		// IPv4 or hostname
		colonIdx := strings.LastIndex(rest, ":")
		if colonIdx == -1 {
			return nil, errors.New("missing port in URI")
		}
		address = rest[:colonIdx]
		portStr = rest[colonIdx+1:]
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, errors.New("invalid port")
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("port out of range")
	}

	if address == "" || uuid == "" {
		return nil, errors.New("missing required fields")
	}

	if name == "" {
		name = address
	}

	s := &Server{
		Address: address,
		Port:    port,
		UUID:    uuid,
		Name:    name,
	}
	if params != nil {
		s.Security = params.Get("security")
		s.Network = params.Get("type")
		s.Flow = params.Get("flow")
		s.SNI = params.Get("sni")
		s.Fingerprint = params.Get("fp")
		s.PublicKey = params.Get("pbk")
		s.ShortID = params.Get("sid")
		if alpn := params.Get("alpn"); alpn != "" {
			s.ALPN = strings.Split(alpn, ",")
		}
	}
	return s, nil
}

func (s *Server) ResolveIPs() error {
	ips, err := net.LookupIP(s.Address)
	if err != nil {
		return err
	}
	var resolved []string
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			resolved = append(resolved, ipv4.String())
		}
	}
	if len(resolved) == 0 {
		return fmt.Errorf("no IPv4 addresses found for %s", s.Address)
	}
	s.IPs = resolved
	return nil
}

func DecodeSubscription(encoded string) ([]*Server, []error) {
	encoded = strings.TrimSpace(encoded)

	var decoded []byte
	var decodeErr error

	// Try all base64 variants (padded and raw, standard and URL-safe)
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	}

	for _, enc := range encodings {
		decoded, decodeErr = enc.DecodeString(encoded)
		if decodeErr == nil {
			break
		}
	}

	if decodeErr != nil {
		return nil, []error{errors.New("failed to decode base64")}
	}

	var servers []*Server
	var parseErrors []error

	lines := strings.Split(string(decoded), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "vless://") {
			continue
		}

		server, err := ParseURI(line)
		if err != nil {
			parseErrors = append(parseErrors, err)
			continue
		}
		servers = append(servers, server)
	}

	return servers, parseErrors
}

// Import is a decoded subscription whose servers resolved. Servers keeps
// subscription order.
type Import struct {
	Servers       []vpnconfig.Server
	Parsed        int
	ParseErrors   []error
	ResolveErrors int
}

// DecodeAndResolve decodes a subscription body and resolves every parsed
// server. The bot's /import, the Web UI import and the subscription watch all
// go through it.
func DecodeAndResolve(body string) Import {
	parsed, parseErrors := DecodeSubscription(body)
	result := Import{Parsed: len(parsed), ParseErrors: parseErrors}
	for _, s := range parsed {
		if err := s.ResolveIPs(); err != nil {
			result.ResolveErrors++
			continue
		}
		result.Servers = append(result.Servers, s.ToVPNConfig())
	}
	return result
}
