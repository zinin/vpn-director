package vless

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestParseURI_ValidURI(t *testing.T) {
	uri := "vless://550e8400-e29b-41d4-a716-446655440000@server.example.com:443?encryption=none#MyServer"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.UUID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected UUID '550e8400-e29b-41d4-a716-446655440000', got '%s'", server.UUID)
	}

	if server.Address != "server.example.com" {
		t.Errorf("expected Address 'server.example.com', got '%s'", server.Address)
	}

	if server.Port != 443 {
		t.Errorf("expected Port 443, got %d", server.Port)
	}

	if server.Name != "MyServer" {
		t.Errorf("expected Name 'MyServer', got '%s'", server.Name)
	}
}

func TestParseURI_URLEncodedName(t *testing.T) {
	uri := "vless://uuid@server.com:443#My%20Server%20Name"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.Name != "My Server Name" {
		t.Errorf("expected Name 'My Server Name', got '%s'", server.Name)
	}
}

func TestParseURI_NameWithEmojiFlag(t *testing.T) {
	// URI with emoji flag in name (like BlancVPN subscription)
	uri := "vless://uuid@server.com:443#%F0%9F%87%B7%F0%9F%87%BA%20%D0%A0%D0%BE%D1%81%D1%81%D0%B8%D1%8F%2C%20%D0%9C%D0%BE%D1%81%D0%BA%D0%B2%D0%B0"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should strip emoji flag and keep only text
	if server.Name != "Россия, Москва" {
		t.Errorf("expected Name 'Россия, Москва', got '%s'", server.Name)
	}
}

func TestParseURI_NameWithMultipleEmojis(t *testing.T) {
	uri := "vless://uuid@server.com:443#🇺🇸%20USA,%20New%20York%20🏙️"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.Name != "USA, New York" {
		t.Errorf("expected Name 'USA, New York', got '%s'", server.Name)
	}
}

func TestParseURI_NoName(t *testing.T) {
	uri := "vless://uuid@server.example.com:443"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Name should default to address when not specified
	if server.Name != "server.example.com" {
		t.Errorf("expected Name 'server.example.com', got '%s'", server.Name)
	}
}

func TestParseURI_NoQueryParams(t *testing.T) {
	uri := "vless://uuid@server.example.com:443#ServerName"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.Address != "server.example.com" {
		t.Errorf("expected Address 'server.example.com', got '%s'", server.Address)
	}
}

func TestParseURI_NotVlessScheme(t *testing.T) {
	uri := "vmess://uuid@server.example.com:443"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for non-vless URI")
	}
}

func TestParseURI_MissingAt(t *testing.T) {
	uri := "vless://uuidserver.example.com:443"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for URI missing @")
	}
}

func TestParseURI_MissingPort(t *testing.T) {
	uri := "vless://uuid@server.example.com"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for URI missing port")
	}
}

func TestParseURI_InvalidPort(t *testing.T) {
	uri := "vless://uuid@server.example.com:notaport"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for invalid port")
	}
}

func TestParseURI_MalformedQuery(t *testing.T) {
	// A bad %-escape in the query must fail loudly rather than silently
	// dropping stream params and emitting a broken (plain-TLS) outbound.
	uri := "vless://uuid@server.example.com:443?security=reality&pbk=%ZZ#Name"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for malformed query")
	}
}

func TestParseURI_PortOutOfRange(t *testing.T) {
	// Numeric but out-of-range ports must be rejected at the entry point so a
	// broken (port 0 / >65535) server never lands in servers.json.
	for _, uri := range []string{
		"vless://uuid@server.example.com:0#Name",
		"vless://uuid@server.example.com:99999#Name",
	} {
		if _, err := ParseURI(uri); err == nil {
			t.Fatalf("expected error for out-of-range port in %q", uri)
		}
	}
}

func TestParseURI_EmptyUUID(t *testing.T) {
	uri := "vless://@server.example.com:443"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for empty UUID")
	}
}

func TestParseURI_EmptyAddress(t *testing.T) {
	uri := "vless://uuid@:443"

	_, err := ParseURI(uri)
	if err == nil {
		t.Fatal("expected error for empty address")
	}
}

func TestParseURI_ComplexQueryParams(t *testing.T) {
	uri := "vless://uuid@server.com:443?encryption=none&security=tls&sni=server.com&fp=chrome#Name"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server.Address != "server.com" {
		t.Errorf("expected Address 'server.com', got '%s'", server.Address)
	}
	if server.Port != 443 {
		t.Errorf("expected Port 443, got %d", server.Port)
	}
	if server.Security != "tls" {
		t.Errorf("expected Security 'tls', got '%s'", server.Security)
	}
	if server.SNI != "server.com" {
		t.Errorf("expected SNI 'server.com', got '%s'", server.SNI)
	}
	if server.Fingerprint != "chrome" {
		t.Errorf("expected Fingerprint 'chrome', got '%s'", server.Fingerprint)
	}
}

func TestParseURI_Reality(t *testing.T) {
	// Real subscription format: headerType=none present, pbk/sid at the end, type after headerType
	// (guards against `type` parsing accidentally matching `headerType`).
	uri := "vless://9ca8@162.249.126.77:443?security=reality&encryption=none&fp=firefox&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=cdn3-87.yahoo.com&pbk=PBKEY&sid=55e6d9bd269aac46#NL"

	s, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Security != "reality" {
		t.Errorf("Security = %q, want reality", s.Security)
	}
	if s.Flow != "xtls-rprx-vision" {
		t.Errorf("Flow = %q, want xtls-rprx-vision", s.Flow)
	}
	if s.Network != "tcp" {
		t.Errorf("Network = %q, want tcp", s.Network)
	}
	if s.SNI != "cdn3-87.yahoo.com" {
		t.Errorf("SNI = %q, want cdn3-87.yahoo.com", s.SNI)
	}
	if s.Fingerprint != "firefox" {
		t.Errorf("Fingerprint = %q, want firefox", s.Fingerprint)
	}
	if s.PublicKey != "PBKEY" {
		t.Errorf("PublicKey = %q, want PBKEY", s.PublicKey)
	}
	if s.ShortID != "55e6d9bd269aac46" {
		t.Errorf("ShortID = %q, want 55e6d9bd269aac46", s.ShortID)
	}
}

func TestToVPNConfig_CarriesStreamParams(t *testing.T) {
	s := &Server{
		Address: "1.2.3.4", Port: 443, UUID: "u", Name: "n", IPs: []string{"1.2.3.4"},
		Security: "reality", Network: "tcp", Flow: "xtls-rprx-vision",
		SNI: "cdn.example.com", Fingerprint: "firefox", PublicKey: "PBK", ShortID: "sid",
		ALPN: []string{"h2"},
	}
	c := s.ToVPNConfig()
	if c.Address != "1.2.3.4" || c.Port != 443 || c.UUID != "u" || c.Name != "n" ||
		len(c.IPs) != 1 || c.IPs[0] != "1.2.3.4" {
		t.Errorf("ToVPNConfig dropped base fields: %+v", c)
	}
	if c.Security != "reality" || c.Network != "tcp" || c.Flow != "xtls-rprx-vision" ||
		c.SNI != "cdn.example.com" || c.Fingerprint != "firefox" ||
		c.PublicKey != "PBK" || c.ShortID != "sid" ||
		len(c.ALPN) != 1 || c.ALPN[0] != "h2" {
		t.Errorf("ToVPNConfig dropped stream params: %+v", c)
	}
}

func TestParseURI_DecodesPercentEncodedALPN(t *testing.T) {
	// Parity guard with the shell importer's _url_decode: a percent-encoded
	// comma (%2C) in alpn must decode (url.ParseQuery) then split into two
	// values. The shell parser mirrors this via _url_decode + tr ','.
	s, err := ParseURI("vless://uuid@1.2.3.4:443?type=tcp&alpn=h2%2Chttp/1.1#N")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.ALPN) != 2 || s.ALPN[0] != "h2" || s.ALPN[1] != "http/1.1" {
		t.Errorf("ALPN = %v, want [h2 http/1.1]", s.ALPN)
	}
}

func TestParseURI_NoParams(t *testing.T) {
	s, err := ParseURI("vless://uuid@host:443#X")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Security != "" || s.Flow != "" || s.SNI != "" {
		t.Errorf("expected empty stream params, got security=%q flow=%q sni=%q", s.Security, s.Flow, s.SNI)
	}
}

func TestParseURI_IPv6Address(t *testing.T) {
	uri := "vless://uuid@[2001:db8::1]:443#IPv6Server"

	server, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.Address != "[2001:db8::1]" {
		t.Errorf("expected Address '[2001:db8::1]', got '%s'", server.Address)
	}

	if server.Port != 443 {
		t.Errorf("expected Port 443, got %d", server.Port)
	}
}

// Tests for DecodeSubscription

func TestDecodeSubscription_ValidBase64(t *testing.T) {
	// Two valid VLESS URIs
	rawContent := "vless://uuid1@server1.com:443#Server1\nvless://uuid2@server2.com:443#Server2"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}

	if servers[0].Name != "Server1" {
		t.Errorf("expected first server name 'Server1', got '%s'", servers[0].Name)
	}

	if servers[1].Name != "Server2" {
		t.Errorf("expected second server name 'Server2', got '%s'", servers[1].Name)
	}
}

func TestDecodeSubscription_URLEncodedBase64(t *testing.T) {
	// URL-safe base64 encoding
	rawContent := "vless://uuid@server.com:443#TestServer"
	encoded := base64.URLEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	if servers[0].Name != "TestServer" {
		t.Errorf("expected server name 'TestServer', got '%s'", servers[0].Name)
	}
}

func TestDecodeSubscription_SkipsEmptyLines(t *testing.T) {
	rawContent := "vless://uuid1@server1.com:443#Server1\n\n\nvless://uuid2@server2.com:443#Server2\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
}

func TestDecodeSubscription_SkipsNonVlessLines(t *testing.T) {
	rawContent := "# Comment line\nvless://uuid@server.com:443#TestServer\nvmess://other@server.com:443"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server (only vless), got %d", len(servers))
	}
}

func TestDecodeSubscription_InvalidBase64(t *testing.T) {
	_, errs := DecodeSubscription("not-valid-base64!!!")

	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
}

func TestDecodeSubscription_InvalidVlessURI(t *testing.T) {
	rawContent := "vless://uuid@server.com:443#ValidServer\nvless://invalid-uri"
	encoded := base64.StdEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	// Should parse the valid one and report error for invalid
	if len(servers) != 1 {
		t.Errorf("expected 1 valid server, got %d", len(servers))
	}

	if len(errs) != 1 {
		t.Errorf("expected 1 parse error, got %d", len(errs))
	}
}

func TestDecodeSubscription_EmptyContent(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(""))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(servers))
	}
}

// Tests for ResolveIPs

func TestResolveIPs_ValidHostname(t *testing.T) {
	server := &Server{
		Address: "google.com",
		Port:    443,
		UUID:    "test",
		Name:    "Test",
	}

	err := server.ResolveIPs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(server.IPs) == 0 {
		t.Error("expected at least one IP to be resolved")
	}
}

func TestResolveIPs_InvalidHostname(t *testing.T) {
	server := &Server{
		Address: "this-hostname-definitely-does-not-exist.invalid",
		Port:    443,
		UUID:    "test",
		Name:    "Test",
	}

	err := server.ResolveIPs()
	if err == nil {
		t.Error("expected error for invalid hostname")
	}
}

func TestDecodeSubscription_RawStdBase64(t *testing.T) {
	// Raw standard base64 (without padding)
	rawContent := "vless://uuid@server.com:443#RawTest"
	encoded := base64.RawStdEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	if servers[0].Name != "RawTest" {
		t.Errorf("expected server name 'RawTest', got '%s'", servers[0].Name)
	}
}

func TestDecodeSubscription_RawURLBase64(t *testing.T) {
	// Raw URL-safe base64 (without padding)
	rawContent := "vless://uuid@server.com:443#RawURLTest"
	encoded := base64.RawURLEncoding.EncodeToString([]byte(rawContent))

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	if servers[0].Name != "RawURLTest" {
		t.Errorf("expected server name 'RawURLTest', got '%s'", servers[0].Name)
	}
}

func TestDecodeSubscription_WhitespaceInput(t *testing.T) {
	// Input with leading/trailing whitespace and newlines
	rawContent := "vless://uuid@server.com:443#WhitespaceTest"
	encoded := "  \n\t" + base64.StdEncoding.EncodeToString([]byte(rawContent)) + "  \n\t"

	servers, errs := DecodeSubscription(encoded)

	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	if servers[0].Name != "WhitespaceTest" {
		t.Errorf("expected server name 'WhitespaceTest', got '%s'", servers[0].Name)
	}
}

func TestDecodeAndResolve(t *testing.T) {
	// IP literals resolve without DNS.
	subscription := strings.Join([]string{
		"vless://uuid-1@203.0.113.10:443?security=reality#Oslo",
		"vless://missing-at-sign:443#Broken",
		"vless://uuid-2@198.51.100.7:8443#Paris",
	}, "\n")

	result := DecodeAndResolve(base64.StdEncoding.EncodeToString([]byte(subscription)))

	if result.Parsed != 2 {
		t.Errorf("Parsed = %d, want 2", result.Parsed)
	}
	if len(result.ParseErrors) != 1 {
		t.Errorf("ParseErrors = %v, want one", result.ParseErrors)
	}
	if result.ResolveErrors != 0 {
		t.Errorf("ResolveErrors = %d, want 0", result.ResolveErrors)
	}
	if len(result.Servers) != 2 {
		t.Fatalf("Servers = %+v, want both parsed servers", result.Servers)
	}
	if got := result.Servers[0]; got.Name != "Oslo" || !reflect.DeepEqual(got.IPs, []string{"203.0.113.10"}) {
		t.Errorf("first server %+v, want Oslo on 203.0.113.10", got)
	}
	if got := result.Servers[1]; got.Name != "Paris" || !reflect.DeepEqual(got.IPs, []string{"198.51.100.7"}) {
		t.Errorf("second server %+v, want Paris on 198.51.100.7", got)
	}
}
