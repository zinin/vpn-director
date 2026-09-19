// internal/service/xray.go
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// XrayService handles Xray configuration generation
type XrayService struct {
	templatePath string
	outputPath   string
	// validate tests a written config before it replaces the live one; nil
	// skips the test. NewXrayService sets xrayTest.
	validate func(path string) error
}

var _ XrayGenerator = (*XrayService)(nil)

func NewXrayService(templatePath, outputPath string) *XrayService {
	return &XrayService{templatePath: templatePath, outputPath: outputPath, validate: xrayTest}
}

// xrayTestTimeout bounds one "xray run -test"; a router needs a second or two.
const xrayTestTimeout = 30 * time.Second

// xrayTest has Xray load the config without starting a server. The outbound
// may come verbatim from a subscription, and it may name a protocol the
// installed Xray lacks or a key it refuses - allowInsecure stops Xray from
// loading any config since 2026-06-01 - and a config Xray rejects takes every
// Xray client, and the bot, offline until the next switch. S24xray runs
// "xray run -confdir", which loads only *.json, so the temp config.json.*
// names its format. Without an xray on PATH - the dev mode, a workstation -
// there is nothing to test with.
func xrayTest(path string) error {
	bin, err := exec.LookPath("xray")
	if err != nil {
		slog.Debug("xray not found, config not tested", "path", path)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), xrayTestTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "run", "-test", "-format", "json", "-c", path).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("xray config test timed out after %s", xrayTestTimeout)
	}
	if err != nil {
		return fmt.Errorf("xray rejected the config: %s", lastLines(string(out), 3))
	}
	return nil
}

// lastLines joins the last n non-empty lines of s with "; ".
func lastLines(s string, n int) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

type xrayUser struct {
	ID         string `json:"id"`
	Encryption string `json:"encryption"`
	Flow       string `json:"flow,omitempty"`
}

type xrayVnext struct {
	Address string     `json:"address"`
	Port    int        `json:"port"`
	Users   []xrayUser `json:"users"`
}

type xrayReality struct {
	ServerName  string `json:"serverName,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"publicKey,omitempty"`
	ShortID     string `json:"shortId,omitempty"`
}

type xrayTLS struct {
	ServerName  string   `json:"serverName,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
}

type xrayStream struct {
	Network         string       `json:"network"`
	Security        string       `json:"security"`
	RealitySettings *xrayReality `json:"realitySettings,omitempty"`
	TLSSettings     *xrayTLS     `json:"tlsSettings,omitempty"`
}

type xrayOutbound struct {
	Protocol       string                 `json:"protocol"`
	Settings       map[string]interface{} `json:"settings"`
	StreamSettings xrayStream             `json:"streamSettings"`
	Tag            string                 `json:"tag"`
}

func buildOutbound(s vpnconfig.Server) xrayOutbound {
	network := s.Network
	if network == "" {
		network = "tcp"
	}
	stream := xrayStream{Network: network}
	switch s.Security {
	case "reality":
		stream.Security = "reality"
		stream.RealitySettings = &xrayReality{
			ServerName:  s.SNI,
			Fingerprint: s.Fingerprint,
			PublicKey:   s.PublicKey,
			ShortID:     s.ShortID,
		}
	case "tls":
		stream.Security = "tls"
		serverName := s.SNI
		if serverName == "" {
			serverName = s.Address
		}
		stream.TLSSettings = &xrayTLS{
			ServerName:  serverName,
			Fingerprint: s.Fingerprint,
			ALPN:        s.ALPN,
		}
	default: // legacy: empty security -> TLS to address with alpn h2, no flow
		stream.Security = "tls"
		serverName := s.SNI
		if serverName == "" {
			serverName = s.Address
		}
		stream.TLSSettings = &xrayTLS{ServerName: serverName, ALPN: []string{"h2"}}
	}
	// Per spec, flow belongs only to tls/reality outbounds; a legacy record
	// (empty security) must not carry it even if the field is populated.
	userFlow := s.Flow
	if s.Security == "" {
		userFlow = ""
	}
	return xrayOutbound{
		Protocol: "vless",
		Settings: map[string]interface{}{
			"vnext": []xrayVnext{{
				Address: s.Address,
				Port:    s.Port,
				Users:   []xrayUser{{ID: s.UUID, Encryption: "none", Flow: userFlow}},
			}},
		},
		StreamSettings: stream,
		Tag:            "proxy-out",
	}
}

// validateStreamParams rejects inputs this generator cannot produce a working
// config for (only tcp transport and tls/reality security), so a future ws/grpc
// or unknown security fails loudly instead of silently emitting a broken outbound.
func validateStreamParams(s vpnconfig.Server) error {
	switch s.Network {
	case "", "tcp":
	default:
		return fmt.Errorf("unsupported network %q (only tcp is supported)", s.Network)
	}
	switch s.Security {
	case "", "tls", "reality":
	default:
		return fmt.Errorf("unsupported security %q (only tls/reality are supported)", s.Security)
	}
	// REALITY cannot complete a handshake without these; fail at generation time
	// rather than emit an incomplete realitySettings that only breaks at runtime.
	// shortId is optional (Xray accepts an empty shortId when the server allows it).
	if s.Security == "reality" {
		var missing []string
		if s.PublicKey == "" {
			missing = append(missing, "public_key")
		}
		if s.SNI == "" {
			missing = append(missing, "sni")
		}
		if s.Fingerprint == "" {
			missing = append(missing, "fingerprint")
		}
		if len(missing) > 0 {
			return fmt.Errorf("reality security requires non-empty fields: %v", missing)
		}
	}
	return nil
}

// InboundPorts overrides the ports the template gives its inbounds. A zero
// field keeps the template's value.
type InboundPorts struct {
	TProxy int
	Socks  int
}

// applyInboundPorts rewrites the ports of the tagged inbounds in place. The
// dokodemo-door port must match advanced.xray.tproxy_port: that is where the
// TPROXY rules send traffic, and a template default left there would leave a
// configured port without a listener.
func applyInboundPorts(cfg map[string]interface{}, ports InboundPorts) {
	byTag := map[string]int{"tproxy-in": ports.TProxy, "socks-in": ports.Socks}
	inbounds, ok := cfg["inbounds"].([]interface{})
	if !ok {
		return
	}
	for _, entry := range inbounds {
		inbound, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		tag, _ := inbound["tag"].(string)
		if port := byTag[tag]; port > 0 {
			inbound["port"] = port
		}
	}
}

// serverOutbound is the proxy-out outbound of a server: the outbound its
// import stored, tagged, or for a record from before outbounds were stored,
// the one buildOutbound makes from the flat VLESS fields.
func serverOutbound(server vpnconfig.Server) (interface{}, error) {
	if len(server.Outbound) > 0 {
		ob, err := vpnconfig.DecodeOutbound(server.Outbound)
		if err != nil {
			return nil, fmt.Errorf("stored outbound: %w", err)
		}
		ob["tag"] = "proxy-out"
		return ob, nil
	}
	if err := validateStreamParams(server); err != nil {
		return nil, err
	}
	return buildOutbound(server), nil
}

// GenerateConfig parses the (valid-JSON) template and replaces outbounds
// with the server's proxy-out outbound, then has Xray test the result before
// it replaces config.json. The optional ports keep the inbounds in step with
// advanced.xray in vpn-director.json; without them the template's ports stand.
func (s *XrayService) GenerateConfig(server vpnconfig.Server, ports ...InboundPorts) error {
	outbound, err := serverOutbound(server)
	if err != nil {
		return err
	}
	template, err := os.ReadFile(s.templatePath)
	if err != nil {
		return fmt.Errorf("read template: %w", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(template, &cfg); err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	cfg["outbounds"] = []interface{}{outbound}
	if len(ports) > 0 {
		applyInboundPorts(cfg, ports[0])
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// Write atomically: temp file in the same dir, then rename. Prevents an
	// interrupted/partial write from truncating the live config.json and
	// bricking Xray (and the bot, which proxies through it). Mirrors the
	// mktemp+mv in configure.sh. 0600 keeps the UUID-bearing config owner-only.
	dir := filepath.Dir(s.outputPath)
	tmp, err := os.CreateTemp(dir, "config.json.*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if s.validate != nil {
		if err := s.validate(tmpName); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, s.outputPath); err != nil {
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}
