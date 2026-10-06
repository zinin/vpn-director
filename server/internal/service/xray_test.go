// internal/service/xray_test.go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const testTemplate = `{"inbounds":[],"outbounds":[],"routing":{}}`

// newTestXrayService is NewXrayService without the xray test, so a test does
// not depend on whether the machine running it has an xray.
func newTestXrayService(templatePath, outputPath string) *XrayService {
	s := NewXrayService(templatePath, outputPath)
	s.validate = nil
	return s
}

func generate(t *testing.T, server vpnconfig.Server) map[string]interface{} {
	t.Helper()
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	outputPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
		t.Fatal(err)
	}
	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
		t.Fatalf("GenerateConfig error: %v", err)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return cfg
}

func outbound0(t *testing.T, cfg map[string]interface{}) map[string]interface{} {
	t.Helper()
	obs, ok := cfg["outbounds"].([]interface{})
	if !ok || len(obs) != 1 {
		t.Fatalf("expected 1 outbound, got %v", cfg["outbounds"])
	}
	return obs[0].(map[string]interface{})
}

func TestGenerateConfig_Reality(t *testing.T) {
	cfg := generate(t, vpnconfig.Server{
		Address: "162.249.126.77", Port: 443, UUID: "abc-123",
		Security: "reality", Network: "tcp", Flow: "xtls-rprx-vision",
		SNI: "cdn3-87.yahoo.com", Fingerprint: "firefox", PublicKey: "PBKEY", ShortID: "55e6",
	})
	ob := outbound0(t, cfg)
	ss := ob["streamSettings"].(map[string]interface{})
	if ss["security"] != "reality" {
		t.Fatalf("security = %v, want reality", ss["security"])
	}
	rs := ss["realitySettings"].(map[string]interface{})
	if rs["publicKey"] != "PBKEY" || rs["shortId"] != "55e6" || rs["serverName"] != "cdn3-87.yahoo.com" || rs["fingerprint"] != "firefox" {
		t.Errorf("realitySettings wrong: %v", rs)
	}
	if ss["network"] != "tcp" {
		t.Errorf("network = %v, want tcp", ss["network"])
	}
	if _, ok := ss["tlsSettings"]; ok {
		t.Errorf("reality outbound must not contain tlsSettings")
	}
	u0 := ob["settings"].(map[string]interface{})["vnext"].([]interface{})[0].(map[string]interface{})["users"].([]interface{})[0].(map[string]interface{})
	if u0["flow"] != "xtls-rprx-vision" {
		t.Errorf("flow = %v, want xtls-rprx-vision", u0["flow"])
	}
}

func vnextAddress(t *testing.T, cfg map[string]interface{}) string {
	t.Helper()
	vnext := outbound0(t, cfg)["settings"].(map[string]interface{})["vnext"].([]interface{})[0].(map[string]interface{})
	addr, _ := vnext["address"].(string)
	return addr
}

func TestGenerateConfig_DialsHostnameNotCachedIP(t *testing.T) {
	cfg := generate(t, vpnconfig.Server{
		Address: "oslo.example", Port: 443, UUID: "u",
		IPs:      []string{"203.0.113.50", "203.0.113.51"},
		Security: "tls",
	})
	if got := vnextAddress(t, cfg); got != "oslo.example" {
		t.Fatalf("vnext.address = %q, want the hostname so CDN/DDNS updates still resolve", got)
	}
	tls := outbound0(t, cfg)["streamSettings"].(map[string]interface{})["tlsSettings"].(map[string]interface{})
	if tls["serverName"] != "oslo.example" {
		t.Fatalf("tls serverName = %v, want the hostname", tls["serverName"])
	}
}

func TestGenerateConfig_TLS(t *testing.T) {
	cfg := generate(t, vpnconfig.Server{
		Address: "1.2.3.4", Port: 443, UUID: "u", Security: "tls", SNI: "host.example.com", Fingerprint: "chrome",
		ALPN: []string{"h2", "http/1.1"},
	})
	ss := outbound0(t, cfg)["streamSettings"].(map[string]interface{})
	if ss["security"] != "tls" {
		t.Fatalf("security = %v, want tls", ss["security"])
	}
	tls := ss["tlsSettings"].(map[string]interface{})
	if tls["serverName"] != "host.example.com" || tls["fingerprint"] != "chrome" {
		t.Errorf("tlsSettings wrong: %v", tls)
	}
	alpn, ok := tls["alpn"].([]interface{})
	if !ok || len(alpn) != 2 || alpn[0] != "h2" || alpn[1] != "http/1.1" {
		t.Errorf("tls alpn = %v, want [h2 http/1.1]", tls["alpn"])
	}
	if _, ok := ss["realitySettings"]; ok {
		t.Errorf("tls outbound must not contain realitySettings")
	}
}

func TestGenerateConfig_Legacy(t *testing.T) {
	cfg := generate(t, vpnconfig.Server{Address: "example.com", Port: 443, UUID: "abc-123", Flow: "xtls-rprx-vision"})
	ob := outbound0(t, cfg)
	ss := ob["streamSettings"].(map[string]interface{})
	if ss["security"] != "tls" {
		t.Fatalf("legacy security = %v, want tls", ss["security"])
	}
	tls := ss["tlsSettings"].(map[string]interface{})
	if tls["serverName"] != "example.com" {
		t.Errorf("legacy serverName = %v, want example.com", tls["serverName"])
	}
	alpn := tls["alpn"].([]interface{})
	if len(alpn) != 1 || alpn[0] != "h2" {
		t.Errorf("legacy alpn = %v, want [h2]", alpn)
	}
	u0 := ob["settings"].(map[string]interface{})["vnext"].([]interface{})[0].(map[string]interface{})["users"].([]interface{})[0].(map[string]interface{})
	if _, ok := u0["flow"]; ok {
		t.Errorf("legacy must not set flow, got %v", u0["flow"])
	}
}

// The TPROXY rules send traffic to advanced.xray.tproxy_port, so the
// dokodemo-door inbound has to listen there: a template default left in place
// leaves that port without a listener and proxying stops.
func TestGenerateConfig_InboundPortsFollowTheConfig(t *testing.T) {
	templatePath, outputPath := writeTemplate(t)
	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "tls", SNI: "s", Fingerprint: "chrome"}

	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server, InboundPorts{TProxy: 23456, Socks: 23457}); err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}

	got := readInboundPorts(t, outputPath)
	if got["tproxy-in"] != 23456 || got["socks-in"] != 23457 {
		t.Errorf("inbound ports = %v, want tproxy-in 23456 and socks-in 23457", got)
	}
}

func TestGenerateConfig_WithoutPortsKeepsTheTemplate(t *testing.T) {
	templatePath, outputPath := writeTemplate(t)
	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "tls", SNI: "s", Fingerprint: "chrome"}

	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}

	got := readInboundPorts(t, outputPath)
	if got["tproxy-in"] != 12345 || got["socks-in"] != 12346 {
		t.Errorf("inbound ports = %v, want the template's 12345 and 12346", got)
	}
}

// writeTemplate drops the repository's Xray template next to a fresh output
// path, so the ports under test are the ones the router really ships with.
func writeTemplate(t *testing.T) (templatePath, outputPath string) {
	t.Helper()

	dir := t.TempDir()
	templatePath = filepath.Join(dir, "config.json.template")
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "router", "opt", "etc", "xray", "config.json.template"))
	if err != nil {
		t.Fatalf("read repository template: %v", err)
	}
	if err := os.WriteFile(templatePath, src, 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return templatePath, filepath.Join(dir, "config.json")
}

func readInboundPorts(t *testing.T, path string) map[string]int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var cfg struct {
		Inbounds []struct {
			Tag  string `json:"tag"`
			Port int    `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse generated config: %v", err)
	}
	ports := make(map[string]int, len(cfg.Inbounds))
	for _, in := range cfg.Inbounds {
		ports[in.Tag] = in.Port
	}
	return ports
}

func TestGenerateConfig_MissingTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	svc := newTestXrayService(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "out"))
	if err := svc.GenerateConfig(vpnconfig.Server{}); err == nil {
		t.Error("expected error for missing template")
	}
}

func TestGenerateConfig_UnsupportedNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
		t.Fatal(err)
	}
	svc := newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
	if err := svc.GenerateConfig(vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Network: "ws", Security: "reality"}); err == nil {
		t.Error("expected error for unsupported network 'ws'")
	}
}

func TestGenerateConfig_UnsupportedSecurity(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
		t.Fatal(err)
	}
	svc := newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
	if err := svc.GenerateConfig(vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u", Security: "xtls"}); err == nil {
		t.Error("expected error for unsupported security 'xtls'")
	}
}

func TestGenerateConfig_RealityMissingRequiredFields(t *testing.T) {
	mkSvc := func() *XrayService {
		tmpDir := t.TempDir()
		templatePath := filepath.Join(tmpDir, "config.json.template")
		if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
			t.Fatal(err)
		}
		return newTestXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
	}
	base := vpnconfig.Server{
		Address: "1.2.3.4", Port: 443, UUID: "u", Security: "reality",
		SNI: "s.example.com", Fingerprint: "chrome", PublicKey: "PBK", ShortID: "sid",
	}
	for _, f := range []string{"public_key", "sni", "fingerprint"} {
		s := base
		switch f {
		case "public_key":
			s.PublicKey = ""
		case "sni":
			s.SNI = ""
		case "fingerprint":
			s.Fingerprint = ""
		}
		if err := mkSvc().GenerateConfig(s); err == nil {
			t.Errorf("expected error when reality %s is empty", f)
		}
	}
	// shortId is optional: reality without it must still generate.
	s := base
	s.ShortID = ""
	if err := mkSvc().GenerateConfig(s); err != nil {
		t.Errorf("reality without shortId should succeed, got %v", err)
	}
}

func TestGenerateConfig_KeepsTemplateLogSection(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	outputPath := filepath.Join(tmpDir, "config.json")
	tmpl := `{"log":{"loglevel":"warning","access":"none","error":"/tmp/xray-error.log"},"inbounds":[],"outbounds":[],"routing":{}}`
	if err := os.WriteFile(templatePath, []byte(tmpl), 0644); err != nil {
		t.Fatal(err)
	}

	server := vpnconfig.Server{Address: "1.2.3.4", Port: 443, UUID: "u1", Security: "tls"}
	if err := newTestXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
		t.Fatalf("GenerateConfig error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	logSection, ok := cfg["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected log section to be preserved, got %v", cfg["log"])
	}
	if logSection["error"] != "/tmp/xray-error.log" || logSection["access"] != "none" {
		t.Errorf("log section altered: %v", logSection)
	}
}

// TestDevTemplateLogSection pins the log section of the real dev-mode template
// shipped at server/testdata/dev/xray.template.json. Without this, deleting the
// section would leave every suite green while dev-mode Xray silently stopped
// writing the error log the Logs tab reads.
func TestDevTemplateLogSection(t *testing.T) {
	// Go runs a test with the package directory as its working directory.
	path := filepath.Join("..", "..", "testdata", "dev", "xray.template.json")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var tmpl map[string]interface{}
	if err := json.Unmarshal(content, &tmpl); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}

	logSection, ok := tmpl["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a log section in %s, got %v", path, tmpl["log"])
	}
	if got := logSection["error"]; got != "testdata/dev/xray-error.log" {
		t.Errorf("log.error = %v, want %q", got, "testdata/dev/xray-error.log")
	}
	if got := logSection["access"]; got != "none" {
		t.Errorf("log.access = %v, want %q", got, "none")
	}
}

// An import stores the outbound it read; the generator puts it in place as
// proxy-out without looking into it, whatever the protocol.
func TestGenerateConfig_StoredOutbound(t *testing.T) {
	cfg := generate(t, vpnconfig.Server{
		Name: "Hysteria", Address: "198.51.100.11", Port: 8449,
		Outbound: json.RawMessage(`{"protocol":"hysteria","settings":{"version":2,"address":"198.51.100.11","port":8449},` +
			`"streamSettings":{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"a"},` +
			`"finalmask":{"quicParams":{"congestion":"bbr"}}}}`),
	})
	ob := outbound0(t, cfg)
	if ob["tag"] != "proxy-out" || ob["protocol"] != "hysteria" {
		t.Fatalf("outbound %v", ob)
	}
	ss := ob["streamSettings"].(map[string]interface{})
	if _, ok := ss["finalmask"]; !ok {
		t.Fatalf("streamSettings %v; a key the generator does not know must pass through", ss)
	}
}

// writeFakeXray puts an xray on PATH that prints msg and exits with code.
func writeFakeXray(t *testing.T, code int, msg string) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\nexit %d\n", msg, code)
	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func testedService(t *testing.T) (*XrayService, string) {
	t.Helper()
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	outputPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return NewXrayService(templatePath, outputPath), outputPath
}

var storedVLESS = vpnconfig.Server{
	Name: "Oslo", Address: "oslo.example", Port: 443,
	Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u","encryption":"none"}]}]}}`),
}

func TestGenerateConfig_XrayAcceptsTheConfig(t *testing.T) {
	writeFakeXray(t, 0, "Configuration OK.")
	svc, outputPath := testedService(t)
	if err := svc.GenerateConfig(storedVLESS); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(outputPath); string(content) == "previous\n" {
		t.Fatal("config.json was not replaced")
	}
}

// A config Xray refuses would take every Xray client offline at the next
// restart: it never replaces the running one, and Xray's own words say why.
func TestGenerateConfig_XrayRejectsTheConfig(t *testing.T) {
	writeFakeXray(t, 23, `Failed to start: infra/conf: "allowInsecure" has been removed`)
	svc, outputPath := testedService(t)
	err := svc.GenerateConfig(storedVLESS)
	if err == nil || !strings.Contains(err.Error(), "xray rejected the config") || !strings.Contains(err.Error(), "allowInsecure") {
		t.Fatalf("err %v", err)
	}
	if content, _ := os.ReadFile(outputPath); string(content) != "previous\n" {
		t.Fatalf("config.json %q; a rejected config must not replace it", content)
	}
	entries, _ := os.ReadDir(filepath.Dir(outputPath))
	for _, e := range entries {
		if name := e.Name(); name != "config.json" && name != "config.json.template" {
			t.Fatalf("left behind: %s", name)
		}
	}
}

// storedXHTTP is a stored vless xhttp outbound whose extra carries the given
// downloadSettings.
func storedXHTTP(downloadSettings string) vpnconfig.Server {
	return vpnconfig.Server{
		Name: "Oslo", Address: "oslo.example", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},` +
			`"streamSettings":{"network":"xhttp","security":"tls","xhttpSettings":{"path":"/x","extra":{"downloadSettings":` + downloadSettings + `}}}}`),
	}
}

// Xray gives downloadSettings a destination only from its address, and
// splithttp's dialer panics on a nil one at the first connection - which
// "xray run -test" never makes. Such an outbound never replaces the running
// config; one that names its address generates as any other.
func TestGenerateConfig_DownloadSettingsWithoutAnAddress(t *testing.T) {
	writeFakeXray(t, 0, "Configuration OK.")
	svc, outputPath := testedService(t)
	err := svc.GenerateConfig(storedXHTTP(`{"network":"xhttp"}`))
	if err == nil || !strings.Contains(err.Error(), "downloadSettings without an address") {
		t.Fatalf("err %v", err)
	}
	if content, _ := os.ReadFile(outputPath); string(content) != "previous\n" {
		t.Fatalf("config.json %q; an outbound that crashes Xray must not replace it", content)
	}

	svc, outputPath = testedService(t)
	if err := svc.GenerateConfig(storedXHTTP(`{"network":"xhttp","address":"dl.example.com","port":443}`)); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(outputPath); !strings.Contains(string(content), "dl.example.com") {
		t.Fatalf("config.json %q; the outbound with its download address was not written", content)
	}
}

// Xray loads its config with encoding/json, which matches a key to a field
// whatever its case: a "DownloadSettings" is the download stream to it, and
// an "Address" its address. A record stored before the importers refused such
// spellings is read the same way.
func TestServerOutbound_DownloadSettingsAsXrayReadsThem(t *testing.T) {
	stored := func(extra string) vpnconfig.Server {
		return vpnconfig.Server{
			Name: "Oslo", Address: "oslo.example", Port: 443,
			Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},` +
				`"streamSettings":{"network":"xhttp","security":"tls","xhttpSettings":{"path":"/x","extra":` + extra + `}}}`),
		}
	}
	if _, err := serverOutbound(stored(`{"DownloadSettings":{}}`)); err == nil || !strings.Contains(err.Error(), "downloadSettings without an address") {
		t.Fatalf("err %v, want the DownloadSettings without an address refused", err)
	}
	if _, err := serverOutbound(stored(`{"downloadSettings":{"Address":"dl.example.com"}}`)); err != nil {
		t.Fatalf("err %v, want the downloadSettings with its Address accepted", err)
	}
}

// splithttp's dialer panics on a scMaxEachPostBytes of 8192 or less the first
// time it dials packet-up, which "xray run -test" never does. The range counts
// from extra when there is one - Xray builds the stream from it, the outer
// mode copied on - and an empty mode is packet-up unless the stream is REALITY.
func TestServerOutbound_SmallPacketUpPosts(t *testing.T) {
	for _, tc := range []struct {
		name, streamSettings string
		refused              bool
	}{
		{"8192", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":8192}}`, true},
		{"a range from 1", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":"1-8192"}}`, true},
		{"packet-up over tls", `{"network":"xhttp","security":"tls","xhttpSettings":{"path":"/x","mode":"packet-up","scMaxEachPostBytes":100}}`, true},
		{"above 8192", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":1000000}}`, false},
		// Xray's default replaces a range whose upper end is 0.
		{"0", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":0}}`, false},
		// Xray's own Build refuses it, and "xray run -test" with it.
		{"8192.0", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":8192.0}}`, false},
		{"stream-up", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","mode":"stream-up","scMaxEachPostBytes":100}}`, false},
		// No mode under REALITY is stream-one.
		{"reality", `{"network":"xhttp","security":"reality","realitySettings":{"serverName":"www.example.org",` +
			`"publicKey":"hhoT6JDl8yIxCxQkytm-w8ToNy6DTsn3t9Njaaoy-dE","fingerprint":"chrome"},` +
			`"xhttpSettings":{"path":"/x","scMaxEachPostBytes":100}}`, false},
		{"extra's range", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":1000000,` +
			`"extra":{"scMaxEachPostBytes":100}}}`, true},
		// Xray builds the stream from extra, and extra names no range.
		{"extra replaces the outer range", `{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","scMaxEachPostBytes":100,` +
			`"extra":{"xmux":{"maxConcurrency":"16-32"}}}}`, false},
		{"splithttp", `{"network":"splithttp","security":"none","splithttpSettings":{"scMaxEachPostBytes":100}}`, true},
		{"folded keys", `{"network":"XHTTP","security":"none","XhttpSettings":{"ScMaxEachPostBytes":100}}`, true},
		{"not xhttp", `{"network":"ws","security":"none","wsSettings":{"scMaxEachPostBytes":1}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := serverOutbound(vpnconfig.Server{
				Name: "Oslo", Address: "oslo.example", Port: 443,
				Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"oslo.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},` +
					`"streamSettings":` + tc.streamSettings + `}`),
			})
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), "scMaxEachPostBytes")):
				t.Fatalf("err %v, want the stream refused over its scMaxEachPostBytes", err)
			case !tc.refused && err != nil:
				t.Fatalf("err %v, want the stream accepted", err)
			}
		})
	}
}

// The test runs under the config lock: an xray that never answers is given
// up on at the bound, and the error says it timed out rather than that Xray
// rejected the config.
func TestXrayTest_TimesOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte("#!/bin/sh\nexec sleep 10\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := xrayTestTimeout
	xrayTestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { xrayTestTimeout = prev })
	path := filepath.Join(t.TempDir(), "config.json.test")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := xrayTest(path); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v, want a timeout", err)
	}
}

func TestGenerateConfig_WithoutXrayNothingIsTested(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	svc, outputPath := testedService(t)
	if err := svc.GenerateConfig(storedVLESS); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(outputPath); string(content) == "previous\n" {
		t.Fatal("config.json was not replaced")
	}
}

// The monitor's prober holds the outbound Generate would write, under a tag of
// its own, and refuses what Generate refuses.
func TestOutboundJSON_TagsTheOutboundGenerateWouldWrite(t *testing.T) {
	stored := vpnconfig.Server{Name: "Oslo", Address: "192.0.2.10", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"192.0.2.10","port":443,"password":"p"}]},"tag":"proxy-out"}`)}
	raw, err := OutboundJSON(stored, "m7")
	if err != nil {
		t.Fatal(err)
	}
	var ob map[string]interface{}
	if err := json.Unmarshal(raw, &ob); err != nil {
		t.Fatal(err)
	}
	if ob["tag"] != "m7" || ob["protocol"] != "trojan" {
		t.Fatalf("outbound %s", raw)
	}

	legacy := vpnconfig.Server{Name: "Legacy", Address: "192.0.2.11", Port: 443, UUID: "u", Security: "tls", SNI: "l.example"}
	raw, err = OutboundJSON(legacy, "m8")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ob); err != nil {
		t.Fatal(err)
	}
	if ob["tag"] != "m8" || ob["protocol"] != "vless" {
		t.Fatalf("legacy outbound %s", raw)
	}

	refused := vpnconfig.Server{Name: "X", Address: "192.0.2.12", Port: 443,
		Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.12","port":443,"users":[{"id":"u"}]}]},"streamSettings":{"network":"xhttp","security":"tls","xhttpSettings":{"extra":{"downloadSettings":{}}}}}`)}
	if _, err := OutboundJSON(refused, "m9"); err == nil || !strings.Contains(err.Error(), "downloadSettings without an address") {
		t.Fatalf("err %v, want the refusal Generate makes", err)
	}
}

// Retagging must preserve the numeric literals the shared validator inspected.
func TestOutboundJSON_PreservesNumericLiterals(t *testing.T) {
	for _, tc := range []struct {
		name, literal string
		refused       bool
	}{
		{name: "decimal packet-up limit", literal: "8192.0"},
		{name: "integer above 2^53", literal: "9007199254740993"},
		{name: "small integer packet-up limit", literal: "8192", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := vpnconfig.Server{Name: "Numbers", Address: "numbers.example", Port: 443,
				Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":"numbers.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","mode":"packet-up","extra":{"scMaxEachPostBytes":%s,"numbers":[%s]}}},"tag":"stored"}`, tc.literal, tc.literal))}
			ob, wantErr := serverOutbound(server)
			raw, err := OutboundJSON(server, "m10")
			if tc.refused {
				if wantErr == nil || !strings.Contains(wantErr.Error(), "scMaxEachPostBytes") {
					t.Fatalf("shared error %v, want the small packet-up refusal", wantErr)
				}
				if err == nil || err.Error() != wantErr.Error() || len(raw) != 0 {
					t.Fatalf("wrapper error %v, want %v and no outbound", err, wantErr)
				}
				return
			}
			if wantErr != nil {
				t.Fatal(wantErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantRaw, err := json.Marshal(ob)
			if err != nil {
				t.Fatal(err)
			}
			want, err := vpnconfig.DecodeOutbound(wantRaw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := vpnconfig.DecodeOutbound(raw)
			if err != nil {
				t.Fatal(err)
			}
			stream := got["streamSettings"].(map[string]interface{})
			extra := stream["xhttpSettings"].(map[string]interface{})["extra"].(map[string]interface{})
			if number, ok := extra["scMaxEachPostBytes"].(json.Number); !ok || number.String() != tc.literal {
				t.Errorf("packet-up literal %v, want exactly %s", extra["scMaxEachPostBytes"], tc.literal)
			}
			if got["tag"] != "m10" {
				t.Errorf("tag %v, want m10", got["tag"])
			}
			delete(got, "tag")
			delete(want, "tag")
			if !reflect.DeepEqual(got, want) {
				t.Error("outbound differs from the marshalled shared outbound beyond its tag")
			}
		})
	}
}

var _ GuardedXrayGenerator = (*XrayService)(nil)

func assertNoXrayTemps(t *testing.T, outputPath string) {
	t.Helper()
	paths, err := filepath.Glob(outputPath + ".*")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path != outputPath+".template" {
			t.Errorf("temporary Xray config left behind: %s", filepath.Base(path))
		}
	}
}

func TestGenerateConfigGuarded_RechecksAfterValidation(t *testing.T) {
	svc, outputPath := testedService(t)
	validated, checkedAfterValidation := false, false
	refused := errors.New("automation lost permission during validation")
	svc.validate = func(path string) error {
		if path == outputPath {
			t.Error("validation received the live path rather than the staged config")
		}
		content, err := os.ReadFile(outputPath)
		if err != nil || string(content) != "previous\n" {
			t.Errorf("live config during validation %q, error %v", content, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("staged mode %o, want 0600", info.Mode().Perm())
		}
		validated = true
		return nil
	}

	err := svc.GenerateConfigGuarded(storedVLESS, InboundPorts{}, func() error {
		if validated {
			checkedAfterValidation = true
			return refused
		}
		return nil
	})
	if !errors.Is(err, refused) {
		t.Errorf("guarded generation error %v, want the final guard refusal", err)
	}
	if !validated || !checkedAfterValidation {
		t.Errorf("validated %v, checked after validation %v; validation must precede the final guard", validated, checkedAfterValidation)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil || string(content) != "previous\n" {
		t.Errorf("live config %q, error %v; a refused publication must leave it unchanged", content, err)
	}
	assertNoXrayTemps(t, outputPath)
}

func TestGenerateConfigGuarded_NilGuardKeepsGenerationSemantics(t *testing.T) {
	templatePath, outputPath := writeTemplate(t)
	svc := newTestXrayService(templatePath, outputPath)
	validations := 0
	svc.validate = func(path string) error {
		validations++
		if path == outputPath {
			t.Error("the live config was used as the validation input")
		}
		return nil
	}
	server := vpnconfig.Server{Address: "203.0.113.50", Port: 443, UUID: "synthetic-id", Security: "tls", SNI: "oslo.example"}
	if err := svc.GenerateConfigGuarded(server, InboundPorts{TProxy: 23456, Socks: 23457}, nil); err != nil {
		t.Fatal(err)
	}
	if validations != 1 {
		t.Errorf("validations %d, want one before publication", validations)
	}
	ports := readInboundPorts(t, outputPath)
	if ports["tproxy-in"] != 23456 || ports["socks-in"] != 23457 {
		t.Errorf("ports %v, want the supplied 23456/23457", ports)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := vnextAddress(t, cfg); got != "203.0.113.50" {
		t.Errorf("dial address %q, want 203.0.113.50", got)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("live config mode %o, want 0600", info.Mode().Perm())
	}
	assertNoXrayTemps(t, outputPath)
}

func TestGenerateConfigGuarded_ValidationFailureKeepsLiveConfig(t *testing.T) {
	svc, outputPath := testedService(t)
	rejected := errors.New("synthetic Xray validation refusal")
	validations := 0
	svc.validate = func(string) error {
		validations++
		return rejected
	}
	if err := svc.GenerateConfigGuarded(storedVLESS, InboundPorts{}, func() error { return nil }); !errors.Is(err, rejected) {
		t.Errorf("generation error %v, want the validation failure", err)
	}
	if validations != 1 {
		t.Errorf("validations %d, want the staged config to be tested", validations)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil || string(content) != "previous\n" {
		t.Errorf("live config %q, error %v; validation failure must not publish", content, err)
	}
	assertNoXrayTemps(t, outputPath)
}

func waitForValidation(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			if !strings.HasPrefix(string(data), "run -test -format json -c ") {
				t.Fatalf("unexpected Xray validation command: %s", data)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("the staged Xray validation never started")
		case <-ticker.C:
		}
	}
}

func TestContextExecutor_ShutdownCancelsXrayValidation(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	started := filepath.Join(dir, "validated")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$VALIDATION_STARTED\"\nexec /bin/sleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("VALIDATION_STARTED", started)
	template, output := writeTemplate(t)
	if err := os.WriteFile(output, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	store := NewConfigService(configDir, filepath.Join(configDir, "data"))
	before := []byte(`{"xray":{"active_server":{"name":"previous","address":"192.0.2.10","port":443,"subscription":"0a1b2c3d","seq":7}}}`)
	if err := os.WriteFile(store.ConfigPath(), before, 0600); err != nil {
		t.Fatal(err)
	}
	xray := NewXrayServiceForContext(root, template, output)
	type generationResult struct {
		generated bool
		err       error
	}
	done := make(chan generationResult, 1)
	go func() {
		generated, _, err := GenerateAndRecordGuardedWalkedServer(store, xray, storedVLESS, storedVLESS, InboundPorts{}, nil)
		done <- generationResult{generated, err}
	}()
	waitForValidation(t, started)
	cancel()
	select {
	case result := <-done:
		if result.generated || !errors.Is(result.err, context.Canceled) {
			t.Errorf("canceled validation published a selection: generated=%v error=%v", result.generated, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon shutdown waited for the 15-second Xray validation timeout")
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "previous\n" {
		t.Errorf("canceled validation replaced the live Xray config: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(store.ConfigPath()); err != nil || string(raw) != string(before) {
		t.Errorf("canceled validation recorded a new active server: %q, %v", raw, err)
	}
	assertNoXrayTemps(t, output)
	if err := store.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error { return nil }); err != nil {
		t.Fatal("shutdown retained the config lock:", err)
	}
}

func TestContextExecutor_CancellationBeforeRenameKeepsLiveConfig(t *testing.T) {
	for _, phase := range []string{"before_generation_without_xray", "after_validation"} {
		t.Run(phase, func(t *testing.T) {
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			t.Setenv("PATH", t.TempDir())
			template, output := writeTemplate(t)
			if err := os.WriteFile(output, []byte("previous\n"), 0600); err != nil {
				t.Fatal(err)
			}
			xray := NewXrayServiceForContext(root, template, output)
			if phase == "after_validation" {
				xray.validate = func(string) error { cancel(); return nil }
			} else {
				cancel()
			}
			if err := xray.GenerateConfig(storedVLESS); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled generation error=%v, want context cancellation", err)
			}
			if raw, err := os.ReadFile(output); err != nil || string(raw) != "previous\n" {
				t.Errorf("%s published despite root cancellation: %q, %v", phase, raw, err)
			}
			assertNoXrayTemps(t, output)
		})
	}
}

func TestContextExecutor_XrayValidationKeepsCommandTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xray"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	previous := xrayTestTimeout
	xrayTestTimeout = 80 * time.Millisecond
	t.Cleanup(func() { xrayTestTimeout = previous })
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	template, output := writeTemplate(t)
	if err := os.WriteFile(output, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := NewXrayServiceForContext(root, template, output).GenerateConfig(storedVLESS)
	if err == nil || !strings.Contains(err.Error(), "timed out") || root.Err() != nil {
		t.Fatalf("validation lost its per-command timeout: error=%v root=%v", err, root.Err())
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "previous\n" {
		t.Fatal("validation timeout replaced the live config")
	}
	assertNoXrayTemps(t, output)
}
