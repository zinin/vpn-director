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
	"strconv"
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
// It stays well under configLockTimeout, because the test runs inside
// UpdateVPNConfig: a bound as long as the lock's would let a single hung xray
// use up the entire wait every other writer is willing to sit through, so an
// apply or an import beside it would fail with "config is busy" instead of
// taking its turn. It is a var so a test can shorten it.
var xrayTestTimeout = 15 * time.Second

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
		if downloadWithoutAddress(ob) {
			return nil, fmt.Errorf("stored outbound: xhttp downloadSettings without an address")
		}
		if smallPacketUpPosts(ob) {
			return nil, fmt.Errorf("stored outbound: xhttp scMaxEachPostBytes of 8192 or less in packet-up mode")
		}
		ob["tag"] = "proxy-out"
		return ob, nil
	}
	if err := validateStreamParams(server); err != nil {
		return nil, err
	}
	return buildOutbound(server), nil
}

// downloadWithoutAddress reports whether v, however deep, holds a
// downloadSettings object whose address is not a non-empty string. Xray reads
// an xhttp extra's downloadSettings as a stream config of its own and gives it
// a destination only from an address (StreamConfig.Build in 26.2.6); without
// one the destination stays nil, and splithttp's dialer dereferences it on the
// first dial - a panic that takes Xray down, and every client with it. "xray
// run -test" never dials, so such a config passes the test and replaces
// config.json: the generator has to refuse it itself.
// xrayconf_build_outbound in lib/xrayconf.sh refuses the same shape.
// Both names are looked up as Xray reads them: it loads its config with
// encoding/json, which matches a key to a field whatever its case
// (strings.EqualFold, the long s and the Kelvin sign included), so a
// "DownloadSettings" is the download stream to it and an "Address" its
// address - and a record stored before the importers refused such spellings
// can hold either.
func downloadWithoutAddress(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for key, child := range t {
			if download, ok := child.(map[string]interface{}); ok && strings.EqualFold(key, "downloadSettings") && !hasAddress(download) {
				return true
			}
			if downloadWithoutAddress(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range t {
			if downloadWithoutAddress(child) {
				return true
			}
		}
	}
	return false
}

// hasAddress reports whether a key of download that Xray reads as its address
// holds a non-empty string.
func hasAddress(download map[string]interface{}) bool {
	for key, value := range download {
		if address, _ := value.(string); address != "" && strings.EqualFold(key, "address") {
			return true
		}
	}
	return false
}

// smallPacketUpPosts reports whether ob holds an xhttp stream Xray would dial
// packet-up with a scMaxEachPostBytes of 8192 or less. splithttp's dialer
// panics on that range ("scMaxEachPostBytes should be bigger than 8192", Dial
// in 26.2.6) the first time it dials packet-up - a panic that takes Xray down,
// and every client with it - and "xray run -test" never dials, so such a
// config passes the test and replaces config.json. When xhttpSettings (or
// splithttpSettings, which it takes precedence over) holds an extra, Xray
// builds the stream from extra alone, with the outer host, path and mode
// copied onto it: the range counts from extra, and the mode is always the
// outer one. An empty or "auto" mode is packet-up unless the stream is
// REALITY; "stream-up" and "stream-one" never reach the panic, and a range
// whose upper end is 0 gives way to Xray's default. The keys are read folded,
// as downloadWithoutAddress reads its own; where one key is spelled several
// ways, any spelling that would panic is enough, since a map cannot say which
// one Xray reads last. xrayconf_build_outbound in lib/xrayconf.sh refuses the
// same streams.
func smallPacketUpPosts(ob map[string]interface{}) bool {
	for _, stream := range objects(foldedValues(ob, "streamSettings")) {
		xhttp, reality := false, true
		for _, network := range foldedStrings(stream, "network") {
			network = strings.ToLower(network)
			xhttp = xhttp || network == "xhttp" || network == "splithttp"
		}
		for _, security := range foldedStrings(stream, "security") {
			reality = reality && strings.ToLower(security) == "reality"
		}
		if !xhttp {
			continue
		}
		settings := objects(foldedValues(stream, "xhttpSettings"))
		if len(settings) == 0 {
			settings = objects(foldedValues(stream, "splithttpSettings"))
		}
		for _, x := range settings {
			packetUp := false
			for _, mode := range foldedStrings(x, "mode") {
				packetUp = packetUp || mode == "packet-up" || (mode == "" || mode == "auto") && !reality
			}
			if !packetUp {
				continue
			}
			// An extra that is no object fails Xray's own Build.
			holders := []map[string]interface{}{x}
			if extras := foldedValues(x, "extra"); len(extras) > 0 {
				holders = objects(extras)
			}
			for _, holder := range holders {
				for _, value := range foldedValues(holder, "scMaxEachPostBytes") {
					if from, to, ok := xrayRange(value); ok && to != 0 && from <= 8192 {
						return true
					}
				}
			}
		}
	}
	return false
}

// foldedValues returns every value m holds under a key Xray reads as name:
// encoding/json matches a key to a field whatever its case.
func foldedValues(m map[string]interface{}, name string) []interface{} {
	var values []interface{}
	for key, value := range m {
		if strings.EqualFold(key, name) {
			values = append(values, value)
		}
	}
	return values
}

// foldedStrings returns foldedValues as strings, "" for a value that is not
// one, and [""] when m holds none.
func foldedStrings(m map[string]interface{}, name string) []string {
	values := foldedValues(m, name)
	if len(values) == 0 {
		return []string{""}
	}
	strs := make([]string, len(values))
	for i, value := range values {
		strs[i], _ = value.(string)
	}
	return strs
}

// objects returns the values that are objects.
func objects(values []interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, value := range values {
		if object, ok := value.(map[string]interface{}); ok {
			out = append(out, object)
		}
	}
	return out
}

// xrayRange reads v as Xray reads an Int32Range: a whole-number literal, or a
// string holding one integer, nothing (0), or two integers joined by "-", a
// leading "-" belonging to the first - strconv.Atoi's integers, a sign and
// digits. from <= to; ok is false for any other shape, which Xray's own Build
// refuses, and "xray run -test" with it.
func xrayRange(v interface{}) (from, to int64, ok bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, 0, false
		}
		from, to = n, n
	case string:
		if t == "" {
			return 0, 0, true
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			from, to = n, n
			break
		}
		skip := 0
		if strings.HasPrefix(t, "-") {
			skip = 1
		}
		i := strings.Index(t[skip:], "-")
		if i < 0 {
			return 0, 0, false
		}
		i += skip
		left, err := strconv.ParseInt(t[:i], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		right, err := strconv.ParseInt(t[i+1:], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		from, to = left, right
	default:
		return 0, 0, false
	}
	if from > to {
		from, to = to, from
	}
	return from, to, true
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
