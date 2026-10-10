package ssrf

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPrivateOrReserved(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},              // loopback
		{"10.0.0.1", true},               // RFC1918
		{"172.16.0.1", true},             // RFC1918
		{"192.168.1.1", true},            // RFC1918
		{"169.254.169.254", true},        // link-local (cloud metadata)
		{"100.64.0.1", true},             // CGNAT (no stdlib method covers it)
		{"0.0.0.0", true},                // unspecified
		{"0.1.2.3", true},                // 0.0.0.0/8 "this network"
		{"224.0.0.1", true},              // multicast
		{"::1", true},                    // IPv6 loopback
		{"::", true},                     // IPv6 unspecified
		{"fc00::1", true},                // IPv6 ULA
		{"fe80::1", true},                // IPv6 link-local
		{"ff02::1", true},                // IPv6 multicast
		{"::ffff:127.0.0.1", true},       // IPv4-mapped loopback
		{"::ffff:192.168.0.1", true},     // IPv4-mapped private
		{"::ffff:100.64.0.1", true},      // IPv4-mapped CGNAT (To4 + extraBlockedCIDRs)
		{"::ffff:169.254.169.254", true}, // IPv4-mapped link-local metadata
		// IPv4-embedded IPv6 transition addresses: the embedded IPv4 must be
		// unwrapped and re-checked, else NAT64/6to4/IPv4-compatible bypass the guard.
		{"64:ff9b::7f00:1", true},       // NAT64 well-known (64:ff9b::/96) -> 127.0.0.1
		{"64:ff9b::a00:1", true},        // NAT64 well-known -> 10.0.0.1
		{"2002:7f00:1::", true},         // 6to4 (2002::/16) -> 127.0.0.1
		{"2002:a00:1::", true},          // 6to4 -> 10.0.0.1
		{"::127.0.0.1", true},           // IPv4-compatible (deprecated, ::/96) -> 127.0.0.1
		{"::169.254.169.254", true},     // IPv4-compatible -> link-local metadata
		{"8.8.8.8", false},              // public
		{"1.1.1.1", false},              // public
		{"2001:4860:4860::8888", false}, // public IPv6
		{"64:ff9b::808:808", false},     // NAT64 wrapping public 8.8.8.8 (must NOT over-block)
		{"2002:808:808::", false},       // 6to4 wrapping public 8.8.8.8
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tt.ip)
			}
			if got := IsPrivateOrReserved(ip); got != tt.blocked {
				t.Errorf("IsPrivateOrReserved(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

func TestDialGuard(t *testing.T) {
	tests := []struct {
		address string
		wantErr bool
	}{
		{"127.0.0.1:80", true},
		{"192.168.1.1:443", true},
		{"100.64.0.1:443", true},
		{"169.254.169.254:80", true},
		{"[::1]:80", true},
		{"[64:ff9b::7f00:1]:443", true}, // NAT64 -> 127.0.0.1
		{"[2002:7f00:1::]:80", true},    // 6to4 -> 127.0.0.1
		{"[::127.0.0.1]:80", true},      // IPv4-compatible -> 127.0.0.1
		{"8.8.8.8:443", false},
		{"1.1.1.1:80", false},
		{"[64:ff9b::808:808]:443", false}, // NAT64 wrapping public 8.8.8.8
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			err := dialGuard("tcp", tt.address, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("dialGuard(%q) err = %v, wantErr %v", tt.address, err, tt.wantErr)
			}
		})
	}
}

func TestIsPrivateHost_RawIP(t *testing.T) {
	if !IsPrivateHost("192.168.1.1") {
		t.Error("expected 192.168.1.1 to be reported private")
	}
	if !IsPrivateHost("127.0.0.1") {
		t.Error("expected 127.0.0.1 to be reported private")
	}
	if IsPrivateHost("8.8.8.8") {
		t.Error("expected 8.8.8.8 to be reported public")
	}
}

func TestNewClient_BlocksLoopbackAtDial(t *testing.T) {
	// httptest binds to loopback; the guarded client must refuse to dial it.
	// This is the DNS-rebinding defense in action: the Control hook checks the
	// resolved connection IP regardless of how DNS produced it, so a host that
	// looked public at pre-flight but resolves private at dial is still blocked
	// (the dial-time predicate itself is also covered by TestDialGuard).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(2 * time.Second)
	resp, err := client.Get(ts.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected guarded client to block loopback dial, got success")
	}
}

func TestNewClient_DisablesRedirects(t *testing.T) {
	c := NewClient(time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect to be set (redirects disabled)")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

func TestNewClient_TimeoutBudget(t *testing.T) {
	// The overall Client.Timeout is the single authoritative bound. There must be
	// no separate ResponseHeaderTimeout (it would starve the body read once
	// headers arrive near the deadline), and the connect/TLS sub-phase budget
	// must never exceed the overall timeout.
	t.Run("small timeout caps sub-phase and drops header timeout", func(t *testing.T) {
		c := NewClient(2 * time.Second)
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
		}
		if tr.ResponseHeaderTimeout != 0 {
			t.Errorf("ResponseHeaderTimeout = %v, want 0 (rely on Client.Timeout)", tr.ResponseHeaderTimeout)
		}
		if tr.TLSHandshakeTimeout != 2*time.Second {
			t.Errorf("TLSHandshakeTimeout = %v, want 2s (min(10s, timeout))", tr.TLSHandshakeTimeout)
		}
	})
	t.Run("large timeout caps sub-phase at 10s", func(t *testing.T) {
		c := NewClient(30 * time.Second)
		tr := c.Transport.(*http.Transport)
		if tr.TLSHandshakeTimeout != 10*time.Second {
			t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
		}
		if c.Timeout != 30*time.Second {
			t.Errorf("Client.Timeout = %v, want 30s", c.Timeout)
		}
	})
}
