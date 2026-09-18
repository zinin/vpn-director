// internal/service/xray_test.go
package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const testTemplate = `{"inbounds":[],"outbounds":[],"routing":{}}`

func generate(t *testing.T, server vpnconfig.Server) map[string]interface{} {
	t.Helper()
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "config.json.template")
	outputPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(templatePath, []byte(testTemplate), 0644); err != nil {
		t.Fatal(err)
	}
	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
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

	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server, InboundPorts{TProxy: 23456, Socks: 23457}); err != nil {
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

	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
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
	svc := NewXrayService(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "out"))
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
	svc := NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
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
	svc := NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
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
		return NewXrayService(templatePath, filepath.Join(tmpDir, "config.json"))
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
	if err := NewXrayService(templatePath, outputPath).GenerateConfig(server); err != nil {
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
